//go:build windows

package main

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"

	shellagent "github.com/amitbet/dynapp-agent"
)

const windowsFirewallRuleName = "DynApp Shell Agent LAN"

func provisionServiceFirewall(config shellagent.Config, executable string) error {
	if config.ListenerMode != shellagent.ListenerLAN {
		return nil
	}
	port, err := listenerPort(config)
	if err != nil {
		return err
	}
	if firewallRuleExists() {
		return nil
	}
	args := []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + windowsFirewallRuleName,
		"dir=in", "action=allow", "protocol=UDP",
		"localport=" + strconv.Itoa(port),
		"service=dynapp-shell-agent", "profile=private",
		"program=" + executable,
	}
	return runWindowsFirewallCommand(args...)
}

func removeServiceFirewall(_ shellagent.Config, _ string) error {
	if !firewallRuleExists() {
		return nil
	}
	return runWindowsFirewallCommand("advfirewall", "firewall", "delete", "rule", "name="+windowsFirewallRuleName)
}

func listenerPort(config shellagent.Config) (int, error) {
	_, portText, err := net.SplitHostPort(config.LANAddress)
	if err != nil {
		return 0, fmt.Errorf("invalid LAN listener address %q: %w", config.LANAddress, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid LAN listener port %q", portText)
	}
	return port, nil
}

func firewallRuleExists() bool {
	command := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+windowsFirewallRuleName)
	output, err := command.CombinedOutput()
	return err == nil && strings.Contains(string(output), windowsFirewallRuleName)
}

func runWindowsFirewallCommand(args ...string) error {
	output, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("Windows firewall command failed: %s", detail)
	}
	return nil
}
