package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Listener is one TCP LISTEN socket owner, matched by its exact LOCAL port.
// UDP sockets and established connections are out of scope on every platform.
type Listener struct {
	Port    int
	PID     int // 0 when the owner is not visible to the current user
	Process string
	Addr    string
}

const toolTimeout = 15 * time.Second

var (
	lsofArgs = []string{"-nP", "+c", "0", "-iTCP", "-sTCP:LISTEN", "-FpcnT"}
	ssArgs   = []string{"-ltnp"}
)

func hasTool(sys system, name string) bool {
	_, err := sys.LookPath(name)
	return err == nil
}

// missingTools returns the system tools required on this platform that are
// not installed. Docker is optional and never listed.
func missingTools(sys system) []string {
	var missing []string
	switch sys.GOOS() {
	case "windows":
		for _, t := range []string{"netstat", "tasklist"} {
			if !hasTool(sys, t) {
				missing = append(missing, t)
			}
		}
	case "linux":
		if !hasTool(sys, "lsof") && !hasTool(sys, "ss") {
			missing = append(missing, "lsof (or ss)")
		}
	default:
		if !hasTool(sys, "lsof") {
			missing = append(missing, "lsof")
		}
	}
	return missing
}

// listing is the result of enumerating listeners: the listeners themselves,
// non-fatal warnings to show on stderr, and whether process names could not
// be resolved (Windows, when tasklist fails).
type listing struct {
	Listeners        []Listener
	Warnings         []string
	NamesUnavailable bool
}

// listListeners enumerates every visible TCP listener. This is the single
// code path used by both --check and kill mode.
func listListeners(ctx context.Context, sys system) (listing, error) {
	if sys.GOOS() == "windows" {
		return listWindows(ctx, sys)
	}
	haveSS := sys.GOOS() == "linux" && hasTool(sys, "ss")
	if hasTool(sys, "lsof") {
		return listUnix(ctx, sys, haveSS)
	}
	if haveSS {
		ls, err := listSS(ctx, sys)
		return listing{Listeners: ls}, err
	}
	return listing{}, fmt.Errorf("no supported port inspection tool found (need lsof)")
}

// listUnix runs lsof and, on Linux where ss exists, also ss: lsof only shows
// sockets of processes the current user may inspect, so ports that ss sees
// but lsof does not are added as listeners with an invisible owner (PID 0).
// That keeps a root-owned listener from being reported as "already free".
func listUnix(ctx context.Context, sys system, haveSS bool) (listing, error) {
	ls, lsofWarn, err := listLsof(ctx, sys)
	if err != nil {
		return listing{}, err
	}
	lst := listing{Listeners: ls}
	if !haveSS {
		if lsofWarn != "" {
			lst.Warnings = append(lst.Warnings, lsofWarn)
		}
		return lst, nil
	}
	sls, serr := listSS(ctx, sys)
	if serr != nil {
		if lsofWarn != "" {
			lst.Warnings = append(lst.Warnings, lsofWarn)
		}
		lst.Warnings = append(lst.Warnings, fmt.Sprintf("%v; listeners owned by other users may be missing from the results", serr))
		return lst, nil
	}
	shown := make(map[int]bool, len(ls))
	for _, l := range ls {
		shown[l.Port] = true
	}
	merged := append([]Listener(nil), ls...)
	for _, l := range sls {
		if !shown[l.Port] {
			merged = append(merged, l)
		}
	}
	lst.Listeners = normalizeListeners(merged)
	return lst, nil
}

