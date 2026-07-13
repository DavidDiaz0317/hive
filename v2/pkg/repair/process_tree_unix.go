//go:build !windows

package repair

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func startRepairProcessTree(command *exec.Cmd) (func() error, error) {
	// A process group is best-effort lifecycle cleanup on Unix: a descendant
	// may deliberately create a new session. Integrity does not depend on this
	// boundary; Worker commits only the immutable, guarded candidate tree that
	// was sealed before validation, never mutable post-tool worktree bytes.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	pid := command.Process.Pid
	return func() error {
		waitErr := command.Wait()
		killErr := syscall.Kill(-pid, syscall.SIGKILL)
		var containmentErr error
		if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			containmentErr = killErr
		}
		if containmentErr != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("repair command result %v; process-tree containment failed: %w", waitErr, containmentErr))
		}
		return waitErr
	}, nil
}

// A Unix process group is best-effort containment: descendants can detach
// into another session. Keep the conservative snapshot deadline on restart.
var repairProcessTreeCrashContainmentGuaranteed = func() bool {
	return false
}
