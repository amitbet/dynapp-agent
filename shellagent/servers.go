package shellagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phase 3a: DynApp-aware servers. The contract lives in the dynapp
// repository, docs/mesh-network.md ("Phase 3a: DynApp-aware servers").

const (
	serversPermission     = "servers.connect"
	serverDescriptorPath  = "/.well-known/dynapp-server"
	serverChallengePath   = "/.well-known/dynapp-server/challenge"
	serverChallengeDomain = "dynapp-server-challenge/v1"
	// serverEntryLifetime drops discovery entries not re-verified in time.
	serverEntryLifetime = 10 * time.Minute
	// serverVerifyLifetime is how long a successful challenge lets HTTP
	// through and counts as "recently verified".
	serverVerifyLifetime = 60 * time.Second
	// serverLoopbackPreference keeps a fresh loopback announcement ahead of
	// the same instance seen on a LAN address.
	serverLoopbackPreference = 2 * time.Minute
	serverMaxProbePorts      = 4
	serverDiscoverBudget     = 3 * time.Second
	serverProbeConcurrency   = 64
	serverProbeDialTimeout   = 300 * time.Millisecond
	serverVerifyTimeout      = 5 * time.Second
	serverDescriptorLimit    = 64 * 1024
)

// ServerAPI is one API an app declares in its `servers.connect` permission.
// Max 0 means no upper bound.
type ServerAPI struct {
	API string `json:"api"`
	Min int    `json:"min,omitempty"`
	Max int    `json:"max,omitempty"`
}

type serverProvide struct {
	API string `json:"api"`
	V   int    `json:"v"`
}

func (api ServerAPI) satisfiedBy(provide serverProvide) bool {
	if api.API != provide.API {
		return false
	}
	minimum := api.Min
	if minimum < 1 {
		minimum = 1
	}
	return provide.V >= minimum && (api.Max == 0 || provide.V <= api.Max)
}

// serverDescriptor is GET /.well-known/dynapp-server after validation.
type serverDescriptor struct {
	V            int             `json:"v"`
	InstanceID   string          `json:"instanceId"`
	PublicKeyJWK BrowserJWK      `json:"publicKeyJwk"`
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Provides     []serverProvide `json:"provides"`
	Apps         []string        `json:"apps"`
	Issuer       string          `json:"issuer"`
}

func (d serverDescriptor) provides(apis []ServerAPI) bool {
	for _, provide := range d.Provides {
		for _, api := range apis {
			if api.satisfiedBy(provide) {
				return true
			}
		}
	}
	return false
}

// serverEntry is one verified instance in the discovery state.
type serverEntry struct {
	InstanceID string           `json:"instanceId"`
	Address    string           `json:"address"`
	Scheme     string           `json:"scheme"`
	Descriptor serverDescriptor `json:"descriptor"`
	Source     string           `json:"source"`
	VerifiedAt int64            `json:"verifiedAt"`
}

type serverVerification struct {
	scheme     string
	descriptor serverDescriptor
	at         time.Time
}

// serverRegistry holds the discovery state (one entry per instance id) and
// the verify cache (per address and instance id).
type serverRegistry struct {
	mu       sync.Mutex
	entries  map[string]serverEntry
	verified map[string]serverVerification
	now      func() time.Time
	// client overrides the HTTP client for descriptor and challenge calls.
	client *http.Client
	// mdnsBrowse and probeHosts are test seams for discovery.
	mdnsBrowse func(ctx context.Context) []serverCandidate
	probeHosts func() []net.IP
}

func (r *serverRegistry) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func verificationKey(address, instanceID string) string { return address + "\n" + instanceID }

// recordVerification caches one successful challenge.
func (r *serverRegistry) recordVerification(address, scheme string, descriptor serverDescriptor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verified == nil {
		r.verified = map[string]serverVerification{}
	}
	now := r.clock()
	for key, value := range r.verified {
		if now.Sub(value.at) >= serverVerifyLifetime {
			delete(r.verified, key)
		}
	}
	r.verified[verificationKey(address, descriptor.InstanceID)] = serverVerification{scheme: scheme, descriptor: descriptor, at: now}
}

