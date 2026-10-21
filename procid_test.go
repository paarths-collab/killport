package main

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A real Linux stat line: 52 fields, starttime (field 22) = 1234567.
const statSample = "1234 (node) S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 11 0 1234567 123456789 4000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0\n"

func TestParseProcStat(t *testing.T) {
	// Fields after the command: state ppid pgrp session tty tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime priority nice threads
	// itrealvalue STARTTIME ...
	tail := " S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 11 0 777 123456789 4000 0 0\n"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", statSample, "1234567", false},
		{"comm with spaces", "1234 (my app)" + tail, "777", false},
		{"comm with parentheses", "1234 (my (weird) app)" + tail, "777", false},
		{"comm ends with paren", "1234 (a))" + tail, "777", false},
		{"comm looks like fields", "1234 (x S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 99999)" + tail, "777", false},
		{"empty comm", "1234 ()" + tail, "777", false},
		{"no closing paren", "1234 (node S 1", "", true},
		{"empty", "", "", true},
		{"too few fields", "1234 (node) S 1 2 3", "", true},
		{"non numeric start", "1234 (node) S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 11 0 abc 1 2", "", true},
		{"negative start", "1234 (node) S 1 1234 1234 0 -1 4194560 1000 0 0 0 5 3 0 0 20 0 11 0 -5 1 2", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcStat([]byte(tt.in))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("parseProcStat = %q, %v; want %q (err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestValidSignalPID(t *testing.T) {
	for _, pid := range []int{0, -1, -1234} {
		if validSignalPID(pid) == nil {
			t.Errorf("pid %d must be rejected (would address a process group)", pid)
		}
	}
	if err := validSignalPID(1234); err != nil {
		t.Errorf("pid 1234: %v", err)
	}
}

// ---- fake process table ----

func TestFakeProcessTable(t *testing.T) {
	f := newFake("linux")
	f.setProc(10, "100", "node")

	id, err := f.ProcessIdentity(10)
	if err != nil || id.StartTime != "100" || id.Strength != strengthStartTime || id.PID != 10 {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if err := f.SignalIfSame(10, "999", false); !errors.Is(err, errIdentityChanged) {
		t.Fatalf("wrong start: %v", err)
	}
	if len(f.signals) != 0 || len(f.kills) != 0 {
		t.Fatalf("refused signal was recorded: %v", f.signals)
	}
	if err := f.SignalIfSame(10, "100", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.signals, []fakeSignal{{PID: 10, StartTime: "100", Force: false}}) {
		t.Fatalf("signals = %v", f.signals)
	}

	f.replaceProc(10, "200")
	if err := f.SignalIfSame(10, "100", true); !errors.Is(err, errIdentityChanged) {
		t.Fatalf("after replace: %v", err)
	}
	f.exitProc(10)
	if _, err := f.ProcessIdentity(10); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("after exit: %v", err)
	}
	if err := f.SignalIfSame(10, "200", true); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("signal after exit: %v", err)
	}
	if len(f.signals) != 1 {
		t.Fatalf("signals = %v", f.signals)
	}

	// Unregistered PIDs are live unless the table is strict.
	if id, err := f.ProcessIdentity(77); err != nil || id.StartTime == "" {
		t.Fatalf("auto proc = %+v, %v", id, err)
	}
	f.strictProcs = true
	if _, err := f.ProcessIdentity(78); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("strict: %v", err)
	}
}

