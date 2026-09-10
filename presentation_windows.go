//go:build windows

package shellagent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var presentationUser32 = windows.NewLazySystemDLL("user32.dll")
var presentationShell32 = windows.NewLazySystemDLL("shell32.dll")
var presentationProcedures = map[string]*windows.LazyProc{}

func uiCall(name string, args ...uintptr) uintptr {
	// All GUI calls run on the helper's single UI thread.
	proc := presentationProcedures[name]
	if proc == nil {
		proc = presentationUser32.NewProc(name)
		presentationProcedures[name] = proc
	}
	result, _, _ := proc.Call(args...)
	return result
}
func uiString(value string) *uint16 {
	value16, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return windows.StringToUTF16Ptr("")
	}
	return value16
}
func presentationSupported() bool { return true }
func launchPresentation(exe string, args []string, token string) (func(), error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return nil, err
	}
	if session == 0 {
		return launchUserSessionProcess(exe, args, "", []string{"DYNAPP_PRESENTATION_TOKEN=" + token})
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "DYNAPP_PRESENTATION_TOKEN="+token)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait()
	return func() { _ = cmd.Process.Kill() }, nil
}

type presentationPoint struct{ X, Y int32 }
type presentationMSG struct {
	Hwnd           uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          presentationPoint
	Private        uint32
}
type presentationWindowClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	SmallIcon                          uintptr
}
type presentationNotifyIcon struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                windows.GUID
	BalloonIcon         uintptr
}
type windowsPresentation struct {
	window         uintptr
	icon           presentationNotifyIcon
	menu           []trayItem
	visible        bool
	controller     *presentationController
	emit           func(presentationReply)
	restartMessage uint32
}

