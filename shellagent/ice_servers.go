package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pion/stun/v3"
	"github.com/pion/webrtc/v4"
)

// fallbackICEServerURLs are used whenever Dyner's list is unavailable. Two
// STUN servers let the NAT check compare mappings (docs/mesh-network.md §4).
var fallbackICEServerURLs = []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}

// iceServersRefreshMargin renews a cached list this long before Dyner says
// its TURN credentials expire, so a session never starts on stale ones.
const iceServersRefreshMargin = 5 * time.Minute

// iceServersFetchTimeout bounds the fetch: it runs before every uncached
// answer, and a slow Dyner must not delay the answer past the browser's
// patience when the STUN fallback would do.
const iceServersFetchTimeout = 3 * time.Second

type iceServerCache struct {
	key        string
	servers    []webrtc.ICEServer
	validUntil time.Time
}

func fallbackICEServers() []webrtc.ICEServer {
	return []webrtc.ICEServer{{URLs: append([]string(nil), fallbackICEServerURLs...)}}
}

// iceServers returns the STUN/TURN servers for an answer: Dyner's list while
// cached, a fresh fetch otherwise, and the two STUN servers on any error.
func (s *Server) iceServers(ctx context.Context, config Config) []webrtc.ICEServer {
	key := config.DynerBaseURL + "\x00" + config.EnvironmentID + "\x00" + config.DeviceCredential
	s.iceServersMu.Lock()
	defer s.iceServersMu.Unlock()
	if s.iceServersCache.key == key && time.Now().Before(s.iceServersCache.validUntil) {
		return s.iceServersCache.servers
	}
	fetchCtx, cancel := context.WithTimeout(ctx, iceServersFetchTimeout)
	defer cancel()
	servers, expiresAt, err := fetchICEServers(fetchCtx, nil, config)
	if err != nil {
		return fallbackICEServers()
	}
	s.iceServersCache = iceServerCache{key: key, servers: servers, validUntil: expiresAt.Add(-iceServersRefreshMargin)}
	return servers
}

func fetchICEServers(ctx context.Context, client *http.Client, config Config) ([]webrtc.ICEServer, time.Time, error) {
	if config.DynerBaseURL == "" || config.EnvironmentID == "" || config.DeviceCredential == "" {
		return nil, time.Time{}, errors.New("agent is not enrolled with Dyner")
	}
	base, err := url.Parse(config.DynerBaseURL)
	if err != nil {
		return nil, time.Time{}, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" + url.PathEscape(config.EnvironmentID) + "/ice/servers"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), strings.NewReader("{}"))
	if err != nil {
		return nil, time.Time{}, err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	request.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{Timeout: iceServersFetchTimeout}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return nil, time.Time{}, fmt.Errorf("Dyner ICE servers failed: HTTP %d", response.StatusCode)
	}
	var payload struct {
		ICEServers []struct {
			URLs       json.RawMessage `json:"urls"`
			Username   string          `json:"username"`
			Credential string          `json:"credential"`
		} `json:"iceServers"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 64*1024)).Decode(&payload); err != nil {
		return nil, time.Time{}, err
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil {
		return nil, time.Time{}, errors.New("Dyner ICE servers carry no valid expiry")
	}
	var servers []webrtc.ICEServer
	for _, entry := range payload.ICEServers {
		// RTCIceServer.urls is a string or a list of strings.
		var urls []string
		if json.Unmarshal(entry.URLs, &urls) != nil {
			var single string
			if json.Unmarshal(entry.URLs, &single) != nil {
				continue
			}
			urls = []string{single}
		}
		// pion rejects the whole configuration over one bad URL, so a
		// malformed entry is dropped instead of failing every answer.
		var valid []string
		for _, raw := range urls {
			parsed, err := stun.ParseURI(raw)
			if err != nil {
				continue
			}
			if (parsed.Scheme == stun.SchemeTypeTURN || parsed.Scheme == stun.SchemeTypeTURNS) && (entry.Username == "" || entry.Credential == "") {
				continue
			}
			valid = append(valid, raw)
		}
		if len(valid) == 0 {
			continue
		}
		server := webrtc.ICEServer{URLs: valid}
		if entry.Username != "" || entry.Credential != "" {
			server.Username, server.Credential = entry.Username, entry.Credential
		}
		servers = append(servers, server)
	}
	if len(servers) == 0 {
		return nil, time.Time{}, errors.New("Dyner returned no usable ICE servers")
	}
	return servers, expiresAt, nil
}
