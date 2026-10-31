package shellagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"nhooyr.io/websocket"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestBodyLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, strconv.FormatInt(n, 10))
	}))
	defer upstream.Close()
	for _, size := range []int{1024*1024 + 1, 10 * 1024 * 1024, 10*1024*1024 + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			result, err := (&Server{}).handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{
				"url": upstream.URL, "method": "POST", "body": strings.Repeat("x", size), "connect": []string{upstream.URL + "/"},
			}}})
			if size > 10*1024*1024 {
				if err == nil || !strings.Contains(err.Error(), "10 MiB") {
					t.Fatalf("oversized request error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := result.(map[string]any)["text"]; got != strconv.Itoa(size) {
				t.Fatalf("received bytes = %v, want %d", got, size)
			}
		})
	}
}

func TestHTTPBinaryUploadOverPairedSocket(t *testing.T) {
	payload := []byte(strings.Repeat("\x00\xff\x80x", 10*1024*1024/4))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if string(data) != string(payload) {
			t.Error("binary upload corrupted")
		}
		_, _ = io.WriteString(w, strconv.Itoa(len(data)))
	}))
	defer upstream.Close()
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socket, hello := dialTestAgent(t, ctx, server, testDialOptions{Connect: map[string]any{"http": []any{upstream.URL + "/"}}})
	defer socket.Close(websocket.StatusNormalClosure, "")
	if hello["features"].(map[string]any)["httpBinaryBody"] != true {
		t.Fatal("binary HTTP feature missing")
	}
	if !sendBinary(socket, ctx, map[string]any{"type": "rpc", "id": "upload", "service": "http", "method": "request", "args": []any{map[string]any{"url": upstream.URL, "method": "POST"}}}, payload) {
		t.Fatal("send failed")
	}
	result := receive(t, ctx, socket)
	if result["type"] != "rpc-result" || result["result"].(map[string]any)["text"] != strconv.Itoa(len(payload)) {
		t.Fatalf("upload result = %v", result)
	}
}
