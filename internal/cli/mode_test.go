package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Tests of --mode auto|native|container and of the observation metadata
// (observation_id, docker_context, provider_errors).

func init() {
	// The observation documents record DOCKER_CONTEXT; keep the whole test
	// binary independent of the developer's shell. Tests that need it use
	// t.Setenv.
	os.Unsetenv("DOCKER_CONTEXT")
	os.Unsetenv("DOCKER_HOST")
}

var obsIDRe = regexp.MustCompile(`^obs:[0-9a-f]{16}$`)

var dockerDownResp = fail(1, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")

// modeWorld builds one of the scenarios and returns the world plus the ports
// to plan.
func modeWorld(scenario string, id string) (*wfWorld, []string) {
	switch scenario {
	case "native only":
		return newWfWorld("linux", true).listen("tcp", 3000, 100, "node").publish(), []string{"3000"}
	case "native only, docker not installed":
		return newWfWorld("linux", false).listen("tcp", 3000, 100, "node").publish(), []string{"3000"}
	case "container only":
		return newWfWorld("linux", true).container(id, "web", 900, 3000).publish(), []string{"3000"}
	case "both":
		return newWfWorld("linux", true).listen("tcp", 3000, 100, "node").container(id, "web", 900, 3001).publish(), []string{"3000", "3001"}
	case "docker down":
		w := newWfWorld("linux", true).listen("tcp", 3000, 100, "node").listen("tcp", 3001, 900, "docker-proxy")
		w.dockerFail = &fakeResp{err: dockerDownResp.err}
		return w.publish(), []string{"3000", "3001"}
	}
	panic("unknown scenario " + scenario)
}

type wantRefusal struct {
	port, pid int
	reason    string // substring
}

func TestModeMatrix(t *testing.T) {
	id := hex64(0xc0ffee)
	proc := procResourceID(100, wfStart(100))
	cont := containerResourceID(id)
	rc, rn := reasonContainerPublishes, reasonNativeOwner

	type want struct {
		resources []string
		refused   []wantRefusal
		exit      int
	}
	cases := []struct {
		scenario string
		auto     want
		native   want
		cont     want
	}{
		{"native only",
			want{[]string{proc}, nil, 0},
			want{[]string{proc}, nil, 0},
			want{nil, []wantRefusal{{3000, 100, rn}}, 1}},
		{"native only, docker not installed",
			want{[]string{proc}, nil, 0},
			want{[]string{proc}, nil, 0},
			want{nil, []wantRefusal{{3000, 100, rn}}, 1}},
		{"container only",
			want{[]string{cont}, nil, 0},
			// The container (and its port proxy) still occupy the port, so it
			// is never called free; native mode just refuses to act on them.
			want{nil, []wantRefusal{{3000, 0, rc}, {3000, 900, rc}}, 1},
			want{[]string{cont}, nil, 0}},
		{"both",
			want{[]string{cont, proc}, nil, 0},
			want{[]string{proc}, []wantRefusal{{3001, 0, rc}, {3001, 900, rc}}, 1},
			want{[]string{cont}, []wantRefusal{{3000, 100, rn}}, 1}},
		{"docker down",
			want{[]string{proc}, []wantRefusal{{3001, 900, "Docker infrastructure"}}, 1},
			want{[]string{proc}, []wantRefusal{{3001, 900, rc}}, 1},
			want{nil, []wantRefusal{{3000, 100, rn}, {3001, 900, "Docker infrastructure"}}, 1}},
	}

	for _, tc := range cases {
		for _, m := range []struct {
			mode string
			want want
		}{{"auto", tc.auto}, {"native", tc.native}, {"container", tc.cont}} {
			t.Run(tc.scenario+"/"+m.mode, func(t *testing.T) {
				w, ports := modeWorld(tc.scenario, id)
				args := append([]string{"plan", "--json", "--mode", m.mode}, ports...)
				code, out, errOut := wfRun(t, w.f, w.f, "", args...)
				d := decodePlanDoc(t, out)
				if code != m.want.exit {
					t.Fatalf("exit %d, want %d\nstderr=%s\n%s", code, m.want.exit, errOut, out)
				}
				if d.Request.Mode != m.mode {
					t.Errorf("request.mode = %q", d.Request.Mode)
				}
				var res []string
				for _, r := range d.Resources {
					res = append(res, r.ResourceID)
				}
				if !reflect.DeepEqual(res, m.want.resources) {
					t.Errorf("resources = %v, want %v", res, m.want.resources)
				}
				if len(d.Refused) != len(m.want.refused) {
					t.Fatalf("refused = %+v, want %+v", d.Refused, m.want.refused)
				}
				for i, wr := range m.want.refused {
					got := d.Refused[i]
					if got.Port != wr.port || got.PID != wr.pid || !strings.Contains(got.Reason, wr.reason) {
						t.Errorf("refused[%d] = %+v, want port %d pid %d reason containing %q", i, got, wr.port, wr.pid, wr.reason)
					}
				}
				for _, e := range d.Endpoints {
					if !e.Occupied {
						t.Errorf("endpoint %d reported free in mode %s: %+v", e.Port, m.mode, e)
					}
				}
				if d.ActionsExecuted != 0 {
					t.Errorf("actions_executed = %d", d.ActionsExecuted)
				}
				assertNoMutation(t, w.f)
			})
		}
	}
}

func TestModeNativeNeverQueriesAContainerForAction(t *testing.T) {
	// Docker is consulted read-only (so a published container is never
	// missed) and no container ever becomes a resource or is stopped.
	id := hex64(0xbeef)
	w, ports := modeWorld("both", id)
	_, out, _ := wfRun(t, w.f, w.f, "", append([]string{"plan", "--json", "--mode", "native"}, ports...)...)
	d := decodePlanDoc(t, out)
	for _, r := range d.Resources {
		if r.Provider == providerDocker {
			t.Errorf("native mode planned a container: %+v", r)
		}
	}
	if w.f.runCount("docker stop") != 0 || w.f.runCount("docker kill") != 0 {
		t.Errorf("docker was asked to mutate: %v", w.f.runs)
	}
	// 3001 is held by the container: its endpoint lists no resource.
	for _, e := range d.Endpoints {
		if e.Port == 3001 && len(e.Resources) != 0 {
			t.Errorf("endpoint 3001 = %+v", e)
		}
	}
}

func TestModeInspectRecordsModeAndStillReportsContainers(t *testing.T) {
	id := hex64(0xabcdef)
	for _, mode := range []string{"auto", "native", "container"} {
		t.Run(mode, func(t *testing.T) {
			w, _ := modeWorld("both", id)
			code, out, errOut := wfRun(t, w.f, w.f, "", "inspect", "--json", "--mode", mode, "3000", "3001")
			if code != 0 {
				t.Fatalf("exit %d stderr=%s\n%s", code, errOut, out)
			}
			var d inspectDoc
			decodeDoc(t, out, &d)
			if d.Request.Mode != mode {
				t.Errorf("request.mode = %q", d.Request.Mode)
			}
			e := d.Endpoints[1]
			if !e.Occupied || len(e.Containers) != 1 || e.Containers[0].ContainerID != id {
				t.Errorf("a published container must be reported as an occupant in every mode: %+v", e)
			}
			if !obsIDRe.MatchString(d.ObservationID) {
				t.Errorf("observation_id = %q", d.ObservationID)
			}
		})
	}
	// The default is auto.
	w, _ := modeWorld("native only", id)
	_, out, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
	var d inspectDoc
	decodeDoc(t, out, &d)
	if d.Request.Mode != "auto" {
		t.Errorf("default mode = %q", d.Request.Mode)
	}
}

func TestModeChangesPlanHash(t *testing.T) {
	id := hex64(0x1234)
	plans := map[string]planDoc{}
	for _, mode := range []string{"auto", "native", "container"} {
		w, ports := modeWorld("native only", id)
		_, out, _ := wfRun(t, w.f, w.f, "", append([]string{"plan", "--json", "--mode", mode}, ports...)...)
		plans[mode] = decodePlanDoc(t, out)
	}
	// Default (no flag) equals explicit auto.
	w, ports := modeWorld("native only", id)
	_, out, _ := wfRun(t, w.f, w.f, "", append([]string{"plan", "--json"}, ports...)...)
	if def := decodePlanDoc(t, out); def.PlanID != plans["auto"].PlanID {
		t.Errorf("default plan %s differs from --mode auto %s", def.PlanID, plans["auto"].PlanID)
	}
	modes := []string{"auto", "native", "container"}
	for i := range modes {
		for j := i + 1; j < len(modes); j++ {
			a, b := plans[modes[i]], plans[modes[j]]
			if a.PlanID == b.PlanID {
				t.Errorf("%s and %s share plan_id %s", modes[i], modes[j], a.PlanID)
			}
			if a.ObservationID == b.ObservationID {
				t.Errorf("%s and %s share observation_id %s", modes[i], modes[j], a.ObservationID)
			}
		}
	}
	// Identical resources under auto and native still hash differently
	// because the mode is part of the request.
	if !reflect.DeepEqual(plans["auto"].Resources, plans["native"].Resources) {
		t.Fatalf("expected the same resources: %+v vs %+v", plans["auto"].Resources, plans["native"].Resources)
	}
	// And the id really covers the mode: editing it is detected.
	p := plans["native"]
	p.Request.Mode = "auto"
	if computePlanID(p) == plans["native"].PlanID {
		t.Errorf("plan_id does not cover request.mode")
	}
}

func TestModeInvalidIsUsageErrorAsJSON(t *testing.T) {
	for _, sub := range []string{"plan", "inspect"} {
		for _, bad := range []string{"bogus", "Native", "AUTO", "", "native,container"} {
			t.Run(sub+"/"+bad, func(t *testing.T) {
				w, _ := modeWorld("native only", hex64(1))
				code, out, _ := wfRun(t, w.f, w.f, "", sub, "--json", "--mode="+bad, "3000")
				m := decodeErr(t, out)
				if code != 2 || m["code"] != "usage" || m["exit_class"] != "usage" || !strings.Contains(m["error"], "--mode") {
					t.Fatalf("code=%d out=%s", code, out)
				}
				if len(w.f.runs) != 0 {
					t.Errorf("an invalid mode must fail before observing anything: %v", w.f.runs)
				}
			})
		}
	}
	// apply has no mode flag: the plan decides.
	w, _ := modeWorld("native only", hex64(1))
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", "--mode", "native", "x.json")
	m := decodeErr(t, out)
	if code != 2 || m["code"] != "usage" {
		t.Errorf("apply --mode: code=%d out=%s", code, out)
	}
}

func TestModeApplyHonoursThePlan(t *testing.T) {
	id := hex64(0xfab)
	t.Run("native plan only signals; the container is left alone", func(t *testing.T) {
		w, ports := modeWorld("both", id)
		path, d := planFile(t, w.f, w.f, append([]string{"--mode", "native"}, ports...)...)
		if len(d.Resources) != 1 || d.Resources[0].Provider != providerNative {
			t.Fatalf("plan = %+v", d.Resources)
		}
		w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if w.f.runCount("docker stop") != 0 {
			t.Errorf("a native plan stopped a container: %v", w.f.runs)
		}
		if w.f.signalCount() != 1 || ad.ActionsExecuted != 1 {
			t.Errorf("signals=%d doc=%+v", w.f.signalCount(), ad)
		}
		if endpointStatus(ad, 3000) != epFree {
			t.Errorf("3000 = %s", endpointStatus(ad, 3000))
		}
		// The container still holds 3001: never reported free.
		if s := endpointStatus(ad, 3001); s == epFree || code == 0 {
			t.Errorf("3001 = %s exit %d; the container still publishes it", s, code)
		}
		if !obsIDRe.MatchString(ad.ObservationID) {
			t.Errorf("observation_id = %q", ad.ObservationID)
		}
	})
	t.Run("container plan only stops containers", func(t *testing.T) {
		w, ports := modeWorld("both", id)
		path, d := planFile(t, w.f, w.f, append([]string{"--mode", "container"}, ports...)...)
		if len(d.Resources) != 1 || d.Resources[0].Provider != providerDocker {
			t.Fatalf("plan = %+v", d.Resources)
		}
		w.f.setResp(stopCmd(id), ok(""))
		h := &hookFake{fakeSystem: w.f}
		h.afterRun = func(key string, err error) {
			if key == stopCmd(id) {
				w.removeContainer(id)
				w.publish()
			}
		}
		_, out, _ := wfRun(t, h, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if w.f.signalCount() != 0 || w.f.killCount() != 0 {
			t.Errorf("a container plan signalled a process: %v", w.f.signals)
		}
		if w.f.runCount(stopCmd(id)) != 1 || ad.ActionsExecuted != 1 {
			t.Errorf("runs=%v doc=%+v", w.f.runs, ad)
		}
		if endpointStatus(ad, 3001) != epFree {
			t.Errorf("3001 = %s", endpointStatus(ad, 3001))
		}
		if s := endpointStatus(ad, 3000); s == epFree {
			t.Errorf("3000 is still held by the native process but was %s", s)
		}
	})
	t.Run("forged plans that contradict their mode are rejected", func(t *testing.T) {
		w, ports := modeWorld("both", id)
		_, auto := planFile(t, w.f, w.f, ports...)
		cases := map[string]func(*planDoc){
			"native mode with a container": func(d *planDoc) { d.Request.Mode = modeNative },
			"container mode with a process": func(d *planDoc) {
				d.Request.Mode = modeContainer
			},
			"unknown mode": func(d *planDoc) { d.Request.Mode = "everything" },
		}
		if len(auto.Resources) != 2 {
			t.Fatalf("plan = %+v", auto.Resources)
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				code, out, _ := wfRun(t, w.f, w.f, forgePlan(t, auto, mutate), "apply", "--json", "-")
				m := decodeErr(t, out)
				if code != 2 || m["code"] != "usage" || !strings.Contains(m["error"], "invalid plan") {
					t.Fatalf("code=%d out=%s", code, out)
				}
				assertNoMutation(t, w.f)
			})
		}
	})
}

