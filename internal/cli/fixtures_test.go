package cli

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Fixture-driven tests. Every input comes from testdata/ (captured or
// synthetic, see testdata/README.md) except the few inline strings that
// exist to cover edge cases the files do not contain.

func fxRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + rel)
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return string(b)
}

// fxSet renders listeners as sorted "port:pid:process" strings.
func fxSet(ls []Listener) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%d:%d:%s", l.Port, l.PID, l.Process))
	}
	sort.Strings(out)
	return out
}

// fxAssertSet compares the listener set (order-independent) with want.
func fxAssertSet(t *testing.T, got []Listener, want ...string) {
	t.Helper()
	w := append([]string(nil), want...)
	sort.Strings(w)
	g := fxSet(got)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Errorf("listener set mismatch\n got  %v\n want %v", g, w)
	}
}

// fxDropLines removes every line that contains one of the substrings.
func fxDropLines(s string, subs ...string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		drop := false
		for _, sub := range subs {
			if strings.Contains(line, sub) {
				drop = true
				break
			}
		}
		if !drop {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

var (
	fxLinuxListeners  = []string{"3000:277:python3", "8080:278:python3", "13000:279:python3", "5432:280:nc"}
	fxDarwinListeners = []string{
		"3000:501:node", "5432:602:postgres", "8080:703:python3",
		"47801:804:com.docker.backend", "13000:1301:devserver", "30001:1401:gateway",
	}
)

func TestFixtureLsof(t *testing.T) {
	tests := []struct {
		name string
		file string
		want []string
	}{
		{"linux listen-only", "linux/lsof_listen.txt", fxLinuxListeners},
		// Unfiltered output: the ESTABLISHED client (pid 284, remote port 3000) must be excluded.
		{"linux all tcp", "linux/lsof_all_tcp.txt", fxLinuxListeners},
		{"darwin listen-only", "darwin/lsof_listen.txt", fxDarwinListeners},
		// Unfiltered output: curl (pid 905, remote port 3000) must be excluded.
		{"darwin all tcp", "darwin/lsof_all_tcp.txt", fxDarwinListeners},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fxAssertSet(t, parseLsof(fxRead(t, tt.file)), tt.want...)
		})
	}
}

func TestFixtureLsofNoClientLeak(t *testing.T) {
	for _, file := range []string{"linux/lsof_all_tcp.txt", "darwin/lsof_all_tcp.txt"} {
		for _, l := range parseLsof(fxRead(t, file)) {
			if l.Port == 38660 || l.Port == 52000 || l.PID == 284 || l.PID == 905 {
				t.Errorf("%s: client connection leaked as listener: %+v", file, l)
			}
		}
	}
}

// TestFixtureLsofOldCommandRegression documents the bug the new lsof command
// fixes: `lsof -i :3000 -t` returned the client as well as the server.
func TestFixtureLsofOldCommandRegression(t *testing.T) {
	pids := map[string]bool{}
	for _, f := range strings.Fields(fxRead(t, "linux/lsof_i_port_3000_t.txt")) {
		pids[f] = true
	}
	if !pids["277"] || !pids["284"] {
		t.Errorf("old command output should hold both server 277 and client 284, got %v", pids)
	}
	// The new enumeration must only see the server on port 3000.
	fxAssertSet(t, groupByPort(parseLsof(fxRead(t, "linux/lsof_all_tcp.txt")))[3000], "3000:277:python3")
}

func TestFixtureSS(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{"ss -ltnp", "linux/ss_ltnp.txt"},
		// All states: TIME-WAIT and ESTAB rows (pids 277 fd4, 284) must not leak in.
		{"ss -tanp", "linux/ss_tanp.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fxAssertSet(t, parseSS(fxRead(t, tt.file)), fxLinuxListeners...)
		})
	}
}

func TestFixtureDarwinNamedPorts(t *testing.T) {
	ls := normalizeListeners(parseLsof(fxRead(t, "darwin/lsof_all_tcp.txt")))
	byPort := groupByPort(ls)
	if n := len(byPort[3000]); n != 1 {
		t.Errorf("port 3000 should have one listener (pid 501 on v4+v6), got %+v", byPort[3000])
	}
	if n := len(byPort[13000]); n != 1 || byPort[13000][0].PID != 1301 {
		t.Errorf("port 13000 listener = %+v, want pid 1301", byPort[13000])
	}
	if n := len(byPort[30001]); n != 1 || byPort[30001][0].PID != 1401 {
		t.Errorf("port 30001 listener = %+v, want pid 1401", byPort[30001])
	}
}

