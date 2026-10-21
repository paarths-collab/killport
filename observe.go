package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// This file is the observation layer of the opt-in inspect / plan / apply
// workflow. It combines two independent sources so that "nobody owns this
// port" is never mistaken for "this port is free":
//
//  1. the SOCKET TABLE (occupancy: /proc/net/*, macOS netstat, Windows netstat)
//  2. OWNER ATTRIBUTION (lsof / ss / netstat+tasklist, plus Docker)
//
// Everything here is read-only.

// Endpoint visibility values.
const (
	visComplete            = "complete_for_requested_scope"
	visPermissionLimited   = "permission_limited"
	visNoneVisible         = "none_visible"
	visProviderUnavailable = "provider_unavailable"
)

// Protocols and scopes of a request.
const (
	protoTCP   = "tcp"
	protoUDP   = "udp"
	scopeTCP   = "listener"
	scopeUDP   = "bound"
	dockerFull = 64 // length of a full container ID
)

// Identity lookup outcomes for an attributed PID.
const (
	identOK     = "ok"
	identGone   = "gone"
	identDenied = "denied"
	identNone   = "none"
)

var (
	lsofUDPArgs = []string{"-nP", "+c", "0", "-iUDP", "-FpcnT"}
	ssUDPArgs   = []string{"-lunp"}
)

func scopeFor(protocol string) string {
	if protocol == protoUDP {
		return scopeUDP
	}
	return scopeTCP
}

// ownerObs is one native process attributed to an endpoint, with the identity
// that later actions bind to.
type ownerObs struct {
	PID       int
	Name      string
	StartTime string
	Strength  string
	Ident     string // identOK | identGone | identDenied | identNone
	IdentErr  error
}

// containerObs is a Docker container as listed by `docker ps --no-trunc`.
type containerObs struct {
	ID     string
	Name   string
	Ports  []int // published host ports of the requested protocol, ascending
	FullID bool  // ID is a full 64-hex immutable ID
}

// endpointObs is the observation of one {protocol, port} endpoint.
type endpointObs struct {
	Protocol      string
	Port          int
	Visibility    string
	Occupied      bool
	TableOccupied bool
	// Unattributed: some occupying socket has no owner we can act on or name
	// (hidden from lsof/ss, an ss row without users, or an identity lookup
	// that was denied). Sockets explained by a container's port proxy do not
	// count.
	Unattributed bool
	// Hidden: the occupancy itself is unexplained (no visible owner at all),
	// as opposed to an attributed owner whose identity lookup was denied.
	Hidden     bool
	Owners     []ownerObs // attributed native processes, ascending PID
	Containers []containerObs
}

// snapshot is one complete observation pass, independent of which ports are
// asked about afterwards.
type snapshot struct {
	goos     string
	protocol string

	tableOK  bool
	tableErr error
	tableHas map[int]bool

	owners           []Listener // PID 0 = occupied but owner not attributable
	ownerErr         error
	namesUnavailable bool

	dockerAvail bool
	dockerErr   error
	containers  []containerObs
	dockerIDs   map[string]bool // every container ID docker listed

	idents   map[int]ownerObs
	warnings []string
}

// collect gathers the socket table, the owners, Docker and the process
// identity of every attributed PID. It never mutates anything and never
// fails as a whole: each provider failure is recorded on the snapshot.
func collect(ctx context.Context, sys system, protocol string) *snapshot {
	s := &snapshot{goos: sys.GOOS(), protocol: protocol, tableHas: map[int]bool{}, dockerIDs: map[string]bool{}, idents: map[int]ownerObs{}}
	if s.goos == "windows" {
		s.collectWindows(ctx, sys)
	} else {
		s.collectTable(ctx, sys)
		s.collectOwnersUnix(ctx, sys)
	}
	s.collectDocker(ctx, sys)
	s.collectIdentities(sys)
	return s
}

func (s *snapshot) warn(format string, args ...any) {
	s.warnings = append(s.warnings, fmt.Sprintf(format, args...))
}

// ---- socket table ---------------------------------------------------------

