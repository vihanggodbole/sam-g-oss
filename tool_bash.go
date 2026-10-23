package sam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/vihanggodbole/sam-g-oss/internal/proc"
)

// killGrace is how long a cancelled command's process group has between
// SIGTERM and SIGKILL, and how long a finished command's leftover processes
// may keep its output open.
const killGrace = 2 * time.Second

func bashTool(procs *procTable) Tool {
	return Tool{
		Name:         "bash",
		ReplayUnsafe: true,
		Description: "Run a command with bash -c in the workspace. stdout and stderr are streamed and share one output limit; " +
			"processes the command leaves behind are stopped when it exits. Optional timeout_seconds stops the command's process group. " +
			"For long-lived processes such as dev servers, set background to true and give a log_file: the command starts in its own " +
			"process group with stdin closed and stdout/stderr appended to that file, and the call returns at once with its pid. " +
			"Give the long-running command itself (for example npm run dev), without a trailing &: when it exits, whatever it left " +
			"running in its group is stopped too. A launch does not mean the process is ready or succeeded; check with ordinary " +
			"commands (for example curl, or read the log file). Background processes keep running after this turn and are stopped " +
			"when the session is deleted or the runtime shuts down.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"command":{"type":"string"},` +
			`"timeout_seconds":{"type":"integer","description":"Optional timeout for a foreground command"},` +
			`"background":{"type":"boolean","description":"Launch and return immediately instead of waiting; requires log_file"},` +
			`"log_file":{"type":"string","description":"Background only: file that receives stdout and stderr (appended)"}},` +
			`"required":["command"]}`),
		Run: func(ctx context.Context, c ToolCall) (string, error) { return runBash(ctx, procs, c) },
	}
}

type bashArgs struct {
	Command    string `json:"command"`
	Timeout    int    `json:"timeout_seconds"`
	Background bool   `json:"background"`
	LogFile    string `json:"log_file"`
}

func runBash(ctx context.Context, procs *procTable, c ToolCall) (string, error) {
	var a bashArgs
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return "", err
	}
	switch {
	case a.Command == "":
		return "", errors.New("command is required")
	case a.Background:
		return launchBackground(ctx, procs, c, a)
	case a.LogFile != "":
		return "", errors.New("log_file is only used with background: true")
	}
	runCtx := ctx
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
		defer cancel()
	}
	cmd := exec.Command("bash", "-c", a.Command)
	cmd.Dir = c.Workspace
	out, err := captureOutput(cmd, c.Output)
	if err != nil {
		return "", err
	}
	defer out.close()
	g, err := startGroupObserved(runCtx, cmd, procs.exitObserver())
	out.started()
	if err != nil {
		return "", err
	}
	// This goroutine owns the group: every signal precedes the final reap.
	heldOpen := false
	select {
	case exitErr := <-g.observed:
		g.markObserved(exitErr)
		if exitErr != nil {
			return "", procs.reportFailure(c.SessionID, g.observationFailure(exitErr))
		}
		// Leftover group members (for example `cmd &` jobs) may still write;
		// they get killGrace to close the output before they are stopped.
		heldOpen = !within(out.done, runCtx.Done(), killGrace) && runCtx.Err() == nil
	case <-runCtx.Done():
		if err := g.terminate(); err != nil {
			return "", procs.reportFailure(c.SessionID, err)
		}
	}
	waitErr := g.finish()
	if ErrorCodeOf(waitErr) == CodeProcessObservation {
		return "", procs.reportFailure(c.SessionID, waitErr)
	}
	if !within(out.done, nil, killGrace) { // a process outside the group still holds the output
		heldOpen = true
	}
	if runCtx.Err() != nil {
		if ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("command timed out after %d seconds and its process group was terminated; earlier side effects may have occurred", a.Timeout)
		}
		return "", runCtx.Err()
	}
	var exit *exec.ExitError
	switch {
	case heldOpen:
		return "", fmt.Errorf("the command exited with status %d, but processes it started kept its output open for %s afterwards, "+
			"so output capture stopped there and the remaining processes in its group were killed; earlier side effects may have occurred. "+
			"To keep a process running, launch it with background and a log_file", cmd.ProcessState.ExitCode(), killGrace)
	case waitErr == nil:
		return "[exit status 0]", nil
	case errors.As(waitErr, &exit):
		return fmt.Sprintf("[exit status %d]", exit.ExitCode()), nil
	}
	return "", waitErr
}

// within reports whether ch closes within d. It gives up early when stop closes.
func within(ch, stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
	case <-stop:
	}
	return false
}

