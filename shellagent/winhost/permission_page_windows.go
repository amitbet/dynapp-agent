//go:build windows

package winhost

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// The agent's permission page (shellagent/native_permission_prompt.html) is
// shown in a second WebView2 inside a window this host owns, modal to the app
// window. It is the same page the macOS host shows. The app's page has no
// path to this web view or its message channel, and the page never navigates.

var (
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procIsWindow        = user32.NewProc("IsWindow")
)

const (
	permissionPageClass = "DynAppPermissionPrompt"
	permissionPageWidth = 500 // CSS pixels, as on macOS
	wmClose             = 0x0010
	wmSize              = 0x0005
	wmActivate          = 0x0006
	wmMove              = 0x0003
	wsPopup             = 0x80000000
	wsCaption           = 0x00C00000
	wsSysMenu           = 0x00080000
	wsExDlgModalFrame   = 0x00000001
	wsExControlParent   = 0x00010000
)

type permissionPage struct {
	hwnd     uintptr
	owner    uintptr
	chromium *edge.Chromium
	done     chan permissionPageResult
	once     sync.Once
}

// permissionPageResult is one answer: decision is "allow", "deny" or
// "dismissed" (window closed, Escape). Only allow carries capabilities.
type permissionPageResult struct {
	capabilities []string
	decision     string
	dontAsk      bool
}

var (
	permissionPageClassOnce sync.Once
	permissionPageClassErr  error
	permissionPages         sync.Map // hwnd -> *permissionPage
	permissionPageProc      = windows.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		if value, ok := permissionPages.Load(hwnd); ok {
			page := value.(*permissionPage)
			switch message {
			case wmClose:
				page.finish(permissionPageResult{decision: "dismissed"})
				return 0
			case wmSize:
				if page.chromium != nil {
					page.chromium.Resize()
				}
			case wmMove:
				if page.chromium != nil {
					_ = page.chromium.NotifyParentWindowPositionChanged()
				}
			case wmActivate:
				if wparam&0xffff != 0 && page.chromium != nil {
					page.chromium.Focus()
				}
			}
		}
		result, _, _ := procDefWindowProc.Call(hwnd, message, wparam, lparam)
		return result
	})
)

func registerPermissionPageClass() error {
	permissionPageClassOnce.Do(func() {
		var instance windows.Handle
		_ = windows.GetModuleHandleEx(0, nil, &instance)
		class := struct {
			Size                 uint32
			Style                uint32
			WndProc              uintptr
			ClassExtra, WndExtra int32
			Instance             windows.Handle
			Icon, Cursor         windows.Handle
			Background           windows.Handle
			MenuName, ClassName  *uint16
			SmallIcon            windows.Handle
		}{WndProc: permissionPageProc, Instance: instance, ClassName: utf16(permissionPageClass), Background: 6 /* COLOR_WINDOW+1 */}
		class.Size = uint32(unsafe.Sizeof(class))
		if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
			permissionPageClassErr = err
		}
	})
	return permissionPageClassErr
}

// askPermissionPage shows page modal to the app window and returns the
// user's answer. It runs off the UI thread; window work is dispatched to it.
// An error means the page could not be shown and the caller should fall back
// to the native checklist.
func (h *host) askPermissionPage(page string) (permissionPageResult, error) {
	if h.view == nil || h.hwnd == 0 {
		return permissionPageResult{}, errors.New("the app window is not ready")
	}
	prompt := &permissionPage{owner: h.hwnd, done: make(chan permissionPageResult, 1)}
	failed := make(chan error, 1)
	h.view.Dispatch(func() {
		if err := prompt.open(h, page); err != nil {
			failed <- err
			return
		}
		failed <- nil
		// Embed pumps messages itself and can swallow the library's dispatch
		// wake-up; queue another so nothing waits for the next one.
		h.view.Dispatch(func() {})
	})
	if err := <-failed; err != nil {
		return permissionPageResult{}, err
	}
	return <-prompt.done, nil
}

