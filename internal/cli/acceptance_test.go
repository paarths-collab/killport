package cli

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The five acceptance scenarios of the structured workflow. Every test runs
// through run()/runIO() with the fake system and asserts both the JSON
// document and the process exit code.

func pidsOf(rs []planResource) []int {
	var pids []int
	for _, r := range rs {
		if r.Provider == providerNative {
			pids = append(pids, r.PID)
		}
	}
	return pids
}

// --- 1. remote ports and UDP -----------------------------------------------

func TestAcceptance1_RemotePortClientSurvivesPlanningAndUDPNeedsProtocolFlag(t *testing.T) {
	for _, goos := range allOS {
		t.Run(goos, func(t *testing.T) {
			w := newWfWorld(goos, false)
			w.listen("tcp", 3000, 100, "node")
			w.connect("tcp", 52000, 3000, 300, "curl")  // ESTABLISHED client: REMOTE port 3000
			w.listen("udp", 3000, 200, "dnsmasq")       // UDP bound to local 3000
			w.connect("udp", 5000, 3000, 400, "chrome") // connected UDP: REMOTE port 3000
			w.listen("tcp", 13000, 600, "lookalike")
			w.publish()

			// Default protocol: only the TCP listener.
			code, out, errOut := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
			if code != 0 {
				t.Fatalf("tcp plan exit %d stderr=%s out=%s", code, errOut, out)
			}
			d := decodePlanDoc(t, out)
			if got := pidsOf(d.Resources); !reflect.DeepEqual(got, []int{100}) {
				t.Fatalf("tcp plan selects %v, want only the listener 100 (client 300, UDP 200/400 must survive)", got)
			}
			if d.Request.Protocol != "tcp" || d.Request.Scope != "listener" || d.ExitClass != "ok" || d.ActionsExecuted != 0 {
				t.Errorf("request/exit = %+v %s", d.Request, d.ExitClass)
			}
			if e := d.Endpoints[0]; e.Protocol != "tcp" || e.Visibility != visComplete || !e.Occupied || !reflect.DeepEqual(e.Resources, []string{d.Resources[0].ResourceID}) {
				t.Errorf("endpoint = %+v", e)
			}

			// --protocol udp: the bound UDP socket, never the connected one
			// whose REMOTE port is 3000, never the TCP listener or client.
			code, out, errOut = wfRun(t, w.f, w.f, "", "plan", "--json", "--protocol", "udp", "3000")
			if code != 0 {
				t.Fatalf("udp plan exit %d stderr=%s out=%s", code, errOut, out)
			}
			d = decodePlanDoc(t, out)
			if got := pidsOf(d.Resources); !reflect.DeepEqual(got, []int{200}) {
				t.Fatalf("udp plan selects %v, want only 200", got)
			}
			if d.Request.Protocol != "udp" || d.Request.Scope != "bound" || d.Endpoints[0].Protocol != "udp" {
				t.Errorf("udp request = %+v endpoint=%+v", d.Request, d.Endpoints[0])
			}

			// The remote-port-only world: nothing is selected, the endpoint is free.
			c := newWfWorld(goos, false)
			c.connect("tcp", 52000, 3000, 300, "curl").connect("udp", 5000, 3000, 400, "chrome").publish()
			for _, args := range [][]string{{"plan", "--json", "3000"}, {"plan", "--json", "--protocol", "udp", "3000"}} {
				code, out, _ = wfRun(t, c.f, c.f, "", args...)
				d = decodePlanDoc(t, out)
				if code != 0 || len(d.Resources) != 0 || d.Endpoints[0].Occupied || d.Endpoints[0].Visibility != visComplete {
					t.Errorf("%v: code=%d plan=%+v", args, code, d)
				}
			}
			assertNoMutation(t, w.f)
			assertNoMutation(t, c.f)
		})
	}
}

// --- 2. plan and --dry-run never mutate ---------------------------------------

