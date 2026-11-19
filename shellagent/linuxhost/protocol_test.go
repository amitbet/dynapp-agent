package linuxhost

import (
	"net"
	"strings"
	"testing"
)

func TestOriginKey(t *testing.T) {
	for raw, want := range map[string]string{
		"https://AmitBet-Grid.dynapp.io/path?x": "https://amitbet-grid.dynapp.io",
		"https://a.dynapp.io:443/":              "https://a.dynapp.io",
		"http://127.0.0.1:8080/x":               "http://127.0.0.1:8080",
		"about:blank":                           "",
		"":                                      "",
	} {
		if got := OriginKey(raw); got != want {
			t.Errorf("OriginKey(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{Origin: "https://amitbet-grid.dynapp.io"}
	cases := []struct {
		name    string
		verdict Verdict
		open    string
		got     func() (Verdict, string)
	}{
		{"app page", Allow, "", func() (Verdict, string) { return p.Navigation("https://amitbet-grid.dynapp.io/x", true, true) }},
		{"blob", Allow, "", func() (Verdict, string) { return p.Navigation("blob:https://amitbet-grid.dynapp.io/1", true, false) }},
		{"clicked external link", Ignore, "https://example.com/", func() (Verdict, string) { return p.Navigation("https://example.com/", true, true) }},
		{"iframe or script load proceeds", Allow, "", func() (Verdict, string) { return p.Navigation("https://www.youtube.com/embed/x", true, false) }},
		{"popup navigates freely", Allow, "", func() (Verdict, string) { return p.Navigation("https://example.com/", false, true) }},
		{"mailto", Ignore, "mailto:a@b.c", func() (Verdict, string) { return p.Navigation("mailto:a@b.c", true, false) }},
		{"file scheme", Ignore, "", func() (Verdict, string) { return p.Navigation("file:///etc/passwd", true, true) }},
		{"main frame leaves", Ignore, "https://example.com/", func() (Verdict, string) { return p.MainFrameResponse("https://example.com/") }},
		{"main frame stays", Allow, "", func() (Verdict, string) { return p.MainFrameResponse("https://amitbet-grid.dynapp.io/?launch=pwa") }},
		{"popup of the app", Allow, "", func() (Verdict, string) { return p.NewWindow("https://amitbet-grid.dynapp.io/print") }},
		{"Dyner sign-in popup", Allow, "", func() (Verdict, string) { return p.NewWindow("https://dynapp.io/connect") }},
		{"foreign popup", Ignore, "https://example.com/", func() (Verdict, string) { return p.NewWindow("https://example.com/") }},
		{"lookalike domain", Ignore, "https://evildynapp.io/", func() (Verdict, string) { return p.NewWindow("https://evildynapp.io/") }},
	}
	for _, test := range cases {
		verdict, open := test.got()
		if verdict != test.verdict || open != test.open {
			t.Errorf("%s: got %v %q, want %v %q", test.name, verdict, open, test.verdict, test.open)
		}
	}
}

func TestParseBridgeMessage(t *testing.T) {
	origin := "https://amitbet-grid.dynapp.io"
	good := `{"dynappNativeHost":1,"op":"open","sid":"s1","hostToken":"t0k"}`
	if message, err := ParseBridgeMessage(good, origin+"/?launch=pwa", origin, "t0k"); err != nil || message.Op != "open" || message.SID != "s1" {
		t.Fatalf("good message: %+v %v", message, err)
	}
	for name, test := range map[string][2]string{
		"no token":       {`{"dynappNativeHost":1,"op":"open","sid":"s1"}`, origin + "/"},
		"wrong token":    {`{"dynappNativeHost":1,"op":"open","hostToken":"nope"}`, origin + "/"},
		"no marker":      {`{"op":"open","hostToken":"t0k"}`, origin + "/"},
		"foreign origin": {good, "https://example.com/"},
		"not json":       {"open", origin + "/"},
	} {
		if _, err := ParseBridgeMessage(test[0], test[1], origin, "t0k"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := ParseBridgeMessage(good, origin+"/", origin, ""); err == nil {
		t.Error("an empty launch token was accepted")
	}
}

func TestWrapBootstrapScopesToken(t *testing.T) {
	script := WrapBootstrap(`(() => { window.seen = typeof __dynappHostToken; })();`, `a"b`)
	if !strings.HasPrefix(script, "(() => {\nconst __dynappHostToken = \"a\\\"b\";\n") || !strings.HasSuffix(script, "\n})();") {
		t.Fatalf("wrapped script = %s", script)
	}
}

func TestParsePromptAnswer(t *testing.T) {
	if answer, ok := ParsePromptAnswer(`{"resize":312}`); !ok || answer.Resize != 312 || answer.Decision != "" {
		t.Fatalf("resize = %+v %v", answer, ok)
	}
	if answer, ok := ParsePromptAnswer(`{"decision":"allow","capabilities":["fs.read"]}`); !ok || len(answer.Capabilities) != 1 {
		t.Fatalf("allow = %+v %v", answer, ok)
	}
	if answer, ok := ParsePromptAnswer(`{"decision":"deny","capabilities":["fs.read"],"dontAsk":true}`); !ok || answer.Capabilities != nil || !answer.DontAsk {
		t.Fatalf("deny = %+v %v", answer, ok)
	}
	for _, raw := range []string{`{"decision":"grant"}`, `{}`, `nope`} {
		if _, ok := ParsePromptAnswer(raw); ok {
			t.Errorf("%s was accepted", raw)
		}
	}
}

func TestParseOptions(t *testing.T) {
	options, err := ParseOptions([]string{"--id", "a/b", "--app-id", "io.dynapp.app.a.b", "--socket", "/tmp/s", "--url", "https://evil.example/", "--origin", "https://a-b.dynapp.io", "--", "/tmp/x.txt", "--weird"})
	if err != nil {
		t.Fatal(err)
	}
	if options.URL != "https://a-b.dynapp.io/?launch=pwa" || options.Name != "a/b" || strings.Join(options.Files, "|") != "/tmp/x.txt|--weird" {
		t.Fatalf("options = %+v", options)
	}
	if _, err := ParseOptions([]string{"--id", "a/b"}); err == nil {
		t.Fatal("missing socket and origin were accepted")
	}
	if options, err := ParseOptions([]string{"--check"}); err != nil || !options.Check {
		t.Fatalf("--check = %+v %v", options, err)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	left, right := net.Pipe()
	a, b := NewFrameConn(left), NewFrameConn(right)
	go func() { _ = a.WriteJSON(map[string]any{"type": "native-host-hello"}) }()
	kind, payload, err := b.Read()
	if err != nil || kind != frameText || string(payload) != `{"type":"native-host-hello"}` {
		t.Fatalf("frame = %d %s %v", kind, payload, err)
	}
}
