//go:build windows

package main

import (
	"github.com/amitbet/dynapp-agent/shellagent/winhost"
)

func runAppHost(options appHostOptions) error {
	return winhost.Run(winhost.Options(options))
}
