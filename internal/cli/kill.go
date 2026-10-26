package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row statuses and actions (the JSON contract of KillResult).
const (
	statusFree    = "free"
	statusPlanned = "planned"
	statusKilled  = "killed"
	statusStopped = "stopped"
	statusFailed  = "failed"

	actionNone = "none"
	actionKill = "kill_process"
	actionStop = "stop_container"
)

const (
	maxConcurrentActions = 4
	verifyInterval       = 200 * time.Millisecond
	verifyAttempts       = 16 // ~3s
)

// KillResult is one row of kill output: one (port, owner) pair, or a single
// row with status "free" for a port nobody listens on.
type KillResult struct {
	Port        string `json:"port"`
	ProcessName string `json:"process_name,omitempty"`
	PID         string `json:"pid,omitempty"`
	Killed      bool   `json:"killed"`
	Error       string `json:"error,omitempty"`
	IsDocker    bool   `json:"is_docker"`
	Status      string `json:"status"`
	Action      string `json:"action"`
	DryRun      bool   `json:"dry_run"`
	ContainerID string `json:"container_id,omitempty"`
}

// act is one mutating operation. It is shared by every row it affects so a
// PID or container holding several requested ports is acted on exactly once.
type act struct {
	kind      string
	pid       int
	name      string
	container container
	err       error

	// Native process identity captured at enumeration time (planKill).
	// startTime is the opaque token from system.ProcessIdentity. It is empty
	// when the platform has no identity support at all (noIdentity), the only
	// case in which the LEGACY command falls back to the unchecked Kill(pid).
	// identErr is a failed lookup (permission, ...): nothing is signalled for
	// it. gone means the process had already exited when its identity was
	// looked up.
	startTime  string
	noIdentity bool
	identErr   error
	gone       bool
}

// ownerKey identifies one owner of one port: a native PID (cid empty) or a
// Docker container (pid zero). PID 0 is an owner that is not visible.
type ownerKey struct {
	port int
	pid  int
	cid  string
}

// ownerInfo describes an owner seen during verification.
type ownerInfo struct {
	name string
	cont container
}

type killRow struct {
	KillResult
	port     int
	act      *act
	owner    ownerKey
	hasOwner bool // false only for "free" rows
}

func (r *killRow) fail(msg string) {
	r.Status = statusFailed
	r.Killed = false
	r.Error = msg
}

