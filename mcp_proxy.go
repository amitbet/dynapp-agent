package shellagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func (s *claudeSession) startToolServer(agent *agentService) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.listener, s.token = listener, randomToken(32)
	mux := http.NewServeMux()
	authorized := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+s.token }
	mux.HandleFunc("/tools", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(s.tools)
	})
	mux.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var call struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&call) != nil || call.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestID := randomToken(18)
		response := make(chan map[string]any, 1)
		s.mu.Lock()
		s.pendingTools[requestID] = response
		s.mu.Unlock()
		agent.publishClaude(s, map[string]any{"type": "tool-call", "requestId": requestID, "tool": call.Name, "arguments": call.Arguments})
		select {
		case result := <-response:
			_ = json.NewEncoder(w).Encode(result)
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "text": "The app did not answer the tool call within 120 seconds."})
		}
		s.mu.Lock()
		delete(s.pendingTools, requestID)
		s.mu.Unlock()
	})
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go func() { _ = s.server.Serve(listener) }()
	return nil
}

func (s *claudeSession) respondTool(id string, outcome map[string]any) bool {
	s.mu.Lock()
	pending := s.pendingTools[id]
	s.mu.Unlock()
	if pending == nil {
		return false
	}
	select {
	case pending <- outcome:
		return true
	default:
		return false
	}
}

func (s *claudeSession) closeToolServer() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.mu.Lock()
	for id, pending := range s.pendingTools {
		select {
		case pending <- map[string]any{"ok": false, "text": "The agent session closed before the app answered."}:
		default:
		}
		delete(s.pendingTools, id)
	}
	for id, pending := range s.pendingControls {
		select {
		case pending <- errors.New("the agent session closed"):
		default:
		}
		delete(s.pendingControls, id)
	}
	s.mu.Unlock()
}

// RunMCPProxy serves the small stdio MCP process Claude Code launches for a
// session. The bearer-protected loopback parent owns tool definitions and calls.
func RunMCPProxy(endpoint, token string, input io.Reader, output io.Writer) error {
	if endpoint == "" || token == "" {
		return errors.New("MCP proxy endpoint and token are required")
	}
	client := &http.Client{Timeout: 125 * time.Second}
	request := func(method, path string, body any, target any) error {
		var reader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return err
			}
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, strings.TrimRight(endpoint, "/")+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			return fmt.Errorf("MCP parent returned HTTP %d", response.StatusCode)
		}
		return json.NewDecoder(response.Body).Decode(target)
	}
	var tools []map[string]any
	if err := request(http.MethodGet, "/tools", nil, &tools); err != nil {
		return err
	}
	writer := json.NewEncoder(output)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var message map[string]any
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}
		id, hasID := message["id"]
		method := stringValue(message["method"])
		if !hasID {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": id}
		switch method {
		case "initialize":
			response["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "dynapp", "version": "1.0.0"}}
		case "ping":
			response["result"] = map[string]any{}
		case "tools/list":
			rows := make([]map[string]any, 0, len(tools))
			for _, tool := range tools {
				rows = append(rows, map[string]any{"name": tool["name"], "description": tool["description"], "inputSchema": tool["inputSchema"]})
			}
			response["result"] = map[string]any{"tools": rows}
		case "tools/call":
			params := objectValue(message["params"])
			outcome := map[string]any{}
			err := request(http.MethodPost, "/call", map[string]any{"name": params["name"], "arguments": params["arguments"]}, &outcome)
			if err != nil {
				response["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
			} else {
				response["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": stringValue(outcome["text"])}}, "isError": outcome["ok"] == false}
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "Method not found"}
		}
		if err := writer.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func RunMCPProxyMain(endpoint, token string) error {
	return RunMCPProxy(endpoint, token, os.Stdin, os.Stdout)
}