// ---- observation id ------------------------------------------------------------

func TestObservationIDIsDeterministicAndSensitive(t *testing.T) {
	id := hex64(0x77)
	ids := func(w *wfWorld) (plan, inspect string) {
		_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		plan = decodePlanDoc(t, out).ObservationID
		_, out, _ = wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		var d inspectDoc
		decodeDoc(t, out, &d)
		return plan, d.ObservationID
	}
	w1, _ := modeWorld("native only", id)
	p1, i1 := ids(w1)
	w1.f.advance(37 * time.Minute) // no time in the id
	p1b, i1b := ids(w1)
	w2, _ := modeWorld("native only", id)
	p2, i2 := ids(w2)
	for _, x := range []string{p1, i1, p1b, i1b, p2, i2} {
		if !obsIDRe.MatchString(x) {
			t.Fatalf("bad observation id %q", x)
		}
	}
	if p1 != p1b || p1 != p2 || i1 != i1b || i1 != i2 {
		t.Errorf("ids are not deterministic: %s %s %s / %s %s %s", p1, p1b, p2, i1, i1b, i2)
	}
	if p1 == i1 {
		t.Errorf("plan and inspect answer different requests but share %s", p1)
	}

	// A different world, or a different identity, gives a different id.
	w3 := newWfWorld("linux", true).listen("tcp", 3000, 101, "node").publish()
	p3, _ := ids(w3)
	w4, _ := modeWorld("native only", id)
	w4.f.replaceProc(100, "another-start-time")
	p4, _ := ids(w4)
	w5, _ := modeWorld("container only", id)
	p5, _ := ids(w5)
	seen := map[string]string{p1: "base"}
	for name, x := range map[string]string{"other pid": p3, "other start time": p4, "container": p5} {
		if prev, dup := seen[x]; dup {
			t.Errorf("%s collides with %s: %s", name, prev, x)
		}
		seen[x] = name
	}
	// Request changes (force) change it too.
	_, out, _ := wfRun(t, w1.f, w1.f, "", "plan", "--json", "--force", "3000")
	if decodePlanDoc(t, out).ObservationID == p1 {
		t.Errorf("--force did not change the observation id")
	}
}

