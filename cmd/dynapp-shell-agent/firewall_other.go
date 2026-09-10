//go:build !darwin && !windows

package main

import shellagent "github.com/amitbet/dynapp-agent"

func provisionServiceFirewall(shellagent.Config, string) error { return nil }
func removeServiceFirewall(shellagent.Config, string) error    { return nil }
