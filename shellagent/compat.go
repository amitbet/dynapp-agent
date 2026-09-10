package shellagent

import (
	"encoding/json"
	"net/http"

	"github.com/amitbet/dynapp-agent/internal/agentutil"
	"github.com/amitbet/dynapp-agent/shellagent/catalog"
	"github.com/amitbet/dynapp-agent/shellagent/connect"
	"github.com/amitbet/dynapp-agent/shellagent/fs"
)

func objectArg(args []any, index int) map[string]any { return agentutil.ObjectArg(args, index) }
func sliceArg(args []any, index int) []any           { return agentutil.SliceArg(args, index) }
func numberArg(args []any, index, fallback, maximum int) int {
	return agentutil.NumberArg(args, index, fallback, maximum)
}
func stringArg(args []any, index int) string { return agentutil.StringArg(args, index) }
func boolArg(args []any, index int) bool     { return agentutil.BoolArg(args, index) }
func stringValue(value any) string           { return agentutil.StringValue(value) }
func objectValue(value any) map[string]any   { return agentutil.ObjectValue(value) }
func userHome() string                       { return agentutil.HomeDir() }
func safeName(value string) string           { return agentutil.SafeName(value) }
func randomToken(size int) string            { return agentutil.RandomToken(size) }

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func normalizePermissionSet(permissions []string) []string { return catalog.Normalize(permissions) }
func suggestedPermissions(declared []string) []string      { return catalog.Suggested(declared) }
func dangerousPermission(permission string) bool           { return catalog.IsDangerous(permission) }
func permissionDanger(permission string) string            { return catalog.Danger(permission) }
func permissionGroupID(permission string) string           { return catalog.GroupID(permission) }
func permissionTitle(permission string) string             { return catalog.Title(permission) }
func describePermissions(permissions []string) []catalog.Descriptor {
	return catalog.Describe(permissions)
}
func groupPermissions(permissions []string) []catalog.GroupSummary {
	return catalog.Group(permissions)
}

func httpConnectEntries(value any) []string { return connect.Entries(value) }
func httpConnectPatterns(value any, scheme string) ([]string, error) {
	return connect.Patterns(value, scheme)
}
func urlAllowedByHTTPConnect(raw string, patterns []string) bool {
	return connect.URLAllowed(raw, patterns)
}
func assertURLAllowedByHTTPConnect(raw string, patterns []string) error {
	return connect.AssertURLAllowed(raw, patterns)
}

func filesystemRoots() ([]map[string]string, error) { return fs.Roots() }
func createZip(sources []string, target string, options map[string]any) error {
	return fs.CreateZip(sources, target, options)
}
func watchSnapshot(root string) (map[string]any, error) { return fs.WatchSnapshot(root) }
func openDesktopPath(path string) error                 { return fs.OpenPath(path) }
func openDesktopPathWith(path, opener string) error     { return fs.OpenPathWith(path, opener) }
func openTerminal(command, cwd string) error            { return fs.OpenTerminal(command, cwd) }