func (s *snapshot) collectTable(ctx context.Context, sys system) {
	var (
		entries []sockEntry
		err     error
	)
	switch s.goos {
	case "linux":
		entries, err = readProcTables(sys, s.protocol)
	case "darwin":
		var out []byte
		out, err = runTool(ctx, sys, toolTimeout, "netstat", "-an", "-p", s.protocol)
		if err == nil {
			entries, err = parseMacNetstat(out, s.protocol)
		} else {
			err = fmt.Errorf("netstat failed: %w", err)
		}
	default:
		err = fmt.Errorf("socket table is not supported on %s", s.goos)
	}
	if err != nil {
		s.tableErr = err
		s.warn("socket table unavailable: %v", err)
		return
	}
	s.tableOK = true
	s.tableHas = occupiedPorts(entries, s.protocol)
}

// readProcTables reads /proc/net/{tcp,tcp6} or {udp,udp6}. The IPv4 table is
// required; a missing IPv6 table just means IPv6 is disabled.
func readProcTables(sys system, protocol string) ([]sockEntry, error) {
	var all []sockEntry
	for _, v6 := range []bool{false, true} {
		path := "/proc/net/" + protocol
		if v6 {
			path += "6"
		}
		data, err := sys.ReadFile(path)
		if err != nil {
			if v6 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		es, perr := parseProcNet(data, protocol, v6)
		if perr != nil {
			return nil, fmt.Errorf("%s: %w", path, perr)
		}
		all = append(all, es...)
	}
	return all, nil
}

// ---- owners ---------------------------------------------------------------

type ownerFn func() ([]Listener, string, error)

func (s *snapshot) collectOwnersUnix(ctx context.Context, sys system) {
	haveLsof := hasTool(sys, "lsof")
	haveSS := s.goos == "linux" && hasTool(sys, "ss")
	var viaLsof, viaSS ownerFn
	if s.protocol == protoUDP {
		viaLsof = func() ([]Listener, string, error) { return listLsofUDP(ctx, sys) }
		viaSS = func() ([]Listener, string, error) {
			out, err := runTool(ctx, sys, toolTimeout, "ss", ssUDPArgs...)
			if err != nil {
				return nil, "", fmt.Errorf("ss failed: %w", err)
			}
			l, perr := parseSSUDP(out)
			return l, "", perr
		}
	} else {
		viaLsof = func() ([]Listener, string, error) { return listLsof(ctx, sys) }
		viaSS = func() ([]Listener, string, error) {
			l, err := listSS(ctx, sys)
			return l, "", err
		}
	}
	var (
		ls   []Listener
		warn string
		err  error
	)
	switch {
	case haveLsof:
		ls, warn, err = viaLsof()
		if err != nil && haveSS {
			lerr := err
			if sls, sw, serr := viaSS(); serr == nil {
				ls, warn, err = sls, sw, nil
				s.warn("lsof failed (%v); owners come from ss and may be incomplete", lerr)
			}
		}
	case haveSS:
		ls, warn, err = viaSS()
	default:
		err = errors.New("no supported port inspection tool found (need lsof)")
	}
	if warn != "" {
		s.warn("%s", warn)
	}
	if err != nil {
		s.ownerErr = err
		s.warn("owner lookup failed: %v", err)
		return
	}
	s.owners = ls
}

// listLsofUDP runs `lsof -iUDP`. Exit status 1 with no output and no stderr
// means nothing matched; the classification matches listLsof.
func listLsofUDP(ctx context.Context, sys system) ([]Listener, string, error) {
	out, err := runTool(ctx, sys, toolTimeout, "lsof", lsofUDPArgs...)
	if err != nil {
		code, hasCode := exitCodeOf(err)
		empty := strings.TrimSpace(string(out)) == ""
		var ce *cmdError
		stderr := ""
		if errors.As(err, &ce) {
			stderr = ce.Stderr
		}
		switch {
		case hasCode && code == 1 && empty && stderr == "":
			return nil, "", nil
		case hasCode && code == 1 && empty:
			return nil, fmt.Sprintf("lsof exited with status 1 and no output but reported: %s; its result may be incomplete", stderr), nil
		case hasCode && code == 1:
			// partial output with warnings is still valid
		default:
			return nil, "", fmt.Errorf("lsof failed: %w", err)
		}
	}
	ls, perr := parseLsofUDP(out)
	if perr != nil {
		return nil, "", fmt.Errorf("lsof: %w", perr)
	}
	return ls, "", nil
}

// netRow is one netstat -ano row reduced to what the workflow needs. PID is 0
// when the PID field is missing or not a positive decimal integer.
type netRow struct {
	Port int
	PID  int
	Addr string
}

// parseNetstatRows extracts the rows of `netstat -ano` that occupy a local
// port for the protocol: TCP rows in LISTENING state (the same predicate as
// parseNetstat, including the wildcard-foreign fallback for localized state
// names) or every UDP row. Unlike parseNetstat it KEEPS rows whose PID is 0 or
// malformed (PID 0), because those still occupy the port. Only the LOCAL port
// is ever matched.
func parseNetstatRows(out string, protocol string) []netRow {
	var res []netRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.ToLower(f[0]) != protocol {
			continue
		}
		minFields := 4
		if protocol == protoTCP {
			minFields = 5
		}
		if len(f) < minFields {
			continue
		}
		_, port, ok := splitHostPort(f[1])
		if !ok {
			continue
		}
		if protocol == protoTCP {
			fhost, fport := "", -1
			if i := strings.LastIndex(f[2], ":"); i >= 0 {
				fhost = f[2][:i]
				if n, err := strconv.Atoi(f[2][i+1:]); err == nil {
					fport = n
				}
			}
			listening := strings.EqualFold(f[3], "LISTENING") ||
				(fport == 0 && isWildcardHost(fhost) && !nonListeningStates[strings.ToUpper(f[3])])
			if !listening {
				continue
			}
		}
		pid := 0
		if n, err := parseDecimalPID(f[len(f)-1]); err == nil {
			pid = n
		}
		res = append(res, netRow{Port: port, PID: pid, Addr: f[1]})
	}
	return res
}

