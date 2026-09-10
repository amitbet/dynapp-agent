package shellagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type agentService struct {
	server   *Server
	socket   protocolSocket
	mu       sync.Mutex
	next     uint64
	sessions map[string]*agentSession
	claude   map[string]*claudeSession
}
type agentSession struct {
	id, appID, threadID, turnID, model, effort string
	command                                    *exec.Cmd
	stdin                                      io.WriteCloser
	mu                                         sync.Mutex
	next                                       int
	pending                                    map[int]chan agentResponse
}
type agentResponse struct {
	result json.RawMessage
	err    error
}

func newAgentService(server *Server, socket protocolSocket) *agentService {
	return &agentService{server: server, socket: socket, sessions: map[string]*agentSession{}, claude: map[string]*claudeSession{}}
}
func (a *agentService) handle(ctx context.Context, request message) (any, error) {
	switch request.Method {
	case "listAgents":
		return discoverAgents(ctx), nil
	case "start":
		options := map[string]any{}
		if len(request.Args) > 0 {
			options, _ = request.Args[0].(map[string]any)
		}
		requested := stringValue(options["agentId"])
		selected := ""
		for _, provider := range discoverAgents(ctx) {
			if provider["available"] == true && (selected == "" || provider["id"] == requested) {
				selected = stringValue(provider["id"])
				if selected == requested {
					break
				}
			}
		}
		if selected == "" {
			return nil, errors.New("no configured coding agent was found; install and sign in to Codex or Claude Code first")
		}
		if selected == "claude" {
			return a.startClaude(ctx, request, options)
		}
		return a.start(ctx, request)
	}
	if claude := a.claudeSession(request.SessionID); claude != nil {
		return a.handleClaude(ctx, claude, request)
	}
	session := a.session(request.SessionID)
	if session == nil {
		return nil, errors.New("the agent session is no longer available")
	}
	switch request.Method {
	case "listModels":
		var response struct {
			Data []map[string]any `json:"data"`
		}
		if err := session.call(ctx, "model/list", map[string]any{}, &response); err != nil {
			return nil, err
		}
		return normalizeAgentModels(response.Data), nil
	case "setModel":
		if len(request.Args) > 0 {
			if value, ok := request.Args[0].(map[string]any); ok {
				session.model = stringValue(value["model"])
				session.effort = stringValue(value["effort"])
			}
		}
		return map[string]any{"model": session.model, "effort": session.effort}, nil
	case "prompt":
		return session.prompt(ctx, request.Args)
	case "interrupt":
		if session.turnID == "" {
			return false, nil
		}
		var ignored any
		err := session.call(ctx, "turn/interrupt", map[string]any{"threadId": session.threadID, "turnId": session.turnID}, &ignored)
		return err == nil, err
	case "respondTool":
		if len(request.Args) < 2 {
			return nil, errors.New("tool result is invalid")
		}
		requestID := request.Args[0]
		outcome, _ := request.Args[1].(map[string]any)
		return true, session.write(map[string]any{"id": requestID, "result": map[string]any{"success": outcome["ok"] != false, "contentItems": []map[string]any{{"type": "inputText", "text": stringValue(outcome["text"])}}}})
	case "stop":
		return a.stop(session.id), nil
	default:
		return nil, errors.New("unsupported agent method")
	}
}