func TestAcceptance2_PlanAndDryRunInvokeZeroMutatingCalls(t *testing.T) {
	id := hex64(0xfeed)
	build := func() *wfWorld {
		w := newWfWorld("linux", true)
		w.listen("tcp", 3000, 100, "node").listen("tcp", 3001, 101, "python3.12")
		w.container(id, "web", 900, 3002, 3003)
		return w.publish()
	}

	t.Run("plan JSON", func(t *testing.T) {
		w := build()
		// Run under readOnlySystem as well: any mutation would panic.
		code, out, errOut := wfRun(t, readOnlySystem{system: w.f}, w.f, "", "plan", "--json", "3000-3003")
		if code != 0 {
			t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
		}
		d := decodePlanDoc(t, out)
		if d.ActionsExecuted != 0 {
			t.Errorf("actions_executed = %d", d.ActionsExecuted)
		}
		if len(d.Resources) != 3 {
			t.Errorf("resources = %+v", d.Resources)
		}
		assertNoMutation(t, w.f)
		// Plain `plan` (which wraps readOnlySystem itself) too.
		wfRun(t, w.f, w.f, "", "plan", "-o", t.TempDir()+"/p.json", "3000-3003")
		assertNoMutation(t, w.f)
	})

	t.Run("human text equals JSON", func(t *testing.T) {
		w := build()
		_, jsonOut, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000-3003")
		d := decodePlanDoc(t, jsonOut)
		code, text, _ := wfRun(t, w.f, w.f, "", "plan", "3000-3003")
		if code != 0 {
			t.Fatalf("human plan exit %d:\n%s", code, text)
		}

		// Expected lines are derived from the DECODED JSON, with the format
		// spelled out here independently of the renderer.
		var want []string
		for _, r := range d.Resources {
			ports := make([]string, len(r.Ports))
			for i, p := range r.Ports {
				ports[i] = fmt.Sprint(p)
			}
			if r.Provider == "docker" {
				want = append(want, fmt.Sprintf("  %s  docker stop -t %d  %s  ports %s", r.ResourceID, r.GraceSeconds, r.Name, strings.Join(ports, ",")))
			} else {
				want = append(want, fmt.Sprintf("  %s  %s  pid %d (%s)  ports %s", r.ResourceID, r.Signal, r.PID, r.Name, strings.Join(ports, ",")))
			}
		}
		// The "Resources" section of the text: indented lines under its header.
		var got []string
		inSection := false
		for _, line := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(line, "Resources ("):
				inSection = true
				if want := fmt.Sprintf("Resources (%d):", len(d.Resources)); line != want {
					t.Errorf("header %q, want %q", line, want)
				}
			case !strings.HasPrefix(line, "  "):
				inSection = false
			case inSection:
				got = append(got, line)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("human resources differ from JSON\n got: %q\nwant: %q\n\n%s", got, want, text)
		}
		// Same ports, same signals/actions, no extra resource ids anywhere.
		for _, r := range d.Resources {
			if strings.Count(text, r.ResourceID) < 1 {
				t.Errorf("%s missing from text", r.ResourceID)
			}
		}
		if n := strings.Count(text, "proc:") + strings.Count(text, "container:"); n < len(d.Resources) {
			t.Errorf("text mentions %d resources, json has %d", n, len(d.Resources))
		}
		if !strings.Contains(text, "Nothing was changed (actions_executed: 0)") || !strings.Contains(text, d.PlanID) {
			t.Errorf("text lacks the no-change statement / plan id:\n%s", text)
		}
		assertNoMutation(t, w.f)
	})

	t.Run("legacy dry-run", func(t *testing.T) {
		w := build()
		w.f.on(cmdDockerPS, ok(id+"|web|0.0.0.0:3002->80/tcp, 0.0.0.0:3003->80/tcp\n"))
		code, out, errOut := wfRun(t, readOnlySystem{system: w.f}, w.f, "", "--dry-run", "--json", "3000", "3001", "3002", "3003")
		if code != 0 {
			t.Fatalf("exit %d stderr=%s out=%s", code, errOut, out)
		}
		rows := decodeKill(t, out)
		if len(rows) != 4 {
			t.Fatalf("rows = %+v", rows)
		}
		for _, r := range rows {
			if r.Status != statusPlanned || r.Killed || !r.DryRun {
				t.Errorf("row %+v", r)
			}
		}
		assertNoMutation(t, w.f)
	})
}

// --- 3. one container, two ports; failed docker ps with a native forwarder ------

