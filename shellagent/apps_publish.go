package shellagent

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type publishSourceFile struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Mode      int    `json:"mode"`
	localPath string
}

func (s *Server) publishAppProject(ctx context.Context, input map[string]any) (result any, resultErr error) {
	root, err := filepath.Abs(strings.TrimSpace(stringValue(input["projectRoot"])))
	if err != nil || root == "" {
		return nil, errors.New("A DynApp project folder is required")
	}
	if stat, statErr := os.Stat(root); statErr != nil || !stat.IsDir() {
		return nil, errors.New("The DynApp project folder does not exist")
	}
	token, err := ResolveAccountToken(s.AccountToken, "")
	if err != nil || token == "" {
		return nil, errors.New("Sign in to Dyner before publishing")
	}
	manifestPath := filepath.Join(root, "app.json")
	manifest, manifestOriginal, err := readPublishJSON(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("app.json is invalid: %w", err)
	}
	id := strings.TrimSpace(stringValue(manifest["id"]))
	if !draftAppIDPattern.MatchString(id) {
		return nil, errors.New("app.json has an invalid DynApp id")
	}
	contentRoot := filepath.Join(root, "content")
	if _, err := os.Stat(filepath.Join(contentRoot, "index.html")); err != nil {
		return nil, errors.New("Build the app before publishing; content/index.html is missing")
	}

	versionPath := filepath.Join(contentRoot, "version.json")
	contentVersion, contentOriginal, err := readPublishJSON(versionPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("content/version.json is invalid: %w", err)
	}
	if contentVersion == nil {
		contentVersion = map[string]any{}
	}
	packagePath := filepath.Join(root, "package.json")
	packageJSON, packageOriginal, _ := readPublishJSON(packagePath)
	lockPath := filepath.Join(root, "package-lock.json")
	lockJSON, lockOriginal, _ := readPublishJSON(lockPath)
	version := strings.TrimSpace(stringValue(manifest["version"]))
	if version == "" {
		version = strings.TrimSpace(stringValue(contentVersion["version"]))
	}
	if stringValue(input["bumpVersion"]) != "" && stringValue(input["bumpVersion"]) != "patch" {
		return nil, errors.New("bumpVersion must be \"patch\" when provided")
	}
	if stringValue(input["bumpVersion"]) == "patch" {
		version, err = bumpPatch(version)
		if err != nil {
			return nil, err
		}
		manifest["version"], contentVersion["version"] = version, version
		if packageJSON != nil {
			packageJSON["version"] = version
		}
		if lockJSON != nil {
			lockJSON["version"] = version
			if packages, ok := lockJSON["packages"].(map[string]any); ok {
				if main, ok := packages[""].(map[string]any); ok {
					main["version"] = version
				}
			}
		}
		writes := []struct {
			path  string
			value map[string]any
		}{{manifestPath, manifest}, {versionPath, contentVersion}}
		if packageJSON != nil {
			writes = append(writes, struct {
				path  string
				value map[string]any
			}{packagePath, packageJSON})
		}
		if lockJSON != nil {
			writes = append(writes, struct {
				path  string
				value map[string]any
			}{lockPath, lockJSON})
		}
		for _, write := range writes {
			if err = writePublishJSON(write.path, write.value); err != nil {
				return nil, err
			}
		}
		defer func() {
			if resultErr == nil {
				return
			}
			_ = os.WriteFile(manifestPath, manifestOriginal, 0o644)
			if contentOriginal != nil {
				_ = os.WriteFile(versionPath, contentOriginal, 0o644)
			} else {
				_ = os.Remove(versionPath)
			}
			if packageOriginal != nil {
				_ = os.WriteFile(packagePath, packageOriginal, 0o644)
			}
			if lockOriginal != nil {
				_ = os.WriteFile(lockPath, lockOriginal, 0o644)
			}
		}()
	}
	base := strings.TrimRight(s.Config.DynerBaseURL, "/")
	if base == "" {
		base = compiledDynerBaseURL
	}
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	account, err := publishJSONRequest(ctx, http.MethodGet, base+"/api/v1/me", headers, nil)
	if err != nil {
		return nil, err
	}
	user, _ := account["user"].(map[string]any)
	owner := strings.TrimSpace(stringValue(user["id"]))
	if owner == "" {
		return nil, errors.New("The signed-in Dyner account has no publishing id")
	}
	storeID := owner + "/" + id
	updates, _ := manifest["updates"].(map[string]any)
	if strings.TrimSpace(stringValue(updates["storeId"])) != storeID {
		return nil, fmt.Errorf("app.json updates.storeId must be %q before publishing", storeID)
	}
	files, err := collectPublishSource(root, manifestSourceRootIsContent(manifest))
	if err != nil {
		return nil, err
	}
	if err := uploadPublishSources(ctx, base, headers, files); err != nil {
		return nil, err
	}
	runnable, err := zipPublishContent(contentRoot, strings.TrimSpace(stringValue(input["releaseNotes"])))
	if err != nil {
		return nil, err
	}
	sourceManifest := map[string]any{"schemaVersion": 1, "algorithm": "sha256", "files": files}
	message := strings.TrimSpace(stringValue(input["message"]))
	if message == "" {
		message = fmt.Sprintf("Published %s v%s from DyMaker.", stringValue(manifest["name"]), version)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("id", storeID)
	_ = form.WriteField("version", version)
	_ = form.WriteField("message", message)
	manifestBytes, _ := json.Marshal(manifest)
	sourceBytes, _ := json.Marshal(sourceManifest)
	_ = form.WriteField("manifest", string(manifestBytes))
	_ = form.WriteField("sourceManifest", string(sourceBytes))
	part, _ := form.CreateFormFile("content", "content.zip")
	_, _ = part.Write(runnable)
	_ = form.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/revisions", &body)
	request.Header = headers.Clone()
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New(publishError(payload, response.StatusCode))
	}
	revision, _ := payload["revision"].(map[string]any)
	app, _ := payload["app"].(map[string]any)
	if stringValue(revision["id"]) == "" {
		return nil, errors.New("Dyner returned an invalid publish response")
	}
	return map[string]any{"storeId": firstString(app["id"], storeID), "revisionId": revision["id"], "version": firstString(revision["version"], version), "message": firstString(revision["message"], message)}, nil
}

