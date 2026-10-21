package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests added for the independent review findings.

// runHelperProcess lets the test binary act as a helper child for the
// osSystem.Run timeout test (no external tool is ever executed).
func runHelperProcess() bool {
	switch os.Getenv("KILLPORT_TEST_HELPER") {
	case "parent":
		// Start a grandchild that inherits stdout (the pipe), then hang.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "KILLPORT_TEST_HELPER=sleeper")
		c.Stdout = os.Stdout
		_ = c.Start()
		time.Sleep(time.Minute)
		return true
	case "sleeper":
		time.Sleep(8 * time.Second)
		return true
	}
	return false
}

func TestOSSystemRunTimeoutDoesNotHangOnGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t.Setenv("KILLPORT_TEST_HELPER", "parent")
	start := time.Now()
	_, err := osSystem{}.Run(ctx, os.Args[0])
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("Run waited %v for the grandchild's pipe (WaitDelay missing)", d)
	}
}

// --- item 2: --json with --help / --version

func TestJSONHelpAndVersionStayJSON(t *testing.T) {
	for _, args := range [][]string{{"--json", "--version"}, {"--version", "-j"}, {"-j", "--version"}} {
		code, out, _ := invoke(t, newFake("linux"), args...)
		m := decodeErr(t, out)
		if code != 0 || m["version"] == "" || len(m) != 1 {
			t.Fatalf("%v: code=%d out=%q", args, code, out)
		}
	}
	for _, args := range [][]string{{"--json", "--help"}, {"-j", "-h"}, {"-jh"}, {"--help", "--json=true"}} {
		code, out, _ := invoke(t, newFake("linux"), args...)
		m := decodeErr(t, out)
		if code != 0 || !strings.Contains(m["usage"], "Usage:") || !strings.Contains(m["usage"], "--dry-run") {
			t.Fatalf("%v: code=%d out=%q", args, code, out)
		}
	}
	// --json=false keeps the plain text behaviour.
	code, out, _ := invoke(t, newFake("linux"), "--json=false", "--version")
	if code != 0 || !strings.Contains(out, "killport version") || json.Valid([]byte(out)) {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// A bad flag next to --help is still a JSON usage error.
	code, out, _ = invoke(t, newFake("linux"), "--json", "--help", "--bogus")
	if code != 2 || decodeErr(t, out)["code"] != "usage" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestPlainHelpAndVersionUnchanged(t *testing.T) {
	code, out, _ := invoke(t, newFake("linux"), "--version")
	if code != 0 || !strings.HasPrefix(out, "killport version") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	code, out, _ = invoke(t, newFake("linux"), "-h")
	if code != 0 || !strings.Contains(out, "Usage:") || json.Valid([]byte(out)) {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// --- item 15

func TestWantsJSON(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"--json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--json=1"}, true},
		{[]string{"--json=false"}, false},
		{[]string{"--json=0"}, false},
		{[]string{"-j"}, true},
		{[]string{"-cj"}, true},
		{[]string{"-jc"}, true},
		{[]string{"-vdj"}, true},
		{[]string{"-j=true"}, true},
		{[]string{"-j=false"}, false},
		{[]string{"-dj=1"}, true},
		{[]string{"-d=j"}, false},
		{[]string{"-cd"}, false},
		{[]string{"-j", "--json=false"}, false},
		{[]string{"--json=false", "-j"}, true},
		{[]string{"-5"}, false},
		{[]string{"3000", "--", "-j"}, false},
		{[]string{"-j", "--", "3000"}, true},
		{[]string{"--jsonx"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := wantsJSON(tt.args); got != tt.want {
			t.Errorf("wantsJSON(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestBundledFlagsKeepJSONOnUsageError(t *testing.T) {
	f := newFake("linux", "lsof")
	code, out, _ := invoke(t, f, "-cj", "--bogus")
	if code != 2 || decodeErr(t, out)["code"] != "usage" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	code, out, errOut := invoke(t, newFake("linux", "lsof"), "-d=j", "--bogus")
	if code != 2 || out != "" || errOut == "" {
		t.Fatalf("-d=j must not enable JSON: code=%d out=%q err=%q", code, out, errOut)
	}
}

// --- item 3 and 4: lsof warnings and Linux visibility via ss

func TestLsofSilentFailureWithStderrIsNotNoListeners(t *testing.T) {
	warn := fail(1, "lsof: WARNING: can't stat() fuse file system /mnt/x")
	t.Run("no ss: warning, port reported free with caveat", func(t *testing.T) {
		f := newFake("linux", "lsof").on(cmdLsof, warn)
		code, out, errOut := invoke(t, f, "--json", "3000")
		if code != 0 || decodeKill(t, out)[0].Status != statusFree {
			t.Fatalf("code=%d out=%s", code, out)
		}
		if !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "can't stat()") {
			t.Errorf("stderr lacks the lsof warning: %q", errOut)
		}
	})
	t.Run("darwin: warning too", func(t *testing.T) {
		f := newFake("darwin", "lsof").on(cmdLsof, warn)
		_, _, errOut := invoke(t, f, "--json", "--check", "3000")
		if !strings.Contains(errOut, "lsof") {
			t.Errorf("stderr = %q", errOut)
		}
	})
	t.Run("linux falls back to ss", func(t *testing.T) {
		f := newFake("linux", "lsof", "ss").on(cmdLsof, warn)
		f.on(cmdSS, ok(ssHeader+"LISTEN 0 1 *:3000 *:* users:((\"node\",pid=77,fd=3))\n"), ok(ssHeader))
		code, out, errOut := invoke(t, f, "--json", "3000")
		if code != 0 || !reflect.DeepEqual(f.kills, []int{77}) {
			t.Fatalf("code=%d kills=%v out=%s", code, f.kills, out)
		}
		if strings.Contains(errOut, "can't stat()") {
			t.Errorf("ss fallback succeeded, no warning expected: %q", errOut)
		}
	})
	t.Run("plain exit 1 without stderr is still no listeners", func(t *testing.T) {
		f := newFake("linux", "lsof").on(cmdLsof, fail(1, ""))
		code, _, errOut := invoke(t, f, "--json", "3000")
		if code != 0 || errOut != "" {
			t.Fatalf("code=%d stderr=%q", code, errOut)
		}
	})
}

func TestLinuxRootOwnedListenerNotReportedFree(t *testing.T) {
	f := newFake("linux", "lsof", "ss").on(cmdLsof, fail(1, ""))
	f.on(cmdSS, ok(ssHeader+"LISTEN 0 128 *:3000 *:*\n"))
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 1 || len(f.kills) != 0 {
		t.Fatalf("code=%d kills=%v", code, f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 || rows[0].Status != statusFailed || rows[0].Killed || !strings.Contains(rows[0].Error, "not visible") || rows[0].Status == statusFree {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestLinuxSSAddsOnlyPortsLsofDidNotShow(t *testing.T) {
	f := newFake("linux", "lsof", "ss")
	f.on(cmdLsof, ok(lsofEntry(100, "node", "*:3000")))
	f.on(cmdSS, ok(ssHeader+
		"LISTEN 0 1 *:3000 *:* users:((\"node\",pid=100,fd=1))\n"+
		"LISTEN 0 1 *:3001 *:*\n"+
		"LISTEN 0 1 *:3002 *:* users:((\"sshd\",pid=9,fd=1))\n"))
	code, out, _ := invoke(t, f, "--check", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got []string
	for _, r := range decodeCheck(t, out) {
		got = append(got, r.Port+":"+r.PID+":"+r.ProcessName)
	}
	if want := []string{"3000:100:node", "3001::", "3002:9:sshd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestLinuxSSFailureWithLsofOKWarns(t *testing.T) {
	f := newFake("linux", "lsof", "ss").on(cmdLsof, ok("")).on(cmdSS, fail(1, "ss: boom"))
	code, out, errOut := invoke(t, f, "--json", "3000")
	if code != 0 || decodeKill(t, out)[0].Status != statusFree {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if !strings.Contains(errOut, "ss failed") || !strings.Contains(errOut, "boom") {
		t.Errorf("stderr = %q", errOut)
	}
}

// --- item 5: -race with concurrent verbose logging

func TestVerboseConcurrentKillsAreSerialised(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		b.WriteString(lsofEntry(100+i, "n", "*:"+strconv.Itoa(4000+i)))
	}
	f := newFake("linux", "lsof").on(cmdLsof, ok(b.String()), ok(""))
	code, out, errOut := invoke(t, f, "-v", "-j", "4000-4007")
	if code != 0 || len(f.kills) != 8 {
		t.Fatalf("code=%d kills=%v", code, f.kills)
	}
	decodeKill(t, out)
	if n := strings.Count(errOut, "kill: pid"); n != 8 {
		t.Errorf("%d kill log lines, want 8:\n%s", n, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(errOut), "\n") {
		if !strings.HasPrefix(line, "[DEBUG] ") {
			t.Errorf("interleaved/garbled log line: %q", line)
		}
	}
}

// --- item 6: Windows without tasklist names

func TestWindowsTasklistFailureRefusesNamelessPIDs(t *testing.T) {
	t.Run("docker backend plus container", func(t *testing.T) {
		f := newFake("windows", "netstat", "tasklist", "docker")
		f.on(cmdNetstat, ok(netstatHeader+netstatListen("3000", 5000)), ok(netstatHeader))
		f.on(cmdTasklist, fail(1, "ERROR: Access denied"))
		f.on(cmdDockerPS, ok("abc123|web|0.0.0.0:3000->80/tcp\n"), ok(""))
		f.on(dockerStopID+"abc123", ok(""))
		code, out, errOut := invoke(t, f, "--json", "3000")
		if code != 1 || len(f.kills) != 0 {
			t.Fatalf("code=%d kills=%v", code, f.kills)
		}
		if !strings.Contains(errOut, "tasklist failed") {
			t.Errorf("stderr lacks tasklist warning: %q", errOut)
		}
		rows := decodeKill(t, out)
		if len(rows) != 2 {
			t.Fatalf("rows = %+v", rows)
		}
		if !rows[0].IsDocker || rows[0].Status != statusStopped {
			t.Errorf("container row = %+v", rows[0])
		}
		if r := rows[1]; r.PID != "5000" || r.Status != statusFailed || r.Killed || r.Error != "process name unavailable (tasklist failed); refusing to kill" {
			t.Errorf("pid row = %+v", r)
		}
	})
	t.Run("plain process", func(t *testing.T) {
		f := newFake("windows", "netstat", "tasklist")
		f.on(cmdNetstat, ok(netstatHeader+netstatListen("3001", 1234)))
		f.on(cmdTasklist, fail(1, "boom"))
		code, out, _ := invoke(t, f, "--json", "3001")
		r := decodeKill(t, out)[0]
		if code != 1 || len(f.kills) != 0 || r.Status != statusFailed || !strings.Contains(r.Error, "process name unavailable") {
			t.Fatalf("code=%d kills=%v row=%+v", code, f.kills, r)
		}
	})
	t.Run("check mode still works with a warning", func(t *testing.T) {
		f := newFake("windows", "netstat", "tasklist")
		f.on(cmdNetstat, ok(netstatHeader+netstatListen("3001", 1234)))
		f.on(cmdTasklist, fail(1, "boom"))
		code, out, errOut := invoke(t, f, "--check", "--json")
		rows := decodeCheck(t, out)
		if code != 0 || len(rows) != 1 || rows[0].SafeToKill || !strings.Contains(errOut, "tasklist failed") {
			t.Fatalf("code=%d rows=%+v err=%q", code, rows, errOut)
		}
	})
}

// --- item 9

func TestMapFindProcessError(t *testing.T) {
	winErr := errors.New("OpenProcess: The parameter is incorrect.")
	if got := mapFindProcessError("windows", winErr); !errors.Is(got, os.ErrProcessDone) {
		t.Errorf("windows parameter error = %v, want ErrProcessDone", got)
	}
	if got := mapFindProcessError("linux", winErr); got != winErr {
		t.Errorf("linux error must be unchanged, got %v", got)
	}
	other := errors.New("OpenProcess: Access is denied.")
	if got := mapFindProcessError("windows", other); got != other {
		t.Errorf("other windows error must be unchanged, got %v", got)
	}
	if mapFindProcessError("windows", nil) != nil {
		t.Error("nil must stay nil")
	}
}

// --- item 10: dry-run is read-only

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
}

func TestReadOnlySystemRefusesMutations(t *testing.T) {
	f := newFake("linux", "lsof", "docker").on(cmdDockerPS, ok("")).on(cmdLsof, ok(""))
	ro := readOnlySystem{system: f}
	mustPanic(t, "Kill", func() { _ = ro.Kill(5) })
	mustPanic(t, "docker stop", func() { _, _ = ro.Run(context.Background(), "docker", "stop", "-t", "10", "abc") })
	mustPanic(t, "docker rm", func() { _, _ = ro.Run(context.Background(), "docker", "rm", "-f", "abc") })
	mustPanic(t, "docker (no args)", func() { _, _ = ro.Run(context.Background(), "docker") })
	mustPanic(t, "kill binary", func() { _, _ = ro.Run(context.Background(), "kill", "-9", "5") })
	mustPanic(t, "taskkill", func() { _, _ = ro.Run(context.Background(), "taskkill", "/F", "/PID", "5") })
	if _, err := ro.Run(context.Background(), "docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}"); err != nil {
		t.Errorf("docker ps must be allowed: %v", err)
	}
	if _, err := ro.Run(context.Background(), "lsof", lsofArgs...); err != nil {
		t.Errorf("lsof must be allowed: %v", err)
	}
	if len(f.kills) != 0 || f.count("docker stop") != 0 {
		t.Errorf("wrapper let a mutation through: kills=%v runs=%v", f.kills, f.runs)
	}
}

func TestExecuteDryRunReturnsEarly(t *testing.T) {
	f := newFake("linux")
	acts := []*act{{kind: actionKill, pid: 5, name: "x"}, {kind: actionStop, container: container{ID: "abc", Name: "web"}}}
	// The read-only wrapper would panic if execute reached the system.
	execute(context.Background(), readOnlySystem{system: f}, acts, true)
	if len(f.kills) != 0 || len(f.runs) != 0 {
		t.Fatalf("dry-run executed: kills=%v runs=%v", f.kills, f.runs)
	}
	for _, a := range acts {
		if a.err != nil {
			t.Errorf("act recorded an error: %v", a.err)
		}
	}
}

func TestDryRunScenariosAllReadOnly(t *testing.T) {
	scenarios := map[string]func() *fakeSystem{
		"native": func() *fakeSystem { f, _ := scenario3000("linux"); return f },
		"windows": func() *fakeSystem {
			f, _ := scenario3000("windows")
			return f
		},
		"docker": func() *fakeSystem {
			f := newFake("linux", "lsof", "docker")
			f.on(cmdLsof, ok(lsofEntry(500, "com.docker.backend", "*:3000")))
			f.on(cmdDockerPS, ok(psWeb))
			return f
		},
		"refused pid 1": func() *fakeSystem { return platformFake("linux", lsofEntry(1, "launchd", "*:3000"), "") },
		"free":          func() *fakeSystem { return platformFake("linux", "", "") },
	}
	for name, mk := range scenarios {
		for _, extra := range [][]string{nil, {"-v"}, {"--json"}} {
			t.Run(name+" "+strings.Join(extra, ""), func(t *testing.T) {
				f := mk()
				args := append([]string{"-d", "3000", "3001"}, extra...)
				invoke(t, f, args...)
				if len(f.kills) != 0 || f.count("docker stop") != 0 {
					t.Fatalf("dry-run mutated: kills=%v runs=%v", f.kills, f.runs)
				}
			})
		}
	}
}

// --- item 11

func TestVerifyUnavailableWording(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		f := newFake("linux", "lsof", "docker")
		f.on(cmdLsof, ok(""))
		f.on(cmdDockerPS, ok(psWeb), fail(1, "daemon went away"))
		f.on(dockerStopID+"abc123", ok(""))
		code, out, _ := invoke(t, f, "--json", "3000")
		r := decodeKill(t, out)[0]
		if code != 1 || r.Killed || r.Status != statusFailed || !strings.Contains(r.Error, "stopped, but could not verify the port is free: ") || !strings.Contains(r.Error, "daemon went away") {
			t.Fatalf("code=%d row=%+v", code, r)
		}
	})
	t.Run("native", func(t *testing.T) {
		f := newFake("linux", "lsof")
		f.on(cmdLsof, ok(lsofEntry(100, "node", "*:3000")), fail(2, "lsof: broken"))
		code, out, _ := invoke(t, f, "--json", "3000")
		r := decodeKill(t, out)[0]
		if code != 1 || r.Killed || r.Status != statusFailed || !strings.Contains(r.Error, "killed, but could not verify the port is free: ") {
			t.Fatalf("code=%d row=%+v", code, r)
		}
	})
}

// --- item 14

func TestDockerStopTimeoutExceedsGrace(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	f.on(cmdLsof, ok(""))
	f.on(cmdDockerPS, ok(psWeb), ok(""))
	f.on(dockerStopID+"abc123", ok(""))
	if code, out, _ := invoke(t, f, "--json", "3000"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	d, found := f.deadlines[dockerStopID+"abc123"]
	if !found {
		t.Fatalf("docker stop ran without a deadline: %v", f.deadlines)
	}
	if d < 30*time.Second {
		t.Fatalf("docker stop timeout = %v, want >= 30s (docker's grace period is 10s)", d)
	}
}

func TestSplitHostPortBounds(t *testing.T) {
	for _, addr := range []string{"*:0", "*:65536", "*:-1", "*:+80", "*:", "*:abc", "noport"} {
		if _, p, ok := splitHostPort(addr); ok {
			t.Errorf("splitHostPort(%q) accepted port %d", addr, p)
		}
	}
	for addr, want := range map[string]int{"*:1": 1, "[::1]:65535": 65535, "127.0.0.53%lo:53": 53} {
		if _, p, ok := splitHostPort(addr); !ok || p != want {
			t.Errorf("splitHostPort(%q) = %d,%v want %d", addr, p, ok, want)
		}
	}
}
