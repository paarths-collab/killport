package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/fatih/color"
)

func marker(c color.Attribute, s string) string {
	return color.New(c, color.Bold).Sprint(s)
}

func okMark() string     { return marker(color.FgGreen, "✔") }
func errMark() string    { return marker(color.FgRed, "✗") }
func warnMark() string   { return marker(color.FgYellow, "!") }
func infoMark() string   { return marker(color.FgBlue, "i") }
func debugLabel() string { return color.New(color.FgCyan).Sprint("DEBUG") }

// cliError is a fatal error with a machine-readable code and exit status.
type cliError struct {
	Code string // usage | missing_dependency | enumeration_failed
	Exit int
	Msg  string
}

func (e *cliError) Error() string { return e.Msg }

func usageErr(msg string) *cliError { return &cliError{Code: "usage", Exit: 2, Msg: msg} }

func writeJSON(w io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		data = []byte(`{"error":"failed to encode output","code":"internal"}`)
	}
	fmt.Fprintln(w, string(data))
}

// reportFatal prints a fatal error: a JSON object on stdout in --json mode
// (stdout is always valid JSON), a message on stderr otherwise.
func (a *app) reportFatal(e *cliError) {
	if a.opts.json {
		writeJSON(a.stdout, map[string]string{"error": e.Msg, "code": e.Code})
		return
	}
	fmt.Fprintf(a.stderr, "[%s] %s\n", errMark(), e.Msg)
}

// warn prints a warning to stderr in both modes.
func (a *app) warn(msg string) {
	fmt.Fprintf(a.stderr, "[%s] warning: %s\n", warnMark(), msg)
}

func containerName(r *killRow) string {
	return strings.TrimPrefix(r.ProcessName, "[Docker] ")
}

func procDesc(r *killRow) string {
	if r.PID == "" {
		return orUnknown(r.ProcessName)
	}
	return fmt.Sprintf("PID %s (%s)", r.PID, orUnknown(r.ProcessName))
}

// printKill writes the kill results: JSON array or human lines that say
// exactly what the JSON says.
func (a *app) printKill(rows []*killRow) {
	if a.opts.json {
		out := make([]KillResult, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.KillResult)
		}
		writeJSON(a.stdout, out)
		return
	}
	w := a.stdout
	// A killed/stopped row only says its own owner is gone; if another owner
	// of the same port failed, say that the port is not free.
	portHeld := map[string]bool{}
	for _, r := range rows {
		if r.Status == statusFailed {
			portHeld[r.Port] = true
		}
	}
	held := func(r *killRow) string {
		if portHeld[r.Port] {
			return fmt.Sprintf(" (port %s is still held by another owner, see below)", r.Port)
		}
		return ""
	}
	for _, r := range rows {
		switch r.Status {
		case statusFree:
			fmt.Fprintf(w, "[%s] Port %s is already free\n", warnMark(), r.Port)
		case statusPlanned:
			if r.Action == actionStop {
				fmt.Fprintf(w, "[%s] [dry-run] Would stop Docker container %s (%s) publishing port %s\n", infoMark(), containerName(r), r.ContainerID, r.Port)
			} else {
				fmt.Fprintf(w, "[%s] [dry-run] Would kill %s on port %s\n", infoMark(), procDesc(r), r.Port)
			}
		case statusKilled:
			fmt.Fprintf(w, "[%s] Killed %s on port %s%s\n", okMark(), procDesc(r), r.Port, held(r))
		case statusStopped:
			fmt.Fprintf(w, "[%s] Stopped Docker container %s (%s) publishing port %s%s\n", okMark(), containerName(r), r.ContainerID, r.Port, held(r))
		default:
			fmt.Fprintf(w, "[%s] Port %s: %s\n", errMark(), r.Port, r.Error)
		}
	}
}

// printCheck writes check results: JSON array or grouped human output.
func (a *app) printCheck(rows []CheckResult, filtered bool) {
	if a.opts.json {
		writeJSON(a.stdout, rows)
		return
	}
	w := a.stdout
	if len(rows) == 0 {
		fmt.Fprintf(w, "[%s] No listening TCP ports were found.\n", warnMark())
		return
	}
	if filtered {
		fmt.Fprintf(w, "[%s] Here is what is currently running on your requested ports:\n", infoMark())
	} else {
		fmt.Fprintf(w, "[%s] Listening TCP ports:\n", infoMark())
	}
	buckets := []struct {
		name string
		cat  string
	}{
		{"System processes", catSystem},
		{"Application processes", catApplication},
		{"Code and runtime processes", catRuntime},
		{"Unidentified processes", catUnknown},
	}
	for _, b := range buckets {
		var entries []CheckResult
		for _, r := range rows {
			if !r.NoListener && r.Error == "" && r.Category == b.cat {
				entries = append(entries, r)
			}
		}
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(w, "[%s] %s (%d):\n", infoMark(), b.name, len(entries))
		for _, r := range entries {
			verdict := "not recommended to stop"
			if r.SafeToKill {
				verdict = "likely safe to stop (name heuristic)"
			}
			who := orUnknown(r.ProcessName)
			if r.PID != "" {
				who += " (PID " + r.PID + ")"
			} else if !r.IsDocker {
				who += " (PID not visible)"
			}
			fmt.Fprintf(w, "[%s] Port %s is being used by %s — %s.\n", infoMark(), r.Port, who, verdict)
		}
	}
	for _, r := range rows {
		switch {
		case r.NoListener:
			fmt.Fprintf(w, "[%s] Port %s is currently free, and no process is listening on it.\n", warnMark(), r.Port)
		case r.Error != "":
			fmt.Fprintf(w, "[%s] Could not check port %s: %s\n", errMark(), r.Port, r.Error)
		}
	}
}
