package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestRealProcessIdentityLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	exerciseRealChild(t, strengthPidfd, strengthStartTime)
}

func TestLinuxStartTimeMatchesProcStat(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", os.Getpid()))
	if err != nil {
		t.Skipf("/proc unavailable: %v", err)
	}
	want, err := parseProcStat(data)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newOSSystem().ProcessIdentity(os.Getpid())
	if err != nil || id.StartTime != want {
		t.Fatalf("identity = %+v, %v; want start %s", id, err, want)
	}
}