// protectedReason returns why a native PID must never be signalled, or "".
func protectedReason(sys system, l Listener) string {
	switch {
	case l.PID == 0:
		return fmt.Sprintf("the owner of port %d is not visible to this user (PID unknown); try again with sudo / as Administrator", l.Port)
	case l.PID <= 1:
		return fmt.Sprintf("refusing to kill PID %d (%s)", l.PID, orUnknown(l.Process))
	case l.PID == sys.Getpid():
		return "refusing to kill killport itself"
	case sys.GOOS() == "windows" && l.PID == 4:
		return "refusing to kill the Windows System process (PID 4)"
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// planKill builds one row per (port, owner) in deterministic order (port
// ascending, containers by name, then PIDs ascending) together with the
// de-duplicated list of actions needed.
func planKill(sys system, ports []int, lst listing, dk dockerState, dryRun bool) ([]*killRow, []*act) {
	byPort := groupByPort(lst.Listeners)
	var (
		rows    []*killRow
		acts    []*act
		procAct = map[int]*act{}
		ctrAct  = map[string]*act{}
	)
	for _, port := range ports {
		base := KillResult{Port: strconv.Itoa(port), DryRun: dryRun, Action: actionNone}
		conts := dk.Owners[port]
		ls := byPort[port]
		if len(conts) == 0 && len(ls) == 0 {
			r := base
			r.Status = statusFree
			rows = append(rows, &killRow{KillResult: r, port: port})
			continue
		}
		for _, c := range conts {
			a := ctrAct[c.ID]
			if a == nil {
				a = &act{kind: actionStop, name: c.Name, container: c}
				ctrAct[c.ID] = a
				acts = append(acts, a)
			}
			r := base
			r.Status = statusPlanned
			r.Action = actionStop
			r.IsDocker = true
			r.ProcessName = "[Docker] " + c.Name
			r.ContainerID = c.ID
			rows = append(rows, &killRow{KillResult: r, port: port, act: a, owner: ownerKey{port: port, cid: c.ID}, hasOwner: true})
		}
		for _, l := range ls {
			r := base
			r.ProcessName = l.Process
			if l.PID > 0 {
				r.PID = strconv.Itoa(l.PID)
			}
			if isDockerInfra(l.Process) && len(conts) > 0 {
				continue // freed by stopping the container that owns the port
			}
			var reason string
			if isDockerInfra(l.Process) {
				reason = fmt.Sprintf("port %d is held by Docker infrastructure (%s, PID %d); stop the container that publishes it instead", port, l.Process, l.PID)
				if dk.Warning != "" {
					reason += "; the owning container is unknown because " + dk.Warning
				} else if !dk.Available {
					reason += "; docker was not found on PATH"
				}
			} else {
				reason = protectedReason(sys, l)
			}
			if reason == "" && lst.NamesUnavailable && l.Process == "" {
				// Without a name Docker's own processes cannot be told apart
				// from ordinary ones, so do not risk killing them.
				reason = "process name unavailable (tasklist failed); refusing to kill"
			}
			owner := ownerKey{port: port, pid: l.PID}
			if reason != "" {
				r.Status = statusFailed
				r.Error = reason
				rows = append(rows, &killRow{KillResult: r, port: port, owner: owner, hasOwner: true})
				continue
			}
			a := procAct[l.PID]
			if a == nil {
				a = &act{kind: actionKill, pid: l.PID, name: l.Process}
				captureIdentity(sys, a)
				procAct[l.PID] = a
				if a.identErr == nil {
					acts = append(acts, a)
				}
			}
			if a.identErr != nil {
				// The identity lookup failed for a reason other than "no such
				// process": signalling an unverified PID is exactly what the
				// identity check exists to prevent.
				r.Status = statusFailed
				r.Error = fmt.Sprintf("cannot verify the identity of PID %d (%s), so nothing was signalled: %v", l.PID, orUnknown(l.Process), a.identErr)
				if looksLikePermission(a.identErr) {
					r.Error += " (try again with sudo / as Administrator)"
				}
				rows = append(rows, &killRow{KillResult: r, port: port, owner: owner, hasOwner: true})
				continue
			}
			r.Status = statusPlanned
			r.Action = actionKill
			rows = append(rows, &killRow{KillResult: r, port: port, act: a, owner: owner, hasOwner: true})
		}
	}
	return rows, acts
}

// captureIdentity records the process start time for a native action. The
// lookup is read-only, so it is also done in dry-run mode. Only a platform
// WITHOUT identity support (strength "none") leaves the act without a
// startTime, and only then does the legacy command fall back to the unchecked
// Kill(pid). A lookup that failed for any other reason (permission, ...) is
// kept as identErr and the process is not signalled. The structured
// plan/apply workflow refuses processes without identity altogether.
func captureIdentity(sys system, a *act) {
	id, err := sys.ProcessIdentity(a.pid)
	switch {
	case err == nil && id.Strength != strengthNone && id.StartTime != "":
		a.startTime = id.StartTime
	case errors.Is(err, errProcessNotFound):
		a.gone = true
	case id.Strength == strengthNone && (err == nil || errors.Is(err, errNoIdentity)):
		a.noIdentity = true
	case err != nil:
		a.identErr = err
	default:
		a.identErr = errors.New("the identity lookup returned no start time")
	}
}

// errChangedBeforeKill is the row error when the PID now belongs to a
// different process than the one that was enumerated.
var errChangedBeforeKill = errors.New("process changed between lookup and kill; nothing was signalled")

// killNative signals the process captured at enumeration time with SIGKILL
// (TerminateProcess on Windows), re-validating its identity first. A process
// that is already gone counts as success.
func killNative(sys system, a *act) error {
	switch {
	case a.gone:
		return nil
	case a.startTime != "":
		err := sys.SignalIfSame(a.pid, a.startTime, true)
		switch {
		case errors.Is(err, errIdentityChanged):
			return errChangedBeforeKill
		case errors.Is(err, errProcessNotFound):
			return nil
		}
		return err
	case a.noIdentity:
		return sys.Kill(a.pid) // legacy fallback: this platform has no process identity
	case a.identErr != nil:
		return fmt.Errorf("cannot verify the identity of PID %d: %w", a.pid, a.identErr)
	}
	return errors.New("no process identity was captured")
}

func looksLikePermission(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "access is denied")
}

// execute performs every action with bounded concurrency and records the
// real error text on each. In a dry run it returns without touching anything.
func execute(ctx context.Context, sys system, acts []*act, dryRun bool) {
	if dryRun {
		return
	}
	sem := make(chan struct{}, maxConcurrentActions)
	var wg sync.WaitGroup
	for _, a := range acts {
		wg.Add(1)
		sem <- struct{}{}
		go func(a *act) {
			defer wg.Done()
			defer func() { <-sem }()
			switch a.kind {
			case actionKill:
				err := killNative(sys, a)
				switch {
				case err == nil || errors.Is(err, os.ErrProcessDone):
				case errors.Is(err, errChangedBeforeKill):
					a.err = err
				default:
					msg := fmt.Sprintf("failed to kill PID %d (%s): %v", a.pid, orUnknown(a.name), err)
					if looksLikePermission(err) {
						msg += " (try again with sudo / as Administrator)"
					}
					a.err = errors.New(msg)
				}
			case actionStop:
				_, err := runTool(ctx, sys, dockerStopTimeout, "docker", "stop", "-t", "10", a.container.ID)
				if err != nil {
					a.err = fmt.Errorf("failed to stop container %s (%s): %v", a.container.Name, a.container.ID, err)
				}
			}
		}(a)
	}
	wg.Wait()
}

// ownersOnPorts returns every owner (native listener or container) of the
// given ports in the current enumeration.
func ownersOnPorts(ports map[int]bool, ls []Listener, dk dockerState) map[ownerKey]ownerInfo {
	owners := map[ownerKey]ownerInfo{}
	for _, l := range ls {
		if ports[l.Port] {
			owners[ownerKey{port: l.Port, pid: l.PID}] = ownerInfo{name: l.Process}
		}
	}
	for p, cs := range dk.Owners {
		if !ports[p] {
			continue
		}
		for _, c := range cs {
			owners[ownerKey{port: p, cid: c.ID}] = ownerInfo{name: c.Name, cont: c}
		}
	}
	return owners
}

// settled reports whether every remaining owner is one that was refused or
// whose action failed (those never go away by themselves).
func settled(owners map[ownerKey]ownerInfo, tolerate map[ownerKey]bool) bool {
	for k := range owners {
		if !tolerate[k] {
			return false
		}
	}
	return true
}

// verify polls until no owner of an acted-on port remains except tolerated
// ones (or the attempts run out) and returns the owners seen in the last
// enumeration.
func verify(ctx context.Context, sys system, ports map[int]bool, needDocker bool, tolerate map[ownerKey]bool) (map[ownerKey]ownerInfo, error) {
	var owners map[ownerKey]ownerInfo
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		lst, err := listListeners(ctx, sys)
		if err != nil {
			return nil, err
		}
		dk := dockerState{Owners: map[int][]container{}}
		if needDocker {
			dk = queryDocker(ctx, sys)
			if dk.Warning != "" {
				return nil, errors.New(dk.Warning)
			}
		}
		owners = ownersOnPorts(ports, lst.Listeners, dk)
		if settled(owners, tolerate) {
			return owners, nil
		}
		if attempt < verifyAttempts-1 {
			sys.Sleep(verifyInterval)
		}
	}
	return owners, nil
}

