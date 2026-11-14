package shellagent

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Two things help a coding agent the user already has find and use the
// app notes (agent_notes.go), without installing anything into it (copies of
// the skill that `dynapp skill install` put in ~/.claude, ~/.codex or ~/.agents
// are refreshed along with the local one):
//
//   - A local copy of the dynapp-developer skill beside installed-apps.md. Coding
//     agents often run in a sandbox without network access, so a note that
//     only links to https://dynapp.io/skill leaves them stuck.
//   - One marked line in the global instructions of coding agents that are
//     already set up on this computer (Codex ~/.codex/AGENTS.md, Claude Code
//     ~/.claude/CLAUDE.md). Some models never search outside their working
//     folder; this line tells them where installed apps are listed.

const (
	localSkillName         = "dynapp-developer"
	localSkillRefreshEvery = 6 * time.Hour
	maxSkillZipBytes       = 4 << 20
	maxSkillFileBytes      = 2 << 20
	maxSkillFiles          = 64
	codingAgentMarkStart   = "<!-- dynapp -->"
	codingAgentMarkEnd     = "<!-- /dynapp -->"
)

// localSkillDir holds <agents>/skill/dynapp-developer/SKILL.md and its references.
func localSkillDir(agentsDir string) string {
	return filepath.Join(agentsDir, "skill", localSkillName)
}

// localSkillPath is the local SKILL.md once a copy exists, otherwise "".
func localSkillPath() string {
	dir, err := codingAgentsDir()
	if err != nil {
		return ""
	}
	skill := filepath.Join(localSkillDir(dir), "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		return ""
	}
	return skill
}

var localSkillRefresh struct {
	sync.Mutex
	at   time.Time
	base string
}

// prepareCodingAgents refreshes the local skill copy (at most every few
// hours) and the global instruction lines. Callers run it only on machines
// with installed apps.
func (s *Server) prepareCodingAgents(ctx context.Context) {
	dir, err := codingAgentsDir()
	if err != nil {
		return
	}
	s.mu.Lock()
	base := s.Config.dynerBaseOrigin()
	s.mu.Unlock()
	if base == "" {
		base = defaultNoteDynerURL
	}
	home, codexHome, homeErr := codingAgentHomes()
	localSkillRefresh.Lock()
	if localSkillRefresh.base != base || time.Since(localSkillRefresh.at) > localSkillRefreshEvery {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var copies []string
		if homeErr == nil {
			copies = userSkillCopies(home, codexHome)
		}
		if err := refreshLocalSkill(refreshCtx, s.dynerHTTPClient(), base, dir, copies...); err == nil {
			localSkillRefresh.at, localSkillRefresh.base = time.Now(), base
		}
		cancel()
	}
	localSkillRefresh.Unlock()
	if homeErr == nil {
		_ = writeCodingAgentPointers(home, codexHome, dir)
	}
}

// userSkillCopies are the user-level skill folders coding agents read. `dynapp
// skill install` puts the skill there; the agent keeps those copies current
// with https://dynapp.io/skill but never installs one itself.
func userSkillCopies(home, codexHome string) []string {
	if home == "" {
		return nil
	}
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return []string{
		filepath.Join(home, ".claude", "skills", localSkillName),
		filepath.Join(codexHome, "skills", localSkillName),
		filepath.Join(home, ".agents", "skills", localSkillName),
	}
}

// refreshLocalSkill downloads <base>/skill/dynapp-developer.zip and replaces
// the local copy only when the archive is complete and valid. Each of
// installed that already holds the skill (a real folder with SKILL.md, not a
// symlink to someone's checkout) is replaced with the same files.
func refreshLocalSkill(ctx context.Context, client *http.Client, base, agentsDir string, installed ...string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/skill/"+localSkillName+".zip", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download the skill: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSkillZipBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxSkillZipBytes {
		return errors.New("the skill archive is too large")
	}
	files, err := readSkillZip(data)
	if err != nil {
		return err
	}
	if err := writeSkillDir(localSkillDir(agentsDir), files); err != nil {
		return err
	}
	for _, target := range installed {
		info, err := os.Lstat(target)
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
			continue
		}
		_ = writeSkillDir(target, files)
	}
	return nil
}

// writeSkillDir replaces target with files, staging them beside it first.
func writeSkillDir(target string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	staging := fmt.Sprintf("%s.partial-%d", target, os.Getpid())
	_ = os.RemoveAll(staging)
	for relative, content := range files {
		file := filepath.Join(staging, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
		if err := os.WriteFile(file, content, 0o644); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
	}
	if err := os.RemoveAll(target); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	return os.Rename(staging, target)
}

// readSkillZip returns the files under dynapp-developer/, keyed by their path
// below it. It rejects anything that could escape the skill folder.
func readSkillZip(data []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("skill archive entry %q is not a regular file", entry.Name)
		}
		name := path.Clean(strings.ReplaceAll(entry.Name, "\\", "/"))
		relative, ok := strings.CutPrefix(name, localSkillName+"/")
		if !ok || relative == "" || path.IsAbs(name) || strings.HasPrefix(relative, "../") || strings.Contains(relative, "/../") || strings.Contains(relative, ":") {
			return nil, fmt.Errorf("unexpected skill archive entry %q", entry.Name)
		}
		if len(files) >= maxSkillFiles || entry.UncompressedSize64 > maxSkillFileBytes {
			return nil, errors.New("the skill archive is too large")
		}
		opened, err := entry.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(io.LimitReader(opened, maxSkillFileBytes+1))
		opened.Close()
		if err != nil {
			return nil, err
		}
		if len(content) > maxSkillFileBytes {
			return nil, errors.New("the skill archive is too large")
		}
		files[relative] = content
	}
	if _, ok := files["SKILL.md"]; !ok {
		return nil, errors.New("the skill archive has no SKILL.md")
	}
	return files, nil
}

// codingAgentPointer is the one line a coding agent reads in every session.
func codingAgentPointer(agentsDir string) string {
	return "To change an app the user installed, first check " + filepath.Join(agentsDir, installedAppsIndexName) + "."
}

// writeCodingAgentPointers adds or updates the marked line in the global
// instructions of each coding agent whose configuration folder exists. It
// never creates a coding agent's folder.
func writeCodingAgentPointers(home, codexHome, agentsDir string) error {
	if home == "" {
		return errors.New("no home folder")
	}
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	line := codingAgentPointer(agentsDir)
	var failures []string
	for _, file := range []string{filepath.Join(codexHome, "AGENTS.md"), filepath.Join(home, ".claude", "CLAUDE.md")} {
		if info, err := os.Stat(filepath.Dir(file)); err != nil || !info.IsDir() {
			continue
		}
		if err := upsertMarkedBlock(file, line); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// upsertMarkedBlock replaces the text between the DynApp markers, or appends
// the block, leaving the rest of the file as it is.
func upsertMarkedBlock(file, body string) error {
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(data)
	block := codingAgentMarkStart + "\n" + body + "\n" + codingAgentMarkEnd + "\n"
	if start := strings.Index(text, codingAgentMarkStart); start >= 0 {
		if length := strings.Index(text[start:], codingAgentMarkEnd); length >= 0 {
			end := start + length + len(codingAgentMarkEnd)
			if end < len(text) && text[end] == '\n' {
				end++
			}
			return writeFileIfChanged(file, []byte(text[:start]+block+text[end:]))
		}
	}
	if text != "" {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n"
	}
	return writeFileIfChanged(file, []byte(text+block))
}
