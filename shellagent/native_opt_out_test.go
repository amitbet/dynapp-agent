package shellagent

import (
	"strings"
	"testing"
)

func TestNativeAppsOptOut(t *testing.T) {
	for _, value := range []string{"", "1"} {
		t.Setenv("DYNAPP_NATIVE_APPS", value)
		if _, reason := (&Server{}).nativeAvailable(); strings.Contains(reason, "turned off") {
			t.Fatalf("DYNAPP_NATIVE_APPS=%q turned native apps off: %q", value, reason)
		}
	}
	for _, value := range []string{"0", "off", "false"} {
		t.Setenv("DYNAPP_NATIVE_APPS", value)
		if ok, reason := (&Server{}).nativeAvailable(); ok || !strings.Contains(reason, "turned off") {
			t.Fatalf("DYNAPP_NATIVE_APPS=%q: %v %q", value, ok, reason)
		}
	}
	t.Setenv("DYNAPP_NATIVE_APPS", "")
	server := &Server{}
	server.Config.NativeAppsDisabled = true
	if ok, reason := server.nativeAvailable(); ok || !strings.Contains(reason, "turned off") {
		t.Fatalf("nativeAppsDisabled: %v %q", ok, reason)
	}
}
