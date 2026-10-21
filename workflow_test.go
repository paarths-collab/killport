package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- test world --------------------------------------------------------------
//
// wfWorld is a tiny model of one machine's sockets and containers. It renders
// consistent /proc/net, lsof, netstat, tasklist and docker ps output into the
// fake system, so the socket table and every owner provider always agree and a
// test can change the world (a process exits, a supervisor rebinds, a
// container is replaced) and republish.

const (
	cmdDockerPSFull  = "docker ps --no-trunc --format {{.ID}}|{{.Names}}|{{.Ports}}"
	cmdLsofUDP       = "lsof -nP +c 0 -iUDP -FpcnT"
	cmdSSUDP         = "ss -lunp"
	cmdNetstatMacTCP = "netstat -an -p tcp"
	cmdNetstatMacUDP = "netstat -an -p udp"
)

type wfSock struct {
	proto  string // tcp | udp
	port   int    // LOCAL port
	remote int    // remote port of a connected socket (0 = none)
	state  string // tcp: "" (LISTEN) or "ESTABLISHED"
	pid    int    // 0 = exists in the socket table only
	name   string
	cid    string // container whose port proxy this socket is
}

type wfContainer struct {
	id, name string
	ports    []string // docker ps "Ports" entries
}

type wfWorld struct {
	mu         sync.Mutex
	f          *fakeSystem
	goos       string
	docker     bool
	dockerFail *fakeResp
	socks      []wfSock
	conts      []wfContainer
}

func newWfWorld(goos string, docker bool) *wfWorld {
	var tools []string
	if goos == "windows" {
		tools = []string{"netstat", "tasklist"}
	} else {
		tools = []string{"lsof"}
	}
	if docker {
		tools = append(tools, "docker")
	}
	w := &wfWorld{f: newFake(goos, tools...), goos: goos, docker: docker}
	return w
}

func wfStart(pid int) string { return strconv.Itoa(100000 + pid) }

// proc registers a live process with a deterministic start time.
func (w *wfWorld) proc(pid int, name string) {
	w.f.setProc(pid, wfStart(pid), name)
}

// listen adds a listening TCP socket / bound UDP socket owned by pid.
func (w *wfWorld) listen(proto string, port, pid int, name string) *wfWorld {
	w.mu.Lock()
	w.socks = append(w.socks, wfSock{proto: proto, port: port, pid: pid, name: name})
	w.mu.Unlock()
	if pid > 0 {
		w.proc(pid, name)
	}
	return w
}

// connect adds a client/connected socket whose REMOTE port is remote.
func (w *wfWorld) connect(proto string, local, remote, pid int, name string) *wfWorld {
	st := ""
	if proto == protoTCP {
		st = "ESTABLISHED"
	}
	w.mu.Lock()
	w.socks = append(w.socks, wfSock{proto: proto, port: local, remote: remote, state: st, pid: pid, name: name})
	w.mu.Unlock()
	if pid > 0 {
		w.proc(pid, name)
	}
	return w
}

// container adds a running container and one port-proxy socket per published
// TCP port. proxyPID 0 makes the proxy invisible to lsof (as for non-root).
func (w *wfWorld) container(id, name string, proxyPID int, ports ...int) *wfWorld {
	w.mu.Lock()
	c := wfContainer{id: id, name: name}
	for _, p := range ports {
		c.ports = append(c.ports, fmt.Sprintf("0.0.0.0:%d->80/tcp", p), fmt.Sprintf("[::]:%d->80/tcp", p))
		w.socks = append(w.socks, wfSock{proto: protoTCP, port: p, pid: proxyPID, name: "docker-proxy", cid: id})
	}
	w.conts = append(w.conts, c)
	w.mu.Unlock()
	if proxyPID > 0 {
		w.proc(proxyPID, "docker-proxy")
	}
	return w
}

func (w *wfWorld) removeContainer(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	conts := w.conts[:0:0]
	for _, c := range w.conts {
		if c.id != id {
			conts = append(conts, c)
		}
	}
	w.conts = conts
	socks := w.socks[:0:0]
	for _, s := range w.socks {
		if s.cid != id {
			socks = append(socks, s)
		}
	}
	w.socks = socks
}

// removePID drops every socket of pid and marks the process as exited.
func (w *wfWorld) removePID(pid int) {
	w.mu.Lock()
	socks := w.socks[:0:0]
	for _, s := range w.socks {
		if s.pid != pid {
			socks = append(socks, s)
		}
	}
	w.socks = socks
	w.mu.Unlock()
	w.f.exitProc(pid)
}

func (w *wfWorld) procNet(proto string) string {
	var b strings.Builder
	b.WriteString(procTCPHeader)
	i := 0
	for _, s := range w.socks {
		if s.proto != proto {
			continue
		}
		st := 0x0A
		switch {
		case proto == protoUDP:
			st = 0x07
		case s.state == "ESTABLISHED":
			st = 0x01
		}
		fmt.Fprintf(&b, "%4d: 00000000:%04X 00000000:%04X %02X 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000000000000000 100 0 0 10 0\n", i, s.port, s.remote, st, 1000+i)
		i++
	}
	return b.String()
}

func (w *wfWorld) lsof(proto string) string {
	pids := map[int]string{}
	for _, s := range w.socks {
		if s.proto == proto && s.pid > 0 {
			pids[s.pid] = s.name
		}
	}
	order := make([]int, 0, len(pids))
	for pid := range pids {
		order = append(order, pid)
	}
	sort.Ints(order)
	var b strings.Builder
	for _, pid := range order {
		fmt.Fprintf(&b, "p%d\nc%s\n", pid, pids[pid])
		n := 0
		for _, s := range w.socks {
			if s.proto != proto || s.pid != pid {
				continue
			}
			name := fmt.Sprintf("*:%d", s.port)
			if s.remote > 0 {
				name = fmt.Sprintf("127.0.0.1:%d->127.0.0.1:%d", s.port, s.remote)
			}
			fmt.Fprintf(&b, "f%d\nn%s\n", 20+n, name)
			if proto == protoTCP {
				st := "LISTEN"
				if s.state != "" {
					st = s.state
				}
				fmt.Fprintf(&b, "TST=%s\n", st)
			}
			n++
		}
	}
	return b.String()
}

func (w *wfWorld) macNetstat(proto string) string {
	var b strings.Builder
	b.WriteString("Active Internet connections (including servers)\nProto Recv-Q Send-Q  Local Address          Foreign Address        (state)\n")
	for _, s := range w.socks {
		if s.proto != proto {
			continue
		}
		local, foreign := fmt.Sprintf("*.%d", s.port), "*.*"
		if s.remote > 0 {
			local, foreign = fmt.Sprintf("127.0.0.1.%d", s.port), fmt.Sprintf("127.0.0.1.%d", s.remote)
		}
		switch {
		case proto == protoUDP:
			fmt.Fprintf(&b, "udp4       0      0  %s  %s\n", local, foreign)
		case s.state == "ESTABLISHED":
			fmt.Fprintf(&b, "tcp4       0      0  %s  %s  ESTABLISHED\n", local, foreign)
		default:
			fmt.Fprintf(&b, "tcp4       0      0  %s  %s  LISTEN\n", local, foreign)
		}
	}
	return b.String()
}

func (w *wfWorld) winNetstat() string {
	var b strings.Builder
	b.WriteString(netstatHeader)
	for _, s := range w.socks {
		switch {
		case s.proto == protoUDP:
			fmt.Fprintf(&b, "  UDP    0.0.0.0:%d           *:*                                    %d\n", s.port, s.pid)
		case s.state == "ESTABLISHED":
			fmt.Fprintf(&b, "  TCP    127.0.0.1:%d        127.0.0.1:%d         ESTABLISHED     %d\n", s.port, s.remote, s.pid)
		default:
			fmt.Fprintf(&b, "  TCP    0.0.0.0:%d           0.0.0.0:0              LISTENING       %d\n", s.port, s.pid)
		}
	}
	return b.String()
}

func (w *wfWorld) tasklist() string {
	seen := map[int]bool{}
	var b strings.Builder
	for _, s := range w.socks {
		if s.pid > 0 && !seen[s.pid] {
			seen[s.pid] = true
			fmt.Fprintf(&b, "\"%s\",\"%d\",\"Console\",\"1\",\"1 K\"\r\n", s.name, s.pid)
		}
	}
	return b.String()
}

// publish renders the world into the fake system.
func (w *wfWorld) publish() *wfWorld {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.f
	switch w.goos {
	case "linux":
		f.setFile("/proc/net/tcp", w.procNet(protoTCP))
		f.setFile("/proc/net/udp", w.procNet(protoUDP))
		f.setResp(cmdLsof, ok(w.lsof(protoTCP)))
		f.setResp(cmdLsofUDP, ok(w.lsof(protoUDP)))
	case "darwin":
		f.setResp(cmdNetstatMacTCP, ok(w.macNetstat(protoTCP)))
		f.setResp(cmdNetstatMacUDP, ok(w.macNetstat(protoUDP)))
		f.setResp(cmdLsof, ok(w.lsof(protoTCP)))
		f.setResp(cmdLsofUDP, ok(w.lsof(protoUDP)))
	case "windows":
		f.setResp(cmdNetstat, ok(w.winNetstat()))
		f.setResp(cmdTasklist, ok(w.tasklist()))
	}
	if w.docker {
		if w.dockerFail != nil {
			f.setResp(cmdDockerPSFull, *w.dockerFail)
		} else {
			var b strings.Builder
			for _, c := range w.conts {
				fmt.Fprintf(&b, "%s|%s|%s\n", c.id, c.name, strings.Join(c.ports, ", "))
			}
			f.setResp(cmdDockerPSFull, ok(b.String()))
		}
	}
	return w
}

