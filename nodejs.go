package shellagent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	maxNodeArchiveBytes = 128 << 20
	nodeDistIndexLimit  = 2 << 20
	nodeChecksumLimit   = 1 << 20
	nodeLoginPathMarker = "__DYNAPP_LOGIN_PATH__"
)

var (
	nodeDistBaseURL = "https://nodejs.org/dist"
	nodeHTTPClient  = &http.Client{Timeout: 5 * time.Minute}
	nodeRuntimeMu   sync.Mutex
	pathHydrationMu sync.Mutex
	pathHydrated    = false
)

type nodeDistRelease struct {
	Version string   `json:"version"`
	LTS     any      `json:"lts"`
	Files   []string `json:"files"`
}

func (s *Server) ensureNodeRuntime() error {
	stateDir := s.StateDir
	if strings.TrimSpace(stateDir) == "" {
		resolved, err := DefaultStateDir()
		if err != nil {
			return err
		}
		stateDir = resolved
	}
	return ensureNodeRuntime(stateDir)
}

func hydrateExecutablePath() {
	pathHydrationMu.Lock()
	defer pathHydrationMu.Unlock()
	if pathHydrated {
		return
	}
	pathHydrated = true
	if err := inheritLoginShellPath(); err != nil {
		log.Printf("DynApp Shell agent: could not read the login-shell PATH: %v", err)
	}
	for _, dir := range extraExecutableDirs() {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			prependPathDir(dir)
		}
	}
}

func extraExecutableDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	if runtime.GOOS == "windows" {
		dirs = append(dirs,
			filepath.Join(os.Getenv("ProgramFiles"), "nodejs"),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "nodejs"),
			filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "nodejs"),
		)
	}
	return dirs
}

func inheritLoginShellPath() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		} else {
			shell = "/bin/sh"
		}
	}
	if !filepath.IsAbs(shell) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, shell, "-lc", fmt.Sprintf("printf '%s%%s\\n' \"$PATH\"", nodeLoginPathMarker))
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return err
	}
	path := parseLoginShellPath(string(output))
	if path == "" {
		return nil
	}
	os.Setenv("PATH", mergePathEntries(path, os.Getenv("PATH")))
	return nil
}

func parseLoginShellPath(output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], nodeLoginPathMarker) {
			return strings.TrimSpace(strings.TrimPrefix(lines[i], nodeLoginPathMarker))
		}
	}
	return ""
}

func mergePathEntries(primary, fallback string) string {
	seen := map[string]bool{}
	entries := []string{}
	for _, value := range []string{primary, fallback} {
		for _, entry := range strings.Split(value, string(os.PathListSeparator)) {
			if entry == "" || seen[entry] {
				continue
			}
			seen[entry] = true
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

func prependPathDir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	os.Setenv("PATH", mergePathEntries(dir, os.Getenv("PATH")))
}

func nodeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "node.exe"
	}
	return "node"
}

func npmBinaryName() string {
	if runtime.GOOS == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

func nodeAndNpmAvailable() bool {
	if _, err := exec.LookPath("node"); err != nil {
		if _, err := exec.LookPath(nodeBinaryName()); err != nil {
			return false
		}
	}
	if _, err := exec.LookPath("npm"); err != nil {
		if _, err := exec.LookPath(npmBinaryName()); err != nil {
			return false
		}
	}
	return true
}

func managedNodeBinDir(stateDir string) string {
	root := filepath.Join(stateDir, "toolchain", "node")
	if runtime.GOOS == "windows" {
		return root
	}
	return filepath.Join(root, "bin")
}

func managedNodeAvailable(stateDir string) bool {
	binDir := managedNodeBinDir(stateDir)
	nodePath := filepath.Join(binDir, nodeBinaryName())
	npmPath := filepath.Join(binDir, npmBinaryName())
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(filepath.Join(binDir, "npm")); err != nil {
			return false
		}
	}
	if _, err := os.Stat(nodePath); err != nil {
		return false
	}
	if _, err := os.Stat(npmPath); err != nil {
		if runtime.GOOS == "windows" {
			if _, err := os.Stat(filepath.Join(binDir, "npm")); err != nil {
				return false
			}
		} else {
			return false
		}
	}
	return true
}

