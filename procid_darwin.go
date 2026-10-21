package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// darwinStartTime returns "sec.usec" of the process start from the kernel
// process table.
func darwinStartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		switch {
		case errors.Is(err, unix.ESRCH), errors.Is(err, unix.ENOENT):
			return "", errProcessNotFound
		case errors.Is(err, unix.EPERM):
			return "", fmt.Errorf("reading process %d: %w", pid, os.ErrPermission)
		case errors.Is(err, unix.EIO):
			// The kernel returns an empty record for an unknown PID, which the
			// library reports as EIO. Confirm with signal 0.
			if kerr := unix.Kill(pid, 0); errors.Is(kerr, unix.ESRCH) {
				return "", errProcessNotFound
			}
		}
		return "", fmt.Errorf("reading process %d: %w", pid, err)
	}
	if int(kp.Proc.P_pid) != pid {
		return "", errProcessNotFound
	}
	return formatDarwinStart(int64(kp.Proc.P_starttime.Sec), int64(kp.Proc.P_starttime.Usec)), nil
}

func formatDarwinStart(sec, usec int64) string {
	return fmt.Sprintf("%d.%06d", sec, usec)
}

func platformProcessIdentity(pid int) (procIdentity, error) {
	if err := validSignalPID(pid); err != nil {
		return procIdentity{}, err
	}
	st, err := darwinStartTime(pid)
	if err != nil {
		return procIdentity{}, err
	}
	return procIdentity{PID: pid, StartTime: st, Strength: strengthStartTime}, nil
}

// platformSignalIfSame compares the start time then signals. macOS has no
// pidfd, so a very small window between the two steps remains.
func platformSignalIfSame(pid int, startTime string, force bool) error {
	if err := validSignalPID(pid); err != nil {
		return err
	}
	cur, err := darwinStartTime(pid)
	if err != nil {
		return err
	}
	if cur != startTime {
		return errIdentityChanged
	}
	sig := unix.SIGTERM
	if force {
		sig = unix.SIGKILL
	}
	err = unix.Kill(pid, sig)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ESRCH):
		return errProcessNotFound
	case errors.Is(err, unix.EPERM):
		return fmt.Errorf("signalling PID %d: %w", pid, os.ErrPermission)
	}
	return fmt.Errorf("signalling PID %d: %w", pid, err)
}
