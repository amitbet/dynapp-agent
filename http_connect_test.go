package shellagent

import "testing"

func TestHTTPConnectPathPrefixes(t *testing.T) {
	patterns := []string{"https://api.github.com/", "https://example.test/api/v1/"}
	allowed := []string{
		"https://api.github.com/repos/a/b",
		"https://example.test/api/v1/items",
		"https://example.test/api/v1",
	}
	denied := []string{
		"https://example.test/api/v10",
		"https://example.test/secret",
		"https://evil.test/",
		"https://user:pass@example.test/api/v1/items",
	}
	for _, url := range allowed {
		if !urlAllowedByHTTPConnect(url, patterns) {
			t.Fatalf("expected allow %s", url)
		}
	}
	for _, url := range denied {
		if urlAllowedByHTTPConnect(url, patterns) {
			t.Fatalf("expected deny %s", url)
		}
	}
	if err := assertURLAllowedByHTTPConnect("https://evil.test/secret", patterns); err == nil {
		t.Fatal("expected connect-list error")
	}
}

func TestHTTPConnectPatternsAcceptStringSlices(t *testing.T) {
	patterns, err := httpConnectPatterns([]string{"https://example.test/api/v1/"}, "https")
	if err != nil || len(patterns) != 1 || patterns[0] != "https://example.test/api/v1/" {
		t.Fatalf("patterns = %#v, %v", patterns, err)
	}
}
