package cli

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyClosedListenerSurvivesDockerFailure(t *testing.T) {
	w := newWfWorld("linux", true).listen("tcp", 3000, 100, "node").publish()
	path, _ := planFile(t, w.f, w.f, "3000", "--grace", "0s")
	w.closePort(100, 3000)
	down := fail(1, "cannot connect to the Docker daemon")
	w.dockerFail = &down
	w.publish()
	_, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	doc := decodeApplyDoc(t, out)
	if w.f.signalCount() != 0 || doc.ActionsExecuted != 0 || doc.Resources[0].Result != resNotAttempted {
		t.Fatalf("a live process that closed its listener was targeted: %+v", doc)
	}
}

func TestApplyRechecksQueuedProcessOwnership(t *testing.T) {
	w := newWfWorld("linux", false)
	var ports []string
	for i := 0; i < 5; i++ {
		w.listen("tcp", 3000+i, 100+i, "node")
		ports = append(ports, fmt.Sprint(3000+i))
	}
	w.publish()
	path, _ := planFile(t, w.f, w.f, append(ports, "--grace", "0s")...)
	var once sync.Once
	w.f.onSignal = func(_ *fakeSystem, s fakeSignal) {
		once.Do(func() {
			// An earlier action makes the fifth queued action obsolete, but
			// its process remains alive with the same identity.
			w.closePort(104, 3004)
			w.publish()
		})
		w.removePID(s.PID)
		w.publish()
	}
	code, out, _ := wfRun(t, w.f, w.f, "", "apply", "--json", path)
	doc := decodeApplyDoc(t, out)
	if got := resultOf(doc, procResourceID(104, wfStart(104))); got.Result != resNotAttempted {
		t.Fatalf("queued process was targeted after closing its listener: %+v", got)
	}
	if code != 0 || doc.ActionsExecuted != 4 || w.f.signalCount() != 4 {
		t.Fatalf("exit=%d doc=%+v", code, doc)
	}
}

func TestPlanRejectsGraceSecondsBeforeDurationOverflow(t *testing.T) {
	// Converting this many seconds to time.Duration wraps negative on 64-bit
	// platforms. Validate the seconds before performing that multiplication.
	overflow := int(math.MaxInt64/int64(time.Second) + 1)
	for _, field := range []string{"request", "resource"} {
		t.Run(field, func(t *testing.T) {
			w := newWfWorld("linux", true).container(hex64(1), "web", 900, 3000).publish()
			_, doc := planFile(t, w.f, w.f, "3000")
			if field == "request" {
				doc.Request.GraceSeconds = overflow
			} else {
				doc.Resources[0].GraceSeconds = overflow
			}
			if err := validatePlan(&doc); err == nil {
				t.Fatal("accepted an overflowing grace period")
			}
		})
	}
}

func TestRemoteDockerHostCannotSupplyLocalPortOwners(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "tcp://remote.example.invalid:2375")
	w := newWfWorld("linux", true).container(hex64(1), "remote-service", 0, 3000).publish()
	w.dropProxy(hex64(1))
	w.publish()
	code, out, _ := wfRun(t, w.f, w.f, "", "plan", "--json", "3000")
	doc := decodePlanDoc(t, out)
	if code == 0 || len(doc.Resources) != 0 {
		t.Fatalf("remote container publication was treated as a local owner: %+v", doc)
	}
	for _, cmd := range w.f.runs {
		if strings.HasPrefix(cmd, "docker ps ") {
			t.Fatalf("queried remote daemon: %s", cmd)
		}
	}
}

func TestDockerEndpointSelection(t *testing.T) {
	for _, tc := range []struct {
		name, goos, host, context, endpoint string
		wantOK                              bool
	}{
		{"local default context", "linux", "", "", "unix:///var/run/docker.sock", true},
		{"remote saved context", "linux", "", "remote", "ssh://remote.example.invalid", false},
		{"remote current context", "linux", "", "", "tcp://remote.example.invalid:2376", false},
		{"explicit local host", "linux", "unix:///run/docker.sock", "", "ssh://remote.example.invalid", true},
		{"context overrides remote host", "linux", "ssh://remote.example.invalid", "desktop-linux", "unix:///run/docker.sock", true},
		{"context overrides local host", "linux", "unix:///run/docker.sock", "remote", "ssh://remote.example.invalid", false},
		{"local Windows pipe", "windows", "", "", "npipe:////./pipe/docker_engine", true},
		{"remote Windows pipe", "windows", "", "", "npipe:////remote/pipe/docker_engine", false},
		{"loopback may be a tunnel", "linux", "tcp://127.0.0.1:2375", "", "", false},
		{"empty endpoint", "linux", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", tc.host)
			t.Setenv("DOCKER_CONTEXT", tc.context)
			f := newFake(tc.goos, "docker")
			f.on("docker context inspect --format {{.Endpoints.docker.Host}}", ok(tc.endpoint))
			err := requireLocalDocker(context.Background(), readOnlySystem{system: f})
			if (err == nil) != tc.wantOK {
				t.Fatalf("local endpoint check = %v; want success=%t", err, tc.wantOK)
			}
		})
	}
}

func TestLegacyKillRejectsRemoteDockerOwners(t *testing.T) {
	t.Setenv("DOCKER_HOST", "ssh://remote.example.invalid")
	t.Setenv("DOCKER_CONTEXT", "")
	f := newFake("linux", "lsof", "docker").on(cmdLsof, ok(""))
	f.on(cmdDockerPS, ok(hex64(1)+"|remote-service|0.0.0.0:3000->80/tcp\n"))
	_, _, warnings := invoke(t, f, "3000", "--json")
	if !strings.Contains(warnings, "remote or unsupported") {
		t.Fatalf("missing remote endpoint warning: %s", warnings)
	}
	if f.runCount("docker ps ") != 0 || f.runCount("docker stop ") != 0 {
		t.Fatal("contacted a remote daemon while freeing a local port")
	}
}
