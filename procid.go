package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseProcStat extracts the start time (field 22, in clock ticks since boot)
// from the contents of /proc/<pid>/stat. The command name (field 2) is wrapped
// in parentheses and may itself contain spaces and parentheses, so everything
// up to the LAST ')' is skipped before the remaining fields are split. It is a
// pure function so it is tested on every OS. The result is returned as the
// decimal string used as the opaque StartTime token.
func parseProcStat(data []byte) (string, error) {
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("malformed stat: no closing parenthesis")
	}
	// After the comm: field 3 (state) is index 0, so field 22 is index 19.
	fields := strings.Fields(s[i+1:])
	const idx = 22 - 3
	if len(fields) <= idx {
		return "", fmt.Errorf("malformed stat: %d fields after command, need %d", len(fields), idx+1)
	}
	st := fields[idx]
	if _, err := strconv.ParseUint(st, 10, 64); err != nil {
		return "", fmt.Errorf("malformed stat: start time %q is not a number", st)
	}
	return st, nil
}

// validSignalPID rejects PIDs that would address a process group or every
// process (0, negative) instead of one process.
func validSignalPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d", pid)
	}
	return nil
}
