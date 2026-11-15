//go:build windows

package winhost

// Native app automation on Windows (docs/native-automation.md in the
// dynapp-agent repository). The agent forwards commands over the control
// channel only after the user allowed automation for this app.
//
// Scripts run through ExecuteScript and report back with postMessage. Real
// input events and screenshots are not implemented on Windows yet.

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
	"unsafe"
)

var (
	procDeleteMenu   = user32.NewProc("DeleteMenu")
	procIsIconic     = user32.NewProc("IsIconic")
	procIsWindowVis  = user32.NewProc("IsWindowVisible")
	procGetDpiForWnd = user32.NewProc("GetDpiForWindow")
)

const (
	stopAutomationCommand = uintptr(0x0020)
	mbYesNo               = 0x00000004
	mbDefButton2          = 0x00000100
	idYes                 = 6
	mfByCommand           = 0x00000000
	swpNoZOrder           = 0x0004
	swpNoActivate         = 0x0010
)

type rect struct{ Left, Top, Right, Bottom int32 }

func (h *host) installAutomationFlag() {
	h.mu.Lock()
	installed := h.automationInit
	h.automationInit = true
	h.mu.Unlock()
	if !installed {
		h.view.Init(`Object.defineProperty(window, "__DYNAPP_NATIVE_AUTOMATION__", {value: true});`)
	}
}

func (h *host) setAutomationMenu(enabled bool) {
	menu, _, _ := procGetSystemMenu.Call(h.hwnd, 0)
	if menu == 0 {
		return
	}
	h.mu.Lock()
	present := h.automationMenu
	h.automationMenu = enabled
	h.mu.Unlock()
	if enabled && !present {
		_, _, _ = procAppendMenu.Call(menu, mfString, stopAutomationCommand, uintptr(unsafe.Pointer(utf16("Stop Automation"))))
	} else if !enabled && present {
		_, _, _ = procDeleteMenu.Call(menu, stopAutomationCommand, mfByCommand)
	}
}

func (h *host) applyAutomation(enabled bool) {
	h.mu.Lock()
	h.automation = enabled
	bridge := h.bridge
	h.mu.Unlock()
	if !bridge || h.view == nil {
		return
	}
	h.view.Dispatch(func() {
		// The flag script stays for later loads; the bridge also follows the
		// state the host delivers after every navigation.
		if enabled {
			h.installAutomationFlag()
		}
		h.setAutomationMenu(enabled)
	})
	h.deliver(map[string]any{"event": "automation", "enabled": enabled})
}

func (h *host) automationEnabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.automation
}

func (h *host) stopAutomation() {
	if control := h.currentControl(); control != nil {
		_ = control.writeJSON(map[string]any{"type": "native-host-automation-stop"})
	}
	h.applyAutomation(false)
}

// reconnectControl restores the control channel after an agent restart so
// automation and reviews keep working without reopening the app.
func (h *host) reconnectControl() {
	for attempt := 0; ; attempt++ {
		delay := time.Duration(500*(1<<min(attempt, 4))) * time.Millisecond
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		time.Sleep(delay)
		h.mu.Lock()
		closed, control := h.closed, h.control
		h.mu.Unlock()
		if closed || control != nil {
			return
		}
		if _, _, err := h.acquireControl(false); err == nil {
			log.Printf("control: reconnected")
			return
		}
	}
}

func (h *host) automationNavigated(source string) {
	if !h.automationEnabled() {
		return
	}
	h.deliver(map[string]any{"event": "automation", "enabled": true})
	if control := h.currentControl(); control != nil {
		_ = control.writeJSON(map[string]any{"type": "native-host-console", "entries": []any{map[string]any{
			"level": "info", "source": "navigation", "text": "Loaded " + source, "time": time.Now().UnixMilli(),
		}}})
	}
}

func (h *host) forwardConsole(entries []json.RawMessage) {
	if len(entries) == 0 || !h.automationEnabled() {
		return
	}
	if control := h.currentControl(); control != nil {
		_ = control.writeJSON(map[string]any{"type": "native-host-console", "entries": entries})
	}
}

func (h *host) finishEval(id, result string) {
	h.mu.Lock()
	known := h.evals[id]
	delete(h.evals, id)
	h.mu.Unlock()
	if !known {
		return
	}
	if control := h.currentControl(); control != nil {
		_ = control.writeJSON(map[string]any{"type": "native-host-automation-result", "id": id, "value": result})
	}
}