// ---- fake helpers ------------------------------------------------------------

func (f *fakeSystem) setResp(cmd string, r ...fakeResp) {
	f.mu.Lock()
	f.responses[cmd] = r
	f.mu.Unlock()
}

func (f *fakeSystem) setFile(path, content string) {
	f.mu.Lock()
	f.files[path] = []byte(content)
	f.mu.Unlock()
}

func (f *fakeSystem) signalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.signals)
}

func (f *fakeSystem) runCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count(prefix)
}

func (f *fakeSystem) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.kills)
}

// assertNoMutation fails if anything was signalled, killed or stopped.
func assertNoMutation(t *testing.T, f *fakeSystem) {
	t.Helper()
	if n := f.signalCount(); n != 0 {
		t.Errorf("signals delivered: %v", f.signals)
	}
	if n := f.killCount(); n != 0 {
		t.Errorf("kills: %v", f.kills)
	}
	for _, r := range f.runs {
		if strings.HasPrefix(r, "docker stop") || strings.HasPrefix(r, "docker kill") || strings.HasPrefix(r, "docker rm") {
			t.Errorf("mutating docker command: %s", r)
		}
	}
}

// hookFake lets a test react to commands (e.g. remove a container from the
// world once `docker stop` ran).
type hookFake struct {
	*fakeSystem
	afterRun func(key string, err error)
}

func (h *hookFake) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := h.fakeSystem.Run(ctx, name, args...)
	if h.afterRun != nil {
		h.afterRun(strings.Join(append([]string{name}, args...), " "), err)
	}
	return out, err
}

// wfRun runs killport with the given stdin against sys and fails on commands
// the fake did not expect.
func wfRun(t *testing.T, sys system, f *fakeSystem, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runIO(args, strings.NewReader(stdin), &out, &errb, sys)
	if f != nil {
		f.mu.Lock()
		unexpected := append([]string(nil), f.unexpected...)
		f.mu.Unlock()
		if len(unexpected) > 0 {
			t.Errorf("unexpected commands: %v", unexpected)
		}
	}
	return code, out.String(), errb.String()
}

// decodeDoc decodes stdout as EXACTLY one JSON document into v.
func decodeDoc(t *testing.T, out string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, out)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout holds more than one JSON document:\n%s", out)
	}
}

func decodePlanDoc(t *testing.T, out string) planDoc {
	t.Helper()
	var d planDoc
	decodeDoc(t, out, &d)
	return d
}

func decodeApplyDoc(t *testing.T, out string) applyDoc {
	t.Helper()
	var d applyDoc
	decodeDoc(t, out, &d)
	return d
}

// planFile runs `plan --json -o <file>` and returns the path and decoded plan.
func planFile(t *testing.T, sys system, f *fakeSystem, args ...string) (string, planDoc) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	all := append([]string{"plan", "--json", "-o", path}, args...)
	_, out, errOut := wfRun(t, sys, f, "", all...)
	if out == "" {
		t.Fatalf("plan produced no output; stderr=%s", errOut)
	}
	doc := decodePlanDoc(t, out)
	return path, doc
}

func resultOf(d applyDoc, id string) applyResource {
	for _, r := range d.Resources {
		if r.ResourceID == id {
			return r
		}
	}
	return applyResource{}
}

func endpointStatus(d applyDoc, port int) string {
	for _, e := range d.Endpoints {
		if e.Port == port {
			return e.Status
		}
	}
	return ""
}

func hex64(n int) string { return fmt.Sprintf("%064x", n) }

var errWrapperTimeout = &cmdError{Name: "docker", Code: -1, Err: fmt.Errorf("timed out: %w", context.DeadlineExceeded)}

func stopCmd(id string) string { return "docker stop -t 10 " + id }

// ---- tests of the building blocks -------------------------------------------------

func TestExitClassPrecedence(t *testing.T) {
	order := []int{exitUsage, exitFailed, exitUncertain, exitFreshPlan, exitOK}
	for i, a := range order {
		for j, b := range order {
			want := a
			if j < i {
				want = b
			}
			if got := worseExit(a, b); got != want {
				t.Errorf("worseExit(%d,%d) = %d, want %d", a, b, got, want)
			}
		}
	}
	names := map[int]string{0: "ok", 1: "failed", 2: "usage", 3: "needs_fresh_plan", 4: "uncertain"}
	for code, name := range names {
		if exitClassName(code) != name {
			t.Errorf("exitClassName(%d) = %q", code, exitClassName(code))
		}
	}
}

func TestParseNetstatRowsTCPAndUDP(t *testing.T) {
	out := netstatHeader +
		"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       1234\n" +
		"  TCP    127.0.0.1:50001        127.0.0.1:3000         ESTABLISHED     555\n" +
		"  TCP    0.0.0.0:3001           0.0.0.0:0              LISTENING       +7\n" + // malformed PID -> kept as PID 0
		"  TCP    0.0.0.0:3002           0.0.0.0:0              LISTENING       0\n" +
		"  UDP    0.0.0.0:3000           *:*                                    777\n" +
		"  UDP    [::]:5353              *:*                                    888\n" +
		"  UDP    127.0.0.1:60000        1.2.3.4:3000                           999\n"
	tcp := parseNetstatRows(out, protoTCP)
	want := []netRow{{3000, 1234, "0.0.0.0:3000"}, {3001, 0, "0.0.0.0:3001"}, {3002, 0, "0.0.0.0:3002"}}
	if !reflect.DeepEqual(tcp, want) {
		t.Errorf("tcp rows = %+v, want %+v", tcp, want)
	}
	udp := parseNetstatRows(out, protoUDP)
	// A UDP row is matched by its LOCAL port only: 60000, never the foreign 3000.
	wantU := []netRow{{3000, 777, "0.0.0.0:3000"}, {5353, 888, "[::]:5353"}, {60000, 999, "127.0.0.1:60000"}}
	if !reflect.DeepEqual(udp, wantU) {
		t.Errorf("udp rows = %+v, want %+v", udp, wantU)
	}
	// parseNetstat keeps its TCP-listener-only behaviour: no UDP rows, no
	// established connections.
	for _, l := range parseNetstat(out) {
		if l.Port == 5353 || l.Port == 60000 || l.Port == 50001 {
			t.Errorf("parseNetstat returned a non-listener row: %+v", l)
		}
	}
}

func TestParseDockerPortsProto(t *testing.T) {
	field := "0.0.0.0:8080->80/tcp, 0.0.0.0:5353->53/udp, [::]:5353->53/udp, 9000/tcp"
	if got := parseDockerPortsProto(field, "udp"); !reflect.DeepEqual(got, []int{5353}) {
		t.Errorf("udp = %v", got)
	}
	if got := parseDockerPortsProto(field, "tcp"); !reflect.DeepEqual(got, []int{8080}) {
		t.Errorf("tcp = %v", got)
	}
	if !reflect.DeepEqual(parseDockerPorts(field), []int{8080}) {
		t.Errorf("parseDockerPorts changed")
	}
}

func TestParseDockerPSFullKeepsFullIDs(t *testing.T) {
	id := hex64(0xabc)
	out := id + "|web|0.0.0.0:3000->80/tcp, 0.0.0.0:3001->81/tcp\nabc123|short|0.0.0.0:4000->80/tcp\nzzz|none|\n"
	cs := parseDockerPSFull(out, protoTCP)
	if len(cs) != 2 {
		t.Fatalf("containers = %+v", cs)
	}
	var full, short *containerObs
	for i := range cs {
		if cs[i].ID == id {
			full = &cs[i]
		} else {
			short = &cs[i]
		}
	}
	if full == nil || !full.FullID || !reflect.DeepEqual(full.Ports, []int{3000, 3001}) {
		t.Errorf("full = %+v", full)
	}
	if short == nil || short.FullID {
		t.Errorf("short = %+v", short)
	}
	if isFullContainerID(strings.ToUpper(id)) {
		t.Errorf("uppercase ids must not count as full ids")
	}
}

// ---- plan / apply behaviour ----------------------------------------------------------

