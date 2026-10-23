package main

// Table-driven fixture corpus.
//
// Each directory under testdata/corpus/ is one case: a case.json plus any
// small files it needs. The runner builds the fake system from the case,
// calls runIO with the case's arguments and asserts every key of "expect".
// The case table and the REAL / SYNTHETIC status of every input is documented
// in testdata/corpus/README.md.
//
// case.json keys (all of them strict: unknown keys fail the case):
//
//	name, description, contract, platform, args   required
//	commands   map exact command line -> response, or an array of responses
//	           served in order (the last one repeats, like the fake's queue).
//	           A response is {stdout | stdout_file, exit, stderr}. exit -1 is a
//	           wrapper timeout (context deadline). exit 0 ignores stderr.
//	files      absolute path read by ReadFile (e.g. /proc/net/tcp) -> file
//	procs      live processes: {pid, start, name, strength?}. strength "none"
//	           simulates a platform without process identity.
//	tools      optional. Explicit list of installed tools (LookPath). Default:
//	           the platform's required tools plus the program of every command.
//	apply_after_plan  optional. Runs plan_args first in the same world, writes
//	           the stdout to a temp file, applies replace_procs / exit_pids
//	           (PID reuse, exit) and then runs args with "@plan" replaced by the
//	           temp file path.
//	expect     exit, json_paths, signals, docker_stops, no_mutation.
//	           json_paths values compare with reflect.DeepEqual on the decoded
//	           document. Segments are map keys or array indexes; a final "#"
//	           yields the length of an array.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const corpusRoot = "testdata/corpus"

var (
	corpusContracts = map[string]bool{"tcp-listener": true, "udp-owner": true, "docker": true, "identity": true, "visibility": true, "errors": true}
	corpusPlatforms = map[string]bool{"linux": true, "darwin": true, "windows": true}
	corpusDirRe     = regexp.MustCompile(`^\d{2}-[a-z0-9-]+$`)
)

type corpusCase struct {
	Name           string                     `json:"name"`
	Description    string                     `json:"description"`
	Contract       string                     `json:"contract"`
	Platform       string                     `json:"platform"`
	Args           []string                   `json:"args"`
	Tools          []string                   `json:"tools"`
	Commands       map[string]json.RawMessage `json:"commands"`
	Files          map[string]string          `json:"files"`
	Procs          []corpusProc               `json:"procs"`
	ApplyAfterPlan *corpusReplan              `json:"apply_after_plan"`
	Expect         corpusExpect               `json:"expect"`
}

type corpusProc struct {
	PID      int    `json:"pid"`
	Start    string `json:"start"`
	Name     string `json:"name"`
	Strength string `json:"strength"`
}

type corpusReplan struct {
	PlanArgs     []string     `json:"plan_args"`
	ReplaceProcs []corpusProc `json:"replace_procs"`
	ExitPIDs     []int        `json:"exit_pids"`
}

type corpusResp struct {
	Stdout     string `json:"stdout"`
	StdoutFile string `json:"stdout_file"`
	Exit       int    `json:"exit"`
	Stderr     string `json:"stderr"`
}

type corpusExpect struct {
	Exit        *int           `json:"exit"`
	JSONPaths   map[string]any `json:"json_paths"`
	Signals     *[]int         `json:"signals"`
	DockerStops *[]string      `json:"docker_stops"`
	NoMutation  bool           `json:"no_mutation"`
}

type corpusResult struct {
	code   int
	stdout string
	stderr string
	f      *fakeSystem
}

// corpusCaseDirs returns the case directory names, sorted.
func corpusCaseDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(corpusRoot)
	if err != nil {
		t.Fatalf("read %s: %v", corpusRoot, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// loadCorpusCase decodes case.json strictly and validates its shape.
func loadCorpusCase(t *testing.T, dir string) *corpusCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "case.json"))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var c corpusCase
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("%s/case.json: %v", dir, err)
	}
	if c.Name != filepath.Base(dir) {
		t.Errorf("%s: name %q must equal the directory name", dir, c.Name)
	}
	if !corpusContracts[c.Contract] {
		t.Errorf("%s: unknown contract %q", dir, c.Contract)
	}
	if !corpusPlatforms[c.Platform] {
		t.Errorf("%s: unknown platform %q", dir, c.Platform)
	}
	if len(c.Args) == 0 || c.Description == "" {
		t.Errorf("%s: args and description are required", dir)
	}
	e := c.Expect
	if e.Exit == nil && len(e.JSONPaths) == 0 && e.Signals == nil && e.DockerStops == nil && !e.NoMutation {
		t.Errorf("%s: expect block asserts nothing", dir)
	}
	return &c
}