// parseDecimalPID parses a strictly decimal, positive PID.
func parseDecimalPID(s string) (int, error) {
	if s == "" || len(s) > 10 {
		return 0, fmt.Errorf("invalid PID %q", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid PID %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid PID %q", s)
	}
	return n, nil
}

// collectWindows: `netstat -ano` is both the socket table and the owner
// source; `tasklist` supplies the names.
func (s *snapshot) collectWindows(ctx context.Context, sys system) {
	out, err := runTool(ctx, sys, toolTimeout, "netstat", "-ano")
	if err != nil {
		err = fmt.Errorf("netstat failed: %w", err)
		s.ownerErr = err
		s.tableErr = err
		s.warn("%v", err)
		return
	}
	rows := parseNetstatRows(string(out), s.protocol)
	s.tableOK = true
	hasPID := false
	for _, r := range rows {
		s.tableHas[r.Port] = true
		s.owners = append(s.owners, Listener{Port: r.Port, PID: r.PID, Addr: r.Addr})
		if r.PID > 0 {
			hasPID = true
		}
	}
	if !hasPID {
		return
	}
	tout, terr := runTool(ctx, sys, toolTimeout, "tasklist", "/FO", "CSV", "/NH")
	if terr != nil {
		s.namesUnavailable = true
		s.warn("tasklist failed (%v); process names are unavailable and owners will not be signalled", terr)
		return
	}
	names := parseTasklist(string(tout))
	for i := range s.owners {
		s.owners[i].Process = names[s.owners[i].PID]
	}
}

// ---- Docker ---------------------------------------------------------------

func (s *snapshot) collectDocker(ctx context.Context, sys system) {
	if !hasTool(sys, "docker") {
		return
	}
	s.dockerAvail = true
	out, err := runTool(ctx, sys, dockerPSTimeout, "docker", "ps", "--no-trunc", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}")
	if err != nil {
		s.dockerErr = fmt.Errorf("docker ps failed: %w", err)
		s.warn("could not query Docker (%v); Docker-published ports may be missing from the results", err)
		return
	}
	s.containers = parseDockerPSFull(string(out), s.protocol)
	for _, line := range strings.Split(string(out), "\n") {
		if id, _, ok := strings.Cut(strings.TrimSpace(line), "|"); ok && strings.TrimSpace(id) != "" {
			s.dockerIDs[strings.TrimSpace(id)] = true
		}
	}
}

// isFullContainerID reports whether id is a 64-digit lowercase hex ID.
func isFullContainerID(id string) bool {
	if len(id) != dockerFull {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// parseDockerPSFull parses `docker ps --no-trunc --format '{{.ID}}|{{.Names}}|{{.Ports}}'`
// for the given protocol. Containers publishing no host port of that protocol
// are omitted. The result is sorted by ID.
func parseDockerPSFull(out, protocol string) []containerObs {
	var res []containerObs
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		if id == "" {
			continue
		}
		ports := parseDockerPortsProto(parts[2], protocol)
		if len(ports) == 0 {
			continue
		}
		res = append(res, containerObs{ID: id, Name: strings.TrimSpace(parts[1]), Ports: ports, FullID: isFullContainerID(id)})
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].ID < res[j].ID })
	return res
}

