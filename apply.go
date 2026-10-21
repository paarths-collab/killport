package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Apply result document constants.
const (
	applyKind = "killport.apply_result"

	// Per-resource results.
	resNotAttempted     = "not_attempted"
	resIdentityChanged  = "identity_changed"
	resSignalSent       = "signal_sent"
	resStopped          = "stopped"
	resPermissionDenied = "permission_denied"
	resTimeoutUnknown   = "timeout_unknown"
	resFailed           = "failed"

	// Per-endpoint statuses after the action.
	epFree            = "free"
	epBlockedOriginal = "blocked_by_original"
	epBlockedNewOwner = "blocked_by_new_owner"
	// epBlockedRefused: the occupant is one the plan itself refused to act on
	// (PID 1, Docker infrastructure, a hidden owner, ...). A fresh plan would
	// refuse it again, so this is a failure, not "needs a fresh plan".
	epBlockedRefused = "blocked_by_refused_owner"
	epNotVerifiable  = "not_verifiable"

	outcomePollInterval = 200 * time.Millisecond

	briefObserveTimeout = 15 * time.Second

	applyInterruptedPrefix = "apply interrupted"
)

// Time limits of one apply: grace plus applyWaveTimeout for every wave of
// maxConcurrentActions resources, never more than maxApplyDuration. After the
// limit (or a cancellation) no new action starts, and the outcome is observed
// once for at most briefObserveTimeout. (Variables only so a test can shorten
// them.)
var (
	applyWaveTimeout = 90 * time.Second
	maxApplyDuration = 10 * time.Minute
)

// applyDoc is the apply result document.
type applyDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	PlanID        string `json:"plan_id"`
	AppliedAt     string `json:"applied_at"`
	// ObservationID identifies the outcome observation (see observationID);
	// DockerContext is the DOCKER_CONTEXT it ran under. Both are absent when
	// nothing was observed (an expired plan).
	ObservationID   string          `json:"observation_id,omitempty"`
	DockerContext   string          `json:"docker_context,omitempty"`
	ActionsExecuted int             `json:"actions_executed"`
	Resources       []applyResource `json:"resources"`
	Endpoints       []applyEndpoint `json:"endpoints"`
	ExitClass       string          `json:"exit_class"`
	Error           string          `json:"error,omitempty"`
}

type applyResource struct {
	ResourceID   string `json:"resource_id"`
	Provider     string `json:"provider"`
	PID          int    `json:"pid,omitempty"`
	ContainerID  string `json:"container_id,omitempty"`
	Ports        []int  `json:"ports"`
	Result       string `json:"result"`
	VerifiedGone bool   `json:"verified_gone"`
	Forced       bool   `json:"forced"`
	Note         string `json:"note,omitempty"`
	Error        string `json:"error,omitempty"`
}

type applyEndpoint struct {
	Protocol   string   `json:"protocol"`
	Port       int      `json:"port"`
	Status     string   `json:"status"`
	Visibility string   `json:"visibility"`
	Holders    []holder `json:"holders,omitempty"`
	// ProviderErrors are the observation failures that bear on this endpoint.
	ProviderErrors []providerError `json:"provider_errors"`
}

