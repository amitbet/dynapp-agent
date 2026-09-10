package main

import (
	"testing"

	"github.com/amitbet/dynapp-agent/shellagent"
)

func TestStartupAddressDoesNotReuseLocalPortForLAN(t *testing.T) {
	config := shellagent.Config{
		ListenerMode: shellagent.ListenerLocal,
		LANAddress:   "127.0.0.1:60349",
	}
	if got := startupAddress(shellagent.DefaultAddress, false, true, config); got != shellagent.DefaultAddress {
		t.Fatalf("LAN startup address = %q, want %q", got, shellagent.DefaultAddress)
	}
}

func TestStartupAddressKeepsSavedLocalPortForLocalMode(t *testing.T) {
	config := shellagent.Config{
		ListenerMode: shellagent.ListenerLocal,
		LANAddress:   "127.0.0.1:60349",
	}
	if got := startupAddress(shellagent.DefaultAddress, false, false, config); got != config.LANAddress {
		t.Fatalf("local startup address = %q, want %q", got, config.LANAddress)
	}
}
