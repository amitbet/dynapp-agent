//go:build darwin

package main

import (
	"fmt"
	"os/exec"
	"strings"

	shellagent "github.com/amitbet/dynapp-agent"
)

const macOSFirewallTool = "/usr/libexec/ApplicationFirewall/socketfilterfw"

func provisionServiceFirewall(config shellagent.Config, executable string) error {
	if config.ListenerMode != shellagent.ListenerLAN {
		return nil
	}
	if err := runFirewallCommand("--add", executable); err != nil {
		return err
	}
	return runFirewallCommand("--unblockapp", executable)
}

func removeServiceFirewall(_ shellagent.Config, executable string) error {
	output, err := exec.Command(macOSFirewallTool, "--listapps").CombinedOutput()
	if err == nil && !strings.Contains(string(output), executable) {
		return nil
	}
	return runFirewallCommand("--remove", executable)
}

func runFirewallCommand(args ...string) error {
	output, err := exec.Command(macOSFirewallTool, args...).CombinedOutput()
	if err != nil {
		detail := string(output)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("macOS firewall command %s failed: %s", args[0], detail)
	}
	return nil
}
