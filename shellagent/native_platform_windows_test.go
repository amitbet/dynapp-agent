//go:build windows

package shellagent

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestPNGToICOWrapsTheImage(t *testing.T) {
	png := append(append([]byte{}, pngSignature...), make([]byte, 16)...)
	binary.BigEndian.PutUint32(png[16:20], 512)
	binary.BigEndian.PutUint32(png[20:24], 512)
	ico := pngToICO(png)
	if !bytes.Equal(ico[:6], []byte{0, 0, 1, 0, 1, 0}) {
		t.Fatalf("ICO header = %v", ico[:6])
	}
	if ico[6] != 0 || ico[7] != 0 {
		t.Fatalf("512 px must be stored as 0 (256+): %v", ico[6:8])
	}
	if size := binary.LittleEndian.Uint32(ico[14:18]); int(size) != len(png) {
		t.Fatalf("image size = %d", size)
	}
	if offset := binary.LittleEndian.Uint32(ico[18:22]); offset != 22 || !bytes.Equal(ico[22:], png) {
		t.Fatalf("image offset = %d", offset)
	}
}

func TestWindowsAppIdentityAndNames(t *testing.T) {
	if got := nativeAppUserModelID("amit-bet/grid_book"); got != "DynApp.amit-bet.grid-book" {
		t.Fatalf("AppUserModelID = %q", got)
	}
	if got := nativeShortcutName(`Notes: "pro"/x?. `); strings.ContainsAny(got, `:"/?`) || strings.HasSuffix(got, ".") {
		t.Fatalf("shortcut name = %q", got)
	}
	if !strings.HasPrefix(nativeEndpoint(""), `\\.\pipe\dynapp-native-host`) {
		t.Fatalf("pipe = %q", nativeEndpoint(""))
	}
}

func TestWindowsNativeAppsNeedNoOptIn(t *testing.T) {
	t.Setenv("DYNAPP_NATIVE_APPS", "")
	server := &Server{}
	if _, reason := server.nativeAvailable(); strings.Contains(reason, "preview") {
		t.Fatalf("native apps still gated: %q", reason)
	}
}
