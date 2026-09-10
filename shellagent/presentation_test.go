package shellagent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

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
