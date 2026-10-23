package main

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Exercise actual Windows handles: fake process tables cannot detect missing
// SYNCHRONIZE rights on a handle passed to WaitForSingleObject.
func TestWindowsProcessIdentityAndTermination(t *testing.T) {
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "KILLPORT_TEST_HELPER=sleeper")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	id, err := platformProcessIdentity(pid)
	if err != nil || id.StartTime == "" || id.Strength != strengthHandle {
		t.Fatalf("live child identity = %+v, %v", id, err)
	}
	if err := platformSignalIfSame(pid, id.StartTime+"9", true); !errors.Is(err, errIdentityChanged) {
		t.Fatalf("wrong identity: %v", err)
	}
	if err := platformSignalIfSame(pid, id.StartTime, true); err != nil {
		t.Fatalf("terminate child: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not terminate")
	}
	if _, err := platformProcessIdentity(pid); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("terminated child identity: %v", err)
	}
}
