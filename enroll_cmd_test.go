package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func enrollmentAgent(t *testing.T, signedIn bool, loginURL string) (*httptest.Server, *int) {
	t.Helper()
	starts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/settings/status":
			if signedIn {
				_, _ = w.Write([]byte(`{"signedIn":true,"dynerBaseUrl":"https://dynapp.io"}`))
			} else {
				_, _ = w.Write([]byte(`{"signedIn":false,"dynerBaseUrl":"https://dynapp.io"}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/settings/login/start":
			starts++
			_, _ = w.Write([]byte(`{"url":"` + loginURL + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &starts
}

func TestEnrollmentURLSkipsSignedInAgentOnlyWhenIfNeeded(t *testing.T) {
	server, starts := enrollmentAgent(t, true, "https://dynapp.io/agent-login?state=x")
	got, err := enrollmentURL(context.Background(), server.URL, true)
	if err != nil || got != "" || *starts != 0 {
		t.Fatalf("--if-needed on a signed-in agent: url=%q err=%v starts=%d", got, err, *starts)
	}
	got, err = enrollmentURL(context.Background(), server.URL, false)
	if err != nil || got != "https://dynapp.io/agent-login?state=x" || *starts != 1 {
		t.Fatalf("explicit enroll must start a new sign-in: url=%q err=%v starts=%d", got, err, *starts)
	}
}

func TestEnrollmentURLRejectsForeignSignInHost(t *testing.T) {
	for _, bad := range []string{"https://evil.example/login", "https://user@dynapp.io/login", "file:///etc/passwd", "http://dynapp.io/login"} {
		server, _ := enrollmentAgent(t, false, bad)
		if got, err := enrollmentURL(context.Background(), server.URL, true); err == nil {
			t.Fatalf("accepted %q as %q", bad, got)
		}
	}
}

func TestRunEnrollmentRequiresLoopback(t *testing.T) {
	if err := runEnrollment("192.168.1.5:9011", true); err == nil {
		t.Fatal("enrollment accepted a non-loopback agent address")
	}
}
