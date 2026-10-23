package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// platformFake builds a fake system for goos whose listener enumeration
// returns `before` first and `after` on every later call (verification).
// before/after are lsof -F output on unix and netstat rows on windows.
func platformFake(goos, before, after string) *fakeSystem {
	if goos == "windows" {
		f := newFake(goos, "netstat", "tasklist")
		f.on(cmdNetstat, ok(netstatHeader+before), ok(netstatHeader+after))
		f.on(cmdTasklist, ok("\"System\",\"4\",\"Services\",\"0\",\"144 K\"\r\n\"node.exe\",\"1234\",\"Console\",\"1\",\"50,000 K\"\r\n\"java.exe\",\"9\",\"Console\",\"1\",\"1 K\"\r\n"))
		return f
	}
	f := newFake(goos, "lsof")
	f.on(cmdLsof, ok(before), ok(after))
	return f
}

// scenario3000 holds an owner of ports 3000 and 3001 (PID 1234 on windows,
// 100 on unix) plus lookalikes that must never be touched.
func scenario3000(goos string) (f *fakeSystem, ownerPID int) {
	if goos == "windows" {
		before := netstatListen("3000", 1234) + netstatListen("3001", 1234) +
			"  TCP    127.0.0.1:50001        127.0.0.1:3000         ESTABLISHED     555\n" +
			"  TCP    127.0.0.1:3000         127.0.0.1:50000        ESTABLISHED     556\n" +
			"  UDP    0.0.0.0:3000           *:*                                    777\n" +
			netstatListen("13000", 9)
		return platformFake(goos, before, netstatListen("13000", 9)), 1234
	}
	before := lsofEntry(100, "node", "*:3000", "[::]:3000", "*:3001") +
		lsofConn(300, "curl", "127.0.0.1:52000->127.0.0.1:3000") +
		lsofEntry(400, "other", "*:13000")
	return platformFake(goos, before, lsofEntry(400, "other", "*:13000")), 100
}

var allOS = []string{"linux", "darwin", "windows"}

func TestKillOnlyListenersOfExactPort(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			f, owner := scenario3000(goos)
			code, out, errOut := invoke(t, f, "--json", "3001", "3000")
			if code != 0 {
				t.Fatalf("exit %d stderr=%s", code, errOut)
			}
			if want := []int{owner}; !reflect.DeepEqual(f.sortedKills(), want) {
				t.Fatalf("kills = %v, want exactly %v (once, listener only)", f.kills, want)
			}
			rows := decodeKill(t, out)
			if len(rows) != 2 || rows[0].Port != "3000" || rows[1].Port != "3001" {
				t.Fatalf("rows = %+v", rows)
			}
			for _, r := range rows {
				if r.Status != statusKilled || !r.Killed || r.Action != actionKill || r.DryRun || r.IsDocker {
					t.Errorf("bad row %+v", r)
				}
			}
			if f.sleeps != 0 {
				t.Errorf("verification polled needlessly: %d sleeps", f.sleeps)
			}
			// ps/kill/taskkill are no longer used.
			for _, r := range f.runs {
				if strings.HasPrefix(r, "ps ") || strings.HasPrefix(r, "kill ") || strings.HasPrefix(r, "taskkill ") {
					t.Errorf("unexpected helper: %s", r)
				}
			}
		})
	}
}

