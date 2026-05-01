package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	Version    = "1.1.1"
	verbose    bool
	dryRun     bool
	jsonOutput bool
	checkOnly  bool
)

// The structure for AI Agents
type KillResult struct {
	Port        string `json:"port"`
	ProcessName string `json:"process_name,omitempty"`
	PID         string `json:"pid,omitempty"`
	Killed      bool   `json:"killed"`
	Error       string `json:"error,omitempty"`
	IsDocker    bool   `json:"is_docker"`
}

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
	successStr = color.New(color.FgGreen, color.Bold).SprintFunc()("✔")
	errorStr   = color.New(color.FgRed, color.Bold).SprintFunc()("✗")
	warnStr    = color.New(color.FgYellow, color.Bold).SprintFunc()("!")
	infoStr    = color.New(color.FgBlue, color.Bold).SprintFunc()("i")
	debugStr   = color.New(color.FgCyan).SprintFunc()("DEBUG")
)

// Thread-safe storage for our JSON results
var (
	resultsMutex sync.Mutex
	allResults   []KillResult
)

func addResult(res KillResult) {
	resultsMutex.Lock()
	allResults = append(allResults, res)
	resultsMutex.Unlock()

	// If we are NOT in JSON mode, print it beautifully in real-time
	if !jsonOutput {
		if res.Error != "" {
			fmt.Printf("[%s] Port %s: %s\n", errorStr, res.Port, res.Error)
		} else if !res.Killed {
			fmt.Printf("[%s] Port %s is already free\n", warnStr, res.Port)
		} else {
			processInfo := res.ProcessName
			if res.PID != "" {
				processInfo += " (PID " + res.PID + ")"
			}
			fmt.Printf("[%s] Killed port %s — %s\n", successStr, res.Port, processInfo)
		}
	}
}

type checkBucket struct {
	name    string
	entries []CheckResult
}

func debugLog(format string, args ...interface{}) {
	if verbose && !jsonOutput {
		fmt.Printf("[%s] %s\n", debugStr, fmt.Sprintf(format, args...))
	}
}

func execWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// Expands ranges like "3000-3005" into individual ports
func expandPorts(args []string) []string {
	var expanded []string
	for _, arg := range args {
		if strings.Contains(arg, "-") {
			parts := strings.Split(arg, "-")
			if len(parts) == 2 {
				start, err1 := strconv.Atoi(parts[0])
				end, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil && start <= end {
					for i := start; i <= end; i++ {
						expanded = append(expanded, strconv.Itoa(i))
					}
					continue
				}
			}
		}
		expanded = append(expanded, arg)
	}
	return expanded
}

