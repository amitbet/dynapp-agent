package fs

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWindowsTrashScriptEmbedsQuotedPaths(t *testing.T) {
	script := windowsTrashScript([]string{`C:\a b\x.txt`, `C:\it's\y.txt`})
	if !strings.Contains(script, `@('C:\a b\x.txt','C:\it''s\y.txt')`) {
		t.Fatalf("paths not embedded as quoted literals: %s", script)
	}
	if strings.Contains(script, "$args") {
		t.Fatalf("script must not rely on $args: %s", script)
	}
	if !strings.Contains(script, "$ErrorActionPreference = 'Stop'") {
		t.Fatalf("failures must exit non-zero: %s", script)
	}
}

func TestEncodePowerShellCommandIsUTF16LEBase64(t *testing.T) {
	script := "Write-Output 'ש'"
	raw, err := base64.StdEncoding.DecodeString(encodePowerShellCommand(script))
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("bad encoding: %v", err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(units)); got != script {
		t.Fatalf("round trip = %q", got)
	}
}

func TestWindowsTrashBatchesSplitsLongLists(t *testing.T) {
	paths := make([]string, 200)
	for i := range paths {
		paths[i] = `C:\` + strings.Repeat("x", 100)
	}
	batches := windowsTrashBatches(paths)
	if len(batches) < 2 {
		t.Fatalf("expected several batches, got %d", len(batches))
	}
	total := 0
	for _, batch := range batches {
		total += len(batch)
		if n := len(encodePowerShellCommand(windowsTrashScript(batch))); n > 30000 {
			t.Fatalf("encoded command too long: %d", n)
		}
	}
	if total != len(paths) {
		t.Fatalf("lost paths: %d of %d", total, len(paths))
	}
	if got := windowsTrashBatches([]string{`C:\one`}); len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("single path batches = %v", got)
	}
}
