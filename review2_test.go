package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression tests for the independent review of the structured workflow
// (findings M1-M6, m1-m8, the nits and the mutation-survivor gaps).

// ---- helpers -------------------------------------------------------------------

// closePort drops pid's socket on one port but keeps the process alive.
func (w *wfWorld) closePort(pid, port int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	socks := w.socks[:0:0]
	for _, s := range w.socks {
		if !(s.pid == pid && s.port == port) {
			socks = append(socks, s)
		}
	}
	w.socks = socks
}

// dropProxy removes the port-proxy sockets of a container but keeps the
// container (docker run with the userland proxy disabled: the port is only
// visible through `docker ps`).
func (w *wfWorld) dropProxy(cid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	socks := w.socks[:0:0]
	for _, s := range w.socks {
		if s.cid != cid {
			socks = append(socks, s)
		}
	}
	w.socks = socks
}

// wfRunCtx is wfRun under a context.
func wfRunCtx(t *testing.T, ctx context.Context, sys system, f *fakeSystem, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runContext(ctx, args, strings.NewReader(stdin), &out, &errb, sys)
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

// ctxStrict makes Run behave like exec.CommandContext: a command started with
// a context that is already done fails. (The plain fake ignores contexts, so
// without this a bug that observes the outcome under a cancelled context would
// go unnoticed.)
type ctxStrict struct{ system }

func (c ctxStrict) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &cmdError{Name: name, Code: -1, Err: fmt.Errorf("timed out: %w", err)}
	}
	return c.system.Run(ctx, name, args...)
}

// removeOnSignal makes a delivered signal end the process, as it would for real.
func removeOnSignal(w *wfWorld) {
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
}

// ---- M1: apply consults the pre-action snapshot --------------------------------------

func TestApplyDoesNotSignalAProcessThatNoLongerHoldsThePlannedPorts(t *testing.T) {
	t.Run("closed the listener, another process owns the port", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, d := planFile(t, w.f, w.f, "3000")
		if len(d.Resources) != 1 || d.Resources[0].PID != 100 {
			t.Fatalf("plan = %+v", d)
		}
		// pid 100 stays alive with the same start time, but no longer
		// listens; pid 200 took the port.
		w.closePort(100, 3000)
		w.listen("tcp", 3000, 200, "other")
		w.publish()

		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if n := w.f.signalCount(); n != 0 {
			t.Fatalf("%d signal(s) were sent although pid 100 no longer holds the port: %v", n, w.f.signals)
		}
		assertNoMutation(t, w.f)
		r := ad.Resources[0]
		if r.Result != resNotAttempted || r.Note != "process no longer holds the planned ports" || ad.ActionsExecuted != 0 {
			t.Errorf("resource = %+v executed=%d", r, ad.ActionsExecuted)
		}
		if got := endpointStatus(ad, 3000); got != epBlockedNewOwner || code != 3 {
			t.Errorf("endpoint = %s, exit = %d; pid 200 must be reported as the new owner", got, code)
		}
		if len(ad.Endpoints[0].Holders) != 1 || ad.Endpoints[0].Holders[0].PID != 200 {
			t.Errorf("holders = %+v", ad.Endpoints[0].Holders)
		}
	})

	t.Run("still holding one of several planned ports is still signalled", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000", "3001")
		w.closePort(100, 3000)
		w.publish()
		removeOnSignal(w)
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if w.f.signalCount() != 1 || ad.Resources[0].Result != resSignalSent || code != 0 {
			t.Errorf("code=%d signals=%v doc=%+v", code, w.f.signals, ad)
		}
	})

	t.Run("incomplete visibility keeps acting as before", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		// pid 100 vanishes from lsof, but port 3000 is still occupied by a
		// socket nobody can attribute: its owner cannot be seen.
		w.closePort(100, 3000)
		w.listen("tcp", 3000, 0, "")
		w.publish()
		wfRun(t, w.f, w.f, "", "apply", "--json", path)
		if w.f.signalCount() != 1 {
			t.Errorf("with an unseen owner the planned process must still be signalled: %v", w.f.signals)
		}
	})

	t.Run("a reused PID is still reported as identity_changed", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.removePID(100)
		w.f.replaceProc(100, "a-new-start-time")
		w.publish()
		_, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		if r := decodeApplyDoc(t, out).Resources[0]; r.Result != resIdentityChanged {
			t.Errorf("resource = %+v", r)
		}
	})
}

// ---- M2: invalid UTF-8 from providers ---------------------------------------------------

