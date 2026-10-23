package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

const (
	processTerminate            = 0x0001
	processQueryLimitedInfo     = 0x1000
	processSynchronize          = 0x00100000
	errorInvalidParameterWindow = syscall.Errno(87)
)

// openForIdentity opens pid with the rights needed to read its creation time
// and to terminate it; the same handle is used for both so the target cannot
// change in between.
func openForIdentity(pid int) (syscall.Handle, error) {
	// WaitForSingleObject (used by handleStartTime) requires SYNCHRONIZE.
	h, err := syscall.OpenProcess(processTerminate|processQueryLimitedInfo|processSynchronize, false, uint32(pid))
	if err != nil {
		switch {
		case errors.Is(err, errorInvalidParameterWindow):
			return 0, errProcessNotFound
		case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
			return 0, fmt.Errorf("opening process %d: %w", pid, os.ErrPermission)
		}
		return 0, fmt.Errorf("opening process %d: %w", pid, err)
	}
	return h, nil
}

// processExited reports whether the process behind h has exited. A process
// handle is signalled exactly when the process has terminated, so a zero-time
// wait is the reliable test. (GetExitCodeProcess == STILL_ACTIVE, 259, is not:
// a process may legitimately exit with code 259.)
func processExited(h syscall.Handle) (bool, error) {
	ev, err := syscall.WaitForSingleObject(h, 0)
	switch ev {
	case syscall.WAIT_OBJECT_0:
		return true, nil
	case syscall.WAIT_TIMEOUT:
		return false, nil
	}
	if err == nil {
		err = fmt.Errorf("unexpected wait result %d", ev)
	}
	return false, err
}

// handleStartTime returns the creation FILETIME (decimal 100ns ticks) of the
// process behind h, or errProcessNotFound if it has already exited (a handle
// to an exited process stays valid until closed).
func handleStartTime(h syscall.Handle, pid int) (string, error) {
	exited, err := processExited(h)
	if err != nil {
		return "", fmt.Errorf("querying process %d: %w", pid, err)
	}
	if exited {
		return "", errProcessNotFound
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return "", fmt.Errorf("querying process %d: %w", pid, err)
	}
	return strconv.FormatUint(uint64(creation.HighDateTime)<<32|uint64(creation.LowDateTime), 10), nil
}

func platformProcessIdentity(pid int) (procIdentity, error) {
	if err := validSignalPID(pid); err != nil {
		return procIdentity{}, err
	}
	h, err := openForIdentity(pid)
	if err != nil {
		return procIdentity{}, err
	}
	defer syscall.CloseHandle(h)
	st, err := handleStartTime(h, pid)
	if err != nil {
		return procIdentity{}, err
	}
	return procIdentity{PID: pid, StartTime: st, Strength: strengthHandle}, nil
}

// platformSignalIfSame ignores force: Windows has no graceful signal for an
// arbitrary process, the process is always terminated.
func platformSignalIfSame(pid int, startTime string, force bool) error {
	if err := validSignalPID(pid); err != nil {
		return err
	}
	h, err := openForIdentity(pid)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	cur, err := handleStartTime(h, pid)
	if err != nil {
		return err
	}
	if cur != startTime {
		return errIdentityChanged
	}
	if err := syscall.TerminateProcess(h, 1); err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			// TerminateProcess on a handle whose process exited since the
			// start-time check fails with ACCESS_DENIED, which is not a
			// permission problem: the process is simply gone.
			if exited, werr := processExited(h); werr == nil && exited {
				return errProcessNotFound
			}
			return fmt.Errorf("terminating PID %d: %w", pid, os.ErrPermission)
		}
		return fmt.Errorf("terminating PID %d: %w", pid, err)
	}
	return nil
}
