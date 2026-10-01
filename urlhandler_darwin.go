//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// The agent is a bare binary and cannot claim a URL scheme, so it maintains a
// tiny launcher bundle whose script runs `dynapp-shell-agent open-url`.
// LaunchServices ignores bundles under /tmp; the state directory is used.
func urlLauncherPath(stateDir string) string {
	return filepath.Join(stateDir, "DynApp Agent.app")
}

func urlLauncherFiles(executable string) map[string][]byte {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>launch</string>
	<key>CFBundleIdentifier</key>
	<string>io.dynapp.agent-launcher</string>
	<key>CFBundleName</key>
	<string>DynApp Agent</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>CFBundleURLTypes</key>
	<array>
		<dict>
			<key>CFBundleURLName</key>
			<string>DynApp agent</string>
			<key>CFBundleURLSchemes</key>
			<array>
				<string>` + urlScheme + `</string>
			</array>
		</dict>
	</array>
	<key>LSBackgroundOnly</key>
	<true/>
</dict>
</plist>
`
	// LaunchServices delivers the URL as an Apple Event the script cannot
	// read; dynapp://start is the only action, so the script does not need it.
	script := "#!/bin/sh\nexec " + shellQuote(executable) + " open-url " + urlScheme + "://start\n"
	return map[string][]byte{"Contents/Info.plist": []byte(plist), "Contents/MacOS/launch": []byte(script)}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// registerURLHandler keeps the launcher pointed at the current binary.
func registerURLHandler(stateDir, executable string) error {
	bundle := urlLauncherPath(stateDir)
	changed := false
	for relative, data := range urlLauncherFiles(executable) {
		path := filepath.Join(bundle, filepath.FromSlash(relative))
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(relative, "/launch") {
			mode = 0o755
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", bundle).CombinedOutput(); err != nil {
			return fmt.Errorf("sign the URL launcher: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return exec.Command(lsregister, "-f", bundle).Run()
}

// startInstalledAgent kicks the launchd job, loading it first if it is
// installed but unloaded. Without a job it starts the agent directly.
func startInstalledAgent(executable string) error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if home, err := os.UserHomeDir(); err == nil {
		plist := filepath.Join(home, "Library", "LaunchAgents", agentServiceName+".plist")
		if _, err := os.Stat(plist); err == nil {
			_ = exec.Command("/bin/launchctl", "bootstrap", domain, plist).Run()
			if exec.Command("/bin/launchctl", "kickstart", domain+"/"+agentServiceName).Run() == nil {
				return nil
			}
		}
	}
	command := exec.Command(executable)
	command.Env = append(os.Environ(), "DYNAPP_AGENT_FOREGROUND=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	if command.Process == nil {
		return errors.New("the agent process did not start")
	}
	return command.Process.Release()
}

// launchd's KeepAlive already restarts the agent.
func prepareServiceRecovery() error { return nil }