func TestPlanApplyRoundTripWithInvalidUTF8ProviderStrings(t *testing.T) {
	cid := hex64(0xc0f)
	w := newWfWorld("linux", true)
	w.listen("tcp", 3000, 100, "caf\xe9d") // a Windows OEM code page byte
	w.listen("tcp", 3002, 101, "\xff\xfe")
	w.container(cid, "na\xffme", 0, 3001)
	w.publish()
	w.f.setResp(stopCmd(cid), ok(""))
	h := &hookFake{fakeSystem: w.f}
	h.afterRun = func(key string, err error) {
		if key == stopCmd(cid) {
			w.removeContainer(cid)
			w.publish()
		}
	}
	removeOnSignal(w)

	path, d := planFile(t, h, w.f, "3000-3002")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(data) {
		t.Fatalf("the plan file is not valid UTF-8")
	}
	if bytes.Contains(data, []byte(`\ufffd`)) {
		t.Errorf("the plan file holds an escaped replacement character (the checksum would not survive): %s", data)
	}
	if len(d.Resources) != 3 {
		t.Fatalf("plan = %+v", d)
	}
	for _, r := range d.Resources {
		if !utf8.ValidString(r.Name) {
			t.Errorf("name %q is not valid UTF-8", r.Name)
		}
	}

	code, out, errOut := wfRun(t, h, w.f, "", "apply", "--json", path)
	if code == 2 {
		t.Fatalf("an untouched plan was rejected: %s", out)
	}
	ad := decodeApplyDoc(t, out)
	if ad.Error != "" || code != 0 || ad.ActionsExecuted != 3 {
		t.Errorf("code=%d stderr=%s doc=%+v", code, errOut, ad)
	}
	for _, e := range ad.Endpoints {
		for _, hd := range e.Holders {
			if !utf8.ValidString(hd.Name) {
				t.Errorf("holder name %q is not valid UTF-8", hd.Name)
			}
		}
	}

	// inspect, too: every string in the document is valid.
	w2 := newWfWorld("linux", false)
	w2.listen("tcp", 3000, 100, "caf\xe9d").publish()
	_, out, _ = wfRun(t, w2.f, w2.f, "", "inspect", "--json", "3000")
	if !utf8.ValidString(out) || strings.Contains(out, `\ufffd`) {
		t.Errorf("inspect output: %q", out)
	}
}

func TestSanitizersMakeValidUTF8(t *testing.T) {
	if got := sanitizeProviderText("caf\xe9d"); got != "caf\uFFFDd" {
		t.Errorf("sanitizeProviderText = %q", got)
	}
	if got := validUTF8("a\xffb"); got != "a\uFFFDb" {
		t.Errorf("validUTF8 = %q", got)
	}
}

// ---- M3: subprocess text never reaches a document or the terminal verbatim --------------

var leakyDockerStderr = "Cannot connect to the Docker daemon at tcp://devbox-alice.local:2375 (DOCKER_HOST=tcp://10.1.2.3:2375): " +
	"open /home/alice/.docker/config.json: permission denied; contact alice@laptop-7 via dial tcp 10.1.2.3:2375"

func assertNoLeak(t *testing.T, what, text string) {
	t.Helper()
	for _, banned := range []string{"/home", "alice", ".local", "devbox", "10.1.2.3", "laptop", ".docker", "config.json"} {
		if strings.Contains(text, banned) {
			t.Errorf("%s leaks %q:\n%s", what, banned, text)
		}
	}
	// The JSON encoder writes "<" as \u003c.
	plain := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(text)
	if regexp.MustCompile(`tcp://[^<]`).MatchString(plain) {
		t.Errorf("%s leaks a tcp:// host:\n%s", what, text)
	}
}

func TestApplyAndInspectNeverLeakSubprocessStderr(t *testing.T) {
	id := hex64(0xd3)
	t.Run("apply: docker stop fails", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.setResp(stopCmd(id), fail(1, leakyDockerStderr))
		for _, args := range [][]string{{"apply", "--json", path}, {"apply", path}} {
			_, out, errOut := wfRun(t, w.f, w.f, "", args...)
			assertNoLeak(t, "apply stdout", out)
			assertNoLeak(t, "apply stderr", errOut)
			if !strings.Contains(out, "permission denied") {
				t.Errorf("the useful part of the error was lost: %s", out)
			}
		}
	})
	t.Run("apply: docker unreachable before acting", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.dockerFail = &fakeResp{err: fail(1, leakyDockerStderr).err}
		w.publish()
		_, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		assertNoLeak(t, "apply stdout", out)
		assertNoLeak(t, "apply stderr", errOut)
	})
	t.Run("apply: docker stop times out", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.setResp(stopCmd(id), fakeResp{err: &cmdError{Name: "docker", Code: -1, Stderr: leakyDockerStderr, Err: fmt.Errorf("timed out: %w", context.DeadlineExceeded)}})
		_, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		if r := decodeApplyDoc(t, out).Resources[0]; r.Result != resTimeoutUnknown || r.Error == "" {
			t.Errorf("resource = %+v", r)
		}
		assertNoLeak(t, "apply stdout", out)
		assertNoLeak(t, "apply stderr", errOut)
	})
	t.Run("plan: identity lookup errors in refusal reasons", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 101, "node")
		w.f.identErrs[100] = fmt.Errorf("open /home/alice/proc/100/stat on devbox-alice.local: %w", os.ErrPermission)
		w.f.identErrs[101] = errors.New("weird failure reading /home/alice/proc/101/stat for alice@laptop-7")
		w.publish()
		for _, args := range [][]string{{"plan", "--json", "3000", "3001"}, {"plan", "3000", "3001"}} {
			_, out, errOut := wfRun(t, w.f, w.f, "", args...)
			assertNoLeak(t, strings.Join(args, " ")+" stdout", out)
			assertNoLeak(t, strings.Join(args, " ")+" stderr", errOut)
			if !strings.Contains(out, "cannot bind process identity") {
				t.Errorf("the refusals should remain: %s", out)
			}
		}
	})
	t.Run("apply: native signal error", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.killErrs[100] = errors.New("kill: open /home/alice/x: failed for alice@laptop-7")
		_, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		assertNoLeak(t, "apply stdout", out)
		assertNoLeak(t, "apply stderr", errOut)
	})
	t.Run("inspect: docker ps fails", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.listen("tcp", 3000, 100, "node")
		w.dockerFail = &fakeResp{err: fail(1, leakyDockerStderr).err}
		w.publish()
		for _, args := range [][]string{{"inspect", "--json", "3000"}, {"inspect", "3000"}, {"plan", "--json", "3000"}, {"plan", "3000"}} {
			_, out, errOut := wfRun(t, w.f, w.f, "", args...)
			assertNoLeak(t, strings.Join(args, " ")+" stdout", out)
			assertNoLeak(t, strings.Join(args, " ")+" stderr", errOut)
		}
		_, out, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		var d inspectDoc
		decodeDoc(t, out, &d)
		if len(d.Warnings) == 0 {
			t.Errorf("the docker failure should still be a warning: %+v", d)
		}
		for _, wmsg := range d.Warnings {
			assertNoLeak(t, "inspect warning", wmsg)
			if len([]rune(wmsg)) > maxProviderMessage {
				t.Errorf("warning is %d runes", len([]rune(wmsg)))
			}
		}
	})
	t.Run("plan: docker proxy refusal names the docker error safely", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.listen("tcp", 3000, 800, "docker-proxy")
		w.dockerFail = &fakeResp{err: fail(1, leakyDockerStderr).err}
		w.publish()
		_, out, errOut := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		assertNoLeak(t, "plan stdout", out)
		assertNoLeak(t, "plan stderr", errOut)
	})
}

