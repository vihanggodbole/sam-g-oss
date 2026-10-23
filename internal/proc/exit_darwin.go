//go:build darwin

package proc

import (
	"errors"
	"syscall"
	"unsafe"
)

// WaitExit blocks until the child pid has exited without reaping it, so the
// zombie keeps its process-group ID reserved until the owner reaps it.
// Darwin's waitid also returns for stopped children (Go issue 19314), so
// this uses a kqueue NOTE_EXIT filter instead. ESRCH while registering is
// ambiguous when the host auto-reaps children, so it is an observation
// failure rather than proof of exit.
func WaitExit(pid int) error {
	syscall.ForkLock.RLock() // close-on-exec before a concurrent launch can inherit it
	kq, err := syscall.Kqueue()
	if err == nil {
		syscall.CloseOnExec(kq)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)
	change := []syscall.Kevent_t{{
		Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT,
	}}
	if _, err := syscall.Kevent(kq, change, nil, nil); err != nil {
		return err
	}
	events := make([]syscall.Kevent_t, 1)
	for {
		n, err := syscall.Kevent(kq, nil, events, nil)
		if errors.Is(err, syscall.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			return err
		}
		return confirmWaitableExit(pid)
	}
}

// confirmWaitableExit checks that NOTE_EXIT left a child status for this
// owner to reap. kqueue notification alone is insufficient when the host
// configures child auto-reaping.
func confirmWaitableExit(pid int) error {
	var info [16]uint64 // aligned storage for Darwin siginfo_t; si_pid is the fourth 32-bit field.
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID_NOCANCEL, 0, uintptr(pid), uintptr(unsafe.Pointer(&info[0])),
			uintptr(syscall.WEXITED|syscall.WNOWAIT|syscall.WNOHANG), 0, 0)
		switch errno {
		case 0:
			got := *(*int32)(unsafe.Add(unsafe.Pointer(&info[0]), 12))
			if got == int32(pid) {
				return nil
			}
			return errors.New("exit notification arrived without a waitable child status")
		case syscall.EINTR:
			continue
		default:
			return errno
		}
	}
}

// GroupAlive reports whether the process group still has a live member.
// Darwin refuses signal 0 (EPERM) for a group left with only its zombie
// leader, so a successful probe means a live member. The unreaped leader
// keeps the group ID reserved, so the probe cannot reach another group.
func GroupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