// recordEntry adds or updates a discovery entry after a challenge succeeded
// at address. A fresh loopback announcement is not displaced by the same
// instance seen elsewhere.
func (r *serverRegistry) recordEntry(address, scheme, source string, descriptor serverDescriptor) {
	r.recordVerification(address, scheme, descriptor)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]serverEntry{}
	}
	now := r.clock()
	r.pruneLocked(now)
	if existing, ok := r.entries[descriptor.InstanceID]; ok && existing.Address != address && existing.Source == "announce" && source != "announce" &&
		now.Sub(time.UnixMilli(existing.VerifiedAt)) < serverLoopbackPreference {
		return
	}
	r.entries[descriptor.InstanceID] = serverEntry{
		InstanceID: descriptor.InstanceID, Address: address, Scheme: scheme,
		Descriptor: descriptor, Source: source, VerifiedAt: now.UnixMilli(),
	}
}

func (r *serverRegistry) pruneLocked(now time.Time) {
	for id, entry := range r.entries {
		if now.Sub(time.UnixMilli(entry.VerifiedAt)) >= serverEntryLifetime {
			delete(r.entries, id)
		}
	}
}

func (r *serverRegistry) list() []serverEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock())
	result := make([]serverEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Descriptor.Name != result[j].Descriptor.Name {
			return result[i].Descriptor.Name < result[j].Descriptor.Name
		}
		return result[i].InstanceID < result[j].InstanceID
	})
	return result
}

func (r *serverRegistry) knownScheme(address, instanceID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[instanceID]; ok && entry.Address == address {
		return entry.Scheme
	}
	if value, ok := r.verified[verificationKey(address, instanceID)]; ok {
		return value.scheme
	}
	return ""
}

// recentlyVerified reports whether address holds a server verified within
// serverVerifyLifetime over scheme whose provides matches one of apis.
func (r *serverRegistry) recentlyVerified(address, scheme string, apis []ServerAPI) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	for key, value := range r.verified {
		if !strings.HasPrefix(key, address+"\n") || value.scheme != scheme || now.Sub(value.at) >= serverVerifyLifetime {
			continue
		}
		if value.descriptor.provides(apis) {
			return true
		}
	}
	return false
}

