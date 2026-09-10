package shellagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueRelayTicketUsesDeviceCredentialWithoutExposingIt(t *testing.T) {
	credential := "env_test.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/remote-environments/env_test/ticket" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "DynApp-Device "+credential {
			t.Fatal("device authorization missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connection":{"carrier":"websocket","endpoint":"wss://relay.example/connect","ticket":"ticket","ticketExpiresAt":"2026-01-01T00:00:00Z","protocol":1,"acceptedAuthTokenHashes":["hash"],"transport":{"kind":"relay","e2eEncryption":true}}}`))
	}))
	defer server.Close()
	connection, err := IssueRelayTicket(context.Background(), server.Client(), Config{SchemaVersion: ConfigSchemaVersion, DynerBaseURL: server.URL, EnvironmentID: "env_test", DeviceCredential: credential, RelayEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if connection.Endpoint != "wss://relay.example/connect" || !connection.Transport.E2EEncryption {
		t.Fatalf("connection = %#v", connection)
	}
}