func TestFixtureNetstat(t *testing.T) {
	// Expected listeners after normalisation: v4/v6 rows of 3000 collapse to one.
	// ESTABLISHED (4120 local 3000; 7788 remote 3000), TIME_WAIT (pid 0) and UDP 5100 are excluded.
	// The 8080 row uses the localized state word ABHÖREN and must still be a listener.
	ls := normalizeListeners(parseNetstat(fxRead(t, "windows/netstat_ano.txt")))
	fxAssertSet(t, ls,
		"3000:4120:",
		"135:1044:",
		"445:4:",
		"5432:6200:",
		"13000:6300:",
		"30001:6400:",
		"8080:6500:",
	)
}

func TestFixtureNetstatCRCRLF(t *testing.T) {
	// Inline: the doubled \r\r\n line endings the Windows capture had before normalisation.
	in := netstatHeader +
		"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       4120\r\r\n" +
		"  TCP    [::]:3000              [::]:0                 LISTENING       4120\r\r\n" +
		"  TCP    127.0.0.1:51000        127.0.0.1:3000         ESTABLISHED     7788\r\r\n"
	fxAssertSet(t, normalizeListeners(parseNetstat(in)), "3000:4120:")
}

func TestFixtureTasklist(t *testing.T) {
	got := parseTasklist(fxRead(t, "windows/tasklist_csv.txt"))
	want := map[int]string{
		4:    "System",
		0:    "System Idle Process",
		1044: "svchost.exe",
		4120: "node.exe",
		5100: "udpsvc.exe",
		6200: "postgres.exe",
		6300: "devserver.exe",
		6400: "gateway.exe",
		6500: "python.exe",
		7788: "chrome.exe",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tasklist map mismatch\n got  %v\n want %v", got, want)
	}
	if got[4] != "System" {
		t.Errorf("PID 4 name = %q, want System", got[4])
	}
}

func TestFixtureTasklistCRCRLF(t *testing.T) {
	in := "\"node.exe\",\"4120\",\"Console\",\"1\",\"45,236 K\"\r\r\n" +
		"INFO: No tasks are running which match the specified criteria.\r\r\n"
	got := parseTasklist(in)
	if !reflect.DeepEqual(got, map[int]string{4120: "node.exe"}) {
		t.Errorf("got %v", got)
	}
}

func TestFixtureDockerPS(t *testing.T) {
	owners := parseDockerPS(fxRead(t, "docker/docker_ps.txt"))
	wantPorts := []int{47801, 47802, 47810, 47811, 47812}
	var gotPorts []int
	for p := range owners {
		gotPorts = append(gotPorts, p)
	}
	sort.Ints(gotPorts)
	if !reflect.DeepEqual(gotPorts, wantPorts) {
		t.Fatalf("host ports = %v, want %v (47820/udp and unpublished 9000 must be excluded)", gotPorts, wantPorts)
	}
	for _, p := range wantPorts {
		cs := owners[p]
		if len(cs) != 1 || cs[0].ID != "2eebb490bf98" || cs[0].Name != "killport-fixture-ports" {
			t.Errorf("port %d owners = %+v", p, cs)
		}
	}
}

func TestFixtureDockerPSInlineRange(t *testing.T) {
	owners := parseDockerPS("abc123|web|0.0.0.0:8000-8002->8000-8002/tcp, :::8000-8002->8000-8002/tcp\n")
	var got []int
	for p := range owners {
		got = append(got, p)
	}
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{8000, 8001, 8002}) {
		t.Errorf("ports = %v, want [8000 8001 8002]", got)
	}
}

// fxLsofListenWithout277 is the post-kill lsof listing: the pid 277 block removed.
func fxLsofListenWithout277(t *testing.T) string {
	t.Helper()
	s := fxRead(t, "linux/lsof_listen.txt")
	block := "p277\ncpython3\nn*:3000\nTST=LISTEN\nTQR=0\nTQS=0\n"
	if !strings.Contains(s, block) {
		t.Fatalf("fixture block for pid 277 not found in lsof_listen.txt")
	}
	return strings.Replace(s, block, "", 1)
}