// ---- M6: a panic is an internal error, not a crash ------------------------------------

type panicSystem struct{ *fakeSystem }

func (p panicSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	panic("boom while running " + name + " in /home/alice/secret")
}

func TestPanicIsRecoveredAsInternalError(t *testing.T) {
	newPanicky := func() (*fakeSystem, panicSystem) {
		f := newFake("linux", "lsof", "docker")
		return f, panicSystem{f}
	}
	for _, args := range [][]string{
		{"inspect", "--json", "3000"}, {"plan", "--json", "3000"}, {"--json", "3000"}, {"--json", "--check"}, {"--json", "--dry-run", "3000"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f, sys := newPanicky()
			code, out, errOut := wfRun(t, sys, f, "", args...)
			if code != 1 {
				t.Errorf("exit = %d, want 1 (a runtime failure, not a usage error)", code)
			}
			m := decodeErr(t, out) // exactly one JSON document
			if m["code"] != "internal" || m["exit_class"] != "failed" || !strings.HasPrefix(m["error"], "internal error: boom") {
				t.Errorf("document = %v", m)
			}
			for _, bad := range []string{"goroutine", ".go:", "runtime.", "/home/alice"} {
				if strings.Contains(out, bad) || strings.Contains(errOut, bad) {
					t.Errorf("stack trace or path in the output (%q):\nstdout=%s\nstderr=%s", bad, out, errOut)
				}
			}
		})
	}
	t.Run("human output: one line on stderr, nothing on stdout", func(t *testing.T) {
		f, sys := newPanicky()
		code, out, errOut := wfRun(t, sys, f, "", "plan", "3000")
		if code != 1 || out != "" {
			t.Errorf("code=%d stdout=%q", code, out)
		}
		if lines := strings.Split(strings.TrimSpace(errOut), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "internal error: boom") {
			t.Errorf("stderr = %q", errOut)
		}
		if strings.Contains(errOut, "goroutine") || strings.Contains(errOut, "/home/alice") {
			t.Errorf("stderr leaks: %q", errOut)
		}
	})
	t.Run("a panic inside an apply action fails that resource only", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 101, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000", "3001")
		removeOnSignal(w)
		sys := &signalPanics{fakeSystem: w.f, pid: 100}
		code, out, _ := wfRun(t, sys, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 1 {
			t.Errorf("exit = %d", code)
		}
		bad, good := resultOf(ad, procResourceID(100, wfStart(100))), resultOf(ad, procResourceID(101, wfStart(101)))
		if bad.Result != resFailed || !strings.Contains(bad.Error, "internal error") || good.Result != resSignalSent {
			t.Errorf("bad=%+v good=%+v", bad, good)
		}
	})
}

type signalPanics struct {
	*fakeSystem
	pid int
}

func (s *signalPanics) SignalIfSame(pid int, start string, force bool) error {
	if pid == s.pid {
		panic("signal exploded")
	}
	return s.fakeSystem.SignalIfSame(pid, start, force)
}

// ---- nit: help --json ------------------------------------------------------------------------

func TestHelpCommandWithJSONIsHelp(t *testing.T) {
	for _, args := range [][]string{{"help", "--json"}, {"--json", "help"}, {"help", "-j"}} {
		code, out, _ := wfRun(t, newFake("linux"), nil, "", args...)
		m := decodeErr(t, out)
		if code != 0 || !strings.Contains(m["usage"], "Usage:") || !strings.Contains(m["usage"], "killport [port...]") {
			t.Errorf("%v: code=%d out=%s", args, code, out)
		}
	}
	code, out, _ := wfRun(t, newFake("linux"), nil, "", "help", "plan", "--json")
	m := decodeErr(t, out)
	if code != 0 || !strings.Contains(m["usage"], "--force") || !strings.Contains(m["usage"], "killport plan") {
		t.Errorf("help plan: code=%d out=%s", code, out)
	}
	// Without --json the cobra help is unchanged.
	code, out, _ = wfRun(t, newFake("linux"), nil, "", "help")
	if code != 0 || json.Valid([]byte(out)) || !strings.Contains(out, "Usage:") {
		t.Errorf("text help: code=%d out=%s", code, out)
	}
}

