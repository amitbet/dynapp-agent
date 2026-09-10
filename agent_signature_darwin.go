//go:build darwin

package shellagent

import (
	"fmt"
	"os/exec"
	"strings"
)

const agentCodeSigningIdentifier = "com.amitbet.dynapp.shell-agent"

func verifyDownloadedAgentSignature(path string) error {
	if output, err := exec.Command("codesign", "--verify", "--strict", path).CombinedOutput(); err != nil {
		return fmt.Errorf("macOS code-signature verification failed: %s", strings.TrimSpace(string(output)))
	}
	output, err := exec.Command("codesign", "-d", "-r-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("read macOS code requirement: %w", err)
	}
	if !strings.Contains(string(output), `identifier "`+agentCodeSigningIdentifier+`"`) {
		return fmt.Errorf("macOS update has unexpected code-signing identifier")
	}
	return nil
}