func TestInspectHumanAndJSON(t *testing.T) {
	w := newWfWorld("linux", true)
	w.listen("tcp", 3000, 100, "node")
	id := hex64(0xd0c)
	w.container(id, "web", 900, 3001)
	w.publish()

	code, out, errOut := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000", "3001", "3002")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, errOut)
	}
	var d inspectDoc
	decodeDoc(t, out, &d)
	if d.SchemaVersion != 1 || d.Kind != "killport.inspect" || d.ExitClass != "ok" || d.OS != "linux" || d.ObservedAt != "2026-10-21T12:00:00Z" {
		t.Errorf("doc = %+v", d)
	}
	if len(d.Endpoints) != 3 {
		t.Fatalf("endpoints = %+v", d.Endpoints)
	}
	e0, e1, e2 := d.Endpoints[0], d.Endpoints[1], d.Endpoints[2]
	if !e0.Occupied || len(e0.Owners) != 1 || e0.Owners[0].PID != 100 || e0.Owners[0].IdentityStrength != "start_time" || e0.Owners[0].StartTime != wfStart(100) {
		t.Errorf("3000 = %+v", e0)
	}
	if !e1.Occupied || len(e1.Containers) != 1 || e1.Containers[0].ContainerID != id || e1.Containers[0].Name != "web" {
		t.Errorf("3001 = %+v", e1)
	}
	if e2.Occupied || e2.Visibility != visComplete || len(e2.Owners) != 0 {
		t.Errorf("3002 = %+v", e2)
	}

	code, out, _ = wfRun(t, w.f, w.f, "", "inspect", "3000", "3002")
	if code != 0 || json.Valid([]byte(out)) {
		t.Fatalf("human inspect: code=%d out=%q", code, out)
	}
	for _, want := range []string{"tcp 3000: occupied (visibility: complete_for_requested_scope)", "pid 100 (node)", "tcp 3002: free (visibility: complete_for_requested_scope)", "Result: ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("human inspect lacks %q:\n%s", want, out)
		}
	}

	// No ports: every occupied port.
	_, out, _ = wfRun(t, w.f, w.f, "", "inspect", "--json")
	decodeDoc(t, out, &d)
	var ports []int
	for _, e := range d.Endpoints {
		ports = append(ports, e.Port)
	}
	if !d.Request.All || !reflect.DeepEqual(ports, []int{3000, 3001}) {
		t.Errorf("inspect all: %+v ports=%v", d.Request, ports)
	}
}

func TestInspectExitClasses(t *testing.T) {
	t.Run("provider failure is failed", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.publish()
		w.f.setResp(cmdLsof, fail(2, "lsof: boom"))
		code, out, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		var d inspectDoc
		decodeDoc(t, out, &d)
		if code != 1 || d.ExitClass != "failed" || d.Endpoints[0].Visibility != visProviderUnavailable {
			t.Errorf("code=%d doc=%+v", code, d)
		}
	})
	t.Run("hidden occupant is uncertain", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 0, "")
		w.publish()
		code, out, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		var d inspectDoc
		decodeDoc(t, out, &d)
		if code != 4 || d.ExitClass != "uncertain" || d.Endpoints[0].Visibility != visPermissionLimited || !d.Endpoints[0].Unattributed || !d.Endpoints[0].Occupied {
			t.Errorf("code=%d doc=%+v", code, d)
		}
	})
	t.Run("bad protocol is usage", func(t *testing.T) {
		code, out, _ := wfRun(t, newFake("linux", "lsof"), nil, "", "inspect", "--protocol", "sctp", "--json", "3000")
		m := decodeErr(t, out)
		if code != 2 || m["code"] != "usage" || m["exit_class"] != "usage" {
			t.Errorf("code=%d out=%s", code, out)
		}
	})
}

func TestPlanForceAndSignals(t *testing.T) {
	for _, tc := range []struct {
		goos, name string
		args       []string
		wantSig    string
		wantForce  bool
	}{
		{"linux", "node", []string{"3000"}, "SIGTERM", false},
		{"linux", "node", []string{"3000", "--force"}, "SIGKILL", true},
		{"darwin", "node", []string{"3000"}, "SIGTERM", false},
		{"darwin", "node", []string{"--force", "3000"}, "SIGKILL", true},
		{"windows", "node.exe", []string{"3000"}, "terminate", false},
		{"windows", "node.exe", []string{"3000", "--force"}, "terminate", true},
	} {
		t.Run(tc.goos+" "+strings.Join(tc.args, " "), func(t *testing.T) {
			w := newWfWorld(tc.goos, false)
			w.listen("tcp", 3000, 1234, tc.name)
			w.publish()
			code, out, errOut := wfRun(t, w.f, w.f, "", append([]string{"plan", "--json"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
			}
			d := decodePlanDoc(t, out)
			if len(d.Resources) != 1 {
				t.Fatalf("resources = %+v", d.Resources)
			}
			r := d.Resources[0]
			if r.Signal != tc.wantSig || r.Action != "signal" || d.Request.Force != tc.wantForce || r.Provider != "native" {
				t.Errorf("resource = %+v request=%+v", r, d.Request)
			}
			if d.OS != tc.goos || r.StartTime != wfStart(1234) || r.ResourceID != "proc:1234:"+wfStart(1234) {
				t.Errorf("plan = %+v", d)
			}
			assertNoMutation(t, w.f)
		})
	}
}

func TestPlanDedupesOneProcessOwningManyPorts(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			w := newWfWorld(goos, false)
			w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 100, "node").listen("tcp", 3002, 200, "vite")
			w.publish()
			_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000-3002")
			d := decodePlanDoc(t, out)
			if len(d.Resources) != 2 {
				t.Fatalf("resources = %+v", d.Resources)
			}
			if r := d.Resources[0]; r.PID != 100 || !reflect.DeepEqual(r.Ports, []int{3000, 3001}) {
				t.Errorf("first = %+v", r)
			}
			if r := d.Resources[1]; r.PID != 200 || !reflect.DeepEqual(r.Ports, []int{3002}) {
				t.Errorf("second = %+v", r)
			}
			if !reflect.DeepEqual(d.Endpoints[0].Resources, d.Endpoints[1].Resources) {
				t.Errorf("endpoint mapping = %+v", d.Endpoints)
			}
		})
	}
}

func TestPlanRefusalRules(t *testing.T) {
	cases := []struct {
		name   string
		goos   string
		pid    int
		pname  string
		reason string
		hide   bool
	}{
		{"pid 1", "linux", 1, "init", "refusing to kill PID 1", false},
		{"own pid", "linux", fakeOwnPID, "killport", "killport itself", false},
		{"windows system", "windows", 4, "System", "System process", false},
		{"docker infra no container", "linux", 800, "docker-proxy", "Docker infrastructure", false},
		{"docker desktop backend", "darwin", 801, "com.docker.backend", "Docker infrastructure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWfWorld(tc.goos, false)
			w.listen("tcp", 3000, tc.pid, tc.pname)
			w.publish()
			code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
			d := decodePlanDoc(t, out)
			if code != 1 || d.ExitClass != "failed" {
				t.Errorf("exit=%d class=%s", code, d.ExitClass)
			}
			if len(d.Resources) != 0 || len(d.Refused) != 1 || !strings.Contains(d.Refused[0].Reason, tc.reason) || d.Refused[0].PID != tc.pid {
				t.Fatalf("plan = %+v", d)
			}
			if len(d.Endpoints[0].Refused) != 1 {
				t.Errorf("endpoint refusal missing: %+v", d.Endpoints[0])
			}
			assertNoMutation(t, w.f)
		})
	}

	t.Run("unattributed ss row", func(t *testing.T) {
		f := newFake("linux", "ss")
		f.setFile("/proc/net/tcp", procTCPHeader+"   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n")
		f.on(cmdSS, ok(ssHeader+"LISTEN 0 128 *:3000 *:*\n"))
		code, out, _ := wfRun(t, f, f, "", "plan", "--json", "3000")
		d := decodePlanDoc(t, out)
		if code != 1 || len(d.Resources) != 0 || len(d.Refused) != 1 || d.Refused[0].PID != 0 || !strings.Contains(d.Refused[0].Reason, "not visible") {
			t.Fatalf("code=%d plan=%+v", code, d)
		}
		if d.Endpoints[0].Visibility != visPermissionLimited {
			t.Errorf("visibility = %s", d.Endpoints[0].Visibility)
		}
	})

	t.Run("ss row without users is unattributed even without a socket table", func(t *testing.T) {
		f := newFake("linux", "ss")
		f.on(cmdSS, ok(ssHeader+"LISTEN 0 128 *:3000 *:*\n"))
		code, out, _ := wfRun(t, f, f, "", "plan", "--json", "3000")
		d := decodePlanDoc(t, out)
		if code != 1 || d.Endpoints[0].Visibility != visPermissionLimited || !d.Endpoints[0].Occupied || len(d.Refused) != 1 {
			t.Fatalf("code=%d plan=%+v", code, d)
		}
	})
}

func TestPlanRefusesProcessWithoutIdentity(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 101, "node")
	w.publish()
	w.f.setProcStrength(100, strengthNone)
	w.f.identErrs[101] = &os.PathError{Op: "open", Path: "x", Err: os.ErrPermission}
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000", "3001")
	d := decodePlanDoc(t, out)
	if code != 1 || len(d.Resources) != 0 || len(d.Refused) != 2 {
		t.Fatalf("code=%d plan=%+v", code, d)
	}
	for _, r := range d.Refused {
		if !strings.Contains(r.Reason, "cannot bind process identity") {
			t.Errorf("reason = %q", r.Reason)
		}
	}
	if d.Endpoints[1].Visibility != visPermissionLimited {
		t.Errorf("denied identity should be permission_limited: %s", d.Endpoints[1].Visibility)
	}
}

