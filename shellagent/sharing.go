package shellagent

import (
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Phase 2b: sharing machines with other Dyner users. The contract lives in
// the dynapp repository, docs/mesh-network.md ("Agent enforcement").

// errNotShared is the frame error a scoped socket receives for anything
// outside its share.
const errNotShared = "Not shared with you"

// sharedCapabilityCeiling is everything a grantee identity can ever hold.
// Dyner always sends exactly this list; the agent clamps to it anyway.
var sharedCapabilityCeiling = []string{"net.protocol.http", "net.protocol.https", "net.tcp.connect", "net.udp.connect"}

// acceptedAccess is one active access row of the environment in a relay
// ticket: the browser auth token hash and the Dyner user it belongs to.
type acceptedAccess struct {
	AuthTokenHash string `json:"authTokenHash"`
	UserID        string `json:"userId"`
	Shared        bool   `json:"shared"`
}

// sessionSubject is the Dyner user a carrier vouches for. An empty UserID
// means the carrier does not know (older Dyner, LAN). Shared marks access
// that exists only through a share.
type sessionSubject struct {
	UserID string
	Shared bool
}

// relayAcceptedAccess builds the accepted token-hash set and the hash →
// subject map from a relay ticket. Legacy hashes have no known subject.
func relayAcceptedAccess(relay RelayConnection) (map[string]bool, map[string]sessionSubject) {
	accepted := make(map[string]bool, len(relay.AcceptedAuthTokenHashes)+len(relay.AcceptedAccess))
	subjects := make(map[string]sessionSubject, len(relay.AcceptedAccess))
	for _, hash := range relay.AcceptedAuthTokenHashes {
		if hash = strings.ToLower(strings.TrimSpace(hash)); hash != "" {
			accepted[hash] = true
		}
	}
	for _, access := range relay.AcceptedAccess {
		hash := strings.ToLower(strings.TrimSpace(access.AuthTokenHash))
		if hash == "" {
			continue
		}
		accepted[hash] = true
		subjects[hash] = sessionSubject{UserID: strings.TrimSpace(access.UserID), Shared: access.Shared}
	}
	return accepted, subjects
}

// identityMatchesSubject is the key-binding rule. A shared identity matches
// only a known, equal subject. Another identity bound to a user matches when
// the subject is unknown or equal. A shared subject never matches a
// non-shared identity, so a grantee cannot present the owner's key id.
func identityMatchesSubject(identity BrowserIdentity, subject sessionSubject) bool {
	if identity.Shared {
		return subject.UserID != "" && identity.UserID != "" && identity.UserID == subject.UserID
	}
	if subject.Shared {
		return false
	}
	return identity.UserID == "" || subject.UserID == "" || identity.UserID == subject.UserID
}

// combineDynerIdentities merges Dyner's browserIdentities and
// sharedIdentities into one list. Shared entries are marked, clamped to the
// grantee capability ceiling and stripped of anything that would make them a
// local approval. A malformed shared entry is dropped rather than failing the
// owner's sync.
func combineDynerIdentities(browser, shared []BrowserIdentity) ([]BrowserIdentity, error) {
	combined := make([]BrowserIdentity, 0, len(browser)+len(shared))
	for _, identity := range browser {
		identity.Shared = false
		identity.Targets = nil
		combined = append(combined, identity)
	}
	for _, identity := range shared {
		identity.Shared = true
		identity.UserID = strings.TrimSpace(identity.UserID)
		identity.Authority = false
		identity.ApprovedAt = 0
		identity.StoreID = ""
		identity.Declared = nil
		identity.Connect = nil
		identity.Capabilities = normalizePermissionSet(intersectStrings(identity.Capabilities, sharedCapabilityCeiling))
		identity.Targets = normalizeTargets(identity.Targets)
		if identity.UserID == "" || identity.Validate() != nil {
			continue
		}
		combined = append(combined, identity)
	}
	return normalizeBrowserIdentities(combined)
}

// targetKey is the canonical `host:port` form: lowercase host without IPv6
// brackets, decimal port.
func targetKey(host string, port int) string {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if host == "" || port < 1 || port > 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func parseTarget(value string) string {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return ""
	}
	return targetKey(host, port)
}

func normalizeTargets(values []string) []string {
	var result []string
	seen := map[string]bool{}
	for _, value := range values {
		key := parseTarget(value)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func unionTargets(left, right []string) []string {
	return normalizeTargets(append(append([]string(nil), left...), right...))
}

func targetAllowed(targets []string, host string, port int) bool {
	key := targetKey(host, port)
	if key == "" {
		return false
	}
	for _, target := range targets {
		if target == key {
			return true
		}
	}
	return false
}

// httpURLTarget returns the `host:port` an HTTP(S) URL reaches, with the
// scheme's default port.
func httpURLTarget(target *url.URL) (string, int, bool) {
	if target == nil || target.Hostname() == "" {
		return "", 0, false
	}
	port := 0
	switch strings.ToLower(target.Scheme) {
	case "http":
		port = 80
	case "https":
		port = 443
	default:
		return "", 0, false
	}
	if text := target.Port(); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return "", 0, false
		}
		port = parsed
	}
	return target.Hostname(), port, true
}

// scopedHTTPTargetAllowed reports whether an unscoped socket, or a scoped
// socket whose targets include the URL's host:port, may reach the URL.
func scopedHTTPTargetAllowed(auth *socketAuthentication, target *url.URL) bool {
	if auth == nil || !auth.scoped {
		return true
	}
	host, port, ok := httpURLTarget(target)
	return ok && targetAllowed(auth.targets, host, port)
}

// scopedNamedHosts lists the target hosts of a scoped socket. The owner named
// them when sharing, so they count as named for the private-address guard;
// the app's connect list still applies.
func scopedNamedHosts(auth *socketAuthentication) []string {
	if auth == nil || !auth.scoped {
		return nil
	}
	hosts := make([]string, 0, len(auth.targets))
	for _, target := range auth.targets {
		if host, _, err := net.SplitHostPort(target); err == nil {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// sharedScopeError is the single dispatch check for scoped sockets. It
// returns errNotShared for every frame a grantee may not send, and "" when
// the frame may proceed to the regular capability checks.
func sharedScopeError(auth *socketAuthentication, request message) string {
	if auth == nil || !auth.scoped {
		return ""
	}
	switch request.Type {
	case "ping", "net-data", "net-close", "channel":
		return ""
	case "net":
		switch request.Action {
		case "data", "close":
			return ""
		case "open":
			switch request.ProtocolName {
			case "tcp", "udp", "http", "https":
				if targetAllowed(auth.targets, request.Target.Host, request.Target.Port) {
					return ""
				}
			}
		}
	case "rpc":
		if request.Service == "http" && request.Method == "request" {
			parsed, err := url.Parse(stringValue(objectArg(request.Args, 0)["url"]))
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && scopedHTTPTargetAllowed(auth, parsed) {
				return ""
			}
		}
	}
	return errNotShared
}

// applySharedScope is defence in depth for every carrier: a socket whose key
// id belongs only to shared identities is scoped to their targets even if
// the carrier did not scope it.
func (s *Server) applySharedScope(auth socketAuthentication) socketAuthentication {
	if auth.keyID == "" || strings.HasPrefix(auth.keyID, nativeKeyPrefix) {
		return auth
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	var targets []string
	for _, identity := range s.Config.BrowserIdentities {
		if identity.KeyID != auth.keyID {
			continue
		}
		if !identity.Shared {
			return auth
		}
		found = true
		targets = unionTargets(targets, identity.Targets)
	}
	if found && !auth.scoped {
		auth.scoped = true
		auth.targets = targets
	}
	return auth
}

// sharedIdentityStillCovers reports whether the current shared identities for
// keyID still include every target a scoped socket was given. A share edit
// that removes a target revokes sockets that relied on it.
func sharedIdentityStillCovers(identities []BrowserIdentity, keyID string, targets []string) bool {
	var current []string
	for _, identity := range identities {
		if identity.KeyID == keyID && identity.Shared {
			current = unionTargets(current, identity.Targets)
		}
	}
	allowed := map[string]bool{}
	for _, target := range current {
		allowed[target] = true
	}
	for _, target := range targets {
		if !allowed[target] {
			return false
		}
	}
	return true
}
