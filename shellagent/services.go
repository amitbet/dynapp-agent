package shellagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

func (s *Server) serviceDir(name string) (string, error) {
	root := s.StateDir
	if root == "" {
		var err error
		root, err = DefaultStateDir()
		if err != nil {
			return "", err
		}
	}
	path := filepath.Join(root, name)
	return path, os.MkdirAll(path, 0o700)
}

func (s *Server) handleCalendarRPC(ctx context.Context, request message) (any, error) {
	if runtime.GOOS != "darwin" {
		if request.Method == "authorizationStatus" {
			return map[string]any{"status": "unsupported", "canRequest": false}, nil
		}
		if request.Method == "requestAccess" {
			return map[string]any{"granted": false, "status": "unsupported"}, nil
		}
		return []any{}, nil
	}
	executable, err := findCalendarHelper()
	if err != nil {
		return nil, err
	}
	command := ""
	arguments := []string{}
	switch request.Method {
	case "authorizationStatus":
		command = "status"
	case "requestAccess":
		command = "request-access"
	case "listCalendars":
		command = "list-calendars"
	case "listEvents":
		command = "list-events"
		options := objectArg(request.Args, 0)
		start, end := stringValue(options["start"]), stringValue(options["end"])
		begin, beginErr := time.Parse(time.RFC3339, start)
		finish, finishErr := time.Parse(time.RFC3339, end)
		if beginErr != nil || finishErr != nil || !finish.After(begin) || finish.Sub(begin) > 366*24*time.Hour {
			return nil, errors.New("calendar range must be positive and no longer than 366 days")
		}
		arguments = []string{begin.UTC().Format(time.RFC3339), finish.UTC().Format(time.RFC3339)}
	default:
		return nil, errors.New("unsupported calendar method")
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(runCtx, executable, append([]string{command}, arguments...)...).Output()
	if err != nil {
		return nil, err
	}
	var result any
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, err
	}
	return result, nil
}
func findCalendarHelper() (string, error) {
	executable, _ := os.Executable()
	candidates := []string{filepath.Join(filepath.Dir(executable), "native", "calendar-helper"), filepath.Join(filepath.Dir(executable), "calendar-helper"), filepath.Join("shell", "native", "bin", "calendar-helper")}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("the macOS calendar helper is unavailable")
}