// corpusDefaultTools is the platform's required tools plus the program of every
// canned command, so a command the case provides always "exists".
func corpusDefaultTools(c *corpusCase) []string {
	set := map[string]bool{}
	switch c.Platform {
	case "windows":
		set["netstat"], set["tasklist"] = true, true
	default:
		set["lsof"] = true
	}
	for cmd := range c.Commands {
		set[strings.Fields(cmd)[0]] = true
	}
	var tools []string
	for k := range set {
		tools = append(tools, k)
	}
	sort.Strings(tools)
	return tools
}

func corpusResponse(t *testing.T, dir, cmd string, r corpusResp) fakeResp {
	t.Helper()
	out := r.Stdout
	if r.StdoutFile != "" {
		if r.Stdout != "" {
			t.Fatalf("%s: command %q sets both stdout and stdout_file", dir, cmd)
		}
		b, err := os.ReadFile(filepath.Join(dir, r.StdoutFile))
		if err != nil {
			t.Fatalf("%s: command %q: %v", dir, cmd, err)
		}
		out = string(b)
	}
	name := strings.Fields(cmd)[0]
	switch {
	case r.Exit == 0:
		return fakeResp{out: out}
	case r.Exit == -1:
		return fakeResp{out: out, err: &cmdError{Name: name, Code: -1, Stderr: r.Stderr, Err: fmt.Errorf("timed out: %w", context.DeadlineExceeded)}}
	}
	return fakeResp{out: out, err: &cmdError{Name: name, Code: r.Exit, Stderr: r.Stderr, Err: fmt.Errorf("exit status %d", r.Exit)}}
}

func buildCorpusFake(t *testing.T, dir string, c *corpusCase) *fakeSystem {
	t.Helper()
	tools := c.Tools
	if tools == nil {
		tools = corpusDefaultTools(c)
	}
	f := newFake(c.Platform, tools...)
	for cmd, raw := range c.Commands {
		var resps []fakeResp
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "[") {
			var list []corpusResp
			if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
				t.Fatalf("%s: command %q: want a non-empty response array: %v", dir, cmd, err)
			}
			for _, r := range list {
				resps = append(resps, corpusResponse(t, dir, cmd, r))
			}
		} else {
			var r corpusResp
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("%s: command %q: %v", dir, cmd, err)
			}
			resps = append(resps, corpusResponse(t, dir, cmd, r))
		}
		f.on(cmd, resps...)
	}
	for path, rel := range c.Files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("%s: file %s: %v", dir, path, err)
		}
		f.setFile(path, string(b))
	}
	for _, p := range c.Procs {
		f.setProc(p.PID, p.Start, p.Name)
		if p.Strength != "" {
			f.setProcStrength(p.PID, p.Strength)
		}
	}
	return f
}

// hasDryRunFlag reports whether args request --dry-run (long or in a short bundle).
func hasDryRunFlag(args []string) bool {
	for _, a := range args {
		if a == "--dry-run" || strings.HasPrefix(a, "--dry-run=") {
			return true
		}
		if isShortBundle(a) && strings.ContainsRune(strings.SplitN(a[1:], "=", 2)[0], 'd') {
			return true
		}
	}
	return false
}

// isShortBundle reports whether a is a bundle of short flags such as -cdj.
func isShortBundle(a string) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-'
}

// runCorpusCase executes one case and returns what the program printed and
// the fake that recorded it.
func runCorpusCase(t *testing.T, dir string, c *corpusCase) (res corpusResult) {
	t.Helper()
	f := buildCorpusFake(t, dir, c)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic while running the case: %v", r)
		}
	}()

	// Only real dry runs are wrapped: readOnlySystem panics on any mutation,
	// and apply runs its actions on goroutines where that panic could not be
	// recovered. Mutation in other cases is checked with assertNoMutation.
	readOnly := hasDryRunFlag(c.Args)
	args := c.Args
	if c.ApplyAfterPlan != nil {
		// Plan first, in the same world, read-only.
		_, planOut, planErr := wfRun(t, readOnlySystem{system: f}, f, "", c.ApplyAfterPlan.PlanArgs...)
		if strings.TrimSpace(planOut) == "" {
			t.Fatalf("%s: plan step printed nothing; stderr=%s", dir, planErr)
		}
		planPath := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(planPath, []byte(planOut), 0o600); err != nil {
			t.Fatalf("write plan: %v", err)
		}
		// The world changes between plan and apply.
		for _, p := range c.ApplyAfterPlan.ReplaceProcs {
			f.replaceProc(p.PID, p.Start)
		}
		for _, pid := range c.ApplyAfterPlan.ExitPIDs {
			f.exitProc(pid)
		}
		args = make([]string, len(c.Args))
		for i, a := range c.Args {
			args[i] = strings.Replace(a, "@plan", planPath, 1)
		}
	}

	var sys system = f
	if readOnly {
		sys = readOnlySystem{system: f}
	}
	res.code, res.stdout, res.stderr = wfRun(t, sys, f, "", args...)
	res.f = f
	return res
}