// ---- m1: PIDs beyond 32 bits --------------------------------------------------------------------

func TestPIDsBeyond32BitsAreRejected(t *testing.T) {
	for _, pid := range []int{1<<32 + 4, 1<<32 + 100, math.MaxInt32 + 1, 0, -1} {
		if err := validSignalPID(pid); err == nil {
			t.Errorf("validSignalPID(%d) accepted", pid)
		}
	}
	for _, pid := range []int{1, 4, 100, math.MaxInt32} {
		if err := validSignalPID(pid); err != nil {
			t.Errorf("validSignalPID(%d) = %v", pid, err)
		}
	}
	// A 64-bit PID that truncates to this very process must not be accepted
	// by the real platform code (read-only lookup only).
	alias := 1<<32 + os.Getpid()
	if id, err := newOSSystem().ProcessIdentity(alias); err == nil {
		t.Errorf("ProcessIdentity(%d) succeeded: %+v (aliases PID %d)", alias, id, os.Getpid())
	}
}

// ---- m2: an empty netstat is not an empty machine --------------------------------------------------

func TestEmptyNetstatIsAProviderErrorNotAFreePort(t *testing.T) {
	cases := []struct {
		goos, cmd string
		empty     []string
	}{
		{"darwin", cmdNetstatMacTCP, []string{"", "  \n\n\t\n"}},
		{"windows", cmdNetstat, []string{"", " \r\n\r\n"}},
	}
	for _, c := range cases {
		for _, empty := range c.empty {
			t.Run(fmt.Sprintf("%s %q", c.goos, empty), func(t *testing.T) {
				w := newWfWorld(c.goos, false)
				w.publish()
				w.f.setResp(c.cmd, ok(empty))
				code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
				d := decodePlanDoc(t, out)
				e := d.Endpoints[0]
				if e.Visibility == visComplete || code == 0 {
					t.Errorf("empty netstat output gave a complete empty table: code=%d endpoint=%+v", code, e)
				}
				want := visNoneVisible
				if c.goos == "windows" {
					want = visProviderUnavailable // netstat is the owner source on Windows
				}
				if e.Visibility != want {
					t.Errorf("visibility = %s, want %s", e.Visibility, want)
				}
				found := false
				for _, pe := range e.ProviderErrors {
					found = found || (pe.Provider == provNetstat && pe.Category == catParse)
				}
				if !found {
					t.Errorf("provider errors = %+v, want netstat/parse", e.ProviderErrors)
				}
				// And apply must not call the port free either.
				path, _ := planFile(t, w.f, w.f, "3000")
				_, aout, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
				if st := endpointStatus(decodeApplyDoc(t, aout), 3000); st != epNotVerifiable {
					t.Errorf("apply endpoint status = %s", st)
				}
			})
		}
	}
	t.Run("a header-only table is a real empty table", func(t *testing.T) {
		for _, goos := range []string{"darwin", "windows"} {
			w := newWfWorld(goos, false).publish() // renders the header line only
			code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
			d := decodePlanDoc(t, out)
			if code != 0 || d.Endpoints[0].Visibility != visComplete || d.Endpoints[0].Occupied {
				t.Errorf("%s: code=%d endpoint=%+v", goos, code, d.Endpoints[0])
			}
		}
	})
	t.Run("rows without a header are accepted", func(t *testing.T) {
		w := newWfWorld("darwin", false).publish()
		w.f.setResp(cmdNetstatMacTCP, ok("tcp4       0      0  *.3000                 *.*                    LISTEN\n"))
		w.f.setResp(cmdLsof, ok(lsofEntry(100, "node", "*:3000")))
		code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		d := decodePlanDoc(t, out)
		if code != 0 || !d.Endpoints[0].Occupied || d.Endpoints[0].Visibility != visComplete || len(d.Endpoints[0].ProviderErrors) != 0 {
			t.Errorf("code=%d endpoint = %+v", code, d.Endpoints[0])
		}
	})
}

// ---- m4: owners the plan refused ----------------------------------------------------------------------

