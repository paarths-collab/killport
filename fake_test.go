package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatih/color"
)

func TestMain(m *testing.M) {
	if runHelperProcess() {
		return
	}
	color.NoColor = true
	os.Exit(m.Run())
}

// Canned command lines (name + space-joined args).
const (
	cmdLsof      = "lsof -nP +c 0 -iTCP -sTCP:LISTEN -FpcnT"
	cmdSS        = "ss -ltnp"
	cmdNetstat   = "netstat -ano"
	cmdTasklist  = "tasklist /FO CSV /NH"
	cmdDockerPS  = "docker ps --format {{.ID}}|{{.Names}}|{{.Ports}}"
	fakeOwnPID   = 4242
	dockerStopID = "docker stop -t 10 "
)

type fakeResp struct {
	out string
	err error
}

func ok(out string) fakeResp { return fakeResp{out: out} }

func fail(code int, stderr string) fakeResp {
	return fakeResp{err: &cmdError{Name: "fake", Code: code, Stderr: stderr, Err: fmt.Errorf("exit status %d", code)}}
}

// fakeSystem implements system. It records every command and Kill call and
// never touches the host. Responses are queued per command line: successive
// calls pop the queue, the last response repeats.
type fakeSystem struct {
	mu         sync.Mutex
	goos       string
	tools      map[string]bool
	responses  map[string][]fakeResp
	runs       []string
	kills      []int
	killErrs   map[int]error
	sleeps     int
	unexpected []string
	// deadlines records, per command line, how much time the context passed
	// to Run had left (only for contexts that have a deadline).
	deadlines map[string]time.Duration

	// Scripted process table (see setProc). Unregistered PIDs are treated as
	// live processes with a synthetic start time unless strictProcs is set.
	procs       map[int]*fakeProc
	strictProcs bool
	identErrs   map[int]error // ProcessIdentity error per PID (e.g. permission)
	// signals records every signal actually DELIVERED by SignalIfSame, i.e.
	// after the identity check passed. A refused (identity changed / gone)
	// call records nothing. kills additionally mirrors the PIDs of delivered
	// signals so legacy assertions on f.kills keep working; killErrs is
	// consulted by both Kill and SignalIfSame.
	signals []fakeSignal
	// onSignal, if set, runs after a signal was delivered (outside the lock)
	// and may mutate the world, e.g. exitProc or a supervisor rebinding.
	onSignal func(f *fakeSystem, s fakeSignal)

	// Files served by ReadFile (path -> content).
	files map[string][]byte
	// Fake clock returned by Now; advanced by Sleep and advance().
	now time.Time
}

type fakeProc struct {
	start    string
	name     string
	strength string
	gone     bool
}

type fakeSignal struct {
	PID       int
	StartTime string
	Force     bool
}

// fakeEpoch is the initial value of the fake clock.
var fakeEpoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func newFake(goos string, tools ...string) *fakeSystem {
	f := &fakeSystem{goos: goos, tools: map[string]bool{}, responses: map[string][]fakeResp{}, killErrs: map[int]error{}, deadlines: map[string]time.Duration{},
		procs: map[int]*fakeProc{}, identErrs: map[int]error{}, files: map[string][]byte{}, now: fakeEpoch}
	for _, t := range tools {
		f.tools[t] = true
	}
	return f
}

func (f *fakeSystem) on(cmd string, resps ...fakeResp) *fakeSystem {
	f.responses[cmd] = resps
	return f
}

func (f *fakeSystem) GOOS() string { return f.goos }
func (f *fakeSystem) Getpid() int  { return fakeOwnPID }

func (f *fakeSystem) LookPath(name string) (string, error) {
	if f.tools[name] {
		return "/fake/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeSystem) Sleep(d time.Duration) {
	f.mu.Lock()
	f.sleeps++
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeSystem) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// setNow sets the fake clock; advance moves it forward.
func (f *fakeSystem) setNow(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

func (f *fakeSystem) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeSystem) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	}
	return append([]byte(nil), b...), nil
}

// setProc registers (or re-registers) a live process with the given opaque
// start time and name. Strength defaults to "start_time".
func (f *fakeSystem) setProc(pid int, startTime, name string) *fakeSystem {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = &fakeProc{start: startTime, name: name, strength: strengthStartTime}
	return f
}

// setProcStrength overrides the identity strength reported for pid, e.g.
// strengthNone to simulate a platform without identity support.
func (f *fakeSystem) setProcStrength(pid int, strength string) *fakeSystem {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.procs[pid]; p != nil {
		p.strength = strength
	} else {
		f.procs[pid] = &fakeProc{start: "fake-" + strconv.Itoa(pid), strength: strength}
	}
	return f
}

