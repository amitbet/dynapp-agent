//go:build windows

package winhost

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	webview2 "github.com/jchv/go-webview2"
)

type controlTestView struct {
	webview2.WebView
	scripts chan string
}

func (v *controlTestView) Dispatch(fn func()) { fn() }
func (v *controlTestView) Eval(script string) { v.scripts <- script }

func controlTestHost(t *testing.T) (*host, net.Listener, *controlTestView) {
	t.Helper()
	pipe := fmt.Sprintf(`\\.\pipe\dynapp-control-test-%d`, time.Now().UnixNano())
	listener, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	view := &controlTestView{scripts: make(chan string, 32)}
	h := &host{
		options: Options{StoreID: "owner/test", Pipe: pipe, Files: []string{`C:\startup.txt`}},
		origin:  "https://example.com", startURL: "https://example.com/original",
		view: view, sockets: map[string]*frameConn{}, reviews: map[string]string{},
	}
	t.Cleanup(h.closeAll)
	return h, listener, view
}

func readControlTestFrame(t *testing.T, peer *frameConn) map[string]any {
	t.Helper()
	kind, payload, err := peer.read()
	if err != nil || kind != frameText {
		t.Fatalf("read frame: kind=%d error=%v", kind, err)
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func acceptControlTestPeer(t *testing.T, listener net.Listener) *frameConn {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out accepting control connection")
	}
	if conn == nil {
		t.Fatal("control listener closed")
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	peer := &frameConn{conn: conn, reader: bufio.NewReader(conn)}
	hello := readControlTestFrame(t, peer)
	if hello["type"] != "native-host-hello" || hello["storeId"] != "owner/test" || hello["purpose"] != "control" {
		t.Fatalf("unauthenticated control reconnect: %v", hello)
	}
	return peer
}

func waitControlDisconnected(t *testing.T, h *host) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		disconnected := h.control == nil
		h.mu.Unlock()
		if disconnected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("control connection was not cleared")
}

func TestNativeWindowsControlReconnectAfterAgentRestart(t *testing.T) {
	h, listener, view := controlTestHost(t)
	started := make(chan string, 1)
	go func() { started <- h.connectControl() }()
	first := acceptControlTestPeer(t, listener)
	if err := first.writeJSON(map[string]any{"type": "native-host-ready", "url": "https://example.com/launch", "bootstrapScript": "initial-script"}); err != nil {
		t.Fatal(err)
	}
	if request := readControlTestFrame(t, first); request["type"] != "native-host-open-files" {
		t.Fatalf("startup files missing: %v", request)
	}
	first.writeJSON(map[string]any{"type": "native-host-open-files-result"})
	if script := <-started; script != "initial-script" {
		t.Fatalf("bootstrap = %q", script)
	}
	first.conn.Close() // an agent update/restart closes the old control pipe
	waitControlDisconnected(t, h)

	paths := []string{`C:\one\same.mkv`, `C:\two\same.mkv`}
	go h.sendDropFiles("drop-after-restart", paths)
	second := acceptControlTestPeer(t, listener)
	second.writeJSON(map[string]any{"type": "native-host-ready", "url": "https://example.com/restarted", "bootstrapScript": "replacement-script"})
	request := readControlTestFrame(t, second)
	if request["type"] != "native-host-drop-files" || request["id"] != "drop-after-restart" || !reflect.DeepEqual(request["paths"], []any{paths[0], paths[1]}) {
		t.Fatalf("reconnected drop (must not replay startup files): %v", request)
	}
	if h.startURL != "https://example.com/launch" {
		t.Fatalf("reconnect changed launch URL: %s", h.startURL)
	}
	second.writeJSON(map[string]any{"type": "native-host-drop-files-result", "id": request["id"], "paths": paths})
	select {
	case script := <-view.scripts:
		if !strings.Contains(script, `"rid":"drop-after-restart"`) || !strings.Contains(script, `"value":[`) {
			t.Fatalf("drop result not delivered: %s", script)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drop result timed out")
	}

	// Permissions uses the same recovered control connection.
	go h.requestReview("review-after-restart")
	if review := readControlTestFrame(t, second); review["type"] != "native-host-review" || review["id"] != "review-after-restart" {
		t.Fatalf("review did not reuse control: %v", review)
	}
	second.writeJSON(map[string]any{"type": "native-host-review-result", "id": "review-after-restart", "changed": false})
	select {
	case <-view.scripts:
	case <-time.After(5 * time.Second):
		t.Fatal("review result timed out")
	}
}

func TestNativeWindowsControlConcurrentReconnect(t *testing.T) {
	h, listener, _ := controlTestHost(t)
	const count = 12
	results := make(chan *frameConn, count)
	errors := make(chan error, count)
	for range count {
		go func() {
			conn, _, err := h.acquireControl(false)
			results <- conn
			errors <- err
		}()
	}
	peer := acceptControlTestPeer(t, listener)
	peer.writeJSON(map[string]any{"type": "native-host-ready"})
	var shared *frameConn
	for range count {
		conn := <-results
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		if shared != nil && conn != shared {
			t.Fatal("concurrent requests opened separate control connections")
		}
		shared = conn
	}
}

func TestNativeWindowsControlReconnectRejectsRevokedApp(t *testing.T) {
	h, listener, view := controlTestHost(t)
	go h.sendDropFiles("revoked-drop", []string{`C:\video.mkv`})
	peer := acceptControlTestPeer(t, listener)
	peer.writeJSON(map[string]any{"type": "native-host-error", "error": "app is no longer installed"})
	select {
	case script := <-view.scripts:
		if !strings.Contains(script, `"error":"app is no longer installed"`) || !strings.Contains(script, `"rid":"revoked-drop"`) {
			t.Fatalf("agent rejection not delivered: %s", script)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent rejection timed out")
	}
	if _, _, err := peer.read(); err == nil {
		t.Fatal("drop sent despite failed reauthentication")
	}
}

func TestNativeWindowsControlReconnectStopsOnWindowClose(t *testing.T) {
	h, listener, _ := controlTestHost(t)
	finished := make(chan error, 1)
	go func() { _, _, err := h.acquireControl(false); finished <- err }()
	peer := acceptControlTestPeer(t, listener)
	h.closeAll() // close while the reconnect handshake is in flight
	peer.writeJSON(map[string]any{"type": "native-host-ready"})
	if err := <-finished; err == nil {
		t.Fatal("control connection installed after window closed")
	}
	if _, _, err := h.acquireControl(false); err == nil {
		t.Fatal("closed window reconnected")
	}
}
