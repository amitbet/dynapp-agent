package shellagent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
)

//go:embed macos-associations.js
var macAssociationScript string

var runMacAssociationCommand = func(args ...string) ([]byte, error) {
	return exec.Command("/usr/bin/osascript", args...).CombinedOutput()
}

func macDefaultAssociations(mode, bundleID string, extensions []string, bundlePath ...string) ([]string, error) {
	encoded, err := json.Marshal(extensions)
	if err != nil {
		return nil, err
	}
	args := []string{"-l", "JavaScript", "-e", macAssociationScript, "--", mode, bundleID, string(encoded)}
	if len(bundlePath) > 0 {
		args = append(args, bundlePath[0])
	}
	output, err := runMacAssociationCommand(args...)
	if err != nil {
		return nil, fmt.Errorf("macOS file associations: %w: %s", err, output)
	}
	var applied []string
	if err := json.Unmarshal(output, &applied); err != nil {
		return nil, fmt.Errorf("read macOS default handlers: %w", err)
	}
	return applied, nil
}
