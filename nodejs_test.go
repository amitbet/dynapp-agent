package shellagent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNodeArchiveName(t *testing.T) {
	got, err := nodeArchiveName("darwin", "arm64", "v22.11.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "node-v22.11.0-darwin-arm64.tar.gz" {
		t.Fatalf("got %q", got)
	}
	got, err = nodeArchiveName("windows", "amd64", "22.11.0")
	if err != nil {
		t.Fatal(err)
	}
	if got != "node-v22.11.0-win-x64.zip" {
		t.Fatalf("got %q", got)
	}
}

func TestSelectLatestLTSPrefersNewestListedBuildForThisMachine(t *testing.T) {
	version, err := selectLatestLTS([]nodeDistRelease{
		{Version: "v23.0.0", LTS: false, Files: []string{"osx-arm64-tar"}},
		{Version: "v22.11.0", LTS: "Jod", Files: []string{"osx-arm64-tar", "linux-x64", "win-x64-zip"}},
		{Version: "v20.18.0", LTS: "Iron", Files: []string{"osx-arm64-tar"}},
	}, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if version != "v22.11.0" {
		t.Fatalf("got %q", version)
	}
}

func TestParseAndMergeLoginShellPath(t *testing.T) {
	if got := parseLoginShellPath("noise\n" + nodeLoginPathMarker + "/opt/homebrew/bin:/usr/bin\n"); got != "/opt/homebrew/bin:/usr/bin" {
		t.Fatalf("got %q", got)
	}
	merged := mergePathEntries("/opt/homebrew/bin:/usr/bin", "/usr/bin:/custom/bin")
	if merged != "/opt/homebrew/bin:/usr/bin:/custom/bin" {
		t.Fatalf("got %q", merged)
	}
}

func TestEnsureNodeRuntimeInstallsOfficialDistributionWhenMissing(t *testing.T) {
	previousHydrated := pathHydrated
	pathHydrated = true
	t.Cleanup(func() { pathHydrated = previousHydrated })

	archiveName, err := nodeArchiveName(runtime.GOOS, runtime.GOARCH, "v22.11.0")
	if err != nil {
		t.Skip(err)
	}
	archiveBytes := fakeNodeArchive(t, archiveName)
	sum := sha256.Sum256(archiveBytes)
	checksumLine := hex.EncodeToString(sum[:]) + "  " + archiveName + "\n"
	fileTag, err := nodeDistFileTag(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprintf(w, `[{"version":"v22.11.0","lts":"Jod","files":[%q]}]`, fileTag)
		case "/v22.11.0/SHASUMS256.txt":
			_, _ = w.Write([]byte(checksumLine))
		case "/v22.11.0/" + archiveName:
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	previousBase := nodeDistBaseURL
	previousClient := nodeHTTPClient
	nodeDistBaseURL = server.URL
	nodeHTTPClient = server.Client()
	t.Cleanup(func() {
		nodeDistBaseURL = previousBase
		nodeHTTPClient = previousClient
	})

	emptyBin := t.TempDir()
	t.Setenv("PATH", emptyBin)
	stateDir := t.TempDir()
	if err := ensureNodeRuntime(stateDir); err != nil {
		t.Fatal(err)
	}
	if !nodeAndNpmAvailable() {
		t.Fatal("managed node/npm were not placed on PATH")
	}
	if !managedNodeAvailable(stateDir) {
		t.Fatal("managed node install is missing")
	}
}

func TestEnsureNodeRuntimeReusesPATHWhenNodeAlreadyExists(t *testing.T) {
	previousHydrated := pathHydrated
	pathHydrated = true
	t.Cleanup(func() { pathHydrated = previousHydrated })
	bin := t.TempDir()
	writeFakeNodeTools(t, bin)
	t.Setenv("PATH", bin)
	stateDir := t.TempDir()
	if err := ensureNodeRuntime(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "toolchain", "node")); err == nil {
		t.Fatal("downloaded Node.js even though it was already on PATH")
	}
}

func TestEnsureAuthoringDependenciesUsesManagedNpm(t *testing.T) {
	previousHydrated := pathHydrated
	pathHydrated = true
	t.Cleanup(func() { pathHydrated = previousHydrated })
	bin := t.TempDir()
	writeFakeNodeTools(t, bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := &Server{StateDir: t.TempDir()}
	if err := agent.ensureAuthoringDependencies(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules", ".installed")); err != nil {
		t.Fatal("npm install was not invoked")
	}
}

func TestAuthoringCommandInheritsAgentPath(t *testing.T) {
	t.Setenv("PATH", "/managed/node/bin"+string(os.PathListSeparator)+"/bin")
	child := authoringCommand("npm run live")
	if runtime.GOOS != "windows" {
		if len(child.Args) < 3 || child.Args[1] != "-c" {
			t.Fatalf("authoring shell args = %q, want /bin/sh -c", child.Args)
		}
	}
	found := false
	for _, entry := range child.Env {
		if strings.HasPrefix(entry, "PATH=") && strings.Contains(entry, "/managed/node/bin") {
			found = true
		}
	}
	if !found {
		t.Fatal("live preview command dropped the Node.js PATH Edit just installed")
	}
}

func fakeNodeArchive(t *testing.T, archiveName string) []byte {
	t.Helper()
	if strings.HasSuffix(archiveName, ".zip") {
		return fakeNodeZip(t)
	}
	return fakeNodeTarGz(t)
}

func fakeNodeTarGz(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	writeTarFile(t, tarWriter, "node-v22.11.0-test/bin/node", "#!/bin/sh\necho node\n", 0o755)
	writeTarFile(t, tarWriter, "node-v22.11.0-test/lib/node_modules/npm/bin/npm-cli.js", "#!/bin/sh\necho npm\n", 0o755)
	header := &tar.Header{
		Name:     "node-v22.11.0-test/bin/npm",
		Typeflag: tar.TypeSymlink,
		Linkname: "../lib/node_modules/npm/bin/npm-cli.js",
		Mode:     0o755,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func fakeNodeZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeZipFile(t, writer, "node-v22.11.0-test/node.exe", "@echo off\r\necho node\r\n")
	writeZipFile(t, writer, "node-v22.11.0-test/npm.cmd", "@echo off\r\necho npm\r\n")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeTarFile(t *testing.T, writer *tar.Writer, name, content string, mode int64) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, content); err != nil {
		t.Fatal(err)
	}
}

func writeZipFile(t *testing.T, writer *zip.Writer, name, content string) {
	t.Helper()
	file, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, content); err != nil {
		t.Fatal(err)
	}
}

func writeFakeNodeTools(t *testing.T, bin string) {
	t.Helper()
	nodeName := "node"
	npmName := "npm"
	script := "#!/bin/sh\nPATH=/bin:/usr/bin:$PATH\nif [ \"$1\" = \"install\" ]; then mkdir -p \"$PWD/node_modules\" && echo ok > \"$PWD/node_modules/.installed\"; fi\n"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
		npmName = "npm.cmd"
		script = "@echo off\r\nif \"%~1\"==\"install\" mkdir node_modules 2>nul & echo ok> node_modules\\.installed\r\n"
	}
	if err := os.WriteFile(filepath.Join(bin, nodeName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, npmName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
