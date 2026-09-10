package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var storeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)

func validStoreID(value string) bool {
	return len(value) <= 160 && storeIDPattern.MatchString(value)
}

func storeSlug(storeID string) string {
	if index := strings.LastIndex(storeID, "/"); index >= 0 {
		return storeID[index+1:]
	}
	return storeID
}

// appIDMatches reports whether a frame's appId names the bound store id (full
// id or slug).
func appIDMatches(boundStoreID, frameAppID string) bool {
	if frameAppID == "" {
		return true
	}
	if boundStoreID == "" {
		return false
	}
	return frameAppID == boundStoreID || strings.EqualFold(frameAppID, storeSlug(boundStoreID))
}

func loopbackHostname(hostname string) bool {
	host := strings.ToLower(strings.Trim(strings.TrimSpace(hostname), "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackHostHeader accepts `127.0.0.1[:port]`, `localhost[:port]`, and
// `[::1][:port]` Host headers. Anything else indicates DNS rebinding.
func loopbackHostHeader(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		return loopbackHostname(hostname)
	}
	return loopbackHostname(host)
}

type originKind int

const (
	originRejected originKind = iota
	originDevelopment
	originHosted
	originIdentity
)

func parseOrigin(value string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, false
	}
	return parsed, true
}

// classifyOrigin applies the transport allowlist: hosted app origins, stored
// identity origins, and loopback development origins.
func classifyOrigin(config Config, identities []BrowserIdentity, origin string) originKind {
	parsed, ok := parseOrigin(origin)
	if !ok {
		return originRejected
	}
	host := strings.ToLower(parsed.Hostname())
	if loopbackHostname(host) {
		return originDevelopment
	}
	canonical := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	if config.isAuthorityOrigin(canonical) {
		return originHosted
	}
	if parsed.Scheme == "https" {
		if domain := config.appDomain(); domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
			return originHosted
		}
	}
	for _, identity := range identities {
		if identity.Origin == canonical || originEquivalent(identity.Origin, canonical) {
			return originIdentity
		}
	}
	return originRejected
}

func (s *Server) originAllowed(origin string) bool {
	s.mu.Lock()
	config := s.Config
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	return classifyOrigin(config, identities, origin) != originRejected
}

// directLocalInit is the first browser record of the direct-local handshake.
type directLocalInit struct {
	Type         string          `json:"type"`
	Version      int             `json:"version"`
	PublicKeyJWK *BrowserJWK     `json:"publicKeyJwk"`
	App          *directLocalApp `json:"app"`
	Client       *BrowserClient  `json:"client"`
}

type directLocalApp struct {
	StoreID             string         `json:"storeId"`
	Revision            any            `json:"revision"`
	DeclaredPermissions []any          `json:"declaredPermissions"`
	Connect             map[string]any `json:"connect"`
}

// resolvedApp is what the agent knows about the connecting app before it asks
// the user for approval.
type resolvedApp struct {
	StoreID     string
	Name        string
	Development bool
	Declared    []string
	Connect     map[string][]string
}

func (s *Server) resolveApp(ctx context.Context, origin string, init directLocalInit) (resolvedApp, error) {
	if init.App == nil || !validStoreID(strings.TrimSpace(init.App.StoreID)) {
		return resolvedApp{}, errors.New("an app store id is required")
	}
	storeID := strings.TrimSpace(init.App.StoreID)
	s.mu.Lock()
	config := s.Config
	identities := append([]BrowserIdentity(nil), s.Config.BrowserIdentities...)
	s.mu.Unlock()
	switch classifyOrigin(config, identities, origin) {
	case originDevelopment:
		connect, err := connectFromInit(init.App.Connect)
		if err != nil {
			return resolvedApp{}, err
		}
		return resolvedApp{
			StoreID: storeID, Name: storeSlug(storeID), Development: true,
			Declared: normalizePermissionList(init.App.DeclaredPermissions), Connect: connect,
		}, nil
	case originHosted:
		return s.resolveHostedApp(ctx, config, origin, storeID)
	default:
		return resolvedApp{}, errors.New("origin is not allowed to pair")
	}
}

func connectFromInit(value map[string]any) (map[string][]string, error) {
	result := map[string][]string{}
	for _, scheme := range []string{"http", "https"} {
		entries := httpConnectEntries(value[scheme])
		if len(entries) == 0 {
			continue
		}
		patterns, err := httpConnectPatterns(entries, scheme)
		if err != nil {
			return nil, err
		}
		result[scheme] = patterns
	}
	return result, nil
}

func normalizePermissionList(values []any) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		id := ""
		switch entry := value.(type) {
		case string:
			id = entry
		case map[string]any:
			id = stringValue(entry["permission"])
		}
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 80 || seen[id] || strings.ContainsAny(id, " \t\n\"'\\") {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// resolveHostedApp fetches the public app record from Dyner without any
// credential and derives declared permissions from the latest manifest.
func (s *Server) resolveHostedApp(ctx context.Context, config Config, origin, storeID string) (resolvedApp, error) {
	detail, err := s.fetchDynerAppDetail(ctx, config, storeID)
	if err != nil {
		return resolvedApp{}, err
	}
	return appFromDynerDetail(config, origin, storeID, detail)
}

// resolveManifestApp resolves an app's manifest through Dyner without the
// hosted-origin check. Relay and LAN sockets and the authority identity use
// it to intersect grants with the manifest's declared permissions.
func (s *Server) resolveManifestApp(ctx context.Context, config Config, storeID string) (resolvedApp, error) {
	if !validStoreID(storeID) {
		return resolvedApp{}, errors.New("an app store id is required")
	}
	detail, err := s.fetchDynerAppDetail(ctx, config, storeID)
	if err != nil {
		return resolvedApp{}, err
	}
	return appFromDynerManifest(storeID, detail)
}

func (s *Server) fetchDynerAppDetail(ctx context.Context, config Config, storeID string) (map[string]any, error) {
	base := strings.TrimRight(strings.TrimSpace(config.DynerBaseURL), "/")
	if base == "" {
		base = compiledDynerBaseURL
	}
	owner, slug, _ := strings.Cut(storeID, "/")
	endpoint := base + "/api/v1/apps/" + url.PathEscape(owner) + "/" + url.PathEscape(slug)
	client := s.dynerHTTPClient()
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("could not load the app from Dyner: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Dyner returned HTTP %d for %s", response.StatusCode, storeID)
	}
	var detail map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&detail); err != nil {
		return nil, errors.New("Dyner returned invalid app details")
	}
	return detail, nil
}