func unique(args []string) []string {
	keys := make(map[string]bool)
	var list []string
	for _, entry := range args {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

func main() {
	var rootCmd = &cobra.Command{
		Use:     "killport [port...]",
		Version: Version,
		Short:   "Free up local ports by killing the processes (or Docker containers) attached to them.",
		Example: "killport 3000\nkillport 3000-3005\nkillport 8080 --json\nkillport --check\nkillport --check 3000 8080",
		Args: func(cmd *cobra.Command, args []string) error {
			if checkOnly {
				return nil
			}
			if len(args) < 1 {
				return fmt.Errorf("requires at least 1 port (or use --check to inspect)")
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			checkDependencies(checkOnly)

			if checkOnly {
				runCheckMode(args)
				return
			}

			// 1. Expand ranges (3000-3005) and remove duplicates
			expandedArgs := expandPorts(args)
			uniquePorts := unique(expandedArgs)

			var wg sync.WaitGroup
			for _, port := range uniquePorts {
				wg.Add(1)
				go func(p string) {
					defer wg.Done()
					processPort(p)
				}(port)
			}
			wg.Wait()

			// 2. Output JSON for AI Agents if requested
			if jsonOutput {
				jsonData, _ := json.MarshalIndent(allResults, "", "  ")
				fmt.Println(string(jsonData))
			}
		},
	}

	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose output for debugging")
	rootCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "Simulate killing processes without actually terminating them")
	rootCmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output results as machine-readable JSON (For AI Agents)")
	rootCmd.Flags().BoolVarP(&checkOnly, "check", "c", false, "Check which process is using port(s) without killing")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func checkDependencies(checkMode bool) {
	var deps []string
	if runtime.GOOS == "windows" {
		deps = []string{"netstat", "tasklist"}
		if !checkMode {
			deps = append(deps, "taskkill")
		}
	} else {
		deps = []string{"lsof", "ps"}
		if !checkMode {
			deps = append(deps, "kill")
		}
	}
	var missing []string
	for _, dep := range deps {
		if _, err := exec.LookPath(dep); err != nil {
			missing = append(missing, dep)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("[%s] Missing system binaries: %v\n", errorStr, strings.Join(missing, ", "))
		os.Exit(1)
	}
}

func runCheckMode(args []string) {
	results := []CheckResult{}
	dockerOwners := getDockerPortOwners()

	if len(args) == 0 {
		all, err := listAllListeningPorts()
		if err != nil {
			results = append(results, CheckResult{Error: fmt.Sprintf("Failed to list ports: %v", err)})
		} else {
			results = append(results, all...)
			appendDockerResults(&results, dockerOwners, nil)
		}
	} else {
		expandedArgs := unique(expandPorts(args))
		for _, port := range expandedArgs {
			num, err := strconv.Atoi(port)
			if err != nil || num < 1 || num > 65535 {
				results = append(results, CheckResult{Port: port, Error: "Invalid port number"})
				continue
			}

			dockerNames := dockerOwners[port]
			pids, pidErr := findPIDs(port)
			if pidErr != nil {
				results = append(results, CheckResult{Port: port, Error: fmt.Sprintf("Failed to find process: %v", pidErr)})
				continue
			}

			if len(pids) == 0 && len(dockerNames) == 0 {
				results = append(results, CheckResult{Port: port, NoListener: true})
				continue
			}

			for _, name := range dockerNames {
				cat := classifyProcess(name, true)
				results = append(results, CheckResult{Port: port, ProcessName: name, Category: cat, IsDocker: true})
			}

			for _, pid := range pids {
				pname := findProcessName(pid)
				pcat := classifyProcess(pname, false)
				results = append(results, CheckResult{
					Port:        port,
					PID:         pid,
					ProcessName: pname,
					Category:    pcat,
				})
			}
		}
	}

	sortCheckResults(results)

	// Ensure each entry has a category and safe-to-kill flag
	setSafeFlags(results)

	if jsonOutput {
		jsonData, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(jsonData))
		return
	}

	if len(results) == 0 {
		fmt.Printf("[%s] No listening ports were found on this machine.\n", warnStr)
		return
	}

	fmt.Printf("[%s] Here is what is currently running on your requested ports:\n", infoStr)
	buckets := groupCheckResults(results)

	for _, bucket := range buckets {
		if len(bucket.entries) == 0 {
			continue
		}

		fmt.Printf("[%s] %s (%d):\n", infoStr, bucket.name, len(bucket.entries))
		for _, res := range bucket.entries {
			if res.PID != "" {
				safe := "unsafe to kill"
				if res.SafeToKill {
					safe = "safe to kill"
				}
				fmt.Printf("[%s] Port %s is being used by %s (PID %s) — %s.\n", infoStr, res.Port, res.ProcessName, res.PID, safe)
			} else {
				safe := "unsafe to kill"
				if res.SafeToKill {
					safe = "safe to kill"
				}
				fmt.Printf("[%s] Port %s is being used by %s — %s.\n", infoStr, res.Port, res.ProcessName, safe)
			}
		}
	}

	for _, res := range results {
		if res.Error == "" && !res.NoListener {
			continue
		}
		if res.NoListener {
			fmt.Printf("[%s] Port %s is currently free, and no process is listening on it.\n", warnStr, res.Port)
			continue
		}
		if res.Port == "" {
			fmt.Printf("[%s] Could not complete the check: %s\n", errorStr, res.Error)
			continue
		}
		fmt.Printf("[%s] Could not check port %s: %s\n", errorStr, res.Port, res.Error)
	}
}

func appendDockerResults(results *[]CheckResult, dockerOwners map[string][]string, filter map[string]bool) {
	for port, names := range dockerOwners {
		if filter != nil && !filter[port] {
			continue
		}
		for _, name := range names {
			*results = append(*results, CheckResult{Port: port, ProcessName: name, Category: classifyProcess(name, true), IsDocker: true})
		}
	}
}

func listAllListeningPorts() ([]CheckResult, error) {
	if runtime.GOOS == "windows" {
		return listAllListeningPortsWindows()
	}
	return listAllListeningPortsUnix()
}

func listAllListeningPortsWindows() ([]CheckResult, error) {
	out, err := execWithTimeout(4*time.Second, "netstat", "-ano")
	if err != nil {
		return nil, err
	}

	results := []CheckResult{}
	seen := make(map[string]bool)
	pidToName := make(map[string]string)

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		proto := strings.ToUpper(fields[0])
		if proto != "TCP" && proto != "UDP" {
			continue
		}

		if proto == "TCP" && !strings.Contains(strings.ToUpper(line), "LISTEN") {
			continue
		}

		localAddr := fields[1]
		pid := strings.TrimSpace(fields[len(fields)-1])
		if pid == "" || pid == "0" {
			continue
		}

		port := extractPort(localAddr)
		if port == "" {
			continue
		}

		key := port + "|" + pid
		if seen[key] {
			continue
		}
		seen[key] = true

		name, ok := pidToName[pid]
		if !ok {
			name = findProcessName(pid)
			pidToName[pid] = name
		}

		results = append(results, CheckResult{Port: port, PID: pid, ProcessName: name, Category: classifyProcess(name, false)})
	}

	return results, nil
}

func listAllListeningPortsUnix() ([]CheckResult, error) {
	out, err := execWithTimeout(4*time.Second, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpcn")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return []CheckResult{}, nil
		}
		return nil, err
	}

	results := []CheckResult{}
	seen := make(map[string]bool)
	currentPID := ""
	currentName := "unknown"

	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}

		switch line[0] {
		case 'p':
			currentPID = strings.TrimSpace(line[1:])
			currentName = "unknown"
		case 'c':
			currentName = strings.TrimSpace(line[1:])
		case 'n':
			port := extractPort(strings.TrimSpace(line[1:]))
			if port == "" || currentPID == "" {
				continue
			}

			key := port + "|" + currentPID
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, CheckResult{Port: port, PID: currentPID, ProcessName: currentName})
			results[len(results)-1].Category = classifyProcess(currentName, false)
		}
	}

	return results, nil
}

