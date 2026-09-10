package shellagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestProtocolFilesystemAndExecParity(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, hello := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	if hello["type"] != "hello" || hello["ok"] != true || hello["protocol"] != float64(ProtocolVersion) {
		t.Fatalf("unexpected hello: %#v", hello)
	}
	if environment, _ := hello["environment"].(map[string]any); environment["id"] != "local" {
		t.Fatalf("unexpected environment: %#v", hello)
	}
	features := hello["features"].(map[string]any)
	if features["multiplexedChannels"] != true || features["reliableStreams"] != false {
		t.Fatalf("unexpected v2 features: %#v", features)
	}

	temp := t.TempDir()
	file := filepath.Join(temp, "hello.txt")
	requestFS(t, ctx, connection, "write", "writeText", []any{file, "hello DynApp"})
	read := requestFS(t, ctx, connection, "read", "readText", []any{file, 1024})
	if got := read["content"]; got != "hello DynApp" {
		t.Fatalf("read content = %#v", got)
	}
	encoded, encodedBody := requestFSBinary(t, ctx, connection, "base64", "readBase64", []any{file, 1024})
	if string(encodedBody) != "hello DynApp" || encoded["size"] != float64(12) {
		t.Fatalf("base64 result=%#v body=%q", encoded, encodedBody)
	}
	chunk, chunkBody := requestFSBinary(t, ctx, connection, "chunk", "readChunk", []any{file, 6, 7})
	if string(chunkBody) != "DynApp" || chunk["bytesRead"] != float64(6) {
		t.Fatalf("chunk result=%#v body=%q", chunk, chunkBody)
	}

	encodedWrite := filepath.Join(temp, "encoded.txt")
	requestFSBinaryWrite(t, ctx, connection, "write-base64", "writeBase64", []any{encodedWrite, nil}, []byte("binary"))
	requestFSBinaryWrite(t, ctx, connection, "write-chunk", "writeChunk", []any{encodedWrite, 6, nil, false}, []byte("!"))
	if data, err := os.ReadFile(encodedWrite); err != nil || string(data) != "binary!" {
		t.Fatalf("chunk write = %q, %v", data, err)
	}
	directory := filepath.Join(temp, "nested")
	requestFS(t, ctx, connection, "mkdir", "mkdir", []any{directory})
	copyPath := filepath.Join(directory, "copied.txt")
	requestFS(t, ctx, connection, "copy", "copy", []any{file, copyPath})
	if data, err := os.ReadFile(copyPath); err != nil || string(data) != "hello DynApp" {
		t.Fatalf("copy = %q, %v", data, err)
	}
	listed := requestFS(t, ctx, connection, "list", "list", []any{temp})
	entries, ok := listed["entries"].([]any)
	if !ok || len(entries) != 3 {
		t.Fatalf("list = %#v", listed)
	}
	stat := requestFS(t, ctx, connection, "stat", "stat", []any{file})
	if stat["isDir"] != false || stat["size"] != float64(12) {
		t.Fatalf("stat = %#v", stat)
	}

	sendRequest(t, ctx, connection, map[string]any{"type": "exec", "id": "exec", "file": "/bin/echo", "args": []string{"agent"}})
	var started, output, exit map[string]any
	for exit == nil {
		message := receive(t, ctx, connection)
		if message["type"] == "exec-start" {
			started = message
		}
		if message["type"] == "exec-output" {
			output = message
		}
		if message["type"] == "exec-exit" {
			exit = message
		}
	}
	requestFS(t, ctx, connection, "move", "move", []any{copyPath, filepath.Join(directory, "moved.txt")})
	size := requestFS(t, ctx, connection, "size", "dirSize", []any{temp})
	if size["files"].(float64) < 3 {
		t.Fatalf("dir size = %#v", size)
	}
	disk := requestFS(t, ctx, connection, "disk", "diskUsage", []any{temp})
	if disk["total"].(float64) <= 0 || disk["free"].(float64) <= 0 {
		t.Fatalf("disk usage = %#v", disk)
	}
	requestFS(t, ctx, connection, "remove", "remove", []any{directory})
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("remove error = %v", err)
	}
	if started == nil || !strings.Contains(output["data"].(string), "agent") || exit["code"] != float64(0) {
		t.Fatalf("exec output=%#v exit=%#v", output, exit)
	}
}