// ---- docker_context ------------------------------------------------------------

func TestDockerContextOmittedWhenUnsetAndRecordedWhenSet(t *testing.T) {
	keys := func(out string) map[string]any {
		var m map[string]any
		decodeDoc(t, out, &m)
		return m
	}
	t.Run("unset", func(t *testing.T) {
		t.Setenv("DOCKER_CONTEXT", "")
		w, _ := modeWorld("native only", hex64(1))
		_, pout, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		_, iout, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
		_, aout, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		for name, out := range map[string]string{"plan": pout, "inspect": iout, "apply": aout} {
			if _, has := keys(out)["docker_context"]; has {
				t.Errorf("%s has docker_context although DOCKER_CONTEXT is unset:\n%s", name, out)
			}
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv("DOCKER_CONTEXT", "colima")
		t.Setenv("DOCKER_HOST", "tcp://build-host.example:2375")
		w, _ := modeWorld("native only", hex64(1))
		_, pout, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		_, iout, _ := wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }
		_, aout, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		for name, out := range map[string]string{"plan": pout, "inspect": iout, "apply": aout} {
			if got := keys(out)["docker_context"]; got != "colima" {
				t.Errorf("%s docker_context = %v", name, got)
			}
			if strings.Contains(out, "build-host") || strings.Contains(out, "2375") {
				t.Errorf("%s leaks DOCKER_HOST:\n%s", name, out)
			}
		}
		// An unset and a set context observe different worlds.
		t.Setenv("DOCKER_CONTEXT", "")
		_, pout2, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		if decodePlanDoc(t, pout).ObservationID == decodePlanDoc(t, pout2).ObservationID {
			t.Errorf("docker_context is not part of the observation id")
		}
	})
	t.Run("a path-like context is sanitised", func(t *testing.T) {
		t.Setenv("DOCKER_CONTEXT", "/home/alice/.docker/ctx")
		w, _ := modeWorld("native only", hex64(1))
		_, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
		if d := decodePlanDoc(t, out); d.DockerContext != "<path>" || strings.Contains(out, "alice") {
			t.Errorf("docker_context = %q", d.DockerContext)
		}
	})
}