// capture copies a command's stdout and stderr into the call's output
// through pipes it owns (not exec.Cmd's), so the leader can be reaped
// independently of when leftover processes close the pipes.
type capture struct {
	readers, writers []*os.File
	done             chan struct{} // both pipes reached EOF or were closed
}

func captureOutput(cmd *exec.Cmd, out *Output) (*capture, error) {
	c := &capture{done: make(chan struct{})}
	var wg sync.WaitGroup
	for _, name := range []string{"stdout", "stderr"} {
		r, w, err := os.Pipe()
		if err != nil {
			c.close()
			return nil, err
		}
		c.readers, c.writers = append(c.readers, r), append(c.writers, w)
		dst := out.Stream(name)
		wg.Go(func() {
			if _, err := io.Copy(dst, r); err != nil {
				_, _ = io.Copy(io.Discard, r) // over the limit: drain so writers do not block
			}
		})
	}
	cmd.Stdout, cmd.Stderr = c.writers[0], c.writers[1]
	go func() { wg.Wait(); close(c.done) }()
	return c, nil
}

// started closes the parent's write ends once the child has them (or the
// start failed), so EOF arrives when the command's processes close theirs.
func (c *capture) started() {
	for _, w := range c.writers {
		_ = w.Close()
	}
}

// close abandons any output still held open and waits for the copiers.
func (c *capture) close() {
	c.started()
	for _, r := range c.readers {
		_ = r.Close()
	}
	if len(c.readers) == 2 {
		<-c.done
	}
}

// maxPIDDigits sizes the launch acknowledgement before the real PID is
// known: PIDs are positive 32-bit integers.
const maxPIDDigits = 10

// launchResult is the concise acknowledgement saved as a launch's result.
func launchResult(pid, logPath string) string {
	return fmt.Sprintf("Launched in background (this does not mean it is ready): {\"pid\":%s,\"log_file\":%s}\n"+
		"Its stdout and stderr are appended to log_file. It stops when its command exits, the session is deleted, "+
		"or the runtime shuts down; to stop it sooner while it runs: kill -TERM -- -%s", pid, mustJSON(logPath), pid)
}