func TestProductionHandlerAcceptsLoopbackWebSocket(t *testing.T) {
	// The production handler runs the same handshake as the test carrier; the
	// test flag stands in for the authority's decision. A development
	// (loopback) Origin is required.
	server := httptest.NewServer((&Server{StateDir: t.TempDir(), autoApprovePairings: true}).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response := dialTestAgent(t, ctx, server, testDialOptions{Origin: "http://localhost:5179"})
	defer connection.Close(websocket.StatusNormalClosure, "")
	if response["type"] != "hello" || response["ok"] != true {
		t.Fatalf("hello response = %#v", response)
	}
}

func TestRejectsUnauthenticatedConnection(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+RemotePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	// A hello before the direct-local challenge is not a valid init record.
	sendRequest(t, ctx, connection, map[string]any{"type": "hello", "protocol": ProtocolVersion, "local": true})
	_, _, err = connection.Read(ctx)
	if websocket.CloseStatus(err) != closeIdentityRejected {
		t.Fatalf("close error = %v", err)
	}
}

func TestProtocolV1RemainsCompatible(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, hello := dialTestAgent(t, ctx, server, testDialOptions{Protocol: LegacyProtocolVersion})
	defer connection.Close(websocket.StatusNormalClosure, "")
	if hello["protocol"] != float64(LegacyProtocolVersion) {
		t.Fatalf("v1 hello = %#v", hello)
	}
	if features := hello["features"].(map[string]any); len(features) != 0 {
		t.Fatalf("v1 features = %#v", features)
	}
}

func TestHostedRelayAuthenticatesAndResetsSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		(&Server{}).handleRelaySocket(ctx, connection, RelayConnection{AcceptedAuthTokenHashes: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, Config{EnvironmentID: "env_test"})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	payload, _ := json.Marshal(map[string]any{"type": "hello", "protocol": ProtocolVersion, "tokenHash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	envelope, _ := json.Marshal(map[string]any{"type": "dynapp-relay-frame", "session": "browser-a", "payload": string(payload)})
	if err := connection.Write(ctx, websocket.MessageText, envelope); err != nil {
		t.Fatal(err)
	}
	_, helloWire, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello map[string]any
	_ = json.Unmarshal(helloWire, &hello)
	if hello["type"] != "hello" || hello["protocol"] != float64(ProtocolVersion) {
		t.Fatalf("relay hello = %#v", hello)
	}
	second, _ := json.Marshal(map[string]any{"type": "ping", "id": "unauthenticated"})
	envelope, _ = json.Marshal(map[string]any{"type": "dynapp-relay-frame", "session": "browser-b", "payload": string(second)})
	if err := connection.Write(ctx, websocket.MessageText, envelope); err != nil {
		t.Fatal(err)
	}
	_, response, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rejected map[string]any
	_ = json.Unmarshal(response, &rejected)
	if rejected["error"] != "Pairing credential rejected" {
		t.Fatalf("session reset = %#v", rejected)
	}
}

func TestRemoteE2EEEnvelopeRoundTrip(t *testing.T) {
	tokenHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sealed, err := sealRemoteFrame(tokenHash, []byte("protected remote record"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openRemoteFrame(tokenHash, sealed)
	if err != nil || string(opened) != "protected remote record" {
		t.Fatalf("open = %q, %v", opened, err)
	}
	if remoteKeyID(tokenHash) == remoteKeyID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatal("key ids must be credential-specific")
	}
	if _, err := openRemoteFrame("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", sealed); err == nil {
		t.Fatal("wrong credential decrypted a frame")
	}
}

func TestProtocolTCPBridge(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		connection, acceptErr := target.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = connection.Write([]byte("ready"))
		buffer := make([]byte, 32)
		count, _ := connection.Read(buffer)
		if count > 0 {
			_, _ = connection.Write(buffer[:count])
		}
	}()
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	port := target.Addr().(*net.TCPAddr).Port
	sendRequest(t, ctx, connection, map[string]any{"type": "net", "id": "open", "action": "open", "protocol": "tcp", "target": map[string]any{"host": "127.0.0.1", "port": port}})
	opened := receive(t, ctx, connection)
	if opened["type"] != "net-result" {
		t.Fatalf("open = %#v", opened)
	}
	bridgeID := opened["result"].(map[string]any)["bridgeId"].(string)
	assertBridgeFrame(t, ctx, connection, bridgeID, "ready")
	sendBridgeFrame(t, ctx, connection, map[string]any{"type": "net-data", "bridgeId": bridgeID}, []byte("echo"))
	assertBridgeFrame(t, ctx, connection, bridgeID, "echo")
	sendRequest(t, ctx, connection, map[string]any{"type": "net-close", "bridgeId": bridgeID})
}

func TestExecForwardsEnvironment(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{
		"type": "exec", "id": "env", "file": "/usr/bin/env",
		"env": map[string]any{"DYNAPP_REMOTE_SSH_PASSWORD": "secret-value"},
	})
	var stdout string
	for {
		message := receive(t, ctx, connection)
		if message["type"] == "exec-output" && message["stream"] != "stderr" {
			if data, ok := message["data"].(string); ok {
				stdout += data
			}
		}
		if message["type"] == "exec-exit" {
			break
		}
		if message["type"] == "exec-error" {
			t.Fatalf("exec-error = %#v", message)
		}
	}
	if !strings.Contains(stdout, "DYNAPP_REMOTE_SSH_PASSWORD=secret-value") {
		t.Fatalf("env output missing password variable: %q", stdout)
	}
}

func TestStreamingProcessSupportsInputAndOutput(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{"type": "exec", "id": "stream", "stream": true, "file": "/bin/sh", "args": []string{"-c", "printf ready; read value; printf \":$value\""}})
	started := receive(t, ctx, connection)
	if started["type"] != "exec-start" || started["processId"] == nil {
		t.Fatalf("start = %#v", started)
	}
	processID := started["processId"].(string)
	seenReady := false
	for !seenReady {
		event := receive(t, ctx, connection)
		if event["type"] == "exec-output" && event["data"] == "ready" {
			seenReady = true
		}
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "process", "id": "write", "processId": processID, "action": "write", "data": "input\n"})
	seenExit := false
	seenInput := false
	for !seenExit {
		event := receive(t, ctx, connection)
		if event["type"] == "exec-output" && event["data"] == ":input" {
			seenInput = true
		}
		if event["type"] == "exec-exit" {
			seenExit = true
		}
	}
	if !seenInput {
		t.Fatal("streamed stdin did not reach child")
	}
}

