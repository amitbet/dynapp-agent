package shellagent

import "testing"

func TestNormalizeBrowserClient(t *testing.T) {
	if got := normalizeBrowserClient(nil); !got.Empty() {
		t.Fatalf("nil client = %#v", got)
	}
	got := normalizeBrowserClient(&BrowserClient{
		Browser:  "  Chrome\n",
		Version:  "140.0.1",
		Platform: "macOS",
		Mode:     "TAB",
	})
	if got.Browser != "Chrome" || got.Mode != "tab" || got.Label != "Chrome 140.0.1 on macOS · browser tab" {
		t.Fatalf("normalized = %#v", got)
	}
	if clipBrowserClientField("x\x00y") != "xy" {
		t.Fatalf("control characters were not stripped")
	}
	long := make([]rune, maxBrowserClientField+20)
	for i := range long {
		long[i] = 'a'
	}
	clipped := normalizeBrowserClient(&BrowserClient{Browser: string(long), Mode: "iframe"})
	if len([]rune(clipped.Browser)) != maxBrowserClientField {
		t.Fatalf("clipped browser length = %d", len([]rune(clipped.Browser)))
	}
	if clipped.Mode != "" {
		t.Fatalf("unknown mode kept: %q", clipped.Mode)
	}
}

func TestApplyBrowserClient(t *testing.T) {
	identity := BrowserIdentity{}
	if applyBrowserClient(&identity, nil) {
		t.Fatal("empty apply changed identity")
	}
	first := &BrowserClient{Browser: "Safari", Platform: "macOS", Mode: "pwa"}
	if !applyBrowserClient(&identity, first) || identity.Client.Label != "Safari on macOS · installed app" {
		t.Fatalf("first apply = %#v", identity.Client)
	}
	if applyBrowserClient(&identity, first) {
		t.Fatal("identical client wrote again")
	}
	if !applyBrowserClient(&identity, &BrowserClient{Browser: "Chrome", Platform: "macOS", Mode: "tab"}) {
		t.Fatal("updated client was ignored")
	}
	if identity.Client.Browser != "Chrome" || identity.Client.Mode != "tab" {
		t.Fatalf("updated client = %#v", identity.Client)
	}
}
