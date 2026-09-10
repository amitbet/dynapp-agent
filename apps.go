package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var draftAppIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func (s *Server) handleAppsRPC(ctx context.Context, request message) (any, error) {
	switch request.Method {
	case "createFromBoilerplate":
		return s.createBoilerplateDraft(objectArg(request.Args, 0))
	case "listDrafts":
		return s.listDrafts()
	case "draft":
		return s.openAppDraft(ctx, objectArg(request.Args, 0))
	case "previewLocal":
		root, openOS := previewRequest(request)
		if root == "" {
			return nil, errors.New("A project folder is required for live preview")
		}
		url, err := s.startLivePreview(root, openOS)
		if err != nil {
			return nil, err
		}
		return map[string]any{"opened": openOS, "live": true, "url": url, "livePreviewUrl": url, "projectRoot": root}, nil
	case "publish":
		return s.publishAppProject(ctx, objectArg(request.Args, 0))
	case "markModified":
		return map[string]any{"projectRoot": strings.TrimSpace(stringArg(request.Args, 0)), "modified": true}, nil
	default:
		return nil, errors.New("unsupported apps method")
	}
}

func (s *Server) draftsDir() (string, error) {
	return s.serviceDir("drafts")
}

func (s *Server) openAppDraft(ctx context.Context, input map[string]any) (any, error) {
	storeID := strings.TrimSpace(stringValue(input["storeId"]))
	appID := strings.ToLower(strings.TrimSpace(stringValue(input["appId"])))
	if !validStoreID(storeID) {
		return nil, errors.New("A full Dyner store id (owner/slug) is required to open a local draft")
	}
	root, err := s.draftsDir()
	if err != nil {
		return nil, err
	}
	draftID := appID
	if draftID == "" {
		draftID = strings.ToLower(storeID[strings.LastIndex(storeID, "/")+1:])
	}
	if !draftAppIDPattern.MatchString(draftID) {
		return nil, errors.New("app id must start with a letter or number and use lowercase letters, numbers, dots, underscores, or hyphens")
	}
	projectRoot := filepath.Join(root, draftID)
	reused := false
	if _, err := os.Stat(filepath.Join(projectRoot, "app.json")); err == nil {
		reused = true
		if _, err := os.Stat(filepath.Join(projectRoot, "content", "index.html")); err != nil {
			if err := s.materializeStoreRunnable(ctx, storeID, projectRoot); err != nil {
				return nil, err
			}
		}
	} else {
		if err := s.materializeStoreDraft(ctx, storeID, projectRoot); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "package.json")); err == nil {
		if err := s.ensureNodeRuntime(); err != nil {
			return nil, err
		}
	}
	s.queueDraftOpen("dymaker", projectRoot)
	openOS := true
	if value, ok := input["openPreview"].(bool); ok {
		openOS = value
	}
	liveURL := ""
	if url, err := s.startLivePreview(projectRoot, openOS); err == nil {
		liveURL = url
	}
	result := map[string]any{
		"appId":       draftID,
		"projectRoot": projectRoot,
		"storeId":     storeID,
		"created":     !reused,
		"reused":      reused,
		"draft":       true,
		"live":        liveURL != "",
		"opened":      openOS && liveURL != "",
		"modifiedAt":  time.Now().UnixMilli(),
	}
	if liveURL != "" {
		result["url"] = liveURL
		result["livePreviewUrl"] = liveURL
	}
	return result, nil
}

func previewRequest(request message) (string, bool) {
	openOS := true
	input := objectArg(request.Args, 0)
	if value, ok := input["openPreview"].(bool); ok {
		openOS = value
	}
	root := strings.TrimSpace(stringValue(input["projectRoot"]))
	if root == "" {
		root = strings.TrimSpace(stringArg(request.Args, 0))
	}
	return root, openOS
}

