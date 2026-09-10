//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
	"time"
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
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