// jwkThumbprint is the RFC 7638 thumbprint of a P-256 public key.
func jwkThumbprint(key BrowserJWK) string {
	canonical := `{"crv":` + strconv.Quote(key.CRV) + `,"kty":` + strconv.Quote(key.KTY) + `,"x":` + strconv.Quote(key.X) + `,"y":` + strconv.Quote(key.Y) + `}`
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func serverPublicKey(key BrowserJWK) (*ecdsa.PublicKey, error) {
	if key.KTY != "EC" || key.CRV != "P-256" {
		return nil, errors.New("server key is not a P-256 key")
	}
	x, err := base64.RawURLEncoding.DecodeString(key.X)
	if err != nil || len(x) != 32 {
		return nil, errors.New("server key is invalid")
	}
	y, err := base64.RawURLEncoding.DecodeString(key.Y)
	if err != nil || len(y) != 32 {
		return nil, errors.New("server key is invalid")
	}
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !public.Curve.IsOnCurve(public.X, public.Y) {
		return nil, errors.New("server key is not on the curve")
	}
	return public, nil
}

func serverChallengeMessage(nonce, address string) []byte {
	return []byte(serverChallengeDomain + "\n" + nonce + "\n" + address)
}

// verifyServerSignature checks a base64url P1363 ES256 signature over the
// challenge message with the descriptor key.
func verifyServerSignature(key BrowserJWK, nonce, address, encoded string) bool {
	public, err := serverPublicKey(key)
	if err != nil {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil || len(signature) != 64 {
		return false
	}
	hash := sha256.Sum256(serverChallengeMessage(nonce, address))
	return ecdsa.Verify(public, hash[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

// validateServerDescriptor applies the section 1 descriptor rules.
func validateServerDescriptor(d serverDescriptor) error {
	if d.V != 1 {
		return errors.New("server descriptor version is not supported")
	}
	if _, err := serverPublicKey(d.PublicKeyJWK); err != nil {
		return err
	}
	if d.InstanceID == "" || d.InstanceID != jwkThumbprint(d.PublicKeyJWK) {
		return errors.New("server instance id is not the thumbprint of its key")
	}
	if len([]rune(d.Name)) > 80 || len([]rune(d.Version)) > 40 || len(d.Issuer) > 200 {
		return errors.New("server descriptor field is too long")
	}
	if len(d.Provides) < 1 || len(d.Provides) > 8 {
		return errors.New("server descriptor must provide 1 to 8 APIs")
	}
	for _, provide := range d.Provides {
		if !validStoreID(provide.API) || provide.V < 1 {
			return errors.New("server descriptor names an invalid API")
		}
	}
	if len(d.Apps) > 8 {
		return errors.New("server descriptor lists too many apps")
	}
	for _, app := range d.Apps {
		if !validStoreID(app) {
			return errors.New("server descriptor names an invalid app")
		}
	}
	return nil
}

func (r *serverRegistry) httpClient() *http.Client {
	if r.client != nil {
		return r.client
	}
	return &http.Client{
		Timeout: serverVerifyTimeout,
		// The proof binds the dialed address; never follow a redirect away.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: serverVerifyTimeout},
	}
}

// verifyAt fetches the descriptor at scheme://address and runs a fresh
// challenge bound to address. expectedID, when set, must match.
func (r *serverRegistry) verifyAt(ctx context.Context, scheme, address, expectedID string) (serverDescriptor, error) {
	if scheme != "http" && scheme != "https" {
		return serverDescriptor{}, errors.New("server scheme must be http or https")
	}
	ctx, cancel := context.WithTimeout(ctx, serverVerifyTimeout)
	defer cancel()
	client := r.httpClient()
	base := scheme + "://" + address
	descriptorRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, base+serverDescriptorPath, nil)
	if err != nil {
		return serverDescriptor{}, err
	}
	descriptorRequest.Header.Set("Accept", "application/json")
	response, err := client.Do(descriptorRequest)
	if err != nil {
		return serverDescriptor{}, fmt.Errorf("server descriptor is unreachable: %w", err)
	}
	var descriptor serverDescriptor
	err = decodeServerJSON(response, &descriptor)
	if err != nil {
		return serverDescriptor{}, fmt.Errorf("server descriptor is invalid: %w", err)
	}
	if err := validateServerDescriptor(descriptor); err != nil {
		return serverDescriptor{}, err
	}
	if expectedID != "" && descriptor.InstanceID != expectedID {
		return serverDescriptor{}, errors.New("server instance id does not match")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return serverDescriptor{}, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	body, _ := json.Marshal(map[string]string{"nonce": nonce, "address": address})
	challengeRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, base+serverChallengePath, bytes.NewReader(body))
	if err != nil {
		return serverDescriptor{}, err
	}
	challengeRequest.Header.Set("Content-Type", "application/json")
	challengeRequest.Header.Set("Accept", "application/json")
	response, err = client.Do(challengeRequest)
	if err != nil {
		return serverDescriptor{}, fmt.Errorf("server challenge failed: %w", err)
	}
	var proof struct {
		InstanceID string `json:"instanceId"`
		Signature  string `json:"signature"`
	}
	if err := decodeServerJSON(response, &proof); err != nil {
		return serverDescriptor{}, fmt.Errorf("server challenge failed: %w", err)
	}
	if proof.InstanceID != descriptor.InstanceID || !verifyServerSignature(descriptor.PublicKeyJWK, nonce, address, proof.Signature) {
		return serverDescriptor{}, errors.New("server did not prove its instance key")
	}
	return descriptor, nil
}

func decodeServerJSON(response *http.Response, value any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, serverDescriptorLimit+1))
	if err != nil {
		return err
	}
	if len(data) > serverDescriptorLimit {
		return errors.New("response is too large")
	}
	return json.Unmarshal(data, value)
}

// verifyAny tries the given schemes in order.
func (r *serverRegistry) verifyAny(ctx context.Context, schemes []string, address, expectedID string) (string, serverDescriptor, error) {
	var last error
	for _, scheme := range schemes {
		descriptor, err := r.verifyAt(ctx, scheme, address, expectedID)
		if err == nil {
			return scheme, descriptor, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("server could not be verified")
	}
	return "", serverDescriptor{}, last
}

// parseServerConnect reads the `servers.connect` entries of a manifest's
// backendPermissions (or a hello's declaredPermissions).
func parseServerConnect(entries []any) ([]ServerAPI, []int) {
	var apis []ServerAPI
	var ports []int
	seenAPI := map[ServerAPI]bool{}
	seenPort := map[int]bool{}
	for _, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(entry["permission"])) != serversPermission {
			continue
		}
		list, _ := entry["apis"].([]any)
		for _, item := range list {
			raw, ok := item.(map[string]any)
			if !ok {
				continue
			}
			api := ServerAPI{API: strings.TrimSpace(stringValue(raw["api"])), Min: int(integer64(raw["min"])), Max: int(integer64(raw["max"]))}
			if !validStoreID(api.API) || api.Min < 0 || api.Max < 0 || (api.Max > 0 && api.Max < api.Min) {
				continue
			}
			if api.Min == 0 {
				api.Min = 1
			}
			if seenAPI[api] || len(apis) >= 4 {
				continue
			}
			seenAPI[api] = true
			apis = append(apis, api)
		}
		if probe, ok := entry["probe"].(map[string]any); ok {
			list, _ := probe["ports"].([]any)
			for _, item := range list {
				port := int(integer64(item))
				if port < 1 || port > 65535 || seenPort[port] || len(ports) >= serverMaxProbePorts {
					continue
				}
				seenPort[port] = true
				ports = append(ports, port)
			}
		}
	}
	return apis, ports
}

func manifestServerConnect(manifest map[string]any) ([]ServerAPI, []int) {
	entries, _ := manifest["backendPermissions"].([]any)
	return parseServerConnect(entries)
}

// serverHTTPTarget applies "HTTP to servers": it returns the `host:port` a
// socket may reach through a recently verified server that provides one of
// the APIs the app declared, without the address being in its connect list.
func (s *Server) serverHTTPTarget(auth *socketAuthentication, target *url.URL) (string, bool) {
	if auth == nil || len(auth.serverAPIs) == 0 {
		return "", false
	}
	// A grantee's share is its grant; anyone else needs servers.connect.
	if !auth.scoped && !socketAllows(auth.capabilities, serversPermission) {
		return "", false
	}
	host, port, ok := httpURLTarget(target)
	if !ok {
		return "", false
	}
	address := targetKey(host, port)
	if address == "" || !scopedHTTPTargetAllowed(auth, target) {
		return "", false
	}
	if !s.servers.recentlyVerified(address, strings.ToLower(target.Scheme), auth.serverAPIs) {
		return "", false
	}
	return address, true
}

// sameServerTarget reports whether a redirect hop stays on the verified
// address and scheme.
func sameServerTarget(target *url.URL, address, scheme string) bool {
	host, port, ok := httpURLTarget(target)
	return ok && strings.EqualFold(target.Scheme, scheme) && targetKey(host, port) == address
}

func serversRPCCapability(request message) string {
	if request.auth != nil && request.auth.scoped {
		// A grantee cannot hold servers.connect; the handler checks the
		// manifest declaration and the share targets instead.
		return ""
	}
	return serversPermission
}

func (s *Server) handleServersRPC(ctx context.Context, request message) (any, error) {
	auth := request.auth
	if auth != nil && auth.scoped && len(auth.serverAPIs) == 0 {
		return nil, errors.New("the app does not declare servers.connect")
	}
	options := objectArg(request.Args, 0)
	switch request.Method {
	case "list":
		if auth != nil && auth.scoped {
			return nil, errors.New(errNotShared)
		}
		return s.serversResult(), nil
	case "discover":
		if auth != nil && auth.scoped {
			return nil, errors.New(errNotShared)
		}
		ports, err := serverPortsArg(options["ports"])
		if err != nil {
			return nil, err
		}
		if len(ports) == 0 && auth != nil {
			ports = auth.serverProbePorts
		}
		s.discoverServers(ctx, ports)
		return s.serversResult(), nil
	case "verify":
		address := parseTarget(stringValue(options["address"]))
		instanceID := strings.TrimSpace(stringValue(options["instanceId"]))
		if address == "" {
			return nil, errors.New("server address must be host:port")
		}
		if len(instanceID) != 43 {
			return nil, errors.New("server instance id is invalid")
		}
		if auth != nil && auth.scoped && !scopedServerAddressAllowed(auth, address) {
			return nil, errors.New(errNotShared)
		}
		schemes := []string{"http", "https"}
		if scheme := strings.ToLower(stringValue(options["scheme"])); scheme != "" {
			if scheme != "http" && scheme != "https" {
				return nil, errors.New("server scheme must be http or https")
			}
			schemes = []string{scheme}
		} else if known := s.servers.knownScheme(address, instanceID); known != "" {
			schemes = []string{known}
		}
		scheme, descriptor, err := s.servers.verifyAny(ctx, schemes, address, instanceID)
		if err != nil {
			return nil, err
		}
		s.servers.recordVerification(address, scheme, descriptor)
		return map[string]any{"ok": true, "descriptor": descriptor}, nil
	}
	return nil, errors.New("unsupported servers method")
}

func scopedServerAddressAllowed(auth *socketAuthentication, address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	port, _ := strconv.Atoi(portText)
	return targetAllowed(auth.targets, host, port)
}

func serverPortsArg(value any) ([]int, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, errors.New("ports must be a list of integers")
	}
	if len(list) > serverMaxProbePorts {
		return nil, fmt.Errorf("at most %d ports may be probed", serverMaxProbePorts)
	}
	var ports []int
	seen := map[int]bool{}
	for _, item := range list {
		number, ok := item.(float64)
		port := int(number)
		if !ok || float64(port) != number || port < 1 || port > 65535 {
			return nil, errors.New("ports must be integers from 1 to 65535")
		}
		if !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	return ports, nil
}

// handleServersAnnounce is POST /v1/servers/announce: a server on this
// machine names its port; the agent verifies it at 127.0.0.1:<port>.
func (s *Server) handleServersAnnounce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !loopbackRequest(r) || !loopbackHostHeader(r.Host) {
		http.Error(w, "servers announce only from this computer", http.StatusForbidden)
		return
	}
	// Servers are not browsers. Refusing an Origin (and form content types)
	// keeps web pages from making the agent dial local ports.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browsers may not announce servers", http.StatusForbidden)
		return
	}
	if mediaType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); mediaType != "" && !strings.EqualFold(mediaType, "application/json") {
		http.Error(w, "announce must send application/json", http.StatusUnsupportedMediaType)
		return
	}
	reader := http.MaxBytesReader(w, r.Body, 4096)
	defer reader.Close()
	var payload struct {
		Port   json.Number `json:"port"`
		Scheme string      `json:"scheme"`
	}
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		http.Error(w, "invalid announce body", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(payload.Port.String())
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "port must be an integer from 1 to 65535", http.StatusBadRequest)
		return
	}
	scheme := strings.ToLower(strings.TrimSpace(payload.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		http.Error(w, "scheme must be http or https", http.StatusBadRequest)
		return
	}
	address := targetKey("127.0.0.1", port)
	descriptor, err := s.servers.verifyAt(r.Context(), scheme, address, "")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.servers.recordEntry(address, scheme, "announce", descriptor)
	writeJSON(w, map[string]any{"ok": true, "instanceId": descriptor.InstanceID})
}