// exitProc marks pid as exited: ProcessIdentity and SignalIfSame report
// errProcessNotFound afterwards.
func (f *fakeSystem) exitProc(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.procs[pid]; p != nil {
		p.gone = true
		return
	}
	f.procs[pid] = &fakeProc{gone: true, strength: strengthStartTime}
}

// replaceProc simulates PID reuse: the same PID now belongs to a new live
// process with a different start time.
func (f *fakeSystem) replaceProc(pid int, newStart string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := ""
	if p := f.procs[pid]; p != nil {
		name = p.name
	}
	f.procs[pid] = &fakeProc{start: newStart, name: name, strength: strengthStartTime}
}

// lookupProc returns the live entry for pid, auto-registering unknown PIDs
// unless strictProcs. Caller holds f.mu.
func (f *fakeSystem) lookupProc(pid int) *fakeProc {
	p := f.procs[pid]
	if p == nil && !f.strictProcs {
		p = &fakeProc{start: "fake-" + strconv.Itoa(pid), strength: strengthStartTime}
		f.procs[pid] = p
	}
	if p == nil || p.gone {
		return nil
	}
	return p
}

func (f *fakeSystem) ProcessIdentity(pid int) (procIdentity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.identErrs[pid]; err != nil {
		return procIdentity{}, err
	}
	p := f.lookupProc(pid)
	if p == nil {
		return procIdentity{}, errProcessNotFound
	}
	if p.strength == strengthNone {
		return procIdentity{PID: pid, Strength: strengthNone}, nil
	}
	return procIdentity{PID: pid, StartTime: p.start, Strength: p.strength}, nil
}

func (f *fakeSystem) SignalIfSame(pid int, startTime string, force bool) error {
	f.mu.Lock()
	p := f.lookupProc(pid)
	if p == nil {
		f.mu.Unlock()
		return errProcessNotFound
	}
	if p.start != startTime {
		f.mu.Unlock()
		return errIdentityChanged
	}
	s := fakeSignal{PID: pid, StartTime: startTime, Force: force}
	f.signals = append(f.signals, s)
	f.kills = append(f.kills, pid)
	err := f.killErrs[pid]
	hook := f.onSignal
	f.mu.Unlock()
	if hook != nil {
		hook(f, s)
	}
	return err
}

func (f *fakeSystem) Kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, pid)
	return f.killErrs[pid]
}

func (f *fakeSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, key)
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines[key] = time.Until(dl)
	}
	q, found := f.responses[key]
	if !found || len(q) == 0 {
		f.unexpected = append(f.unexpected, key)
		return nil, fmt.Errorf("fake: unexpected command %q", key)
	}
	r := q[0]
	if len(q) > 1 {
		f.responses[key] = q[1:]
	}
	return []byte(r.out), r.err
}

func (f *fakeSystem) count(prefix string) int {
	n := 0
	for _, r := range f.runs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeSystem) sortedKills() []int {
	k := append([]int(nil), f.kills...)
	sort.Ints(k)
	return k
}

// invoke runs killport against the fake system.
func invoke(t *testing.T, f *fakeSystem, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb, f)
	if len(f.unexpected) > 0 {
		t.Errorf("unexpected commands: %v", f.unexpected)
	}
	return code, out.String(), errb.String()
}

func decodeKill(t *testing.T, out string) []KillResult {
	t.Helper()
	var rows []KillResult
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	return rows
}

func decodeCheck(t *testing.T, out string) []CheckResult {
	t.Helper()
	var rows []CheckResult
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	return rows
}

func decodeErr(t *testing.T, out string) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out)
	}
	return m
}

// lsofEntry renders one lsof -F process block with a single listening socket.
func lsofEntry(pid int, cmd string, names ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "p%d\nc%s\n", pid, cmd)
	for i, n := range names {
		fmt.Fprintf(&b, "f%d\nn%s\nTST=LISTEN\n", 20+i, n)
	}
	return b.String()
}

// lsofConn renders a client connection block (must never be a listener).
func lsofConn(pid int, cmd, name string) string {
	return fmt.Sprintf("p%d\nc%s\nf30\nn%s\nTST=ESTABLISHED\n", pid, cmd, name)
}

func netstatListen(port string, pid int) string {
	return fmt.Sprintf("  TCP    0.0.0.0:%s           0.0.0.0:0              LISTENING       %d\n", port, pid)
}

const netstatHeader = "\nActive Connections\n\n  Proto  Local Address          Foreign Address        State           PID\n"

const ssHeader = "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"
