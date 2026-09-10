//go:build windows

package main

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for processIsAlive(pid) {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the old agent to exit")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func processIsAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}
