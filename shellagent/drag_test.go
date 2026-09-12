package shellagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDragUsesItsOwnCapability(t *testing.T) {
	if got := rpcCapability(message{Service: "drag", Method: "startPromises"}); got != "drag.files" {
		t.Fatalf("drag capability = %q", got)
	}
	if runtime.GOOS == "darwin" && !stringSliceHas(capabilities(), "drag.files") {
		t.Fatal("macOS agent did not advertise drag.files")
	}
}

func TestDragPromiseStreamsThroughSharedFileWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drag.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{promises: map[string]*clipboardPromise{
		"drag-1": {id: "drag-1", appID: "source", service: "drag", sourceID: "remote", name: "drag.txt", size: 5, path: path, file: file},
	}}
	request := message{AppID: "source", Service: "drag", Method: "writePromiseBinary", Args: []any{"drag-1", float64(0)}}
	if _, err = server.handleDragRPC(nil, request, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	request.Method = "finishPromise"
	request.Args = []any{"drag-1"}
	if _, err = server.handleDragRPC(nil, request, nil); err != nil {
		t.Fatal(err)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "hello" {
		t.Fatalf("drag output = %q, %v", data, readErr)
	}
}

func TestCancelledDragRemovesOnlyUnrequestedDragPromises(t *testing.T) {
	active, err := os.Create(filepath.Join(t.TempDir(), "active.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	server := &Server{promises: map[string]*clipboardPromise{
		"pending":   {id: "pending", appID: "app", service: "drag"},
		"active":    {id: "active", appID: "app", service: "drag", file: active},
		"clipboard": {id: "clipboard", appID: "app", service: "clipboard"},
	}}
	server.handleFilePromiseDragEnd([]string{"pending", "active", "clipboard"}, "cancelled")
	if server.promises["pending"] != nil || server.promises["active"] == nil || server.promises["clipboard"] == nil {
		t.Fatalf("unexpected promises after cancellation: %#v", server.promises)
	}
}