func TestApplyReportsRefusedOwnersAsFailureNotFreshPlan(t *testing.T) {
	w := newWfWorld("linux", true)
	w.listen("tcp", 3000, 100, "node")
	w.listen("tcp", 3001, 1, "init")
	w.listen("tcp", 3002, 800, "docker-proxy")
	w.listen("tcp", 3003, 0, "") // nobody can see the owner
	w.listen("tcp", 3004, 300, "locked")
	w.f.identErrs[300] = os.ErrPermission
	w.publish()
	path, d := planFile(t, w.f, w.f, "3000-3004")
	if len(d.Resources) != 1 || len(d.Refused) != 4 {
		t.Fatalf("plan = %+v", d)
	}
	removeOnSignal(w)
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	for port, want := range map[int]string{3000: epFree, 3001: epBlockedRefused, 3002: epBlockedRefused, 3003: epBlockedRefused, 3004: epBlockedRefused} {
		if got := endpointStatus(ad, port); got != want {
			t.Errorf("port %d: status %s, want %s", port, got, want)
		}
	}
	if code != 1 || ad.ExitClass != "failed" {
		t.Errorf("exit = %d class = %s; a refused owner is a failure, not needs_fresh_plan (a fresh plan would refuse it again)", code, ad.ExitClass)
	}
	if applyClass(&applyDoc{Endpoints: []applyEndpoint{{Status: epBlockedRefused, Visibility: visComplete}}}) != exitFailed {
		t.Errorf("blocked_by_refused_owner must map to exit class 1")
	}

	t.Run("a genuinely new owner is still a fresh-plan case", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 1, "init")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000", "3001")
		w.listen("tcp", 3000, 777, "newcomer").listen("tcp", 3001, 778, "newcomer2").publish()
		removeOnSignal(w)
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		// 3001 holds the refused pid 1 AND a newcomer: the refusal decides
		// (a fresh plan would refuse pid 1 again).
		if endpointStatus(ad, 3000) != epBlockedNewOwner || endpointStatus(ad, 3001) != epBlockedRefused {
			t.Errorf("statuses: %+v", ad.Endpoints)
		}
		if code != 1 { // 1 (refused) outranks 3 (new owner)
			t.Errorf("exit = %d", code)
		}
	})

	t.Run("a container refused in native mode", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.listen("tcp", 3000, 100, "node")
		w.container(hex64(0xaa), "web", 0, 3001) // no visible proxy: only the container is judged
		w.publish()
		path, d := planFile(t, w.f, w.f, "--mode", "native", "3000", "3001")
		if len(d.Resources) != 1 || len(d.Refused) < 1 {
			t.Fatalf("plan = %+v", d)
		}
		removeOnSignal(w)
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if endpointStatus(ad, 3001) != epBlockedRefused || code != 1 {
			t.Errorf("code=%d endpoints=%+v", code, ad.Endpoints)
		}
	})
}

// ---- m6 (portable part): see procid_linux_test.go for the pidfd fallbacks --------------------------------

// ---- m8: deadline and cancellation -------------------------------------------------------------------------

func TestApplyDeadline(t *testing.T) {
	mk := func(n, grace int) *planDoc {
		return &planDoc{Request: planRequest{GraceSeconds: grace}, Resources: make([]planResource, n)}
	}
	for _, tc := range []struct {
		n, grace int
		want     time.Duration
	}{
		{0, 10, 100 * time.Second},                  // at least one wave
		{1, 10, 100 * time.Second},                  // 10s + 90s
		{4, 10, 100 * time.Second},                  // one wave of 4
		{5, 10, 190 * time.Second},                  // two waves
		{12, 30, 30*time.Second + 3*90*time.Second}, // three waves
		{100, 10, 10 * time.Minute},                 // capped at 10 minutes
		{1, 3600, 10 * time.Minute},                 // capped
	} {
		if got := applyDeadline(mk(tc.n, tc.grace)); got != tc.want {
			t.Errorf("applyDeadline(%d resources, grace %ds) = %v, want %v", tc.n, tc.grace, got, tc.want)
		}
	}
}

func TestApplyWithACancelledContextActsOnNothingAndReportsUncertain(t *testing.T) {
	w := newWfWorld("linux", true)
	w.listen("tcp", 3000, 100, "node")
	id := hex64(0x77)
	w.container(id, "web", 900, 3001)
	w.publish()
	path, _ := planFile(t, w.f, w.f, "3000", "3001")
	lsofBefore := w.f.runCount("lsof")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, errOut := wfRunCtx(t, ctx, ctxStrict{w.f}, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out) // exactly one document
	assertNoMutation(t, w.f)
	if code != exitUncertain || ad.ExitClass != "uncertain" {
		t.Errorf("exit = %d class = %s, want uncertain (4); stderr=%s", code, ad.ExitClass, errOut)
	}
	if !strings.HasPrefix(ad.Error, applyInterruptedPrefix) {
		t.Errorf("error = %q", ad.Error)
	}
	if len(ad.Resources) != 2 || ad.ActionsExecuted != 0 {
		t.Fatalf("resources = %+v", ad.Resources)
	}
	for _, r := range ad.Resources {
		if r.Result != resNotAttempted || !strings.Contains(r.Note, "interrupted") {
			t.Errorf("resource = %+v", r)
		}
	}
	// The outcome was still observed, once, and reported.
	if n := w.f.runCount("lsof") - lsofBefore; n != 1 {
		t.Errorf("lsof ran %d times after cancel, want exactly 1 (the brief outcome observation)", n)
	}
	if len(ad.Endpoints) != 2 || ad.Endpoints[0].Status != epBlockedOriginal || ad.Endpoints[1].Status != epBlockedOriginal {
		t.Errorf("endpoints = %+v", ad.Endpoints)
	}
	// Human output says it too.
	code, out, _ = wfRunCtx(t, ctx, ctxStrict{w.f}, w.f, "", "apply", path)
	if code != 4 || !strings.Contains(out, "apply interrupted") {
		t.Errorf("human: code=%d out=%s", code, out)
	}
}