// listLsof runs lsof. Exit status 1 with empty stdout and empty stderr means
// "nothing matched". Exit status 1 with empty stdout but a stderr message is
// not trusted as "no listeners": the second return value is a warning.
func listLsof(ctx context.Context, sys system) ([]Listener, string, error) {
	out, err := runTool(ctx, sys, toolTimeout, "lsof", lsofArgs...)
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
			return nil, "", nil // lsof: nothing matched
		case hasCode && code == 1 && empty:
			return nil, fmt.Sprintf("lsof exited with status 1 and no output but reported: %s; its result may be incomplete", stderr), nil
		case hasCode && code == 1:
			// lsof exits 1 when it merely warned about unreadable
			// entries; the listing it produced is still valid.
		default:
			return nil, "", fmt.Errorf("lsof failed: %w", err)
		}
	}
	return normalizeListeners(parseLsof(string(out))), "", nil
}

func listSS(ctx context.Context, sys system) ([]Listener, error) {
	out, err := runTool(ctx, sys, toolTimeout, "ss", ssArgs...)
	if err != nil {
		return nil, fmt.Errorf("ss failed: %w", err)
	}
	return normalizeListeners(parseSS(string(out))), nil
}

func listWindows(ctx context.Context, sys system) (listing, error) {
	out, err := runTool(ctx, sys, toolTimeout, "netstat", "-ano")
	if err != nil {
		return listing{}, fmt.Errorf("netstat failed: %w", err)
	}
	lst := listing{Listeners: normalizeListeners(parseNetstat(string(out)))}
	if len(lst.Listeners) == 0 {
		return lst, nil
	}
	tout, terr := runTool(ctx, sys, toolTimeout, "tasklist", "/FO", "CSV", "/NH")
	if terr != nil {
		// Without names Docker's own processes cannot be recognised, so
		// kill mode refuses nameless PIDs.
		lst.NamesUnavailable = true
		lst.Warnings = append(lst.Warnings, fmt.Sprintf("tasklist failed (%v); process names are unavailable and listeners will not be killed", terr))
		return lst, nil
	}
	names := parseTasklist(string(tout))
	for i := range lst.Listeners {
		lst.Listeners[i].Process = names[lst.Listeners[i].PID]
	}
	return lst, nil
}

// normalizeListeners de-duplicates by (port, pid) and sorts by port, pid.
func normalizeListeners(in []Listener) []Listener {
	type key struct{ port, pid int }
	seen := make(map[key]bool)
	out := make([]Listener, 0, len(in))
	for _, l := range in {
		k := key{l.Port, l.PID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].PID < out[j].PID
	})
	return out
}

func groupByPort(ls []Listener) map[int][]Listener {
	m := make(map[int][]Listener)
	for _, l := range ls {
		m[l.Port] = append(m[l.Port], l)
	}
	return m
}

// splitHostPort splits "host:port" at the last colon; host may be empty,
// "*", an IPv4/IPv6 address, bracketed, or carry a "%iface" scope.
func splitHostPort(addr string) (host string, port int, ok bool) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return "", 0, false
	}
	p, err := strconv.Atoi(addr[idx+1:])
	if err != nil || p < 1 || p > maxPort || addr[idx+1] == '+' || addr[idx+1] == '-' {
		return "", 0, false
	}
	return addr[:idx], p, true
}

// parseLsof parses `lsof -FpcnT` output. Only entries that are TCP listening
// sockets are returned: connection entries ("a->b") and entries whose TST
// state is not LISTEN are skipped.
func parseLsof(out string) []Listener {
	var (
		res      []Listener
		seen     = make(map[[2]int]bool)
		pid      int
		haveP    bool
		command  string
		fileName string
		fileSt   string
	)
	flush := func() {
		name, st := fileName, fileSt
		fileName, fileSt = "", ""
		if name == "" || !haveP {
			return
		}
		if strings.Contains(name, "->") {
			return
		}
		if st != "" && !strings.EqualFold(st, "LISTEN") {
			return
		}
		local := name
		if i := strings.IndexByte(local, ' '); i >= 0 {
			local = local[:i]
		}
		_, port, ok := splitHostPort(local)
		if !ok {
			return
		}
		k := [2]int{port, pid}
		if seen[k] {
			return
		}
		seen[k] = true
		res = append(res, Listener{Port: port, PID: pid, Process: command, Addr: local})
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		val := line[1:]
		switch line[0] {
		case 'p':
			flush()
			n, err := strconv.Atoi(strings.TrimSpace(val))
			haveP = err == nil && n > 0
			pid = n
			command = ""
		case 'c':
			command = strings.TrimSpace(val)
		case 'f':
			flush()
		case 'n':
			// A second name means the previous entry is complete.
			if fileName != "" {
				flush()
			}
			fileName = strings.TrimSpace(val)
		case 'T':
			if strings.HasPrefix(val, "ST=") {
				// lsof prints the name before the state, but accept the
				// reverse order too: a state that arrives when the
				// current entry already has one starts the next entry.
				if fileSt != "" {
					flush()
				}
				fileSt = strings.TrimSpace(val[3:])
			}
		}
	}
	flush()
	return res
}

