//go:build windows

package winhost

import (
	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Exercise actual WebView2 child windows and DWM, rather than only a fake
// bridge. The OS caption is retained until the shared UI opts in.
func TestNativeWindowsTitlebarWebView2(t *testing.T) {
	if os.Getenv("DYNAPP_TEST_WEBVIEW2") != "1" {
		t.Skip("set DYNAPP_TEST_WEBVIEW2=1")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	profile, err := os.MkdirTemp("", "dynapp-titlebar-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	view := webview2.NewWithOptions(webview2.WebViewOptions{DataPath: profile, WindowOptions: webview2.WindowOptions{Title: "DynApp titlebar test", Width: 800, Height: 600}})
	if view == nil {
		t.Fatal("WebView2 unavailable")
	}
	defer view.Destroy()
	h := &host{view: view, hwnd: uintptr(view.Window())}
	h.addPermissionsMenu()
	chromium, err := chromiumOf(view)
	if err != nil {
		t.Fatal(err)
	}
	var done bool
	var failure string
	chromium.NavigationCompletedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		view.Dispatch(func() {
			h.setTitlebar(true)
			var rect titlebarRect
			procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&rect)))
			child, _, _ := procGetWindow.Call(h.hwnd, 5)
			if child == 0 {
				failure = "WebView2 has no child HWND"
			} else {
				region, _, _ := procCreateRectRgn.Call(0, 0, 0, 0)
				defer procDeleteObject.Call(region)
				getRegion := user32.NewProc("GetWindowRgn")
				getRegion.Call(child, region)
				contains := gdi.NewProc("PtInRegion")
				dpi := h.titlebarScale()
				inside, _, _ := contains.Call(region, uintptr(rect.Right/2), uintptr(18*dpi/96))
				outside, _, _ := contains.Call(region, uintptr(rect.Right-10), uintptr(18*dpi/96))
				if inside == 0 || outside != 0 {
					failure = "web view does not reserve the native caption buttons"
				}
			}
			h.setTitlebar(false)
			if h.titlebarEnabled {
				failure = "titlebar did not restore"
			}
			done = true
			view.Terminate()
		})
	}
	timer := time.AfterFunc(20*time.Second, func() { view.Dispatch(func() { failure = "navigation timed out"; view.Terminate() }) })
	defer timer.Stop()
	view.SetHtml("<!doctype html><title>Titlebar test</title><p>Native titlebar</p>")
	view.Run()
	if !done || failure != "" {
		t.Fatalf("titlebar: %s (completed %v)", failure, done)
	}
}
