//go:build windows

package winhost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// Unlike the fake COM tests, this checks the interface IDs and vtable slots
// against the installed Microsoft runtime, using actual filesystem File objects.
func TestNativeWindowsDropWebView2(t *testing.T) {
	if os.Getenv("DYNAPP_TEST_WEBVIEW2") != "1" {
		t.Skip("set DYNAPP_TEST_WEBVIEW2=1 with WebView2 installed")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<input id="files" type="file" multiple>`)
	}))
	defer server.Close()
	paths := []string{}
	for _, folder := range []string{"one", "two"} {
		dir := filepath.Join(t.TempDir(), folder)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "same-תיקייה.mkv")
		if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	profile, err := os.MkdirTemp("", "dynapp-webview2-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	view := webview2.NewWithOptions(webview2.WebViewOptions{DataPath: profile})
	if view == nil {
		t.Fatal("WebView2 runtime is unavailable")
	}
	defer view.Destroy()
	h := &host{view: view, origin: server.URL}
	if err := h.hookWebView(false); err != nil {
		t.Fatal(err)
	}
	if err := h.installFileDropReceiver(); err != nil {
		t.Fatal(err)
	}
	chromium, err := chromiumOf(view)
	if err != nil {
		t.Fatal(err)
	}
	chromium.MessageCallback = func(string) {}
	var failure error
	completed := false
	finish := func(err error) { failure = err; completed = true; view.Terminate() }
	h.dropHandler.receive = func(args uintptr) {
		if comString(args, 5) != "file-test" {
			return
		}
		got, err := webViewDropPaths(args)
		if err == nil && !reflect.DeepEqual(got, paths) {
			err = fmt.Errorf("paths %q; want %q", got, paths)
		}
		finish(err)
	}
	var callbacks []*devToolsTestCallback
	var call func(string, any, func(json.RawMessage))
	call = func(method string, params any, next func(json.RawMessage)) {
		data, _ := json.Marshal(params)
		callback := &devToolsTestCallback{vtable: &devToolsTestVTable, done: func(code uintptr, data string) {
			if code != 0 {
				finish(fmt.Errorf("%s: HRESULT %#x: %s", method, code, data))
				return
			}
			next(json.RawMessage(data))
		}}
		callbacks = append(callbacks, callback)
		// ICoreWebView2::CallDevToolsProtocolMethod follows remove_WebMessageReceived.
		code := comCall(uintptr(unsafe.Pointer(h.core)), 36, uintptr(unsafe.Pointer(utf16(method))), uintptr(unsafe.Pointer(utf16(string(data)))), uintptr(unsafe.Pointer(callback)))
		if code != 0 {
			finish(fmt.Errorf("%s: HRESULT %#x", method, code))
		}
	}
	navigating := false
	chromium.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		if navigating || originKey(documentSource(sender)) != server.URL {
			return
		}
		navigating = true
		call("Runtime.evaluate", map[string]any{"expression": `document.getElementById("files")`, "objectGroup": "dynapp-file-test"}, func(data json.RawMessage) {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(data, &object); err != nil {
				finish(err)
				return
			}
			var objectID string
			for depth := 0; depth < 4; depth++ {
				_ = json.Unmarshal(object["objectId"], &objectID)
				if objectID != "" {
					break
				}
				if err := json.Unmarshal(object["result"], &object); err != nil {
					break
				}
			}
			if objectID == "" {
				finish(fmt.Errorf("file input was not found: %s", data))
				return
			}
			call("DOM.setFileInputFiles", map[string]any{"objectId": objectID, "files": paths}, func(json.RawMessage) {
				view.Eval(`chrome.webview.postMessageWithAdditionalObjects("file-test",document.getElementById("files").files)`)
			})
		})
	}
	timer := time.AfterFunc(30*time.Second, func() { view.Dispatch(func() { finish(fmt.Errorf("WebView2 file test timed out")) }) })
	defer timer.Stop()
	view.Navigate(server.URL)
	view.Run()
	runtime.KeepAlive(callbacks)
	if !completed || failure != nil {
		t.Fatalf("native file drop: completed=%v: %v", completed, failure)
	}
}

type devToolsTestCallback struct {
	vtable *[4]uintptr
	done   func(uintptr, string)
}

var devToolsTestVTable = [4]uintptr{
	syscall.NewCallback(func(this, iid, result uintptr) uintptr {
		// The test callback only exposes IUnknown and the SDK completion interface.
		guid := *(*windows.GUID)(unsafe.Pointer(iid))
		unknown := windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
		completion := windows.GUID{Data1: 0x5c4889f0, Data2: 0x5ef6, Data3: 0x4c5a, Data4: [8]byte{0x95, 0x2c, 0xd8, 0xf1, 0xb9, 0x2d, 0x05, 0x74}}
		if guid != unknown && guid != completion {
			return 0x80004002
		}
		*(*uintptr)(unsafe.Pointer(result)) = this
		return 0
	}),
	syscall.NewCallback(func(uintptr) uintptr { return 1 }),
	syscall.NewCallback(func(uintptr) uintptr { return 1 }),
	syscall.NewCallback(func(this, code, data uintptr) uintptr {
		(*devToolsTestCallback)(unsafe.Pointer(this)).done(code, windows.UTF16PtrToString((*uint16)(unsafe.Pointer(data))))
		return 0
	}),
}
