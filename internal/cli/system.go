package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// system is the only way killport touches the operating system. The real
// implementation is osSystem; tests substitute a fake that records every call.
type system interface {
	GOOS() string
	LookPath(name string) (string, error)
	// Run executes a command and returns its stdout. On failure the error
	// carries the exit status and the trimmed stderr text; stdout is still
	// returned because some tools (lsof) exit non-zero with valid output.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// Kill forcibly terminates a process (SIGKILL on Unix, TerminateProcess
	// on Windows).
	//
	// Deprecated: Kill signals whatever currently owns the PID. Use
	// ProcessIdentity + SignalIfSame, which cannot hit a process that reused
	// the PID. Kill remains only as the fallback of the legacy command when no
	// process identity is available on the platform.
	Kill(pid int) error
	Getpid() int
	Sleep(d time.Duration)

	// ProcessIdentity returns a token that identifies the process currently
	// running under pid, so a later SignalIfSame can tell whether the PID was
	// reused. It returns errProcessNotFound when no such process exists;
	// permission failures wrap os.ErrPermission. It never mutates anything.
	ProcessIdentity(pid int) (procIdentity, error)
	// SignalIfSame delivers SIGTERM (force=false) or SIGKILL (force=true) to
	// pid, but only if the live process still has the given start time. It
	// returns errIdentityChanged (and sends nothing) if the process differs,
	// errProcessNotFound if it is gone. Where the OS allows (Linux pidfd,
	// Windows handle) the check and the signal are atomic. On Windows force is
	// ignored: the process is always terminated.
	SignalIfSame(pid int, startTime string, force bool) error
	// ReadFile reads a file (used for /proc/net/*).
	ReadFile(path string) ([]byte, error)
	// Now is the current wall-clock time.
	Now() time.Time
}

// procIdentity identifies one incarnation of a process.
type procIdentity struct {
	PID int
	// StartTime is an OPAQUE token (decimal clock ticks on Linux, "sec.usec"
	// on macOS, decimal FILETIME on Windows). Callers may only compare it for
	// equality and pass it back to SignalIfSame; its format is not stable and
	// differs per platform.
	StartTime string
	// Strength is how strongly the platform can bind a later signal to this
	// incarnation: "pidfd" and "handle" are atomic, "start_time" compares then
	// signals (a tiny race remains), "none" means no identity is available.
	Strength string
}

// Identity strengths.
const (
	strengthPidfd     = "pidfd"
	strengthHandle    = "handle"
	strengthStartTime = "start_time"
	strengthNone      = "none"
)

var (
	errProcessNotFound = errors.New("process not found")
	errIdentityChanged = errors.New("process identity changed (PID reused)")
)

type osSystem struct{}

func newOSSystem() system { return osSystem{} }

func (osSystem) GOOS() string                         { return runtime.GOOS }
func (osSystem) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osSystem) Getpid() int                          { return os.Getpid() }
func (osSystem) Sleep(d time.Duration)                { time.Sleep(d) }

// SleepContext sleeps for d or until ctx is done, whichever comes first.
func (osSystem) SleepContext(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
func (osSystem) Now() time.Time                       { return time.Now() }
func (osSystem) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osSystem) ProcessIdentity(pid int) (procIdentity, error) { return platformProcessIdentity(pid) }

func (osSystem) SignalIfSame(pid int, startTime string, force bool) error {
	return platformSignalIfSame(pid, startTime, force)
}

func (osSystem) Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return mapFindProcessError(runtime.GOOS, err)
	}
	return p.Kill()
}

// mapFindProcessError turns the Windows "The parameter is incorrect" error
// that os.FindProcess returns for a PID that no longer exists into
// os.ErrProcessDone, so an already-gone process is not reported as a failure.
func mapFindProcessError(goos string, err error) error {
	if err != nil && goos == "windows" && strings.Contains(strings.ToLower(err.Error()), "parameter is incorrect") {
		return os.ErrProcessDone
	}
	return err
}

func (osSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// If a timed-out tool's grandchild keeps the output pipes open, do not
	// wait for it forever after the process itself has been killed.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	ce := &cmdError{Name: name, Code: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ce.Code = ee.ExitCode()
	}
	if ctx.Err() != nil {
		ce.Err = fmt.Errorf("timed out: %w", ctx.Err())
		ce.Code = -1
	}
	return stdout.Bytes(), ce
}

