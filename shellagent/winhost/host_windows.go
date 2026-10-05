//go:build windows

// Package winhost is the Windows app host: the agent executable run as
// `dynapp-shell-agent.exe app --id <owner/slug>` shows one installed app in a
// WebView2 window (docs/native-host.md in the dynapp repository).
package winhost

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// Options come from the Start menu shortcut the agent wrote.
type Options struct {
	StoreID string
	Pipe    string
	URL     string
	Origin  string
	Name    string
	Icon    string
	Files   []string
}

var aumidUnsafe = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// AppUserModelID is the taskbar identity of one installed app. The shortcut
// and the running process must use the same value.
func AppUserModelID(storeID string) string {
	owner, slug, _ := strings.Cut(storeID, "/")
	id := "DynApp." + aumidUnsafe.ReplaceAllString(owner, "-") + "." + aumidUnsafe.ReplaceAllString(slug, "-")
	if len(id) > 128 {
		id = id[:128]
	}
	return id
}

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	shell32                = windows.NewLazySystemDLL("shell32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procMessageBox         = user32.NewProc("MessageBoxW")
	procLoadImage          = user32.NewProc("LoadImageW")
	procSendMessage        = user32.NewProc("SendMessageW")
	procFindWindow         = user32.NewProc("FindWindowW")
	procShowWindow         = user32.NewProc("ShowWindow")
	procSetForeground      = user32.NewProc("SetForegroundWindow")
	procGetSystemMenu      = user32.NewProc("GetSystemMenu")
	procAppendMenu         = user32.NewProc("AppendMenuW")
	procSetWindowLongPtr   = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc     = user32.NewProc("CallWindowProcW")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procSetAppUserModelID  = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
	procShellExecute       = shell32.NewProc("ShellExecuteW")
	procCreateMutex        = kernel32.NewProc("CreateMutexW")
	procFreeConsole        = kernel32.NewProc("FreeConsole")
	procGetForegroundWnd   = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput  = user32.NewProc("AttachThreadInput")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procBringWindowToTop   = user32.NewProc("BringWindowToTop")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	errAgentNotAvailable   = errors.New("the DynApp agent is not running")
	permissionsMenuCommand = uintptr(0x0010)
)

const (
	mbYesNo              = 0x00000004
	mbYesNoCancel        = 0x00000003
	mbIconWarning        = 0x00000030
	mbDefButton2         = 0x00000100
	mbSetForeground      = 0x00010000
	idYes                = 6
	idNo                 = 7
	wmSetIcon            = 0x0080
	wmSysCommand         = 0x0112
	imageIcon            = 1
	lrLoadFromFile       = 0x00000010
	smCXIcon             = 11
	smCXSmIcon           = 49
	gwlpWndProc          = ^uintptr(3) // -4
	mfString             = 0x00000000
	mfSeparator          = 0x00000800
	swRestore            = 9
	swShowNormal         = 1
	frameText       byte = 1
	frameBinary     byte = 2
)

func utf16(value string) *uint16 {
	pointer, _ := windows.UTF16PtrFromString(value)
	return pointer
}

func originKey(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

// frameConn speaks the agent's native channel framing.
type frameConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
}

func dialAgent(pipe string) (*frameConn, error) {
	timeout := 2 * time.Second
	conn, err := winio.DialPipe(pipe, &timeout)
	if err != nil {
		return nil, err
	}
	return &frameConn{conn: conn, reader: bufio.NewReaderSize(conn, 64*1024)}, nil
}

func (c *frameConn) read() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[:4])
	if length < 1 || length > 64<<20 {
		return 0, nil, errors.New("invalid agent frame")
	}
	payload := make([]byte, length-1)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, nil, err
	}
	return header[4], payload, nil
}

func (c *frameConn) write(kind byte, payload []byte) error {
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)+1))
	frame[4] = kind
	copy(frame[5:], payload)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write(frame)
	return err
}

