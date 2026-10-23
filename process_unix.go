//go:build unix

package sam

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vihanggodbole/sam-g-oss/internal/proc"
)

// lockOwned takes a nonblocking exclusive advisory lock on "<file>.lock"
// next to the file's real path (symlinks resolved), so every path to the
// same file contends for the same lock, and returns that real path. It
// guards the SQLite database and the credential store ("what" names it in
// errors). The file itself is not flock'ed because that conflicts with
// SQLite's own locks on macOS, and the credential file is replaced by
// rename. A file with several hard links is refused: another name would
// take a different lock.
func lockOwned(path, what string) (string, *os.File, error) {
	db, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", nil, err
	}
	st, err := db.Stat()
	_ = db.Close()
	if err != nil {
		return "", nil, err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		return "", nil, errorf(CodeInvalidArgument,
			"%s %s has %d hard links; sam can only guarantee single ownership for a file reachable by one filename. "+
				"Remove the extra links (or copy the file while nothing uses it) and try again", what, path, sys.Nlink)
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "", nil, errorf(CodeConflict, "%s %s is owned by another running sam process", what, path)
		}
		return "", nil, err
	}
	return path, f, nil
}

// group is a command started in its own process group, whose group ID is
// the leader's PID. The owner observes the child without reaping it, signals
// the group, then reaps the leader. A successful observation pins the group
// ID as a zombie until Wait. If observation fails, ownership is uncertain:
// the owner sends no further signals and reports that cleanup was not
// confirmed. The host must not ignore SIGCHLD or enable auto-reaping.
type group struct {
	cmd  *exec.Cmd
	pgid int

	observed        chan error
	observationDone bool // only the owner goroutine touches observation state
	observationErr  error
	signalsClosed   bool
	reaped          bool
}

// startGroup starts cmd in a new process group unless ctx is already done.
// Checking first avoids needless launches; it cannot make cancellation and
// the OS start atomic.
func startGroup(ctx context.Context, cmd *exec.Cmd) (*group, error) {
	return startGroupObserved(ctx, cmd, proc.WaitExit)
}

func startGroupObserved(ctx context.Context, cmd *exec.Cmd, observe func(int) error) (*group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if observe == nil {
		observe = proc.WaitExit
	}
	g := &group{cmd: cmd, pgid: cmd.Process.Pid, observed: make(chan error, 1)}
	go func() { g.observed <- observe(g.pgid) }()
	return g, nil
}

func (g *group) markObserved(err error) {
	g.observationDone = true
	g.observationErr = err
}

func (g *group) pollObservation() (bool, error) {
	if g.observationDone {
		return true, g.observationErr
	}
	select {
	case err := <-g.observed:
		g.markObserved(err)
		return true, err
	default:
		return false, nil
	}
}

func (g *group) waitObservation() error {
	if !g.observationDone {
		g.markObserved(<-g.observed)
	}
	return g.observationErr
}

// abandon seals the group against future signals and reaps the leader
// asynchronously. Waiting here could hang forever if observation failed
// while the command is still running; the caller reports uncertain cleanup
// and returns without pretending the command stopped.
func (g *group) abandon() {
	if g.signalsClosed || g.reaped {
		return
	}
	g.signalsClosed = true
	go func() { _ = g.cmd.Wait() }()
}

func (g *group) observationFailure(err error) error {
	g.abandon()
	return errorf(CodeProcessObservation,
		"could not confirm exit of process %d: %v; no further process-group signals will be sent because ownership is uncertain. "+
			"The command may still be running; cleanup was not confirmed", g.pgid, err)
}

func (g *group) signal(sig syscall.Signal) error {
	if g.signalsClosed || g.reaped {
		panic("sam: process group signalled after ownership was released")
	}
	done, err := g.pollObservation()
	if err != nil {
		return g.observationFailure(err)
	}
	err = syscall.Kill(-g.pgid, sig)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) && done {
		// The leader's successful unreaped-exit observation pins the group
		// ID. ESRCH now means there are no remaining signalable members.
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return g.observationFailure(fmt.Errorf("process group %d was not found while its ownership was unconfirmed: %w", g.pgid, err))
	}
	g.abandon()
	return errorf(CodeProcessObservation, "could not signal process group %d: %v; cleanup was not confirmed", g.pgid, err)
}

// terminate sends SIGTERM to the whole group and gives all its members up
// to killGrace to exit. It probes group liveness only after a successful
// non-reaping exit observation has pinned the group ID. If observation
// fails, it stops immediately without another probe or signal.
func (g *group) terminate() error {
	done, err := g.pollObservation()
	if err != nil {
		return g.observationFailure(err)
	}
	if done && !proc.GroupAlive(g.pgid) {
		return nil
	}
	if err := g.signal(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(killGrace)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		done, err := g.pollObservation()
		if err != nil {
			return g.observationFailure(err)
		}
		if done && !proc.GroupAlive(g.pgid) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		<-tick.C
	}
}

// finish sends the group's last signal, SIGKILL for any member left, waits
// for a confirmed leader exit, and only then reaps it. An observation error
// seals the group and returns immediately; it never becomes a natural exit.
func (g *group) finish() error {
	done, err := g.pollObservation()
	if err != nil {
		return g.observationFailure(err)
	}
	if !done || proc.GroupAlive(g.pgid) {
		if err := g.signal(syscall.SIGKILL); err != nil {
			return err
		}
	}
	if err := g.waitObservation(); err != nil {
		return g.observationFailure(err)
	}
	g.signalsClosed = true
	waitErr := g.cmd.Wait()
	g.reaped = true
	if errors.Is(waitErr, syscall.ECHILD) {
		return errorf(CodeProcessObservation,
			"process %d could not be reaped by its owner (%v); process-group ownership was lost and cleanup was not confirmed", g.pgid, waitErr)
	}
	return waitErr
}