func TestStreamingPTYSupportsTerminalDetectionInputAndResize(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{
		"type": "exec", "id": "pty", "stream": true, "mode": "pty", "cols": 91, "rows": 37,
		// dash (Ubuntu /bin/sh) aborts `read` on SIGWINCH; ignore WINCH so a
		// live resize cannot make the PTY exit before stdin is echoed.
		"file": "/bin/sh", "args": []string{"-c", "trap '' WINCH; test -t 0 && stty size && printf 'TERM=%s\\n' \"$TERM\"; read value; printf ':%s' \"$value\""},
	})
	started := receive(t, ctx, connection)
	if started["type"] != "exec-start" || started["processId"] == nil {
		t.Fatalf("start = %#v", started)
	}
	processID := started["processId"].(string)
	var output string
	for !strings.Contains(output, "37 91") || !strings.Contains(output, "TERM=xterm-256color") {
		event := receive(t, ctx, connection)
		if event["type"] == "exec-output" {
			output += event["data"].(string)
		}
		if event["type"] == "exec-exit" {
			t.Fatalf("PTY exited before terminal probe finished: %q", output)
		}
		if event["type"] == "exec-error" {
			t.Fatalf("exec-error = %#v", event)
		}
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "process", "id": "resize", "processId": processID, "action": "resize", "cols": 100, "rows": 40})
	for {
		event := receive(t, ctx, connection)
		if event["type"] == "process-result" && event["id"] == "resize" {
			break
		}
		if event["type"] == "exec-output" {
			if data, ok := event["data"].(string); ok {
				output += data
			}
		}
		if event["type"] == "exec-exit" {
			t.Fatalf("PTY exited during resize: %q", output)
		}
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "process", "id": "write-pty", "processId": processID, "action": "write", "data": "input\n"})
	for !strings.Contains(output, ":input") {
		event := receive(t, ctx, connection)
		if event["type"] == "exec-output" {
			output += event["data"].(string)
		}
		if event["type"] == "exec-exit" && !strings.Contains(output, ":input") {
			t.Fatalf("PTY exited before input was echoed: %q", output)
		}
	}
}