func TestVisibilityPermissionLimitedWhenLsofCannotSeeOccupant(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 0, "") // in the socket table, invisible to lsof
	w.listen("tcp", 3001, 100, "node")
	w.publish()
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000", "3001", "3002")
	d := decodePlanDoc(t, out)
	got := []string{d.Endpoints[0].Visibility, d.Endpoints[1].Visibility, d.Endpoints[2].Visibility}
	want := []string{visPermissionLimited, visComplete, visComplete}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visibility = %v, want %v", got, want)
	}
	if !d.Endpoints[0].Occupied || d.Endpoints[2].Occupied {
		t.Errorf("occupied = %+v", d.Endpoints)
	}
	if code != 1 {
		t.Errorf("exit = %d (a hidden occupant is also a refusal)", code)
	}
}

func TestVisibilityNoneVisibleWhenProcNetUnreadable(t *testing.T) {
	f := newFake("linux", "lsof")
	f.on(cmdLsof, fail(1, ""))
	code, out, errOut := wfRun(t, f, f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	if d.Endpoints[0].Visibility != visNoneVisible || d.Endpoints[0].Occupied {
		t.Errorf("endpoint = %+v", d.Endpoints[0])
	}
	if code != 4 || d.ExitClass != "uncertain" {
		t.Errorf("code=%d class=%s", code, d.ExitClass)
	}
	if !strings.Contains(errOut, "socket table unavailable") {
		t.Errorf("stderr should explain: %q", errOut)
	}
	// An endpoint is never "free" without complete visibility: apply must
	// not report free either.
	path, _ := planFile(t, f, f, "3000")
	code, out, _ = wfRun(t, f, f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if ad.Endpoints[0].Status != epNotVerifiable || code != 4 {
		t.Errorf("apply code=%d endpoints=%+v", code, ad.Endpoints)
	}
}

func TestVisibilityContainerProxyIsExplained(t *testing.T) {
	// The container's port proxy is invisible to lsof (non-root): the socket
	// is explained by the container, so visibility stays complete.
	w := newWfWorld("linux", true)
	w.container(hex64(1), "web", 0, 3000)
	w.publish()
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	if code != 0 || d.Endpoints[0].Visibility != visComplete || len(d.Resources) != 1 || len(d.Refused) != 0 {
		t.Fatalf("code=%d plan=%+v", code, d)
	}
}

func TestPlanDockerWithNonFullIDIsRefused(t *testing.T) {
	w := newWfWorld("linux", true)
	w.container("abc123def456", "web", 0, 3000)
	w.publish()
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	if code != 1 || len(d.Resources) != 0 || len(d.Refused) != 1 || !strings.Contains(d.Refused[0].Reason, "64-hex") {
		t.Fatalf("code=%d plan=%+v", code, d)
	}
}

func TestPlanIDAndNoPII(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	if !strings.HasPrefix(d.PlanID, "sha256:") || len(d.PlanID) != len("sha256:")+64 {
		t.Fatalf("plan_id = %q", d.PlanID)
	}
	if d.PlanID != computePlanID(d) {
		t.Errorf("plan_id does not recompute")
	}
	// Independent recomputation from the raw JSON: blank plan_id, re-encode
	// compactly in struct order.
	blank := d
	blank.PlanID = ""
	raw, _ := json.Marshal(blank)
	if computePlanID(d) != computePlanID(blank) || len(raw) == 0 {
		t.Errorf("plan_id must not depend on its own value")
	}
	if d.ActionsExecuted != 0 {
		t.Errorf("actions_executed = %d", d.ActionsExecuted)
	}
	for _, banned := range []string{"/Users", "/home", "hostname", "USER=", "cmdline"} {
		if strings.Contains(out, banned) {
			t.Errorf("plan leaks %q:\n%s", banned, out)
		}
	}
	var raw2 map[string]any
	decodeDoc(t, out, &raw2)
	want := []string{"schema_version", "kind", "plan_id", "tool_version", "os", "created_at", "expires_at", "observation_id", "request", "endpoints", "resources", "refused", "actions_executed", "exit_class"}
	var got []string
	for k := range raw2 {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan keys = %v, want %v", got, want)
	}
}

func TestApplyTamperedPlanIsUsageError(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	path, d := planFile(t, w.f, w.f, "3000")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	edits := map[string]func(string) string{
		"port":     func(s string) string { return strings.Replace(s, "3000", "3001", 1) },
		"pid":      func(s string) string { return strings.Replace(s, `"pid": 100`, `"pid": 101`, 1) },
		"signal":   func(s string) string { return strings.Replace(s, "SIGTERM", "SIGKILL", 1) },
		"expires":  func(s string) string { return strings.Replace(s, "12:05:00", "23:05:00", 1) },
		"plan_id":  func(s string) string { return strings.Replace(s, d.PlanID, "sha256:"+strings.Repeat("0", 64), 1) },
		"unknown":  func(s string) string { return strings.Replace(s, `"kind"`, `"extra": 1, "kind"`, 1) },
		"trailing": func(s string) string { return s + `{"x":1}` },
		"garbage":  func(s string) string { return "not json" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			edited := edit(string(data))
			if edited == string(data) {
				t.Fatalf("edit %s did not change the plan", name)
			}
			p := filepath.Join(t.TempDir(), "p.json")
			if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			before := w.f.signalCount()
			code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", p)
			m := decodeErr(t, out)
			if code != 2 || m["code"] != "usage" || m["exit_class"] != "usage" || m["error"] == "" {
				t.Fatalf("code=%d out=%s", code, out)
			}
			if name == "port" || name == "pid" || name == "signal" || name == "expires" || name == "plan_id" {
				if !strings.Contains(m["error"], "modified or corrupted") {
					t.Errorf("error = %q", m["error"])
				}
			}
			if w.f.signalCount() != before {
				t.Errorf("a tampered plan signalled something")
			}
		})
	}
	// Unmodified plan still applies afterwards.
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	if code, _, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path); code != 0 {
		t.Errorf("the untouched plan should apply, exit %d", code)
	}
}

