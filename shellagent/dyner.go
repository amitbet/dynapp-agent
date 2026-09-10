package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RelayConnection is the device-authenticated configuration issued by Dyner.
// The agent keeps the device credential private and gives browser credentials
// only to the relay protocol implementation.
type RelayConnection struct {
	Carrier                 string            `json:"carrier"`
	Endpoint                string            `json:"endpoint"`
	Ticket                  string            `json:"ticket"`
	TicketExpiresAt         string            `json:"ticketExpiresAt"`
	Protocol                int               `json:"protocol"`
	AcceptedAuthTokenHashes []string          `json:"acceptedAuthTokenHashes"`
	BrowserIdentities       []BrowserIdentity `json:"browserIdentities"`
	Transport               struct {
		Kind          string `json:"kind"`
		E2EEncryption bool   `json:"e2eEncryption"`
		ShellEnabled  *bool  `json:"shellEnabled"`
	} `json:"transport"`
}

type dynerHTTPError struct{ StatusCode int }

func (err dynerHTTPError) Error() string {
	return fmt.Sprintf("Dyner browser identity sync failed: HTTP %d", err.StatusCode)
}

// Dyner's relay-ticket claims currently use a distinct, stable v1 contract.
// It is intentionally independent of the agent-to-PWA protocol version.
const dynerRelayProtocolVersion = 1

func (connection RelayConnection) Validate() error {
	if connection.Carrier != "websocket" || connection.Protocol != dynerRelayProtocolVersion || connection.Ticket == "" {
		return errors.New("Dyner returned an invalid relay connection")
	}
	endpoint, err := url.Parse(connection.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "wss" && endpoint.Scheme != "ws") {
		return errors.New("Dyner returned an invalid relay endpoint")
	}
	return nil
}

// IssueRelayTicket reads the current account policy using the device
// credential. It does not log or return that credential.
func IssueRelayTicket(ctx context.Context, client *http.Client, config Config) (RelayConnection, error) {
	if err := config.Validate(); err != nil {
		return RelayConnection{}, err
	}
	if !config.RelayEnabled {
		return RelayConnection{}, errors.New("relay is disabled in local agent configuration")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base, err := url.Parse(config.DynerBaseURL)
	if err != nil {
		return RelayConnection{}, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" + url.PathEscape(config.EnvironmentID) + "/ticket"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return RelayConnection{}, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return RelayConnection{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return RelayConnection{}, fmt.Errorf("Dyner device ticket failed: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Connection RelayConnection `json:"connection"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return RelayConnection{}, err
	}
	if err := payload.Connection.Validate(); err != nil {
		return RelayConnection{}, err
	}
	return payload.Connection, nil
}

// SyncBrowserIdentities refreshes the public-key allow-list used by direct
// LAN WebTransport. It is provisioning/synchronization, never part of a
// browser's connection handshake.
func SyncBrowserIdentities(ctx context.Context, client *http.Client, config Config) ([]BrowserIdentity, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.DeviceCredential == "" || config.EnvironmentID == "" {
		return nil, errors.New("agent is not enrolled with Dyner")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	base, err := url.Parse(config.DynerBaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" + url.PathEscape(config.EnvironmentID) + "/browser-identities"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, dynerHTTPError{StatusCode: response.StatusCode}
	}
	var payload struct {
		BrowserIdentities []BrowserIdentity `json:"browserIdentities"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return normalizeBrowserIdentities(payload.BrowserIdentities)
}