// ---- plan_id coverage ------------------------------------------------------------

func TestPlanIDCoversObservationFields(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "desktop-linux")
	w, _ := modeWorld("docker down", hex64(5))
	path, d := planFile(t, w.f, w.f, "3000", "3001")
	if d.ObservationID == "" || d.DockerContext != "desktop-linux" || len(d.Endpoints[0].ProviderErrors) == 0 {
		t.Fatalf("plan lacks observation metadata: %+v", d)
	}
	mutations := map[string]func(*planDoc){
		"observation_id": func(p *planDoc) { p.ObservationID = "obs:0000000000000000" },
		"docker_context": func(p *planDoc) { p.DockerContext = "other" },
		"provider_error message": func(p *planDoc) {
			p.Endpoints[0].ProviderErrors = append([]providerError{}, p.Endpoints[0].ProviderErrors...)
			p.Endpoints[0].ProviderErrors[0].Message += "!"
		},
		"provider_error category": func(p *planDoc) {
			p.Endpoints[0].ProviderErrors = append([]providerError{}, p.Endpoints[0].ProviderErrors...)
			p.Endpoints[0].ProviderErrors[0].Category = "timeout"
		},
		"provider_errors dropped": func(p *planDoc) { p.Endpoints[0].ProviderErrors = []providerError{} },
		"mode":                    func(p *planDoc) { p.Request.Mode = "native" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cp := d
			cp.Endpoints = append([]planEndpoint{}, d.Endpoints...)
			mutate(&cp)
			if computePlanID(cp) == d.PlanID {
				t.Fatalf("plan_id unchanged after editing %s", name)
			}
			// The same edit made to the file is rejected by apply.
			cp.PlanID = d.PlanID
			data, _ := json.Marshal(cp)
			p := filepath.Join(t.TempDir(), "p.json")
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", p)
			m := decodeErr(t, out)
			if code != 2 || !strings.Contains(m["error"], "modified or corrupted") {
				t.Errorf("code=%d out=%s", code, out)
			}
		})
	}
	// The untouched plan file still decodes strictly.
	data, _ := os.ReadFile(path)
	if _, err := decodePlan(data); err != nil {
		t.Errorf("plan no longer decodes: %v", err)
	}
}

