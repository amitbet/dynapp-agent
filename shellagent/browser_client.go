package shellagent

import (
	"strings"
)

const (
	maxBrowserClientField = 80
	browserClientModePWA  = "pwa"
	browserClientModeTab  = "tab"
)

func (client BrowserClient) Empty() bool {
	return client.Browser == "" && client.Version == "" && client.Platform == "" && !client.Mobile && client.Mode == "" && client.Label == ""
}

func applyBrowserClient(identity *BrowserIdentity, incoming *BrowserClient) bool {
	if identity == nil {
		return false
	}
	next := normalizeBrowserClient(incoming)
	if next.Empty() || next == identity.Client {
		return false
	}
	identity.Client = next
	return true
}

func normalizeBrowserClient(incoming *BrowserClient) BrowserClient {
	if incoming == nil {
		return BrowserClient{}
	}
	mode := strings.ToLower(strings.TrimSpace(incoming.Mode))
	if mode != browserClientModePWA && mode != browserClientModeTab {
		mode = ""
	}
	normalized := BrowserClient{
		Browser:  clipBrowserClientField(incoming.Browser),
		Version:  clipBrowserClientField(incoming.Version),
		Platform: clipBrowserClientField(incoming.Platform),
		Mobile:   incoming.Mobile,
		Mode:     mode,
		Label:    clipBrowserClientField(incoming.Label),
	}
	if normalized.Empty() {
		return BrowserClient{}
	}
	if normalized.Label == "" {
		normalized.Label = formatBrowserClientLabel(normalized)
	}
	return normalized
}

func formatBrowserClientLabel(client BrowserClient) string {
	name := strings.TrimSpace(client.Browser + " " + client.Version)
	parts := make([]string, 0, 3)
	if name != "" {
		parts = append(parts, name)
	}
	if platform := strings.TrimSpace(client.Platform); platform != "" {
		if len(parts) > 0 {
			parts[0] = parts[0] + " on " + platform
		} else {
			parts = append(parts, platform)
		}
	}
	switch client.Mode {
	case browserClientModePWA:
		parts = append(parts, "installed app")
	case browserClientModeTab:
		parts = append(parts, "browser tab")
	}
	return strings.Join(parts, " · ")
}

func clipBrowserClientField(value string) string {
	trimmed := strings.TrimSpace(value)
	count := 0
	var builder strings.Builder
	for _, char := range trimmed {
		if char < 32 || char == 127 {
			continue
		}
		count++
		if count > maxBrowserClientField {
			break
		}
		builder.WriteRune(char)
	}
	return strings.TrimSpace(builder.String())
}