func readPublishJSON(path string) (map[string]any, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var value map[string]any
	err = json.Unmarshal(data, &value)
	return value, data, err
}
func writePublishJSON(path string, value map[string]any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
func bumpPatch(version string) (string, error) {
	var a, b, c int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &a, &b, &c); err != nil || fmt.Sprintf("%d.%d.%d", a, b, c) != version {
		return "", fmt.Errorf("Cannot bump invalid semantic version %s", version)
	}
	return fmt.Sprintf("%d.%d.%d", a, b, c+1), nil
}
func firstString(value any, fallback string) string {
	if text := strings.TrimSpace(stringValue(value)); text != "" {
		return text
	}
	return fallback
}
func publishError(payload map[string]any, status int) string {
	if text := firstString(payload["error"], ""); text != "" {
		return text
	}
	if text := firstString(payload["message"], ""); text != "" {
		return text
	}
	return fmt.Sprintf("Dyner publish failed: HTTP %d", status)
}

func collectPublishSource(root string, includeContent bool) ([]publishSourceFile, error) {
	excluded := map[string]bool{"content": !includeContent, ".dymaker": true, ".git": true, ".hg": true, ".svn": true, "node_modules": true, ".cache": true, ".vite": true, "dist": false}
	var files []publishSourceFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Source symlinks are not supported: %s", rel)
		}
		if info.IsDir() && excluded[info.Name()] {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if excluded[info.Name()] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		mode := 420
		if info.Mode()&0o111 != 0 {
			mode = 493
		}
		files = append(files, publishSourceFile{Path: rel, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(data)), Mode: mode, localPath: path})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func manifestSourceRootIsContent(manifest map[string]any) bool {
	source, _ := manifest["source"].(map[string]any)
	root := strings.TrimPrefix(strings.Trim(strings.ReplaceAll(stringValue(source["root"]), "\\", "/"), " /"), "./")
	return root == "content"
}

func uploadPublishSources(ctx context.Context, base string, headers http.Header, files []publishSourceFile) error {
	blobs := make([]map[string]any, 0, len(files))
	byDigest := map[string]publishSourceFile{}
	for _, file := range files {
		blobs = append(blobs, map[string]any{"digest": file.Digest, "size": file.Size})
		byDigest[file.Digest] = file
	}
	checked, err := publishJSONRequest(ctx, http.MethodPost, base+"/api/v1/source/blobs/check", headers, map[string]any{"blobs": blobs})
	if err != nil {
		return err
	}
	missing, _ := checked["missing"].([]any)
	if len(missing) == 0 {
		return nil
	}
	var raw bytes.Buffer
	raw.WriteString("DYNPACK1")
	_ = binary.Write(&raw, binary.BigEndian, uint32(len(missing)))
	for _, item := range missing {
		file, ok := byDigest[stringValue(item)]
		if !ok {
			return errors.New("Dyner requested an unknown source blob")
		}
		digest, _ := hex.DecodeString(strings.TrimPrefix(file.Digest, "sha256:"))
		raw.Write(digest)
		_ = binary.Write(&raw, binary.BigEndian, uint64(file.Size))
		data, err := os.ReadFile(file.localPath)
		if err != nil {
			return err
		}
		raw.Write(data)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write(raw.Bytes())
	_ = gz.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/v1/source/packs", &compressed)
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/vnd.dyner.source-pack")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var p map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&p)
		return errors.New(publishError(p, resp.StatusCode))
	}
	return nil
}
func publishJSONRequest(ctx context.Context, method, url string, headers http.Header, input any) (map[string]any, error) {
	var body io.Reader
	if input != nil {
		data, _ := json.Marshal(input)
		body = bytes.NewReader(data)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, body)
	req.Header = headers.Clone()
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(publishError(payload, resp.StatusCode))
	}
	return payload, nil
}
func zipPublishContent(root, notes string) ([]byte, error) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		_, err = io.Copy(writer, input)
		return err
	})
	if err == nil && notes != "" {
		writer, e := archive.Create("VERSION_NOTES.txt")
		if e != nil {
			return nil, e
		}
		_, err = writer.Write([]byte(strings.TrimSpace(notes) + "\n"))
	}
	if closeErr := archive.Close(); err == nil {
		err = closeErr
	}
	return buffer.Bytes(), err
}