func TestFakeFilesAndClock(t *testing.T) {
	f := newFake("linux")
	f.files["/proc/net/tcp"] = []byte("x")
	if b, err := f.ReadFile("/proc/net/tcp"); err != nil || string(b) != "x" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	if _, err := f.ReadFile("/nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	t0 := f.Now()
	f.advance(time.Minute)
	f.Sleep(time.Second)
	if got := f.Now().Sub(t0); got != time.Minute+time.Second {
		t.Fatalf("clock moved %v", got)
	}
	f.setNow(fakeEpoch)
	if !f.Now().Equal(fakeEpoch) {
		t.Fatal("setNow")
	}
}

// ---- legacy kill path ----

// afterLookup runs a callback right after ProcessIdentity returned, i.e.
// between the identity lookup at enumeration time and the kill.
type afterLookup struct {
	*fakeSystem
	fn func(pid int)
}

func (a afterLookup) ProcessIdentity(pid int) (procIdentity, error) {
	id, err := a.fakeSystem.ProcessIdentity(pid)
	if a.fn != nil {
		a.fn(pid)
	}
	return id, err
}

func runWith(t *testing.T, sys system, args ...string) (code int, stdout string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, sys)
	return code, out.String()
}

func TestLegacyKillSignalsWithCapturedStartTime(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			f, owner := scenario3000(goos)
			f.setProc(owner, "tok-1", "node")
			code, out, errOut := invoke(t, f, "--json", "3000", "3001")
			if code != 0 {
				t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
			}
			want := []fakeSignal{{PID: owner, StartTime: "tok-1", Force: true}}
			if !reflect.DeepEqual(f.signals, want) {
				t.Fatalf("signals = %v, want %v (SIGKILL as before, once)", f.signals, want)
			}
			for _, r := range decodeKill(t, out) {
				if r.Status != statusKilled || !r.Killed {
					t.Errorf("row = %+v", r)
				}
			}
		})
	}
}

func TestLegacyKillIdentityChangedBetweenLookupAndKill(t *testing.T) {
	f, owner := scenario3000("linux")
	f.setProc(owner, "orig", "node")
	sys := afterLookup{fakeSystem: f, fn: func(pid int) {
		if pid == owner {
			f.replaceProc(pid, "reused")
		}
	}}
	code, out := runWith(t, sys, "--json", "3000")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out=%s", code, out)
	}
	if len(f.signals) != 0 || len(f.kills) != 0 {
		t.Fatalf("a replacement process was signalled: signals=%v kills=%v", f.signals, f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if r.Status != statusFailed || r.Killed || r.Error != "process changed between lookup and kill; nothing was signalled" {
			t.Errorf("row = %+v", r)
		}
	}
	// Human output reports the same reason.
	f2, owner2 := scenario3000("linux")
	f2.setProc(owner2, "orig", "node")
	sys2 := afterLookup{fakeSystem: f2, fn: func(pid int) { f2.replaceProc(pid, "reused") }}
	var o, e bytes.Buffer
	if c := run([]string{"3000"}, &o, &e, sys2); c != 1 || !strings.Contains(o.String()+e.String(), "nothing was signalled") {
		t.Errorf("human: code=%d out=%q err=%q", c, o.String(), e.String())
	}
}

func TestLegacyKillProcessGoneIsSuccess(t *testing.T) {
	t.Run("exited before lookup", func(t *testing.T) {
		f, owner := scenario3000("linux")
		f.exitProc(owner)
		code, out, _ := invoke(t, f, "--json", "3000")
		if code != 0 {
			t.Fatalf("exit = %d out=%s", code, out)
		}
		if len(f.signals) != 0 || len(f.kills) != 0 {
			t.Fatalf("signalled a gone process: %v %v", f.signals, f.kills)
		}
	})
	t.Run("exited between lookup and kill", func(t *testing.T) {
		f, owner := scenario3000("linux")
		f.setProc(owner, "orig", "node")
		sys := afterLookup{fakeSystem: f, fn: func(pid int) { f.exitProc(pid) }}
		code, out := runWith(t, sys, "--json", "3000")
		if code != 0 {
			t.Fatalf("exit = %d out=%s", code, out)
		}
		if len(f.signals) != 0 {
			t.Fatalf("signals = %v", f.signals)
		}
		for _, r := range decodeKill(t, out) {
			if r.Status != statusKilled || r.Error != "" {
				t.Errorf("row = %+v", r)
			}
		}
	})
}

