package cli

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Categories assigned by the name heuristic.
const (
	catSystem      = "system"
	catRuntime     = "runtime"
	catApplication = "application"
	catUnknown     = "unknown"
)

// CheckResult is one row of --check output. SafeToKill is an advisory name
// heuristic, never a guarantee.
type CheckResult struct {
	Port        string `json:"port"`
	ProcessName string `json:"process_name,omitempty"`
	PID         string `json:"pid,omitempty"`
	Category    string `json:"category,omitempty"`
	SafeToKill  bool   `json:"safe_to_kill"`
	IsDocker    bool   `json:"is_docker,omitempty"`
	NoListener  bool   `json:"no_listener,omitempty"`
	Error       string `json:"error,omitempty"`
}

var (
	systemNames = setOf(
		"system", "svchost", "services", "wininit", "lsass", "spoolsv", "csrss", "smss",
		"launchd", "systemd", "systemd-resolved", "systemd-resolve", "systemd-networkd",
		"dbus-daemon", "cupsd", "cron", "crond", "init", "kthreadd", "kworker",
		"kernel_task", "windowserver", "sshd", "rpcbind", "avahi-daemon", "mdnsresponder",
		"controlcenter", "rapportd", "sharingd",
	)
	runtimeNames = setOf(
		"python", "node", "npm", "npx", "pnpm", "yarn", "bun", "deno", "uvicorn",
		"gunicorn", "django", "flask", "php", "ruby", "java", "dotnet", "go", "cargo",
		"rust", "rustc", "ts-node", "tsx", "vite", "webpack", "parcel", "next", "nuxt",
		"rails", "puma", "antigravity", "next-server", "next-router-worker",
	)
	runtimePrefixes = []string{"language_server"}
	systemPrefixes  = []string{"systemd-"}
	versionSuffix   = regexp.MustCompile(`[0-9][0-9.]*$`)
	// parenSuffix strips a trailing parenthesised note such as the
	// "(v14.2.3)" in the process title "next-server (v14.2.3)".
	parenSuffix = regexp.MustCompile(`\s*\([^)]*\)$`)
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// baseName normalises a process name for exact matching: lowercase, no .exe,
// no "/suffix" (kworker/0:1) and no trailing version digits (python3.12).
func baseName(name string) string {
	n := parenSuffix.ReplaceAllString(normProcName(name), "")
	if i := strings.IndexByte(n, '/'); i > 0 {
		n = n[:i]
	}
	if stripped := versionSuffix.ReplaceAllString(n, ""); stripped != "" {
		n = stripped
	}
	return n
}

// classifyProcess maps a process name to a category using exact matches on
// the normalised name (never substrings). An empty name is "unknown".
func classifyProcess(name string, isDocker bool) string {
	if isDocker {
		return catApplication
	}
	if strings.TrimSpace(name) == "" {
		return catUnknown
	}
	if isDockerInfra(name) {
		return catSystem
	}
	n := baseName(name)
	if systemNames[n] || systemNames[normProcName(name)] {
		return catSystem
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(n, p) {
			return catSystem
		}
	}
	if runtimeNames[n] {
		return catRuntime
	}
	for _, p := range runtimePrefixes {
		if strings.HasPrefix(n, p) {
			return catRuntime
		}
	}
	// A real name that is neither system nor runtime is an ordinary
	// application; only a missing name is "unknown".
	return catApplication
}

// safeToKill is an advisory heuristic: true only for a Docker container, or a
// real PID (> 1, not killport itself) with a recognised runtime/application
// name that is not system or Docker infrastructure.
func safeToKill(r CheckResult, ownPID int) bool {
	if r.NoListener || r.Error != "" {
		return false
	}
	if r.IsDocker {
		return r.Category == catApplication
	}
	pid, err := strconv.Atoi(r.PID)
	if err != nil || pid <= 1 || pid == ownPID {
		return false
	}
	if strings.TrimSpace(r.ProcessName) == "" || isDockerInfra(r.ProcessName) {
		return false
	}
	return r.Category == catRuntime || r.Category == catApplication
}

// buildCheckRows turns the shared enumeration into CheckResult rows, sorted by
// port, then containers, then PID. When ports is non-empty only those ports
// are reported and free ones get a no_listener row.
func buildCheckRows(ports []int, listeners []Listener, dk dockerState, ownPID int) []CheckResult {
	byPort := groupByPort(listeners)
	want := ports
	if len(want) == 0 {
		seen := map[int]bool{}
		for p := range byPort {
			seen[p] = true
		}
		for p := range dk.Owners {
			seen[p] = true
		}
		for p := range seen {
			want = append(want, p)
		}
		sort.Ints(want)
	}
	rows := []CheckResult{}
	for _, port := range want {
		ps := strconv.Itoa(port)
		conts := dk.Owners[port]
		ls := byPort[port]
		if len(conts) == 0 && len(ls) == 0 {
			rows = append(rows, CheckResult{Port: ps, NoListener: true})
			continue
		}
		for _, c := range conts {
			r := CheckResult{Port: ps, ProcessName: "[Docker] " + c.Name, IsDocker: true, Category: classifyProcess(c.Name, true)}
			r.SafeToKill = safeToKill(r, ownPID)
			rows = append(rows, r)
		}
		for _, l := range ls {
			r := CheckResult{Port: ps, ProcessName: l.Process, Category: classifyProcess(l.Process, false)}
			if l.PID > 0 {
				r.PID = strconv.Itoa(l.PID)
			}
			r.SafeToKill = safeToKill(r, ownPID)
			rows = append(rows, r)
		}
	}
	return rows
}
