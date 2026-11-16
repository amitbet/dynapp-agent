package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// networkProfile is one connected network as the OS classifies it. Windows
// asks the user "Private or Public?" when it first joins a network, which is
// the only signal the agent has that a LAN is trusted.
type networkProfile struct {
	Name      string `json:"name"`
	Interface string `json:"interface"`
	Category  string `json:"category"` // "private", "public" or "domain"
}

var errNetworkCategoryUnsupported = errors.New("network category is not reported on this platform")

const networkCategoryCacheTTL = 30 * time.Second

type networkCategoryCache struct {
	mu       sync.Mutex
	at       time.Time
	profiles []networkProfile
	err      error
}

// readNetworkProfiles is replaced per platform and in tests.
var readNetworkProfiles = readPlatformNetworkProfiles

func (c *networkCategoryCache) get(ctx context.Context) ([]networkProfile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < networkCategoryCacheTTL {
		return c.profiles, c.err
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.profiles, c.err = readNetworkProfiles(readCtx)
	c.at = time.Now()
	return c.profiles, c.err
}

// networkSettingsStatus tells the settings page whether to suggest LAN mode.
// It suggests only when every connected network is one the user marked
// Private (or a domain network): one Public network means the machine may be
// on café or hotel Wi-Fi, where the listener should stay on loopback.
func networkSettingsStatus(profiles []networkProfile, err error, mode string) map[string]any {
	if err != nil {
		return map[string]any{"supported": false}
	}
	category := networkCategorySummary(profiles)
	return map[string]any{
		"supported":  true,
		"category":   category,
		"profiles":   profiles,
		"suggestLAN": mode != ListenerLAN && (category == "private" || category == "domain"),
	}
}

func networkCategorySummary(profiles []networkProfile) string {
	if len(profiles) == 0 {
		return "none"
	}
	seen := map[string]bool{}
	for _, profile := range profiles {
		seen[profile.Category] = true
	}
	switch {
	case seen["public"] || seen[""]:
		if len(seen) == 1 && seen["public"] {
			return "public"
		}
		return "mixed"
	case seen["private"] && seen["domain"]:
		return "private"
	case seen["domain"]:
		return "domain"
	default:
		return "private"
	}
}

// parseWindowsNetworkProfiles reads Get-NetConnectionProfile output converted
// to JSON: one object for a single network, an array for several.
func parseWindowsNetworkProfiles(output []byte) ([]networkProfile, error) {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return []networkProfile{}, nil
	}
	type rawProfile struct {
		Name            string `json:"Name"`
		InterfaceAlias  string `json:"InterfaceAlias"`
		NetworkCategory any    `json:"NetworkCategory"`
	}
	var list []rawProfile
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, err
		}
	} else {
		var single rawProfile
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, err
		}
		list = []rawProfile{single}
	}
	profiles := make([]networkProfile, 0, len(list))
	for _, raw := range list {
		profiles = append(profiles, networkProfile{
			Name: raw.Name, Interface: raw.InterfaceAlias,
			Category: windowsNetworkCategory(raw.NetworkCategory),
		})
	}
	return profiles, nil
}

// The enum serializes as a name or, on older PowerShell, as its number
// (0 Public, 1 Private, 2 DomainAuthenticated).
func windowsNetworkCategory(value any) string {
	switch typed := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "private", "1":
			return "private"
		case "domainauthenticated", "domain", "2":
			return "domain"
		case "public", "0":
			return "public"
		}
	case float64:
		switch typed {
		case 1:
			return "private"
		case 2:
			return "domain"
		case 0:
			return "public"
		}
	}
	return ""
}