func TestApplyCancelledMidwayStopsStartingNewActions(t *testing.T) {
	w := newWfWorld("linux", false)
	const n = 12
	var ports []string
	for i := 0; i < n; i++ {
		w.listen("tcp", 3000+i, 100+i, "node")
		ports = append(ports, fmt.Sprint(3000+i))
	}
	w.publish()
	path, _ := planFile(t, w.f, w.f, ports...)
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	w.f.onSignal = func(f *fakeSystem, s fakeSignal) {
		once.Do(cancel) // the user presses ^C during the first action
		w.removePID(s.PID)
		w.publish()
	}
	code, out, _ := wfRunCtx(t, ctx, ctxStrict{w.f}, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	sent, skipped := 0, 0
	for _, r := range ad.Resources {
		switch r.Result {
		case resSignalSent:
			sent++
		case resNotAttempted:
			skipped++
		}
	}
	if sent < 1 || sent > maxConcurrentActions || skipped != n-sent || ad.ActionsExecuted != sent {
		t.Errorf("sent=%d skipped=%d executed=%d (at most %d actions may already have been in flight)", sent, skipped, ad.ActionsExecuted, maxConcurrentActions)
	}
	if w.f.signalCount() != sent {
		t.Errorf("signals = %d, sent = %d", w.f.signalCount(), sent)
	}
	if code != exitUncertain || ad.ExitClass != "uncertain" || !strings.HasPrefix(ad.Error, applyInterruptedPrefix) {
		t.Errorf("code=%d class=%s error=%q", code, ad.ExitClass, ad.Error)
	}
}

func TestApplyDockerStopInterruptedIsTimeoutUnknownOrGone(t *testing.T) {
	id := hex64(0x99)
	w := newWfWorld("linux", true)
	w.container(id, "web", 900, 3000)
	w.publish()
	path, _ := planFile(t, w.f, w.f, "3000")
	ctx, cancel := context.WithCancel(context.Background())
	// docker stop is killed by the cancellation; the daemon keeps stopping.
	w.f.setResp(stopCmd(id), fakeResp{err: &cmdError{Name: "docker", Code: -1, Err: fmt.Errorf("timed out: %w", context.Canceled)}})
	h := &hookFake{fakeSystem: w.f}
	h.afterRun = func(key string, err error) {
		if key == stopCmd(id) {
			cancel()
		}
	}
	code, out, _ := wfRunCtx(t, ctx, ctxStrict{h}, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if r := ad.Resources[0]; r.Result != resTimeoutUnknown {
		t.Errorf("resource = %+v", r)
	}
	if code != exitUncertain {
		t.Errorf("exit = %d", code)
	}
}

// blockingStop makes `docker stop` hang until its context is done, as a stuck
// daemon would.
type blockingStop struct {
	system
	stop string
}

func (b blockingStop) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.Join(append([]string{name}, args...), " ") == b.stop {
		<-ctx.Done()
		return nil, &cmdError{Name: name, Code: -1, Err: fmt.Errorf("timed out: %w", ctx.Err())}
	}
	return b.system.Run(ctx, name, args...)
}

func TestApplyStopsAtItsTotalDeadline(t *testing.T) {
	old := applyWaveTimeout
	applyWaveTimeout = 80 * time.Millisecond
	defer func() { applyWaveTimeout = old }()

	id := hex64(0xdead)
	w := newWfWorld("linux", true)
	w.container(id, "web", 900, 3000)
	w.publish()
	path, _ := planFile(t, w.f, w.f, "--grace", "0s", "3000")
	stop := "docker stop -t 0 " + id
	w.f.setResp(stop, ok(""))

	start := time.Now()
	code, out, _ := wfRun(t, ctxStrict{blockingStop{system: w.f, stop: stop}}, w.f, "", "apply", "--json", path)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("apply took %v although its total deadline is ~80ms (the docker stop wrapper alone allows 20s)", d)
	}
	ad := decodeApplyDoc(t, out)
	if r := ad.Resources[0]; r.Result != resTimeoutUnknown {
		t.Errorf("resource = %+v", r)
	}
	if code != exitUncertain || !strings.HasPrefix(ad.Error, applyInterruptedPrefix) || !strings.Contains(ad.Error, "time limit") {
		t.Errorf("code=%d class=%s error=%q", code, ad.ExitClass, ad.Error)
	}
}

func TestSleepCtxIsContextAware(t *testing.T) {
	f := newFake("linux")
	sleepCtx(context.Background(), f, time.Second)
	if f.sleeps != 1 {
		t.Errorf("sleeps = %d", f.sleeps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sleepCtx(ctx, f, time.Second)
	if f.sleeps != 1 {
		t.Errorf("a cancelled context must not sleep; sleeps = %d", f.sleeps)
	}
	// The real system wakes up when cancelled.
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel2() }()
	start := time.Now()
	sleepCtx(ctx2, newOSSystem(), 10*time.Second)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("sleepCtx did not wake on cancel: %v", d)
	}
	// And the wrappers pass the context through.
	ctx3, cancel3 := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel3() }()
	start = time.Now()
	sleepCtx(ctx3, newVerboseSystem(newOSSystem(), &bytes.Buffer{}), 10*time.Second)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("verbose wrapper did not wake on cancel: %v", d)
	}
}

// ---- nits: plan -o, control bytes ---------------------------------------------------------------------------