func TestFixtureRunLinuxKillOnlyServer(t *testing.T) {
	f := newFake("linux", "lsof")
	// The first listing is the unfiltered capture (defence in depth: the
	// client must never be picked up); the verification listing shows 3000 free.
	f.on(cmdLsof, ok(fxRead(t, "linux/lsof_all_tcp.txt")), ok(fxLsofListenWithout277(t)))
	code, out, errOut := invoke(t, f, "--json", "3000")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s stdout=%s", code, errOut, out)
	}
	if !reflect.DeepEqual(f.sortedKills(), []int{277}) {
		t.Fatalf("kills = %v, want exactly [277]", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.PID != "277" || r.ProcessName != "python3" || !r.Killed || r.Status != statusKilled || r.Action != actionKill {
		t.Errorf("row = %+v", r)
	}
}

func TestFixtureRunLinuxCheckSSFallback(t *testing.T) {
	// lsof missing: the ss path must be used.
	f := newFake("linux", "ss").on(cmdSS, ok(fxRead(t, "linux/ss_ltnp.txt")))
	code, out, errOut := invoke(t, f, "--check", "3000", "--json")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, errOut)
	}
	rows := decodeCheck(t, out)
	if len(rows) != 1 || rows[0].PID != "277" || rows[0].Port != "3000" {
		t.Fatalf("rows = %+v", rows)
	}
	if !rows[0].SafeToKill {
		t.Errorf("python3 server should be advisory safe_to_kill: %+v", rows[0])
	}
}

// fxWindowsFake returns a windows fake fed with the netstat and tasklist fixtures.
// after is the netstat listing returned by the verification pass.
func fxWindowsFake(t *testing.T, after string) *fakeSystem {
	t.Helper()
	netstat := fxRead(t, "windows/netstat_ano.txt")
	f := newFake("windows", "netstat", "tasklist")
	f.on(cmdNetstat, ok(netstat), ok(after))
	f.on(cmdTasklist, ok(fxRead(t, "windows/tasklist_csv.txt")))
	return f
}

// fxWindowsAfter3000 is netstat_ano.txt with the listeners of port 3000 removed.
func fxWindowsAfter3000(t *testing.T) string {
	t.Helper()
	return fxDropLines(fxRead(t, "windows/netstat_ano.txt"), "0.0.0.0:3000 ", "[::]:3000 ")
}

func TestFixtureRunWindowsKillJSON(t *testing.T) {
	f := fxWindowsFake(t, fxWindowsAfter3000(t))
	code, out, errOut := invoke(t, f, "--json", "3000")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s stdout=%s", code, errOut, out)
	}
	if !reflect.DeepEqual(f.sortedKills(), []int{4120}) {
		t.Fatalf("kills = %v, want exactly [4120] (never the client 7788)", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 || rows[0].PID != "4120" || rows[0].ProcessName != "node.exe" || !rows[0].Killed || rows[0].Status != statusKilled {
		t.Errorf("rows = %+v", rows)
	}
}

func TestFixtureRunWindowsSystemPIDRefused(t *testing.T) {
	f := fxWindowsFake(t, fxRead(t, "windows/netstat_ano.txt"))
	code, out, _ := invoke(t, f, "--json", "445")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if len(f.kills) != 0 {
		t.Fatalf("signalled PID 4: kills = %v", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 || rows[0].Status != statusFailed || rows[0].Killed || !strings.Contains(rows[0].Error, "PID 4") {
		t.Errorf("rows = %+v", rows)
	}
}

func TestFixtureRunWindowsDryRun(t *testing.T) {
	f := fxWindowsFake(t, fxRead(t, "windows/netstat_ano.txt"))
	code, out, errOut := invoke(t, f, "--dry-run", "--json", "3000")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, errOut)
	}
	if len(f.kills) != 0 {
		t.Fatalf("dry run signalled: %v", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Status != statusPlanned || r.Killed || !r.DryRun || r.PID != "4120" {
		t.Errorf("row = %+v", r)
	}
}

func TestFixtureRunDockerStopOnce(t *testing.T) {
	f := newFake("linux", "lsof", "docker")
	f.on(cmdLsof, ok(""))
	// First docker ps: the initial enumeration. Second: verification, container gone.
	f.on(cmdDockerPS, ok(fxRead(t, "docker/docker_ps.txt")), ok(""))
	f.on(dockerStopID+"2eebb490bf98", ok("2eebb490bf98\n"))
	code, out, errOut := invoke(t, f, "--json", "47801", "47802")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s stdout=%s", code, errOut, out)
	}
	if n := f.count("docker stop"); n != 1 {
		t.Fatalf("docker stop ran %d times: %v", n, f.runs)
	}
	if len(f.kills) != 0 {
		t.Errorf("native kill issued for a container port: %v", f.kills)
	}
	rows := decodeKill(t, out)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.Status != statusStopped || !r.Killed || !r.IsDocker || r.ContainerID != "2eebb490bf98" {
			t.Errorf("row = %+v", r)
		}
	}
}