func extractPort(addr string) string {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return ""
	}

	trimmed = strings.Trim(trimmed, "[]")
	idx := strings.LastIndex(trimmed, ":")
	if idx == -1 || idx == len(trimmed)-1 {
		return ""
	}

	port := trimmed[idx+1:]
	if _, err := strconv.Atoi(port); err != nil {
		return ""
	}

	return port
}

func sortCheckResults(results []CheckResult) {
	sort.Slice(results, func(i, j int) bool {
		ci := categoryRank(results[i].Category)
		cj := categoryRank(results[j].Category)
		if ci != cj {
			return ci < cj
		}

		pi, errI := strconv.Atoi(results[i].Port)
		pj, errJ := strconv.Atoi(results[j].Port)

		if errI == nil && errJ == nil && pi != pj {
			return pi < pj
		}
		if results[i].Port != results[j].Port {
			return results[i].Port < results[j].Port
		}

		if results[i].PID != results[j].PID {
			return results[i].PID < results[j].PID
		}

		return results[i].ProcessName < results[j].ProcessName
	})
}

func groupCheckResults(results []CheckResult) []checkBucket {
	buckets := []checkBucket{
		{name: "System processes"},
		{name: "Application processes"},
		{name: "Code and runtime processes"},
	}

	for _, res := range results {
		if res.Error != "" {
			continue
		}
		switch res.Category {
		case "system":
			buckets[0].entries = append(buckets[0].entries, res)
		case "runtime":
			buckets[2].entries = append(buckets[2].entries, res)
		default:
			buckets[1].entries = append(buckets[1].entries, res)
		}
	}

	return buckets
}

