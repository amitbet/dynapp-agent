package shellagent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent/desktop"
)

var claudeModels = []map[string]any{
	{"id": "opus", "label": "Auto", "description": "Uses the latest Opus model (currently Opus 5).", "isDefault": true, "defaultEffort": "medium", "efforts": claudeEfforts()},
	{"id": "claude-opus-5", "label": "Opus 5", "description": "Claude Opus 5.", "defaultEffort": "medium", "efforts": claudeEfforts()},
	{"id": "claude-fable-5", "label": "Fable 5", "description": "Claude Fable 5.", "defaultEffort": "medium", "efforts": claudeEfforts()},
	{"id": "claude-opus-4-8", "label": "Opus 4.8", "description": "Claude Opus 4.8.", "defaultEffort": "medium", "efforts": claudeEfforts()},
	{"id": "claude-sonnet-5", "label": "Sonnet 5", "description": "Claude Sonnet 5.", "defaultEffort": "medium", "efforts": claudeEfforts()},
}

func claudeEfforts() []map[string]any {
	rows := []map[string]any{}
	for _, id := range []string{"low", "medium", "high", "xhigh", "max"} {
		rows = append(rows, map[string]any{"id": id, "label": id})
	}
	return rows
}

type claudeSession struct {
	id, appID, model, effort string
	command                  *exec.Cmd
	stdin                    io.WriteCloser
	server                   *http.Server
	listener                 net.Listener
	token                    string
	tools                    []map[string]any
	mu                       sync.Mutex
	pendingControls          map[string]chan error
	pendingTools             map[string]chan map[string]any
	closed                   bool
}

type agentCandidate struct{ id, label, env, binary, auth string }

var agentCandidates = []agentCandidate{{"codex", "Codex", "DYNAPP_AGENT_BINARY", "codex", "login status"}, {"claude", "Claude Code", "DYNAPP_CLAUDE_BINARY", "claude", "auth status"}}

// Windows installs these CLIs as npm .cmd shims that start Node, which starts
// the native binary; a cold start there regularly takes several seconds.
var agentProbeTimeout = 20 * time.Second

func discoverAgents(ctx context.Context) []map[string]any {
	user := agentUser()
	defer user.Close()
	result := make([]map[string]any, len(agentCandidates))
	var wg sync.WaitGroup
	for index, item := range agentCandidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result[index] = probeAgent(ctx, user, item)
		}()
	}
	wg.Wait()
	return result
}

// probeAgent reports whether one CLI is installed and signed in. `reason`
// says why it is unavailable, so apps can tell the user what to fix.
func probeAgent(ctx context.Context, user *desktop.UserEnvironment, item agentCandidate) map[string]any {
	row := map[string]any{"id": item.id, "label": item.label, "source": nil, "version": nil, "installed": false, "configured": false, "available": false}
	path, err := agentBinary(user, item.env, item.binary)
	if err != nil {
		row["reason"] = item.binary + " was not found on the user's PATH"
		return row
	}
	output, err := runAgentProbe(ctx, user, item, path, "--version")
	if err != nil {
		row["reason"] = probeFailure(item.binary+" --version", output, err)
		return row
	}
	row["version"] = strings.TrimSpace(output)
	row["source"] = "external"
	row["installed"] = true
	auth := strings.Fields(item.auth)
	if item.id == "codex" {
		auth = codexArgs(ctx, user, path, auth...)
	}
	output, err = runAgentProbe(ctx, user, item, path, auth...)
	if err != nil {
		row["reason"] = probeFailure(item.binary+" "+item.auth, output, err)
		return row
	}
	row["configured"] = true
	row["available"] = true
	return row
}

func runAgentProbe(ctx context.Context, user *desktop.UserEnvironment, item agentCandidate, path string, args ...string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, agentProbeTimeout)
	defer cancel()
	command := user.Command(probeCtx, path, args...)
	if item.id == "codex" {
		command.Env = codexEnvironment(user)
	}
	// Killing a shim leaves its child holding the output pipe; don't wait
	// for that child to exit on its own.
	command.WaitDelay = 2 * time.Second
	output, err := command.CombinedOutput()
	if err != nil && probeCtx.Err() != nil {
		err = probeCtx.Err()
	}
	return string(output), err
}