// clonePlan deep-copies a plan through JSON, so a forged case never shares a
// backing array (resources, ports, endpoint references) with the base plan or
// with another case.
func clonePlan(t *testing.T, base planDoc) planDoc {
	t.Helper()
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var d planDoc
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// forgePlan builds a self-consistent plan (valid plan_id) from a deep copy of
// base with a mutation, to show that validation rejects dangerous content even
// when the checksum fits.
func forgePlan(t *testing.T, base planDoc, mutate func(*planDoc)) string {
	t.Helper()
	d := clonePlan(t, base)
	mutate(&d)
	d.PlanID = computePlanID(d)
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// resIdx returns the index of the first resource of the given provider.
func resIdx(d *planDoc, provider string) int {
	for i, r := range d.Resources {
		if r.Provider == provider {
			return i
		}
	}
	panic("no " + provider + " resource in the base plan")
}

// retarget replaces resource i and rewrites every endpoint reference to it, so
// the forged plan stays consistent and is rejected for the intended reason
// only.
func retarget(d *planDoc, i int, r planResource) {
	old := d.Resources[i].ResourceID
	d.Resources[i] = r
	for e := range d.Endpoints {
		for j, id := range d.Endpoints[e].Resources {
			if id == old {
				d.Endpoints[e].Resources[j] = r.ResourceID
			}
		}
	}
}

// forgedProc / forgedContainer are valid resources to be spoiled by one field.
func forgedProc(edit func(*planResource)) func(*planDoc) {
	return func(d *planDoc) {
		i := resIdx(d, providerNative)
		r := d.Resources[i]
		r.Ports = append([]int(nil), r.Ports...)
		edit(&r)
		retarget(d, i, r)
	}
}

func forgedContainer(edit func(*planResource)) func(*planDoc) {
	return func(d *planDoc) {
		i := resIdx(d, providerDocker)
		r := d.Resources[i]
		r.Ports = append([]int(nil), r.Ports...)
		edit(&r)
		retarget(d, i, r)
	}
}

// decodeAndValidate is the pure part of apply's plan intake.
func decodeAndValidate(data string) error {
	doc, err := decodePlan([]byte(data))
	if err != nil {
		return err
	}
	return validatePlan(doc)
}

func TestApplyRejectsForgedDangerousPlans(t *testing.T) {
	// One valid base plan per OS: a native process on 3000 and (linux) a
	// container on 3001. Every case below is a deep copy of it with ONE
	// spoiled field, re-checksummed, and must be rejected for that reason.
	lin := newWfWorld("linux", true)
	lin.listen("tcp", 3000, 100, "node")
	lin.container(hex64(0xd0c), "web", 0, 3001)
	lin.publish()
	_, linBase := planFile(t, lin.f, lin.f, "3000", "3001")
	if len(linBase.Resources) != 2 || len(linBase.Endpoints) != 2 {
		t.Fatalf("base plan = %+v", linBase)
	}
	win := newWfWorld("windows", false)
	win.listen("tcp", 3000, 1234, "node.exe")
	win.publish()
	_, winBase := planFile(t, win.f, win.f, "3000")

	noop := func(*planDoc) {}
	t.Run("controls: the unspoiled plans pass the whole intake", func(t *testing.T) {
		for name, tc := range map[string]struct {
			base   planDoc
			mutate func(*planDoc)
		}{
			"linux":                             {linBase, noop},
			"windows":                           {winBase, noop},
			"pid 4 is an ordinary pid on linux": {linBase, forgedProc(func(r *planResource) { r.PID = 4; r.ResourceID = procResourceID(4, r.StartTime) })},
			"pid 2 is accepted (no rule)":       {linBase, forgedProc(func(r *planResource) { r.PID = 2; r.ResourceID = procResourceID(2, r.StartTime) })},
		} {
			if err := decodeAndValidate(forgePlan(t, tc.base, tc.mutate)); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		// And the harness itself must not have disturbed the base.
		if err := decodeAndValidate(forgePlan(t, linBase, noop)); err != nil {
			t.Fatal(err)
		}
	})

	badID := func(id string) func(*planDoc) {
		return forgedContainer(func(r *planResource) { r.ContainerID = id; r.ResourceID = containerResourceID(id) })
	}
	badPID := func(pid int) func(*planDoc) {
		return forgedProc(func(r *planResource) { r.PID = pid; r.ResourceID = procResourceID(pid, r.StartTime) })
	}
	type forged struct {
		name   string
		base   *planDoc
		mutate func(*planDoc)
		want   string // substring of the rejection
	}
	lb, wb := &linBase, &winBase
	cases := []forged{
		// protected and out-of-range PIDs
		{"pid 1", lb, badPID(1), "targets protected PID 1"},
		{"pid 0", lb, badPID(0), "targets protected PID 0"},
		{"negative pid", lb, badPID(-5), "targets protected PID -5"},
		{"windows system pid 4", wb, forgedProc(func(r *planResource) { r.PID = 4; r.ResourceID = procResourceID(4, r.StartTime) }), "Windows System process"},
		{"pid beyond 32 bits aliasing pid 4", wb, badPID(1<<32 + 4), "out of range"},
		{"pid just beyond int32", lb, badPID(math.MaxInt32 + 1), "out of range"},
		// duplicates
		{"duplicate resources", lb, func(d *planDoc) { d.Resources = append(d.Resources, d.Resources[resIdx(d, providerNative)]) }, "duplicate resource"},
		{"duplicate container", lb, func(d *planDoc) { d.Resources = append(d.Resources, d.Resources[resIdx(d, providerDocker)]) }, "duplicate resource"},
		// container ids: option injection and malformed ids
		{"container id -t", lb, badID("-t"), "invalid container id"},
		{"container id --all", lb, badID("--all"), "invalid container id"},
		{"container id --signal=KILL", lb, badID("--signal=KILL"), "invalid container id"},
		{"container id 63 hex", lb, badID(strings.Repeat("a", 63)), "invalid container id"},
		{"container id 65 hex", lb, badID(strings.Repeat("a", 65)), "invalid container id"},
		{"container id uppercase", lb, badID(strings.Repeat("A", 64)), "invalid container id"},
		{"container id non-hex", lb, badID(strings.Repeat("g", 64)), "invalid container id"},
		{"container id empty", lb, badID(""), "invalid container id"},
		// ports
		{"port outside request", lb, forgedProc(func(r *planResource) { r.Ports = []int{4000} }), "which was not requested"},
		{"container port outside request", lb, forgedContainer(func(r *planResource) { r.Ports = []int{3000, 4000} }), "which was not requested"},
		{"resource without ports", lb, forgedProc(func(r *planResource) { r.Ports = []int{} }), "has no ports"},
		{"request ports unsorted", lb, func(d *planDoc) { d.Request.Ports = []int{3001, 3000} }, "unique, ascending"},
		{"request port zero", lb, func(d *planDoc) { d.Request.Ports = []int{0, 3000, 3001} }, "unique, ascending"},
		{"request port too large", lb, func(d *planDoc) { d.Request.Ports = append(d.Request.Ports, 70000) }, "unique, ascending"},
		{"request without ports", lb, func(d *planDoc) { d.Request.Ports = []int{} }, "no ports"},
		// signals
		{"signal SIGHUP", lb, forgedProc(func(r *planResource) { r.Signal = "SIGHUP" }), `has signal "SIGHUP", expected "SIGTERM"`},
		{"signal empty", lb, forgedProc(func(r *planResource) { r.Signal = "" }), `has signal "", expected "SIGTERM"`},
		{"signal lowercase", lb, forgedProc(func(r *planResource) { r.Signal = "sigterm" }), `has signal "sigterm"`},
		{"terminate on linux", lb, forgedProc(func(r *planResource) { r.Signal = sigTerminate }), `has signal "terminate", expected "SIGTERM"`},
		{"SIGKILL without force", lb, forgedProc(func(r *planResource) { r.Signal = sigKill }), `has signal "SIGKILL", expected "SIGTERM"`},
		{"SIGTERM although force", lb, func(d *planDoc) { d.Request.Force = true }, `has signal "SIGTERM", expected "SIGKILL"`},
		{"SIGTERM on windows", wb, forgedProc(func(r *planResource) { r.Signal = sigTerm }), `has signal "SIGTERM", expected "terminate"`},
		// action / provider mismatches
		{"native resource with docker_stop", lb, forgedProc(func(r *planResource) { r.Action = actionDockerStop }), `unsupported action "docker_stop"`},
		{"native resource with empty action", lb, forgedProc(func(r *planResource) { r.Action = "" }), "unsupported action"},
		{"container with signal action", lb, forgedContainer(func(r *planResource) { r.Action = actionSignal }), `unsupported action "signal"`},
		{"container with pid set", lb, forgedContainer(func(r *planResource) { r.PID = 100 }), "inconsistent with its container id"},
		{"container with start time", lb, forgedContainer(func(r *planResource) { r.StartTime = "1" }), "inconsistent with its container id"},
		{"native resource with container id", lb, forgedProc(func(r *planResource) { r.ContainerID = hex64(1) }), "inconsistent with its pid/start_time"},
		{"native provider on a container resource", lb, forgedContainer(func(r *planResource) { r.Provider = providerNative }), "protected PID 0"},
		{"docker provider on a native resource", lb, forgedProc(func(r *planResource) { r.Provider = providerDocker }), "invalid container id"},
		{"unknown provider", lb, forgedProc(func(r *planResource) { r.Provider = "systemd" }), `unknown provider "systemd"`},
		{"resource id mismatch", lb, forgedProc(func(r *planResource) { r.ResourceID = "proc:101:" + r.StartTime }), "inconsistent with its pid/start_time"},
		{"no start time", lb, forgedProc(func(r *planResource) { r.StartTime = ""; r.ResourceID = procResourceID(r.PID, "") }), "has no process identity"},
		{"no identity strength", lb, forgedProc(func(r *planResource) { r.IdentityStrength = "" }), "has no process identity"},
		{"identity strength none", lb, forgedProc(func(r *planResource) { r.IdentityStrength = strengthNone }), "has no process identity"},
		{"container grace out of range", lb, forgedContainer(func(r *planResource) { r.GraceSeconds = 999999 }), "out-of-range grace"},
		{"container negative grace", lb, forgedContainer(func(r *planResource) { r.GraceSeconds = -1 }), "out-of-range grace"},
		// mode
		{"native resource in a container-mode plan", lb, func(d *planDoc) { d.Request.Mode = modeContainer }, "plan's mode is \"container\""},
		{"container in a native-mode plan", lb, func(d *planDoc) { d.Request.Mode = modeNative }, "plan's mode is \"native\""},
		{"unknown mode", lb, func(d *planDoc) { d.Request.Mode = "everything" }, "unsupported mode"},
		// request and document
		{"bad protocol", lb, func(d *planDoc) { d.Request.Protocol = "sctp" }, "unsupported protocol"},
		{"scope mismatch", lb, func(d *planDoc) { d.Request.Scope = scopeUDP }, "does not match protocol"},
		{"huge grace", lb, func(d *planDoc) { d.Request.GraceSeconds = 999999 }, "grace_seconds 999999 is out of range"},
		{"wrong version", lb, func(d *planDoc) { d.SchemaVersion = 2 }, "unsupported schema_version"},
		{"wrong kind", lb, func(d *planDoc) { d.Kind = "killport.apply_result" }, `kind is "killport.apply_result"`},
		{"endpoint references unknown resource", lb, func(d *planDoc) { d.Endpoints[0].Resources = append(d.Endpoints[0].Resources, "proc:999:1") }, "references unknown resource"},
		{"endpoint for an unrequested port", lb, func(d *planDoc) { d.Endpoints[0].Port = 4000 }, "was not requested"},
		// time window (review m3)
		{"expiry far in the future", lb, func(d *planDoc) { d.ExpiresAt = "2099-01-01T00:00:00Z" }, "expires_at must not"},
		{"expiry just over 24h", lb, func(d *planDoc) { d.ExpiresAt = "2026-10-22T12:00:01Z" }, "expires_at must not"},
		{"expiry before creation", lb, func(d *planDoc) { d.ExpiresAt = "2026-10-21T11:59:59Z" }, "expires_at must not"},
		{"created_at unparseable", lb, func(d *planDoc) { d.CreatedAt = "yesterday" }, "created_at is not an RFC 3339 time"},
		{"expires_at unparseable", lb, func(d *planDoc) { d.ExpiresAt = "" }, "expires_at is not an RFC 3339 time"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := lin
			if c.base == wb {
				w = win
			}
			data := forgePlan(t, *c.base, c.mutate)
			// 1. the intake rejects it, for the intended reason
			err := decodeAndValidate(data)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("intake error = %v, want it to contain %q", err, c.want)
			}
			// 2. and the command exits 2 with that reason and touches nothing
			code, out, _ := wfRun(t, w.f, w.f, data, "apply", "--json", "-")
			m := decodeErr(t, out)
			if code != 2 || m["code"] != "usage" || m["exit_class"] != "usage" || !strings.Contains(m["error"], "invalid plan") || !strings.Contains(m["error"], c.want) {
				t.Fatalf("code=%d out=%s", code, out)
			}
			assertNoMutation(t, w.f)
		})
	}
}

// The plan's own PID is deliberately NOT a validation error: it is a
// per-resource failure at apply time ("refusing to signal killport itself"),
// so the rest of the plan still runs. A forged plan naming killport must never
// get a signal delivered to it.
func TestApplyRefusesToSignalItselfEvenInForgedPlan(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, fakeOwnPID, "killport")
	w.publish()
	_, base := planFile(t, w.f, w.f, "3000", "3001")
	if len(base.Resources) != 1 || len(base.Refused) != 1 {
		t.Fatalf("the plan itself already refuses killport: %+v", base)
	}
	data := forgePlan(t, base, func(d *planDoc) {
		own := planResource{ResourceID: procResourceID(fakeOwnPID, wfStart(fakeOwnPID)), Provider: providerNative, PID: fakeOwnPID, Name: "killport",
			StartTime: wfStart(fakeOwnPID), IdentityStrength: strengthStartTime, Ports: []int{3001}, Action: actionSignal, Signal: sigTerm}
		d.Resources = append(d.Resources, own)
		d.Refused = []planRefusal{}
		for i := range d.Endpoints {
			d.Endpoints[i].Refused = []planRefusal{}
			if d.Endpoints[i].Port == 3001 {
				d.Endpoints[i].Resources = append(d.Endpoints[i].Resources, own.ResourceID)
			}
		}
	})
	if err := decodeAndValidate(data); err != nil {
		t.Fatalf("a plan naming the own PID is structurally valid: %v", err)
	}
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	code, out, _ := wfRun(t, w.f, w.f, data, "apply", "--json", "-")
	ad := decodeApplyDoc(t, out)
	own := resultOf(ad, procResourceID(fakeOwnPID, wfStart(fakeOwnPID)))
	if own.Result != resFailed || !strings.Contains(own.Error, "refusing to signal killport itself") {
		t.Errorf("own resource = %+v", own)
	}
	for _, s := range w.f.signals {
		if s.PID == fakeOwnPID {
			t.Fatalf("killport signalled itself: %+v", w.f.signals)
		}
	}
	if resultOf(ad, procResourceID(100, wfStart(100))).Result != resSignalSent {
		t.Errorf("the legitimate resource should still run: %+v", ad.Resources)
	}
	if code != 1 || ad.ActionsExecuted != 1 {
		t.Errorf("code=%d executed=%d", code, ad.ActionsExecuted)
	}
}

