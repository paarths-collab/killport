package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
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
}

func newFake(goos string, tools ...string) *fakeSystem {
	f := &fakeSystem{goos: goos, tools: map[string]bool{}, responses: map[string][]fakeResp{}, killErrs: map[int]error{}, deadlines: map[string]time.Duration{}}
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

func (f *fakeSystem) Sleep(time.Duration) {
	f.mu.Lock()
	f.sleeps++
	f.mu.Unlock()
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
