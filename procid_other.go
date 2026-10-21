//go:build !linux && !darwin && !windows

package main

import "errors"

var errNoIdentity = errors.New("process identity is not supported on this platform")

func platformProcessIdentity(pid int) (procIdentity, error) {
	return procIdentity{PID: pid, Strength: strengthNone}, errNoIdentity
}

func platformSignalIfSame(pid int, startTime string, force bool) error {
	return errNoIdentity
}