func probeFailure(command, output string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("%s did not finish within %s", command, agentProbeTimeout)
	}
	detail := strings.TrimSpace(output)
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	if detail == "" {
		detail = err.Error()
	}
	return command + " failed: " + detail
}

func (a *agentService) startClaude(ctx context.Context, request message, options map[string]any) (any, error) {
	user := agentUser()
	defer user.Close()
	path, err := agentBinary(user, "DYNAPP_CLAUDE_BINARY", "claude")
	if err != nil {
		return nil, errors.New("Claude Code is not installed or is not on the user's PATH")
	}
	appID := request.AppID
	if appID == "" {
		appID = "pwa"
	}
	cwd, err := a.agentWorkspace(user, appID)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.next++
	id := "claude_" + strconv.FormatUint(a.next, 36)
	a.mu.Unlock()
	session := &claudeSession{id: id, appID: appID, model: stringValue(options["model"]), effort: stringValue(options["effort"]), pendingControls: map[string]chan error{}, pendingTools: map[string]chan map[string]any{}}
	if raw, ok := options["tools"].([]any); ok {
		for _, value := range raw {
			if tool, ok := value.(map[string]any); ok {
				session.tools = append(session.tools, tool)
			}
		}
	}
	args := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--include-partial-messages", "--permission-mode", "dontAsk", "--no-session-persistence", "--setting-sources=user", "--tools", "", "--strict-mcp-config"}
	if len(session.tools) > 0 {
		if err := session.startToolServer(a); err != nil {
			return nil, err
		}
		executable, err := os.Executable()
		if err != nil {
			session.closeToolServer()
			return nil, err
		}
		mcpConfig := map[string]any{"mcpServers": map[string]any{"dynapp": map[string]any{"command": executable, "args": []string{"--mcp-endpoint", "http://" + session.listener.Addr().String(), "--mcp-token", session.token, "mcp-proxy"}}}}
		mcpJSON, _ := json.Marshal(mcpConfig)
		args = append(args, "--mcp-config", string(mcpJSON))
	}
	if session.model != "" {
		args = append(args, "--model", session.model)
	}
	if session.effort != "" {
		args = append(args, "--effort", session.effort)
	}
	if len(session.tools) > 0 {
		names := make([]string, 0, len(session.tools))
		for _, tool := range session.tools {
			names = append(names, "mcp__dynapp__"+stringValue(tool["name"]))
		}
		args = append(args, "--allowedTools", strings.Join(names, ","))
	}
	command := user.Command(nil, path, args...)
	command.Dir = cwd
	stdout, err := command.StdoutPipe()
	if err != nil {
		session.closeToolServer()
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		session.closeToolServer()
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		session.closeToolServer()
		return nil, err
	}
	session.command, session.stdin = command, stdin
	if err := command.Start(); err != nil {
		session.closeToolServer()
		return nil, err
	}
	a.mu.Lock()
	a.claude[id] = session
	a.mu.Unlock()
	go a.readClaudeOutput(session, stdout)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			a.publishClaude(session, map[string]any{"type": "log", "text": scanner.Text()})
		}
	}()
	go func() {
		_ = command.Wait()
		a.publishClaude(session, map[string]any{"type": "status", "status": "stopped"})
		a.stopClaude(id)
	}()
	if err := session.control(ctx, map[string]any{"subtype": "initialize"}); err != nil {
		a.stopClaude(id)
		return nil, err
	}
	a.publishClaude(session, map[string]any{"type": "status", "status": "ready"})
	return map[string]any{"sessionId": id, "threadId": nil, "cwd": cwd, "agentId": "claude"}, nil
}

