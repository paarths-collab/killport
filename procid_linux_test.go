package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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

// swapPidfdHooks installs fakes for the pidfd syscalls and restores the real
// ones when the test ends.
func swapPidfdHooks(t *testing.T) {
	t.Helper()
	o1, o2, o3, o4, o5 := pidfdOpen, pidfdSendSignal, pidfdClose, killPID, procStartTime
	t.Cleanup(func() { pidfdOpen, pidfdSendSignal, pidfdClose, killPID, procStartTime = o1, o2, o3, o4, o5 })
}

// devNullFD stands in for a pidfd: any valid descriptor works because the
// signalling syscall is faked as well.
func devNullFD() (int, error) { return unix.Open("/dev/null", unix.O_RDONLY, 0) }

// Review finding m6: when pidfd_open or pidfd_send_signal is unavailable
// (ENOSYS) or filtered by a seccomp profile (EPERM), signalling falls back to
// compare-then-kill(2) and the identity compare is never skipped.
func TestLinuxPidfdUnavailableFallsBackToCheckedKill(t *testing.T) {
	sys := newOSSystem()
	for _, tc := range []struct {
		name string
		open func(int) (int, error)
		send func(int, unix.Signal) error
	}{
		{"pidfd_open ENOSYS", func(int) (int, error) { return -1, unix.ENOSYS }, nil},
		{"pidfd_open EPERM", func(int) (int, error) { return -1, unix.EPERM }, nil},
		{"pidfd_send_signal ENOSYS", func(int) (int, error) { return devNullFD() }, func(int, unix.Signal) error { return unix.ENOSYS }},
		{"pidfd_send_signal EPERM from seccomp", func(int) (int, error) { return devNullFD() }, func(int, unix.Signal) error { return unix.EPERM }},
	} {
		t.Run(tc.name+": the child is killed through kill(2)", func(t *testing.T) {
			child := startChild(t)
			pid := child.Process.Pid
			id, err := sys.ProcessIdentity(pid)
			if err != nil {
				t.Fatal(err)
			}
			swapPidfdHooks(t)
			pidfdOpen = tc.open
			if tc.send != nil {
				pidfdSendSignal = tc.send
			}
			var kills []int
			realKill := killPID
			killPID = func(p int, sig unix.Signal) error { kills = append(kills, p); return realKill(p, sig) }
			if err := sys.SignalIfSame(pid, id.StartTime, true); err != nil {
				t.Fatalf("SignalIfSame: %v", err)
			}
			if len(kills) != 1 || kills[0] != pid {
				t.Errorf("kill(2) calls = %v", kills)
			}
			if ws := waitChild(t, child); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Errorf("wait status = %v", ws)
			}
		})
		t.Run(tc.name+": a wrong start time sends nothing", func(t *testing.T) {
			child := startChild(t)
			pid := child.Process.Pid
			id, _ := sys.ProcessIdentity(pid)
			swapPidfdHooks(t)
			pidfdOpen = tc.open
			if tc.send != nil {
				pidfdSendSignal = tc.send
			}
			killPID = func(p int, sig unix.Signal) error {
				t.Errorf("kill(%d) although the start time differs", p)
				return nil
			}
			if err := sys.SignalIfSame(pid, id.StartTime+"9", true); !errors.Is(err, errIdentityChanged) {
				t.Errorf("err = %v, want errIdentityChanged", err)
			}
			if !alive(pid) {
				t.Error("the child died")
			}
		})
	}
}

// A PID reused between the pidfd attempt and the fallback must be caught by the
// fallback's own compare.
func TestLinuxPidfdFallbackComparesIdentityAgain(t *testing.T) {
	swapPidfdHooks(t)
	var reads int
	procStartTime = func(pid int) (string, error) {
		reads++
		if reads == 1 {
			return "100", nil // the original process, seen right after pidfd_open
		}
		return "999", nil // the PID now belongs to somebody else
	}
	pidfdOpen = func(int) (int, error) { return devNullFD() }
	pidfdSendSignal = func(int, unix.Signal) error { return unix.EPERM } // blocked by seccomp
	killPID = func(p int, sig unix.Signal) error { t.Errorf("kill(%d) sent to a reused PID", p); return nil }
	if err := platformSignalIfSame(4321, "100", false); !errors.Is(err, errIdentityChanged) {
		t.Fatalf("err = %v, want errIdentityChanged", err)
	}
	if reads != 2 {
		t.Errorf("start time was read %d times, want 2 (before the pidfd signal and again in the fallback)", reads)
	}
}

func TestLinuxPidfdFallbackReportsRealPermissionAndGone(t *testing.T) {
	swapPidfdHooks(t)
	procStartTime = func(int) (string, error) { return "100", nil }
	pidfdOpen = func(int) (int, error) { return devNullFD() }
	pidfdSendSignal = func(int, unix.Signal) error { return unix.EPERM }
	killPID = func(int, unix.Signal) error { return unix.EPERM } // kill(2) says the same: a genuine EPERM
	if err := platformSignalIfSame(4321, "100", false); !errors.Is(err, os.ErrPermission) {
		t.Errorf("genuine EPERM: err = %v, want a permission error", err)
	}
	killPID = func(int, unix.Signal) error { return unix.ESRCH }
	if err := platformSignalIfSame(4321, "100", false); !errors.Is(err, errProcessNotFound) {
		t.Errorf("exited meanwhile: err = %v, want errProcessNotFound", err)
	}
	// pidfd_open reporting ESRCH is "gone", not a reason to fall back.
	pidfdOpen = func(int) (int, error) { return -1, unix.ESRCH }
	killPID = func(int, unix.Signal) error { t.Error("fell back although the process is gone"); return nil }
	if err := platformSignalIfSame(4321, "100", false); !errors.Is(err, errProcessNotFound) {
		t.Errorf("ESRCH: err = %v", err)
	}
	// With a working pidfd the signal goes through it and kill(2) is not used.
	pidfdOpen = func(int) (int, error) { return devNullFD() }
	var sent []unix.Signal
	pidfdSendSignal = func(_ int, sig unix.Signal) error { sent = append(sent, sig); return nil }
	if err := platformSignalIfSame(4321, "100", false); err != nil || len(sent) != 1 || sent[0] != unix.SIGTERM {
		t.Errorf("pidfd path: err=%v sent=%v", err, sent)
	}
}