// ---- provider errors ---------------------------------------------------------------

func perrsFor(t *testing.T, out string, port int) []providerError {
	t.Helper()
	d := decodePlanDoc(t, out)
	for _, e := range d.Endpoints {
		if e.Port == port {
			return e.ProviderErrors
		}
	}
	t.Fatalf("no endpoint %d in %s", port, out)
	return nil
}

func TestProviderErrorCategories(t *testing.T) {
	timeout := fakeResp{err: errWrapperTimeout}
	tests := []struct {
		name     string
		goos     string
		args     []string // extra plan args
		setup    func(w *wfWorld)
		provider string
		category string
		contains string
		onPort   int
	}{
		{"docker unavailable", "linux", nil, func(w *wfWorld) { w.dockerFail = &fakeResp{err: dockerDownResp.err} }, "docker", "unavailable", "Cannot connect to the Docker daemon", 3000},
		{"docker timeout", "linux", nil, func(w *wfWorld) { w.dockerFail = &timeout }, "docker", "timeout", "timed out", 3000},
		{"docker permission", "linux", nil, func(w *wfWorld) {
			w.dockerFail = &fakeResp{err: fail(1, "permission denied while trying to connect to the Docker daemon socket").err}
		}, "docker", "permission", "permission denied", 3000},
		{"docker failed", "linux", nil, func(w *wfWorld) { w.dockerFail = &fakeResp{err: fail(1, "boom").err} }, "docker", "failed", "boom", 3000},
		{"lsof failed", "linux", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdLsof, fail(2, "lsof: boom")) }, "lsof", "failed", "boom", 3000},
		{"lsof timeout", "darwin", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdLsof, timeout) }, "lsof", "timeout", "timed out", 3000},
		{"lsof permission", "darwin", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdLsof, fail(2, "lsof: Operation not permitted")) }, "lsof", "permission", "not permitted", 3000},
		{"lsof unavailable", "linux", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdLsof, fail(127, "")) }, "lsof", "unavailable", "", 3000},
		{"lsof udp parse", "linux", []string{"--protocol", "udp"}, func(w *wfWorld) {
			w.publish()
			w.f.setResp(cmdLsofUDP, ok("this is not lsof field output\n"))
		}, "lsof", "parse", "no process records", 3000},
		{"proc unavailable", "linux", nil, func(w *wfWorld) { w.publish(); delete(w.f.files, "/proc/net/tcp") }, "proc", "unavailable", "does not exist", 3000},
		{"proc parse", "linux", nil, func(w *wfWorld) { w.publish(); w.f.setFile("/proc/net/tcp", "this is not a socket table\n") }, "proc", "parse", "none could be parsed", 3000},
		{"netstat darwin failed", "darwin", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdNetstatMacTCP, fail(1, "netstat: boom")) }, "netstat", "failed", "boom", 3000},
		{"netstat windows permission", "windows", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdNetstat, fail(1, "Access is denied.")) }, "netstat", "permission", "Access is denied", 3000},
		{"tasklist failed", "windows", nil, func(w *wfWorld) { w.publish(); w.f.setResp(cmdTasklist, fail(1, "tasklist: boom")) }, "tasklist", "failed", "boom", 3000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docker := strings.HasPrefix(tc.name, "docker")
			w := newWfWorld(tc.goos, docker)
			name := "node"
			if tc.goos == "windows" {
				name = "node.exe"
			}
			w.listen(tcOrUDP(tc.args), 3000, 100, name)
			tc.setup(w)
			if docker {
				w.publish()
			}
			args := append([]string{"plan", "--json"}, tc.args...)
			_, out, _ := wfRun(t, w.f, w.f, "", append(args, "3000")...)
			var found *providerError
			for _, pe := range perrsFor(t, out, tc.onPort) {
				pe := pe
				if pe.Provider == tc.provider {
					found = &pe
				}
			}
			if found == nil {
				t.Fatalf("no %s provider error:\n%s", tc.provider, out)
			}
			if found.Category != tc.category {
				t.Errorf("category = %q, want %q (%+v)", found.Category, tc.category, found)
			}
			if !strings.Contains(found.Message, tc.contains) {
				t.Errorf("message %q lacks %q", found.Message, tc.contains)
			}
			if len(found.Message) > 200 || strings.Contains(found.Message, "/var/") || strings.Contains(found.Message, "/proc/") {
				t.Errorf("message not sanitised: %q", found.Message)
			}
		})
	}
}

