package main

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// linuxStartTime reads the start time token of pid from /proc/<pid>/stat.
func linuxStartTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errProcessNotFound
		}
		return "", fmt.Errorf("reading process %d: %w", pid, err)
	}
	st, err := parseProcStat(data)
	if err != nil {
		return "", fmt.Errorf("process %d: %w", pid, err)
	}
	return st, nil
}

var (
	pidfdOnce sync.Once
	pidfdOK   bool
)

// pidfdSupported probes once whether pidfd_open works here (kernel < 5.3 and
// some seccomp profiles lack it).
func pidfdSupported() bool {
	pidfdOnce.Do(func() {
		fd, err := unix.PidfdOpen(os.Getpid(), 0)
		if err == nil {
			_ = unix.Close(fd)
			pidfdOK = true
		}
	})
	return pidfdOK
}

func platformProcessIdentity(pid int) (procIdentity, error) {
	if err := validSignalPID(pid); err != nil {
		return procIdentity{}, err
	}
	st, err := linuxStartTime(pid)
	if err != nil {
		return procIdentity{}, err
	}
	strength := strengthStartTime
	if pidfdSupported() {
		strength = strengthPidfd
	}
	return procIdentity{PID: pid, StartTime: st, Strength: strength}, nil
}

func unixSignal(force bool) unix.Signal {
	if force {
		return unix.SIGKILL
	}
	return unix.SIGTERM
}

// mapSignalErr normalises errors from kill/pidfd_send_signal.
func mapSignalErr(pid int, err error) error {
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

// platformSignalIfSame binds the signal to the process via a pidfd: the pidfd
// is opened first, then the start time is re-read, so a PID that is reused
// after the open can never receive the signal (the pidfd keeps pointing at the
// original process). Without pidfd support it falls back to compare + kill.
func platformSignalIfSame(pid int, startTime string, force bool) error {
	if err := validSignalPID(pid); err != nil {
		return err
	}
	sig := unixSignal(force)
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errProcessNotFound
		}
		// ENOSYS (old kernel) or blocked by a sandbox: weaker but still
		// identity-checked path.
		cur, serr := linuxStartTime(pid)
		if serr != nil {
			return serr
		}
		if cur != startTime {
			return errIdentityChanged
		}
		return mapSignalErr(pid, unix.Kill(pid, sig))
	}
	defer unix.Close(fd)
	cur, err := linuxStartTime(pid)
	if err != nil {
		return err
	}
	if cur != startTime {
		return errIdentityChanged
	}
	return mapSignalErr(pid, unix.PidfdSendSignal(fd, sig, nil, 0))
}
