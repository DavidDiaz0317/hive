//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

func configureDetachedProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processMatchesExecutable(pid int, expected string) bool {
	if runtime.GOOS != "linux" {
		return processIsAlive(pid)
	}
	actual, err := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe"))
	if err != nil {
		return false
	}
	expectedResolved, err := filepath.EvalSymlinks(expected)
	return err == nil && actual == expectedResolved
}

func processIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}