// initialOwners is the set of owners seen before any action was taken.
func initialOwners(ls []Listener, dk dockerState) map[ownerKey]bool {
	m := map[ownerKey]bool{}
	for _, l := range ls {
		m[ownerKey{port: l.Port, pid: l.PID}] = true
	}
	for p, cs := range dk.Owners {
		for _, c := range cs {
			m[ownerKey{port: p, cid: c.ID}] = true
		}
	}
	return m
}

// runKill plans, executes and verifies. It returns the rows and whether any
// row failed.
func (a *app) runKill(ctx context.Context, ports []int, lst listing, dk dockerState) ([]*killRow, bool) {
	rows, acts := planKill(a.sys, ports, lst, dk, a.opts.dryRun)

	execute(ctx, a.sys, acts, a.opts.dryRun)
	if !a.opts.dryRun {
		rows = a.finishRows(ctx, rows, initialOwners(lst.Listeners, dk))
	}
	failed := false
	for _, r := range rows {
		if r.Status == statusFailed {
			failed = true
		}
	}
	return rows, failed
}

// finishRows applies action errors, verifies every acted-on owner and settles
// each row's final status. Verification is per (port, owner): a row is killed
// or stopped only if its own owner is gone. Any other owner still present on
// an acted-on port keeps (or gets) its own failed row, so a port that is
// still held is never reported as freed.
func (a *app) finishRows(ctx context.Context, rows []*killRow, initial map[ownerKey]bool) []*killRow {
	acted := map[int]bool{}
	tolerate := map[ownerKey]bool{}
	actKind := map[int]string{}
	needDocker := false
	for _, r := range rows {
		switch {
		case r.act != nil && r.act.err != nil:
			r.fail(r.act.err.Error())
			tolerate[r.owner] = true
		case r.act != nil:
			acted[r.port] = true
			actKind[r.port] = r.act.kind
			if r.act.kind == actionStop {
				needDocker = true
			}
		case r.hasOwner: // refused
			tolerate[r.owner] = true
		}
	}
	if len(acted) == 0 {
		return rows
	}
	owners, verr := verify(ctx, a.sys, acted, needDocker, tolerate)
	for _, r := range rows {
		if r.act == nil || r.act.err != nil {
			continue
		}
		verb := "killed"
		if r.act.kind == actionStop {
			verb = "stopped"
		}
		switch {
		case verr != nil:
			r.fail(fmt.Sprintf("%s, but could not verify the port is free: %v", verb, verr))
		case hasOwner(owners, r.owner):
			if r.act.kind == actionStop {
				r.fail(fmt.Sprintf("port %d still in use after %s: container %s (%s) is still running", r.port, r.act.kind, containerName(r), r.ContainerID))
			} else {
				r.fail(fmt.Sprintf("PID %s (%s) still holds port %d after %s", r.PID, orUnknown(r.ProcessName), r.port, r.act.kind))
			}
		default:
			r.Killed = true
			if r.act.kind == actionStop {
				r.Status = statusStopped
			} else {
				r.Status = statusKilled
			}
		}
	}
	if verr != nil {
		return rows
	}

	// Owners nobody planned for (a replacement process, a restarted
	// container) or that were skipped as Docker infrastructure but are still
	// there get their own failed row.
	have := map[ownerKey]bool{}
	for _, r := range rows {
		if r.hasOwner {
			have[r.owner] = true
		}
	}
	keys := make([]ownerKey, 0, len(owners))
	for k := range owners {
		if !have[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		rows = append(rows, extraOwnerRow(k, owners[k], initial[k], actKind[k.port]))
	}
	sortRows(rows)
	return rows
}

func hasOwner(owners map[ownerKey]ownerInfo, k ownerKey) bool {
	_, ok := owners[k]
	return ok
}

// extraOwnerRow reports an owner that still holds (or newly holds) a port
// that was acted on.
func extraOwnerRow(k ownerKey, info ownerInfo, wasThere bool, kind string) *killRow {
	r := KillResult{Port: strconv.Itoa(k.port), Status: statusFailed, Action: actionNone}
	var who string
	switch {
	case k.cid != "":
		r.IsDocker = true
		r.ProcessName = "[Docker] " + info.cont.Name
		r.ContainerID = k.cid
		who = fmt.Sprintf("Docker container %s (%s)", info.cont.Name, k.cid)
	case k.pid > 0:
		r.ProcessName = info.name
		r.PID = strconv.Itoa(k.pid)
		who = fmt.Sprintf("PID %d (%s)", k.pid, orUnknown(info.name))
	default:
		who = "an owner that is not visible to this user"
	}
	if wasThere {
		r.Error = fmt.Sprintf("port %d is still held by %s after %s", k.port, who, kind)
	} else {
		r.Error = fmt.Sprintf("port %d is now held by %s", k.port, who)
	}
	return &killRow{KillResult: r, port: k.port, owner: k, hasOwner: true}
}

// sortRows orders rows by port, then containers (by name), then PIDs.
func sortRows(rows []*killRow) {
	class := func(r *killRow) int {
		switch {
		case !r.hasOwner:
			return 0
		case r.owner.cid != "":
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.port != b.port {
			return a.port < b.port
		}
		ca, cb := class(a), class(b)
		if ca != cb {
			return ca < cb
		}
		if ca == 1 {
			if a.ProcessName != b.ProcessName {
				return a.ProcessName < b.ProcessName
			}
			return a.owner.cid < b.owner.cid
		}
		return a.owner.pid < b.owner.pid
	})
}
