//go:build !windows

package main

import "errors"

var errWindowsOnly = errors.New("install-user and remove-service are Windows commands; macOS and Linux use the launchd and systemd installers")

func runInstallUser(bool) error { return errWindowsOnly }

func runRemoveService() error { return errWindowsOnly }
