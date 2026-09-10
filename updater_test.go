package shellagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// useTestReleaseKey swaps the embedded release public key for a fresh pair
// and returns a signer producing detached `.sig` payloads.
func useTestReleaseKey(t *testing.T) func(binary []byte) string {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	previous := releaseSigningPublicKey
	releaseSigningPublicKey = public
	t.Cleanup(func() { releaseSigningPublicKey = previous })
	return func(binary []byte) string {
		sum := sha256.Sum256(binary)
		return base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(hex.EncodeToString(sum[:]))))
	}
}

type fakeRelease struct {
	server     *httptest.Server
	binaryName string
	binary     []byte
	signature  string
	downloads  atomic.Int64
}

func newFakeRelease(t *testing.T, version string, binary []byte, sign func([]byte) string, withSignature bool) *fakeRelease {
	t.Helper()
	release := &fakeRelease{binary: binary}
	release.binaryName = fmt.Sprintf("dynapp-shell-agent-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		release.binaryName += ".exe"
	}
	release.signature = sign(binary)
	hash := sha256.Sum256(binary)
	release.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/amitbet/dynapp-agent/releases/latest":
			assets := []githubAsset{
				{Name: release.binaryName, BrowserURL: release.server.URL + "/agent", Size: int64(len(binary))},
				{Name: release.binaryName + ".sha256", BrowserURL: release.server.URL + "/checksum", Size: 80},
			}
			if withSignature {
				assets = append(assets, githubAsset{Name: release.binaryName + ".sig", BrowserURL: release.server.URL + "/signature", Size: 88})
			}
			_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v" + version, Assets: assets})
		case "/agent":
			release.downloads.Add(1)
			_, _ = w.Write(binary)
		case "/checksum":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(hash[:]), release.binaryName)
		case "/signature":
			_, _ = fmt.Fprintf(w, "%s\n", release.signature)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(release.server.Close)
	return release
}

func (release *fakeRelease) config(current string, onUpdate func(context.Context, string, string) error) SelfUpdateConfig {
	return SelfUpdateConfig{
		Enabled: true, Repository: "amitbet/dynapp-agent", Version: current,
		APIBaseURL: release.server.URL, HTTPClient: release.server.Client(), OnUpdate: onUpdate,
	}
}

func TestNewerRelease(t *testing.T) {
	for _, test := range []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.4", "1.2.3", true},
		{"1.3.0", "v1.2.99", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.2", "1.2.3", false},
		{"latest", "1.2.3", false},
	} {
		if got := newerRelease(test.latest, test.current); got != test.want {
			t.Errorf("newerRelease(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
		}
	}
}

func TestCheckSelfUpdateStagesButDoesNotApplyWithActiveBridge(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	var applied atomic.Int64
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: SelfUpdateConfig{HTTPClient: release.server.Client()}}
	bridges := newBridgeSet(&agent.activeBridges)
	client, remote := net.Pipe()
	defer client.Close()
	bridges.put("tcp_1", remote, nil)
	config := release.config("1.2.3", func(_ context.Context, _, _ string) error {
		applied.Add(1)
		return nil
	})
	agent.SelfUpdate = config
	agent.checkSelfUpdateWithVerifier(t.Context(), config, func(string) error { return nil })
	if got := applied.Load(); got != 0 {
		t.Fatalf("update callback count = %d, want 0", got)
	}
	updates, err := filepath.Glob(filepath.Join(agent.StateDir, "updates", release.binaryName))
	if err != nil || len(updates) != 1 {
		t.Fatalf("staged update = %v, %v", updates, err)
	}
	if _, err := os.Stat(updates[0]); err != nil {
		t.Fatal(err)
	}
	_ = bridges.take("tcp_1").Close()
}

