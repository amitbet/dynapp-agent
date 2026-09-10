package shellagent

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const ConfigSchemaVersion = 1

const (
	ListenerOff   = "off"
	ListenerLocal = "local"
	ListenerLAN   = "lan"
)

// DefaultDynerBaseURL matches the packaged Electron Shell and web runtime.
// Local development can override it with DYNER_BASE_URL or --dyner-url.
const compiledDynerBaseURL = "https://dynapp.io"

// Config is local agent state, not application payload state. The device
// credential is deliberately never exposed by the WebSocket protocol.
type Config struct {
	SchemaVersion          int               `json:"schemaVersion"`
	DynerBaseURL           string            `json:"dynerBaseUrl,omitempty"`
	EnvironmentID          string            `json:"environmentId,omitempty"`
	DeviceCredential       string            `json:"deviceCredential,omitempty"`
	ListenerName           string            `json:"listenerName,omitempty"`
	ListenerMode           string            `json:"listenerMode,omitempty"`
	RelayEnabled           bool              `json:"relayEnabled"`
	LANEnabled             bool              `json:"lanEnabled,omitempty"`
	LANAddress             string            `json:"lanAddress,omitempty"`
	HostedEnvironmentID    string            `json:"hostedEnvironmentId,omitempty"`
	HostedDeviceCredential string            `json:"hostedDeviceCredential,omitempty"`
	HostedName             string            `json:"hostedName,omitempty"`
	BrowserIdentities      []BrowserIdentity `json:"browserIdentities,omitempty"`
	// AppDomain is the hosted app domain (`https://<owner>-<slug>.<appDomain>`).
	// When empty it is derived from DynerBaseURL.
	AppDomain string `json:"appDomain,omitempty"`
	// AuthorityOrigins lists additional origins (besides the Dyner base
	// origin) whose Dyner client may decide permissions for this agent. It is
	// meant for LOCAL_RUN development servers.
	AuthorityOrigins []string `json:"authorityOrigins,omitempty"`
	// CatalogAppID is the Dyner catalog app (`owner/slug`) that hosts the
	// permission screen. Defaults to DefaultCatalogAppID.
	CatalogAppID string `json:"catalogAppId,omitempty"`
}

// DefaultCatalogAppID is the store id of the Dyner client unless configured.
const DefaultCatalogAppID = "amit-bet/dyner"

func (config Config) catalogAppID() string {
	if id := strings.TrimSpace(config.CatalogAppID); validStoreID(id) {
		return id
	}
	return DefaultCatalogAppID
}

// isAuthorityOrigin reports whether a browser origin may act as the
// permission authority: the Dyner base origin or a configured extra origin.
func (config Config) isAuthorityOrigin(origin string) bool {
	parsed, ok := parseOrigin(origin)
	if !ok {
		return false
	}
	canonical := strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	if base := config.dynerBaseOrigin(); base != "" && canonical == base {
		return true
	}
	// Dyner has two supported browser entry points: the catalog origin and the
	// catalog app's own hosted PWA origin. Trust only the exact hosted catalog
	// origin here. Other *.dynapp.io apps must still go through app grants.
	if hosted := config.catalogHostedOrigin(); hosted != "" && canonical == hosted {
		return true
	}
	for _, candidate := range config.AuthorityOrigins {
		if extra, ok := parseOrigin(candidate); ok && strings.ToLower(extra.Scheme+"://"+extra.Host) == canonical {
			return true
		}
	}
	return false
}

func (config Config) catalogHostedOrigin() string {
	domain := config.appDomain()
	if domain == "" {
		return ""
	}
	owner, slug, found := strings.Cut(config.catalogAppID(), "/")
	if !found || owner == "" || slug == "" {
		return ""
	}
	host := strings.ToLower(strings.ReplaceAll(owner, "-", "") + "-" + slug + "." + domain)
	return "https://" + host
}

// reviewURL is where the Dyner client shows one pending permission request.
func (config Config) reviewURL(requestID string) string {
	base := strings.TrimRight(strings.TrimSpace(config.DynerBaseURL), "/")
	if base == "" {
		base = compiledDynerBaseURL
	}
	return base + "/app/" + config.catalogAppID() + "#/permissions?request=" + url.QueryEscape(requestID)
}

