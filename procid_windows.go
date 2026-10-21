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
	stillActive                 = 259
	errorInvalidParameterWindow = syscall.Errno(87)
)

// openForIdentity opens pid with the rights needed to read its creation time
// and to terminate it; the same handle is used for both so the target cannot
// change in between.
func openForIdentity(pid int) (syscall.Handle, error) {
	h, err := syscall.OpenProcess(processTerminate|processQueryLimitedInfo, false, uint32(pid))
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

// handleStartTime returns the creation FILETIME (decimal 100ns ticks) of the
// process behind h, or errProcessNotFound if it has already exited (a handle
// to an exited process stays valid until closed).
func handleStartTime(h syscall.Handle, pid int) (string, error) {
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return "", fmt.Errorf("querying process %d: %w", pid, err)
	}
	if code != stillActive {
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
			return fmt.Errorf("terminating PID %d: %w", pid, os.ErrPermission)
		}
		return fmt.Errorf("terminating PID %d: %w", pid, err)
	}
	return nil
}