func TestApplyWrongOSIsUsageError(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	path, d := planFile(t, w.f, w.f, "3000")
	if d.OS != "linux" {
		t.Fatalf("os = %s", d.OS)
	}
	other := newWfWorld("darwin", false)
	other.publish()
	code, out, _ := wfRun(t, other.f, other.f, "", "apply", "--json", path)
	m := decodeErr(t, out)
	if code != 2 || !strings.Contains(m["error"], "linux") || !strings.Contains(m["error"], "darwin") || m["exit_class"] != "usage" {
		t.Errorf("code=%d out=%s", code, out)
	}
	assertNoMutation(t, other.f)
}

func TestApplyExpiredPlanNeedsFreshPlan(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	path, d := planFile(t, w.f, w.f, "3000", "--ttl", "1m")
	if d.ExpiresAt != "2026-10-21T12:01:00Z" {
		t.Fatalf("expires_at = %s", d.ExpiresAt)
	}
	// Exactly at expiry the plan is still valid; one second later it is not.
	w.f.setNow(fakeEpoch.Add(61 * time.Second))
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code != 3 || ad.ExitClass != "needs_fresh_plan" || ad.Error != "plan expired" {
		t.Fatalf("code=%d doc=%+v", code, ad)
	}
	if len(ad.Resources) != 1 || ad.Resources[0].Result != resNotAttempted || ad.ActionsExecuted != 0 {
		t.Errorf("resources = %+v", ad.Resources)
	}
	assertNoMutation(t, w.f)

	// Human output says so too.
	code, out, _ = wfRun(t, w.f, w.f, "", "apply", path)
	if code != 3 || !strings.Contains(out, "plan expired") || !strings.Contains(out, "needs_fresh_plan") {
		t.Errorf("human: code=%d out=%s", code, out)
	}

	// At exactly expires_at the plan can still be applied.
	w.f.setNow(fakeEpoch.Add(time.Minute))
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	if code, _, _ = wfRun(t, w.f, w.f, "", "apply", "--json", path); code != 0 {
		t.Errorf("a plan at its expiry instant should still apply, exit %d", code)
	}
}

func TestApplyFromStdinAndRoundTrip(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }

	// plan --json piped into apply -
	_, planOut, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	code, out, errOut := wfRun(t, w.f, w.f, planOut, "apply", "--json", "-")
	if code != 0 {
		t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
	}
	ad := decodeApplyDoc(t, out)
	if ad.Kind != "killport.apply_result" || ad.SchemaVersion != 1 || ad.ExitClass != "ok" || ad.ActionsExecuted != 1 {
		t.Fatalf("doc = %+v", ad)
	}
	r := ad.Resources[0]
	if r.Result != resSignalSent || !r.VerifiedGone || r.Forced || r.PID != 100 || ad.Endpoints[0].Status != epFree {
		t.Errorf("resource=%+v endpoints=%+v", r, ad.Endpoints)
	}
	if ad.PlanID != decodePlanDoc(t, planOut).PlanID || ad.AppliedAt != "2026-10-21T12:00:00Z" {
		t.Errorf("plan id / time: %+v", ad)
	}
	if len(w.f.signals) != 1 || w.f.signals[0].Force || w.f.signals[0].PID != 100 || w.f.signals[0].StartTime != wfStart(100) {
		t.Errorf("signals = %+v", w.f.signals)
	}

	// plan -o file, then apply file: the file equals the --json stdout.
	w2 := newWfWorld("linux", false)
	w2.listen("tcp", 3000, 100, "node")
	w2.publish()
	path := filepath.Join(t.TempDir(), "plan.json")
	_, stdout, _ := wfRun(t, w2.f, w2.f, "", "plan", "--json", "-o", path, "3000")
	fileData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(fileData, &a) != nil || json.Unmarshal([]byte(stdout), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("plan file differs from stdout:\n%s\n%s", fileData, stdout)
	}
	w2.f.onSignal = func(f *fakeSystem, s fakeSignal) { w2.removePID(s.PID); w2.publish() }
	if code, _, _ = wfRun(t, w2.f, w2.f, "", "apply", path); code != 0 {
		t.Errorf("apply of the written plan: exit %d", code)
	}
	if len(w2.f.signals) != 1 {
		t.Errorf("signals = %+v", w2.f.signals)
	}

	// -o without --json: human output, file still written.
	path2 := filepath.Join(t.TempDir(), "plan2.json")
	code, out, _ = wfRun(t, w2.f, w2.f, "", "plan", "-o", path2, "3001")
	if _, err := os.Stat(path2); err != nil || code != 0 || json.Valid([]byte(out)) || !strings.Contains(out, "Plan written to "+path2) {
		t.Errorf("human -o: code=%d err=%v out=%s", code, err, out)
	}

	// An unwritable -o path is a runtime failure with one JSON error.
	code, out, _ = wfRun(t, w2.f, w2.f, "", "plan", "--json", "-o", filepath.Join(t.TempDir(), "no", "such", "dir", "p.json"), "3000")
	m := decodeErr(t, out)
	if code != 1 || m["code"] != "write_failed" || m["exit_class"] != "failed" {
		t.Errorf("unwritable: code=%d out=%s", code, out)
	}
}

func TestApplyForcedFlag(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	path, _ := planFile(t, w.f, w.f, "--force", "3000")
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code != 0 || !ad.Resources[0].Forced || !w.f.signals[0].Force {
		t.Errorf("code=%d doc=%+v signals=%+v", code, ad, w.f.signals)
	}
}

func TestApplyWindowsTerminates(t *testing.T) {
	w := newWfWorld("windows", false)
	w.listen("tcp", 3000, 1234, "node.exe")
	w.publish()
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	path, d := planFile(t, w.f, w.f, "3000")
	if d.Resources[0].Signal != "terminate" {
		t.Fatalf("plan = %+v", d.Resources)
	}
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code != 0 || ad.Endpoints[0].Status != epFree || len(w.f.signals) != 1 || !w.f.signals[0].Force || !ad.Resources[0].Forced {
		t.Errorf("code=%d doc=%+v signals=%+v", code, ad, w.f.signals)
	}
}

func TestApplyDarwinUDP(t *testing.T) {
	w := newWfWorld("darwin", false)
	w.listen("udp", 5353, 100, "mDNS")
	w.connect("udp", 6000, 5353, 300, "chrome") // remote 5353: never selected
	w.publish()
	path, d := planFile(t, w.f, w.f, "--protocol", "udp", "5353")
	if len(d.Resources) != 1 || d.Resources[0].PID != 100 || d.Request.Protocol != "udp" || d.Request.Scope != "bound" {
		t.Fatalf("plan = %+v", d)
	}
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code != 0 || ad.Endpoints[0].Status != epFree || ad.Endpoints[0].Protocol != "udp" {
		t.Errorf("code=%d doc=%+v", code, ad)
	}
}

