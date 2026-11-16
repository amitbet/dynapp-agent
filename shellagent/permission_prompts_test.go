package shellagent

import (
	"strings"
	"testing"
)

func TestNativePermissionPageEmbedsRequestOnce(t *testing.T) {
	if strings.Count(nativePermissionPromptTemplate, "__DYNAPP_PERMISSION_REQUEST__") != 1 {
		t.Fatal("the prompt template must contain the request placeholder exactly once")
	}
	groups := nativePermissionGroups([]string{"fs.readText"}, nil, map[string]string{"fs.readText": "</script><script>alert(1)</script>"})
	page := nativePermissionPage("required", "Probe </script>", "https://probe.example", groups, []string{"files"})
	if strings.Contains(page, "__DYNAPP_PERMISSION_REQUEST__") {
		t.Fatal("placeholder left in the page")
	}
	if strings.Contains(page, "</script><script>alert(1)") || strings.Contains(page, "Probe </script>") {
		t.Fatal("request text can end the script element")
	}
	if !strings.Contains(page, `"kind":"required"`) {
		t.Fatal("request kind missing from the page")
	}
}

func TestNativePermissionRequestFrameKeepsOldHostsWorking(t *testing.T) {
	frame := nativePermissionRequestFrame("req_1", "required", "Probe", "https://probe.example", nil, nil)
	if frame["kind"] != "delta" {
		t.Fatalf("old hosts need a kind they know, got %v", frame["kind"])
	}
	if page, _ := frame["page"].(string); !strings.Contains(page, `"kind":"required"`) {
		t.Fatal("the page must carry the real kind")
	}
}