func TestPlanOutputIsPrivateAndNeverWritesThroughASymlink(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	dir := t.TempDir()

	path := filepath.Join(dir, "plan.json")
	if code, _, errOut := wfRun(t, w.f, w.f, "", "plan", "-o", path, "3000"); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("plan file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
	// Overwriting a regular file is fine, and stays private.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := wfRun(t, w.f, w.f, "", "plan", "-o", path, "3000"); code != 0 {
		t.Fatalf("overwrite: exit %d %s", code, errOut)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("overwritten plan file mode = %v", fi.Mode().Perm())
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".killport-plan-*")); len(matches) != 0 {
		t.Errorf("temporary files left behind: %v", matches)
	}

	if runtime.GOOS == "windows" {
		return
	}
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "-o", link, "3000")
	m := decodeErr(t, out)
	if code != 1 || m["code"] != "write_failed" || !strings.Contains(m["error"], "symbolic link") {
		t.Errorf("code=%d out=%s", code, out)
	}
	if data, _ := os.ReadFile(victim); string(data) != "precious" {
		t.Errorf("the symlink target was overwritten: %q", data)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link itself was replaced: %v %v", fi, err)
	}
	// A dangling link is refused as well.
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "-o", dangling, "3000"); code != 1 {
		t.Errorf("dangling link: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "does-not-exist.json")); err == nil {
		t.Errorf("the dangling link target was created")
	}
	// A directory is not a plan file.
	if code, _, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "-o", dir, "3000"); code != 1 {
		t.Errorf("directory: exit %d", code)
	}
}

func TestHumanOutputStripsControlBytesFromNames(t *testing.T) {
	evil := "node\x1b[31m\x07\r\u202e\u0085"
	if got := humanText(evil); strings.ContainsAny(got, "\x1b\x07\r\u202e\u0085") || got != "node?[31m????" {
		t.Errorf("humanText = %q", got)
	}
	if got := humanText("caf\xe9 ok é"); got != "caf\uFFFD ok é" {
		t.Errorf("humanText = %q", got)
	}
	cid := hex64(0xe1)
	w := newWfWorld("linux", true)
	w.listen("tcp", 3000, 100, "node\x1b[2Jevil")
	w.container(cid, "web\x1b]0;pwned\x07", 0, 3001)
	w.publish()
	w.f.setResp(stopCmd(cid), ok(""))
	h := &hookFake{fakeSystem: w.f}
	h.afterRun = func(key string, err error) {}
	for _, args := range [][]string{{"inspect", "3000", "3001"}, {"plan", "3000", "3001"}, {"inspect"}} {
		_, out, _ := wfRun(t, h, w.f, "", args...)
		if strings.ContainsAny(out, "\x1b\x07") {
			t.Errorf("%v: terminal escape in human output: %q", args, out)
		}
		if !strings.Contains(out, "node?[2Jevil") {
			t.Errorf("%v: the name should be visible with '?' for the escape byte:\n%s", args, out)
		}
	}
	// Apply (holder lines) and the legacy commands.
	path, _ := planFile(t, h, w.f, "3000", "3001")
	_, out, _ := wfRun(t, h, w.f, "", "apply", path)
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("apply: terminal escape in human output: %q", out)
	}
	for _, args := range [][]string{{"--check", "3000"}, {"--dry-run", "3000", "3001"}} {
		f := newFake("linux", "lsof")
		f.on(cmdLsof, ok(lsofEntry(100, "node\x1b[2Jevil", "*:3000")))
		_, out, _ = wfRun(t, f, f, "", args...)
		if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "node?[2Jevil") {
			t.Errorf("legacy %v: %q", args, out)
		}
	}
	// JSON keeps the original bytes escaped by the encoder (never raw).
	_, out, _ = wfRun(t, h, w.f, "", "inspect", "--json", "3000")
	if strings.ContainsAny(out, "\x1b\x07") || !json.Valid([]byte(out)) {
		t.Errorf("json: %q", out)
	}
}

// ---- gaps found by mutation testing ---------------------------------------------------------------------------------

// A container published without the userland proxy leaves no socket row and
// no lsof owner: only `docker ps` shows the port. It must count as occupied.
func TestContainerOnlyOccupancyIsNeverFree(t *testing.T) {
	id := hex64(0xc1)
	build := func() *wfWorld {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.dropProxy(id)
		return w.publish()
	}
	w := build()
	if strings.Count(string(w.f.files["/proc/net/tcp"]), "\n") != 1 {
		t.Fatalf("the socket table should hold no row:\n%s", w.f.files["/proc/net/tcp"])
	}

	_, out, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000", "3001")
	var d inspectDoc
	decodeDoc(t, out, &d)
	e := d.Endpoints[0]
	if !e.Occupied || len(e.Containers) != 1 || len(e.Owners) != 0 || e.Visibility != visComplete {
		t.Errorf("inspect 3000 = %+v", e)
	}
	if d.Endpoints[1].Occupied {
		t.Errorf("inspect 3001 = %+v", d.Endpoints[1])
	}
	// "inspect" without ports lists it as occupied, too.
	_, out, _ = wfRun(t, w.f, w.f, "", "inspect", "--json")
	decodeDoc(t, out, &d)
	if len(d.Endpoints) != 1 || d.Endpoints[0].Port != 3000 || !d.Endpoints[0].Occupied {
		t.Errorf("inspect all = %+v", d.Endpoints)
	}
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	pd := decodePlanDoc(t, out)
	if code != 0 || !pd.Endpoints[0].Occupied || len(pd.Resources) != 1 || len(pd.Refused) != 0 {
		t.Errorf("plan code=%d %+v", code, pd)
	}

	t.Run("stop fails: still occupied, never free", func(t *testing.T) {
		w := build()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.setResp(stopCmd(id), fail(1, "Error response from daemon: boom"))
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if st := endpointStatus(ad, 3000); st != epBlockedOriginal || code != 1 {
			t.Errorf("status %s code %d", st, code)
		}
	})
	t.Run("stop succeeds but the container is still listed: not free", func(t *testing.T) {
		w := build()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.setResp(stopCmd(id), ok(""))
		_, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		if st := endpointStatus(decodeApplyDoc(t, out), 3000); st == epFree {
			t.Errorf("a port that docker ps still publishes was called free")
		}
	})
	t.Run("stop succeeds and the container is gone: free", func(t *testing.T) {
		w := build()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.setResp(stopCmd(id), ok(""))
		h := &hookFake{fakeSystem: w.f}
		h.afterRun = func(key string, err error) {
			if key == stopCmd(id) {
				w.removeContainer(id)
				w.publish()
			}
		}
		code, out, _ := wfRun(t, h, w.f, "", "apply", "--json", path)
		if st := endpointStatus(decodeApplyDoc(t, out), 3000); st != epFree || code != 0 {
			t.Errorf("status %s code %d", st, code)
		}
	})
}