func TestDeterministicOutput(t *testing.T) {
	build := func() *wfWorld {
		w := newWfWorld("linux", true)
		w.listen("tcp", 3002, 300, "vite").listen("tcp", 3000, 100, "node").listen("tcp", 3001, 100, "node")
		w.container(hex64(0xbb), "b", 0, 3003)
		w.container(hex64(0xaa), "a", 910, 3004, 3005)
		w.listen("tcp", 3006, 0, "")
		return w.publish()
	}
	for _, args := range [][]string{
		{"plan", "--json", "3000-3007"},
		{"plan", "3000-3007"},
		{"inspect", "--json", "3000-3007"},
		{"inspect"},
	} {
		w1, w2 := build(), build()
		c1, o1, e1 := wfRun(t, w1.f, w1.f, "", args...)
		c2, o2, e2 := wfRun(t, w2.f, w2.f, "", args...)
		if o1 != o2 || e1 != e2 || c1 != c2 || o1 == "" {
			t.Errorf("%v is not deterministic:\n%s\n---\n%s", args, o1, o2)
		}
	}
	// Resources: containers by id, then processes by pid; ports ascending.
	w := build()
	_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3007", "3005", "3001", "3000-3002", "3003", "3004", "3006")
	d := decodePlanDoc(t, out)
	var ids []string
	for _, r := range d.Resources {
		ids = append(ids, r.ResourceID)
	}
	want := []string{"container:" + hex64(0xaa), "container:" + hex64(0xbb), "proc:100:" + wfStart(100), "proc:300:" + wfStart(300)}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("resource order = %v, want %v", ids, want)
	}
	if !reflect.DeepEqual(d.Request.Ports, []int{3000, 3001, 3002, 3003, 3004, 3005, 3006, 3007}) {
		t.Errorf("ports = %v", d.Request.Ports)
	}
}

func TestJSONIsExactlyOneDocumentOnEveryPath(t *testing.T) {
	good := func() *wfWorld {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		return w.publish()
	}
	w := good()
	path, d := planFile(t, w.f, w.f, "3000")
	planJSON, _ := os.ReadFile(path)
	type tc struct {
		name  string
		args  []string
		stdin string
		w     *wfWorld
		want  int
		errCl string // exit_class when the doc is an error object
	}
	missing := newWfWorld("linux", false)
	missing.f.tools = map[string]bool{}
	cases := []tc{
		{"inspect ok", []string{"inspect", "--json", "3000"}, "", good(), 0, ""},
		{"inspect all", []string{"inspect", "--json"}, "", good(), 0, ""},
		{"inspect bad port", []string{"inspect", "--json", "abc"}, "", good(), 2, "usage"},
		{"inspect bad protocol", []string{"inspect", "--protocol", "icmp", "--json"}, "", good(), 2, "usage"},
		{"inspect bad flag", []string{"inspect", "--json", "--bogus"}, "", good(), 2, "usage"},
		{"inspect missing tool", []string{"inspect", "--json", "3000"}, "", missing, 1, "failed"},
		{"plan ok", []string{"plan", "--json", "3000"}, "", good(), 0, ""},
		{"plan no ports", []string{"plan", "--json"}, "", good(), 2, "usage"},
		{"plan bad ttl", []string{"plan", "--json", "--ttl", "48h", "3000"}, "", good(), 2, "usage"},
		{"plan zero ttl", []string{"plan", "--json", "--ttl", "0s", "3000"}, "", good(), 2, "usage"},
		{"plan negative grace", []string{"plan", "--json", "--grace=-1s", "3000"}, "", good(), 2, "usage"},
		{"plan bad ttl syntax", []string{"plan", "--json", "--ttl", "soon", "3000"}, "", good(), 2, "usage"},
		{"plan missing tool", []string{"plan", "--json", "3000"}, "", missing, 1, "failed"},
		{"plan range", []string{"plan", "--json", "3000-3002"}, "", good(), 0, ""},
		{"apply no arg", []string{"apply", "--json"}, "", good(), 2, "usage"},
		{"apply two args", []string{"apply", "--json", "a", "b"}, "", good(), 2, "usage"},
		{"apply missing file", []string{"apply", "--json", filepath.Join(t.TempDir(), "nope.json")}, "", good(), 2, "usage"},
		{"apply stdin garbage", []string{"apply", "--json", "-"}, "{", good(), 2, "usage"},
		{"apply stdin empty", []string{"apply", "--json", "-"}, "", good(), 2, "usage"},
		{"apply ok", []string{"apply", "--json", path}, "", good(), 1, ""}, // process keeps running: blocked_by_original
		{"apply stdin", []string{"apply", "--json", "-"}, string(planJSON), good(), 1, ""},
		{"apply missing tool", []string{"apply", "--json", path}, "", missing, 1, "failed"},
		{"help", []string{"plan", "--json", "--help"}, "", good(), 0, ""},
	}
	_ = d
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, _ := wfRun(t, c.w.f, c.w.f, c.stdin, c.args...)
			var v map[string]any
			decodeDoc(t, out, &v)
			if code != c.want {
				t.Errorf("exit = %d, want %d\n%s", code, c.want, out)
			}
			if c.errCl != "" {
				if v["exit_class"] != c.errCl || v["error"] == nil || v["code"] == nil {
					t.Errorf("error document = %v", v)
				}
				return
			}
			if _, isErr := v["error"]; isErr && c.name != "help" {
				if _, hasKind := v["kind"]; !hasKind {
					t.Errorf("unexpected error document %v", v)
				}
			}
		})
	}
	// Warnings and diagnostics stay on stderr.
	bad := good()
	bad.f.setResp(cmdLsof, fail(2, "lsof: broken"))
	code, out, errOut := wfRun(t, bad.f, bad.f, "", "plan", "--json", "3000")
	decodeDoc(t, out, &map[string]any{})
	if code != 1 || !strings.Contains(errOut, "warning") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestWorkflowHelpAndCompat(t *testing.T) {
	// Subcommand help as JSON and as text.
	code, out, _ := wfRun(t, newFake("linux"), nil, "", "plan", "-j", "-h")
	m := decodeErr(t, out)
	if code != 0 || !strings.Contains(m["usage"], "--protocol") || !strings.Contains(m["usage"], "--force") {
		t.Errorf("json help: code=%d out=%s", code, out)
	}
	code, out, _ = wfRun(t, newFake("linux"), nil, "", "apply", "--help")
	if code != 0 || !strings.Contains(out, "Usage:") || json.Valid([]byte(out)) {
		t.Errorf("text help: code=%d out=%s", code, out)
	}
	// Legacy invocations still reach the root command next to subcommands.
	f := newFake("linux", "lsof").on(cmdLsof, ok(""))
	code, out, _ = wfRun(t, f, f, "", "--json", "3000")
	if code != 0 || decodeKill(t, out)[0].Status != statusFree {
		t.Errorf("legacy kill: code=%d out=%s", code, out)
	}
	code, out, _ = wfRun(t, f, f, "", "--json", "plan3000")
	if code != 2 || decodeErr(t, out)["code"] != "usage" {
		t.Errorf("unknown word: code=%d out=%s", code, out)
	}
	if _, has := decodeErr(t, out)["exit_class"]; has {
		t.Errorf("legacy errors must not gain an exit_class field: %s", out)
	}
}