// serverCandidate is an address discovery found but has not yet verified.
type serverCandidate struct {
	address    string
	scheme     string
	instanceID string
	source     string
}

// discoverServers runs an mDNS browse and a port probe within the discover
// budget and verifies every candidate it finds.
func (s *Server) discoverServers(ctx context.Context, ports []int) {
	ctx, cancel := context.WithTimeout(ctx, serverDiscoverBudget)
	defer cancel()
	candidates := make(chan serverCandidate, 256)
	var finders sync.WaitGroup
	finders.Add(2)
	go func() {
		defer finders.Done()
		browse := s.servers.mdnsBrowse
		if browse == nil {
			browse = browseDynappMDNS
		}
		for _, candidate := range browse(ctx) {
			select {
			case candidates <- candidate:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer finders.Done()
		if len(ports) == 0 {
			return
		}
		hosts := s.servers.probeHosts
		if hosts == nil {
			hosts = attachedIPv4Hosts
		}
		probeServerPorts(ctx, hosts(), ports, candidates)
	}()
	go func() {
		finders.Wait()
		close(candidates)
	}()
	seen := map[string]bool{}
	var verifiers sync.WaitGroup
	limit := make(chan struct{}, 16)
	for candidate := range candidates {
		key := candidate.scheme + "://" + candidate.address
		if seen[key] {
			continue
		}
		seen[key] = true
		verifiers.Add(1)
		go func(candidate serverCandidate) {
			defer verifiers.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			schemes := []string{candidate.scheme}
			if candidate.scheme == "" {
				schemes = []string{"http", "https"}
			}
			scheme, descriptor, err := s.servers.verifyAny(ctx, schemes, candidate.address, candidate.instanceID)
			if err == nil {
				s.servers.recordEntry(candidate.address, scheme, candidate.source, descriptor)
			}
		}(candidate)
	}
	verifiers.Wait()
}

// attachedIPv4Hosts lists every host of the private IPv4 /24 networks this
// machine is directly attached to, except its own addresses.
func attachedIPv4Hosts() []net.IP {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	own := map[string]bool{}
	networks := map[[3]byte]bool{}
	var order [][3]byte
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := prefix.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			own[ip.String()] = true
			key := [3]byte{ip[0], ip[1], ip[2]}
			if !networks[key] && len(order) < 4 {
				networks[key] = true
				order = append(order, key)
			}
		}
	}
	var hosts []net.IP
	for _, key := range order {
		for last := 1; last < 255; last++ {
			ip := net.IPv4(key[0], key[1], key[2], byte(last)).To4()
			if !own[ip.String()] {
				hosts = append(hosts, ip)
			}
		}
	}
	return hosts
}