func TestClientWithRemotePortNeverKilled(t *testing.T) {
	f := platformFake("linux", lsofConn(300, "curl", "127.0.0.1:52000->127.0.0.1:3000"), "")
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 0 || len(f.kills) != 0 {
		t.Fatalf("code=%d kills=%v", code, f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 || rows[0].Status != statusFree {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestVerificationSamePIDStillHolds(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			// The killed PID is still listed afterwards (kill had no effect).
			owner, name := 100, "node"
			var f *fakeSystem
			if goos == "windows" {
				owner, name = 1234, "node.exe"
				f = platformFake(goos, netstatListen("3000", 1234), netstatListen("3000", 1234))
			} else {
				f = platformFake(goos, lsofEntry(100, "node", "*:3000"), lsofEntry(100, "node", "*:3000"))
			}
			code, out, _ := invoke(t, f, "--json", "3000")
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			rows := decodeKill(t, out)
			if len(rows) != 1 {
				t.Fatalf("rows=%+v", rows)
			}
			r := rows[0]
			want := fmt.Sprintf("PID %d (%s) still holds port 3000 after kill_process", owner, name)
			if r.Status != statusFailed || r.Killed || !strings.Contains(r.Error, want) {
				t.Errorf("row = %+v, want error containing %q", r, want)
			}
			if !reflect.DeepEqual(f.sortedKills(), []int{owner}) {
				t.Errorf("kills = %v", f.kills)
			}
			if f.sleeps != verifyAttempts-1 {
				t.Errorf("sleeps = %d, want %d", f.sleeps, verifyAttempts-1)
			}
		})
	}
}

// A replacement process with a new PID takes over the port: the killed PID's
// own row stays killed (it is gone) and the new owner gets its own failed row.
func TestVerificationReplacementOwnerGetsOwnRow(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			owner, repl := 100, 101
			var f *fakeSystem
			if goos == "windows" {
				owner, repl = 1234, 1235
				f = platformFake(goos, netstatListen("3000", 1234), netstatListen("3000", 1235))
			} else {
				f = platformFake(goos, lsofEntry(100, "node", "*:3000"), lsofEntry(101, "node", "*:3000"))
			}
			code, out, _ := invoke(t, f, "--json", "3000")
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			rows := decodeKill(t, out)
			if len(rows) != 2 {
				t.Fatalf("rows=%+v", rows)
			}
			if r := rows[0]; r.PID != strconv.Itoa(owner) || r.Status != statusKilled || !r.Killed || r.Error != "" {
				t.Errorf("killed row = %+v", r)
			}
			r := rows[1]
			want := fmt.Sprintf("port 3000 is now held by PID %d", repl)
			if r.PID != strconv.Itoa(repl) || r.Status != statusFailed || r.Killed || r.Action != actionNone || !strings.Contains(r.Error, want) {
				t.Errorf("new owner row = %+v, want error containing %q", r, want)
			}
			if !reflect.DeepEqual(f.sortedKills(), []int{owner}) {
				t.Errorf("kills = %v", f.kills)
			}
			if f.sleeps != verifyAttempts-1 {
				t.Errorf("sleeps = %d, want %d", f.sleeps, verifyAttempts-1)
			}
		})
	}
}

