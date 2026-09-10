package shellagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const liveReloadScript = `<script>(()=>{let v;setInterval(async()=>{try{const n=await fetch('/.dynapp-live-version',{cache:'no-store'}).then(r=>r.text());if(v&&n!==v)location.reload();v=n}catch{}},500)})()</script>`

type livePreview struct {
	root     string
	url      string
	server   *http.Server
	listener net.Listener
	command  *exec.Cmd
}

type liveAuthoringFile struct {
	Authoring struct {
		Mode       string `json:"mode"`
		DevCommand string `json:"devCommand"`
		DevURL     string `json:"devUrl"`
	} `json:"authoring"`
}

func readViteAuthoring(root string) (string, string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "dynapp.json"))
	if err != nil {
		return "", "", false
	}
	var config liveAuthoringFile
	if json.Unmarshal(data, &config) != nil {
		return "", "", false
	}
	mode := strings.TrimSpace(config.Authoring.Mode)
	devURL := strings.TrimSpace(config.Authoring.DevURL)
	if (mode != "vite" && mode != "command") || !isLoopbackHTTPURL(devURL) {
		return "", "", false
	}
	return strings.TrimSpace(config.Authoring.DevCommand), devURL, true
}

func isLoopbackHTTPURL(rawURL string) bool {
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil || request.URL.Scheme != "http" && request.URL.Scheme != "https" {
		return false
	}
	host := strings.Trim(request.URL.Hostname(), "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func waitForPreviewURL(rawURL string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(rawURL)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("live dev server did not become ready")
}

func (s *Server) startAuthoringCommand(root, command string) (*exec.Cmd, error) {
	if command == "" {
		return nil, nil
	}
	if err := s.ensureAuthoringDependencies(root); err != nil {
		return nil, err
	}
	child := authoringCommand(command)
	child.Dir = root
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return nil, err
	}
	return child, nil
}

func contentVersion(root string) string {
	entries := []string{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr == nil {
			entries = append(entries, fmt.Sprintf("%s:%d:%d", filepath.ToSlash(relative), info.Size(), info.ModTime().UnixNano()))
		}
		return nil
	})
	sort.Strings(entries)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(entries, "\n"))))
}

func serveLiveHTML(w http.ResponseWriter, path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	html := string(data)
	if index := strings.LastIndex(strings.ToLower(html), "</body>"); index >= 0 {
		html = html[:index] + liveReloadScript + html[index:]
	} else {
		html += liveReloadScript
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
	return true
}

func (s *Server) startLivePreview(projectRoot string, openOS bool) (string, error) {
	root := filepath.Clean(projectRoot)
	devCommand, devURL, vite := readViteAuthoring(root)
	if vite {
		s.previewMu.Lock()
		defer s.previewMu.Unlock()
		if s.previews == nil {
			s.previews = map[string]*livePreview{}
		}
		if existing := s.previews[root]; existing != nil {
			if openOS {
				_ = openLocalPath(context.Background(), existing.url, false)
			}
			return existing.url, nil
		}
		child, err := s.startAuthoringCommand(root, devCommand)
		if err != nil {
			return "", err
		}
		if err := waitForPreviewURL(devURL); err != nil {
			if child != nil && child.Process != nil {
				_ = child.Process.Kill()
			}
			return "", err
		}
		s.previews[root] = &livePreview{root: root, url: devURL, command: child}
		if openOS {
			_ = openLocalPath(context.Background(), devURL, false)
		}
		return devURL, nil
	}
	content := filepath.Join(root, "content")
	if _, err := os.Stat(filepath.Join(content, "index.html")); err != nil {
		return "", err
	}
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previews == nil {
		s.previews = map[string]*livePreview{}
	}
	if existing := s.previews[root]; existing != nil {
		if openOS {
			_ = openLocalPath(context.Background(), existing.url, false)
		}
		return existing.url, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	fileServer := http.FileServer(http.Dir(content))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/.dynapp-live-version" {
			_, _ = w.Write([]byte(contentVersion(content)))
			return
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/" || strings.HasSuffix(strings.ToLower(r.URL.Path), ".html")) {
			relative := strings.TrimPrefix(r.URL.Path, "/")
			if relative == "" {
				relative = "index.html"
			}
			if serveLiveHTML(w, filepath.Join(content, filepath.FromSlash(relative))) {
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: mux}
	preview := &livePreview{
		root:     root,
		url:      "http://" + listener.Addr().String() + "/",
		server:   server,
		listener: listener,
	}
	s.previews[root] = preview
	go func() { _ = server.Serve(listener) }()
	if openOS {
		_ = openLocalPath(context.Background(), preview.url, false)
	}
	return preview.url, nil
}

func (s *Server) closeLivePreviews() {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	for root, preview := range s.previews {
		if preview.server != nil {
			_ = preview.server.Close()
		}
		if preview.command != nil && preview.command.Process != nil {
			_ = preview.command.Process.Kill()
		}
		delete(s.previews, root)
	}
}
