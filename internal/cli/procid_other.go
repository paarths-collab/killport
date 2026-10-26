//go:build !linux && !darwin && !windows

package cli

func platformProcessIdentity(pid int) (procIdentity, error) {
	return procIdentity{PID: pid, Strength: strengthNone}, errNoIdentity
}

func platformSignalIfSame(pid int, startTime string, force bool) error {
	return errNoIdentity
}