func (s *Server) materializeStoreDraft(ctx context.Context, storeID, projectRoot string) error {
	base := strings.TrimRight(s.Config.DynerBaseURL, "/")
	if base == "" {
		base = compiledDynerBaseURL
	}
	token, _ := ResolveAccountToken(s.AccountToken, "")
	headers := scopedBearerHeaders(base, token)
	client := &http.Client{Timeout: 60 * time.Second}
	detailsURL := base + "/api/v1/apps/" + storeID
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
	if err != nil {
		return err
	}
	copyHeaders(request.Header, headers.forURL(detailsURL))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Could not load this app's Dyner source")
	}
	var payload struct {
		LatestRevision *struct {
			ID                   string         `json:"id"`
			Version              string         `json:"version"`
			SourceURL            string         `json:"source_url"`
			SourceSnapshotDigest string         `json:"source_snapshot_digest"`
			Manifest             map[string]any `json:"manifest"`
			RunnableURL          string         `json:"runnable_url"`
			RunnableSHA256       string         `json:"runnable_sha256"`
		} `json:"latestRevision"`
		Revisions []struct {
			ID                   string         `json:"id"`
			Version              string         `json:"version"`
			SourceURL            string         `json:"source_url"`
			SourceSnapshotDigest string         `json:"source_snapshot_digest"`
			Manifest             map[string]any `json:"manifest"`
			RunnableURL          string         `json:"runnable_url"`
			RunnableSHA256       string         `json:"runnable_sha256"`
		} `json:"revisions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return errors.New("Dyner returned invalid app details")
	}
	revision := payload.LatestRevision
	if revision == nil && len(payload.Revisions) > 0 {
		revision = &payload.Revisions[0]
	}
	if revision == nil || strings.TrimSpace(revision.SourceURL) == "" || strings.TrimSpace(revision.SourceSnapshotDigest) == "" {
		return errors.New("This Dyner revision does not include editable source")
	}
	staging, err := os.MkdirTemp(filepath.Dir(projectRoot), ".draft-"+filepath.Base(projectRoot)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := materializeSourceSnapshot(client, revision.SourceURL, revision.SourceSnapshotDigest, staging, headers); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(staging, "app.json")); err != nil {
		return errors.New("The editable source is not a valid DynApp project")
	}
	if strings.TrimSpace(revision.RunnableURL) == "" {
		return errors.New("This Dyner revision does not include a runnable app")
	}
	if err := materializeRunnableArchive(client, revision.RunnableURL, revision.RunnableSHA256, filepath.Join(staging, "content"), headers); err != nil {
		return err
	}
	ensureDraftLiveAuthoring(staging)
	if err := os.RemoveAll(projectRoot); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(projectRoot), 0o700); err != nil {
		return err
	}
	return os.Rename(staging, projectRoot)
}

func (s *Server) materializeStoreRunnable(ctx context.Context, storeID, projectRoot string) error {
	base := strings.TrimRight(s.Config.DynerBaseURL, "/")
	if base == "" {
		base = compiledDynerBaseURL
	}
	token, _ := ResolveAccountToken(s.AccountToken, "")
	headers := scopedBearerHeaders(base, token)
	client := &http.Client{Timeout: 60 * time.Second}
	detailsURL := base + "/api/v1/apps/" + storeID
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, detailsURL, nil)
	if err != nil {
		return err
	}
	copyHeaders(request.Header, headers.forURL(detailsURL))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Could not load this app's Dyner release")
	}
	var payload struct {
		LatestRevision *struct {
			RunnableURL    string `json:"runnable_url"`
			RunnableSHA256 string `json:"runnable_sha256"`
		} `json:"latestRevision"`
		Revisions []struct {
			RunnableURL    string `json:"runnable_url"`
			RunnableSHA256 string `json:"runnable_sha256"`
		} `json:"revisions"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return errors.New("Dyner returned invalid app details")
	}
	runnableURL, runnableSHA := "", ""
	if payload.LatestRevision != nil {
		runnableURL, runnableSHA = payload.LatestRevision.RunnableURL, payload.LatestRevision.RunnableSHA256
	} else if len(payload.Revisions) > 0 {
		runnableURL, runnableSHA = payload.Revisions[0].RunnableURL, payload.Revisions[0].RunnableSHA256
	}
	if strings.TrimSpace(runnableURL) == "" {
		return errors.New("This Dyner revision does not include a runnable app")
	}
	return materializeRunnableArchive(client, runnableURL, runnableSHA, filepath.Join(projectRoot, "content"), headers)
}

