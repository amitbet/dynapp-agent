//go:build !darwin && !windows

package main

import "github.com/amitbet/dynapp-agent/shellagent"

func provisionServiceFirewall(shellagent.Config, string) error { return nil }
func removeServiceFirewall(shellagent.Config, string) error    { return nil }