func classifyProcess(processName string, isDocker bool) string {
	if isDocker {
		return "application"
	}

	name := strings.ToLower(strings.TrimSpace(processName))
	if name == "" {
		return "application"
	}

	if strings.HasPrefix(name, "[docker]") {
		return "application"
	}

	systemHints := []string{
		"system",
		"svchost",
		"services.exe",
		"wininit",
		"lsass",
		"spoolsv",
		"launchd",
		"systemd",
		"dbus-daemon",
		"cupsd",
		"cron",
		"init",
		"kthreadd",
		"kworker",
		"kernel_task",
		"windowserver",
	}
	for _, hint := range systemHints {
		if strings.Contains(name, hint) {
			return "system"
		}
	}

	runtimeHints := []string{
		"python",
		"python3",
		"node",
		"npm",
		"npx",
		"pnpm",
		"yarn",
		"bun",
		"deno",
		"uvicorn",
		"gunicorn",
		"django",
		"flask",
		"php",
		"ruby",
		"java",
		"dotnet",
		"go",
		"cargo",
		"rust",
		"ts-node",
		"vite",
		"webpack",
		"parcel",
		"next",
		"nuxt",
		"antigravity",
		"language_server",
	}
	for _, hint := range runtimeHints {
		if strings.Contains(name, hint) {
			return "runtime"
		}
	}

	return "application"
}

func categoryRank(category string) int {
	switch category {
	case "system":
		return 0
	case "application":
		return 1
	case "runtime":
		return 2
	default:
		return 3
	}
}

func safeToKill(category, pid string, isDocker bool) bool {
	if isDocker {
		return true
	}
	// Windows System PID is often 4, avoid killing it
	if pid == "4" || pid == "0" {
		return false
	}
	if category == "system" {
		return false
	}
	return true
}

func setSafeFlags(results []CheckResult) {
	for i := range results {
		// ensure category exists
		if results[i].Category == "" {
			results[i].Category = classifyProcess(results[i].ProcessName, results[i].IsDocker)
		}
		results[i].SafeToKill = safeToKill(results[i].Category, results[i].PID, results[i].IsDocker)
	}
}

func findDockerContainerByPort(port string) (bool, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, ""
	}

	out, err := execWithTimeout(3*time.Second, "docker", "ps", "--format", "{{.Names}}|{{.Ports}}")
	if err != nil {
		return false, ""
	}

	target := ":" + port + "->"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.Contains(line, target) {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) > 0 {
			return true, "[Docker] " + parts[0]
		}
	}

	return false, ""
}

func getDockerPortOwners() map[string][]string {
	owners := make(map[string][]string)
	if _, err := exec.LookPath("docker"); err != nil {
		return owners
	}

	out, err := execWithTimeout(3*time.Second, "docker", "ps", "--format", "{{.Names}}|{{.Ports}}")
	if err != nil {
		return owners
	}

	portRe := regexp.MustCompile(`:(\d+)->`)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 2)
		if len(parts) < 2 {
			continue
		}

		container := strings.TrimSpace(parts[0])
		portSpec := parts[1]
		if container == "" || portSpec == "" {
			continue
		}

		matches := portRe.FindAllStringSubmatch(portSpec, -1)
		seen := make(map[string]bool)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			port := m[1]
			if seen[port] {
				continue
			}
			seen[port] = true
			owners[port] = append(owners[port], "[Docker] "+container)
		}
	}

	return owners
}