// probeServerPorts dials every host on every port (64 at a time, 300 ms
// each) and reports each open one as a candidate.
func probeServerPorts(ctx context.Context, hosts []net.IP, ports []int, found chan<- serverCandidate) {
	limit := make(chan struct{}, serverProbeConcurrency)
	var group sync.WaitGroup
	dialer := &net.Dialer{Timeout: serverProbeDialTimeout}
outer:
	for _, host := range hosts {
		for _, port := range ports {
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				break outer
			}
			group.Add(1)
			go func(address string) {
				defer group.Done()
				defer func() { <-limit }()
				dialCtx, cancel := context.WithTimeout(ctx, serverProbeDialTimeout)
				defer cancel()
				connection, err := dialer.DialContext(dialCtx, "tcp", address)
				if err != nil {
					return
				}
				_ = connection.Close()
				select {
				case found <- serverCandidate{address: address, source: "probe"}:
				case <-ctx.Done():
				}
			}(net.JoinHostPort(host.String(), strconv.Itoa(port)))
		}
	}
	group.Wait()
}

func unionServerAPIs(left, right []ServerAPI) []ServerAPI {
	result := append([]ServerAPI(nil), left...)
	for _, api := range right {
		found := false
		for _, existing := range result {
			if existing == api {
				found = true
				break
			}
		}
		if !found {
			result = append(result, api)
		}
	}
	return result
}

// serversResult is the list/discover result. environmentId is this agent's
// Dyner environment ("" when not enrolled), so a browser on the local
// connection can register a server against the right environment.
func (s *Server) serversResult() map[string]any {
	s.mu.Lock()
	environmentID := s.Config.EnvironmentID
	s.mu.Unlock()
	return map[string]any{"environmentId": environmentID, "servers": s.servers.list()}
}
