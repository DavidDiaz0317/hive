//go:build windows

package repair

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func startRepairProcessTree(command *exec.Cmd) (func() error, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create repair command job: %w", err)
	}
	closeJob := true
	defer func() {
		if closeJob {
			_ = windows.CloseHandle(job)
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, fmt.Errorf("configure repair command job: %w", err)
	}
	// Start suspended so the executable cannot spawn a child before it is
	// assigned to the kill-on-close job. Resume only after exact containment.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED, HideWindow: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("open repair command for process-tree containment: %w", err)
	}
	assignErr := windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if assignErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("contain repair command process tree: %w", assignErr)
	}
	if err := resumePrimaryProcessThread(uint32(command.Process.Pid)); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = command.Wait()
		return nil, err
	}
	closeJob = false
	return func() error {
		waitErr := command.Wait()
		terminateErr := windows.TerminateJobObject(job, 1)
		emptyErr := waitForEmptyRepairJob(job)
		closeErr := windows.CloseHandle(job)
		var containmentErr error
		if terminateErr != nil {
			containmentErr = fmt.Errorf("terminate repair command descendants: %w", terminateErr)
		} else if emptyErr != nil {
			containmentErr = emptyErr
		} else if closeErr != nil {
			containmentErr = fmt.Errorf("close repair command job: %w", closeErr)
		}
		if containmentErr != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("repair command result %v; process-tree containment failed: %w", waitErr, containmentErr))
		}
		return waitErr
	}, nil
}

// Every repair command is assigned before resume to a kill-on-close Job
// Object. Process exit or a Hive crash therefore closes the last job handle
// and terminates every contained descendant, so snapshot restoration does not
// need to wait for the conservative command deadline on Windows.
var repairProcessTreeCrashContainmentGuaranteed = func() bool {
	return true
}

type repairJobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func waitForEmptyRepairJob(job windows.Handle) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var accounting repairJobAccounting
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return fmt.Errorf("query repair command job completion: %w", err)
		}
		if accounting.ActiveProcesses == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("repair command job remained active after termination")
}

func resumePrimaryProcessThread(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("enumerate suspended repair command threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("read suspended repair command threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == processID {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open suspended repair command thread: %w", err)
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume contained repair command: %w", resumeErr)
			}
			return nil
		}
		entry.Size = uint32(unsafe.Sizeof(windows.ThreadEntry32{}))
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			break
		}
	}
	return fmt.Errorf("suspended repair command has no primary thread")
}
