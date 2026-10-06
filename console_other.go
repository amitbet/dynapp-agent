//go:build !windows

package main

func detachConsole()          {}
func prepareConsole([]string) {}