// ---- identities -----------------------------------------------------------

func lookupOwner(sys system, pid int, name string) ownerObs {
	o := ownerObs{PID: pid, Name: name}
	id, err := sys.ProcessIdentity(pid)
	switch {
	case err == nil && id.Strength != strengthNone && id.StartTime != "":
		o.Ident, o.StartTime, o.Strength = identOK, id.StartTime, id.Strength
	case errors.Is(err, errProcessNotFound):
		o.Ident, o.IdentErr = identGone, err
	case err != nil && looksLikePermission(err):
		o.Ident, o.IdentErr = identDenied, err
	default:
		o.Ident, o.Strength, o.IdentErr = identNone, strengthNone, err
	}
	return o
}

func (s *snapshot) collectIdentities(sys system) {
	for _, l := range s.owners {
		if l.PID <= 0 {
			continue
		}
		if _, done := s.idents[l.PID]; done {
			continue
		}
		s.idents[l.PID] = lookupOwner(sys, l.PID, l.Process)
	}
}

// ---- per-endpoint projection ---------------------------------------------

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// endpoint projects the snapshot onto one port.
func (s *snapshot) endpoint(port int) endpointObs {
	e := endpointObs{Protocol: s.protocol, Port: port}
	e.TableOccupied = s.tableOK && s.tableHas[port]
	for _, c := range s.containers {
		if containsInt(c.Ports, port) {
			e.Containers = append(e.Containers, c)
		}
	}
	hiddenRow := false
	seen := map[int]bool{}
	for _, l := range s.owners {
		if l.Port != port {
			continue
		}
		if l.PID <= 0 {
			hiddenRow = true
			continue
		}
		if seen[l.PID] {
			continue
		}
		seen[l.PID] = true
		o := s.idents[l.PID]
		if o.Ident == identGone {
			continue // exited since lsof ran; the socket table decides occupancy
		}
		if o.Name == "" {
			o.Name = l.Process
		}
		e.Owners = append(e.Owners, o)
	}
	sort.Slice(e.Owners, func(i, j int) bool { return e.Owners[i].PID < e.Owners[j].PID })

	explained := len(e.Containers) > 0
	deniedActionable := false
	for _, o := range e.Owners {
		if o.Ident == identDenied && !(explained && isDockerInfra(o.Name)) {
			deniedActionable = true
		}
	}
	e.Hidden = !explained && (hiddenRow || (e.TableOccupied && len(e.Owners) == 0))
	e.Unattributed = e.Hidden || deniedActionable
	e.Occupied = e.TableOccupied || len(e.Owners) > 0 || len(e.Containers) > 0 || hiddenRow
	e.Visibility = s.visibility(&e)
	return e
}

// visibility implements the precedence in the design: provider_unavailable,
// permission_limited, none_visible, complete_for_requested_scope.
func (s *snapshot) visibility(e *endpointObs) string {
	switch {
	case s.ownerErr != nil || s.dockerErr != nil:
		return visProviderUnavailable
	case s.namesUnavailable && len(e.Owners) > 0:
		return visProviderUnavailable
	case e.Unattributed:
		return visPermissionLimited
	case !s.tableOK && len(e.Owners) == 0 && len(e.Containers) == 0:
		return visNoneVisible
	}
	return visComplete
}

// occupiedPortList returns every port that shows any sign of occupancy,
// ascending (used when no ports were requested).
func (s *snapshot) occupiedPortList() []int {
	seen := map[int]bool{}
	for p := range s.tableHas {
		seen[p] = true
	}
	for _, l := range s.owners {
		seen[l.Port] = true
	}
	for _, c := range s.containers {
		for _, p := range c.Ports {
			seen[p] = true
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

// endpoints observes the given ports (ascending) or, when ports is nil, every
// occupied port.
func (s *snapshot) endpoints(ports []int) []endpointObs {
	if ports == nil {
		ports = s.occupiedPortList()
	}
	res := make([]endpointObs, 0, len(ports))
	for _, p := range ports {
		res = append(res, s.endpoint(p))
	}
	return res
}

// providerFailed reports whether an owner or Docker provider failed outright.
func (s *snapshot) providerFailed() bool {
	return s.ownerErr != nil || s.dockerErr != nil || s.namesUnavailable
}
