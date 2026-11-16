package shellagent

import (
	"context"
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

func TestHostDecisionTreatsOldNullAsDismissed(t *testing.T) {
	if _, outcome := hostDecision(nativeHostFrame{}); outcome != decisionDismissed {
		t.Fatalf("null capabilities from an old host = %q", outcome)
	}
	if granted, outcome := hostDecision(nativeHostFrame{Capabilities: []string{"fs.readText"}}); outcome != decisionAllow || len(granted) != 1 {
		t.Fatalf("old host allow = %v %q", granted, outcome)
	}
	if granted, outcome := hostDecision(nativeHostFrame{Decision: "allow"}); outcome != decisionAllow || granted == nil {
		t.Fatalf("allow with nothing checked = %v %q", granted, outcome)
	}
	if _, outcome := hostDecision(nativeHostFrame{Decision: "deny", Capabilities: []string{"fs.readText"}}); outcome != decisionDeny {
		t.Fatalf("deny = %q", outcome)
	}
}

func TestDismissedLaunchPromptIsAskedAgainThenWaits(t *testing.T) {
	declared := []string{"fs.readText", "secrets.manage"}
	app := NativeApp{StoreID: "o/a"}
	for attempt := 0; attempt < maxLaunchDismissals; attempt++ {
		kind, ask := launchPermissionAsk(app, declared)
		if kind != "pairing" || len(ask) != 2 {
			t.Fatalf("attempt %d: kind %q ask %v", attempt, kind, ask)
		}
		app = applyNativeDecision(app, kind, ask, nil, decisionDismissed, false)
		if app.ApprovedAt != 0 || len(app.Denied) != 0 || len(app.Declared) != 0 {
			t.Fatalf("a dismissal must store no decision: %+v", app)
		}
	}
	if kind, _ := launchPermissionAsk(app, declared); kind != "" {
		t.Fatalf("after %d dismissals the launch prompt must stop, got %q", maxLaunchDismissals, kind)
	}
}

func TestDenyIsRememberedButStaysAskable(t *testing.T) {
	declared := []string{"fs.readText", "fs.writeText", "secrets.manage"}
	app := applyNativeDecision(NativeApp{StoreID: "o/a"}, "pairing", declared, []string{"fs.readText"}, decisionAllow, false)
	if !sameCapabilitySet(app.Capabilities, []string{"fs.readText"}) || !sameCapabilitySet(app.Denied, []string{"fs.writeText", "secrets.manage"}) {
		t.Fatalf("partial allow: %+v", app)
	}
	if kind, _ := launchPermissionAsk(app, declared); kind != "" {
		t.Fatalf("refused permissions must not come back at launch, got %q", kind)
	}
	if kind, ask := launchPermissionAsk(app, append(declared, "clipboard.read")); kind != "delta" || !sameCapabilitySet(ask, []string{"clipboard.read"}) {
		t.Fatalf("an update's new permission: %q %v", kind, ask)
	}
	app = applyNativeDecision(app, "required", []string{"secrets.manage"}, nil, decisionDeny, true)
	if !stringSliceHas(app.DontAsk, "secrets.manage") {
		t.Fatalf("don't ask again not stored: %+v", app)
	}
	app = applyNativeDecision(app, "required", []string{"fs.writeText"}, []string{"fs.writeText"}, decisionAllow, false)
	if !stringSliceHas(app.Capabilities, "fs.writeText") || stringSliceHas(app.Denied, "fs.writeText") || !stringSliceHas(app.Capabilities, "fs.readText") {
		t.Fatalf("a later grant must clear the refusal and keep the rest: %+v", app)
	}
	app = applyNativeDecision(app, "review", declared, declared, decisionAllow, false)
	if len(app.DontAsk) != 0 || len(app.Denied) != 0 {
		t.Fatalf("granting in review clears refusals: %+v", app)
	}
}

func TestPermissionAskableHonoursDontAsk(t *testing.T) {
	agent := &Server{Config: Config{NativeApps: []NativeApp{{StoreID: "o/a", DontAsk: []string{"secrets.manage"}}}}}
	auth := &socketAuthentication{storeID: "o/a", native: true, declared: []string{"fs.readText", "secrets.manage"}}
	if !agent.permissionAskable(auth, "fs.readText") {
		t.Fatal("declared, ungranted permission must be askable")
	}
	if agent.permissionAskable(auth, "secrets.manage") {
		t.Fatal("don't ask again must not be askable")
	}
	if agent.permissionAskable(auth, "fs.exec") {
		t.Fatal("undeclared permission must not be askable")
	}
	refusal := agent.refusal(auth, "fs.readText", "")
	if fields := refusal.fields(); fields["code"] != "permission-required" || fields["askable"] != true {
		t.Fatalf("refusal fields = %v", fields)
	}
	if agent.ensureCapability(context.Background(), auth, "fs.readText", false) {
		t.Fatal("without a user action nothing may prompt or grant")
	}
}

func TestRevokeRemovesTheAppGrantForEveryBrowserKey(t *testing.T) {
	origin := "https://amitbet-commander.dynapp.io"
	key := func(id string) BrowserIdentity {
		return BrowserIdentity{KeyID: id, Origin: origin, StoreID: "amit-bet/commander", Capabilities: []string{"fs.list"}, ApprovedAt: 1}
	}
	other := BrowserIdentity{KeyID: "k-other", Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes", Capabilities: []string{"fs.list"}, ApprovedAt: 1}
	agent := &Server{Config: Config{BrowserIdentities: []BrowserIdentity{key("k1"), key("k2"), key("k3"), other}}}
	if _, err := agent.revokeGrant(map[string]any{"keyId": "k2", "origin": origin, "storeId": "amit-bet/commander"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := findApprovedAppGrant(agent.Config.BrowserIdentities, origin, "amit-bet/commander"); ok {
		t.Fatalf("a revoked app grant must not be reusable by another key: %#v", agent.Config.BrowserIdentities)
	}
	if len(agent.Config.BrowserIdentities) != 1 || agent.Config.BrowserIdentities[0].KeyID != "k-other" {
		t.Fatalf("other apps' grants must stay: %#v", agent.Config.BrowserIdentities)
	}
}