var ssUserRe = regexp.MustCompile(`\("((?:[^"\\]|\\.)*)",pid=(\d+)`)

// parseSS parses `ss -ltnp` output. Rows without a users column (sockets of
// other users when not root) are kept as PID 0 listeners so the port is still
// reported as in use.
func parseSS(out string) []Listener {
	var res []Listener
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "LISTEN") {
			continue
		}
		_, port, ok := splitHostPort(fields[3])
		if !ok {
			continue
		}
		matched := false
		if idx := strings.Index(line, "users:("); idx >= 0 {
			for _, m := range ssUserRe.FindAllStringSubmatch(line[idx:], -1) {
				pid, err := strconv.Atoi(m[2])
				if err != nil {
					continue
				}
				matched = true
				res = append(res, Listener{Port: port, PID: pid, Process: m[1], Addr: fields[3]})
			}
		}
		if !matched {
			res = append(res, Listener{Port: port, Addr: fields[3]})
		}
	}
	return res
}

var nonListeningStates = setOf(
	"BOUND", "CLOSED", "SYN_SENT", "SYN_RECEIVED", "ESTABLISHED", "TIME_WAIT",
	"CLOSE_WAIT", "FIN_WAIT_1", "FIN_WAIT_2", "LAST_ACK", "CLOSING", "DELETE_TCB",
)

func isWildcardHost(h string) bool {
	return h == "0.0.0.0" || h == "[::]" || h == "*" || h == "::"
}

// parseNetstat parses `netstat -ano` output into TCP listeners (names are not
// available here). Rows are matched on their LOCAL port only; established and
// closing connections, UDP rows and PID 0 rows are excluded.
func parseNetstat(out string) []Listener {
	var res []Listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") {
			continue
		}
		_, port, ok := splitHostPort(f[1])
		if !ok {
			continue
		}
		fhost, fport := "", -1
		if i := strings.LastIndex(f[2], ":"); i >= 0 {
			fhost = f[2][:i]
			if n, err := strconv.Atoi(f[2][i+1:]); err == nil {
				fport = n
			}
		}
		// The wildcard-foreign fallback covers localized LISTENING names,
		// but never a state Windows names as non-listening (e.g. BOUND).
		listening := strings.EqualFold(f[3], "LISTENING") ||
			(fport == 0 && isWildcardHost(fhost) && !nonListeningStates[strings.ToUpper(f[3])])
		if !listening {
			continue
		}
		pid, err := strconv.Atoi(f[len(f)-1])
		if err != nil || pid <= 0 {
			continue
		}
		res = append(res, Listener{Port: port, PID: pid, Addr: f[1]})
	}
	return res
}

// parseTasklist parses `tasklist /FO CSV /NH` into pid -> image name.
// Lines that are not CSV process rows (e.g. "INFO: No tasks...") are ignored.
func parseTasklist(out string) map[int]string {
	names := make(map[int]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := csv.NewReader(strings.NewReader(line))
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		rec, err := r.Read()
		if err != nil || len(rec) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil {
			continue
		}
		names[pid] = rec[0]
	}
	return names
}