func ensureNodeRuntime(stateDir string) error {
	nodeRuntimeMu.Lock()
	defer nodeRuntimeMu.Unlock()
	hydrateExecutablePath()
	if nodeAndNpmAvailable() {
		return nil
	}
	if managedNodeAvailable(stateDir) {
		prependPathDir(managedNodeBinDir(stateDir))
		if nodeAndNpmAvailable() {
			return nil
		}
	}
	if err := installOfficialNode(stateDir); err != nil {
		return fmt.Errorf("Node.js and npm are required to edit this app, but they are not installed: %w", err)
	}
	prependPathDir(managedNodeBinDir(stateDir))
	if !nodeAndNpmAvailable() {
		return errors.New("Node.js was installed but npm is still not on PATH")
	}
	return nil
}

func nodeArchiveName(goos, goarch, version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return "", errors.New("Node.js version is missing")
	}
	arch, err := nodeDistArch(goos, goarch)
	if err != nil {
		return "", err
	}
	if goos == "windows" {
		return fmt.Sprintf("node-v%s-win-%s.zip", version, arch), nil
	}
	osName := goos
	if goos == "darwin" {
		osName = "darwin"
	}
	return fmt.Sprintf("node-v%s-%s-%s.tar.gz", version, osName, arch), nil
}

func nodeDistArch(goos, goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	case "386":
		if goos == "windows" {
			return "x86", nil
		}
	}
	return "", fmt.Errorf("this machine's CPU (%s/%s) has no official Node.js build", goos, goarch)
}

func nodeDistFileTag(goos, goarch string) (string, error) {
	arch, err := nodeDistArch(goos, goarch)
	if err != nil {
		return "", err
	}
	switch goos {
	case "darwin":
		return "osx-" + arch + "-tar", nil
	case "linux":
		return "linux-" + arch, nil
	case "windows":
		return "win-" + arch + "-zip", nil
	default:
		return "", fmt.Errorf("this OS (%s) has no official Node.js build", goos)
	}
}

func selectLatestLTS(releases []nodeDistRelease, goos, goarch string) (string, error) {
	wanted, err := nodeDistFileTag(goos, goarch)
	if err != nil {
		return "", err
	}
	for _, release := range releases {
		if !isNodeLTS(release.LTS) {
			continue
		}
		if !releaseHasFile(release.Files, wanted) {
			continue
		}
		version := strings.TrimSpace(release.Version)
		if version == "" {
			continue
		}
		return version, nil
	}
	return "", errors.New("nodejs.org did not list an LTS build for this machine")
}

func isNodeLTS(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return false
	}
}

func releaseHasFile(files []string, wanted string) bool {
	for _, file := range files {
		if file == wanted {
			return true
		}
	}
	return false
}

func installOfficialNode(stateDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	releases, err := fetchNodeIndex(ctx)
	if err != nil {
		return err
	}
	version, err := selectLatestLTS(releases, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	archiveName, err := nodeArchiveName(runtime.GOOS, runtime.GOARCH, version)
	if err != nil {
		return err
	}
	log.Printf("DynApp Shell agent: installing Node.js %s for app authoring", version)
	toolchain := filepath.Join(stateDir, "toolchain")
	if err := os.MkdirAll(toolchain, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(toolchain, ".node-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	archivePath := filepath.Join(staging, archiveName)
	versionPath := version
	if !strings.HasPrefix(versionPath, "v") {
		versionPath = "v" + versionPath
	}
	archiveURL := strings.TrimRight(nodeDistBaseURL, "/") + "/" + versionPath
	if err := downloadNodeFile(ctx, archiveURL+"/"+archiveName, archivePath, maxNodeArchiveBytes); err != nil {
		return err
	}
	checksums, err := downloadNodeChecksums(ctx, archiveURL+"/SHASUMS256.txt")
	if err != nil {
		return err
	}
	expected, ok := checksums[archiveName]
	if !ok {
		return fmt.Errorf("nodejs.org omitted a checksum for %s", archiveName)
	}
	actual, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("Node.js archive checksum mismatch for %s", archiveName)
	}
	extracted := filepath.Join(staging, "extracted")
	if err := os.MkdirAll(extracted, 0o700); err != nil {
		return err
	}
	if err := extractNodeArchive(archivePath, extracted); err != nil {
		return err
	}
	source, err := nodeRootFromExtracted(extracted)
	if err != nil {
		return err
	}
	destination := filepath.Join(toolchain, "node")
	_ = os.RemoveAll(destination)
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return nil
}

func fetchNodeIndex(ctx context.Context) ([]nodeDistRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(nodeDistBaseURL, "/")+"/index.json", nil)
	if err != nil {
		return nil, err
	}
	response, err := nodeHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("nodejs.org index returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, nodeDistIndexLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > nodeDistIndexLimit {
		return nil, errors.New("nodejs.org index is too large")
	}
	var releases []nodeDistRelease
	if err := json.Unmarshal(data, &releases); err != nil || len(releases) == 0 {
		return nil, errors.New("nodejs.org index is invalid")
	}
	return releases, nil
}

func downloadNodeChecksums(ctx context.Context, checksumURL string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := nodeHTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("nodejs.org checksums returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, nodeChecksumLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > nodeChecksumLimit {
		return nil, errors.New("nodejs.org checksum list is too large")
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			continue
		}
		checksums[filepath.Base(fields[1])] = fields[0]
	}
	if len(checksums) == 0 {
		return nil, errors.New("nodejs.org checksum list is empty")
	}
	return checksums, nil
}