func (a *agentService) handleClaude(ctx context.Context, session *claudeSession, request message) (any, error) {
	switch request.Method {
	case "listModels":
		return claudeModels, nil
	case "setModel":
		value := objectArg(request.Args, 0)
		session.model, session.effort = stringValue(value["model"]), stringValue(value["effort"])
		if err := session.control(ctx, map[string]any{"subtype": "set_model", "model": session.model}); err != nil {
			return nil, err
		}
		if err := session.control(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": session.effort}}); err != nil {
			return nil, err
		}
		return map[string]any{"model": session.model, "effort": session.effort}, nil
	case "prompt":
		return session.prompt(request.Args)
	case "interrupt":
		return true, session.control(ctx, map[string]any{"subtype": "interrupt", "cancel_queued": true})
	case "respondTool":
		if len(request.Args) < 2 {
			return nil, errors.New("tool result is invalid")
		}
		return session.respondTool(stringValue(request.Args[0]), objectArg(request.Args, 1)), nil
	case "stop":
		return a.stopClaude(session.id), nil
	default:
		return nil, errors.New("unsupported agent method")
	}
}

func (s *claudeSession) prompt(args []any) (any, error) {
	value := objectArg(args, 0)
	text := strings.TrimSpace(stringValue(value["text"]))
	if text == "" {
		return nil, nil
	}
	content := []map[string]any{{"type": "text", "text": text}}
	if images, ok := value["images"].([]any); ok {
		for _, image := range images {
			path := stringValue(image)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			media := "image/png"
			switch strings.ToLower(filepath.Ext(path)) {
			case ".jpg", ".jpeg":
				media = "image/jpeg"
			case ".gif":
				media = "image/gif"
			case ".webp":
				media = "image/webp"
			}
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": base64.StdEncoding.EncodeToString(data)}})
		}
	}
	turnID := randomToken(16)
	err := s.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil, "session_id": s.id, "uuid": turnID})
	return map[string]any{"turnId": turnID}, err
}

func (s *claudeSession) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}
func (s *claudeSession) control(ctx context.Context, request map[string]any) error {
	id := randomToken(12)
	response := make(chan error, 1)
	s.mu.Lock()
	s.pendingControls[id] = response
	s.mu.Unlock()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return err
	}
	select {
	case err := <-response:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("Claude %s timed out", request["subtype"])
	}
}

func (a *agentService) readClaudeOutput(session *claudeSession, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var msg map[string]any
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		if msg["type"] == "control_response" {
			response := objectValue(msg["response"])
			id := stringValue(response["request_id"])
			session.mu.Lock()
			pending := session.pendingControls[id]
			delete(session.pendingControls, id)
			session.mu.Unlock()
			if pending != nil {
				if response["subtype"] == "success" {
					pending <- nil
				} else {
					pending <- errors.New(stringValue(response["error"]))
				}
			}
			continue
		}
		if msg["type"] == "stream_event" {
			event := objectValue(msg["event"])
			delta := objectValue(event["delta"])
			if event["type"] == "content_block_delta" && delta["type"] == "text_delta" {
				a.publishClaude(session, map[string]any{"type": "delta", "text": delta["text"]})
			}
			continue
		}
		if msg["type"] == "assistant" && msg["error"] != nil {
			a.publishClaude(session, map[string]any{"type": "error", "message": "Claude: " + stringValue(msg["error"])})
		}
		if msg["type"] == "result" {
			status := "completed"
			if msg["subtype"] != "success" || msg["is_error"] == true {
				status = "failed"
				a.publishClaude(session, map[string]any{"type": "error", "message": "Claude turn ended with " + stringValue(msg["subtype"])})
			}
			a.publishClaude(session, map[string]any{"type": "turn-end", "status": status})
			a.publishClaude(session, map[string]any{"type": "status", "status": "ready"})
		}
	}
}

func (a *agentService) publishClaude(s *claudeSession, event map[string]any) {
	event["sessionId"] = s.id
	send(a.socket, context.Background(), map[string]any{"type": "rpc-event", "service": "agent", "event": event})
}
func (a *agentService) claudeSession(id string) *claudeSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claude[id]
}
func (a *agentService) stopClaude(id string) bool {
	a.mu.Lock()
	s := a.claude[id]
	delete(a.claude, id)
	a.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true
	}
	s.closed = true
	s.mu.Unlock()
	s.closeToolServer()
	if s.command != nil && s.command.Process != nil {
		_ = s.command.Process.Kill()
	}
	return true
}