func TestApplyDockerWrapperTimeoutContainerGoneIsStopped(t *testing.T) {
	id := hex64(0xd1)
	w := newWfWorld("linux", true)
	w.container(id, "web", 900, 3000)
	w.publish()
	path, _ := planFile(t, w.f, w.f, "3000")
	w.f.setResp(stopCmd(id), fakeResp{err: errWrapperTimeout})
	h := &hookFake{fakeSystem: w.f}
	h.afterRun = func(key string, err error) {
		if key == stopCmd(id) {
			// The stop did complete after the wrapper gave up.
			w.removeContainer(id)
			w.publish()
		}
	}
	code, out, _ := wfRun(t, h, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	r := ad.Resources[0]
	if code != 0 || r.Result != resStopped || !r.VerifiedGone || ad.Endpoints[0].Status != epFree {
		t.Errorf("code=%d doc=%+v", code, ad)
	}
}

func TestApplyDockerFailureModes(t *testing.T) {
	id := hex64(0xd2)
	setup := func() (*wfWorld, string) {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		return w, path
	}
	t.Run("permission denied", func(t *testing.T) {
		w, path := setup()
		w.f.setResp(stopCmd(id), fail(1, "permission denied while trying to connect to the Docker daemon socket"))
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		r := ad.Resources[0]
		if code != 1 || r.Result != resPermissionDenied || !strings.Contains(r.Error, "permission denied") || ad.ActionsExecuted != 1 {
			t.Errorf("code=%d doc=%+v", code, ad)
		}
	})
	t.Run("other error", func(t *testing.T) {
		w, path := setup()
		w.f.setResp(stopCmd(id), fail(1, "Error response from daemon: boom"))
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 1 || ad.Resources[0].Result != resFailed || !strings.Contains(ad.Resources[0].Error, "boom") {
			t.Errorf("code=%d doc=%+v", code, ad)
		}
	})
	t.Run("no such container", func(t *testing.T) {
		w, path := setup()
		w.f.setResp(stopCmd(id), fail(1, "Error response from daemon: No such container: "+id))
		_, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if ad.Resources[0].Result != resNotAttempted || ad.ActionsExecuted != 0 {
			t.Errorf("doc=%+v", ad)
		}
	})
	t.Run("docker unreachable at apply time", func(t *testing.T) {
		w, path := setup()
		w.dockerFail = &fakeResp{err: fail(1, "Cannot connect to the Docker daemon").err}
		w.publish()
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		r := ad.Resources[0]
		if r.Result != resNotAttempted || !strings.Contains(r.Error, "Cannot connect") || ad.Endpoints[0].Visibility != visProviderUnavailable || ad.Endpoints[0].Status != epNotVerifiable {
			t.Errorf("doc=%+v", ad)
		}
		if code != 4 {
			t.Errorf("exit = %d, want 4", code)
		}
		if w.f.runCount("docker stop") != 0 {
			t.Errorf("docker stop ran although the container could not be confirmed")
		}
	})
}

func TestApplyNativeErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		result string
		code   int
	}{
		{"permission", fmt.Errorf("signalling PID 100: %w", os.ErrPermission), resPermissionDenied, 1},
		{"other", errors.New("kill: weird"), resFailed, 1},
		{"gone during signal", errProcessNotFound, resNotAttempted, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWfWorld("linux", false)
			w.listen("tcp", 3000, 100, "node")
			w.publish()
			path, _ := planFile(t, w.f, w.f, "3000")
			w.f.killErrs[100] = tc.err
			code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
			ad := decodeApplyDoc(t, out)
			if ad.Resources[0].Result != tc.result && !(tc.err == errProcessNotFound) {
				t.Errorf("result = %s, want %s", ad.Resources[0].Result, tc.result)
			}
			if tc.code >= 0 && code != tc.code {
				t.Errorf("exit = %d, want %d", code, tc.code)
			}
			if tc.result == resFailed && !strings.Contains(ad.Resources[0].Error, "weird") {
				t.Errorf("real error text lost: %+v", ad.Resources[0])
			}
		})
	}
}

func TestApplyIsolatesPlannedResourcesOnly(t *testing.T) {
	// A second process appears on the planned port and an unrelated one
	// listens elsewhere: apply signals only what the plan names.
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node").listen("tcp", 4000, 500, "postgres")
	w.publish()
	path, _ := planFile(t, w.f, w.f, "3000")
	w.listen("tcp", 3000, 101, "intruder").publish()
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if len(w.f.signals) != 1 || w.f.signals[0].PID != 100 {
		t.Errorf("signals = %+v", w.f.signals)
	}
	if code != 3 || ad.Endpoints[0].Status != epBlockedNewOwner || len(ad.Endpoints[0].Holders) != 1 || ad.Endpoints[0].Holders[0].PID != 101 {
		t.Errorf("code=%d doc=%+v", code, ad)
	}
}

func TestApplyHumanOutput(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
	path, _ := planFile(t, w.f, w.f, "3000")
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", path)
	if code != 0 || json.Valid([]byte(out)) {
		t.Fatalf("code=%d out=%s", code, out)
	}
	for _, want := range []string{"proc:100:" + wfStart(100) + "  signal_sent  verified_gone=true forced=false", "tcp 3000: free (visibility: complete_for_requested_scope)", "Actions executed: 1. Result: ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPlanUnderReadOnlySystemNeverPanics(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			w := newWfWorld(goos, goos != "windows")
			w.listen("tcp", 3000, 1234, "node")
			if goos != "windows" {
				w.container(hex64(7), "web", 0, 3001)
			}
			w.publish()
			ro := readOnlySystem{system: w.f}
			for _, args := range [][]string{{"plan", "--json", "3000", "3001"}, {"inspect", "--json"}, {"plan", "--protocol", "udp", "3000"}} {
				wfRun(t, ro, w.f, "", args...)
			}
			assertNoMutation(t, w.f)
		})
	}
}

func TestVerboseLogsWorkflowCommandsToStderr(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	code, out, errOut := wfRun(t, w.f, w.f, "", "plan", "--verbose", "--json", "3000")
	decodePlanDoc(t, out)
	if code != 0 || !strings.Contains(errOut, "run: lsof") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestPlanUDPDockerPublishedPorts(t *testing.T) {
	id := hex64(0x53)
	w := newWfWorld("linux", true)
	w.container(id, "dns", 0, 8080)
	w.mu.Lock()
	// The same container additionally publishes UDP 5353.
	w.conts[0].ports = append(w.conts[0].ports, "0.0.0.0:5353->53/udp", "[::]:5353->53/udp")
	w.socks = append(w.socks, wfSock{proto: protoUDP, port: 5353, pid: 0, name: "docker-proxy", cid: id})
	w.mu.Unlock()
	w.publish()
	// The --no-trunc listing is shared by both protocols.
	for _, tc := range []struct {
		args []string
		port int
	}{{[]string{"plan", "--json", "--protocol", "udp", "5353"}, 5353}, {[]string{"plan", "--json", "8080"}, 8080}} {
		code, out, _ := wfRun(t, w.f, w.f, "", tc.args...)
		d := decodePlanDoc(t, out)
		if code != 0 || len(d.Resources) != 1 || d.Resources[0].ContainerID != id || !reflect.DeepEqual(d.Resources[0].Ports, []int{tc.port}) {
			t.Errorf("%v: code=%d plan=%+v", tc.args, code, d)
		}
	}
	// A tcp plan for the UDP-only port, and a udp plan for the TCP-only port, select nothing.
	_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "5353")
	if d := decodePlanDoc(t, out); len(d.Resources) != 0 || d.Endpoints[0].Occupied {
		t.Errorf("tcp plan of a udp-only port: %+v", d)
	}
	_, out, _ = wfRun(t, w.f, w.f, "", "plan", "--json", "--protocol", "udp", "8080")
	if d := decodePlanDoc(t, out); len(d.Resources) != 0 || d.Endpoints[0].Occupied {
		t.Errorf("udp plan of a tcp-only port: %+v", d)
	}
}

func TestWindowsTasklistFailureRefusesAndIsProviderUnavailable(t *testing.T) {
	w := newWfWorld("windows", false)
	w.listen("tcp", 3000, 1234, "node.exe")
	w.publish()
	w.f.setResp(cmdTasklist, fail(1, "tasklist: boom"))
	code, out, errOut := wfRun(t, w.f, w.f, "", "plan", "--json", "3000", "3001")
	d := decodePlanDoc(t, out)
	if code != 1 || len(d.Resources) != 0 || len(d.Refused) != 1 || !strings.Contains(d.Refused[0].Reason, "tasklist") {
		t.Fatalf("code=%d plan=%+v", code, d)
	}
	if d.Endpoints[0].Visibility != visProviderUnavailable {
		t.Errorf("visibility = %s", d.Endpoints[0].Visibility)
	}
	if !strings.Contains(errOut, "tasklist failed") {
		t.Errorf("stderr = %q", errOut)
	}
	assertNoMutation(t, w.f)
}

func TestDarwinNetstatFailureIsNoneVisibleWhenNothingAttributed(t *testing.T) {
	w := newWfWorld("darwin", false)
	w.listen("tcp", 3001, 100, "node")
	w.publish()
	w.f.setResp(cmdNetstatMacTCP, fail(1, "netstat: not found"))
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000", "3001")
	d := decodePlanDoc(t, out)
	if d.Endpoints[0].Visibility != visNoneVisible || d.Endpoints[0].Occupied {
		t.Errorf("3000 = %+v", d.Endpoints[0])
	}
	// An attributed owner keeps its endpoint usable even without the table.
	if d.Endpoints[1].Visibility != visComplete || len(d.Resources) != 1 {
		t.Errorf("3001 = %+v resources=%+v", d.Endpoints[1], d.Resources)
	}
	if code != 4 {
		t.Errorf("exit = %d, want 4", code)
	}
}

func TestApplyActsOnEveryResourceOnceWithBoundedConcurrency(t *testing.T) {
	w := newWfWorld("linux", false)
	const n = 12
	var ports []string
	for i := 0; i < n; i++ {
		w.listen("tcp", 3000+i, 100+i, "node")
		ports = append(ports, strconv.Itoa(3000+i))
	}
	w.publish()
	path, d := planFile(t, w.f, w.f, ports...)
	if len(d.Resources) != n {
		t.Fatalf("resources = %d", len(d.Resources))
	}
	var mu sync.Mutex
	running, peak := 0, 0
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		w.removePID(s.PID)
		w.publish()
	}
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code != 0 || ad.ActionsExecuted != n {
		t.Fatalf("code=%d executed=%d", code, ad.ActionsExecuted)
	}
	if peak < 1 || peak > maxConcurrentActions {
		t.Errorf("peak concurrency %d, want 1..%d", peak, maxConcurrentActions)
	}
	seen := map[int]int{}
	for _, s := range w.f.signals {
		seen[s.PID]++
	}
	if len(seen) != n {
		t.Errorf("signalled %d distinct pids", len(seen))
	}
	for pid, c := range seen {
		if c != 1 {
			t.Errorf("pid %d signalled %d times", pid, c)
		}
	}
	for i, r := range ad.Resources {
		if r.PID != 100+i {
			t.Errorf("results out of order at %d: %+v", i, r)
		}
	}
}