func TestStreamingProcessCanIgnoreStdin(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server)
	defer connection.Close(websocket.StatusNormalClosure, "")
	sendRequest(t, ctx, connection, map[string]any{
		"type": "exec", "id": "ignore-stdin", "stream": true, "stdin": "ignore",
		"file": "/bin/sh", "args": []string{"-c", "if read value; then printf unexpected; else printf eof; fi"},
	})
	started := receive(t, ctx, connection)
	if started["type"] != "exec-start" || started["processId"] == nil {
		t.Fatalf("start = %#v", started)
	}
	seenEOF := false
	for {
		event := receive(t, ctx, connection)
		if event["type"] == "exec-output" && event["data"] == "eof" {
			seenEOF = true
		}
		if event["type"] == "exec-exit" {
			break
		}
	}
	if !seenEOF {
		t.Fatal("ignored stdin was not connected to EOF")
	}
}

func requestFS(t *testing.T, ctx context.Context, connection *websocket.Conn, id, method string, args []any) map[string]any {
	t.Helper()
	sendRequest(t, ctx, connection, map[string]any{"type": "fs", "id": id, "method": method, "args": args})
	reply := receive(t, ctx, connection)
	if reply["type"] != "fs-result" || reply["id"] != id {
		t.Fatalf("fs %s = %#v", method, reply)
	}
	if reply["result"] == nil {
		return map[string]any{}
	}
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("fs %s result = %#v", method, reply["result"])
	}
	return result
}
func requestFSBinary(t *testing.T, ctx context.Context, connection *websocket.Conn, id, method string, args []any) (map[string]any, []byte) {
	t.Helper()
	sendRequest(t, ctx, connection, map[string]any{"type": "fs", "id": id, "method": method, "args": args})
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	header, body, err := decodeBinaryFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != "fs-result" || header.ID != id {
		t.Fatalf("fs %s header = %#v", method, header)
	}
	var raw map[string]any
	if err := json.Unmarshal(data[8:8+int(binary.BigEndian.Uint32(data[4:8]))], &raw); err != nil {
		t.Fatal(err)
	}
	result, ok := raw["result"].(map[string]any)
	if !ok {
		t.Fatalf("fs %s result = %#v", method, raw)
	}
	return result, body
}
func requestFSBinaryWrite(t *testing.T, ctx context.Context, connection *websocket.Conn, id, method string, args []any, body []byte) {
	t.Helper()
	header, err := json.Marshal(map[string]any{"type": "fs", "id": id, "method": method, "args": args})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 8+len(header)+len(body))
	copy(frame[:4], "DFB1")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(header)))
	copy(frame[8:], header)
	copy(frame[8+len(header):], body)
	if err := connection.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	response := receive(t, ctx, connection)
	if response["type"] != "fs-result" || response["id"] != id {
		t.Fatalf("binary fs %s = %#v", method, response)
	}
}
func sendBridgeFrame(t *testing.T, ctx context.Context, connection *websocket.Conn, header any, body []byte) {
	t.Helper()
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 8+len(encoded)+len(body))
	copy(frame[:4], "DFB1")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(encoded)))
	copy(frame[8:], encoded)
	copy(frame[8+len(encoded):], body)
	if err := connection.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
}
func assertBridgeFrame(t *testing.T, ctx context.Context, connection *websocket.Conn, bridgeID, want string) {
	t.Helper()
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	header, body, err := decodeBinaryFrame(data)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != "net-data" || header.BridgeID != bridgeID || string(body) != want {
		t.Fatalf("bridge frame header=%#v body=%q", header, body)
	}
}
func sendRequest(t *testing.T, ctx context.Context, connection *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}
func receive(t *testing.T, ctx context.Context, connection *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestHTTPRequestsStayOrderedOnOneConnection(t *testing.T) {
	started := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started <- struct{}{}
		time.Sleep(200 * time.Millisecond)
		_, _ = response.Write([]byte("ok"))
	}))
	defer upstream.Close()
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server, testDialOptions{Connect: map[string]any{"http": []any{upstream.URL + "/"}}})
	defer connection.Close(websocket.StatusNormalClosure, "")
	for _, id := range []string{"h1", "h2"} {
		sendRequest(t, ctx, connection, map[string]any{
			"type":    "rpc",
			"id":      id,
			"service": "http",
			"method":  "request",
			"args":    []any{map[string]any{"url": upstream.URL, "method": "GET", "connect": []any{upstream.URL + "/"}}},
		})
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first HTTP RPC did not start")
	}
	select {
	case <-started:
		t.Fatal("second HTTP RPC started before the first response completed")
	case <-time.After(100 * time.Millisecond):
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		response := receive(t, ctx, connection)
		if response["type"] != "rpc-result" {
			t.Fatalf("rpc = %#v", response)
		}
		id, _ := response["id"].(string)
		seen[id] = true
	}
	if !seen["h1"] || !seen["h2"] {
		t.Fatalf("results = %#v", seen)
	}
}

