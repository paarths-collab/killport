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

// The pidfd and kill syscalls and the start-time read are variables so tests
// can simulate a kernel or sandbox without pidfd, a pidfd_send_signal blocked
// by seccomp, and a PID that is reused in between.
var (
	pidfdOpen       = func(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }
	pidfdSendSignal = func(fd int, sig unix.Signal) error { return unix.PidfdSendSignal(fd, sig, nil, 0) }
	pidfdClose      = unix.Close
	killPID         = func(pid int, sig unix.Signal) error { return unix.Kill(pid, sig) }
	procStartTime   = linuxStartTime
)

// compareThenKill is the weaker identity-checked path: re-read the start
// time, compare it with the planned one, and only then kill(2). A tiny race
// between the compare and the kill remains; the compare itself is never
// skipped.
func compareThenKill(pid int, startTime string, sig unix.Signal) error {
	cur, err := procStartTime(pid)
	if err != nil {
		return err
	}
	if cur != startTime {
		return errIdentityChanged
	}
	return mapSignalErr(pid, killPID(pid, sig))
}

// platformSignalIfSame binds the signal to the process via a pidfd: the pidfd
// is opened first, then the start time is re-read, so a PID that is reused
// after the open can never receive the signal (the pidfd keeps pointing at the
// original process). Without pidfd support (pidfd_open or pidfd_send_signal
// missing, ENOSYS, or blocked by a seccomp profile with EPERM) it falls back
// to compare + kill, which checks the identity again.
func platformSignalIfSame(pid int, startTime string, force bool) error {
	if err := validSignalPID(pid); err != nil {
		return err
	}
	sig := unixSignal(force)
	fd, err := pidfdOpen(pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errProcessNotFound
		}
		// ENOSYS (old kernel) or blocked by a sandbox: weaker but still
		// identity-checked path.
		return compareThenKill(pid, startTime, sig)
	}
	defer pidfdClose(fd)
	cur, err := procStartTime(pid)
	if err != nil {
		return err
	}
	if cur != startTime {
		return errIdentityChanged
	}
	err = pidfdSendSignal(fd, sig)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		// pidfd_send_signal itself is unavailable (ENOSYS) or filtered by a
		// seccomp profile (EPERM). A genuine EPERM (not allowed to signal that
		// process) is reproduced by kill(2) and reported as a permission error.
		return compareThenKill(pid, startTime, sig)
	}
	return mapSignalErr(pid, err)
}
