//go:build windows

package main

import (
	"os/exec"
	"strings"

	"github.com/amitbet/dynapp-agent/shellagent"
)

// The first rule allowed UDP only on the LAN listener port, only on the
// Private profile, and only when the service was installed with LAN mode
// already on. Hole punching answers on ephemeral UDP ports, a home network
// is often classified Public, and LAN mode is usually enabled later from the
// settings page, so a Windows machine accepted nothing inbound: the LAN
// probe failed and every punch sat in ICE checking until the relay took
// over. The rule is now scoped to the executable, covers every UDP port and
// profile, and is applied at each service start. Every connection it admits
// still authenticates: the QUIC listener pins its certificate and checks the
// browser key, and ICE runs DTLS with a Dyner-issued session.
const (
	windowsFirewallRuleName       = "DynApp Shell Agent (UDP)"
	windowsFirewallLegacyRuleName = "DynApp Shell Agent LAN"
)

func firewallWanted(config shellagent.Config) bool {
	return config.ListenerMode == shellagent.ListenerLAN || config.RelayEnabled
}

func provisionServiceFirewall(config shellagent.Config, executable string) error {
	if !firewallWanted(config) {
		return nil
	}
	if firewallRuleExists(windowsFirewallLegacyRuleName) {
		if err := runWindowsFirewallCommand("advfirewall", "firewall", "delete", "rule", "name="+windowsFirewallLegacyRuleName); err != nil {
			return err
		}
	}
	if firewallRuleExists(windowsFirewallRuleName) {
		return nil
	}
	return runWindowsFirewallCommand(
		"advfirewall", "firewall", "add", "rule",
		"name="+windowsFirewallRuleName,
		"dir=in", "action=allow", "protocol=UDP", "localport=any",
		"profile=any", "program="+executable,
	)
}

// refreshServiceFirewall runs at service start so a rule missing since
// install, or predating the current shape, is put in place without a
// reinstall. The service runs as LocalSystem, which netsh accepts.
func refreshServiceFirewall(config shellagent.Config, executable string) error {
	return provisionServiceFirewall(config, executable)
}

func removeServiceFirewall(_ shellagent.Config, _ string) error {
	for _, name := range []string{windowsFirewallRuleName, windowsFirewallLegacyRuleName} {
		if !firewallRuleExists(name) {
			continue
		}
		if err := runWindowsFirewallCommand("advfirewall", "firewall", "delete", "rule", "name="+name); err != nil {
			return err
		}
	}
	return nil
}

func firewallRuleExists(name string) bool {
	output, err := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+name).CombinedOutput()
	return err == nil && strings.Contains(string(output), name)
}

func runWindowsFirewallCommand(args ...string) error {
	output, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return &firewallError{detail: detail}
	}
	return nil
}

type firewallError struct{ detail string }

func (e *firewallError) Error() string { return "Windows firewall command failed: " + e.detail }