func downloadNodeFile(ctx context.Context, fileURL, destination string, maxBytes int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return err
	}
	response, err := nodeHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Node.js download returned HTTP %d", response.StatusCode)
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	count, err := io.Copy(file, io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if count <= 0 || count > maxBytes {
		return errors.New("Node.js archive size is invalid")
	}
	return nil
}

func extractNodeArchive(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(file, magic[:2]); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if magic[0] == 0x1f && magic[1] == 0x8b {
		return extractTarGz(file, destination)
	}
	return extractZipArchive(archivePath, destination)
}

func extractTarGz(file *os.File, destination string) error {
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	var root string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if root == "" {
			root = firstPathComponent(header.Name)
		}
		relative := stripSharedRoot(header.Name, root)
		if relative == "" {
			continue
		}
		target, err := safeArchiveTarget(destination, relative)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := header.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(output, reader); err != nil {
				output.Close()
				return err
			}
			if err := output.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func extractZipArchive(archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	root := ""
	for _, entry := range reader.File {
		if root == "" {
			root = firstPathComponent(entry.Name)
		}
		relative := stripSharedRoot(entry.Name, root)
		if relative == "" {
			continue
		}
		target, err := safeArchiveTarget(destination, relative)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		mode := entry.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func firstPathComponent(name string) string {
	normalized := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	normalized = strings.TrimSuffix(normalized, "/")
	if i := strings.Index(normalized, "/"); i >= 0 {
		return normalized[:i]
	}
	return normalized
}

func stripSharedRoot(name, root string) string {
	normalized := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	if root == "" {
		return strings.TrimSuffix(normalized, "/")
	}
	if normalized == root || normalized == root+"/" {
		return ""
	}
	prefix := root + "/"
	if strings.HasPrefix(normalized, prefix) {
		return strings.TrimSuffix(strings.TrimPrefix(normalized, prefix), "/")
	}
	return strings.TrimSuffix(normalized, "/")
}

func safeArchiveTarget(destination, relative string) (string, error) {
	if relative == "" || strings.HasPrefix(relative, "/") {
		return "", errors.New("Node.js archive contains an unsafe path")
	}
	for _, part := range strings.Split(relative, "/") {
		if part == ".." {
			return "", errors.New("Node.js archive contains an unsafe path")
		}
	}
	target := filepath.Join(destination, filepath.FromSlash(relative))
	if !pathInside(destination, target) {
		return "", errors.New("Node.js archive contains an unsafe path")
	}
	return target, nil
}

func nodeRootFromExtracted(extracted string) (string, error) {
	if looksLikeNodeRoot(extracted) {
		return extracted, nil
	}
	entries, err := os.ReadDir(extracted)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(extracted, entry.Name())
		if looksLikeNodeRoot(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("Node.js archive did not contain node and npm")
}

func looksLikeNodeRoot(root string) bool {
	unixNode := filepath.Join(root, "bin", "node")
	unixNpm := filepath.Join(root, "bin", "npm")
	windowsNode := filepath.Join(root, "node.exe")
	windowsNpm := filepath.Join(root, "npm.cmd")
	if _, err := os.Stat(unixNode); err == nil {
		if _, err := os.Stat(unixNpm); err == nil {
			return true
		}
	}
	if _, err := os.Stat(windowsNode); err == nil {
		if _, err := os.Stat(windowsNpm); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(root, "npm")); err == nil {
			return true
		}
	}
	return false
}