func (h *host) handleAutomation(frame map[string]any, conn *frameConn) {
	id, _ := frame["id"].(string)
	op, _ := frame["op"].(string)
	reply := func(value any, err error) {
		result := map[string]any{"type": "native-host-automation-result", "id": id}
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["value"] = value
		}
		_ = conn.writeJSON(result)
	}
	if op == "consent" {
		appName, _ := frame["appName"].(string)
		if appName == "" {
			appName = h.options.Name
		}
		bringToFront(h.hwnd, swRestore)
		text := fmt.Sprintf("A coding agent on this PC, such as Codex or Claude Code, asked to control %s: run scripts in it and read what it shows. It can use everything %s is allowed to do.\n\nAllow this only if you started that agent. You can turn it off from the window menu (Alt+Space) > Stop Automation.\n\nAllow coding agents to control %s?", appName, appName, appName)
		approved := messageBox(h.hwnd, text, appName, mbYesNo|mbIconWarning|mbDefButton2) == idYes
		reply(map[string]any{"approved": approved}, nil)
		return
	}
	if !h.automationEnabled() {
		reply(nil, fmt.Errorf("automation is off for this app"))
		return
	}
	h.mu.Lock()
	bridge := h.bridge
	h.mu.Unlock()
	if !bridge {
		reply(nil, fmt.Errorf("the app is running without the agent bridge"))
		return
	}
	switch op {
	case "eval":
		script, _ := frame["script"].(string)
		fallback, _ := frame["fallback"].(string)
		h.mu.Lock()
		h.evals[id] = true
		h.mu.Unlock()
		quoted, _ := json.Marshal(id)
		post := `.then((json) => window.chrome.webview.postMessage(JSON.stringify({ dynappNativeHost: 1, op: "automationResult", id: ` + string(quoted) + `, json })))`
		first := "(async () => {\n" + script + "\n})()" + post
		h.view.Dispatch(func() {
			h.view.Eval(first)
			// A script that does not parse never runs, so it never marks
			// itself as started; the statement form runs instead.
			if fallback != "" {
				h.view.Eval(`setTimeout(() => { if (!(globalThis[Symbol.for("dynapp.automation")]?.has(` + string(quoted) + `))) { (async () => {` + "\n" + fallback + "\n" + `})()` + post + ` } }, 0)`)
			}
		})
		// The agent times the call out; drop the id after that.
		go func() {
			time.Sleep(11 * time.Minute)
			h.mu.Lock()
			delete(h.evals, id)
			h.mu.Unlock()
		}()
	case "reload":
		h.view.Dispatch(func() { h.view.Eval("location.reload()") })
		reply(map[string]any{"reloaded": true}, nil)
	case "navigate":
		target, _ := frame["url"].(string)
		if originKey(target) != h.origin {
			reply(nil, fmt.Errorf("automation can only navigate inside the app"))
			return
		}
		h.view.Dispatch(func() { h.view.Navigate(target) })
		reply(map[string]any{"url": target}, nil)
	case "window":
		// Window and WebView2 calls belong on the UI thread.
		h.view.Dispatch(func() { h.automationWindow(frame, reply) })
	case "snapshot", "click", "type", "key":
		reply(nil, fmt.Errorf("%s is not available on Windows yet; use eval (for example element.click()) instead", op))
	default:
		reply(nil, fmt.Errorf("this app host does not support %s", op))
	}
}

func (h *host) automationWindow(frame map[string]any, reply func(any, error)) {
	var window, client rect
	_, _, _ = procGetWindowRect.Call(h.hwnd, uintptr(unsafe.Pointer(&window)))
	_, _, _ = procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&client)))
	dpi, _, _ := procGetDpiForWnd.Call(h.hwnd)
	scale := 1.0
	if dpi != 0 {
		scale = float64(dpi) / 96
	}
	number := func(key string) (float64, bool) {
		value, ok := frame[key].(float64)
		return value, ok
	}
	width, hasWidth := number("width")
	height, hasHeight := number("height")
	x, hasX := number("x")
	y, hasY := number("y")
	if hasWidth || hasHeight || hasX || hasY {
		// Width and height are the viewport in CSS pixels; add the frame.
		frameWidth := (window.Right - window.Left) - (client.Right - client.Left)
		frameHeight := (window.Bottom - window.Top) - (client.Bottom - client.Top)
		newWidth, newHeight := window.Right-window.Left, window.Bottom-window.Top
		if hasWidth {
			newWidth = int32(width*scale) + frameWidth
		}
		if hasHeight {
			newHeight = int32(height*scale) + frameHeight
		}
		left, top := window.Left, window.Top
		if hasX {
			left = int32(x * scale)
		}
		if hasY {
			top = int32(y * scale)
		}
		_, _, _ = procSetWindowPos.Call(h.hwnd, 0, uintptr(left), uintptr(top), uintptr(newWidth), uintptr(newHeight), swpNoZOrder|swpNoActivate)
		_, _, _ = procGetWindowRect.Call(h.hwnd, uintptr(unsafe.Pointer(&window)))
		_, _, _ = procGetClientRect.Call(h.hwnd, uintptr(unsafe.Pointer(&client)))
	}
	if focus, _ := frame["focus"].(bool); focus {
		bringToFront(h.hwnd, swRestore)
	}
	iconic, _, _ := procIsIconic.Call(h.hwnd)
	visible, _, _ := procIsWindowVis.Call(h.hwnd)
	foreground, _, _ := procGetForegroundWnd.Call()
	reply(map[string]any{
		"x": float64(window.Left) / scale, "y": float64(window.Top) / scale,
		"width": float64(window.Right-window.Left) / scale, "height": float64(window.Bottom-window.Top) / scale,
		"viewportWidth": float64(client.Right-client.Left) / scale, "viewportHeight": float64(client.Bottom-client.Top) / scale,
		"scale": scale, "key": foreground == h.hwnd, "visible": visible != 0, "minimized": iconic != 0,
		"url": documentSource(h.core),
	}, nil)
}
