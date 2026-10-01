//go:build !windows

package main

import "errors"

// macOS apps run the Swift host inside their own bundle instead.
func runAppHost(appHostOptions) error {
	return errors.New("the app subcommand is the Windows app host")
}