func TestSelfUpdateRequiresDetachedSignatureAsset(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, false)
	var applied atomic.Int64
	agent := &Server{StateDir: t.TempDir()}
	config := release.config("1.2.3", func(_ context.Context, _, _ string) error {
		applied.Add(1)
		return nil
	})
	agent.SelfUpdate = config
	agent.checkSelfUpdateWithVerifier(t.Context(), config, func(string) error { return nil })
	if applied.Load() != 0 || release.downloads.Load() != 0 {
		t.Fatalf("release without .sig was downloaded (%d) or applied (%d)", release.downloads.Load(), applied.Load())
	}
}

func TestSelfUpdateRejectsInvalidSignature(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	// A signature produced by a different key over the same digest.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	sum := sha256.Sum256(release.binary)
	release.signature = base64.StdEncoding.EncodeToString(ed25519.Sign(other, []byte(hex.EncodeToString(sum[:]))))
	var applied atomic.Int64
	agent := &Server{StateDir: t.TempDir()}
	config := release.config("1.2.3", func(_ context.Context, _, _ string) error {
		applied.Add(1)
		return nil
	})
	agent.SelfUpdate = config
	agent.checkSelfUpdateWithVerifier(t.Context(), config, func(string) error { return nil })
	if applied.Load() != 0 {
		t.Fatal("binary with a foreign signature was applied")
	}
	if updates, _ := filepath.Glob(filepath.Join(agent.StateDir, "updates", release.binaryName)); len(updates) != 0 {
		t.Fatalf("unsigned binary was staged: %v", updates)
	}
}

func TestStagedUpdateIsReVerifiedBeforeUse(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	agent := &Server{StateDir: t.TempDir()}
	updateDir := filepath.Join(agent.StateDir, "updates")
	if err := os.MkdirAll(updateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(updateDir, release.binaryName)
	if err := os.WriteFile(staged, []byte("tampered while waiting"), 0o700); err != nil {
		t.Fatal(err)
	}
	var appliedPath string
	config := release.config("1.2.3", func(_ context.Context, path, _ string) error {
		appliedPath = path
		return nil
	})
	agent.SelfUpdate = config
	agent.checkSelfUpdateWithVerifier(t.Context(), config, func(string) error { return nil })
	if appliedPath != staged {
		t.Fatalf("applied path = %q, want %q", appliedPath, staged)
	}
	if release.downloads.Load() != 1 {
		t.Fatalf("tampered staged binary was reused instead of re-downloaded (downloads = %d)", release.downloads.Load())
	}
	data, err := os.ReadFile(staged)
	if err != nil || string(data) != "new shell agent" {
		t.Fatalf("staged binary = %q, %v", data, err)
	}
	expected := stagedExpectation{SHA256: hex.EncodeToString(sumOf(release.binary)), Signature: release.signature}
	if err := verifyStagedAgent(staged, expected, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("tampered again"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyStagedAgent(staged, expected, func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("re-verification accepted a tampered binary: %v", err)
	}
}

func TestEmbeddedReleaseKeyMatchesPayloadSigningKey(t *testing.T) {
	key, err := parseEd25519PublicKey(releaseSigningPublicKeyPEM)
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("embedded key = %v, %v", key, err)
	}
	if !strings.Contains(releaseSigningPublicKeyPEM, "MCowBQYDK2VwAyEAqL2M/7uEco7+Osb1xCHI1bpkYhHv/yutsvak/JZhxWQ=") {
		t.Fatal("embedded key does not match keys/payload-signing.pub")
	}
}

func TestBeginUpdateBlocksNewBridges(t *testing.T) {
	server := &Server{}
	if !server.beginUpdate() {
		t.Fatal("beginUpdate unexpectedly failed")
	}
	if release, allowed := server.lockBridgeOpen(); allowed || release != nil {
		t.Fatal("bridge open was allowed during update")
	}
	server.cancelUpdate()
	if release, allowed := server.lockBridgeOpen(); !allowed || release == nil {
		t.Fatal("bridge open remained blocked after update cancellation")
	} else {
		release()
	}
}
