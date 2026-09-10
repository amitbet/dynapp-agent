package shellagent

import (
	"net/http"
	"os/exec"
	"strings"

	"github.com/amitbet/dynapp-agent/shellagent/apphost"
	"github.com/amitbet/dynapp-agent/shellagent/assoc"
	"github.com/amitbet/dynapp-agent/shellagent/rdp"
)

func (s *Server) ensureNodeRuntime() error {
	stateDir := s.StateDir
	if strings.TrimSpace(stateDir) == "" {
		resolved, err := DefaultStateDir()
		if err != nil {
			return err
		}
		stateDir = resolved
	}
	return apphost.EnsureNode(stateDir)
}

func authoringCommand(command string) *exec.Cmd { return apphost.AuthoringCommand(command) }

func (s *Server) ensureAuthoringDependencies(root string) error {
	stateDir := s.StateDir
	if strings.TrimSpace(stateDir) == "" {
		resolved, err := DefaultStateDir()
		if err != nil {
			return err
		}
		stateDir = resolved
	}
	return apphost.EnsureAuthoringDependencies(stateDir, root)
}

func materializeSourceSnapshot(client *http.Client, sourceURL, expectedDigest, destination string, headers scopedHeaders) error {
	return apphost.MaterializeSource(client, sourceURL, expectedDigest, destination, apphost.HeadersFrom(headers.origin, headers.token))
}

func materializeRunnableArchive(client *http.Client, runnableURL, expectedSHA256, destination string, headers scopedHeaders) error {
	return apphost.MaterializeRunnable(client, runnableURL, expectedSHA256, destination, apphost.HeadersFrom(headers.origin, headers.token))
}

type AssociationOptions = assoc.AssociationOptions

func InstallAssociations(options AssociationOptions) error { return assoc.InstallAssociations(options) }
func RemoveAssociations(appID string) error                { return assoc.RemoveAssociations(appID) }
func AssociationState(appID string) (map[string]any, error) {
	return assoc.AssociationState(appID)
}

func OpenHostedApp(appURL string, appName ...string) error {
	return assoc.OpenHostedApp(appURL, appName...)
}

func uniqueExtensions(values []string) []string { return assoc.UniqueExtensions(values) }

func hydrateExecutablePath() { apphost.HydratePath() }

func managedNodeAvailable(stateDir string) bool { return apphost.ManagedNodeAvailable(stateDir) }

func managedNodeBinDir(stateDir string) string { return apphost.ManagedNodeBinDir(stateDir) }

func prependPathDir(dir string) { apphost.PrependPathDir(dir) }

func copyHeaders(dst, src http.Header) { apphost.CopyHeaders(dst, src) }

func randomBridgeToken() string { return rdp.RandomBridgeToken() }

func integer64(value any) int64 {
	switch number := value.(type) {
	case int:
		return int64(number)
	case int64:
		return number
	case uint64:
		return int64(number)
	case float64:
		return int64(number)
	}
	return 0
}

type sourceSnapshotFile = apphost.SnapshotFile

func encodeSourcePack(files []sourceSnapshotFile, blobs map[string][]byte) []byte {
	return apphost.EncodeSourcePack(files, blobs)
}

func snapshotDigestMatches(expected string, document []byte) bool {
	return apphost.SnapshotDigestMatches(expected, document)
}

func shellQuote(value string) string { return assoc.ShellQuote(value) }