// Docker Detection Feature
func stopDockerContainer(port string) (bool, string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "", nil // Docker not installed
	}

	out, err := execWithTimeout(3*time.Second, "docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}")
	if err != nil {
		return false, "", nil
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	target := ":" + port + "->"

	for _, line := range lines {
		if strings.Contains(line, target) {
			parts := strings.Split(line, "|")
			if len(parts) >= 2 {
				containerID := parts[0]
				name := "[Docker] " + parts[1]

				if dryRun {
					return true, name, nil
				}
				_, stopErr := execWithTimeout(10*time.Second, "docker", "stop", containerID)
				return true, name, stopErr
			}
		}
	}
	return false, "", nil
}

func processPort(port string) {
	num, err := strconv.Atoi(port)
	if err != nil || num < 1 || num > 65535 {
		addResult(KillResult{Port: port, Error: "Invalid port number"})
		return
	}

	// 1. Try Docker First
	isDocker, name, err := stopDockerContainer(port)
	if isDocker {
		if err != nil {
			addResult(KillResult{Port: port, ProcessName: name, Killed: false, IsDocker: true, Error: err.Error()})
		} else {
			addResult(KillResult{Port: port, ProcessName: name, Killed: true, IsDocker: true})
		}
		return
	}

	// 2. Try OS-Level Processes
	pids, err := findPIDs(port)
	if err != nil {
		addResult(KillResult{Port: port, Error: fmt.Sprintf("Failed to find process: %v", err)})
		return
	}
	if len(pids) == 0 {
		addResult(KillResult{Port: port, Killed: false})
		return
	}

	for _, pid := range pids {
		procName := findProcessName(pid)
		killErr := killPID(pid)

		if killErr != nil {
			addResult(KillResult{Port: port, ProcessName: procName, PID: pid, Killed: false, Error: "Requires sudo/Admin"})
		} else {
			addResult(KillResult{Port: port, ProcessName: procName, PID: pid, Killed: !dryRun})
		}
	}
}

// Spec 2 functions (existing logic)
func findPIDs(port string) ([]string, error) {
	if runtime.GOOS == "windows" {
		out, err := execWithTimeout(3*time.Second, "netstat", "-ano")
		if err != nil {
			return nil, err
		}
		var pids []string
		seen := make(map[string]bool)
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			localAddr := fields[1]
			if strings.HasSuffix(localAddr, ":"+port) {
				isTCP := strings.HasPrefix(line, "TCP")
				isListening := strings.Contains(strings.ToUpper(line), "LISTEN")
				if isTCP && !isListening {
					continue
				}
				pid := strings.TrimSpace(fields[len(fields)-1])
				if pid != "0" && !seen[pid] {
					pids = append(pids, pid)
					seen[pid] = true
				}
			}
		}
		return pids, nil
	}
	out, err := execWithTimeout(3*time.Second, "lsof", "-i", ":"+port, "-t")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var pids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			pids = append(pids, line)
		}
	}
	return pids, nil
}

func findProcessName(pid string) string {
	if runtime.GOOS == "windows" {
		out, _ := execWithTimeout(2*time.Second, "tasklist", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH")
		parts := strings.Split(string(out), ",")
		if len(parts) > 0 {
			return strings.Trim(parts[0], "\"")
		}
		return "unknown"
	}
	out, _ := execWithTimeout(2*time.Second, "ps", "-p", pid, "-o", "comm=")
	return strings.TrimSpace(string(out))
}

func killPID(pid string) error {
	if dryRun {
		return nil
	}

	if runtime.GOOS == "windows" {
		_, err := execWithTimeout(2*time.Second, "taskkill", "/F", "/PID", pid)
		return err
	}
	_, err := execWithTimeout(2*time.Second, "kill", "-9", pid)
	return err
}