// BrowserIdentity contains only the public half of a PWA Shell identity. The
// private Web Crypto key is non-extractable and remains in the browser profile.
type BrowserIdentity struct {
	KeyID        string     `json:"keyId"`
	PublicKeyJWK BrowserJWK `json:"publicKeyJwk"`
	Origin       string     `json:"origin"`
	Capabilities []string   `json:"capabilities"`
	// StoreID binds the pairing to one app (`owner/slug`). Identities synced
	// from Dyner pairing have no store id and match any app.
	StoreID string `json:"storeId,omitempty"`
	// Connect holds the approved HTTP connect allowlist per scheme.
	Connect map[string][]string `json:"connect,omitempty"`
	// ApprovedAt is the local approval time in Unix milliseconds. Zero means
	// the identity came from Dyner and is replaced on every sync.
	ApprovedAt int64 `json:"approvedAt,omitempty"`
	// Declared is the manifest permission set the authority reviewed when it
	// decided. Permissions declared later are requested as a delta; ones the
	// authority deliberately left unchecked are not asked again.
	Declared []string `json:"declared,omitempty"`
	// Authority marks the Dyner client identity that decides permissions.
	Authority bool `json:"authority,omitempty"`
	// Client is a short, non-secret description of the browser that holds
	// this key (name, OS, tab vs installed app). It is display-only.
	Client BrowserClient `json:"client,omitempty"`
}

