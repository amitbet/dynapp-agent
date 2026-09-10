package shellagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// releaseSigningPublicKeyPEM is the Ed25519 public key that verifies detached
// signatures on release assets (`<asset>.sig` = base64(Ed25519(ASCII sha256-hex))).
const releaseSigningPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAN7uT22H3riIuApXQfQYREO7jr78DgAdwz6EwgPTc7as=
-----END PUBLIC KEY-----
`

// releaseSigningPublicKey is a variable so tests can substitute a key pair.
var releaseSigningPublicKey = mustParseEd25519PublicKey(releaseSigningPublicKeyPEM)

func mustParseEd25519PublicKey(pemText string) ed25519.PublicKey {
	key, err := parseEd25519PublicKey(pemText)
	if err != nil {
		panic(err)
	}
	return key
}

func parseEd25519PublicKey(pemText string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("release signing key is not a PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("release signing key is not Ed25519")
	}
	return key, nil
}

// verifyReleaseSignature checks a detached signature over the ASCII sha256
// hex digest of an asset.
func verifyReleaseSignature(sha256Hex, signatureBase64 string) error {
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureBase64))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("release signature is malformed")
	}
	if !ed25519.Verify(releaseSigningPublicKey, []byte(strings.ToLower(sha256Hex)), signature) {
		return errors.New("release signature does not match the binary")
	}
	return nil
}

// AgentVersion and AgentRepository are replaced by the release build with
// ldflags. Development builds keep self-update disabled by default.
var AgentVersion = "dev"
var AgentRepository = "amitbet/dynapp-agent"

const (
	DefaultSelfUpdateInterval = time.Hour
	maxAgentBinaryBytes       = 256 * 1024 * 1024
)

// SelfUpdateConfig describes the release poller. OnUpdate runs only after the
// downloaded binary passes its checksum and the agent has reserved the bridge
// set for shutdown.
type SelfUpdateConfig struct {
	Enabled    bool
	Repository string
	Version    string
	Interval   time.Duration
	APIBaseURL string
	HTTPClient *http.Client
	OnUpdate   func(context.Context, string, string) error
}

func DefaultSelfUpdateConfig() SelfUpdateConfig {
	return SelfUpdateConfig{
		Enabled:    isReleaseVersion(AgentVersion),
		Repository: AgentRepository,
		Version:    AgentVersion,
		Interval:   DefaultSelfUpdateInterval,
	}
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name       string `json:"name"`
	BrowserURL string `json:"browser_download_url"`
	Size       int64  `json:"size"`
}

func (s *Server) startSelfUpdater() {
	s.mu.Lock()
	if s.selfUpdateCancel != nil {
		s.mu.Unlock()
		return
	}
	config := s.SelfUpdate
	if config.Version == "" {
		config.Version = AgentVersion
	}
	if config.Repository == "" {
		config.Repository = AgentRepository
	}
	if config.Interval <= 0 {
		config.Interval = DefaultSelfUpdateInterval
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = "https://api.github.com"
	}
	if !config.Enabled || !isReleaseVersion(config.Version) || config.OnUpdate == nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.selfUpdateCancel = cancel
	s.mu.Unlock()
	go s.runSelfUpdater(ctx, config)
}

func (s *Server) runSelfUpdater(ctx context.Context, config SelfUpdateConfig) {
	// Check once at startup so an agent that was offline at release time does
	// not wait another hour. The ticker then keeps the normal one-hour cadence.
	s.checkSelfUpdate(ctx, config)
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkSelfUpdate(ctx, config)
		}
	}
}

func (s *Server) checkSelfUpdate(ctx context.Context, config SelfUpdateConfig) {
	s.checkSelfUpdateWithVerifier(ctx, config, verifyDownloadedAgentSignature)
}

func (s *Server) checkSelfUpdateWithVerifier(ctx context.Context, config SelfUpdateConfig, verifySignature func(string) error) {
	release, err := fetchLatestRelease(ctx, config)
	if err != nil {
		log.Printf("DynApp Shell agent: update check failed: %v", err)
		return
	}
	if !newerRelease(release.TagName, config.Version) {
		return
	}
	assets, err := selectAgentAssets(release, release.TagName)
	if err != nil {
		log.Printf("DynApp Shell agent: release %s has no %s asset: %v", release.TagName, runtime.GOOS, err)
		return
	}
	path, expected, err := s.stageAgentUpdate(ctx, assets, release.TagName, verifySignature)
	if err != nil {
		log.Printf("DynApp Shell agent: update download failed: %v", err)
		return
	}
	if s.ActiveBridges() != 0 {
		log.Printf("DynApp Shell agent: update %s downloaded; waiting for %d active bridge(s)", release.TagName, s.ActiveBridges())
		return
	}
	if !s.beginUpdate() {
		return
	}
	// The staged file sat on disk while bridges drained; verify it again right
	// before handing it to the update helper.
	if err := verifyStagedAgent(path, expected, verifySignature); err != nil {
		s.cancelUpdate()
		_ = os.Remove(path)
		log.Printf("DynApp Shell agent: staged update %s failed re-verification: %v", release.TagName, err)
		return
	}
	if err := config.OnUpdate(ctx, path, normalizedReleaseVersion(release.TagName)); err != nil {
		s.cancelUpdate()
		log.Printf("DynApp Shell agent: applying update %s failed: %v", release.TagName, err)
	}
}

func fetchLatestRelease(ctx context.Context, config SelfUpdateConfig) (githubRelease, error) {
	if !validRepository(config.Repository) {
		return githubRelease{}, errors.New("invalid GitHub repository")
	}
	base := strings.TrimRight(config.APIBaseURL, "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+config.Repository+"/releases/latest", nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "dynapp-shell-agent/"+config.Version)
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return githubRelease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return githubRelease{}, fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(&release); err != nil {
		return githubRelease{}, err
	}
	return release, nil
}

type agentReleaseAssets struct {
	Binary, Checksum, Signature githubAsset
}

// stagedExpectation is what a staged binary must still satisfy when reused.
type stagedExpectation struct {
	SHA256    string
	Signature string
}

func selectAgentAssets(release githubRelease, tag string) (agentReleaseAssets, error) {
	version := normalizedReleaseVersion(tag)
	base := fmt.Sprintf("dynapp-shell-agent-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		base += ".exe"
	}
	var assets agentReleaseAssets
	for _, asset := range release.Assets {
		switch asset.Name {
		case base:
			assets.Binary = asset
		case base + ".sha256":
			assets.Checksum = asset
		case base + ".sig":
			assets.Signature = asset
		}
	}
	if assets.Binary.Name == "" || assets.Checksum.Name == "" || assets.Binary.BrowserURL == "" || assets.Checksum.BrowserURL == "" {
		return agentReleaseAssets{}, errors.New("binary or checksum sidecar is missing")
	}
	if assets.Signature.Name == "" || assets.Signature.BrowserURL == "" {
		return agentReleaseAssets{}, errors.New("detached .sig signature asset is missing")
	}
	if assets.Binary.Size <= 0 || assets.Binary.Size > maxAgentBinaryBytes {
		return agentReleaseAssets{}, errors.New("binary size is invalid")
	}
	return assets, nil
}

// verifyStagedAgent re-checks a binary on disk: sha256, detached Ed25519
// signature, and the platform signature (codesign on macOS).
func verifyStagedAgent(path string, expected stagedExpectation, verifySignature func(string) error) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAgentBinaryBytes {
		return errors.New("staged update is not a regular file")
	}
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected.SHA256) {
		return errors.New("staged update checksum mismatch")
	}
	if err := verifyReleaseSignature(actual, expected.Signature); err != nil {
		return err
	}
	if verifySignature == nil {
		verifySignature = verifyDownloadedAgentSignature
	}
	return verifySignature(path)
}

func (s *Server) stageAgentUpdate(ctx context.Context, assets agentReleaseAssets, version string, verifySignature func(string) error) (string, stagedExpectation, error) {
	stateDir := s.StateDir
	if stateDir == "" {
		var err error
		stateDir, err = DefaultStateDir()
		if err != nil {
			return "", stagedExpectation{}, err
		}
	}
	updateDir := filepath.Join(stateDir, "updates")
	if err := os.MkdirAll(updateDir, 0o700); err != nil {
		return "", stagedExpectation{}, err
	}
	client := s.SelfUpdate.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	expectedSHA, err := downloadChecksum(ctx, client, assets.Checksum.BrowserURL)
	if err != nil {
		return "", stagedExpectation{}, err
	}
	signature, err := downloadSignature(ctx, client, assets.Signature.BrowserURL)
	if err != nil {
		return "", stagedExpectation{}, err
	}
	expected := stagedExpectation{SHA256: expectedSHA, Signature: signature}
	path := filepath.Join(updateDir, assets.Binary.Name)
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() && info.Size() > 0 {
		// A previously staged binary is only reused after it passes the same
		// checks a fresh download would.
		if err := verifyStagedAgent(path, expected, verifySignature); err == nil {
			return path, expected, nil
		}
		_ = os.Remove(path)
	}
	temporary, err := os.CreateTemp(updateDir, ".agent-*.download")
	if err != nil {
		return "", stagedExpectation{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := downloadAsset(ctx, client, assets.Binary.BrowserURL, temporary); err != nil {
		_ = temporary.Close()
		return "", stagedExpectation{}, err
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", stagedExpectation{}, err
	}
	if err := temporary.Close(); err != nil {
		return "", stagedExpectation{}, err
	}
	if err := verifyStagedAgent(temporaryPath, expected, verifySignature); err != nil {
		return "", stagedExpectation{}, fmt.Errorf("release %s: %w", version, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", stagedExpectation{}, err
	}
	return path, expected, nil
}

func downloadSignature(ctx context.Context, client *http.Client, assetURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("signature asset returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	signature := strings.TrimSpace(string(data))
	if decoded, err := base64.StdEncoding.DecodeString(signature); err != nil || len(decoded) != ed25519.SignatureSize {
		return "", errors.New("signature asset is invalid")
	}
	return signature, nil
}

func downloadAsset(ctx context.Context, client *http.Client, assetURL string, destination *os.File) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("asset returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxAgentBinaryBytes {
		return errors.New("asset is too large")
	}
	count, err := io.Copy(destination, io.LimitReader(response.Body, maxAgentBinaryBytes+1))
	if err != nil {
		return err
	}
	if count <= 0 || count > maxAgentBinaryBytes {
		return errors.New("asset size is invalid")
	}
	return nil
}

func downloadChecksum(ctx context.Context, client *http.Client, assetURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("checksum asset returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", errors.New("checksum asset is invalid")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("checksum asset is not hexadecimal")
	}
	return fields[0], nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		for _, char := range part {
			if !(char == '-' || char == '_' || char == '.' || char >= '0' && char <= '9' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z') {
				return false
			}
		}
	}
	return true
}

func normalizedReleaseVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	return value
}

func isReleaseVersion(value string) bool {
	parts := strings.Split(normalizedReleaseVersion(value), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func newerRelease(latest, current string) bool {
	if !isReleaseVersion(latest) || !isReleaseVersion(current) {
		return false
	}
	parse := func(value string) [3]int64 {
		parts := strings.Split(normalizedReleaseVersion(value), ".")
		var result [3]int64
		for index, part := range parts {
			result[index], _ = strconv.ParseInt(part, 10, 64)
		}
		return result
	}
	left, right := parse(latest), parse(current)
	for index := range left {
		if left[index] != right[index] {
			return left[index] > right[index]
		}
	}
	return false
}