type searchConfig struct {
	Roots      []string `json:"roots"`
	Exclusions []string `json:"exclusions"`
	Count      int      `json:"count"`
	UpdatedAt  string   `json:"updatedAt,omitempty"`
}
type searchEntry struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Ext     string `json:"ext"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	MtimeMs int64  `json:"mtimeMs"`
}

func (s *Server) searchPaths() (string, string, error) {
	root, err := s.serviceDir("file-search")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(root, "config.json"), filepath.Join(root, "index.jsonl"), nil
}
func (s *Server) loadSearchConfig() (searchConfig, error) {
	configPath, _, err := s.searchPaths()
	if err != nil {
		return searchConfig{}, err
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return searchConfig{Roots: defaultFileSearchRoots()}, nil
	}
	if err != nil {
		return searchConfig{}, err
	}
	var config searchConfig
	err = json.Unmarshal(data, &config)
	return config, err
}

func defaultFileSearchRoots() []string {
	roots, err := filesystemRoots()
	if err != nil {
		return []string{userHome()}
	}
	return defaultFileSearchRootsFrom(runtime.GOOS, userHome(), roots)
}

func defaultFileSearchRootsFrom(platform, home string, roots []map[string]string) []string {
	paths := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, root := range roots {
		path := filepath.Clean(root["path"])
		if path == "." || path == "" {
			continue
		}
		key := path
		if platform == "windows" {
			key = strings.ToLower(key)
		}
		if !seen[key] {
			seen[key] = true
			paths = append(paths, path)
		}
	}
	if platform == "windows" {
		drives := paths[:0]
		for _, path := range paths {
			if len(path) == 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
				drives = append(drives, path)
			}
		}
		if len(drives) > 0 {
			return drives
		}
	}
	if len(paths) == 0 {
		return []string{home}
	}
	return paths
}
func (s *Server) saveSearchConfig(config searchConfig) error {
	configPath, _, err := s.searchPaths()
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(config, "", "  ")
	return os.WriteFile(configPath, append(data, '\n'), 0o600)
}
func (s *Server) rebuildSearch(options map[string]any) (map[string]any, error) {
	config, err := s.loadSearchConfig()
	if err != nil {
		return nil, err
	}
	if roots := stringSlice(options["roots"]); len(roots) > 0 {
		config.Roots = roots
	}
	if exclusions, ok := options["exclusions"]; ok {
		config.Exclusions = stringSlice(exclusions)
	}
	_, indexPath, err := s.searchPaths()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(indexPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	writer := bufio.NewWriterSize(file, 256*1024)
	count := 0
	for _, root := range config.Roots {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			for _, excluded := range config.Exclusions {
				if withinPath(path, excluded) {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			item := searchEntry{Path: path, Name: entry.Name(), Ext: strings.TrimPrefix(strings.ToLower(filepath.Ext(entry.Name())), "."), IsDir: entry.IsDir(), Size: info.Size(), MtimeMs: info.ModTime().UnixMilli()}
			data, _ := json.Marshal(item)
			_, _ = writer.Write(append(data, '\n'))
			count++
			return nil
		})
	}
	_ = writer.Flush()
	_ = file.Close()
	config.Count = count
	config.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveSearchConfig(config); err != nil {
		return nil, err
	}
	return s.searchStatus(config), nil
}
func withinPath(candidate, parent string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
func stringSlice(value any) []string {
	raw, _ := value.([]any)
	out := []string{}
	seen := map[string]bool{}
	for _, item := range raw {
		path := filepath.Clean(stringValue(item))
		if filepath.IsAbs(path) && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}
func (s *Server) searchStatus(config searchConfig) map[string]any {
	return map[string]any{"state": "ready", "error": nil, "warning": nil, "roots": config.Roots, "exclusions": config.Exclusions, "count": config.Count, "scanned": config.Count, "updatedAt": config.UpdatedAt, "startedAt": nil, "queryFeatures": []string{"wildcards", "caseSensitive"}}
}
func validExactFileName(value string) bool {
	name := strings.TrimSpace(value)
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "\x00/\\")
}

func appendUniqueSearchRoot(roots []string, seen map[string]bool, value string) []string {
	path := filepath.Clean(value)
	if !filepath.IsAbs(path) {
		return roots
	}
	key := path
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	if seen[key] {
		return roots
	}
	seen[key] = true
	return append(roots, path)
}

func liveFileSearchRoots(config searchConfig) []string {
	seen := map[string]bool{}
	roots := []string{}
	home := userHome()
	for _, name := range []string{"Downloads", "Videos", "Desktop", "Documents", "Movies"} {
		roots = appendUniqueSearchRoot(roots, seen, filepath.Join(home, name))
	}
	for _, root := range config.Roots {
		roots = appendUniqueSearchRoot(roots, seen, root)
	}
	// The index can be configured narrowly, but resolving an explicitly dropped
	// filename is expected to work anywhere on this machine. Search every mounted
	// filesystem after the likely user folders. The deadline below keeps this
	// fallback from turning a stale index into an unbounded RPC.
	for _, root := range defaultFileSearchRoots() {
		roots = appendUniqueSearchRoot(roots, seen, root)
	}
	return roots
}

type liveDirectoryListing struct {
	path    string
	entries []os.DirEntry
}

func liveExactFileSearch(ctx context.Context, config searchConfig, name string, limit int) ([]searchEntry, bool) {
	if !validExactFileName(name) || limit <= 0 {
		return nil, false
	}
	deadline, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	queue := liveFileSearchRoots(config)
	visited := map[string]bool{}
	results := []searchEntry{}
	timedOut := false
	for len(queue) > 0 && len(results) < limit {
		select {
		case <-deadline.Done():
			timedOut = true
			return results, timedOut
		default:
		}
		batchSize := 32
		if len(queue) < batchSize {
			batchSize = len(queue)
		}
		batch := queue[:batchSize]
		queue = queue[batchSize:]
		listings := make(chan liveDirectoryListing, len(batch))
		launched := 0
		for _, directory := range batch {
			key := directory
			if runtime.GOOS == "windows" {
				key = strings.ToLower(key)
			}
			if visited[key] {
				continue
			}
			visited[key] = true
			launched++
			go func(path string) {
				entries, _ := os.ReadDir(path)
				listings <- liveDirectoryListing{path: path, entries: entries}
			}(directory)
		}
		for index := 0; index < launched; index++ {
			select {
			case <-deadline.Done():
				timedOut = true
				return results, timedOut
			case listing := <-listings:
				for _, entry := range listing.entries {
					path := filepath.Join(listing.path, entry.Name())
					excluded := false
					for _, exclusion := range config.Exclusions {
						if withinPath(path, exclusion) {
							excluded = true
							break
						}
					}
					if excluded || entry.Type()&os.ModeSymlink != 0 {
						continue
					}
					if entry.IsDir() {
						queue = append(queue, path)
						continue
					}
					if !strings.EqualFold(entry.Name(), name) {
						continue
					}
					info, err := entry.Info()
					if err != nil || !info.Mode().IsRegular() {
						continue
					}
					results = append(results, searchEntry{
						Path: path, Name: entry.Name(), Ext: strings.TrimPrefix(strings.ToLower(filepath.Ext(entry.Name())), "."),
						Size: info.Size(), MtimeMs: info.ModTime().UnixMilli(),
					})
					if len(results) >= limit {
						break
					}
				}
			}
		}
		if len(results) > 0 {
			return results, false
		}
	}
	return results, timedOut
}

func (s *Server) querySearch(ctx context.Context, config searchConfig, query string, options map[string]any) (map[string]any, error) {
	_, indexPath, err := s.searchPaths()
	if err != nil {
		return nil, err
	}
	exactName := strings.TrimSpace(stringValue(options["exactName"]))
	live, _ := options["live"].(bool)
	file, err := os.Open(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		if live && validExactFileName(exactName) {
			limit := 20
			if value, ok := options["limit"].(float64); ok && value > 0 && value < float64(limit) {
				limit = int(value)
			}
			results, timedOut := liveExactFileSearch(ctx, config, exactName, limit)
			return map[string]any{
				"query": query, "total": len(results), "results": results,
				"liveSearched": true, "liveTimedOut": timedOut,
			}, nil
		}
		if _, err = s.rebuildSearch(map[string]any{}); err != nil {
			return nil, err
		}
		file, err = os.Open(indexPath)
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limit := 250
	if value, ok := options["limit"].(float64); ok && value > 0 && value <= 5000 {
		limit = int(value)
	}
	caseSensitive, _ := options["caseSensitive"].(bool)
	terms := strings.Fields(query)
	results := []searchEntry{}
	total := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var entry searchEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if searchMatch(entry, terms, caseSensitive) {
			total++
			if len(results) < limit {
				results = append(results, entry)
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return strings.ToLower(results[i].Name) < strings.ToLower(results[j].Name) })
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	liveSearched, liveTimedOut := false, false
	if live && validExactFileName(exactName) {
		hasExact := false
		for _, result := range results {
			if !result.IsDir && strings.EqualFold(result.Name, exactName) {
				hasExact = true
				break
			}
		}
		if !hasExact {
			liveSearched = true
			fresh, timedOut := liveExactFileSearch(ctx, config, exactName, limit)
			liveTimedOut = timedOut
			seen := map[string]bool{}
			for _, result := range results {
				seen[strings.ToLower(result.Path)] = true
			}
			for _, result := range fresh {
				key := strings.ToLower(result.Path)
				if seen[key] || len(results) >= limit {
					continue
				}
				seen[key] = true
				results = append(results, result)
				total++
			}
		}
	}
	return map[string]any{"query": query, "total": total, "results": results, "liveSearched": liveSearched, "liveTimedOut": liveTimedOut}, nil
}
func searchMatch(entry searchEntry, terms []string, caseSensitive bool) bool {
	path, name, ext := entry.Path, entry.Name, entry.Ext
	if !caseSensitive {
		path, name, ext = strings.ToLower(path), strings.ToLower(name), strings.ToLower(ext)
	}
	for _, raw := range terms {
		negative := strings.HasPrefix(raw, "-")
		term := strings.TrimPrefix(raw, "-")
		target := path
		if strings.HasPrefix(term, "file:") {
			target = name
			term = strings.TrimPrefix(term, "file:")
		} else if strings.HasPrefix(term, "ext:") {
			target = ext
			term = strings.TrimPrefix(term, "ext:")
		}
		if !caseSensitive {
			term = strings.ToLower(term)
		}
		term = strings.Trim(term, "*\"")
		matched := term == "" || strings.Contains(target, term)
		if negative == matched {
			return false
		}
	}
	return true
}
func (s *Server) handleFileSearchRPC(ctx context.Context, request message) (any, error) {
	config, err := s.loadSearchConfig()
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case "status":
		return s.searchStatus(config), nil
	case "search":
		return s.querySearch(ctx, config, stringValue(request.Args[0]), objectArg(request.Args, 1))
	case "details":
		paths := sliceArg(request.Args, 0)
		out := make([]map[string]any, 0, len(paths))
		for _, value := range paths {
			path := stringValue(value)
			info, err := os.Stat(path)
			if err != nil {
				out = append(out, map[string]any{"path": path, "exists": false, "isDir": nil, "size": nil, "mtimeMs": nil})
			} else {
				out = append(out, map[string]any{"path": path, "exists": true, "isDir": info.IsDir(), "size": info.Size(), "mtimeMs": info.ModTime().UnixMilli()})
			}
		}
		return out, nil
	case "configure", "rebuild":
		if !request.allows("fs.list") {
			return nil, errors.New("configuring search roots requires fs.list")
		}
		return s.rebuildSearch(objectArg(request.Args, 0))
	case "repair":
		status, err := s.rebuildSearch(map[string]any{})
		return map[string]any{"repaired": err == nil, "scopes": config.Roots, "status": status}, err
	case "cancel":
		return false, nil
	case "open", "reveal":
		if !request.allows("fs.openPath") {
			return nil, errors.New("opening search results requires fs.openPath")
		}
		if len(request.Args) < 1 {
			return nil, errors.New("file path is required")
		}
		return true, openLocalPath(ctx, stringValue(request.Args[0]), request.Method == "reveal")
	default:
		return nil, errors.New("unsupported file-search method")
	}
}
func openLocalPath(ctx context.Context, path string, reveal bool) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		args := []string{path}
		if reveal {
			args = []string{"-R", path}
		}
		command = exec.CommandContext(ctx, "open", args...)
	case "windows":
		if reveal {
			command = exec.CommandContext(ctx, "explorer.exe", "/select,", path)
		} else {
			command = exec.CommandContext(ctx, "cmd.exe", "/c", "start", "", path)
		}
	default:
		target := path
		if reveal {
			target = filepath.Dir(path)
		}
		command = exec.CommandContext(ctx, "xdg-open", target)
	}
	return command.Start()
}

type secretMetadata struct {
	Name      string `json:"name"`
	UpdatedAt string `json:"updatedAt"`
}

func (s *Server) secretMetadataPath(appID string) (string, error) {
	root, err := s.serviceDir("secrets")
	return filepath.Join(root, safeName(appID)+".json"), err
}
func (s *Server) loadSecretMetadata(appID string) ([]secretMetadata, error) {
	path, err := s.secretMetadataPath(appID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []secretMetadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	var values []secretMetadata
	err = json.Unmarshal(data, &values)
	return values, err
}
func (s *Server) saveSecretMetadata(appID string, values []secretMetadata) error {
	path, err := s.secretMetadataPath(appID)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(values, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
func validSecretName(name string) bool {
	if len(name) < 1 || len(name) > 128 {
		return false
	}
	for index, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || index > 0 && (r == '.' || r == '_' || r == '-')) {
			return false
		}
	}
	return true
}
func (s *Server) handleSecretsRPC(ctx context.Context, request message) (any, error) {
	_ = ctx
	appID := request.AppID
	if appID == "" {
		return nil, errors.New("app id is required")
	}
	metadata, err := s.loadSecretMetadata(appID)
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case "listMetadata":
		return metadata, nil
	case "configure":
		if len(request.Args) < 2 {
			return nil, errors.New("secret value is required")
		}
		name, value := stringValue(request.Args[0]), stringValue(request.Args[1])
		if !validSecretName(name) || value == "" || len(value) > 64*1024 {
			return nil, errors.New("secret is invalid")
		}
		if err := keyring.Set("DynApp/"+appID, name, value); err != nil {
			return nil, err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		found := false
		for i := range metadata {
			if metadata[i].Name == name {
				metadata[i].UpdatedAt = now
				found = true
			}
		}
		if !found {
			metadata = append(metadata, secretMetadata{Name: name, UpdatedAt: now})
		}
		sort.Slice(metadata, func(i, j int) bool { return metadata[i].Name < metadata[j].Name })
		return map[string]any{"name": name, "updatedAt": now}, s.saveSecretMetadata(appID, metadata)
	case "remove":
		name := stringValue(request.Args[0])
		_ = keyring.Delete("DynApp/"+appID, name)
		filtered := metadata[:0]
		removed := false
		for _, item := range metadata {
			if item.Name == name {
				removed = true
			} else {
				filtered = append(filtered, item)
			}
		}
		return removed, s.saveSecretMetadata(appID, filtered)
	default:
		return nil, fmt.Errorf("unsupported secrets method")
	}
}
