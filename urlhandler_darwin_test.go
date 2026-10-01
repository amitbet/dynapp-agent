//go:build darwin

package main

import (
	"strings"
	"testing"
)

func TestURLLauncherClaimsTheSchemeAndRunsOpenURL(t *testing.T) {
	files := urlLauncherFiles("/Users/o'neil/.local/bin/dynapp-shell-agent")
	plist := string(files["Contents/Info.plist"])
	for _, want := range []string{"<string>dynapp</string>", "<key>CFBundleURLSchemes</key>", "<key>LSBackgroundOnly</key>", "io.dynapp.agent-launcher"} {
		if !strings.Contains(plist, want) {
			t.Fatalf("Info.plist lacks %q", want)
		}
	}
	script := string(files["Contents/MacOS/launch"])
	if script != "#!/bin/sh\nexec '/Users/o'\\''neil/.local/bin/dynapp-shell-agent' open-url dynapp://start\n" {
		t.Fatalf("launcher script = %q", script)
	}
}
