package main

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
	Kill(pid int) error
	Getpid() int
	Sleep(d time.Duration)
}

type osSystem struct{}

func newOSSystem() system { return osSystem{} }

func (osSystem) GOOS() string                         { return runtime.GOOS }
func (osSystem) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (osSystem) Getpid() int                          { return os.Getpid() }
func (osSystem) Sleep(d time.Duration)                { time.Sleep(d) }

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

// readOnlySystem is the defence-in-depth wrapper used for --dry-run: any
// attempt to kill a process or run a command that is not a known read-only
// lookup panics, so a dry run can never mutate anything by accident.
type readOnlySystem struct{ system }

func (r readOnlySystem) Kill(pid int) error {
	panic(fmt.Sprintf("dry-run attempted to kill PID %d", pid))
}

func (r readOnlySystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	switch name {
	case "lsof", "ss", "netstat", "tasklist":
	case "docker":
		if len(args) == 0 || args[0] != "ps" {
			panic("dry-run attempted a mutating docker command: docker " + strings.Join(args, " "))
		}
	default:
		panic("dry-run attempted to run " + name)
	}
	return r.system.Run(ctx, name, args...)
}