func (p *permissionPage) open(h *host, page string) error {
	if err := registerPermissionPageClass(); err != nil {
		return err
	}
	dpi, _, _ := procGetDpiForWindow.Call(p.owner)
	if dpi == 0 {
		dpi = 96
	}
	scale := func(css int) int { return css * int(dpi) / 96 }
	width, height := scale(permissionPageWidth), scale(440)
	x, y := p.centered(width, height)
	title := h.options.Name + " permissions"
	hwnd, _, err := procCreateWindowEx.Call(
		wsExDlgModalFrame|wsExControlParent,
		uintptr(unsafe.Pointer(utf16(permissionPageClass))),
		uintptr(unsafe.Pointer(utf16(title))),
		wsPopup|wsCaption|wsSysMenu,
		uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		p.owner, 0, 0, 0,
	)
	if hwnd == 0 {
		return err
	}
	p.hwnd = hwnd
	permissionPages.Store(hwnd, p)
	chromium := edge.NewChromium()
	chromium.DataPath = h.webViewDataPath()
	chromium.MessageCallback = p.message
	if !chromium.Embed(hwnd) {
		permissionPages.Delete(hwnd)
		_, _, _ = procDestroyWindow.Call(hwnd)
		return errors.New("could not create the permission web view")
	}
	p.chromium = chromium
	if settings, err := chromium.GetSettings(); err == nil {
		_ = settings.PutAreDefaultContextMenusEnabled(false)
		_ = settings.PutAreDevToolsEnabled(false)
		_ = settings.PutIsStatusBarEnabled(false)
	}
	chromium.Resize()
	chromium.NavigateToString(page)
	// Modal: the app window stays disabled until the user answers.
	_, _, _ = procEnableWindow.Call(p.owner, 0)
	_, _, _ = procShowWindow.Call(hwnd, swShowNormal)
	_, _, _ = procSetForeground.Call(hwnd)
	chromium.Focus()
	return nil
}

func (p *permissionPage) centered(width, height int) (int, int) {
	var owner struct{ Left, Top, Right, Bottom int32 }
	if ok, _, _ := procGetWindowRect.Call(p.owner, uintptr(unsafe.Pointer(&owner))); ok == 0 {
		return 100, 100
	}
	x := int(owner.Left) + (int(owner.Right-owner.Left)-width)/2
	y := int(owner.Top) + (int(owner.Bottom-owner.Top)-height)/3
	if y < int(owner.Top) {
		y = int(owner.Top)
	}
	return x, y
}

// message handles posts from the page: its content height, then the answer.
func (p *permissionPage) message(raw string) {
	var message struct {
		DynappPermission int      `json:"dynappPermission"`
		Resize           float64  `json:"resize"`
		Decision         string   `json:"decision"`
		Capabilities     []string `json:"capabilities"`
		DontAsk          bool     `json:"dontAsk"`
	}
	if json.Unmarshal([]byte(raw), &message) != nil || message.DynappPermission != 1 {
		return
	}
	switch message.Decision {
	case "allow":
		if message.Capabilities == nil {
			message.Capabilities = []string{}
		}
		p.finish(permissionPageResult{decision: "allow", capabilities: message.Capabilities})
		return
	case "deny", "dismissed":
		p.finish(permissionPageResult{decision: message.Decision, dontAsk: message.DontAsk && message.Decision == "deny"})
		return
	}
	if message.Resize > 0 {
		p.fitHeight(message.Resize)
	}
}

func (p *permissionPage) fitHeight(css float64) {
	dpi, _, _ := procGetDpiForWindow.Call(p.hwnd)
	if dpi == 0 {
		dpi = 96
	}
	if css < 220 {
		css = 220
	}
	if css > 680 {
		css = 680
	}
	var window, client struct{ Left, Top, Right, Bottom int32 }
	_, _, _ = procGetWindowRect.Call(p.hwnd, uintptr(unsafe.Pointer(&window)))
	_, _, _ = procGetClientRect.Call(p.hwnd, uintptr(unsafe.Pointer(&client)))
	frame := int(window.Bottom-window.Top) - int(client.Bottom-client.Top)
	height := int(css*float64(dpi)/96) + frame
	width := int(window.Right - window.Left)
	x, y := p.centered(width, height)
	_, _, _ = procSetWindowPos.Call(p.hwnd, 0, uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0x0004 /* SWP_NOZORDER */)
}

// finish runs on the UI thread (window procedure or page message).
func (p *permissionPage) finish(result permissionPageResult) {
	p.once.Do(func() {
		// Re-enable the owner first so Windows activates it, not another app.
		_, _, _ = procEnableWindow.Call(p.owner, 1)
		if p.hwnd != 0 {
			permissionPages.Delete(p.hwnd)
			if alive, _, _ := procIsWindow.Call(p.hwnd); alive != 0 {
				_, _, _ = procDestroyWindow.Call(p.hwnd)
			}
		}
		_, _, _ = procSetForeground.Call(p.owner)
		p.done <- result
	})
}

// webViewDataPath is the app's WebView2 user data folder. The prompt shares
// it so both web views run in one browser process; the page's opaque origin
// gives it no access to the app's storage.
func webViewDataPath(options Options) string {
	return filepath.Join(filepath.Dir(options.Icon), "WebView2")
}

func (h *host) webViewDataPath() string {
	return webViewDataPath(h.options)
}