func TestLANRPCRequiresRemoteEnvironmentManagementCapability(t *testing.T) {
	request := message{Service: "lan", Method: "setEnabled"}
	if required := rpcCapability(request); required != "remoteEnv.manage" {
		t.Fatalf("LAN RPC capability = %q", required)
	}
	if socketAllows([]string{"fs.remote"}, rpcCapability(request)) {
		t.Fatal("ordinary remote filesystem permission granted LAN administration")
	}
}

func TestSocketAllowsFailsClosedWhenCapabilitiesAreMissing(t *testing.T) {
	if socketAllows(nil, "fs.remote") {
		t.Fatal("nil capabilities granted fs.remote")
	}
	if socketAllows([]string{}, "fs.execFile") {
		t.Fatal("empty capabilities granted fs.execFile")
	}
	if !socketAllows(nil, "") {
		t.Fatal("empty required capability should skip the gate")
	}
	if !socketAllows([]string{"fs.remote"}, "fs.list") {
		t.Fatal("fs.remote should still cover structured filesystem methods")
	}
	if socketAllows([]string{"fs.remote"}, "fs.exec") {
		t.Fatal("fs.remote should not cover fs.exec")
	}
}

func TestCapabilityCrossoversAreRemoved(t *testing.T) {
	for _, required := range []string{"fs.execFile", "fs.execTerminal", "fs.openPath", "fs.openWith"} {
		if socketAllows([]string{"fs.remote"}, required) {
			t.Fatalf("fs.remote granted %s", required)
		}
	}
	if socketAllows([]string{"fs.execFile"}, "clipboard.write") {
		t.Fatal("fs.execFile implied clipboard.write")
	}
	if !socketAllows([]string{"fs.remote"}, "fs.readText") {
		t.Fatal("fs.remote should still cover fs.readText")
	}
}

func TestSameCapabilitySetIgnoresOrderAndDuplicates(t *testing.T) {
	if !sameCapabilitySet([]string{"fs.remote", "fs.execFile"}, []string{"fs.execFile", "fs.remote", "fs.remote"}) {
		t.Fatal("reordered capability sets should match")
	}
	if sameCapabilitySet([]string{"fs.remote"}, []string{"fs.execFile"}) {
		t.Fatal("different capability sets should not match")
	}
}

func TestDirectLocalInfoAdvertisesWebTransportCertificate(t *testing.T) {
	server := httptest.NewServer((&Server{
		StateDir: t.TempDir(),
		Config:   Config{EnvironmentID: "env_local", LANEnabled: true, LANAddress: "127.0.0.1:9011"},
	}).Handler())
	defer server.Close()
	options, err := http.NewRequest(http.MethodOptions, server.URL+"/.well-known/dynapp-direct-local", nil)
	if err != nil {
		t.Fatal(err)
	}
	optionsResponse, err := http.DefaultClient.Do(options)
	if err != nil {
		t.Fatal(err)
	}
	_ = optionsResponse.Body.Close()
	if optionsResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d", optionsResponse.StatusCode)
	}
	if optionsResponse.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("direct-local info must be readable from hosted HTTPS apps")
	}
	response, err := http.Get(server.URL + "/.well-known/dynapp-direct-local")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["carrier"] != "webtransport" || payload["environmentId"] != "env_local" {
		t.Fatalf("payload = %#v", payload)
	}
	hashes, _ := payload["serverCertificateHashes"].([]any)
	if len(hashes) == 0 {
		t.Fatal("expected a WebTransport certificate hash")
	}
}

// newTestServer builds the in-process agent used by protocol tests. The test
// carrier still runs the full direct-local handshake; pending decisions are
// auto-approved with the declared set and a missing Origin defaults to a
// loopback development origin.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{testWebSocket: true, autoApprovePairings: true, StateDir: t.TempDir()}
}