func (c *frameConn) writeJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.write(frameText, data)
}

type host struct {
	options   Options
	view      webview2.WebView
	core      *edge.ICoreWebView2
	hwnd      uintptr
	startURL  string
	origin    string
	control   *frameConn
	mu        sync.Mutex
	sockets   map[string]*frameConn
	prompts   map[string]bool
	reviews   map[string]string
	oldProc   uintptr
	promptsMu sync.Mutex
}

// Run shows the app and returns when its window closes.
func Run(options Options) error {
	runtime.LockOSThread()
	// The agent is a console program; the app window must not bring one.
	_, _, _ = procFreeConsole.Call()
	closeLog := openHostLog(options)
	defer closeLog()
	log.Printf("app host starting: storeId=%s origin=%s url=%s pipe=%s", options.StoreID, options.Origin, options.URL, options.Pipe)
	appID := AppUserModelID(options.StoreID)
	_, _, _ = procSetAppUserModelID.Call(uintptr(unsafe.Pointer(utf16(appID))))
	h := &host{options: options, startURL: options.URL, origin: originKey(options.Origin), sockets: map[string]*frameConn{}, prompts: map[string]bool{}, reviews: map[string]string{}}
	// Queue document opens before focusing an existing instance.
	var bootstrap string
	if len(options.Files) > 0 {
		bootstrap = h.connectControl()
		if h.control == nil {
			return errors.New("cannot open files without the DynApp agent")
		}
	}
	if focusExistingInstance(appID, options.Name) {
		if h.control != nil {
			_ = h.control.conn.Close()
		}
		return nil
	}
	if h.control == nil {
		bootstrap = h.connectControl()
	}
	dataPath := filepath.Join(filepath.Dir(options.Icon), "WebView2")
	debug := os.Getenv("DYNAPP_HOST_DEBUG") == "1"
	h.view = webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     debug,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview2.WindowOptions{
			Title: options.Name, Width: 1280, Height: 820, Center: true,
		},
	})
	if h.view == nil {
		return errors.New("could not create the WebView2 window; is the WebView2 Runtime installed?")
	}
	defer h.view.Destroy()
	h.hwnd = uintptr(h.view.Window())
	// The shortcut starts minimized so its console stays out of sight; the
	// first ShowWindow call follows that, so show the window again.
	bringToFront(h.hwnd, swShowNormal)
	h.applyIcon()
	if err := h.hookWebView(bootstrap != ""); err != nil {
		log.Printf("DynApp app host: %v", err)
		bootstrap = ""
	}
	h.addPermissionsMenu()
	if bootstrap != "" {
		h.view.Init(bootstrap)
	}
	h.view.Navigate(h.startURL)
	h.view.Run()
	h.closeAll()
	return nil
}

