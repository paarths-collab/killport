package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Plan document constants.
const (
	planSchemaVersion = 1
	planKind          = "killport.plan"

	defaultPlanTTL   = 5 * time.Minute
	maxPlanTTL       = 24 * time.Hour
	defaultGrace     = 10 * time.Second
	maxGrace         = time.Hour
	dockerStopMargin = 20 * time.Second // wrapper timeout = grace + margin

	providerNative = "native"
	providerDocker = "docker"

	actionSignal     = "signal"
	actionDockerStop = "docker_stop"

	sigTerm      = "SIGTERM"
	sigKill      = "SIGKILL"
	sigTerminate = "terminate"

	// Provider modes (--mode).
	modeAuto      = "auto"
	modeNative    = "native"
	modeContainer = "container"

	reasonContainerPublishes = "port is published by a container; use --mode container"
	reasonNativeOwner        = "owned by a native process; use --mode native or auto"
)

// modeUsage is the help text of --mode.
const modeUsage = "Provider mode: auto (default; containers and native processes), native (only native processes are planned) or container (only containers are planned)"

// parseModeFlag validates the value of the --mode flag (which must be given
// explicitly if present).
func parseModeFlag(m string) (string, *cliError) {
	if m == "" {
		return "", usageErr("invalid --mode \"\": expected auto, native or container")
	}
	return parseMode(m)
}

// parseMode validates a mode value. "" (a plan from before modes existed)
// means auto.
func parseMode(m string) (string, *cliError) {
	switch m {
	case "", modeAuto:
		return modeAuto, nil
	case modeNative, modeContainer:
		return m, nil
	}
	return "", usageErr(fmt.Sprintf("invalid --mode %q: expected auto, native or container", m))
}

// Exit classes of the opt-in workflow.
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 2
	exitFreshPlan = 3
	exitUncertain = 4
)

func exitClassName(code int) string {
	switch code {
	case exitOK:
		return "ok"
	case exitFailed:
		return "failed"
	case exitUsage:
		return "usage"
	case exitFreshPlan:
		return "needs_fresh_plan"
	case exitUncertain:
		return "uncertain"
	}
	return "failed"
}

// exitRank orders exit classes by precedence: 2 > 1 > 4 > 3 > 0.
func exitRank(code int) int {
	switch code {
	case exitUsage:
		return 4
	case exitFailed:
		return 3
	case exitUncertain:
		return 2
	case exitFreshPlan:
		return 1
	}
	return 0
}

// worseExit returns the class that wins under the precedence rule.
func worseExit(a, b int) int {
	if exitRank(b) > exitRank(a) {
		return b
	}
	return a
}