// holder is whatever occupies an endpoint after the action.
type holder struct {
	PID         int    `json:"pid,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	Name        string `json:"name"`
}

// applier executes the resources of one validated plan.
type applier struct {
	ctx context.Context
	sys system
	doc *planDoc
	pre *snapshot // observation taken just before acting
	// results are the per-resource outcomes, set once the actions finished.
	results []applyResource
}

// expiredApplyDoc is the result for a plan whose time has passed: nothing was
// attempted and nothing was observed.
func expiredApplyDoc(doc *planDoc, now time.Time) *applyDoc {
	out := &applyDoc{
		SchemaVersion: planSchemaVersion, Kind: applyKind, PlanID: doc.PlanID, AppliedAt: formatTime(now),
		Resources: []applyResource{}, Endpoints: []applyEndpoint{},
		ExitClass: exitClassName(exitFreshPlan), Error: "plan expired",
	}
	for _, r := range doc.Resources {
		out.Resources = append(out.Resources, newApplyResource(r, resNotAttempted, "plan expired"))
	}
	return out
}

func newApplyResource(r planResource, result, note string) applyResource {
	return applyResource{
		ResourceID: r.ResourceID, Provider: r.Provider, PID: r.PID, ContainerID: r.ContainerID,
		Ports: append([]int{}, r.Ports...), Result: result, Note: note,
	}
}

// applyDeadline is the total time one apply may take: the grace period plus
// applyWaveTimeout for every wave of concurrent actions, at most
// maxApplyDuration.
func applyDeadline(doc *planDoc) time.Duration {
	waves := (len(doc.Resources) + maxConcurrentActions - 1) / maxConcurrentActions
	if waves < 1 {
		waves = 1
	}
	d := time.Duration(doc.Request.GraceSeconds)*time.Second + time.Duration(waves)*applyWaveTimeout
	if d > maxApplyDuration {
		d = maxApplyDuration
	}
	return d
}

// briefCtx returns ap.ctx while it is alive and otherwise a fresh short
// context that ignores the cancellation, so the outcome can still be looked
// at after an interrupt or a deadline.
func (ap *applier) briefCtx() (context.Context, context.CancelFunc) {
	if ap.ctx.Err() == nil {
		return ap.ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ap.ctx), briefObserveTimeout)
}

// applyPlan acts on the resources IN the plan (never on anything it finds
// by itself), then observes the outcome. It returns the result document and
// its exit code and the provider warnings seen after acting.
func applyPlan(ctx context.Context, sys system, doc *planDoc) (*applyDoc, int, []string) {
	startedAt := sys.Now()
	ctx, cancel := context.WithTimeout(ctx, applyDeadline(doc))
	defer cancel()
	ap := &applier{ctx: ctx, sys: sys, doc: doc}
	if ctx.Err() == nil {
		ap.pre = collect(ctx, sys, doc.Request.Protocol)
	} else {
		// Interrupted before anything started: nothing will be acted on.
		ap.pre = &snapshot{protocol: doc.Request.Protocol, tableHas: map[int]bool{}, dockerIDs: map[string]bool{}, idents: map[int]ownerObs{}}
	}

	results := make([]applyResource, len(doc.Resources))
	attempted := make([]bool, len(doc.Resources))
	skipped := false
	sem := make(chan struct{}, maxConcurrentActions)
	var wg sync.WaitGroup
	for i := range doc.Resources {
		sem <- struct{}{}
		if ctx.Err() != nil {
			// Cancelled or out of time: do not start any further action.
			<-sem
			results[i] = newApplyResource(doc.Resources[i], resNotAttempted, applyInterruptedPrefix+" before this resource was acted on")
			skipped = true
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], attempted[i] = ap.safeAct(doc.Resources[i])
		}(i)
	}
	wg.Wait()
	ap.results = results

	executed, waitable := 0, false
	for i, r := range results {
		if attempted[i] {
			executed++
		}
		switch r.Result {
		case resSignalSent, resStopped, resTimeoutUnknown:
			waitable = true
		}
	}

	final, statuses := ap.observeOutcome(waitable)
	interrupted := ctx.Err() != nil && (skipped || !allFree(statuses))

	for i := range results {
		results[i].VerifiedGone = ap.verifiedGone(doc.Resources[i], final)
		results[i].Forced = results[i].Result == resSignalSent && doc.Resources[i].Signal != sigTerm
	}
	out := &applyDoc{
		SchemaVersion: planSchemaVersion, Kind: applyKind, PlanID: doc.PlanID, AppliedAt: formatTime(startedAt),
		ActionsExecuted: executed, Resources: results, Endpoints: statuses,
		DockerContext: final.dockerContext,
		ObservationID: observationID(doc.Request, final.endpoints(doc.Request.Ports), final.dockerContext),
	}
	code := applyClass(out)
	if interrupted {
		// The results are partial: whatever was not done is not known to be
		// harmless, so the class is "uncertain" (re-inspect, then re-plan).
		code = exitUncertain
		out.Error = applyInterruptedPrefix + " (" + ctxReason(ctx.Err()) + "); the results are partial"
	}
	out.ExitClass = exitClassName(code)
	return out, code, final.warnings
}

func ctxReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "time limit reached"
	}
	return "cancelled"
}

func allFree(statuses []applyEndpoint) bool {
	for _, e := range statuses {
		if e.Status != epFree {
			return false
		}
	}
	return true
}

// safeAct is act with a panic turned into a failed resource: a panic on an
// action goroutine would otherwise take the whole process down.
func (ap *applier) safeAct(r planResource) (res applyResource, attempted bool) {
	defer func() {
		if p := recover(); p != nil {
			res = newApplyResource(r, resFailed, "")
			res.Error = sanitizeProviderText(fmt.Sprint("internal error: ", p))
			attempted = true
		}
	}()
	return ap.act(r)
}

// act performs one resource's action. The second result reports whether a
// mutating call was actually issued.
func (ap *applier) act(r planResource) (applyResource, bool) {
	if ap.ctx.Err() != nil {
		return newApplyResource(r, resNotAttempted, applyInterruptedPrefix+" before this resource was acted on"), false
	}
	if r.Provider == providerDocker {
		return ap.stopContainer(r)
	}
	return ap.signalProcess(r)
}

func (ap *applier) signalProcess(r planResource) (applyResource, bool) {
	if r.PID == ap.sys.Getpid() {
		res := newApplyResource(r, resFailed, "")
		res.Error = "refusing to signal killport itself"
		return res, false
	}
	if ap.noLongerHolds(r) {
		return newApplyResource(r, resNotAttempted, "process no longer holds the planned ports"), false
	}
	force := r.Signal != sigTerm
	err := ap.sys.SignalIfSame(r.PID, r.StartTime, force)
	switch {
	case err == nil:
		return newApplyResource(r, resSignalSent, ""), true
	case errors.Is(err, errIdentityChanged):
		return newApplyResource(r, resIdentityChanged, "the PID now belongs to a different process; nothing was signalled"), false
	case errors.Is(err, errProcessNotFound), errors.Is(err, os.ErrProcessDone):
		return newApplyResource(r, resNotAttempted, "process already exited"), false
	}
	res := newApplyResource(r, resFailed, "")
	res.Error = sanitizeProviderText(err.Error())
	if looksLikePermission(err) {
		res.Result = resPermissionDenied
	}
	return res, false
}

// noLongerHolds reports whether the pre-action observation PROVES that the
// planned incarnation of a process is alive but no longer holds any planned
// port (it closed its listener, another process took the port): signalling it
// would then hit a process that no longer has anything to do with the plan.
// It is false whenever that cannot be proven: any planned port without
// complete visibility, any port where the PID is still seen as an owner (an
// owner with another start time is for SignalIfSame to report), or a PID that
// is gone or reused (SignalIfSame reports that too).
func (ap *applier) noLongerHolds(r planResource) bool {
	for _, port := range r.Ports {
		e := ap.pre.endpoint(port)
		if e.Visibility != visComplete {
			return false
		}
		for _, o := range e.Owners {
			if o.PID == r.PID {
				return false
			}
		}
	}
	id, err := ap.sys.ProcessIdentity(r.PID)
	return err == nil && id.StartTime == r.StartTime
}

// dockerLists reports whether the pre-action observation shows the container
// and whether it still publishes one of the planned ports.
func (ap *applier) dockerLists(r planResource) (listed, publishing bool) {
	listed = ap.pre.dockerIDs[r.ContainerID]
	for _, c := range ap.pre.containers {
		if c.ID != r.ContainerID {
			continue
		}
		for _, p := range r.Ports {
			if containsInt(c.Ports, p) {
				publishing = true
			}
		}
	}
	return listed, publishing
}

func (ap *applier) stopContainer(r planResource) (applyResource, bool) {
	switch {
	case ap.pre.dockerErr != nil:
		res := newApplyResource(r, resNotAttempted, "")
		res.Error = "cannot confirm the container is still running: " + sanitizeProviderText(ap.pre.dockerErr.Error())
		return res, false
	case !ap.pre.dockerAvail:
		res := newApplyResource(r, resNotAttempted, "")
		res.Error = "docker was not found on PATH"
		return res, false
	}
	listed, publishing := ap.dockerLists(r)
	switch {
	case !listed:
		return newApplyResource(r, resNotAttempted, "container no longer running"), false
	case !publishing:
		return newApplyResource(r, resNotAttempted, "container no longer publishes the planned ports"), false
	}

	grace := time.Duration(r.GraceSeconds) * time.Second
	_, err := runTool(ap.ctx, ap.sys, grace+dockerStopMargin, "docker", "stop", "-t", strconv.Itoa(r.GraceSeconds), r.ContainerID)
	if err == nil {
		return newApplyResource(r, resStopped, ""), true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded), ap.ctx.Err() != nil:
		// The wrapper timed out, or apply was cancelled / ran out of time and
		// the docker client was killed (the daemon may still finish the
		// stop): ask Docker whether the container is gone.
		again := &snapshot{protocol: ap.doc.Request.Protocol, dockerIDs: map[string]bool{}}
		ctx, cancel := ap.briefCtx()
		again.collectDocker(ctx, ap.sys)
		cancel()
		if again.dockerErr == nil && again.dockerAvail && !again.dockerIDs[r.ContainerID] {
			return newApplyResource(r, resStopped, "docker stop timed out but the container is gone"), true
		}
		res := newApplyResource(r, resTimeoutUnknown, "")
		res.Error = sanitizeProviderText(err.Error())
		return res, true
	case strings.Contains(msg, "no such container"):
		return newApplyResource(r, resNotAttempted, "container no longer running"), false
	}
	res := newApplyResource(r, resFailed, "")
	res.Error = sanitizeProviderText(err.Error())
	if looksLikePermission(err) {
		res.Result = resPermissionDenied
	}
	return res, true
}

// observeOutcome polls until every planned endpoint is free or the grace
// period ends. With nothing to wait for it observes exactly once. After a
// cancellation it observes once more (with a fresh short context) and stops.
func (ap *applier) observeOutcome(wait bool) (*snapshot, []applyEndpoint) {
	deadline := ap.sys.Now().Add(time.Duration(ap.doc.Request.GraceSeconds) * time.Second)
	for {
		ctx, cancel := ap.briefCtx()
		interrupted := ap.ctx.Err() != nil
		snap := collect(ctx, ap.sys, ap.doc.Request.Protocol)
		cancel()
		statuses := ap.classify(snap)
		if allFree(statuses) || !wait || interrupted || !ap.sys.Now().Before(deadline) {
			return snap, statuses
		}
		sleepCtx(ap.ctx, ap.sys, outcomePollInterval)
	}
}

// classify derives each planned endpoint's status from one snapshot. An
// endpoint is free only with complete visibility and no occupant at all; an
// occupant is "original" only when it is exactly a planned resource (same pid
// AND start time, or the same full container ID).
func (ap *applier) classify(snap *snapshot) []applyEndpoint {
	// A planned resource whose stop timed out may or may not still be
	// shutting down: whatever it holds is "not verifiable", not "blocked".
	type ref struct {
		start  string
		unsure bool
	}
	plannedProc := map[int]map[int]ref{}    // port -> pid -> ref
	plannedCont := map[int]map[string]ref{} // port -> container id -> ref
	for i, r := range ap.doc.Resources {
		unsure := i < len(ap.results) && ap.results[i].Result == resTimeoutUnknown
		for _, p := range r.Ports {
			if r.Provider == providerDocker {
				if plannedCont[p] == nil {
					plannedCont[p] = map[string]ref{}
				}
				plannedCont[p][r.ContainerID] = ref{unsure: unsure}
			} else {
				if plannedProc[p] == nil {
					plannedProc[p] = map[int]ref{}
				}
				plannedProc[p][r.PID] = ref{start: r.StartTime, unsure: unsure}
			}
		}
	}
	// Owners the plan refused: by PID, by container name, or (PID 0, no name)
	// an owner that was not visible at all.
	refusedProc := map[int]map[int]bool{}
	refusedCont := map[int]map[string]bool{}
	refusedHidden := map[int]bool{}
	for _, rf := range ap.doc.Refused {
		switch {
		case rf.PID > 0:
			if refusedProc[rf.Port] == nil {
				refusedProc[rf.Port] = map[int]bool{}
			}
			refusedProc[rf.Port][rf.PID] = true
		case rf.Name != "":
			if refusedCont[rf.Port] == nil {
				refusedCont[rf.Port] = map[string]bool{}
			}
			refusedCont[rf.Port][rf.Name] = true
		default:
			refusedHidden[rf.Port] = true
		}
	}
	out := make([]applyEndpoint, 0, len(ap.doc.Request.Ports))
	for _, port := range ap.doc.Request.Ports {
		e := snap.endpoint(port)
		var holders []holder
		original, other, unknown, refused := false, false, false, false
		for _, o := range e.Owners {
			holders = append(holders, holder{PID: o.PID, Name: o.Name})
			pr, planned := plannedProc[port][o.PID]
			switch {
			case refusedProc[port][o.PID]:
				refused = true
			case isDockerInfra(o.Name) && len(e.Containers) > 0:
				// The port proxy of a container that is still listed; the
				// container itself is judged below.
			case isDockerInfra(o.Name) && snap.dockerErr != nil:
				unknown = true // whose proxy this is cannot be established
			case o.Ident != identOK:
				unknown = true
			case planned && pr.start == o.StartTime:
				original = true
			default:
				other = true
			}
		}
		for _, c := range e.Containers {
			holders = append(holders, holder{ContainerID: c.ID, Name: c.Name})
			switch cr, planned := plannedCont[port][c.ID]; {
			case refusedCont[port][c.Name] && !planned:
				refused = true
			case planned && cr.unsure:
				unknown = true
			case planned:
				original = true
			default:
				other = true
			}
		}
		if e.Hidden && refusedHidden[port] {
			refused = true
		}
		var status string
		switch {
		case original:
			status = epBlockedOriginal
		case refused:
			status = epBlockedRefused
		case other:
			status = epBlockedNewOwner
		case unknown, e.Visibility != visComplete:
			status = epNotVerifiable
		case !e.Occupied:
			status = epFree
		default:
			status = epBlockedNewOwner
		}
		out = append(out, applyEndpoint{
			Protocol: e.Protocol, Port: port, Status: status, Visibility: e.Visibility, Holders: holders,
			ProviderErrors: append([]providerError{}, e.ProviderErrors...),
		})
	}
	return out
}

// verifiedGone reports whether the ORIGINAL resource no longer exists.
func (ap *applier) verifiedGone(r planResource, snap *snapshot) bool {
	if r.Provider == providerDocker {
		return snap.dockerAvail && snap.dockerErr == nil && !snap.dockerIDs[r.ContainerID]
	}
	id, err := ap.sys.ProcessIdentity(r.PID)
	switch {
	case errors.Is(err, errProcessNotFound):
		return true
	case err != nil || id.StartTime == "":
		return false
	}
	return id.StartTime != r.StartTime
}

// applyClass is the exit class of an apply result (precedence 2 > 1 > 4 > 3 > 0).
func applyClass(d *applyDoc) int {
	code := exitOK
	for _, r := range d.Resources {
		switch r.Result {
		case resFailed, resPermissionDenied:
			code = worseExit(code, exitFailed)
		case resTimeoutUnknown:
			code = worseExit(code, exitUncertain)
		case resIdentityChanged:
			code = worseExit(code, exitFreshPlan)
		}
	}
	for _, e := range d.Endpoints {
		switch e.Status {
		case epBlockedOriginal, epBlockedRefused:
			code = worseExit(code, exitFailed)
		case epNotVerifiable:
			code = worseExit(code, exitUncertain)
		case epBlockedNewOwner:
			code = worseExit(code, exitFreshPlan)
		}
		if e.Visibility != visComplete {
			code = worseExit(code, exitUncertain)
		}
	}
	return code
}
