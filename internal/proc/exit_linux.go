//go:build linux

package proc

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// WaitExit blocks until the child pid has exited without reaping it
// (waitid with WNOWAIT, as Go's own os package does on Linux), so the
// zombie keeps its process-group ID reserved until the owner reaps it.
func WaitExit(pid int) error {
	const pPID = 1
	var info [16]uint64 // siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info)),
			syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		switch e {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		}
		return e
	}
}

// GroupAlive reports whether the process group still has a live member.
// Linux counts a zombie as signallable, and the leader stays a zombie
// until its owner reaps it, so this looks for a member in /proc that is
// not a zombie. The unreaped leader keeps the group ID reserved, so every
// process found with that group really belongs to it.
func GroupAlive(pgid int) bool {
	if syscall.Kill(-pgid, 0) != nil {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // cannot tell: keep the full grace
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		i := bytes.LastIndexByte(b, ')')
		if err != nil || i < 0 {
			continue
		}
		// After "pid (comm)": state, ppid, pgrp, ...
		f := strings.Fields(string(b[i+1:]))
		if len(f) > 2 && f[2] == strconv.Itoa(pgid) && f[0] != "Z" && f[0] != "X" {
			return true
		}
	}
	return false
}