func TestAcceptance3_OneContainerTwoPortsOneStopAndFailedDockerPSRefusesForwarder(t *testing.T) {
	id := hex64(0xa11ce)

	t.Run("one resource, one docker stop on the full id, two endpoint results", func(t *testing.T) {
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000, 3001) // docker-proxy PID 900 listens on both
		w.publish()
		path, d := planFile(t, w.f, w.f, "3000", "3001")
		if len(d.Resources) != 1 || d.Resources[0].Provider != "docker" || d.Resources[0].ContainerID != id || len(d.Resources[0].ContainerID) != 64 {
			t.Fatalf("resources = %+v", d.Resources)
		}
		if r := d.Resources[0]; !reflect.DeepEqual(r.Ports, []int{3000, 3001}) || r.Action != "docker_stop" || r.ResourceID != "container:"+id {
			t.Errorf("resource = %+v", r)
		}
		if len(d.Endpoints) != 2 || len(d.Refused) != 0 {
			t.Fatalf("endpoints=%+v refused=%+v", d.Endpoints, d.Refused)
		}
		for _, e := range d.Endpoints {
			if !reflect.DeepEqual(e.Resources, []string{"container:" + id}) {
				t.Errorf("endpoint %d -> %v", e.Port, e.Resources)
			}
		}

		w.f.setResp(stopCmd(id), ok(""))
		h := &hookFake{fakeSystem: w.f}
		h.afterRun = func(key string, err error) {
			if key == stopCmd(id) {
				w.removeContainer(id)
				w.publish()
			}
		}
		code, out, errOut := wfRun(t, h, w.f, "", "apply", "--json", path)
		if code != 0 {
			t.Fatalf("apply exit %d stderr=%s out=%s", code, errOut, out)
		}
		ad := decodeApplyDoc(t, out)
		if n := w.f.runCount("docker stop"); n != 1 {
			t.Errorf("docker stop ran %d times: %v", n, w.f.runs)
		}
		if w.f.runCount(stopCmd(id)) != 1 {
			t.Errorf("the stop must target the 64-hex id exactly: %v", w.f.runs)
		}
		if len(ad.Resources) != 1 || ad.Resources[0].Result != resStopped || !ad.Resources[0].VerifiedGone || ad.ActionsExecuted != 1 {
			t.Errorf("resources = %+v", ad.Resources)
		}
		if len(ad.Endpoints) != 2 || ad.Endpoints[0].Status != epFree || ad.Endpoints[1].Status != epFree || ad.Endpoints[0].Port != 3000 || ad.Endpoints[1].Port != 3001 {
			t.Errorf("endpoints = %+v", ad.Endpoints)
		}
		if ad.ExitClass != "ok" {
			t.Errorf("exit_class = %s", ad.ExitClass)
		}
		if w.f.signalCount() != 0 || w.f.killCount() != 0 {
			t.Errorf("the docker-proxy must not be signalled: %v %v", w.f.signals, w.f.kills)
		}
		// The docker command is bounded by grace + 20s (default grace 10s).
		w.f.mu.Lock()
		left := w.f.deadlines[stopCmd(id)]
		w.f.mu.Unlock()
		if left < 25*time.Second || left > 30*time.Second {
			t.Errorf("docker stop wrapper timeout has %v left, want ~30s (grace 10s + 20s)", left)
		}
	})

	for _, tc := range []struct{ goos, name string }{
		{"linux", "docker-proxy"},
		{"darwin", "com.docker.backend"},
		{"windows", "com.docker.backend.exe"},
	} {
		t.Run("failed docker ps refuses the forwarder on "+tc.goos, func(t *testing.T) {
			w := newWfWorld(tc.goos, true)
			w.listen("tcp", 3000, 800, tc.name)
			w.dockerFail = &fakeResp{err: fail(1, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock").err}
			w.publish()

			path, d := planFile(t, w.f, w.f, "3000")
			code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
			if code != 1 {
				t.Fatalf("plan exit = %d, want 1\n%s", code, out)
			}
			d = decodePlanDoc(t, out)
			if len(d.Resources) != 0 || len(d.Refused) != 1 {
				t.Fatalf("plan = %+v", d)
			}
			rf := d.Refused[0]
			if rf.PID != 800 || rf.Port != 3000 || !strings.Contains(rf.Reason, "Docker infrastructure") || !strings.Contains(rf.Reason, "docker ps failed") || !strings.Contains(rf.Reason, "Cannot connect to the Docker daemon") {
				t.Errorf("refusal = %+v", rf)
			}
			if d.ExitClass != "failed" || d.Endpoints[0].Visibility != visProviderUnavailable {
				t.Errorf("class=%s visibility=%s", d.ExitClass, d.Endpoints[0].Visibility)
			}

			// Applying that plan must not touch the forwarder either.
			code, _, _ = wfRun(t, w.f, w.f, "", "apply", "--json", path)
			if code == 0 {
				t.Errorf("apply of a plan with nothing to do against an unverifiable endpoint must not claim ok")
			}
			assertNoMutation(t, w.f)
		})
	}
}

// --- 4. plan to apply drift ---------------------------------------------------------

func TestAcceptance4_DriftBetweenPlanAndApply(t *testing.T) {
	t.Run("a: PID exits and is reused", func(t *testing.T) {
		for _, listens := range []bool{false, true} {
			t.Run(fmt.Sprintf("reused process listens=%v", listens), func(t *testing.T) {
				w := newWfWorld("linux", false)
				w.listen("tcp", 3000, 100, "node")
				w.publish()
				path, d := planFile(t, w.f, w.f, "3000")
				if len(d.Resources) != 1 {
					t.Fatalf("plan = %+v", d)
				}

				w.removePID(100)
				if listens {
					w.listen("tcp", 3000, 100, "reused")
				}
				w.f.replaceProc(100, "a-new-start-time")
				w.publish()

				code, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
				ad := decodeApplyDoc(t, out)
				if code != 3 || ad.ExitClass != "needs_fresh_plan" {
					t.Fatalf("exit %d class %s stderr=%s\n%s", code, ad.ExitClass, errOut, out)
				}
				if r := ad.Resources[0]; r.Result != resIdentityChanged || r.VerifiedGone != true {
					t.Errorf("resource = %+v", r)
				}
				if n := w.f.signalCount(); n != 0 {
					t.Errorf("%d signal(s) reached the reused PID: %v", n, w.f.signals)
				}
				assertNoMutation(t, w.f)
				if ad.ActionsExecuted != 0 {
					t.Errorf("actions_executed = %d", ad.ActionsExecuted)
				}
				want := epFree
				if listens {
					want = epBlockedNewOwner
				}
				if got := endpointStatus(ad, 3000); got != want {
					t.Errorf("endpoint = %s, want %s", got, want)
				}
			})
		}
	})

	t.Run("b: container replaced under the same name", func(t *testing.T) {
		oldID, newID := hex64(0x01d), hex64(0x0ee)
		w := newWfWorld("linux", true)
		w.container(oldID, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")

		w.removeContainer(oldID)
		w.container(newID, "web", 901, 3000)
		w.publish()

		code, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 3 || ad.ExitClass != "needs_fresh_plan" {
			t.Fatalf("exit %d class %s stderr=%s\n%s", code, ad.ExitClass, errOut, out)
		}
		if r := ad.Resources[0]; r.Result != resNotAttempted || r.ContainerID != oldID || !r.VerifiedGone {
			t.Errorf("resource = %+v", r)
		}
		if n := w.f.runCount("docker stop"); n != 0 {
			t.Errorf("docker stop ran %d time(s): %v", n, w.f.runs)
		}
		assertNoMutation(t, w.f)
		if w.f.sleeps != 0 {
			t.Errorf("nothing was attempted, so there is nothing to wait for; slept %d times", w.f.sleeps)
		}
		if e := ad.Endpoints[0]; e.Status != epBlockedNewOwner || len(e.Holders) == 0 {
			t.Errorf("endpoint = %+v", e)
		} else {
			found := false
			for _, h := range e.Holders {
				found = found || h.ContainerID == newID
			}
			if !found {
				t.Errorf("the replacement container should be reported as the holder: %+v", e.Holders)
			}
		}
	})

	t.Run("c: re-applying the same plan after success touches nothing", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.onSignal = func(f *fakeSystem, s fakeSignal) { w.removePID(s.PID); w.publish() }

		code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		first := decodeApplyDoc(t, out)
		if code != 0 || first.Resources[0].Result != resSignalSent || first.Endpoints[0].Status != epFree {
			t.Fatalf("first apply: code=%d doc=%+v", code, first)
		}
		if n := w.f.signalCount(); n != 1 {
			t.Fatalf("signals after first apply = %d", n)
		}

		// A different process now listens on the port (new PID).
		w.listen("tcp", 3000, 777, "newcomer").publish()
		code, out, _ = wfRun(t, w.f, w.f, "", "apply", "--json", path)
		second := decodeApplyDoc(t, out)
		if n := w.f.signalCount(); n != 1 {
			t.Errorf("re-apply signalled again: %v", w.f.signals)
		}
		if r := second.Resources[0]; r.Result != resNotAttempted || !r.VerifiedGone || second.ActionsExecuted != 0 {
			t.Errorf("second resource = %+v", r)
		}
		if code != 3 || second.Endpoints[0].Status != epBlockedNewOwner {
			t.Errorf("newcomer must be reported, never acted on: code=%d %+v", code, second.Endpoints)
		}

		// And when nothing replaced it, the re-apply is a clean no-op.
		w.removePID(777)
		w.publish()
		code, out, _ = wfRun(t, w.f, w.f, "", "apply", "--json", path)
		third := decodeApplyDoc(t, out)
		if code != 0 || third.Resources[0].Result != resNotAttempted || third.Endpoints[0].Status != epFree || w.f.signalCount() != 1 {
			t.Errorf("no-op re-apply: code=%d doc=%+v signals=%v", code, third, w.f.signals)
		}
	})
}

// --- 5. outcomes: timeout, new owner, original still running -------------------------

func TestAcceptance5_OutcomeObservation(t *testing.T) {
	t.Run("a: docker stop times out and the container is still listed", func(t *testing.T) {
		id := hex64(0x7131)
		w := newWfWorld("linux", true)
		w.container(id, "web", 900, 3000)
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000", "--grace", "2s")
		stop := "docker stop -t 2 " + id
		w.f.setResp(stop, fakeResp{err: errWrapperTimeout})

		code, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 4 || ad.ExitClass != "uncertain" {
			t.Fatalf("exit %d class %s stderr=%s\n%s", code, ad.ExitClass, errOut, out)
		}
		r := ad.Resources[0]
		if r.Result != resTimeoutUnknown || r.VerifiedGone || r.Error == "" {
			t.Errorf("resource = %+v", r)
		}
		if ad.Endpoints[0].Status != epNotVerifiable {
			t.Errorf("endpoint = %+v", ad.Endpoints[0])
		}
		if w.f.runCount(stop) != 1 {
			t.Errorf("stop must run once: %v", w.f.runs)
		}
		// 1 pre-action query + 1 re-query after the timeout + polling.
		if n := w.f.runCount("docker ps"); n < 3 {
			t.Errorf("docker ps ran %d times, want the re-query after the timeout and the outcome polls", n)
		}
	})

	t.Run("b: a supervisor rebinds the port with a new PID after SIGTERM", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000")
		w.f.onSignal = func(f *fakeSystem, s fakeSignal) {
			w.removePID(s.PID)
			w.listen("tcp", 3000, 101, "node") // the supervisor's respawn
			w.publish()
		}
		code, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 3 || ad.ExitClass != "needs_fresh_plan" {
			t.Fatalf("exit %d class %s stderr=%s\n%s", code, ad.ExitClass, errOut, out)
		}
		if r := ad.Resources[0]; r.Result != resSignalSent || !r.VerifiedGone {
			t.Errorf("the original is gone: %+v", r)
		}
		e := ad.Endpoints[0]
		if e.Status != epBlockedNewOwner || len(e.Holders) != 1 || e.Holders[0].PID != 101 {
			t.Errorf("endpoint = %+v", e)
		}
		if len(w.f.signals) != 1 || w.f.signals[0].PID != 100 {
			t.Errorf("only the planned process may be signalled (never the respawn): %+v", w.f.signals)
		}
	})

	t.Run("c: signal accepted but the process keeps running", func(t *testing.T) {
		w := newWfWorld("linux", false)
		w.listen("tcp", 3000, 100, "node")
		w.publish()
		path, _ := planFile(t, w.f, w.f, "3000", "--grace", "1s")
		code, out, errOut := wfRun(t, w.f, w.f, "", "apply", "--json", path)
		ad := decodeApplyDoc(t, out)
		if code != 1 || ad.ExitClass != "failed" {
			t.Fatalf("exit %d class %s stderr=%s\n%s", code, ad.ExitClass, errOut, out)
		}
		r := ad.Resources[0]
		if r.Result != resSignalSent || r.VerifiedGone || r.Forced {
			t.Errorf("resource = %+v", r)
		}
		if got := endpointStatus(ad, 3000); got != epBlockedOriginal {
			t.Errorf("signal dispatch alone must never mean free; endpoint = %s", got)
		}
		if len(w.f.signals) != 1 || w.f.signals[0].Force {
			t.Errorf("exactly one SIGTERM expected, got %+v (no automatic escalation)", w.f.signals)
		}
		// The outcome poll really waited out the grace period (1s / 200ms).
		if w.f.sleeps < 4 {
			t.Errorf("polled %d times", w.f.sleeps)
		}
	})
}
