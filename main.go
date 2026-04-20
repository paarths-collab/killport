package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	Version    = "dev"
	verbose    bool
	dryRun     bool
	jsonOutput bool
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

var (
	successStr = color.New(color.FgGreen, color.Bold).SprintFunc()("✔")
	errorStr   = color.New(color.FgRed, color.Bold).SprintFunc()("✗")
	warnStr    = color.New(color.FgYellow, color.Bold).SprintFunc()("!")
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
		Example: `  killport 3000
  killport 3000-3005
  killport 8080 --json`,
		Args: cobra.MinimumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			checkDependencies()

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

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func checkDependencies() {
	var deps []string
	if runtime.GOOS == "windows" {
		deps = []string{"netstat", "tasklist", "taskkill"}
	} else {
		deps = []string{"lsof", "kill", "ps"}
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
