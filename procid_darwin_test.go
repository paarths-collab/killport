package main

import (
	"os"
	"regexp"
	"runtime"
	"testing"
)

func TestRealProcessIdentityDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	exerciseRealChild(t, strengthStartTime)
}

func TestDarwinStartTimeFormat(t *testing.T) {
	if got := formatDarwinStart(1700000000, 42); got != "1700000000.000042" {
		t.Errorf("formatDarwinStart = %q", got)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	id, err := newOSSystem().ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d{6}$`).MatchString(id.StartTime) {
		t.Errorf("start time %q is not sec.usec", id.StartTime)
	}
}