func (w *windowsPresentation) notify(operation uintptr) bool {
	w.icon.Size = uint32(unsafe.Sizeof(w.icon))
	result, _, _ := presentationShell32.NewProc("Shell_NotifyIconW").Call(operation, uintptr(unsafe.Pointer(&w.icon)))
	return result != 0
}
func copyUIString(destination []uint16, value string) {
	clear(destination)
	encoded, _ := windows.UTF16FromString(value)
	if len(encoded) > len(destination) {
		encoded = encoded[:len(destination)-1]
	}
	copy(destination, encoded)
}
func (w *windowsPresentation) setTray(options trayOptions) error {
	w.icon.Window = w.window
	w.icon.ID = 1
	w.icon.Flags = 1 | 2 | 4
	w.icon.Callback = 0x8001
	w.icon.Icon = uiCall("LoadIconW", 0, 32512)
	copyUIString(w.icon.Tip[:], options.Tooltip)
	operation := uintptr(0)
	if w.visible {
		operation = 1
	}
	if !w.notify(operation) {
		return errors.New("Windows could not create or update the tray icon")
	}
	w.visible = true
	w.menu = options.Menu
	return nil
}
func (w *windowsPresentation) destroyTray() {
	if w.visible {
		w.notify(2)
	}
	w.visible = false
	w.menu = nil
}
func (w *windowsPresentation) balloon(title, body string) bool {
	if !w.visible {
		return false
	}
	w.icon.Flags = 0x10
	copyUIString(w.icon.InfoTitle[:], title)
	copyUIString(w.icon.Info[:], body)
	w.icon.InfoFlags = 1
	return w.notify(1)
}
func (w *windowsPresentation) register(id uint32, key presentationKey) bool {
	return uiCall("RegisterHotKey", w.window, uintptr(id), uintptr(key.Modifiers|0x4000), uintptr(key.Code)) != 0
}
func (w *windowsPresentation) unregister(id uint32) {
	uiCall("UnregisterHotKey", w.window, uintptr(id))
}
func (w *windowsPresentation) popup() {
	ids := map[uintptr]string{}
	next := uintptr(1)
	var build func([]trayItem) uintptr
	build = func(items []trayItem) uintptr {
		menu := uiCall("CreatePopupMenu")
		for _, item := range items {
			flags := uintptr(0)
			id := next
			next++
			if item.Type == "separator" {
				flags = 0x800
			} else {
				if item.Enabled != nil && !*item.Enabled {
					flags |= 3
				}
				if item.Checked {
					flags |= 8
				}
				if len(item.Submenu) > 0 {
					flags |= 0x10
					id = build(item.Submenu)
				} else {
					ids[id] = item.ID
				}
			}
			uiCall("AppendMenuW", menu, flags, id, uintptr(unsafe.Pointer(uiString(item.Label))))
		}
		return menu
	}
	menu := build(w.menu)
	defer uiCall("DestroyMenu", menu)
	var point presentationPoint
	uiCall("GetCursorPos", uintptr(unsafe.Pointer(&point)))
	uiCall("SetForegroundWindow", w.window)
	selected := uiCall("TrackPopupMenu", menu, 0x100|2, uintptr(point.X), uintptr(point.Y), 0, w.window, 0)
	uiCall("PostMessageW", w.window, 0, 0, 0)
	if id := ids[selected]; id != "" {
		if event := w.controller.menuClick(id); event != nil {
			w.menu = w.controller.tray.Menu
			w.emit(presentationReply{Service: "tray", Event: event})
		}
	}
}
func runNativePresentation(commands <-chan presentationCommand, emit func(presentationReply)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if dpi := presentationUser32.NewProc("SetProcessDpiAwarenessContext"); dpi.Find() == nil {
		dpi.Call(^uintptr(3))
	}
	w := &windowsPresentation{emit: emit}
	controller := &presentationController{native: w, platform: "windows"}
	w.controller = controller
	w.restartMessage = uint32(uiCall("RegisterWindowMessageW", uintptr(unsafe.Pointer(uiString("TaskbarCreated")))))
	callback := syscall.NewCallback(func(hwnd uintptr, message uint32, wp, lp uintptr) uintptr {
		switch message {
		case 0x312:
			if event := controller.hotkey(uint32(wp)); event != nil {
				emit(presentationReply{Service: "globalShortcut", Event: event})
			}
			return 0
		case 0x8001:
			switch uint32(lp) {
			case 0x202:
				emit(presentationReply{Service: "tray", Event: map[string]any{"kind": "click"}})
			case 0x203:
				emit(presentationReply{Service: "tray", Event: map[string]any{"kind": "double-click"}})
			case 0x205, 0x7b:
				if len(w.menu) > 0 {
					w.popup()
				}
			}
			return 0
		case 0x10:
			uiCall("DestroyWindow", hwnd)
			return 0
		case 2:
			uiCall("PostQuitMessage", 0)
			return 0
		}
		if message == w.restartMessage && w.visible {
			w.visible = false
			if controller.tray != nil {
				_ = w.setTray(*controller.tray)
			}
			return 0
		}
		return uiCall("DefWindowProcW", hwnd, uintptr(message), wp, lp)
	})
	name := uiString("DynAppPresentationHelper")
	instance, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
	class := presentationWindowClass{Proc: callback, ClassName: name, Instance: instance}
	class.Size = uint32(unsafe.Sizeof(class))
	if uiCall("RegisterClassExW", uintptr(unsafe.Pointer(&class))) == 0 {
		return errors.New("could not register native helper window")
	}
	w.window = uiCall("CreateWindowExW", 0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if w.window == 0 {
		return fmt.Errorf("could not create native helper window")
	}
	defer uiCall("DestroyWindow", w.window)
	defer controller.close()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var msg presentationMSG
		for uiCall("PeekMessageW", uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1) != 0 {
			if msg.Message == 0x12 {
				return nil
			}
			uiCall("TranslateMessage", uintptr(unsafe.Pointer(&msg)))
			uiCall("DispatchMessageW", uintptr(unsafe.Pointer(&msg)))
		}
		select {
		case command, ok := <-commands:
			if !ok {
				return nil
			}
			result, err := controller.handle(command)
			reply := presentationReply{ID: command.ID, Result: result}
			if err != nil {
				reply.Error = err.Error()
			}
			emit(reply)
		case <-ticker.C:
		}
	}
}
