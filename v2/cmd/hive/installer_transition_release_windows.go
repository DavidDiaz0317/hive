//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func waitForInstallerExecutableRelease(executable string, timeout time.Duration) error {
	path, err := windows.UTF16PtrFromString(filepath.Clean(executable))
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, openErr := windows.CreateFile(
			path,
			windows.DELETE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if openErr == nil {
			if closeErr := windows.CloseHandle(handle); closeErr != nil {
				return fmt.Errorf("close scheduler release probe: %w", closeErr)
			}
			return nil
		}
		if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(openErr, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("probe scheduler executable release: %w", openErr)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("scheduler executable remained in use after %s: %w", timeout, openErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