type testDialOptions struct {
	Origin   string
	StoreID  string
	Declared []string
	Connect  map[string]any
	Client   *BrowserClient
	Protocol int
	Browser  *testBrowser
	// InitVersion overrides the init record version (default 2).
	InitVersion int
	// SkipApp omits the app block from the init record.
	SkipApp bool
	// AllowPending returns the pending frame instead of waiting for hello.
	ReturnPending bool
	// ExpectedClose returns the close reason instead of requiring a hello.
	ExpectedClose websocket.StatusCode
}

type testBrowser struct {
	private *ecdsa.PrivateKey
	key     BrowserJWK
	keyID   string
}

func newTestBrowser(t *testing.T) *testBrowser {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := BrowserJWK{
		KTY: "EC", CRV: "P-256",
		X: base64.RawURLEncoding.EncodeToString(private.PublicKey.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(private.PublicKey.Y.FillBytes(make([]byte, 32))),
	}
	return &testBrowser{private: private, key: key, keyID: BrowserKeyID(key)}
}

func (b *testBrowser) sign(t *testing.T, nonce, origin string) string {
	t.Helper()
	digest := sha256.Sum256(directLocalPayload(nonce, origin, b.keyID))
	r, s, err := ecdsa.Sign(rand.Reader, b.private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...))
}

// dialTestAgent connects over the loopback WebSocket, completes the
// direct-local challenge (init v2 with an app block), waits through a pending
// approval when one is issued, and exchanges hello.
func dialTestAgent(t *testing.T, ctx context.Context, server *httptest.Server, options ...testDialOptions) (*websocket.Conn, map[string]any) {
	t.Helper()
	var option testDialOptions
	if len(options) > 0 {
		option = options[0]
	}
	connection, frames := startTestHandshake(t, ctx, server, option)
	hello := frames
	if hello["type"] == "dynapp-direct-local-pending" && !option.ReturnPending {
		hello = receive(t, ctx, connection)
	}
	return connection, hello
}

// startTestHandshake runs init/challenge/auth and hello, returning the first
// post-auth frame (pending or hello).
func startTestHandshake(t *testing.T, ctx context.Context, server *httptest.Server, option testDialOptions) (*websocket.Conn, map[string]any) {
	t.Helper()
	origin := option.Origin
	if origin == "" {
		origin = testDevelopmentOrigin
	}
	browser := option.Browser
	if browser == nil {
		browser = newTestBrowser(t)
	}
	headers := http.Header{}
	if option.Origin != "" {
		headers.Set("Origin", option.Origin)
	}
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+RemotePath, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	storeID := option.StoreID
	if storeID == "" {
		storeID = "test/app"
	}
	declared := option.Declared
	if declared == nil {
		declared = capabilities()
	}
	version := option.InitVersion
	if version == 0 {
		version = 2
	}
	init := map[string]any{"type": "dynapp-direct-local-init", "version": version, "publicKeyJwk": browser.key}
	if option.Client != nil {
		init["client"] = option.Client
	}
	if !option.SkipApp {
		app := map[string]any{"storeId": storeID, "revision": nil, "declaredPermissions": declared}
		if option.Connect != nil {
			app["connect"] = option.Connect
		}
		init["app"] = app
	}
	sendRequest(t, ctx, connection, init)
	challenge := receive(t, ctx, connection)
	if challenge["type"] != "dynapp-direct-local-challenge" {
		t.Fatalf("challenge = %#v", challenge)
	}
	nonce, _ := challenge["nonce"].(string)
	sendRequest(t, ctx, connection, map[string]any{"type": "dynapp-direct-local-auth", "version": 1, "keyId": browser.keyID, "origin": origin, "signature": browser.sign(t, nonce, origin)})
	protocol := option.Protocol
	if protocol == 0 {
		protocol = ProtocolVersion
	}
	sendRequest(t, ctx, connection, map[string]any{"type": "hello", "protocol": protocol})
	if option.ExpectedClose != 0 {
		_, _, err := connection.Read(ctx)
		if websocket.CloseStatus(err) != option.ExpectedClose {
			t.Fatalf("close = %v, want %d", err, option.ExpectedClose)
		}
		var closeError websocket.CloseError
		if !errors.As(err, &closeError) {
			t.Fatalf("close error = %v", err)
		}
		return connection, map[string]any{"closeCode": int(option.ExpectedClose), "closeReason": closeError.Reason}
	}
	return connection, receive(t, ctx, connection)
}