func normalizeAgentModels(models []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := stringValue(model["model"])
		if id == "" {
			id = stringValue(model["id"])
		}
		if id == "" || model["hidden"] == true || strings.Contains(strings.ToLower(id), "fast") {
			continue
		}
		label := stringValue(model["displayName"])
		if label == "" {
			label = id
		}
		efforts := []map[string]any{}
		if raw, ok := model["supportedReasoningEfforts"].([]any); ok {
			for _, item := range raw {
				effortID := stringValue(item)
				description := ""
				if entry, ok := item.(map[string]any); ok {
					effortID = stringValue(entry["reasoningEffort"])
					description = stringValue(entry["description"])
				}
				if effortID != "" {
					efforts = append(efforts, map[string]any{"id": effortID, "label": effortID, "description": description})
				}
			}
		}
		result = append(result, map[string]any{
			"id": id, "label": label, "description": stringValue(model["description"]),
			"isDefault": model["isDefault"] == true, "efforts": efforts,
			"defaultEffort": model["defaultReasoningEffort"],
		})
	}
	return result
}
func (a *agentService) start(ctx context.Context, request message) (any, error) {
	path := strings.TrimSpace(os.Getenv("DYNAPP_AGENT_BINARY"))
	if path == "" {
		path, _ = exec.LookPath("codex")
	}
	if path == "" {
		return nil, errors.New("Codex is not installed or is not on the service PATH")
	}
	options := map[string]any{}
	if len(request.Args) > 0 {
		options, _ = request.Args[0].(map[string]any)
	}
	appID := request.AppID
	if appID == "" {
		appID = "pwa"
	}
	root := a.server.StateDir
	if root == "" {
		root, _ = DefaultStateDir()
	}
	cwd := filepath.Join(root, "agent-workspaces", safeName(appID))
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return nil, err
	}
	command := exec.Command(path, "app-server")
	command.Dir = cwd
	if home := strings.TrimSpace(os.Getenv("DYNAPP_AGENT_HOME")); home != "" {
		command.Env = append(os.Environ(), "CODEX_HOME="+home)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.next++
	id := "agent_" + strconv.FormatUint(a.next, 36)
	session := &agentSession{id: id, appID: appID, command: command, stdin: stdin, pending: map[int]chan agentResponse{}, model: stringValue(options["model"]), effort: stringValue(options["effort"])}
	a.sessions[id] = session
	a.mu.Unlock()
	go a.readOutput(session, stdout)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			a.publish(session, map[string]any{"type": "log", "text": scanner.Text()})
		}
	}()
	go func() {
		_ = command.Wait()
		a.publish(session, map[string]any{"type": "status", "status": "stopped"})
		a.stop(id)
	}()
	var initialized any
	if err := session.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "dynapp-shell", "title": "DynApp", "version": "1.0.0"}, "capabilities": map[string]any{"experimentalApi": true}}, &initialized); err != nil {
		a.stop(id)
		return nil, err
	}
	_ = session.write(map[string]any{"method": "initialized"})
	params := map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "workspace-write", "serviceTier": "default"}
	if session.model != "" {
		params["model"] = session.model
	}
	if tools, ok := options["tools"].([]any); ok && len(tools) > 0 {
		params["dynamicTools"] = tools
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		ThreadID string `json:"threadId"`
	}
	if err := session.call(ctx, "thread/start", params, &started); err != nil {
		a.stop(id)
		return nil, err
	}
	session.threadID = started.Thread.ID
	if session.threadID == "" {
		session.threadID = started.ThreadID
	}
	if session.threadID == "" {
		a.stop(id)
		return nil, errors.New("the agent did not return a thread id")
	}
	a.publish(session, map[string]any{"type": "status", "status": "ready"})
	return map[string]any{"sessionId": id, "threadId": session.threadID, "cwd": cwd, "agentId": "codex"}, nil
}
func (a *agentService) readOutput(session *agentSession, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 32*1024*1024)
	for scanner.Scan() {
		var message map[string]any
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		if idNumber, ok := message["id"].(float64); ok && (message["result"] != nil || message["error"] != nil) {
			id := int(idNumber)
			session.mu.Lock()
			pending := session.pending[id]
			delete(session.pending, id)
			session.mu.Unlock()
			if pending != nil {
				raw, _ := json.Marshal(message["result"])
				if problem := message["error"]; problem != nil {
					pending <- agentResponse{err: fmt.Errorf("agent request failed: %v", problem)}
				} else {
					pending <- agentResponse{result: raw}
				}
			}
			continue
		}
		method := stringValue(message["method"])
		params, _ := message["params"].(map[string]any)
		if _, hasID := message["id"]; hasID && method != "" {
			if method == "item/tool/call" {
				a.publish(session, map[string]any{"type": "tool-call", "requestId": message["id"], "tool": params["tool"], "arguments": params["arguments"]})
			} else {
				_ = session.write(map[string]any{"id": message["id"], "result": map[string]any{"decision": "decline", "success": false, "contentItems": []any{}}})
			}
			continue
		}
		switch method {
		case "item/agentMessage/delta":
			a.publish(session, map[string]any{"type": "delta", "text": params["delta"]})
		case "turn/started":
			if turn, ok := params["turn"].(map[string]any); ok {
				session.turnID = stringValue(turn["id"])
			}
			a.publish(session, map[string]any{"type": "status", "status": "running"})
		case "turn/completed":
			session.turnID = ""
			status := "completed"
			if turn, ok := params["turn"].(map[string]any); ok {
				status = stringValue(turn["status"])
			}
			a.publish(session, map[string]any{"type": "turn-end", "status": status})
			a.publish(session, map[string]any{"type": "status", "status": "ready"})
		case "error":
			a.publish(session, map[string]any{"type": "error", "message": params["message"]})
		}
	}
}
func (s *agentSession) prompt(ctx context.Context, args []any) (any, error) {
	text := ""
	images := []any{}
	if len(args) > 0 {
		if value, ok := args[0].(map[string]any); ok {
			text = stringValue(value["text"])
			images, _ = value["images"].([]any)
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	input := []map[string]any{{"type": "text", "text": text}}
	for _, image := range images {
		input = append(input, map[string]any{"type": "localImage", "path": stringValue(image), "detail": "auto"})
	}
	params := map[string]any{"threadId": s.threadID, "input": input, "approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "workspaceWrite"}, "serviceTier": "default"}
	if s.model != "" {
		params["model"] = s.model
	}
	if s.effort != "" {
		params["effort"] = s.effort
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turnId"`
	}
	if err := s.call(ctx, "turn/start", params, &response); err != nil {
		return nil, err
	}
	s.turnID = response.Turn.ID
	if s.turnID == "" {
		s.turnID = response.TurnID
	}
	return map[string]any{"turnId": s.turnID}, nil
}
func (s *agentSession) call(ctx context.Context, method string, params any, target any) error {
	s.mu.Lock()
	s.next++
	id := s.next
	pending := make(chan agentResponse, 1)
	s.pending[id] = pending
	s.mu.Unlock()
	if err := s.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	select {
	case response := <-pending:
		if response.err != nil {
			return response.err
		}
		if target != nil {
			return json.Unmarshal(response.result, target)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%s timed out", method)
	}
}
func (s *agentSession) write(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(data, '\n'))
	return err
}
func (a *agentService) publish(session *agentSession, event map[string]any) {
	event["sessionId"] = session.id
	send(a.socket, context.Background(), map[string]any{"type": "rpc-event", "service": "agent", "event": event})
}
func (a *agentService) session(id string) *agentSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessions[id]
}
func (a *agentService) stop(id string) bool {
	a.mu.Lock()
	session := a.sessions[id]
	delete(a.sessions, id)
	a.mu.Unlock()
	if session == nil {
		return false
	}
	_ = session.command.Process.Kill()
	return true
}
func (a *agentService) closeAll() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.sessions))
	for id := range a.sessions {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.stop(id)
	}
	a.mu.Lock()
	claudeIDs := make([]string, 0, len(a.claude))
	for id := range a.claude {
		claudeIDs = append(claudeIDs, id)
	}
	a.mu.Unlock()
	for _, id := range claudeIDs {
		a.stopClaude(id)
	}
}