// scopedHeaders sends the account bearer token only to URLs whose origin is
// the configured Dyner base origin. Content hosts and CDNs never receive it.
type scopedHeaders struct {
	origin string
	token  string
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

func (s *Server) queueDraftOpen(appID, projectRoot string) {
	s.externalMu.Lock()
	if s.externalOpens == nil {
		s.externalOpens = map[string][]string{}
	}
	s.externalOpens[appID] = append(s.externalOpens[appID], projectRoot)
	s.externalMu.Unlock()
	_ = QueueExternalOpenFallback(s.StateDir, appID, []string{projectRoot})
}

func ensureDraftLiveAuthoring(projectRoot string) {
	dynappPath := filepath.Join(projectRoot, "dynapp.json")
	if _, err := os.Stat(dynappPath); err == nil {
		return
	}
	packagePath := filepath.Join(projectRoot, "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil || strings.TrimSpace(pkg.Scripts["build"]) == "" {
		return
	}
	_ = os.WriteFile(dynappPath, []byte(`{
  "authoring": {
    "mode": "build",
    "buildCommand": "npm run build",
    "watch": ["src", "public"]
  }
}
`), 0o600)
}

func (s *Server) createBoilerplateDraft(input map[string]any) (any, error) {
	id := strings.ToLower(strings.TrimSpace(stringValue(input["id"])))
	if id == "" {
		id = "my-dynapp"
	}
	if !draftAppIDPattern.MatchString(id) {
		return nil, errors.New("app id must start with a letter or number and use lowercase letters, numbers, dots, underscores, or hyphens")
	}
	name := strings.TrimSpace(stringValue(input["name"]))
	if name == "" {
		name = "My DynApp"
	}
	description := strings.TrimSpace(stringValue(input["description"]))
	if description == "" {
		description = "A modifiable DynApp."
	}
	root, err := s.draftsDir()
	if err != nil {
		return nil, err
	}
	projectRoot := filepath.Join(root, id)
	if _, err := os.Stat(filepath.Join(projectRoot, "app.json")); err == nil {
		return map[string]any{"appId": id, "projectRoot": projectRoot, "created": false, "reused": true, "draft": true}, nil
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "src"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "content", "assets"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "public"), 0o700); err != nil {
		return nil, err
	}
	manifest := map[string]any{
		"schemaVersion": 2,
		"id":            id,
		"name":          name,
		"icon":          strings.ToUpper(id[:1]),
		"version":       "0.1.0",
		"description":   description,
		"license":       "MIT",
		"source":        map[string]any{"root": "src", "language": "javascript", "framework": "none"},
		"scripts":       map[string]any{"build": "node build.mjs"},
		"authoring":     map[string]any{"mode": "build", "buildCommand": "npm run build", "watch": []string{"src", "public"}},
	}
	files := map[string]string{
		"app.json": prettyJSON(manifest),
		"dynapp.json": `{
  "authoring": {
    "mode": "build",
    "buildCommand": "npm run build",
    "watch": ["src", "public"]
  }
}
`,
		"package.json": prettyJSON(map[string]any{
			"name":    "dynapp-" + id,
			"version": "0.1.0",
			"private": true,
			"type":    "module",
			"scripts": map[string]string{"build": "node build.mjs"},
		}),
		"dyner.json": `{
  "schemaVersion": 1,
  "source": ".",
  "content": "content",
  "manifest": "app.json",
  "build": { "command": ["npm", "run", "build"], "output": "content" }
}
`,
		"src/main.js":            "const app = document.querySelector(\"#app\");\napp.innerHTML = `<main><h1>" + jsString(name) + "</h1><p>" + jsString(description) + "</p></main>`;\n",
		"src/styles.css":         "body { font-family: system-ui, sans-serif; margin: 2rem; }\n",
		"src/index.html":         "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlText(name) + "</title><link rel=\"stylesheet\" href=\"./styles.css\"></head><body><div id=\"app\"></div><script type=\"module\" src=\"./main.js\"></script></body></html>\n",
		"content/index.html":     "<!doctype html><html><head><meta charset=\"utf-8\"><title>" + htmlText(name) + "</title><link rel=\"stylesheet\" href=\"assets/app.css\"></head><body><div id=\"app\"></div><script type=\"module\" src=\"assets/app.js\"></script></body></html>\n",
		"content/assets/app.js":  "const app = document.querySelector(\"#app\");\napp.innerHTML = `<main><h1>" + jsString(name) + "</h1><p>" + jsString(description) + "</p></main>`;\n",
		"content/assets/app.css": "body { font-family: system-ui, sans-serif; margin: 2rem; }\n",
		"content/version.json":   "{\n  \"version\": \"0.1.0\"\n}\n",
		"build.mjs": `import * as fs from "node:fs/promises";
await fs.mkdir(new URL("./content/assets/", import.meta.url), { recursive: true });
await Promise.all([
  fs.copyFile(new URL("./src/main.js", import.meta.url), new URL("./content/assets/app.js", import.meta.url)),
  fs.copyFile(new URL("./src/styles.css", import.meta.url), new URL("./content/assets/app.css", import.meta.url)),
]);
`,
		"CHANGES.md": "# Changes\n\n- Created from the DynApp starter as a local draft.\n",
	}
	for path, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return nil, err
		}
	}
	if err := s.ensureNodeRuntime(); err != nil {
		return nil, err
	}
	return map[string]any{
		"appId":       id,
		"projectRoot": projectRoot,
		"created":     true,
		"reused":      false,
		"draft":       true,
		"modifiedAt":  time.Now().UnixMilli(),
	}, nil
}

func (s *Server) listDrafts() (any, error) {
	root, err := s.draftsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []any{}, nil
		}
		return nil, err
	}
	drafts := []any{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectRoot := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(projectRoot, "app.json")); err != nil {
			continue
		}
		info, _ := entry.Info()
		modifiedAt := int64(0)
		if info != nil {
			modifiedAt = info.ModTime().UnixMilli()
		}
		drafts = append(drafts, map[string]any{
			"appId":       entry.Name(),
			"projectRoot": projectRoot,
			"modifiedAt":  modifiedAt,
		})
	}
	return drafts, nil
}

func prettyJSON(value any) string {
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}\n"
	}
	return string(bytes) + "\n"
}

func jsString(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "`", "\\`")
}

func htmlText(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(value)
}