func TestLegacyKillFallsBackWithoutIdentity(t *testing.T) {
	t.Run("strength none", func(t *testing.T) {
		f, owner := scenario3000("linux")
		f.setProcStrength(owner, strengthNone)
		code, out, _ := invoke(t, f, "--json", "3000")
		if code != 0 {
			t.Fatalf("exit = %d out=%s", code, out)
		}
		if len(f.signals) != 0 || !reflect.DeepEqual(f.kills, []int{owner}) {
			t.Fatalf("expected the documented legacy Kill fallback: signals=%v kills=%v", f.signals, f.kills)
		}
	})
	// Review finding m7: the unchecked Kill(pid) is only for a platform WITHOUT
	// identity support. A lookup that fails for another reason (permission
	// denied, ...) used to fall back to it too, which defeats the identity
	// check exactly when it matters; the row now fails with the real error and
	// nothing is sent. (This subtest used to assert the old fallback.)
	t.Run("lookup error sends nothing", func(t *testing.T) {
		f, owner := scenario3000("linux")
		f.identErrs[owner] = os.ErrPermission
		code, out, _ := invoke(t, f, "--json", "3000")
		if code != 1 || len(f.signals) != 0 || len(f.kills) != 0 {
			t.Fatalf("code=%d signals=%v kills=%v", code, f.signals, f.kills)
		}
		rows := decodeKill(t, out)
		if len(rows) != 1 || rows[0].Status != statusFailed || rows[0].Killed || !strings.Contains(rows[0].Error, "cannot verify the identity") || !strings.Contains(rows[0].Error, "permission") {
			t.Fatalf("rows = %+v", rows)
		}
	})
}

func TestLegacyDryRunLooksUpIdentityButNeverSignals(t *testing.T) {
	f, owner := scenario3000("linux")
	f.setProc(owner, "tok", "node")
	// readOnlySystem panics on any signal; ProcessIdentity must pass through.
	code, out, _ := invoke(t, f, "--json", "--dry-run", "3000")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 || rows[0].Status != statusPlanned || rows[0].PID != "100" {
		t.Fatalf("rows = %+v", rows)
	}
	if len(f.signals) != 0 || len(f.kills) != 0 {
		t.Fatalf("dry run signalled: %v %v", f.signals, f.kills)
	}
}

func TestReadOnlySystemSignalIfSamePanicsOthersPass(t *testing.T) {
	f := newFake("linux").setProc(5, "s", "x")
	f.files["/a"] = []byte("a")
	ro := readOnlySystem{system: f}
	mustPanic(t, "SignalIfSame", func() { _ = ro.SignalIfSame(5, "s", true) })
	if len(f.signals) != 0 {
		t.Fatalf("signals = %v", f.signals)
	}
	if id, err := ro.ProcessIdentity(5); err != nil || id.StartTime != "s" {
		t.Errorf("ProcessIdentity: %+v %v", id, err)
	}
	if b, err := ro.ReadFile("/a"); err != nil || string(b) != "a" {
		t.Errorf("ReadFile: %q %v", b, err)
	}
	if !ro.Now().Equal(fakeEpoch) {
		t.Errorf("Now = %v", ro.Now())
	}
}

func TestVerboseSystemLogsSignal(t *testing.T) {
	f := newFake("linux").setProc(5, "s", "x")
	var buf bytes.Buffer
	v := newVerboseSystem(f, &buf)
	if err := v.SignalIfSame(5, "s", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "kill: pid 5 start s force=true") {
		t.Errorf("log = %q", buf.String())
	}
	if err := v.SignalIfSame(5, "other", true); !errors.Is(err, errIdentityChanged) || !strings.Contains(buf.String(), "error") {
		t.Errorf("err=%v log=%q", err, buf.String())
	}
}
