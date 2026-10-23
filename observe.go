package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
	// ProviderErrors are the provider failures that bear on this endpoint
	// (sanitised, sorted, de-duplicated). Never nil after endpoint().
	ProviderErrors []providerError
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

	// dockerContext is the DOCKER_CONTEXT environment variable (sanitised),
	// "" when unset. killport never shells out to read Docker's contexts and
	// never records DOCKER_HOST.
	dockerContext string
	// perrs are the provider failures seen while collecting. Unlike
	// warnings they are structured and path/user sanitised.
	perrs []scopedProviderError
}

// collect gathers the socket table, the owners, Docker and the process
// identity of every attributed PID. It never mutates anything and never
// fails as a whole: each provider failure is recorded on the snapshot.
func collect(ctx context.Context, sys system, protocol string) *snapshot {
	s := &snapshot{goos: sys.GOOS(), protocol: protocol, tableHas: map[int]bool{}, dockerIDs: map[string]bool{}, idents: map[int]ownerObs{}}
	s.dockerContext = sanitizeProviderText(strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")))
	if s.goos == "windows" {
		s.collectWindows(ctx, sys)
	} else {
		s.collectTable(ctx, sys)
		s.collectOwnersUnix(ctx, sys)
	}
	s.collectDocker(ctx, sys)
	for i := range s.owners {
		// Process names come from other programs (possibly in a legacy code
		// page); make them valid UTF-8 before they reach any document.
		s.owners[i].Process = validUTF8(s.owners[i].Process)
	}
	s.collectIdentities(sys)
	return s
}

// warn records a warning. Warnings are derived from provider output and end
// up on stderr and in the inspect document, so they are sanitised here.
func (s *snapshot) warn(format string, args ...any) {
	s.warnings = append(s.warnings, sanitizeProviderText(fmt.Sprintf(format, args...)))
}

// ---- socket table ---------------------------------------------------------

// tableProvider names the socket-table source for the platform.
func (s *snapshot) tableProvider() string {
	if s.goos == "linux" {
		return provProc
	}
	return provNetstat
}

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
		switch {
		case err != nil:
			err = fmt.Errorf("netstat failed: %w", err)
		case !netstatHasTable(out):
			err = &parseError{errNoNetstatTable}
		default:
			entries, err = parseMacNetstat(out, s.protocol)
			if err != nil {
				err = &parseError{err}
			}
		}
	default:
		err = fmt.Errorf("socket table is not supported on %s", s.goos)
	}
	if err != nil {
		s.tableErr = err
		s.warn("socket table unavailable: %v", err)
		s.addProviderError(s.tableProvider(), err, false)
		return
	}
	s.tableOK = true
	s.tableHas = occupiedPorts(entries, s.protocol)
}

var errNoNetstatTable = errors.New("netstat produced no socket table (empty or unrecognised output)")