// cmdError is a failed external command: exit status plus stderr text.
type cmdError struct {
	Name   string
	Code   int
	Stderr string
	Err    error
}

func (e *cmdError) Error() string {
	msg := e.Name + ": " + e.Err.Error()
	if s := e.Stderr; s != "" {
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		msg += ": " + s
	}
	return msg
}

func (e *cmdError) Unwrap() error { return e.Err }
func (e *cmdError) ExitCode() int { return e.Code }

// exitCodeOf extracts a process exit status from an error, if it has one.
func exitCodeOf(err error) (int, bool) {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// runTool runs a command with its own timeout.
func runTool(ctx context.Context, sys system, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return sys.Run(ctx, name, args...)
}

// sleepCtx sleeps through sys, but returns at once when ctx is done and, where
// the system supports it, wakes up when ctx is cancelled.
func sleepCtx(ctx context.Context, sys system, d time.Duration) {
	if ctx.Err() != nil {
		return
	}
	if s, ok := sys.(interface {
		SleepContext(ctx context.Context, d time.Duration)
	}); ok {
		s.SleepContext(ctx, d)
		return
	}
	sys.Sleep(d)
}

// verboseSystem logs every external command and kill to a writer (stderr).
// Actions run concurrently, so writes to the shared writer are serialised.
type verboseSystem struct {
	system
	w  io.Writer
	mu *sync.Mutex
}

func newVerboseSystem(s system, w io.Writer) verboseSystem {
	return verboseSystem{system: s, w: w, mu: &sync.Mutex{}}
}

func (v verboseSystem) logf(format string, args ...any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fmt.Fprintf(v.w, format, args...)
}

func (v verboseSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	start := time.Now()
	out, err := v.system.Run(ctx, name, args...)
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
	}
	v.logf("[%s] run: %s (%s, %s)\n", debugLabel(), strings.Join(append([]string{name}, args...), " "), time.Since(start).Round(time.Millisecond), status)
	return out, err
}

func (v verboseSystem) SleepContext(ctx context.Context, d time.Duration) { sleepCtx(ctx, v.system, d) }

func (v verboseSystem) Kill(pid int) error {
	start := time.Now()
	err := v.system.Kill(pid)
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
	}
	v.logf("[%s] kill: pid %d (%s, %s)\n", debugLabel(), pid, time.Since(start).Round(time.Microsecond), status)
	return err
}

func (v verboseSystem) SignalIfSame(pid int, startTime string, force bool) error {
	start := time.Now()
	err := v.system.SignalIfSame(pid, startTime, force)
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
	}
	v.logf("[%s] kill: pid %d start %s force=%t (%s, %s)\n", debugLabel(), pid, startTime, force, time.Since(start).Round(time.Microsecond), status)
	return err
}

// readOnlyWrap puts the readOnlySystem guard around a system. Every read-only
// command (legacy --dry-run, inspect, plan) goes through it; it is a variable
// only so a test can observe that they do.
var readOnlyWrap = func(s system) system { return readOnlySystem{system: s} }

// readOnlySystem is the defence-in-depth wrapper used for --dry-run: any
// attempt to kill a process or run a command that is not a known read-only
// lookup panics, so a dry run can never mutate anything by accident.
type readOnlySystem struct{ system }

func (r readOnlySystem) SleepContext(ctx context.Context, d time.Duration) {
	sleepCtx(ctx, r.system, d)
}

func (r readOnlySystem) Kill(pid int) error {
	panic(fmt.Sprintf("dry-run attempted to kill PID %d", pid))
}

func (r readOnlySystem) SignalIfSame(pid int, startTime string, force bool) error {
	panic(fmt.Sprintf("dry-run attempted to signal PID %d", pid))
}

// ProcessIdentity, ReadFile and Now are read-only and pass through.

func (r readOnlySystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "lsof", "ss", "netstat", "tasklist":
	case "docker":
		contextLookup := len(args) == len(dockerContextArgs) && strings.Join(args, "\x00") == strings.Join(dockerContextArgs, "\x00")
		if !contextLookup && (len(args) == 0 || args[0] != "ps") {
			panic("dry-run attempted a mutating docker command: docker " + strings.Join(args, " "))
		}
	default:
		panic("dry-run attempted to run " + name)
	}
	return r.system.Run(ctx, name, args...)
}
