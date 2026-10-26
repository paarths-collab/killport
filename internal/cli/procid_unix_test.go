//go:build linux || darwin

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// startChild spawns `sleep 60` as a child of the test binary. It is the only
// process these tests ever signal. The child is always killed on cleanup.
func startChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitChild(t *testing.T, cmd *exec.Cmd) syscall.WaitStatus {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("child did not terminate")
	}
	return cmd.ProcessState.Sys().(syscall.WaitStatus)
}

func alive(pid int) bool { return unix.Kill(pid, 0) == nil }

// exerciseRealChild runs the shared real-process scenario against osSystem.
// wantStrengths lists the acceptable identity strengths on this platform.
func exerciseRealChild(t *testing.T, wantStrengths ...string) {
	sys := newOSSystem()

	t.Run("identity is stable and wrong start time is refused", func(t *testing.T) {
		cmd := startChild(t)
		pid := cmd.Process.Pid
		id, err := sys.ProcessIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if id.PID != pid || id.StartTime == "" {
			t.Fatalf("identity = %+v", id)
		}
		okStrength := false
		for _, s := range wantStrengths {
			okStrength = okStrength || id.Strength == s
		}
		if !okStrength {
			t.Fatalf("strength = %q, want one of %v", id.Strength, wantStrengths)
		}
		again, err := sys.ProcessIdentity(pid)
		if err != nil || again.StartTime != id.StartTime {
			t.Fatalf("second lookup = %+v, %v (token must be stable)", again, err)
		}

		// Simulated PID reuse: right PID, wrong start time.
		for _, force := range []bool{false, true} {
			err = sys.SignalIfSame(pid, id.StartTime+"9", force)
			if !errors.Is(err, errIdentityChanged) {
				t.Fatalf("wrong start (force=%v): err = %v, want errIdentityChanged", force, err)
			}
		}
		if !alive(pid) {
			t.Fatal("child was signalled despite a wrong start time")
		}
		// And it really is still running (not terminated by a pending signal).
		time.Sleep(100 * time.Millisecond)
		if !alive(pid) {
			t.Fatal("child died after a refused signal")
		}
	})

	t.Run("SIGTERM with the right start time, then not found", func(t *testing.T) {
		cmd := startChild(t)
		pid := cmd.Process.Pid
		id, err := sys.ProcessIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := sys.SignalIfSame(pid, id.StartTime, false); err != nil {
			t.Fatal(err)
		}
		ws := waitChild(t, cmd)
		if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Fatalf("wait status = %v, want terminated by SIGTERM", ws)
		}
		// Exited and reaped: gone.
		if _, err := sys.ProcessIdentity(pid); !errors.Is(err, errProcessNotFound) {
			t.Fatalf("ProcessIdentity after reap: %v, want errProcessNotFound", err)
		}
		if err := sys.SignalIfSame(pid, id.StartTime, false); !errors.Is(err, errProcessNotFound) {
			t.Fatalf("SignalIfSame after reap: %v, want errProcessNotFound", err)
		}
	})

	t.Run("force sends SIGKILL", func(t *testing.T) {
		cmd := startChild(t)
		pid := cmd.Process.Pid
		id, err := sys.ProcessIdentity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := sys.SignalIfSame(pid, id.StartTime, true); err != nil {
			t.Fatal(err)
		}
		ws := waitChild(t, cmd)
		if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("wait status = %v, want terminated by SIGKILL", ws)
		}
	})

	t.Run("invalid PIDs are rejected without signalling anything", func(t *testing.T) {
		for _, pid := range []int{0, -1} {
			if _, err := sys.ProcessIdentity(pid); err == nil {
				t.Errorf("ProcessIdentity(%d) succeeded", pid)
			}
			if err := sys.SignalIfSame(pid, "1", true); err == nil {
				t.Errorf("SignalIfSame(%d) succeeded (would address a process group)", pid)
			}
		}
	})

	t.Run("own process has an identity", func(t *testing.T) {
		id, err := sys.ProcessIdentity(os.Getpid())
		if err != nil || id.StartTime == "" {
			t.Fatalf("identity = %+v, %v", id, err)
		}
	})
}