// planDoc is the plan document. It deliberately contains no hostnames,
// usernames, command lines or paths.
type planDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	PlanID        string `json:"plan_id"`
	ToolVersion   string `json:"tool_version"`
	OS            string `json:"os"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	// ObservationID identifies the observation the plan was built from (see
	// observationID); DockerContext is the DOCKER_CONTEXT it ran under.
	ObservationID   string         `json:"observation_id,omitempty"`
	DockerContext   string         `json:"docker_context,omitempty"`
	Request         planRequest    `json:"request"`
	Endpoints       []planEndpoint `json:"endpoints"`
	Resources       []planResource `json:"resources"`
	Refused         []planRefusal  `json:"refused"`
	ActionsExecuted int            `json:"actions_executed"`
	// ExitClass is the class `killport plan` exits with for this plan. It is
	// a pure function of the plan content and is covered by plan_id.
	ExitClass string `json:"exit_class"`
}

type planRequest struct {
	Protocol     string `json:"protocol"`
	Scope        string `json:"scope"`
	Mode         string `json:"mode,omitempty"` // auto | native | container; "" in older plans means auto
	Ports        []int  `json:"ports"`
	Force        bool   `json:"force"`
	GraceSeconds int    `json:"grace_seconds"`
}

type planEndpoint struct {
	Protocol   string        `json:"protocol"`
	Port       int           `json:"port"`
	Visibility string        `json:"visibility"`
	Occupied   bool          `json:"occupied"`
	Resources  []string      `json:"resources"`
	Refused    []planRefusal `json:"refused"`
	// ProviderErrors are the observation failures that bear on this endpoint.
	ProviderErrors []providerError `json:"provider_errors"`
}

// planResource is one thing apply may act on: a native process (bound to its
// start time) or a Docker container (bound to its full immutable ID).
type planResource struct {
	ResourceID       string `json:"resource_id"`
	Provider         string `json:"provider"`
	PID              int    `json:"pid,omitempty"`
	ContainerID      string `json:"container_id,omitempty"`
	Name             string `json:"name"`
	StartTime        string `json:"start_time,omitempty"`
	IdentityStrength string `json:"identity_strength,omitempty"`
	Ports            []int  `json:"ports"`
	Action           string `json:"action"`
	Signal           string `json:"signal,omitempty"`
	GraceSeconds     int    `json:"grace_seconds,omitempty"`
}

// planRefusal is an owner the plan will not act on.
type planRefusal struct {
	Port   int    `json:"port"`
	PID    int    `json:"pid,omitempty"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// planOptions are the parsed flags of `killport plan`.
type planOptions struct {
	Protocol string
	Mode     string
	Ports    []int
	Force    bool
	Grace    time.Duration
	TTL      time.Duration
}

func procResourceID(pid int, start string) string { return "proc:" + strconv.Itoa(pid) + ":" + start }
func containerResourceID(id string) string        { return "container:" + id }

func formatTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// graceSeconds rounds a grace duration up to whole seconds.
func graceSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// computePlanID is "sha256:" + hex(sha256(compact JSON of the plan with an
// empty plan_id)).
func computePlanID(doc planDoc) string {
	doc.PlanID = ""
	data, err := json.Marshal(doc)
	if err != nil {
		// planDoc only holds plain values; this cannot happen.
		panic("killport: cannot encode plan: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// buildPlan turns one observation into a plan. It never mutates anything.
func buildPlan(sys system, snap *snapshot, opts planOptions, now time.Time) *planDoc {
	mode, _ := parseMode(opts.Mode)
	doc := &planDoc{
		SchemaVersion: planSchemaVersion,
		Kind:          planKind,
		ToolVersion:   version(),
		OS:            sys.GOOS(),
		CreatedAt:     formatTime(now),
		ExpiresAt:     formatTime(now.Add(opts.TTL)),
		DockerContext: snap.dockerContext,
		Request: planRequest{
			Protocol:     opts.Protocol,
			Scope:        scopeFor(opts.Protocol),
			Mode:         mode,
			Ports:        append([]int{}, opts.Ports...),
			Force:        opts.Force,
			GraceSeconds: graceSeconds(opts.Grace),
		},
		Endpoints: []planEndpoint{},
		Resources: []planResource{},
		Refused:   []planRefusal{},
	}
	signal := sigTerm
	switch {
	case sys.GOOS() == "windows":
		signal = sigTerminate
	case opts.Force:
		signal = sigKill
	}

	byID := map[string]*planResource{}
	seenRefusal := map[planRefusal]bool{}
	observed := make([]endpointObs, 0, len(opts.Ports))
	for _, port := range opts.Ports {
		e := snap.endpoint(port)
		observed = append(observed, e)
		pe := planEndpoint{
			Protocol: e.Protocol, Port: port, Visibility: e.Visibility, Occupied: e.Occupied, Resources: []string{}, Refused: []planRefusal{},
			ProviderErrors: append([]providerError{}, e.ProviderErrors...),
		}

		use := func(r planResource) {
			cur := byID[r.ResourceID]
			if cur == nil {
				r.Ports = []int{port}
				byID[r.ResourceID] = &r
			} else {
				cur.Ports = append(cur.Ports, port)
			}
			pe.Resources = append(pe.Resources, r.ResourceID)
		}
		refuse := func(pid int, name, reason string) {
			rf := planRefusal{Port: port, PID: pid, Name: name, Reason: reason}
			if seenRefusal[rf] {
				return
			}
			seenRefusal[rf] = true
			pe.Refused = append(pe.Refused, rf)
			doc.Refused = append(doc.Refused, rf)
		}

		for _, c := range e.Containers {
			if mode == modeNative {
				// Still an occupant (the endpoint is never called free), but
				// native mode never acts on containers.
				refuse(0, c.Name, reasonContainerPublishes)
				continue
			}
			if !c.FullID {
				refuse(0, c.Name, fmt.Sprintf("docker reported the container id %q, which is not a full 64-hex id; cannot bind the action to an immutable id", sanitizeProviderText(c.ID)))
				continue
			}
			use(planResource{
				ResourceID: containerResourceID(c.ID), Provider: providerDocker, ContainerID: c.ID, Name: c.Name,
				Action: actionDockerStop, GraceSeconds: doc.Request.GraceSeconds,
			})
		}
		for _, o := range e.Owners {
			l := Listener{Port: port, PID: o.PID, Process: o.Name}
			if isDockerInfra(o.Name) && mode == modeNative {
				refuse(o.PID, o.Name, reasonContainerPublishes)
				continue
			}
			if isDockerInfra(o.Name) {
				if len(e.Containers) > 0 {
					continue // freed by stopping the container that publishes the port
				}
				reason := fmt.Sprintf("port %d is held by Docker infrastructure (%s, PID %d); stop the container that publishes it instead", port, o.Name, o.PID)
				switch {
				case snap.dockerErr != nil:
					reason += "; the owning container is unknown because " + sanitizeProviderText(snap.dockerErr.Error())
				case !snap.dockerAvail:
					reason += "; docker was not found on PATH"
				}
				refuse(o.PID, o.Name, reason)
				continue
			}
			if mode == modeContainer {
				refuse(o.PID, o.Name, reasonNativeOwner)
				continue
			}
			if reason := protectedReason(sys, l); reason != "" {
				refuse(o.PID, o.Name, reason)
				continue
			}
			if snap.namesUnavailable && o.Name == "" {
				refuse(o.PID, o.Name, "process name unavailable (tasklist failed); refusing to signal")
				continue
			}
			switch o.Ident {
			case identOK:
				use(planResource{
					ResourceID: procResourceID(o.PID, o.StartTime), Provider: providerNative, PID: o.PID, Name: o.Name,
					StartTime: o.StartTime, IdentityStrength: o.Strength, Action: actionSignal, Signal: signal,
				})
			case identDenied:
				refuse(o.PID, o.Name, fmt.Sprintf("cannot bind process identity: permission denied (%v); try again with sudo / as Administrator", sanitizeProviderText(o.IdentErr.Error())))
			default:
				msg := "cannot bind process identity"
				if o.IdentErr != nil {
					msg += ": " + sanitizeProviderText(o.IdentErr.Error())
				}
				refuse(o.PID, o.Name, msg)
			}
		}
		if e.Hidden {
			refuse(0, "", fmt.Sprintf("the owner of port %d is not visible to this user (no process could be attributed to the occupying socket); try again with sudo / as Administrator", port))
		}
		doc.Endpoints = append(doc.Endpoints, pe)
	}

	for _, r := range byID {
		doc.Resources = append(doc.Resources, *r)
	}
	sort.Slice(doc.Resources, func(i, j int) bool {
		a, b := doc.Resources[i], doc.Resources[j]
		if (a.Provider == providerDocker) != (b.Provider == providerDocker) {
			return a.Provider == providerDocker // containers first
		}
		if a.Provider == providerDocker {
			return a.ContainerID < b.ContainerID
		}
		return a.PID < b.PID
	})
	sort.Slice(doc.Refused, func(i, j int) bool {
		a, b := doc.Refused[i], doc.Refused[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.PID != b.PID {
			return a.PID < b.PID
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Reason < b.Reason
	})
	doc.ObservationID = observationID(doc.Request, observed, snap.dockerContext)
	doc.ExitClass = exitClassName(planClass(doc))
	doc.PlanID = computePlanID(*doc)
	return doc
}

// planClass is the exit class of a freshly built plan.
func planClass(doc *planDoc) int {
	code := exitOK
	for _, e := range doc.Endpoints {
		switch e.Visibility {
		case visProviderUnavailable:
			code = worseExit(code, exitFailed)
		case visPermissionLimited, visNoneVisible:
			code = worseExit(code, exitUncertain)
		}
		if e.Occupied && len(e.Resources) == 0 {
			code = worseExit(code, exitFailed)
		}
	}
	if len(doc.Refused) > 0 {
		code = worseExit(code, exitFailed)
	}
	return code
}

// ---- loading and validating a plan --------------------------------------

var errPlanModified = errors.New("plan was modified or corrupted")

// decodePlan parses a plan file strictly (unknown fields are rejected, one
// document only) and verifies its plan_id. It returns a usage-class error for
// anything wrong with the document itself.
func decodePlan(data []byte) (*planDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc planDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid plan: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid plan: unexpected data after the plan document")
	}
	if doc.Kind != planKind {
		return nil, fmt.Errorf("invalid plan: kind is %q, expected %q", doc.Kind, planKind)
	}
	if doc.SchemaVersion != planSchemaVersion {
		return nil, fmt.Errorf("invalid plan: unsupported schema_version %d (this killport understands %d)", doc.SchemaVersion, planSchemaVersion)
	}
	if doc.PlanID != computePlanID(doc) {
		return nil, errPlanModified
	}
	return &doc, nil
}

// validatePlan checks the semantic invariants apply relies on before it
// touches anything. The plan_id is a checksum, not a signature, so a forged
// plan could still be self-consistent; these checks bound what it can name.
func validatePlan(doc *planDoc) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("invalid plan: "+format, args...)
	}
	req := doc.Request
	if req.Protocol != protoTCP && req.Protocol != protoUDP {
		return bad("unsupported protocol %q", req.Protocol)
	}
	if req.Scope != scopeFor(req.Protocol) {
		return bad("scope %q does not match protocol %q", req.Scope, req.Protocol)
	}
	mode, merr := parseMode(req.Mode)
	if merr != nil {
		return bad("unsupported mode %q", req.Mode)
	}
	if req.GraceSeconds < 0 || time.Duration(req.GraceSeconds)*time.Second > maxGrace {
		return bad("grace_seconds %d is out of range", req.GraceSeconds)
	}
	if len(req.Ports) == 0 {
		return bad("no ports")
	}
	reqPorts := map[int]bool{}
	for i, p := range req.Ports {
		if p < 1 || p > maxPort || reqPorts[p] || (i > 0 && p < req.Ports[i-1]) {
			return bad("request ports must be unique, ascending and within 1-%d", maxPort)
		}
		reqPorts[p] = true
	}
	created, err := time.Parse(time.RFC3339, doc.CreatedAt)
	if err != nil {
		return bad("created_at is not an RFC 3339 time")
	}
	expires, err := time.Parse(time.RFC3339, doc.ExpiresAt)
	if err != nil {
		return bad("expires_at is not an RFC 3339 time")
	}
	// A plan lives at most maxPlanTTL; a forged far-future expiry would make
	// a stale plan valid forever.
	if expires.Before(created) || expires.Sub(created) > maxPlanTTL {
		return bad("expires_at must not precede created_at or be more than %s after it", maxPlanTTL)
	}
	ids := map[string]bool{}
	for _, r := range doc.Resources {
		if ids[r.ResourceID] {
			return bad("duplicate resource %q", r.ResourceID)
		}
		ids[r.ResourceID] = true
		if len(r.Ports) == 0 {
			return bad("resource %q has no ports", r.ResourceID)
		}
		for _, p := range r.Ports {
			if !reqPorts[p] {
				return bad("resource %q names port %d which was not requested", r.ResourceID, p)
			}
		}
		switch r.Provider {
		case providerNative:
			if mode == modeContainer {
				return bad("resource %q is a native process but the plan's mode is %q", r.ResourceID, mode)
			}
			if r.PID <= 1 {
				return bad("resource %q targets protected PID %d", r.ResourceID, r.PID)
			}
			if r.PID > maxPID {
				return bad("resource %q has PID %d, which is out of range", r.ResourceID, r.PID)
			}
			if doc.OS == "windows" && r.PID == 4 {
				return bad("resource %q targets the Windows System process", r.ResourceID)
			}
			if r.StartTime == "" || r.IdentityStrength == "" || r.IdentityStrength == strengthNone {
				return bad("resource %q has no process identity", r.ResourceID)
			}
			if r.ResourceID != procResourceID(r.PID, r.StartTime) || r.ContainerID != "" {
				return bad("resource %q is inconsistent with its pid/start_time", r.ResourceID)
			}
			if r.Action != actionSignal {
				return bad("resource %q has unsupported action %q", r.ResourceID, r.Action)
			}
			want := sigTerm
			switch {
			case doc.OS == "windows":
				want = sigTerminate
			case req.Force:
				want = sigKill
			}
			if r.Signal != want {
				return bad("resource %q has signal %q, expected %q", r.ResourceID, r.Signal, want)
			}
		case providerDocker:
			if mode == modeNative {
				return bad("resource %q is a container but the plan's mode is %q", r.ResourceID, mode)
			}
			if !isFullContainerID(r.ContainerID) {
				return bad("resource %q has an invalid container id", r.ResourceID)
			}
			if r.ResourceID != containerResourceID(r.ContainerID) || r.PID != 0 || r.StartTime != "" {
				return bad("resource %q is inconsistent with its container id", r.ResourceID)
			}
			if r.Action != actionDockerStop {
				return bad("resource %q has unsupported action %q", r.ResourceID, r.Action)
			}
			if r.GraceSeconds < 0 || time.Duration(r.GraceSeconds)*time.Second > maxGrace {
				return bad("resource %q has an out-of-range grace period", r.ResourceID)
			}
		default:
			return bad("resource %q has unknown provider %q", r.ResourceID, r.Provider)
		}
	}
	for _, e := range doc.Endpoints {
		if e.Protocol != req.Protocol || !reqPorts[e.Port] {
			return bad("endpoint %s/%d was not requested", e.Protocol, e.Port)
		}
		for _, id := range e.Resources {
			if !ids[id] {
				return bad("endpoint %s/%d references unknown resource %q", e.Protocol, e.Port, id)
			}
		}
	}
	return nil
}

// ---- human rendering ------------------------------------------------------

// resourceLine is the one-line human description of a planned resource. The
// plan text and the tests derive it from the same JSON fields.
func resourceLine(r planResource) string {
	ports := joinInts(r.Ports)
	if r.Provider == providerDocker {
		return humanText(fmt.Sprintf("%s  docker stop -t %d  %s  ports %s", r.ResourceID, r.GraceSeconds, orUnknown(r.Name), ports))
	}
	return humanText(fmt.Sprintf("%s  %s  pid %d (%s)  ports %s", r.ResourceID, r.Signal, r.PID, orUnknown(r.Name), ports))
}

func refusalLine(r planRefusal) string {
	who := "owner not visible"
	switch {
	case r.PID > 0:
		who = fmt.Sprintf("pid %d (%s)", r.PID, orUnknown(r.Name))
	case r.Name != "":
		who = r.Name
	}
	return humanText(fmt.Sprintf("port %d  %s: %s", r.Port, who, r.Reason))
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