func appFromDynerDetail(config Config, origin, storeID string, detail map[string]any) (resolvedApp, error) {
	canonical := strings.ToLower(strings.TrimSpace(origin))
	app, _ := detail["app"].(map[string]any)
	hosted := stringList(detail["hostedOrigins"])
	if len(hosted) == 0 && app != nil {
		hosted = stringList(app["hostedOrigins"])
	}
	if len(hosted) > 0 {
		allowed := false
		for _, candidate := range hosted {
			if strings.ToLower(strings.TrimRight(strings.TrimSpace(candidate), "/")) == canonical {
				allowed = true
			}
		}
		if !allowed {
			return resolvedApp{}, errors.New("origin is not a hosted origin of this app")
		}
	} else if !legacyHostedOriginMatches(config, canonical, storeID) {
		return resolvedApp{}, errors.New("origin does not match this app's hosted origin")
	}
	return appFromDynerManifest(storeID, detail)
}

// appFromDynerManifest derives the app name, declared permissions, and
// connect lists from a Dyner app record's latest revision.
func appFromDynerManifest(storeID string, detail map[string]any) (resolvedApp, error) {
	app, _ := detail["app"].(map[string]any)
	revision, _ := detail["latestRevision"].(map[string]any)
	if revision == nil {
		if revisions, _ := detail["revisions"].([]any); len(revisions) > 0 {
			revision, _ = revisions[0].(map[string]any)
		}
	}
	if revision == nil {
		return resolvedApp{}, errors.New("this app has no published revision")
	}
	manifest, _ := revision["manifest"].(map[string]any)
	if manifest == nil {
		return resolvedApp{}, errors.New("this app's revision has no manifest")
	}
	name := strings.TrimSpace(stringValue(manifest["name"]))
	if app != nil {
		if value := strings.TrimSpace(stringValue(app["name"])); value != "" {
			name = value
		}
	}
	if name == "" {
		name = storeSlug(storeID)
	}
	declared, connect, err := manifestBackendPermissions(manifest)
	if err != nil {
		return resolvedApp{}, err
	}
	return resolvedApp{StoreID: storeID, Name: name, Declared: declared, Connect: connect}, nil
}

// legacyHostedOriginMatches covers Dyner servers that do not yet return
// hostedOrigins: the app subdomain is `<owner-without-hyphens>-<slug>` and the
// catalog runs on the Dyner base origin.
func legacyHostedOriginMatches(config Config, origin, storeID string) bool {
	if base := config.dynerBaseOrigin(); base != "" && origin == base {
		return true
	}
	parsed, ok := parseOrigin(origin)
	if !ok || parsed.Scheme != "https" {
		return false
	}
	domain := config.appDomain()
	if domain == "" {
		return false
	}
	owner, slug, _ := strings.Cut(storeID, "/")
	expected := strings.ReplaceAll(owner, "-", "") + "-" + slug + "." + domain
	return strings.EqualFold(parsed.Hostname(), expected)
}

func manifestBackendPermissions(manifest map[string]any) ([]string, map[string][]string, error) {
	entries, _ := manifest["backendPermissions"].([]any)
	declared := normalizePermissionList(entries)
	rawConnect := map[string][]string{}
	for _, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		permission := strings.TrimSpace(stringValue(entry["permission"]))
		if permission != "net.protocol.http" && permission != "net.protocol.https" {
			continue
		}
		scheme := strings.TrimPrefix(permission, "net.protocol.")
		rawConnect[scheme] = append(rawConnect[scheme], httpConnectEntries(entry["connect"])...)
	}
	if network, ok := manifest["net"].(map[string]any); ok {
		for _, scheme := range []string{"http", "https"} {
			if protocol, ok := network["protocol"].(map[string]any); ok {
				if item, ok := protocol[scheme].(map[string]any); ok {
					rawConnect[scheme] = append(rawConnect[scheme], httpConnectEntries(item["connect"])...)
				}
			}
			if item, ok := network["protocol."+scheme].(map[string]any); ok {
				rawConnect[scheme] = append(rawConnect[scheme], httpConnectEntries(item["connect"])...)
			}
		}
	}
	connect := map[string][]string{}
	for scheme, entries := range rawConnect {
		if len(entries) == 0 {
			continue
		}
		patterns, err := httpConnectPatterns(entries, scheme)
		if err != nil {
			return nil, nil, fmt.Errorf("manifest connect list for %s is invalid", scheme)
		}
		connect[scheme] = patterns
	}
	return declared, connect, nil
}

func stringList(value any) []string {
	switch list := value.(type) {
	case []any:
		result := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	case []string:
		return list
	}
	return nil
}

func (s *Server) dynerHTTPClient() *http.Client {
	if s.DynerHTTPClient != nil {
		return s.DynerHTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}