// launchBackground starts a command that outlives the call. Its output goes
// to the log file (appended), never to the call's captured output.
func launchBackground(ctx context.Context, procs *procTable, c ToolCall, a bashArgs) (string, error) {
	if a.LogFile == "" {
		return "", errors.New("background commands need log_file: the file that receives their stdout and stderr")
	}
	if a.Timeout != 0 {
		return "", errors.New("timeout_seconds applies only to foreground commands; stop a background process with kill")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := resolvePath(c.Workspace, a.LogFile)
	if err != nil {
		return "", err
	}
	target, _, _, err := regularTarget(p)
	if err != nil {
		return "", err
	}
	// The acknowledgement must fit the call's output limit; check before
	// launching, so a live process never loses its PID and log path.
	n := len(launchResult(strconv.Itoa(1e9), target))
	if c.uncertain {
		n += len(uncertainWarning) + 1
	}
	if c.Output != nil && n > c.Output.limit {
		return "", fmt.Errorf("the command was not started: its launch result (PID and log path, up to %d bytes) would not fit "+
			"this session's tool_output_limit of %d bytes. Use a shorter log_file path, or ask the user to raise tool_output_limit "+
			"in the session config (changeable while the session is idle)", n, c.Output.limit)
	}
	log, err := openLog(target)
	if err != nil {
		return "", err
	}
	defer log.Close() // the child keeps its own descriptor
	cmd := exec.Command("bash", "-c", a.Command)
	cmd.Dir = c.Workspace
	cmd.Stdout, cmd.Stderr = log, log // stdin stays nil: /dev/null
	bp, err := procs.start(ctx, c.SessionID, cmd)
	if err != nil {
		return "", err
	}
	if c.launched != nil {
		*c.launched = append(*c.launched, bp)
	}
	return launchResult(strconv.Itoa(bp.pgid), target), nil
}

// openLog opens a background log for appending, creating it and its parent
// directories. The open does not block (a FIFO swapped in after the path
// check fails instead of waiting for a reader), and the opened descriptor
// itself must be a regular file. This is not protection against an
// adversarial filesystem.
func openLog(target string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", target)
	}
	if err == nil {
		err = syscall.SetNonblock(int(f.Fd()), false) // the child gets an ordinary blocking descriptor
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// procTable tracks the background processes this runtime started, by
// session. Each launch has one owner goroutine. Observation failures are
// retained because the runtime can no longer safely signal that process
// group, and deletion/shutdown must not claim confirmed cleanup.
type procTable struct {
	mu          sync.Mutex
	closed      bool
	procs       map[string][]*bgProc
	failures    map[string][]error
	observeExit func(int) error // optional fault injection; nil uses the OS observer
	onFailure   func(string, error)
}

// bgProc is one background launch.
type bgProc struct {
	*group
	stop     chan struct{} // closed to request a stop
	stopOnce sync.Once
	done     chan struct{} // closed after its owner settled or relinquished it
}

func (p *bgProc) requestStop() { p.stopOnce.Do(func() { close(p.stop) }) }

func (t *procTable) exitObserver() func(int) error {
	if t != nil && t.observeExit != nil {
		return t.observeExit
	}
	return proc.WaitExit
}

// start launches cmd for a session unless the call was already cancelled or
// the runtime is stopping. Launch and registration happen under one lock,
// so a concurrent stopAll never misses a process.
func (t *procTable) start(ctx context.Context, sessionID string, cmd *exec.Cmd) (*bgProc, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("the runtime is shutting down; the background command was not started")
	}
	g, err := startGroupObserved(ctx, cmd, t.exitObserver())
	if err != nil {
		return nil, err
	}
	p := &bgProc{group: g, stop: make(chan struct{}), done: make(chan struct{})}
	if t.procs == nil {
		t.procs = map[string][]*bgProc{}
	}
	t.procs[sessionID] = append(t.procs[sessionID], p)
	go t.own(sessionID, p)
	return p, nil
}

// own is the launch's single owner. Natural exit kills leftovers at once;
// requested cleanup gives the whole group SIGTERM and killGrace. An
// observation failure releases ownership without signalling, reports that
// cleanup is uncertain, and asynchronously reaps the leader if possible.
func (t *procTable) own(sessionID string, p *bgProc) {
	stopping := false
	select {
	case err := <-p.observed:
		p.markObserved(err)
	case <-p.stop:
		stopping = true
	}
	_, observeErr := p.pollObservation()
	var err error
	if observeErr != nil {
		err = p.observationFailure(observeErr)
	} else {
		if stopping {
			err = p.terminate()
		}
		if err == nil {
			err = p.finish()
		}
	}
	if err != nil {
		var exit *exec.ExitError
		if ErrorCodeOf(err) == CodeProcessObservation || !errors.As(err, &exit) {
			if ErrorCodeOf(err) != CodeProcessObservation {
				err = errorf(CodeProcessObservation, "background process %d cleanup was not confirmed: %v", p.pgid, err)
			}
			t.recordFailure(sessionID, err)
		}
	}
	t.mu.Lock()
	t.procs[sessionID] = slices.DeleteFunc(t.procs[sessionID], func(x *bgProc) bool { return x == p })
	if len(t.procs[sessionID]) == 0 {
		delete(t.procs, sessionID)
	}
	t.mu.Unlock()
	close(p.done)
}

func (t *procTable) reportFailure(sessionID string, err error) error {
	if t != nil && sessionID != "" && ErrorCodeOf(err) == CodeProcessObservation {
		t.recordFailure(sessionID, err)
	}
	return err
}

func (t *procTable) recordFailure(sessionID string, err error) {
	t.mu.Lock()
	if t.failures == nil {
		t.failures = map[string][]error{}
	}
	t.failures[sessionID] = append(t.failures[sessionID], err)
	callback := t.onFailure
	t.mu.Unlock()
	if callback != nil {
		callback(sessionID, err)
	}
}

func (t *procTable) failuresFor(sessionID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return errors.Join(t.failures[sessionID]...)
}

func (t *procTable) allFailures() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.failures))
	for id := range t.failures {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	var failures []error
	for _, id := range keys {
		failures = append(failures, t.failures[id]...)
	}
	return errors.Join(failures...)
}

// stopSession stops the session's background processes and waits until
// their owners finish or ctx ends. It also returns any earlier observation
// failure for this session, since cleanup can no longer be confirmed.
func (t *procTable) stopSession(ctx context.Context, sessionID string) error {
	t.mu.Lock()
	ps := slices.Clone(t.procs[sessionID])
	t.mu.Unlock()
	if err := stopAndWait(ctx, ps); err != nil {
		return err
	}
	return t.failuresFor(sessionID)
}

// stopAll refuses later launches and joins cleanup already in progress.
// Previously observed failures are returned instead of being reported as a
// successful shutdown.
func (t *procTable) stopAll() error {
	t.mu.Lock()
	t.closed = true
	var ps []*bgProc
	for _, s := range t.procs {
		ps = append(ps, s...)
	}
	t.mu.Unlock()
	if err := stopAndWait(context.Background(), ps); err != nil {
		return err
	}
	return t.allFailures()
}

func stopAndWait(ctx context.Context, ps []*bgProc) error {
	for _, p := range ps {
		p.requestStop()
	}
	for _, p := range ps {
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