// corpusJSONPath walks doc by dot-separated segments. A numeric segment indexes
// an array, a final "#" returns the array length.
func corpusJSONPath(doc any, path string) (any, bool) {
	segs := strings.Split(path, ".")
	cur := doc
	for i, seg := range segs {
		switch v := cur.(type) {
		case map[string]any:
			x, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = x
		case []any:
			if seg == "#" {
				if i != len(segs)-1 {
					return nil, false
				}
				return float64(len(v)), true
			}
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(v) {
				return nil, false
			}
			cur = v[n]
		default:
			return nil, false
		}
	}
	return cur, true
}

func assertCorpusExpect(t *testing.T, dir string, c *corpusCase, r corpusResult) {
	t.Helper()
	e := c.Expect
	if e.Exit != nil && r.code != *e.Exit {
		t.Errorf("exit = %d, want %d\nstdout=%s\nstderr=%s", r.code, *e.Exit, r.stdout, r.stderr)
	}
	if len(e.JSONPaths) > 0 {
		var doc any
		decodeDoc(t, r.stdout, &doc)
		paths := make([]string, 0, len(e.JSONPaths))
		for p := range e.JSONPaths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			want := e.JSONPaths[p]
			got, ok := corpusJSONPath(doc, p)
			if !ok {
				t.Errorf("json path %q does not exist", p)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("json path %q = %v, want %v", p, got, want)
			}
		}
	}
	r.f.mu.Lock()
	kills := append([]int{}, r.f.kills...)
	var stops []string
	for _, run := range r.f.runs {
		if strings.HasPrefix(run, "docker stop ") {
			fs := strings.Fields(run)
			stops = append(stops, fs[len(fs)-1])
		}
	}
	r.f.mu.Unlock()
	if e.Signals != nil {
		want := append([]int{}, *e.Signals...)
		sort.Ints(want)
		sort.Ints(kills)
		if !reflect.DeepEqual(append([]int{}, kills...), want) {
			t.Errorf("signalled PIDs = %v, want %v", kills, want)
		}
	}
	if e.DockerStops != nil {
		want := sortedStrings(*e.DockerStops)
		stops = sortedStrings(stops)
		if !reflect.DeepEqual(append([]string{}, stops...), want) {
			t.Errorf("docker stops = %v, want %v", stops, want)
		}
	}
	if e.NoMutation {
		assertNoMutation(t, r.f)
	}
}

// TestCorpusFixtures runs every case under testdata/corpus.
func TestCorpusFixtures(t *testing.T) {
	dirs := corpusCaseDirs(t)
	if len(dirs) < 26 {
		t.Fatalf("corpus has %d cases, want at least 26", len(dirs))
	}
	for _, d := range dirs {
		if !corpusDirRe.MatchString(d) {
			t.Errorf("case directory %q must match NN-slug", d)
		}
	}
	for _, d := range dirs {
		dir := filepath.Join(corpusRoot, d)
		c := loadCorpusCase(t, dir)
		t.Run(d, func(t *testing.T) {
			r := runCorpusCase(t, dir, c)
			assertCorpusExpect(t, dir, c, r)
		})
	}
}

// ---- legacy predicate comparison --------------------------------------------
//
// These three functions compare DECLARED contracts on one invented Windows
// netstat listing. They are not product benchmarks and they do not measure
// anything about real machines:
//
//   - auditedGoPredicate is the OLD buggy selection: HasPrefix("TCP") on the
//     untrimmed line, the LISTEN check applied only when that prefix matched,
//     and PID "0" skipped. Real netstat rows are indented, so the prefix never
//     matches and the LISTEN check never runs.
//   - npmWindowsPredicate is the contract of the npm-style selection: TCP or
//     UDP, local port 3000, a positive decimal PID, and no state test.
//   - parseNetstat is the current parser, the one listWindows uses.
//
// The brief lists seven row shapes. The eighth (an IPv6 wildcard listener) is
// added so the table has eight rows.