// An owner whose identity cannot be read cannot be judged: after the action it
// is "not verifiable", not a confirmed new owner.
func TestApplyUnreadableIdentityOfAnotherOwnerIsNotVerifiable(t *testing.T) {
	w := newWfWorld("linux", false)
	w.listen("tcp", 3000, 100, "node")
	w.publish()
	path, _ := planFile(t, w.f, w.f, "3000")
	w.listen("tcp", 3000, 200, "locked").publish()
	w.f.identErrs[200] = os.ErrPermission
	removeOnSignal(w)
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if st := endpointStatus(ad, 3000); st != epNotVerifiable || code != exitUncertain {
		t.Errorf("status %s code %d; an owner with an unreadable identity is unknown, not 'new'", st, code)
	}
	if w.f.signalCount() != 1 {
		t.Errorf("signals = %v", w.f.signals)
	}
}

// lsof may list a process that exited before its identity was read; it must not
// be treated as an owner (the socket table decides occupancy).
func TestOwnerThatExitedBeforeItsIdentityWasReadIsIgnored(t *testing.T) {
	f := newFake("linux", "lsof")
	f.setFile("/proc/net/tcp", procTCPHeader)
	f.setFile("/proc/net/udp", procTCPHeader)
	f.on(cmdLsof, ok(lsofEntry(100, "node", "*:3000")))
	f.exitProc(100)
	code, out, _ := wfRun(t, f, f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	e := d.Endpoints[0]
	if code != 0 || e.Occupied || e.Visibility != visComplete || len(e.Resources) != 0 || len(d.Refused) != 0 || len(d.Resources) != 0 {
		t.Errorf("code=%d plan=%+v", code, d)
	}
	_, out, _ = wfRun(t, f, f, "", "inspect", "--json", "3000")
	var id inspectDoc
	decodeDoc(t, out, &id)
	if id.Endpoints[0].Occupied || len(id.Endpoints[0].Owners) != 0 {
		t.Errorf("inspect = %+v", id.Endpoints[0])
	}
}

// Every read-only command runs under the readOnlySystem guard: deleting the
// wrapper from any of them must fail this test.
type recordingSystem struct {
	system
	mu   *sync.Mutex
	runs *[]string
}

func (r recordingSystem) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	*r.runs = append(*r.runs, name)
	r.mu.Unlock()
	return r.system.Run(ctx, name, args...)
}

func TestReadOnlyCommandsRunUnderTheReadOnlyWrapper(t *testing.T) {
	orig := readOnlyWrap
	defer func() { readOnlyWrap = orig }()
	var (
		wraps int32
		mu    sync.Mutex
		runs  []string
	)
	readOnlyWrap = func(s system) system {
		atomic.AddInt32(&wraps, 1)
		return recordingSystem{system: orig(s), mu: &mu, runs: &runs}
	}
	world := func() *wfWorld {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		return w.publish()
	}
	reset := func() {
		atomic.StoreInt32(&wraps, 0)
		mu.Lock()
		runs = nil
		mu.Unlock()
	}
	for _, tc := range []struct {
		name     string
		args     []string
		readOnly bool
	}{
		{"plan", []string{"plan", "--json", "3000"}, true},
		{"plan human", []string{"plan", "3000"}, true},
		{"inspect", []string{"inspect", "--json", "3000"}, true},
		{"inspect all", []string{"inspect"}, true},
		{"legacy dry-run", []string{"--dry-run", "--json", "3000"}, true},
		{"legacy dry-run short", []string{"-d", "3000"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			w := world()
			wfRun(t, w.f, w.f, "", tc.args...)
			mu.Lock()
			n := len(runs)
			mu.Unlock()
			if atomic.LoadInt32(&wraps) != 1 || n == 0 {
				t.Errorf("%v: wrapped %d time(s), %d command(s) went through the wrapper; every command must run under readOnlySystem", tc.args, wraps, n)
			}
		})
	}
	t.Run("apply and the legacy kill do not", func(t *testing.T) {
		reset()
		w := world()
		path, _ := planFile(t, w.f, w.f, "3000")
		reset()
		removeOnSignal(w)
		wfRun(t, w.f, w.f, "", "apply", "--json", path)
		w2 := world()
		removeOnSignal(w2)
		wfRun(t, w2.f, w2.f, "", "--json", "3000")
		if n := atomic.LoadInt32(&wraps); n != 0 {
			t.Errorf("a mutating command was wrapped read-only %d time(s)", n)
		}
	})
}