// netstatHasTable reports whether netstat output looks like a socket table: a
// "Proto ..." header line or at least one tcp/udp row. Empty output with exit
// status 0 is a broken tool, not an empty machine.
func netstatHasTable(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		w := strings.ToLower(f[0])
		if w == "proto" || strings.HasPrefix(w, "tcp") || strings.HasPrefix(w, "udp") {
			return true
		}
	}
	return false
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
			return nil, &parseError{fmt.Errorf("%s: %w", path, perr)}
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
			if perr != nil {
				perr = &parseError{perr}
			}
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
		ls       []Listener
		warn     string
		err      error
		warnProv = provLsof
	)
	switch {
	case haveLsof:
		ls, warn, err = viaLsof()
		if err != nil {
			s.addProviderError(provLsof, err, false)
			if haveSS {
				lerr := err
				sls, sw, serr := viaSS()
				if serr == nil {
					ls, warn, err = sls, sw, nil
					warnProv = provSS
					s.warn("lsof failed (%v); owners come from ss and may be incomplete", lerr)
				} else {
					s.addProviderError(provSS, serr, false)
				}
			}
		}
	case haveSS:
		ls, warn, err = viaSS()
		warnProv = provSS
		if err != nil {
			s.addProviderError(provSS, err, false)
		}
	default:
		err = errors.New("no supported port inspection tool found (need lsof)")
		s.addProviderError(provLsof, err, false)
	}
	if warn != "" {
		s.warn("%s", warn)
		s.addProviderError(warnProv, errors.New(warn), false)
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
		return nil, "", &parseError{fmt.Errorf("lsof: %w", perr)}
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
		s.addProviderError(provNetstat, err, false)
		return
	}
	if !netstatHasTable(out) {
		err := &parseError{errNoNetstatTable}
		s.ownerErr = err
		s.tableErr = err
		s.warn("%v", err)
		s.addProviderError(provNetstat, err, false)
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
		s.addProviderError(provTasklist, terr, true)
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
	if err := requireLocalDocker(ctx, sys); err != nil {
		s.dockerErr = err
		s.warn("could not query Docker: %v", err)
		s.addProviderError(provDocker, err, false)
		return
	}
	out, err := runTool(ctx, sys, dockerPSTimeout, "docker", "ps", "--no-trunc", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}")
	if err != nil {
		s.dockerErr = fmt.Errorf("docker ps failed: %w", err)
		s.warn("could not query Docker (%v); Docker-published ports may be missing from the results", err)
		s.addProviderError(provDocker, err, false)
		return
	}
	s.containers = parseDockerPSFull(string(out), s.protocol)
	for _, line := range strings.Split(string(out), "\n") {
		if id, _, ok := strings.Cut(strings.TrimSpace(line), "|"); ok && strings.TrimSpace(id) != "" {
			s.dockerIDs[validUTF8(strings.TrimSpace(id))] = true
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
		id := validUTF8(strings.TrimSpace(parts[0]))
		if id == "" {
			continue
		}
		ports := parseDockerPortsProto(parts[2], protocol)
		if len(ports) == 0 {
			continue
		}
		res = append(res, containerObs{ID: id, Name: validUTF8(strings.TrimSpace(parts[1])), Ports: ports, FullID: isFullContainerID(id)})
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
	e.ProviderErrors = s.endpointProviderErrors(&e, explained)
	return e
}

// endpointProviderErrors lists the provider failures that bear on one
// endpoint: the snapshot-wide ones, plus the identity lookups of its owners
// that failed for a reason other than the process having exited.
func (s *snapshot) endpointProviderErrors(e *endpointObs, explained bool) []providerError {
	res := []providerError{}
	add := func(pe providerError) {
		for _, x := range res {
			if x == pe {
				return
			}
		}
		res = append(res, pe)
	}
	for _, sp := range s.perrs {
		if sp.ownersOnly && len(e.Owners) == 0 {
			continue
		}
		add(sp.providerError)
	}
	for _, o := range e.Owners {
		if o.IdentErr == nil || (o.Ident != identDenied && o.Ident != identNone) {
			continue
		}
		if explained && isDockerInfra(o.Name) {
			continue // the container is what gets acted on; the proxy's identity is irrelevant
		}
		err := fmt.Errorf("cannot read the identity of pid %d: %w", o.PID, o.IdentErr)
		add(newProviderError(provProc, providerCategory(err), err.Error()))
	}
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Message < b.Message
	})
	return res
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

// ---- provider errors ---------------------------------------------------------

// Providers and failure categories reported in provider_errors.
const (
	provDocker   = "docker"
	provLsof     = "lsof"
	provSS       = "ss"
	provNetstat  = "netstat"
	provTasklist = "tasklist"
	provProc     = "proc"

	catUnavailable = "unavailable"
	catTimeout     = "timeout"
	catPermission  = "permission"
	catFailed      = "failed"
	catParse       = "parse"

	maxProviderMessage = 200
)

// providerError is one failed observation source, as it appears in the
// inspect / plan / apply documents. Messages are sanitised: no absolute
// paths, hostnames or user names, at most 200 characters.
type providerError struct {
	Provider string `json:"provider"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

// scopedProviderError is a snapshot-level provider failure. ownersOnly marks
// failures that only matter to endpoints that have native owners (tasklist).
type scopedProviderError struct {
	providerError
	ownersOnly bool
}

// parseError marks an error produced while parsing a provider's output (as
// opposed to running it). Its text is the wrapped error's text.
type parseError struct{ err error }

func (e *parseError) Error() string { return e.err.Error() }
func (e *parseError) Unwrap() error { return e.err }

// providerCategory classifies a provider failure: parse, timeout, permission,
// unavailable (the tool, file or daemon is not there) or failed.
func providerCategory(err error) string {
	var pe *parseError
	switch {
	case errors.As(err, &pe):
		return catParse
	case errors.Is(err, context.DeadlineExceeded):
		return catTimeout
	case looksLikePermission(err):
		return catPermission
	case looksUnavailable(err):
		return catUnavailable
	}
	return catFailed
}

func looksUnavailable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	if code, ok := exitCodeOf(err); ok && code == 127 {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"executable file not found", "command not found", "no such file or directory",
		"cannot connect to the docker daemon", "is the docker daemon running", "error during connect",
		"connection refused", "no supported port inspection tool", "is not supported on",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

func newProviderError(provider, category, msg string) providerError {
	return providerError{Provider: provider, Category: category, Message: sanitizeProviderText(msg)}
}

// addProviderError records a failure of one provider on the snapshot
// (de-duplicated).
func (s *snapshot) addProviderError(provider string, err error, ownersOnly bool) {
	sp := scopedProviderError{providerError: newProviderError(provider, providerCategory(err), err.Error()), ownersOnly: ownersOnly}
	for _, x := range s.perrs {
		if x == sp {
			return
		}
	}
	s.perrs = append(s.perrs, sp)
}

// Building blocks of the path scrubbers. A path segment may contain single
// spaces ("John Smith", "Program Files (x86)") as long as a separator follows
// it, which is what separates a path with spaces from a path followed by prose.
const (
	// pathWord is one word of a spaced path segment; ":" ends it so that
	// "/x: failed to open /y" is not read as one segment.
	pathWord = `(?:[^\s/\\:"'<>|()\[\]\x60,;]|\([^\s()]*\))+`
	// pathMore is the rest of a path after its first separator: any number of
	// (possibly spaced) segments each followed by a separator, then the last
	// run of non-space characters.
	pathMore = `(?:` + pathWord + `(?: ` + pathWord + `)*[\\/])*[^\s"'<>|)\]\x60,;]*`
	// pathStart are the characters that may precede a path inside a message.
	pathStart = `(^|[\s"'(\[=,:])`
)

var (
	// scheme://rest, e.g. unix:///var/run/docker.sock or tcp://10.0.0.5:2375.
	urlRe = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9+.-]*)://[^\s"'<>)\]\x60,;]*`)
	// A quoted path: the whole quoted text is replaced (a quoted path may
	// contain spaces), the quotes are kept.
	quotedPathRes = []*regexp.Regexp{quotedPathRe(`"`), quotedPathRe(`'`), quotedPathRe(`\x60`)}
	// Home-directory forms: ~/x, ~user/x, $HOME, ${HOME}/x, %USERPROFILE%\x.
	tildePathRe = regexp.MustCompile(pathStart + `~[A-Za-z0-9._-]*[\\/]` + pathMore)
	envPathRe   = regexp.MustCompile(pathStart + `(?i:\$\{?(?:HOME|USER|USERNAME|LOGNAME)\}?|%[A-Za-z_][A-Za-z0-9_]*%)(?:[\\/]` + pathMore + `)?`)
	// A Windows drive path (C:\x, C:/x) or UNC path (\\host\share).
	winPathRe = regexp.MustCompile(`(?:\b[A-Za-z]:[\\/]|\\\\)` + pathMore)
	// An absolute Unix path: a "/" that starts a token (or follows one of the
	// usual delimiters, ":" and "=" included) and is followed by at least one
	// more character that is not another "/" (so "://" is not a path).
	unixPathRe = regexp.MustCompile(pathStart + `/(?:` + pathWord + `(?: ` + pathWord + `)*[\\/])*[^\s/"'<>|)\]\x60,;][^\s"'<>|)\]\x60,;]*`)
	// Address literals. Port numbers are left alone.
	ipv4Re = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	ipv6Re = regexp.MustCompile(`(^|[^0-9A-Za-z:])(?i:(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}|(?:[0-9a-f]{1,4}:){1,7}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?|::[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})(?:%[0-9A-Za-z._-]+)?`)
	// Host names of the usual private zones, and user@host.
	privateHostRe = regexp.MustCompile(`(?i)\b[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*\.(?:local|lan|internal|home|localdomain|corp|intranet)\b`)
	userAtHostRe  = regexp.MustCompile(`[^\s@"'<>,;()\[\]\x60]+@[^\s@"'<>,;)\]\x60]+`)
)

// quotedPathRe matches q, a path start (drive, UNC, "/", ~, $VAR, %VAR%), the
// rest of the quoted text, and the closing q.
func quotedPathRe(q string) *regexp.Regexp {
	rest := `[^` + q + `\n]`
	return regexp.MustCompile(q + `((?:[A-Za-z]:[\\/]|\\\\|/` + rest + `|~[A-Za-z0-9._-]*[\\/]|\$\{?[A-Za-z]|%[A-Za-z_][A-Za-z0-9_]*%)` + rest + `*)` + q)
}

// validUTF8 replaces invalid UTF-8 in a string that came from outside the
// process (a process name in a Windows OEM code page, say) so that it
// survives a JSON round trip unchanged. Every provider-originated string that
// ends up in a document goes through it (or through sanitizeProviderText).
func validUTF8(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// sanitizeProviderText removes what must never leave the machine in a
// document or in human output: URLs and hostnames, absolute paths (including
// quoted ones, ones with spaces, ~ and $HOME forms), IP address literals,
// private host names and user@host pairs (each becomes a "<...>" marker; user
// names normally appear only inside such paths, e.g. /home/alice/...), and the
// result is at most 200 characters of valid UTF-8.
func sanitizeProviderText(s string) string {
	s = humanText(strings.Join(strings.Fields(scrubText(s)), " "))
	if utf8.RuneCountInString(s) > maxProviderMessage {
		r := []rune(s)
		s = string(r[:maxProviderMessage-3]) + "..."
	}
	return s
}

// scrubText applies the scrubbing of sanitizeProviderText without shortening
// the text.
func scrubText(s string) string {
	s = validUTF8(s)
	s = urlRe.ReplaceAllStringFunc(s, func(m string) string {
		scheme, rest, _ := strings.Cut(m, "://")
		if strings.HasPrefix(rest, "/") || scheme == "npipe" {
			return scheme + ":<path>"
		}
		return scheme + "://<host>"
	})
	for i, q := range []string{`"`, `'`, "`"} {
		s = quotedPathRes[i].ReplaceAllString(s, q+"<path>"+q)
	}
	s = tildePathRe.ReplaceAllString(s, "${1}<path>")
	s = envPathRe.ReplaceAllString(s, "${1}<path>")
	s = winPathRe.ReplaceAllString(s, "<path>")
	s = unixPathRe.ReplaceAllString(s, "${1}<path>")
	s = ipv4Re.ReplaceAllString(s, "<ip>")
	s = ipv6Re.ReplaceAllString(s, "${1}<ip>")
	s = privateHostRe.ReplaceAllString(s, "<host>")
	s = userAtHostRe.ReplaceAllString(s, "<user@host>")
	return s
}

// ---- observation id -----------------------------------------------------------

type obsOwner struct {
	PID       int    `json:"pid"`
	Name      string `json:"name"`
	StartTime string `json:"start_time,omitempty"`
	Strength  string `json:"identity_strength,omitempty"`
}

type obsContainer struct {
	ID   string `json:"container_id"`
	Name string `json:"name"`
}

type obsEndpoint struct {
	Protocol       string          `json:"protocol"`
	Port           int             `json:"port"`
	Visibility     string          `json:"visibility"`
	Occupied       bool            `json:"occupied"`
	Unattributed   bool            `json:"unattributed"`
	Owners         []obsOwner      `json:"owners"`
	Containers     []obsContainer  `json:"containers"`
	ProviderErrors []providerError `json:"provider_errors"`
}

// observationID is "obs:" + the first 16 hex digits of sha256 over the
// canonical observation (endpoints with owners, containers and provider
// errors, plus the Docker context) and the request it answers. It contains no
// time, so the same world and the same request always give the same id.
func observationID(request any, eps []endpointObs, dockerContext string) string {
	canon := struct {
		Request       any           `json:"request"`
		DockerContext string        `json:"docker_context"`
		Endpoints     []obsEndpoint `json:"endpoints"`
	}{Request: request, DockerContext: dockerContext, Endpoints: make([]obsEndpoint, 0, len(eps))}
	for _, e := range eps {
		oe := obsEndpoint{
			Protocol: e.Protocol, Port: e.Port, Visibility: e.Visibility, Occupied: e.Occupied, Unattributed: e.Unattributed,
			Owners: []obsOwner{}, Containers: []obsContainer{}, ProviderErrors: append([]providerError{}, e.ProviderErrors...),
		}
		for _, o := range e.Owners {
			oe.Owners = append(oe.Owners, obsOwner{PID: o.PID, Name: o.Name, StartTime: o.StartTime, Strength: o.Strength})
		}
		for _, c := range e.Containers {
			oe.Containers = append(oe.Containers, obsContainer{ID: c.ID, Name: c.Name})
		}
		canon.Endpoints = append(canon.Endpoints, oe)
	}
	data, err := json.Marshal(canon)
	if err != nil {
		panic("killport: cannot encode observation: " + err.Error())
	}
	sum := sha256.Sum256(data)
	return "obs:" + hex.EncodeToString(sum[:])[:16]
}