// legacyCompareRows are invented rows, indented the way netstat prints them.
var legacyCompareRows = []string{
	"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       4120", // listener, IPv4
	"  TCP    [::]:3000              [::]:0                 LISTENING       4121", // listener, IPv6 wildcard
	"  TCP    127.0.0.1:3000         127.0.0.1:51000        ESTABLISHED     4130", // indented ESTABLISHED, local 3000
	"  TCP    127.0.0.1:51000        127.0.0.1:3000         ESTABLISHED     7788", // remote-only client
	"  TCP    0.0.0.0:13000          0.0.0.0:0              LISTENING       6300", // neighbour port listener
	"  UDP    0.0.0.0:3000           *:*                                    5100", // UDP bound to 3000
	"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       abc",  // malformed PID
	"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       0",    // PID 0
}

func legacyCompareNetstat() string {
	return "\nActive Connections\n\n  Proto  Local Address          Foreign Address        State           PID\n" +
		strings.Join(legacyCompareRows, "\n") + "\n"
}

// auditedGoPredicate reproduces the old selection on untrimmed lines.
func auditedGoPredicate(out string) []string {
	var pids []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "TCP") && !strings.Contains(line, "LISTEN") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[1], ":3000") {
			continue
		}
		if pid := f[len(f)-1]; pid != "0" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// npmWindowsPredicate selects TCP or UDP rows whose LOCAL port is 3000 and whose
// PID is a positive decimal number. It has no state test.
func npmWindowsPredicate(out string) []string {
	var pids []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if p := strings.ToUpper(f[0]); p != "TCP" && p != "UDP" {
			continue
		}
		local := f[1]
		i := strings.LastIndex(local, ":")
		if i < 0 || local[i+1:] != "3000" {
			continue
		}
		pid := f[len(f)-1]
		if n, err := strconv.Atoi(pid); err != nil || n <= 0 || strings.Trim(pid, "0123456789") != "" {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// currentParserPIDs runs the real parser and keeps the rows of port 3000, as
// listWindows and groupByPort do for a requested port.
func currentParserPIDs(out string) (onPort3000, all []string) {
	for _, l := range parseNetstat(out) {
		all = append(all, strconv.Itoa(l.PID))
		if l.Port == 3000 {
			onPort3000 = append(onPort3000, strconv.Itoa(l.PID))
		}
	}
	return onPort3000, all
}

func sortedStrings(xs []string) []string {
	out := append([]string{}, xs...)
	sort.Strings(out)
	return out
}

// TestCorpusLegacyPredicateComparison asserts the selected PID set of each
// predicate on the same eight invented rows.
func TestCorpusLegacyPredicateComparison(t *testing.T) {
	out := legacyCompareNetstat()
	if n := len(legacyCompareRows); n != 8 {
		t.Fatalf("row table has %d rows, want 8", n)
	}

	audited := sortedStrings(auditedGoPredicate(out))
	if want := sortedStrings([]string{"4120", "4121", "4130", "5100", "abc"}); !reflect.DeepEqual(audited, want) {
		t.Errorf("audited predicate selects %v, want %v (indented ESTABLISHED 4130 and UDP 5100 are the bug)", audited, want)
	}

	npm := sortedStrings(npmWindowsPredicate(out))
	if want := sortedStrings([]string{"4120", "4121", "4130", "5100"}); !reflect.DeepEqual(npm, want) {
		t.Errorf("npm predicate selects %v, want %v", npm, want)
	}

	current, all := currentParserPIDs(out)
	if want := sortedStrings([]string{"4120", "4121"}); !reflect.DeepEqual(sortedStrings(current), want) {
		t.Errorf("current parser selects on port 3000 %v, want only listeners %v", current, want)
	}
	if want := sortedStrings([]string{"4120", "4121", "6300"}); !reflect.DeepEqual(sortedStrings(all), want) {
		t.Errorf("current parser listeners (all ports) = %v, want %v", all, want)
	}

	// The remote-only client 7788 is never selected by any predicate.
	for name, set := range map[string][]string{"audited": audited, "npm": npm, "current": current} {
		for _, pid := range set {
			if pid == "7788" {
				t.Errorf("%s selected the remote-only client", name)
			}
		}
	}
}
