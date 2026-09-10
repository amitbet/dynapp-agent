package shellagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

type fakePresentation struct {
	registered map[uint32]presentationKey
	tray       *trayOptions
	reject     bool
}

func (f *fakePresentation) setTray(options trayOptions) error { f.tray = &options; return nil }
func (f *fakePresentation) destroyTray()                      { f.tray = nil }
func (f *fakePresentation) balloon(string, string) bool       { return f.tray != nil }
func (f *fakePresentation) register(id uint32, key presentationKey) bool {
	if f.reject {
		return false
	}
	if f.registered == nil {
		f.registered = map[uint32]presentationKey{}
	}
	f.registered[id] = key
	return true
}
func (f *fakePresentation) unregister(id uint32) { delete(f.registered, id) }

func TestPresentationShortcutParsing(t *testing.T) {
	for _, entry := range []struct {
		input, platform string
		key             presentationKey
	}{
		{"CommandOrControl+Shift+A", "darwin", presentationKey{65, 12}},
		{"CmdOrCtrl+Alt+F19", "windows", presentationKey{130, 3}},
		{"Control+Option+Space", "darwin", presentationKey{32, 3}},
		{"Super+PageDown", "windows", presentationKey{34, 8}},
	} {
		got, err := parsePresentationKey(entry.input, entry.platform)
		if err != nil || got != entry.key {
			t.Errorf("%s = %+v, %v", entry.input, got, err)
		}
	}
	for _, input := range []string{"A", "Ctrl+", "Unknown+A", "Ctrl+A+B", "Ctrl+F21", "Ctrl+MediaPlayPause"} {
		if _, err := parsePresentationKey(input, "windows"); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
func TestPresentationShortcutOwnershipAndFailure(t *testing.T) {
	native := &fakePresentation{}
	p := &presentationController{native: native, platform: "darwin"}
	call := func(method, key string) any {
		t.Helper()
		result, err := p.handle(presentationCommand{Service: "globalShortcut", Method: method, Args: []any{key}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if call("register", "Cmd+Shift+A") != true {
		t.Fatal("registration failed")
	}
	if call("register", "Command+Shift+A") != false {
		t.Fatal("alias double registration succeeded")
	}
	call("unregister", "Cmd+Shift+B")
	if len(native.registered) != 1 {
		t.Fatal("unregister removed another shortcut")
	}
	if p.hotkey(1) == nil || p.hotkey(2) != nil {
		t.Fatal("event ownership mismatch")
	}
	call("unregister", "Command+Shift+A")
	if len(native.registered) != 0 {
		t.Fatal("alias unregister failed")
	}
	native.reject = true
	if call("register", "Ctrl+B") != false || len(p.shortcuts) != 0 {
		t.Fatal("failed native registration retained state")
	}
	native.reject = false
	call("register", "Ctrl+C")
	p.close()
	if len(native.registered) != 0 {
		t.Fatal("close leaked shortcut")
	}
}
func TestPresentationTrayValidationAndUpdates(t *testing.T) {
	native := &fakePresentation{}
	p := &presentationController{native: native, platform: "windows"}
	call := func(method string, value any) error {
		_, err := p.handle(presentationCommand{Service: "tray", Method: method, Args: []any{value}})
		return err
	}
	if err := call("create", map[string]any{"tooltip": "Original", "menu": []any{map[string]any{"id": "toggle", "label": "Toggle", "type": "checkbox"}}}); err != nil {
		t.Fatal(err)
	}
	if err := call("setMenu", []any{map[string]any{"id": "same"}, map[string]any{"id": "same"}}); err == nil {
		t.Fatal("duplicate ids accepted")
	}
	if native.tray.Menu[0].ID != "toggle" {
		t.Fatal("invalid update changed native menu")
	}
	if err := call("setTitle", "Title"); err != nil {
		t.Fatal(err)
	}
	if native.tray.Tooltip != "Original" || len(native.tray.Menu) != 1 {
		t.Fatal("partial update discarded tray state")
	}
	event := p.menuClick("toggle")
	if !reflect.DeepEqual(event, map[string]any{"id": "toggle", "checked": true}) {
		t.Fatalf("event=%v", event)
	}
	if p.menuClick("other") != nil {
		t.Fatal("unknown menu event delivered")
	}
	if err := validateTray(trayOptions{Menu: make([]trayItem, 129)}); err == nil {
		t.Fatal("oversized menu accepted")
	}
	if err := call("create", map[string]any{"tooltip": strings.Repeat("x", 1025)}); err == nil {
		t.Fatal("oversized text accepted")
	}
	p.close()
	if native.tray != nil {
		t.Fatal("close leaked tray")
	}
}
func TestPresentationBridgeCorrelatesRepliesAndCloses(t *testing.T) {
	for _, id := range []string{"request", "wrong"} {
		t.Run(id, func(t *testing.T) {
			client, helper := net.Pipe()
			defer helper.Close()
			stopped := make(chan struct{})
			bridge := &presentationBridge{conn: client, replies: make(chan presentationReply, 1), done: make(chan struct{}), stop: func() { close(stopped) }}
			go func() {
				var command presentationCommand
				_ = json.NewDecoder(helper).Decode(&command)
				bridge.replies <- presentationReply{ID: id, Result: true}
			}()
			_, err := bridge.call(context.Background(), message{ID: "request", Service: "tray", Method: "destroy"})
			if (err != nil) != (id == "wrong") {
				t.Fatalf("reply id %s: %v", id, err)
			}
			bridge.close()
			bridge.close()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("helper not stopped")
			}
		})
	}
}
func TestPresentationPermissionsAreRequiredBeforeHelperLaunch(t *testing.T) {
	server := httptest.NewServer(newTestServer(t).Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, _ := dialTestAgent(t, ctx, server, testDialOptions{Declared: []string{"fs.readText"}})
	defer connection.Close(websocket.StatusNormalClosure, "")
	for _, entry := range []struct{ service, permission string }{{"tray", "tray.manage"}, {"globalShortcut", "globalShortcut"}, {"screen", "screen.capture"}} {
		sendRequest(t, ctx, connection, map[string]any{"type": "rpc", "id": entry.service, "service": entry.service, "method": "destroy"})
		reply := receive(t, ctx, connection)
		if reply["type"] != "rpc-error" || !strings.Contains(reply["error"].(string), entry.permission) {
			t.Fatalf("permission bypass: %#v", reply)
		}
		if rpcCapability(message{Service: entry.service}) != entry.permission {
			t.Fatal("wrong capability")
		}
	}
}

func TestPresentationPrivateChannelAuthenticationEventsAndShutdown(t *testing.T) {
	server := &Server{}
	socket := &recordingProtocolSocket{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	helperFinished := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	launch := func(exe string, args []string, token string) (func(), error) {
		if len(args) != 2 || strings.Contains(strings.Join(args, " "), token) {
			t.Fatal("helper credential leaked into argv")
		}
		go func() {
			defer close(helperFinished)
			bad, err := net.Dial("tcp", args[1])
			if err != nil {
				return
			}
			fmt.Fprintln(bad, "wrong credential")
			bad.Close()
			connection, err := net.Dial("tcp", args[1])
			if err != nil {
				return
			}
			defer connection.Close()
			fmt.Fprintln(connection, token)
			scanner := bufio.NewScanner(connection)
			for scanner.Scan() {
				var command presentationCommand
				if json.Unmarshal(scanner.Bytes(), &command) != nil {
					return
				}
				encoder := json.NewEncoder(connection)
				_ = encoder.Encode(presentationReply{Service: "tray", Event: map[string]any{"id": "open", "checked": false}})
				_ = encoder.Encode(presentationReply{ID: command.ID, Result: true})
			}
		}()
		return func() { once.Do(func() { close(stopped) }) }, nil
	}
	bridge, err := server.connectPresentation(socket, ctx, launch)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	result, err := bridge.call(ctx, message{ID: "one", Service: "tray", Method: "create"})
	if err != nil || result != true {
		t.Fatalf("RPC: %v %v", result, err)
	}
	events := socket.events()
	if len(events) != 1 || events[0]["type"] != "rpc-event" || events[0]["service"] != "tray" {
		t.Fatalf("events=%v", events)
	}
	server.closePresentations()
	select {
	case <-helperFinished:
	case <-ctx.Done():
		t.Fatal("shutdown did not close helper channel")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("shutdown did not reap helper")
	}
}