func focusExistingInstance(appID, title string) bool {
	_, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(utf16(`Local\`+appID))))
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false
	}
	if hwnd, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(utf16("webview"))), uintptr(unsafe.Pointer(utf16(title)))); hwnd != 0 {
		bringToFront(hwnd, swRestore)
	}
	return true
}

// connectControl obtains the bridge script and start URL from the agent,
// starting a per-user agent if none answers. It returns "" when the app must
// run as a plain web app.
func (h *host) connectControl() string {
	conn, err := dialAgent(h.options.Pipe)
	if err != nil {
		if executable, exeErr := os.Executable(); exeErr == nil {
			// open-url starts the machine service when it exists, else a
			// per-user agent, so a stopped service is not shadowed.
			command := exec.Command(executable, "open-url", "dynapp://start")
			command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS}
			_ = command.Start()
		}
		for attempt := 0; attempt < 20 && err != nil; attempt++ {
			time.Sleep(250 * time.Millisecond)
			conn, err = dialAgent(h.options.Pipe)
		}
		if err != nil {
			log.Printf("control: cannot reach the agent on %s: %v; loading without the bridge", h.options.Pipe, err)
			return ""
		}
	}
	_ = conn.writeJSON(map[string]any{"type": "native-host-hello", "version": 1, "storeId": h.options.StoreID, "purpose": "control"})
	_ = conn.conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	kind, payload, err := conn.read()
	_ = conn.conn.SetReadDeadline(time.Time{})
	if err != nil || kind != frameText {
		log.Printf("control: no ready frame: %v", err)
		_ = conn.conn.Close()
		return ""
	}
	var ready map[string]any
	if json.Unmarshal(payload, &ready) != nil || ready["type"] != "native-host-ready" {
		log.Printf("control: agent refused the app: %s", truncate(string(payload), 300))
		if message, _ := ready["error"].(string); message != "" {
			messageBox(0, message, h.options.Name, mbIconWarning)
		}
		_ = conn.conn.Close()
		return ""
	}
	if value, _ := ready["url"].(string); originKey(value) == h.origin && value != "" {
		h.startURL = value
	}
	if len(h.options.Files) > 0 {
		if err := queueOpenFiles(conn, h.options.Files); err != nil {
			_ = conn.conn.Close()
			messageBox(0, err.Error(), h.options.Name, mbIconWarning)
			return ""
		}
	}
	h.control = conn
	go h.readControl(conn)
	script, _ := ready["bootstrapScript"].(string)
	log.Printf("control: ready; bridge script %d bytes, start %s, capabilities %v", len(script), h.startURL, ready["capabilities"])
	return script
}

func (h *host) readControl(conn *frameConn) {
	for {
		kind, payload, err := conn.read()
		if err != nil {
			h.mu.Lock()
			h.control = nil
			pending := h.reviews
			h.reviews = map[string]string{}
			h.mu.Unlock()
			for rid := range pending {
				if !strings.HasPrefix(rid, "menu-") {
					h.deliver(map[string]any{"rid": rid, "event": "review", "error": errAgentNotAvailable.Error()})
				}
			}
			return
		}
		if kind != frameText {
			continue
		}
		var frame map[string]any
		if json.Unmarshal(payload, &frame) != nil {
			continue
		}
		switch frame["type"] {
		case "native-host-permission-request":
			h.prompt(frame, conn)
		case "native-host-review-result":
			id, _ := frame["id"].(string)
			h.mu.Lock()
			_, known := h.reviews[id]
			delete(h.reviews, id)
			h.mu.Unlock()
			if !known {
				continue
			}
			changed, _ := frame["changed"].(bool)
			if !strings.HasPrefix(id, "menu-") {
				reply := map[string]any{"rid": id, "event": "review", "changed": changed}
				if message, _ := frame["error"].(string); message != "" {
					reply["error"] = message
				}
				h.deliver(reply)
			}
			if changed {
				h.view.Dispatch(func() { h.view.Eval("location.reload()") })
			}
		}
	}
}

// hookWebView attaches the origin-checked bridge and navigation policy to the
// library's WebView2 instance.
func (h *host) hookWebView(bridge bool) error {
	chromium, err := chromiumOf(h.view)
	if err != nil {
		return err
	}
	h.core = unexportedField[*edge.ICoreWebView2](reflect.ValueOf(chromium).Elem(), "webview")
	if h.core == nil {
		return errors.New("WebView2 is not ready")
	}
	if settings, err := chromium.GetSettings(); err == nil {
		_ = settings.PutAreDefaultContextMenusEnabled(true)
	}
	original := chromium.MessageCallback
	chromium.MessageCallback = func(message string) {
		if !strings.Contains(message, `"dynappNativeHost":1`) {
			if original != nil {
				original(message)
			}
			return
		}
		// Only the installed app's own top-level document may use the bridge.
		if source := documentSource(h.core); !bridge || originKey(source) != h.origin {
			log.Printf("bridge: dropped a message from %q (app origin %s, bridge %v)", source, h.origin, bridge)
			return
		}
		h.handleBridge(message)
	}
	chromium.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		source := documentSource(sender)
		if source == "" || strings.HasPrefix(source, "about:") || originKey(source) == h.origin {
			return
		}
		openExternal(source)
		h.view.Navigate(h.startURL)
	}
	return nil
}

func chromiumOf(view webview2.WebView) (*edge.Chromium, error) {
	value := reflect.ValueOf(view)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return nil, errors.New("unexpected WebView2 wrapper")
	}
	browser := value.Elem().FieldByName("browser")
	if !browser.IsValid() {
		return nil, errors.New("the WebView2 wrapper changed; update winhost")
	}
	chromium, ok := reflect.NewAt(browser.Type(), unsafe.Pointer(browser.UnsafeAddr())).Elem().Interface().(*edge.Chromium)
	if !ok {
		return nil, errors.New("the WebView2 browser is not Chromium")
	}
	return chromium, nil
}

func unexportedField[T any](value reflect.Value, name string) T {
	var zero T
	field := value.FieldByName(name)
	if !field.IsValid() {
		return zero
	}
	result, _ := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(T)
	return result
}

// documentSource calls ICoreWebView2::get_Source, the URL of the current
// top-level document. The vtable is IUnknown (3 entries), get_Settings,
// get_Source.
func documentSource(view *edge.ICoreWebView2) string {
	if view == nil {
		return ""
	}
	vtable := *(*unsafe.Pointer)(unsafe.Pointer(view))
	getSource := *(*uintptr)(unsafe.Add(vtable, 4*unsafe.Sizeof(uintptr(0))))
	var source *uint16
	result, _, _ := syscall.SyscallN(getSource, uintptr(unsafe.Pointer(view)), uintptr(unsafe.Pointer(&source)))
	if result != 0 || source == nil {
		return ""
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(source))
	return windows.UTF16PtrToString(source)
}

func openExternal(target string) {
	scheme := strings.ToLower(strings.SplitN(target, ":", 2)[0])
	if scheme != "http" && scheme != "https" && scheme != "mailto" {
		return
	}
	_, _, _ = procShellExecute.Call(0, uintptr(unsafe.Pointer(utf16("open"))), uintptr(unsafe.Pointer(utf16(target))), 0, 0, 1)
}

func (h *host) applyIcon() {
	if h.options.Icon == "" {
		return
	}
	for _, size := range []struct {
		which  uintptr
		metric uintptr
	}{{1, smCXIcon}, {0, smCXSmIcon}} {
		pixels, _, _ := procGetSystemMetrics.Call(size.metric)
		icon, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(utf16(h.options.Icon))), imageIcon, pixels, pixels, lrLoadFromFile)
		if icon != 0 {
			_, _, _ = procSendMessage.Call(h.hwnd, wmSetIcon, size.which, icon)
		}
	}
}

// addPermissionsMenu puts "Permissions…" in the window's system menu.
func (h *host) addPermissionsMenu() {
	menu, _, _ := procGetSystemMenu.Call(h.hwnd, 0)
	if menu == 0 {
		return
	}
	_, _, _ = procAppendMenu.Call(menu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenu.Call(menu, mfString, permissionsMenuCommand, uintptr(unsafe.Pointer(utf16("Permissions…"))))
	callback := windows.NewCallback(func(hwnd, message, wparam, lparam uintptr) uintptr {
		if message == wmSysCommand && wparam&0xFFF0 == permissionsMenuCommand {
			go h.requestReview("")
			return 0
		}
		result, _, _ := procCallWindowProc.Call(h.oldProc, hwnd, message, wparam, lparam)
		return result
	})
	h.oldProc, _, _ = procSetWindowLongPtr.Call(h.hwnd, gwlpWndProc, callback)
}

type bridgeMessage struct {
	Op     string `json:"op"`
	SID    string `json:"sid"`
	RID    string `json:"rid"`
	Text   string `json:"text"`
	Binary string `json:"binary"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

func (h *host) handleBridge(raw string) {
	var message bridgeMessage
	if json.Unmarshal([]byte(raw), &message) != nil {
		return
	}
	switch message.Op {
	case "open":
		h.mu.Lock()
		_, exists := h.sockets[message.SID]
		h.mu.Unlock()
		if message.SID != "" && !exists {
			go h.openSocket(message.SID)
		}
	case "send":
		h.mu.Lock()
		conn := h.sockets[message.SID]
		h.mu.Unlock()
		if conn == nil {
			return
		}
		if message.Binary != "" {
			if data, err := base64.StdEncoding.DecodeString(message.Binary); err == nil {
				_ = conn.write(frameBinary, data)
			}
		} else {
			_ = conn.write(frameText, []byte(message.Text))
		}
	case "close":
		h.endSocket(message.SID, message.Code, message.Reason)
	case "review":
		if message.RID != "" {
			go h.requestReview(message.RID)
		}
	}
}

// openSocket bridges one page socket to a new agent connection.
func (h *host) openSocket(sid string) {
	conn, err := dialAgent(h.options.Pipe)
	if err != nil {
		log.Printf("socket %s: cannot reach the agent: %v", sid, err)
		h.deliver(map[string]any{"sid": sid, "event": "error", "error": errAgentNotAvailable.Error()})
		h.deliver(map[string]any{"sid": sid, "event": "close", "code": 1006, "reason": errAgentNotAvailable.Error()})
		return
	}
	h.mu.Lock()
	h.sockets[sid] = conn
	h.mu.Unlock()
	_ = conn.writeJSON(map[string]any{"type": "native-host-hello", "version": 1, "storeId": h.options.StoreID, "purpose": "app"})
	ready := false
	for {
		kind, payload, err := conn.read()
		if err != nil {
			h.endSocket(sid, 1006, "The DynApp agent connection closed")
			return
		}
		// The agent writes JSON with sorted keys, so "type" is rarely first.
		if kind == frameText && (!ready || strings.Contains(string(payload), `"type":"native-host-`)) {
			var frame map[string]any
			if json.Unmarshal(payload, &frame) == nil {
				if kindName, _ := frame["type"].(string); strings.HasPrefix(kindName, "native-host-") {
					switch kindName {
					case "native-host-ready":
						ready = true
						log.Printf("socket %s: ready, capabilities %v", sid, frame["capabilities"])
						h.deliver(map[string]any{"sid": sid, "event": "open"})
					case "native-host-permission-request":
						h.prompt(frame, conn)
					case "native-host-error":
						text, _ := frame["error"].(string)
						log.Printf("socket %s: agent refused: %s", sid, text)
						h.deliver(map[string]any{"sid": sid, "event": "error", "error": text})
						h.endSocket(sid, 4401, text)
						return
					case "native-host-close":
						code, _ := frame["code"].(float64)
						reason, _ := frame["reason"].(string)
						h.endSocket(sid, int(code), reason)
						return
					}
					continue
				}
			}
		}
		if !ready {
			continue
		}
		if kind == frameBinary {
			h.deliver(map[string]any{"sid": sid, "event": "message", "binary": base64.StdEncoding.EncodeToString(payload)})
		} else {
			h.deliver(map[string]any{"sid": sid, "event": "message", "text": string(payload)})
		}
	}
}

func (h *host) endSocket(sid string, code int, reason string) {
	h.mu.Lock()
	conn := h.sockets[sid]
	delete(h.sockets, sid)
	h.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.conn.Close()
	if code == 0 {
		code = 1000
	}
	h.deliver(map[string]any{"sid": sid, "event": "close", "code": code, "reason": reason})
}

func (h *host) closeAll() {
	h.mu.Lock()
	sockets := h.sockets
	h.sockets = map[string]*frameConn{}
	control := h.control
	h.mu.Unlock()
	for _, conn := range sockets {
		_ = conn.conn.Close()
	}
	if control != nil {
		_ = control.conn.Close()
	}
}

func (h *host) deliver(message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	script := "window.__dynappNativeHostDeliver && window.__dynappNativeHostDeliver(" + string(data) + ")"
	h.view.Dispatch(func() { h.view.Eval(script) })
}

func (h *host) requestReview(rid string) {
	h.mu.Lock()
	control := h.control
	h.mu.Unlock()
	if control == nil {
		if rid != "" {
			h.deliver(map[string]any{"rid": rid, "event": "review", "error": errAgentNotAvailable.Error()})
		}
		messageBox(h.hwnd, "The DynApp agent is not running, so permissions cannot be changed right now.", h.options.Name, mbIconWarning)
		return
	}
	id := rid
	if id == "" {
		id = fmt.Sprintf("menu-%d", time.Now().UnixNano())
	}
	h.mu.Lock()
	h.reviews[id] = id
	h.mu.Unlock()
	_ = control.writeJSON(map[string]any{"type": "native-host-review", "id": id})
}

type permissionGroup struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Danger      string   `json:"danger"`
	Permissions []string `json:"permissions"`
	Reasons     []string `json:"reasons"`
	Granted     bool     `json:"granted"`
	Suggested   bool     `json:"suggested"`
}

// prompt asks with system message boxes, which page script cannot draw or
// click. "No" is the default button. Very-high-risk groups are asked one by
// one and start unanswered.
func (h *host) prompt(frame map[string]any, conn *frameConn) {
	requestID, _ := frame["requestId"].(string)
	h.promptsMu.Lock()
	if requestID == "" || h.prompts[requestID] {
		h.promptsMu.Unlock()
		return
	}
	h.prompts[requestID] = true
	h.promptsMu.Unlock()
	go func() {
		// One prompt at a time; the lock also serializes the message boxes.
		h.promptsMu.Lock()
		defer h.promptsMu.Unlock()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		var groups []permissionGroup
		raw, _ := json.Marshal(frame["groups"])
		_ = json.Unmarshal(raw, &groups)
		kind, _ := frame["kind"].(string)
		appName, _ := frame["appName"].(string)
		origin, _ := frame["origin"].(string)
		if appName == "" {
			appName = h.options.Name
		}
		capabilities, decided := askPermissions(h.hwnd, kind, appName, origin, groups)
		reply := map[string]any{"type": "native-host-permission-decision", "requestId": requestID, "capabilities": nil}
		if decided {
			reply["capabilities"] = capabilities
		}
		_ = conn.writeJSON(reply)
	}()
}

func describeGroup(group permissionGroup) string {
	lines := []string{fmt.Sprintf("• %s (%s risk)", group.Title, group.Danger)}
	if group.Summary != "" {
		lines = append(lines, "   "+group.Summary)
	}
	for _, reason := range group.Reasons {
		lines = append(lines, "   “"+reason+"”")
	}
	return strings.Join(lines, "\n")
}

func askPermissions(owner uintptr, kind, appName, origin string, groups []permissionGroup) ([]string, bool) {
	capabilities := []string{}
	if kind == "review" {
		for _, group := range groups {
			state := "not allowed"
			if group.Granted {
				state = "allowed"
			}
			text := fmt.Sprintf("Allow %s to use this?\n\n%s\n\nCurrently %s.\n%s", appName, describeGroup(group), state, origin)
			switch messageBox(owner, text, appName+" permissions", mbYesNoCancel|mbIconWarning|mbDefButton2|mbSetForeground) {
			case idYes:
				capabilities = append(capabilities, group.Permissions...)
			case idNo:
			default:
				return nil, false
			}
		}
		return capabilities, true
	}
	var ordinary, risky []permissionGroup
	for _, group := range groups {
		if group.Danger == "very-high" {
			risky = append(risky, group)
		} else {
			ordinary = append(ordinary, group)
		}
	}
	question := "wants to use this computer"
	if kind == "delta" {
		question = "is asking for more access"
	}
	if len(ordinary) > 0 {
		descriptions := make([]string, len(ordinary))
		for index, group := range ordinary {
			descriptions[index] = describeGroup(group)
		}
		text := fmt.Sprintf("%s %s:\n\n%s\n\nAllow these? You can change this later from the window menu → Permissions….\n%s", appName, question, strings.Join(descriptions, "\n\n"), origin)
		if messageBox(owner, text, appName, mbYesNo|mbIconWarning|mbDefButton2|mbSetForeground) != idYes {
			return nil, true
		}
		for _, group := range ordinary {
			capabilities = append(capabilities, group.Permissions...)
		}
	}
	for _, group := range risky {
		text := fmt.Sprintf("%s also asks for a very-high-risk permission:\n\n%s\n\nAllow it?\n%s", appName, describeGroup(group), origin)
		if messageBox(owner, text, appName, mbYesNo|mbIconWarning|mbDefButton2|mbSetForeground) == idYes {
			capabilities = append(capabilities, group.Permissions...)
		}
	}
	return capabilities, true
}

func messageBox(owner uintptr, text, title string, flags uintptr) int {
	result, _, _ := procMessageBox.Call(owner, uintptr(unsafe.Pointer(utf16(text))), uintptr(unsafe.Pointer(utf16(title))), flags)
	return int(result)
}

const (
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040
)

var (
	hwndTopmost   = ^uintptr(0)     // HWND_TOPMOST (-1)
	hwndNoTopmost = ^uintptr(0) - 1 // HWND_NOTOPMOST (-2)
)

// bringToFront raises a window that a background process (the agent, asked
// by Dyner in the browser) started. Windows refuses SetForegroundWindow from a
// process without the last input, so share the foreground window's input
// queue for the call, and pass through topmost so the window at least lands
// above the browser if the focus change is still refused.
func bringToFront(hwnd uintptr, show uintptr) {
	_, _, _ = procShowWindow.Call(hwnd, show)
	current, _, _ := procGetCurrentThreadID.Call()
	if foreground, _, _ := procGetForegroundWnd.Call(); foreground != 0 && foreground != hwnd {
		if thread, _, _ := procGetWindowThreadPID.Call(foreground, 0); thread != 0 && thread != current {
			if attached, _, _ := procAttachThreadInput.Call(current, thread, 1); attached != 0 {
				defer procAttachThreadInput.Call(current, thread, 0)
			}
		}
	}
	_, _, _ = procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	_, _, _ = procSetWindowPos.Call(hwnd, hwndNoTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	_, _, _ = procBringWindowToTop.Call(hwnd)
	_, _, _ = procSetForeground.Call(hwnd)
}

// openHostLog writes host.log next to the app's icon; it is how a native app
// on Windows can be diagnosed, since the host has no console.
func openHostLog(options Options) func() {
	dir := filepath.Dir(options.Icon)
	if options.Icon == "" {
		dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "DynApp")
	}
	path := filepath.Join(dir, "host.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		_ = os.Remove(path)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return func() { _ = file.Close() }
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// Wait for acknowledgement before a second host exits after focusing the app.
func queueOpenFiles(conn *frameConn, paths []string) error {
	if len(paths) > 64 {
		return errors.New("too many files to open")
	}
	files := make([]string, len(paths))
	for i, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		files[i] = absolute
	}
	_ = conn.conn.SetDeadline(time.Now().Add(6 * time.Second))
	defer conn.conn.SetDeadline(time.Time{})
	if err := conn.writeJSON(map[string]any{"type": "native-host-open-files", "id": "launch-files", "paths": files}); err != nil {
		return err
	}
	kind, payload, err := conn.read()
	if err != nil {
		return err
	}
	var result struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if kind != frameText || json.Unmarshal(payload, &result) != nil || result.Type != "native-host-open-files-result" {
		return errors.New("invalid file-open acknowledgement")
	}
	if result.Error != "" {
		return errors.New(result.Error)
	}
	return nil
}