// Two owners on one port and the kill of the second is refused by the OS:
// the first row is killed, the second keeps the real error, exit 1.
func TestSiblingKillRefusedByOS(t *testing.T) {
	both := lsofEntry(100, "node", "*:3000") + lsofEntry(101, "node", "*:3000")
	f := platformFake("linux", both, lsofEntry(101, "node", "*:3000"))
	f.killErrs[101] = fmt.Errorf("kill: %w", os.ErrPermission)
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	rows := decodeKill(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.PID != "100" || r.Status != statusKilled || !r.Killed {
		t.Errorf("row100 = %+v", r)
	}
	if r := rows[1]; r.PID != "101" || r.Status != statusFailed || r.Killed || !strings.Contains(r.Error, "permission") {
		t.Errorf("row101 = %+v", r)
	}
	if !reflect.DeepEqual(f.sortedKills(), []int{100, 101}) {
		t.Errorf("kills = %v", f.kills)
	}
	// The human output must not leave the impression that the port is free.
	f2 := platformFake("linux", both, lsofEntry(101, "node", "*:3000"))
	f2.killErrs[101] = os.ErrPermission
	_, human, _ := invoke(t, f2, "3000")
	if !strings.Contains(human, "Killed PID 100 (node) on port 3000 (port 3000 is still held by another owner") || strings.Contains(human, "already free") {
		t.Errorf("human output = %q", human)
	}
}

// A killable process and a protected PID 1 share the port: PID 1 is refused,
// the other is killed, and nothing claims the port is free.
func TestSiblingRefusedOwnerKeepsPortHeld(t *testing.T) {
	both := lsofEntry(1, "launchd", "*:3000") + lsofEntry(100, "node", "*:3000")
	f := platformFake("darwin", both, lsofEntry(1, "launchd", "*:3000"))
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !reflect.DeepEqual(f.kills, []int{100}) {
		t.Fatalf("kills = %v, want only 100", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; r.PID != "1" || r.Status != statusFailed || r.Killed || !strings.Contains(r.Error, "refusing to kill PID 1") {
		t.Errorf("pid 1 row = %+v", r)
	}
	if r := rows[1]; r.PID != "100" || r.Status != statusKilled || !r.Killed {
		t.Errorf("pid 100 row = %+v", r)
	}
	for _, r := range rows {
		if r.Status == statusFree || strings.Contains(r.Error, "free") {
			t.Errorf("row claims the port is free: %+v", r)
		}
	}
}

// The container is gone but Docker's own proxy still holds the port: the
// proxy gets its own failed row naming it.
func TestContainerStoppedButInfraStillHoldsPort(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	infra := lsofEntry(500, "docker-proxy", "*:3000")
	f.on(cmdLsof, ok(infra), ok(infra))
	f.on(cmdDockerPS, ok(psWeb), ok(""))
	f.on(dockerStopID+"abc123", ok(""))
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 1 || len(f.kills) != 0 {
		t.Fatalf("code=%d kills=%v", code, f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if r := rows[0]; !r.IsDocker || r.Status != statusStopped || !r.Killed {
		t.Errorf("container row = %+v", r)
	}
	if r := rows[1]; r.PID != "500" || r.Status != statusFailed || r.Killed || !strings.Contains(r.Error, "still held by PID 500 (docker-proxy) after stop_container") {
		t.Errorf("infra row = %+v", r)
	}
}

func TestKillErrorTextPreserved(t *testing.T) {
	f, owner := scenario3000("linux")
	f.killErrs[owner] = errors.New("operation not permitted")
	code, out, _ := invoke(t, f, "--json", "3000")
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	r := decodeKill(t, out)[0]
	if r.Killed || r.Status != statusFailed || !strings.Contains(r.Error, "operation not permitted") || strings.Contains(r.Error, "Requires sudo/Admin") {
		t.Errorf("row = %+v", r)
	}
	if !strings.Contains(r.Error, "sudo") {
		t.Errorf("permission hint missing: %q", r.Error)
	}
}

func TestProcessAlreadyGoneIsNotAnError(t *testing.T) {
	f, owner := scenario3000("linux")
	f.killErrs[owner] = os.ErrProcessDone
	code, _, _ := invoke(t, f, "--json", "3000")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
}

func TestProtectedPIDsRefused(t *testing.T) {
	tests := []struct {
		name  string
		f     *fakeSystem
		port  string
		match string
	}{
		{"pid 1", platformFake("linux", lsofEntry(1, "launchd", "*:80"), ""), "80", "refusing to kill PID 1"},
		{"own pid", platformFake("darwin", lsofEntry(fakeOwnPID, "killport", "*:81"), ""), "81", "killport itself"},
		{"windows system", platformFake("windows", netstatListen("445", 4), ""), "445", "PID 4"},
		{"invisible owner", newFake("linux", "ss").on(cmdSS, ok("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 128 *:82 *:*\n")), "82", "not visible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, out, _ := invoke(t, tt.f, "--json", tt.port)
			if code != 1 {
				t.Fatalf("exit = %d", code)
			}
			if len(tt.f.kills) != 0 {
				t.Fatalf("signalled protected process: %v", tt.f.kills)
			}
			r := decodeKill(t, out)[0]
			if r.Status != statusFailed || r.Killed || r.Action != actionNone || !strings.Contains(r.Error, tt.match) {
				t.Errorf("row = %+v", r)
			}
		})
	}
}

const psWeb = "abc123|web|0.0.0.0:3000->80/tcp, [::]:3000->80/tcp, 0.0.0.0:3001->81/tcp\n"

func dockerFake(goos string) *fakeSystem {
	f := platformFake(goos, "", "")
	f.tools["docker"] = true
	return f
}

func TestDockerContainerStoppedOnceForTwoPorts(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	infra := lsofEntry(500, "com.docker.backend", "*:3000", "*:3001")
	f.on(cmdLsof, ok(infra), ok(""))
	f.on(cmdDockerPS, ok(psWeb), ok(""))
	f.on(dockerStopID+"abc123", ok("abc123\n"))
	code, out, errOut := invoke(t, f, "--json", "3000", "3001")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if n := f.count("docker stop"); n != 1 {
		t.Fatalf("docker stop ran %d times: %v", n, f.runs)
	}
	if len(f.kills) != 0 {
		t.Fatalf("docker infrastructure was killed: %v", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Status != statusStopped || !r.Killed || !r.IsDocker || r.ContainerID != "abc123" || r.Action != actionStop || r.PID != "" {
			t.Errorf("row = %+v", r)
		}
	}
}

func TestDockerStopFailureReported(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	f.on(cmdLsof, ok(""))
	f.on(cmdDockerPS, ok(psWeb))
	f.on(dockerStopID+"abc123", fail(1, "Error response from daemon: boom"))
	code, out, _ := invoke(t, f, "--json", "3000")
	r := decodeKill(t, out)[0]
	if code != 1 || r.Status != statusFailed || r.Killed || !strings.Contains(r.Error, "boom") {
		t.Fatalf("code=%d row=%+v", code, r)
	}
}

func TestDockerPSFailureProtectsInfra(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	f.on(cmdLsof, ok(lsofEntry(500, "docker-proxy", "*:3000")+lsofEntry(600, "node", "*:3002")), ok(""))
	f.on(cmdDockerPS, fail(1, "Cannot connect to the Docker daemon"))
	code, out, errOut := invoke(t, f, "--json", "3000", "3002")
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(errOut, "warning") || !strings.Contains(errOut, "Cannot connect") {
		t.Errorf("stderr lacks docker ps warning: %q", errOut)
	}
	rows := decodeKill(t, out)
	if rows[0].Status != statusFailed || !strings.Contains(rows[0].Error, "Docker infrastructure") || !strings.Contains(rows[0].Error, "Cannot connect") {
		t.Errorf("infra row = %+v", rows[0])
	}
	if !reflect.DeepEqual(f.kills, []int{600}) {
		t.Errorf("kills = %v, want only the ordinary node process", f.kills)
	}
	if rows[1].Status != statusKilled {
		t.Errorf("node row = %+v", rows[1])
	}
}

func TestDockerInfraWithoutDockerBinaryRefused(t *testing.T) {
	f := newFake("linux", "lsof")
	f.on(cmdLsof, ok(lsofEntry(500, "docker-proxy", "*:3000")))
	code, _, _ := invoke(t, f, "--json", "3000")
	if code != 1 || len(f.kills) != 0 {
		t.Fatalf("code=%d kills=%v", code, f.kills)
	}
}

func TestDockerStillRunningAfterStopFailsVerification(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	f.on(cmdLsof, ok(""))
	f.on(cmdDockerPS, ok(psWeb))
	f.on(dockerStopID+"abc123", ok(""))
	code, out, _ := invoke(t, f, "--json", "3000")
	r := decodeKill(t, out)[0]
	if code != 1 || r.Killed || !strings.Contains(r.Error, "still in use after stop_container") {
		t.Fatalf("code=%d row=%+v", code, r)
	}
}

func TestDryRunNativeNeverMutates(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			f, owner := scenario3000(goos)
			code, out, _ := invoke(t, f, "--dry-run", "3000")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if len(f.kills) != 0 || f.count("docker stop") != 0 {
				t.Fatalf("dry-run mutated: kills=%v runs=%v", f.kills, f.runs)
			}
			for _, bad := range []string{"already free", "Killed", "Stopped"} {
				if strings.Contains(out, bad) {
					t.Errorf("output contains %q:\n%s", bad, out)
				}
			}
			if !strings.Contains(out, "Would kill PID") || !strings.Contains(out, "on port 3000") {
				t.Errorf("output = %q (owner %d)", out, owner)
			}

			f2, _ := scenario3000(goos)
			code, out, _ = invoke(t, f2, "--dry-run", "--json", "3000", "3001")
			rows := decodeKill(t, out)
			if code != 0 || len(rows) != 2 || len(f2.kills) != 0 {
				t.Fatalf("code=%d rows=%+v kills=%v", code, rows, f2.kills)
			}
			for _, r := range rows {
				if r.Status != statusPlanned || r.Killed || !r.DryRun || r.Action != actionKill {
					t.Errorf("row = %+v", r)
				}
			}
		})
	}
}

func TestDryRunDockerNeverStops(t *testing.T) {
	newF := func() *fakeSystem {
		f := newFake("linux", "lsof", "docker")
		f.on(cmdLsof, ok(lsofEntry(500, "com.docker.backend", "*:3000")))
		f.on(cmdDockerPS, ok(psWeb))
		return f
	}
	f := newF()
	code, out, _ := invoke(t, f, "--dry-run", "3000", "3001")
	if code != 0 || len(f.kills) != 0 || f.count("docker stop") != 0 {
		t.Fatalf("code=%d kills=%v runs=%v", code, f.kills, f.runs)
	}
	if !strings.Contains(out, "Would stop Docker container web (abc123) publishing port 3000") {
		t.Errorf("output = %q", out)
	}
	for _, bad := range []string{"already free", "Killed", "Stopped"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
	f = newF()
	_, out, _ = invoke(t, f, "-d", "-j", "3000")
	r := decodeKill(t, out)[0]
	if r.Status != statusPlanned || r.Killed || !r.DryRun || !r.IsDocker || r.Action != actionStop {
		t.Errorf("row = %+v", r)
	}
}

func TestHumanOutputKilledAndFree(t *testing.T) {
	f, _ := scenario3000("linux")
	code, out, _ := invoke(t, f, "3000", "9999")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "Killed PID 100 (node) on port 3000") || !strings.Contains(out, "Port 9999 is already free") {
		t.Errorf("output = %q", out)
	}
}

func TestExitCodesAndJSON(t *testing.T) {
	t.Run("free port exit 0", func(t *testing.T) {
		f := newFake("linux", "lsof").on(cmdLsof, fail(1, ""))
		code, out, _ := invoke(t, f, "--json", "3000")
		rows := decodeKill(t, out)
		if code != 0 || len(rows) != 1 || rows[0].Status != statusFree || rows[0].Killed || rows[0].Action != actionNone {
			t.Fatalf("code=%d rows=%+v", code, rows)
		}
	})
	t.Run("lsof exit 1 with output still parsed", func(t *testing.T) {
		f := newFake("linux", "lsof").on(cmdLsof, fakeResp{out: lsofEntry(100, "node", "*:3000"), err: fail(1, "warn").err}, ok(""))
		code, _, _ := invoke(t, f, "--json", "3000")
		if code != 0 || !reflect.DeepEqual(f.kills, []int{100}) {
			t.Fatalf("code=%d kills=%v", code, f.kills)
		}
	})
	for _, args := range [][]string{
		{"--json", "0"}, {"--json", "65536"}, {"--json", "5000-3000"}, {"--json", "+3000"},
		{"--json", "1-99999999999"}, {"--json", "3000", "abc"}, {"--json", "--bogus"}, {"--bogus", "--json"}, {"-j", "-5"},
	} {
		t.Run("usage "+strings.Join(args, " "), func(t *testing.T) {
			f, _ := scenario3000("linux")
			code, out, _ := invoke(t, f, args...)
			if code != 2 {
				t.Fatalf("exit = %d", code)
			}
			m := decodeErr(t, out)
			if m["code"] != "usage" || m["error"] == "" {
				t.Fatalf("json = %v", m)
			}
			if len(f.runs) != 0 || len(f.kills) != 0 {
				t.Fatalf("usage error still inspected/killed: runs=%v kills=%v", f.runs, f.kills)
			}
		})
	}
	t.Run("usage human goes to stderr", func(t *testing.T) {
		f := newFake("linux", "lsof")
		code, out, errOut := invoke(t, f, "nope")
		if code != 2 || out != "" || !strings.Contains(errOut, "invalid port") {
			t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
	t.Run("no args", func(t *testing.T) {
		code, out, errOut := invoke(t, newFake("linux", "lsof"))
		if code != 2 || out != "" || !strings.Contains(errOut, "requires at least 1 port") {
			t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
		}
		code, out, _ = invoke(t, newFake("linux", "lsof"), "--json")
		if code != 2 || decodeErr(t, out)["code"] != "usage" {
			t.Fatalf("code=%d out=%q", code, out)
		}
	})
	t.Run("missing lsof json", func(t *testing.T) {
		f := newFake("darwin")
		code, out, _ := invoke(t, f, "--json", "3000")
		m := decodeErr(t, out)
		if code != 1 || m["code"] != "missing_dependency" || !strings.Contains(m["error"], "lsof") {
			t.Fatalf("code=%d json=%v", code, m)
		}
	})
	t.Run("missing lsof human to stderr", func(t *testing.T) {
		code, out, errOut := invoke(t, newFake("darwin"), "3000")
		if code != 1 || out != "" || !strings.Contains(errOut, "lsof") {
			t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
		}
	})
	t.Run("missing windows tools", func(t *testing.T) {
		code, out, _ := invoke(t, newFake("windows", "netstat"), "--json", "--check")
		m := decodeErr(t, out)
		if code != 1 || m["code"] != "missing_dependency" || !strings.Contains(m["error"], "tasklist") {
			t.Fatalf("code=%d json=%v", code, m)
		}
	})
	t.Run("linux ss fallback", func(t *testing.T) {
		f := newFake("linux", "ss")
		f.on(cmdSS, ok("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 1 *:3000 *:* users:((\"node\",pid=77,fd=3))\n"), ok("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"))
		code, _, _ := invoke(t, f, "--json", "3000")
		if code != 0 || !reflect.DeepEqual(f.kills, []int{77}) {
			t.Fatalf("code=%d kills=%v", code, f.kills)
		}
	})
	for _, mode := range []string{"3000", "--check"} {
		t.Run("enumeration failure "+mode, func(t *testing.T) {
			args := []string{"--json", mode}
			if mode != "--check" {
				args = []string{"--json", "3000"}
			}
			f := newFake("linux", "lsof").on(cmdLsof, fail(2, "lsof: broken"))
			code, out, _ := invoke(t, f, args...)
			m := decodeErr(t, out)
			if code != 1 || m["code"] != "enumeration_failed" || !strings.Contains(m["error"], "broken") {
				t.Fatalf("code=%d json=%v", code, m)
			}
		})
	}
	t.Run("windows enumeration failure", func(t *testing.T) {
		f := newFake("windows", "netstat", "tasklist").on(cmdNetstat, fail(1, "access denied"))
		code, out, _ := invoke(t, f, "--json", "3000")
		if code != 1 || decodeErr(t, out)["code"] != "enumeration_failed" {
			t.Fatalf("code=%d out=%s", code, out)
		}
	})
	t.Run("failed kill exit 1 json array", func(t *testing.T) {
		f, owner := scenario3000("linux")
		f.killErrs[owner] = errors.New("boom")
		code, out, _ := invoke(t, f, "--json", "3000", "9999")
		rows := decodeKill(t, out)
		if code != 1 || len(rows) != 2 || rows[1].Status != statusFree {
			t.Fatalf("code=%d rows=%+v", code, rows)
		}
	})
	t.Run("help and version exit 0", func(t *testing.T) {
		code, out, _ := invoke(t, newFake("linux"), "--version")
		if code != 0 || !strings.Contains(out, "killport version") {
			t.Fatalf("code=%d out=%q", code, out)
		}
		code, _, _ = invoke(t, newFake("linux"), "--help")
		if code != 0 {
			t.Fatalf("help exit %d", code)
		}
	})
}

func TestResultsOrderedByPort(t *testing.T) {
	f := newFake("linux", "lsof")
	f.on(cmdLsof, ok(lsofEntry(30, "c", "*:3002")+lsofEntry(10, "a", "*:3000")), ok(""))
	_, out, _ := invoke(t, f, "--json", "3002", "3001", "3000")
	rows := decodeKill(t, out)
	var got []string
	for _, r := range rows {
		got = append(got, r.Port+":"+r.Status)
	}
	if !reflect.DeepEqual(got, []string{"3000:killed", "3001:free", "3002:killed"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestSamePIDOnTwoPortsKilledOnce(t *testing.T) {
	f, _ := scenario3000("linux")
	invoke(t, f, "--json", "3000", "3001")
	if len(f.kills) != 1 {
		t.Fatalf("kills = %v", f.kills)
	}
}

func TestManyPortsBoundedConcurrency(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString(lsofEntry(100+i, "n", "*:"+strconv.Itoa(4000+i)))
	}
	f := newFake("linux", "lsof").on(cmdLsof, ok(b.String()), ok(""))
	code, _, _ := invoke(t, f, "--json", "4000-4019")
	if code != 0 || len(f.kills) != 20 {
		t.Fatalf("code=%d kills=%d", code, len(f.kills))
	}
}

func TestVerboseLogsToStderrOnly(t *testing.T) {
	f, _ := scenario3000("linux")
	code, out, errOut := invoke(t, f, "--verbose", "--json", "3000")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	decodeKill(t, out) // stdout still pure JSON
	if !strings.Contains(errOut, "run: lsof") || !strings.Contains(errOut, "kill: pid 100") {
		t.Errorf("stderr = %q", errOut)
	}
	if strings.Contains(out, "DEBUG") {
		t.Errorf("debug leaked to stdout")
	}
	f, _ = scenario3000("linux")
	_, _, errOut = invoke(t, f, "3000")
	if strings.Contains(errOut, "run:") {
		t.Errorf("verbose logging without --verbose: %q", errOut)
	}
}

func TestCheckModeJSON(t *testing.T) {
	ss := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
		"LISTEN 0 1 127.0.0.53%lo:53 0.0.0.0:* users:((\"systemd-resolve\",pid=800,fd=1))\n" +
		"LISTEN 0 1 *:3000 *:* users:((\"node\",pid=100,fd=1))\n" +
		"LISTEN 0 1 *:3001 *:* users:((\"mystery-tool\",pid=101,fd=1))\n" +
		"LISTEN 0 1 *:3002 *:* users:((\"init\",pid=1,fd=1))\n" +
		"LISTEN 0 1 *:3003 *:*\n" +
		"LISTEN 0 1 *:3004 *:* users:((\"com.docker.backend\",pid=500,fd=1))\n"
	f := newFake("linux", "ss", "docker")
	f.on(cmdSS, ok(ss))
	f.on(cmdDockerPS, ok("abc123|web|0.0.0.0:8080->80/tcp\n"))
	code, out, _ := invoke(t, f, "--check", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(f.kills) != 0 || f.count("docker stop") != 0 {
		t.Fatal("check mode mutated")
	}
	byPort := map[string]CheckResult{}
	for _, r := range decodeCheck(t, out) {
		byPort[r.Port] = r
	}
	wantSafe := map[string]bool{"53": false, "3000": true, "3001": true, "3002": false, "3003": false, "3004": false, "8080": true}
	for p, want := range wantSafe {
		r, found := byPort[p]
		if !found {
			t.Fatalf("port %s missing from %v", p, byPort)
		}
		if r.SafeToKill != want {
			t.Errorf("port %s safe_to_kill = %v, want %v (%+v)", p, r.SafeToKill, want, r)
		}
	}
	if byPort["3003"].Category != catUnknown || byPort["3003"].PID != "" {
		t.Errorf("hidden owner row = %+v", byPort["3003"])
	}
	if byPort["8080"].Category != catApplication || !byPort["8080"].IsDocker {
		t.Errorf("docker row = %+v", byPort["8080"])
	}
	if byPort["53"].Category != catSystem {
		t.Errorf("53 = %+v", byPort["53"])
	}
	if !strings.Contains(out, `"safe_to_kill"`) {
		t.Error("safe_to_kill must always be present")
	}
}

func TestCheckModeFilteredAndFree(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			f, owner := scenario3000(goos)
			code, out, _ := invoke(t, f, "--check", "--json", "3000", "9999", "13000")
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			rows := decodeCheck(t, out)
			if len(rows) != 3 {
				t.Fatalf("rows = %+v", rows)
			}
			if rows[0].Port != "3000" || rows[0].PID == "" || rows[0].NoListener {
				t.Errorf("3000: %+v (owner %d)", rows[0], owner)
			}
			if rows[1].Port != "9999" || !rows[1].NoListener || rows[1].SafeToKill || rows[1].Category != "" {
				t.Errorf("9999: %+v", rows[1])
			}
			if len(f.kills) != 0 {
				t.Fatal("check mode killed")
			}
		})
	}
}

func TestCheckModeHuman(t *testing.T) {
	f, _ := scenario3000("linux")
	code, out, _ := invoke(t, f, "--check", "3000", "9999")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "Port 3000 is being used by node (PID 100)") || !strings.Contains(out, "likely safe to stop (name heuristic)") {
		t.Errorf("out = %q", out)
	}
	if !strings.Contains(out, "Port 9999 is currently free") || strings.Contains(out, "safe to kill") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "Application processes (2)") {
		t.Errorf("free port counted as application: %q", out)
	}
}

func TestCheckModeInvalidPortIsUsage(t *testing.T) {
	f := newFake("linux", "lsof")
	code, out, _ := invoke(t, f, "--check", "--json", "99999")
	if code != 2 || decodeErr(t, out)["code"] != "usage" || len(f.runs) != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
}
