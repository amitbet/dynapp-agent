//go:build windows

package winhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WebView2 reflects the actual DOM File objects as ICoreWebView2File. Paths
// come from the runtime, never from filenames or page-supplied path strings.
// Interface IDs are from the released WebView2.h, not the proposal IDL.
// https://learn.microsoft.com/microsoft-edge/webview2/reference/win32/icorewebview2webmessagereceivedeventargs2
var (
	iidMessageArgs2   = windows.GUID{Data1: 0x06fc7ab7, Data2: 0xc90c, Data3: 0x4297, Data4: [8]byte{0x93, 0x89, 0x33, 0xca, 0x01, 0xcf, 0x6d, 0x5e}}
	iidWebViewFile    = windows.GUID{Data1: 0xf2c19559, Data2: 0x6bc1, Data3: 0x4583, Data4: [8]byte{0xa7, 0x57, 0x90, 0x02, 0x1b, 0xe9, 0xaf, 0xec}}
	iidMessageHandler = windows.GUID{Data1: 0x57213f19, Data2: 0x00e6, Data3: 0x49fa, Data4: [8]byte{0x8e, 0x07, 0x89, 0x8e, 0xa0, 0x1e, 0xcb, 0xd2}}
)

// Keep pointers converted to uintptr by callers alive and off moving Go
// stacks across the wrapper and the native call, just like LazyProc.Call.
//
//go:uintptrescapes
func comCall(object uintptr, slot uintptr, args ...uintptr) uintptr {
	table := *(*uintptr)(unsafe.Pointer(object))
	method := *(*uintptr)(unsafe.Pointer(table + slot*unsafe.Sizeof(uintptr(0))))
	result, _, _ := syscall.SyscallN(method, append([]uintptr{object}, args...)...)
	return result
}
func comRelease(object uintptr) {
	if object != 0 {
		comCall(object, 2)
	}
}
func comQuery(object uintptr, iid *windows.GUID) uintptr {
	var result uintptr
	if comCall(object, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&result))) != 0 {
		return 0
	}
	return result
}
func comString(object, slot uintptr) string {
	var value *uint16
	if comCall(object, slot, uintptr(unsafe.Pointer(&value))) != 0 || value == nil {
		return ""
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(value))
	return windows.UTF16PtrToString(value)
}

type webMessageHandler struct {
	vtable  *[4]uintptr
	refs    int32
	receive func(uintptr)
}

var dropMessageVTable = [4]uintptr{
	syscall.NewCallback(func(this, iid, result uintptr) uintptr {
		if result == 0 {
			return 0x80004003
		}
		*(*uintptr)(unsafe.Pointer(result)) = 0
		wanted := *(*windows.GUID)(unsafe.Pointer(iid))
		unknown := windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
		if wanted != unknown && wanted != iidMessageHandler {
			return 0x80004002
		}
		*(*uintptr)(unsafe.Pointer(result)) = this
		atomic.AddInt32(&(*webMessageHandler)(unsafe.Pointer(this)).refs, 1)
		return 0
	}),
	syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(atomic.AddInt32(&(*webMessageHandler)(unsafe.Pointer(this)).refs, 1))
	}),
	syscall.NewCallback(func(this uintptr) uintptr {
		return uintptr(atomic.AddInt32(&(*webMessageHandler)(unsafe.Pointer(this)).refs, -1))
	}),
	syscall.NewCallback(func(this, sender, args uintptr) uintptr {
		(*webMessageHandler)(unsafe.Pointer(this)).receive(args)
		return 0
	}),
}

func (h *host) installFileDropReceiver() error {
	handler := &webMessageHandler{vtable: &dropMessageVTable, refs: 1, receive: h.receiveFileDrop}
	var token int64
	// ICoreWebView2::add_WebMessageReceived is slot 34 (including IUnknown).
	result := comCall(uintptr(unsafe.Pointer(h.core)), 34, uintptr(unsafe.Pointer(handler)), uintptr(unsafe.Pointer(&token)))
	if result != 0 {
		return fmt.Errorf("register WebView2 file drops: HRESULT %#x", result)
	}
	h.dropHandler = handler // keep the Go callback object alive for the view's lifetime
	return nil
}

func (h *host) receiveFileDrop(args uintptr) {
	if args == 0 {
		return
	}
	// Use the event's source, not only the current page: queued messages can
	// arrive after navigation. Only top-level WebMessageReceived is registered.
	source := comString(args, 3)
	if originKey(source) != h.origin || originKey(documentSource(h.core)) != h.origin {
		return
	}
	raw := comString(args, 5)
	var message struct {
		Marker int    `json:"dynappNativeHost"`
		Op     string `json:"op"`
		RID    string `json:"rid"`
	}
	if json.Unmarshal([]byte(raw), &message) != nil || message.Marker != 1 || message.Op != "dropFiles" || message.RID == "" {
		return
	}
	reply := map[string]any{"event": "result", "rid": message.RID}
	paths, err := webViewDropPaths(args)
	if err == nil {
		// Read COM objects on the UI thread, then reconnect off-thread so an
		// agent restart cannot freeze the window or permanently disable drops.
		go h.sendDropFiles(message.RID, paths)
		return
	}
	reply["error"] = err.Error()
	h.deliver(reply)
}

func (h *host) sendDropFiles(rid string, paths []string) {
	control, _, err := h.acquireControl(false)
	// The agent checks the current grant for this authenticated host's store
	// ID on every drop, including after revocation or uninstall.
	if err == nil {
		err = control.writeJSON(map[string]any{"type": "native-host-drop-files", "id": rid, "paths": paths})
	}
	if err != nil {
		h.deliver(map[string]any{"event": "result", "rid": rid, "error": err.Error()})
	}
}

//go:uintptrescapes
func webViewDropPaths(args uintptr) ([]string, error) {
	extended := comQuery(args, &iidMessageArgs2)
	if extended == 0 {
		return nil, errors.New("update the WebView2 Runtime to receive native file drops")
	}
	defer comRelease(extended)
	var collection uintptr
	if comCall(extended, 6, uintptr(unsafe.Pointer(&collection))) != 0 || collection == 0 {
		return nil, errors.New("could not read dropped files")
	}
	defer comRelease(collection)
	var count uint32
	if comCall(collection, 3, uintptr(unsafe.Pointer(&count))) != 0 || count > 128 {
		return nil, errors.New("invalid dropped file count (maximum 128)")
	}
	paths := make([]string, 0, count)
	for index := uint32(0); index < count; index++ {
		var object uintptr
		if comCall(collection, 4, uintptr(index), uintptr(unsafe.Pointer(&object))) != 0 || object == 0 {
			return nil, errors.New("could not read dropped file")
		}
		file := comQuery(object, &iidWebViewFile)
		comRelease(object)
		if file == 0 {
			return nil, errors.New("dropped object is not a filesystem file")
		}
		path := comString(file, 3)
		comRelease(file)
		if !filepath.IsAbs(path) {
			return nil, errors.New("dropped file has no original filesystem path")
		}
		paths = append(paths, path)
	}
	return paths, nil
}
