package apphost

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func EnsureNode(stateDir string) error { return ensureNodeRuntime(stateDir) }

func AuthoringCommand(command string) *exec.Cmd {
	var child *exec.Cmd
	if runtime.GOOS == "windows" {
		child = exec.Command("cmd.exe", "/d", "/s", "/c", command)
	} else {
		child = exec.Command("/bin/sh", "-c", command)
	}
	child.Env = os.Environ()
	return child
}

func EnsureAuthoringDependencies(stateDir, root string) error {
	if err := ensureNodeRuntime(stateDir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "package.json")); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); err == nil {
		return nil
	}
	child := exec.Command("npm", "install", "--no-audit", "--no-fund")
	child.Dir = root
	child.Env = os.Environ()
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	return child.Run()
}

type scopedHeaders struct {
	origin string
	token  string
}

func HeadersFrom(origin, token string) scopedHeaders {
	return scopedHeaders{origin: strings.ToLower(strings.TrimSpace(origin)), token: strings.TrimSpace(token)}
}

func scopedBearerHeaders(base, token string) scopedHeaders {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return scopedHeaders{}
	}
	return scopedHeaders{origin: strings.ToLower(parsed.Scheme + "://" + parsed.Host), token: strings.TrimSpace(token)}
}

func (h scopedHeaders) forURL(target string) http.Header {
	headers := http.Header{}
	if h.token == "" || h.origin == "" {
		return headers
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return headers
	}
	if strings.ToLower(parsed.Scheme+"://"+parsed.Host) == h.origin {
		headers.Set("Authorization", "Bearer "+h.token)
	}
	return headers
}

func MaterializeSource(client *http.Client, sourceURL, expectedDigest, destination string, headers scopedHeaders) error {
	return materializeSourceSnapshot(client, sourceURL, expectedDigest, destination, headers)
}

func MaterializeRunnable(client *http.Client, runnableURL, expectedSHA256, destination string, headers scopedHeaders) error {
	return materializeRunnableArchive(client, runnableURL, expectedSHA256, destination, headers)
}

func HydratePath() { hydrateExecutablePath() }

func ManagedNodeAvailable(stateDir string) bool { return managedNodeAvailable(stateDir) }

func ManagedNodeBinDir(stateDir string) string { return managedNodeBinDir(stateDir) }

func PrependPathDir(dir string) { prependPathDir(dir) }

func CopyHeaders(dst, src http.Header) { copyHeaders(dst, src) }

type SnapshotFile = sourceSnapshotFile

func SnapshotDigestMatches(expected string, document []byte) bool {
	return snapshotDigestMatches(expected, document)
}

func EncodeSourcePack(files []sourceSnapshotFile, blobs map[string][]byte) []byte {
	return encodeSourcePack(files, blobs)
}
