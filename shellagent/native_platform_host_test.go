package shellagent

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"testing"
	"time"
)

// platformTestAgent enables the platform channel on any OS and trusts the
// pipe peer, as the Android uid check would.
func platformTestAgent(t *testing.T) *Server {
	t.Helper()
	agent, _ := nativeTestAgent(t, notesNativeApp())
	previousEnabled, previousPeer := nativePlatformChannelEnabled, verifyNativePlatformPeer
	nativePlatformChannelEnabled = func() bool { return true }
	verifyNativePlatformPeer = func(net.Conn) error { return nil }
	t.Cleanup(func() {
		nativePlatformChannelEnabled, verifyNativePlatformPeer = previousEnabled, previousPeer
		activeNativePlatform.Store(nil)
	})
	return agent
}

func TestNativePlatformChannelRegistersTheCatalogWithAuthority(t *testing.T) {
	agent := platformTestAgent(t)
	host := dialNative(t, agent, "", "platform")
	ready := readNative(t, host)
	if ready["type"] != "native-platform-ready" {
		t.Fatalf("ready = %v", ready)
	}
	catalog, _ := ready["catalog"].(map[string]any)
	storeID := agent.Config.catalogAppID()
	if catalog["storeId"] != storeID || catalog["origin"] == "" {
		t.Fatalf("catalog = %v", catalog)
	}
	app, ok := agent.nativeApp(storeID)
	if !ok || !app.Authority || app.Path != nativeCatalogPath {
		t.Fatalf("catalog record = %+v %v", app, ok)
	}
	if !slices.Contains(app.Capabilities, permissionsManage) {
		t.Fatalf("catalog capabilities = %v", app.Capabilities)
	}
}

func TestNativePlatformCallsRoundTripThroughTheHost(t *testing.T) {
	agent := platformTestAgent(t)
	host := dialNative(t, agent, "", "platform")
	readNative(t, host)
	type outcome struct {
		result json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := callNativePlatform(context.Background(), "launch", map[string]any{"storeId": "amit-bet/notes"})
		done <- outcome{result, err}
	}()
	request := readNative(t, host)
	if request["type"] != "native-platform-request" || request["op"] != "launch" {
		t.Fatalf("request = %v", request)
	}
	writeNative(t, host, map[string]any{"type": "native-platform-result", "id": request["id"], "result": map[string]any{"launched": true}})
	got := <-done
	if got.err != nil || string(got.result) != `{"launched":true}` {
		t.Fatalf("call = %s %v", got.result, got.err)
	}

	go func() {
		_, err := callNativePlatform(context.Background(), "install", nil)
		done <- outcome{nil, err}
	}()
	request = readNative(t, host)
	writeNative(t, host, map[string]any{"type": "native-platform-result", "id": request["id"], "error": "Pinning was cancelled"})
	if got := <-done; got.err == nil || got.err.Error() != "Pinning was cancelled" {
		t.Fatalf("error call = %v", got.err)
	}
}

func TestNativePlatformCallFailsWhenTheHostLeaves(t *testing.T) {
	agent := platformTestAgent(t)
	host := dialNative(t, agent, "", "platform")
	readNative(t, host)
	done := make(chan error, 1)
	go func() {
		_, err := callNativePlatform(context.Background(), "launch", nil)
		done <- err
	}()
	readNative(t, host)
	_ = host.conn.Close()
	select {
	case err := <-done:
		if err != errNativePlatformUnavailable {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not end when the host left")
	}
	if _, err := callNativePlatform(context.Background(), "launch", nil); err != errNativePlatformUnavailable {
		t.Fatalf("call without host = %v", err)
	}
}

func TestNativePlatformPurposeIsRejectedOnDesktop(t *testing.T) {
	agent, _ := nativeTestAgent(t, notesNativeApp())
	host := dialNative(t, agent, "", "platform")
	if frame := readNative(t, host); frame["type"] != "native-host-error" {
		t.Fatalf("frame = %v", frame)
	}
}