func tcOrUDP(args []string) string {
	for i, a := range args {
		if a == "--protocol" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return protoTCP
}

func TestProviderErrorsAlsoCoverWarningOnlyFailures(t *testing.T) {
	// lsof fails but ss succeeds: today that is only a warning (visibility
	// stays complete); it must be in provider_errors as well.
	f := newFake("linux", "lsof", "ss")
	f.setFile("/proc/net/tcp", procTCPHeader+procRow("00000000:0BB8", "00000000:0000", "0A"))
	f.on(cmdLsof, fail(2, "lsof: can't read /Users/alice/secret"))
	f.on(cmdSS, ok(ssHeader+"LISTEN 0 128 *:3000 *:* users:((\"node\",pid=100,fd=3))\n"))
	f.setProc(100, "s100", "node")
	code, out, errOut := wfRun(t, f, f, "", "plan", "--json", "3000")
	d := decodePlanDoc(t, out)
	if code != 0 || d.Endpoints[0].Visibility != visComplete || len(d.Resources) != 1 {
		t.Fatalf("code=%d plan=%+v stderr=%s", code, d, errOut)
	}
	pes := d.Endpoints[0].ProviderErrors
	if len(pes) != 1 || pes[0].Provider != "lsof" || pes[0].Category != "failed" {
		t.Fatalf("provider_errors = %+v", pes)
	}
	if strings.Contains(out, "alice") || strings.Contains(out, "/Users") {
		t.Errorf("path leaked:\n%s", out)
	}
	if !strings.Contains(errOut, "owners come from ss") {
		t.Errorf("the warning must remain on stderr: %s", errOut)
	}

	// ss only, and failing.
	g := newFake("linux", "ss")
	g.setFile("/proc/net/tcp", procTCPHeader+procRow("00000000:0BB8", "00000000:0000", "0A"))
	g.on(cmdSS, fail(1, "ss: boom"))
	code, out, _ = wfRun(t, g, g, "", "plan", "--json", "3000")
	pes = perrsFor(t, out, 3000)
	if code == 0 || len(pes) != 1 || pes[0].Provider != "ss" || pes[0].Category != "failed" {
		t.Errorf("code=%d provider_errors = %+v", code, pes)
	}
}

func TestProviderErrorsIdentityPermissionAppearsOnTheEndpoint(t *testing.T) {
	w := newWfWorld("linux", false).listen("tcp", 3000, 100, "node").listen("tcp", 3001, 101, "python3")
	w.publish()
	w.f.identErrs[100] = fmt.Errorf("open /proc/100/stat: %w", os.ErrPermission)
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000", "3001")
	d := decodePlanDoc(t, out)
	if code != 1 || len(d.Refused) != 1 || !strings.Contains(d.Refused[0].Reason, "cannot bind process identity") {
		t.Fatalf("code=%d plan=%+v", code, d)
	}
	if strings.Contains(d.Refused[0].Reason, "/proc/100") {
		t.Errorf("refusal reason leaks a path: %q", d.Refused[0].Reason)
	}
	pes := perrsFor(t, out, 3000)
	if len(pes) != 1 || pes[0].Provider != "proc" || pes[0].Category != "permission" || !strings.Contains(pes[0].Message, "pid 100") {
		t.Errorf("3000 provider_errors = %+v", pes)
	}
	if other := perrsFor(t, out, 3001); len(other) != 0 {
		t.Errorf("3001 should have none: %+v", other)
	}
	// No failure: the array is present and empty, never null.
	g := newWfWorld("linux", false).listen("tcp", 3000, 100, "node").publish()
	_, out, _ = wfRun(t, g.f, g.f, "", "plan", "--json", "3000")
	if !strings.Contains(out, `"provider_errors": []`) {
		t.Errorf("expected an empty provider_errors array:\n%s", out)
	}
}

func TestProviderErrorsInInspectAndApply(t *testing.T) {
	id := hex64(0x99)
	w, _ := modeWorld("container only", id)
	path, d := planFile(t, w.f, w.f, "3000")
	if len(d.Resources) != 1 {
		t.Fatalf("plan = %+v", d)
	}
	// Docker goes down between plan and apply.
	w.dockerFail = &fakeResp{err: dockerDownResp.err}
	w.publish()
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	ad := decodeApplyDoc(t, out)
	if code == 0 || len(ad.Endpoints) != 1 {
		t.Fatalf("code=%d doc=%+v", code, ad)
	}
	pes := ad.Endpoints[0].ProviderErrors
	if len(pes) != 1 || pes[0].Provider != "docker" || pes[0].Category != "unavailable" || strings.Contains(pes[0].Message, "/var/run") {
		t.Errorf("apply provider_errors = %+v", pes)
	}
	if !obsIDRe.MatchString(ad.ObservationID) {
		t.Errorf("apply observation_id = %q", ad.ObservationID)
	}
	assertNoMutation(t, w.f)

	// inspect shows the same entry and a human rendering mentions it.
	_, out, _ = wfRun(t, w.f, w.f, "", "inspect", "--json", "3000")
	var id2 inspectDoc
	decodeDoc(t, out, &id2)
	if got := id2.Endpoints[0].ProviderErrors; len(got) != 1 || got[0] != pes[0] {
		t.Errorf("inspect provider_errors = %+v", got)
	}
	_, text, _ := wfRun(t, w.f, w.f, "", "inspect", "3000")
	if !strings.Contains(text, "docker unavailable") {
		t.Errorf("human inspect omits the provider error:\n%s", text)
	}
	_, text, _ = wfRun(t, w.f, w.f, "", "plan", "3000")
	if !strings.Contains(text, "Provider errors (1):") {
		t.Errorf("human plan omits the provider error:\n%s", text)
	}

	// An expired plan observed nothing: no observation metadata.
	w2, _ := modeWorld("native only", id)
	p2, _ := planFile(t, w2.f, w2.f, "3000", "--ttl", "1m")
	w2.f.advance(2 * time.Minute)
	_, out, _ = wfRun(t, w2.f, w2.f, "", "apply", "--json", p2)
	if ad2 := decodeApplyDoc(t, out); ad2.ObservationID != "" || ad2.Error != "plan expired" {
		t.Errorf("expired apply = %+v", ad2)
	}
}

func TestSanitizeProviderText(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		in, want string
	}{
		{"open /proc/net/tcp: no such file or directory", "open <path> no such file or directory"},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?", "Cannot connect to the Docker daemon at unix:<path> Is the docker daemon running?"},
		{"error during connect: Get tcp://build-host.example:2375/v1.4/containers/json: dial tcp", "error during connect: Get tcp://<host> dial tcp"},
		{`lsof: can't stat "/Users/alice/work/app": Permission denied`, `lsof: can't stat "<path>": Permission denied`},
		{`CreateFile C:\Users\Bob\AppData\x.log: Access is denied.`, `CreateFile <path> Access is denied.`},
		{`copy \\fileserver\share\x failed`, `copy <path> failed`},
		{"path=/etc/passwd and (/tmp/x)", "path=<path> and (<path>)"},
		{"12/tcp and tcp/udp are not paths, nor is a lone / slash", "12/tcp and tcp/udp are not paths, nor is a lone / slash"},
		{"line one\nline two\t\tend", "line one line two end"},
		{"", ""},
		// Review finding M4: paths with spaces, quoted paths, delimiters before
		// the slash, home-directory forms, addresses and host names.
		{`CreateFile C:\Users\John Smith\AppData\x.log: Access is denied.`, `CreateFile <path> Access is denied.`},
		{`C:\Users\John Smith\AppData\x.log`, `<path>`},
		{`copy C:\Program Files (x86)\Docker\bin\docker.exe failed`, `copy <path> failed`},
		{`cannot open /Users/John Smith/Library/x: no access`, `cannot open <path> no access`},
		{`/Users/John Smith/Library/x`, `<path>`},
		{`exec "C:\Program Files\Docker\Docker\resources\bin\docker.exe" not found`, `exec "<path>" not found`},
		{`can't open '/home/alice/my file.txt' now`, `can't open '<path>' now`},
		{"read `/home/alice/x y/z`!", "read `<path>`!"},
		{`unterminated "/home/alice/x`, `unterminated "<path>`},
		{`copy \\fileserver\My Share\x y failed`, `copy <path> y failed`},
		{`error:/home/alice/x`, `error:<path>`},
		{`path:/home/alice/x`, `path:<path>`},
		{`config=/etc/docker/daemon.json`, `config=<path>`},
		{`open ~/.docker/config.json: permission denied`, `open <path> permission denied`},
		{`open ~alice/.docker/config.json`, `open <path>`},
		{`HOME=$HOME/.docker failed`, `HOME=<path> failed`},
		{`cannot read ${HOME}/.docker/config.json`, `cannot read <path>`},
		{`see %USERPROFILE%\.docker\config.json for details`, `see <path> for details`},
		{`dir is %APPDATA% today`, `dir is <path> today`},
		{`dial tcp 10.1.2.3:2375: connect: connection refused`, `dial tcp <ip>:2375: connect: connection refused`},
		{`listen on 0.0.0.0:3000 and 127.0.0.1:3001`, `listen on <ip>:3000 and <ip>:3001`},
		{`dial tcp [fe80::1]:2375: i/o timeout`, `dial tcp [<ip>]:2375: i/o timeout`},
		{`dial tcp [::1]:2375 and 2001:db8:85a3::8a2e:370:7334`, `dial tcp [<ip>]:2375 and <ip>`},
		{`lookup devbox-alice.local: no such host`, `lookup <host>: no such host`},
		{`lookup nas.lan and build.internal and pc.home`, `lookup <host> and <host> and <host>`},
		{`connect alice@laptop-7: refused`, `connect <user@host> refused`},
		{`port 3000 is busy at 12:05:00 (10:30/45) via [::]:8080->80/tcp`, `port 3000 is busy at 12:05:00 (10:30/45) via [::]:8080->80/tcp`},
		{"caf\xe9d", "caf\uFFFDd"},
		{"escape \x1b[31mred\x07", "escape ?[31mred?"},
	}
	for _, tc := range tests {
		if got := sanitizeProviderText(tc.in); got != tc.want {
			t.Errorf("sanitize(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
	got := sanitizeProviderText(long)
	if len(got) != 200 || !strings.HasSuffix(got, "...") {
		t.Errorf("long message: len %d %q", len(got), got[len(got)-5:])
	}
	// Multi-byte text is cut on a rune boundary.
	if got := sanitizeProviderText(strings.Repeat("é", 300)); len([]rune(got)) != 200 {
		t.Errorf("runes = %d", len([]rune(got)))
	}
	// Path scrubbing happens before truncation: no half path survives.
	if got := sanitizeProviderText(strings.Repeat("a ", 98) + "/very/long/private/path/that/gets/cut"); strings.Contains(got, "/very") || strings.Contains(got, "private") {
		t.Errorf("path survived: %q", got)
	}
}

func TestProviderErrorMessagesInDocumentsAreSortedAndDeduplicated(t *testing.T) {
	// A failing lsof with ss also failing yields both, sorted by provider.
	f := newFake("linux", "lsof", "ss")
	f.setFile("/proc/net/tcp", procTCPHeader)
	f.on(cmdLsof, fail(2, "lsof: boom"))
	f.on(cmdSS, fail(1, "ss: boom"))
	_, out, _ := wfRun(t, f, f, "", "plan", "--json", "3000")
	pes := perrsFor(t, out, 3000)
	var got []string
	for _, pe := range pes {
		got = append(got, pe.Provider+"/"+pe.Category)
	}
	if !sort.StringsAreSorted(got) || !reflect.DeepEqual(got, []string{"lsof/failed", "ss/failed"}) {
		t.Errorf("provider errors = %v", got)
	}
}
