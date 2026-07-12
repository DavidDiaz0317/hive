//go:build !windows

package integrated

import "syscall"

func productionLeaseOwnerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