// BrowserClient is what the Permissions list uses to tell one key from
// another. It is supplied by the browser at handshake time and sanitized
// before it is stored.
type BrowserClient struct {
	Browser  string `json:"browser,omitempty"`
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
	Mobile   bool   `json:"mobile,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Label    string `json:"label,omitempty"`
}

type BrowserJWK struct {
	KTY string `json:"kty"`
	CRV string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func DefaultStateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "DynApp", "shell-agent"), nil
}

func ConfigPath(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		var err error
		stateDir, err = DefaultStateDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(stateDir, "config.json"), nil
}

func LoadConfig(stateDir string) (Config, error) {
	path, err := ConfigPath(stateDir)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{SchemaVersion: ConfigSchemaVersion}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, err
	}
	if config.SchemaVersion != ConfigSchemaVersion {
		return Config{}, errors.New("unsupported shell-agent configuration version")
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	if config.SchemaVersion != ConfigSchemaVersion {
		return errors.New("unsupported shell-agent configuration version")
	}
	switch config.ListenerMode {
	case "", ListenerOff, ListenerLocal, ListenerLAN:
	default:
		return errors.New("listener mode must be off, local, or lan")
	}
	if config.LANEnabled || config.ListenerMode == ListenerLocal || config.ListenerMode == ListenerLAN {
		if config.LANAddress == "" {
			return errors.New("listener address is required when the local listener is enabled")
		}
	}
	for _, identity := range config.BrowserIdentities {
		if err := identity.Validate(); err != nil {
			return err
		}
	}
	if config.DynerBaseURL != "" && !AllowedDynerBaseURL(config.DynerBaseURL) {
		return errors.New("Dyner base URL must use HTTPS (or HTTP on a private/local host)")
	}
	if err := validateDeviceCredential(config.EnvironmentID, config.DeviceCredential); err != nil {
		return err
	}
	if err := validateDeviceCredential(config.HostedEnvironmentID, config.HostedDeviceCredential); err != nil {
		return err
	}
	if (config.DeviceCredential != "" || config.HostedDeviceCredential != "") && config.DynerBaseURL == "" {
		return errors.New("Dyner base URL is required when a device credential is stored")
	}
	return nil
}

func validateDeviceCredential(environmentID, credential string) error {
	if credential == "" && environmentID == "" {
		return nil
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 2 || parts[0] != environmentID || len(parts[1]) < 40 {
		return errors.New("invalid DynApp device credential")
	}
	return nil
}

func AllowedDynerBaseURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && isLocalNetworkHost(parsed.Hostname())
}

func isLocalNetworkHost(hostname string) bool {
	host := strings.ToLower(strings.Trim(hostname, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	if host == "::1" || host == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func DefaultDynerBaseURL() string {
	for _, name := range []string{"DYNER_BASE_URL", "DYNAPP_DYNER_URL"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return compiledDynerBaseURL
}

func DefaultListenerName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "This device"
	}
	return strings.TrimSpace(name)
}

func EnvEnabled(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// DefaultListenerCapabilities is the environment capability ceiling announced
// to Dyner at registration. Per-socket grants never come from it; they derive
// from the synced browser identity intersected with the app manifest.
func DefaultListenerCapabilities() []string {
	// Registration advertises the agent's complete capability ceiling. Actual
	// access still comes from a browser-identity grant intersected with the app
	// manifest. Keeping a second hand-maintained subset here caused newly added
	// granular permissions (for example fs.home and fs.list) to be silently
	// dropped by Dyner before they could ever reach a remote agent.
	return capabilities()
}

func loopbackAddressForPort(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func lanAddressForPort(port int) string {
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
}

func (config *Config) ApplyListenerDefaults(loopbackAddress string, enableLAN bool) {
	if strings.TrimSpace(config.DynerBaseURL) == "" {
		config.DynerBaseURL = DefaultDynerBaseURL()
	} else {
		config.DynerBaseURL = strings.TrimRight(strings.TrimSpace(config.DynerBaseURL), "/")
	}
	if strings.TrimSpace(config.ListenerName) == "" {
		config.ListenerName = DefaultListenerName()
	}
	if config.ListenerMode == "" {
		if config.LANEnabled {
			config.ListenerMode = ListenerLAN
		} else {
			config.ListenerMode = ListenerLocal
		}
	}
	if enableLAN {
		config.ListenerMode = ListenerLAN
	}
	port := 9011
	if loopbackAddress != "" {
		port = portFromAddress(loopbackAddress)
	}
	switch config.ListenerMode {
	case ListenerLAN:
		config.LANEnabled = true
		config.LANAddress = lanAddressForPort(port)
	case ListenerOff:
		config.LANEnabled = false
	default:
		config.ListenerMode = ListenerLocal
		config.LANEnabled = true
		config.LANAddress = loopbackAddressForPort(port)
	}
}

func (config Config) ListenerEndpoint() string {
	port := portFromAddress(config.LANAddress)
	if config.ListenerMode == ListenerLAN {
		endpoints := lanEndpoints(config.LANAddress, port, config.DynerBaseURL)
		if len(endpoints) > 0 {
			return endpoints[0]
		}
	}
	return "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + RemotePath
}

func (config Config) relayTicketConfig() Config {
	if config.HostedDeviceCredential != "" {
		config.DeviceCredential = config.HostedDeviceCredential
		config.EnvironmentID = config.HostedEnvironmentID
	}
	return config
}

func (identity BrowserIdentity) Validate() error {
	if identity.PublicKeyJWK.KTY != "EC" || identity.PublicKeyJWK.CRV != "P-256" ||
		len(identity.PublicKeyJWK.X) != 43 || len(identity.PublicKeyJWK.Y) != 43 ||
		len(identity.KeyID) != 43 || !validIdentityOrigin(identity.Origin) {
		return errors.New("invalid browser identity")
	}
	if identity.StoreID != "" && !validStoreID(identity.StoreID) {
		return errors.New("browser identity store id is invalid")
	}
	if BrowserKeyID(identity.PublicKeyJWK) != identity.KeyID {
		return errors.New("browser identity key id does not match its public key")
	}
	return nil
}

// validIdentityOrigin accepts HTTPS origins and plain-HTTP loopback
// development origins.
func validIdentityOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && loopbackHostname(parsed.Hostname())
}

// AppDomain returns the hosted app domain. It is explicit configuration, or
// otherwise derived from the Dyner base URL host. Private or IP-literal Dyner
// hosts have no wildcard app domain; only the base origin counts as hosted.
func (config Config) appDomain() string {
	if domain := strings.ToLower(strings.TrimSpace(config.AppDomain)); domain != "" {
		return strings.TrimPrefix(domain, ".")
	}
	base := strings.TrimSpace(config.DynerBaseURL)
	if base == "" {
		base = compiledDynerBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "dynapp.io"
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || isLocalNetworkHost(host) || net.ParseIP(host) != nil {
		return ""
	}
	return strings.TrimPrefix(host, "www.")
}

// dynerBaseOrigin is the scheme://host[:port] of the configured Dyner server.
func (config Config) dynerBaseOrigin() string {
	base := strings.TrimSpace(config.DynerBaseURL)
	if base == "" {
		base = compiledDynerBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

func SaveConfig(stateDir string, config Config) error {
	config.SchemaVersion = ConfigSchemaVersion
	if err := config.Validate(); err != nil {
		return err
	}
	path, err := ConfigPath(stateDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// Redacted returns account diagnostics without ever displaying a credential.
func (config Config) Redacted() map[string]any {
	return map[string]any{
		"schemaVersion":  config.SchemaVersion,
		"dynerBaseUrl":   config.DynerBaseURL,
		"environmentId":  config.EnvironmentID,
		"enrolled":       config.DeviceCredential != "",
		"relayEnabled":   config.RelayEnabled,
		"lanEnabled":     config.LANEnabled,
		"lanAddress":     config.LANAddress,
		"listenerMode":   config.resolvedListenerMode(),
		"listenerName":   config.ListenerName,
		"hostedEnrolled": config.HostedDeviceCredential != "",
	}
}

func (config Config) resolvedListenerMode() string {
	if config.ListenerMode != "" {
		return config.ListenerMode
	}
	if config.LANEnabled {
		return ListenerLAN
	}
	return ListenerLocal
}
