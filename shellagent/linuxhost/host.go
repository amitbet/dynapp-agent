//go:build linux && cgo && webkitgtk

package linuxhost

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <stdlib.h>
#include "gtkhost.h"
*/
import "C"

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/cgo"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var errAgentNotAvailable = errors.New("the DynApp agent is not running")

type pendingPrompt struct {
	id   string
	page string
	conn *FrameConn
}

type host struct {
	options  Options
	policy   Policy
	token    string
	startURL string

	mu        sync.Mutex
	control   *FrameConn
	controlMu sync.Mutex
	closed    bool
	sockets   map[string]*FrameConn
	wt        *WebTransportBridge
	reviews   map[string]bool
	prompts   map[string]bool // request ids already queued or shown

	// Main-thread state.
	promptQueue []pendingPrompt
	showing     *pendingPrompt
	started     bool
}

var current *host

// Check opens the display and reports why it cannot.
func Check() error {
	if message := C.host_check(); message != nil {
		return errors.New(C.GoString(message))
	}
	return nil
}

// Run shows the app and returns when its window closes. It must run on the
// main thread.
func Run(options Options) int {
	runtime.LockOSThread()
	closeLog := openHostLog(options)
	defer closeLog()
	log.Printf("app host starting: storeId=%s appId=%s origin=%s url=%s socket=%s", options.StoreID, options.AppID, options.Origin, options.URL, options.Socket)
	current = &host{
		options: options, policy: Policy{Origin: OriginKey(options.Origin)}, token: randomToken(),
		startURL: options.URL, sockets: map[string]*FrameConn{}, reviews: map[string]bool{}, prompts: map[string]bool{},
	}
	current.wt = &WebTransportBridge{Origin: current.policy.Origin, Deliver: current.deliverWebTransport}
	argv := []string{os.Args[0]}
	for _, file := range options.Files {
		if absolute, err := filepath.Abs(file); err == nil {
			argv = append(argv, absolute)
		}
	}
	cArgs := make([]*C.char, len(argv)+1)
	for index, arg := range argv {
		cArgs[index] = C.CString(arg)
	}
	defer func() {
		for _, arg := range cArgs {
			if arg != nil {
				C.free(unsafe.Pointer(arg))
			}
		}
	}()
	appID := C.CString(options.AppID)
	defer C.free(unsafe.Pointer(appID))
	status := C.host_run(appID, C.int(len(argv)), &cArgs[0])
	current.closeAll()
	return int(status)
}

func randomToken() string {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}

// ---- Main-thread helpers -------------------------------------------------

func dispatch(function func()) {
	C.host_dispatch(C.uintptr_t(cgo.NewHandle(function)))
}

//export goDispatch
func goDispatch(handle C.uintptr_t) {
	value := cgo.Handle(handle)
	function := value.Value().(func())
	value.Delete()
	function()
}

func withCString(value string, use func(*C.char)) {
	text := C.CString(value)
	defer C.free(unsafe.Pointer(text))
	use(text)
}

func showError(title, message string) {
	withCString(title, func(cTitle *C.char) {
		withCString(message, func(cMessage *C.char) { C.host_show_error(cTitle, cMessage) })
	})
}

func openExternal(target string) {
	if target != "" {
		withCString(target, func(value *C.char) { C.host_open_external(value) })
	}
}

func cStrings(values **C.char, count C.int) []string {
	if values == nil || count <= 0 {
		return nil
	}
	items := unsafe.Slice(values, int(count))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil {
			result = append(result, C.GoString(item))
		}
	}
	return result
}

// ---- Application callbacks -----------------------------------------------

//export goStart
func goStart(paths **C.char, count C.int) {
	h := current
	if h.started {
		return
	}
	h.started = true
	h.options.Files = append(h.options.Files[:0], cStrings(paths, count)...)
	bootstrap := ""
	_, script, err := h.acquireControl(true)
	if err != nil {
		log.Printf("control: %v; loading without the bridge", err)
		if len(h.options.Files) > 0 {
			showError(h.options.Name, "Cannot open files without the DynApp agent: "+err.Error())
		}
	} else {
		bootstrap = script
	}
	dataDir, cacheDir := webDataDirs(h.options)
	_ = os.MkdirAll(dataDir, 0o700)
	_ = os.MkdirAll(cacheDir, 0o700)
	var userScript *C.char
	if bootstrap != "" {
		userScript = C.CString(WrapBootstrap(bootstrap, h.token))
		defer C.free(unsafe.Pointer(userScript))
	}
	debug := 0
	if os.Getenv("DYNAPP_HOST_DEBUG") == "1" {
		debug = 1
	}
	withCString(h.options.Name, func(title *C.char) {
		withCString(h.options.Icon, func(icon *C.char) {
			withCString(dataDir, func(data *C.char) {
				withCString(cacheDir, func(cache *C.char) {
					C.host_create_window(title, icon, data, cache, userScript, C.int(debug))
				})
			})
		})
	})
	withCString(h.startURL, func(start *C.char) { C.host_load(start) })
}

//export goOpenFiles
func goOpenFiles(paths **C.char, count C.int) {
	files := cStrings(paths, count)
	C.host_present()
	h := current
	go func() {
		control, _, err := h.acquireControl(false)
		if err == nil {
			err = queueOpenFiles(control, files)
		}
		if err != nil {
			dispatch(func() { showError(h.options.Name, err.Error()) })
		}
	}()
}

//export goWindowClosed
func goWindowClosed() {
	current.closeAll()
}

//export goMenuPermissions
func goMenuPermissions() {
	go current.requestReview("")
}

//export goIsAppOrigin
func goIsAppOrigin(uri *C.char) C.int {
	if current.policy.isApp(C.GoString(uri)) {
		return 1
	}
	return 0
}

//export goNavigationPolicy
func goNavigationPolicy(uri *C.char, mainView C.int, userGesture C.int) C.int {
	verdict, target := current.policy.Navigation(C.GoString(uri), mainView != 0, userGesture != 0)
	openExternal(target)
	return C.int(verdict)
}

//export goMainFrameResponse
func goMainFrameResponse(uri *C.char) C.int {
	verdict, target := current.policy.MainFrameResponse(C.GoString(uri))
	if verdict == Ignore {
		log.Printf("navigation: kept the app window off %s", C.GoString(uri))
	}
	openExternal(target)
	return C.int(verdict)
}

//export goNewWindowPolicy
func goNewWindowPolicy(uri *C.char) C.int {
	verdict, target := current.policy.NewWindow(C.GoString(uri))
	openExternal(target)
	return C.int(verdict)
}

//export goDrop
func goDrop(paths **C.char, count C.int, x C.int, y C.int) {
	files := cStrings(paths, count)
	if len(files) > 0 {
		current.deliver(map[string]any{"event": "drop", "paths": files, "x": int(x), "y": int(y)})
	}
}

//export goBridgeMessage
func goBridgeMessage(raw *C.char, document *C.char) {
	h := current
	message, err := ParseBridgeMessage(C.GoString(raw), C.GoString(document), h.policy.Origin, h.token)
	if err != nil {
		log.Printf("bridge: dropped a message: %v", err)
		return
	}
	h.handleBridge(message)
}

//export goPromptMessage
func goPromptMessage(id *C.char, raw *C.char) {
	current.promptMessage(C.GoString(id), C.GoString(raw))
}

// ---- Agent control channel -----------------------------------------------

func (h *host) currentControl() *FrameConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.control
}

// acquireControl connects the control channel, starting the agent through
// its dynapp:// handler when it does not answer. Only the initial connection
// may change the start URL or queue launch files.
func (h *host) acquireControl(initial bool) (*FrameConn, string, error) {
	h.controlMu.Lock()
	defer h.controlMu.Unlock()
	h.mu.Lock()
	control, closed := h.control, h.closed
	h.mu.Unlock()
	if closed {
		return nil, "", errors.New("the app window is closed")
	}
	if control != nil {
		return control, "", nil
	}
	conn, err := DialAgent(h.options.Socket)
	if err != nil {
		startAgent()
		deadline := time.Now().Add(8 * time.Second)
		for err != nil && time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			conn, err = DialAgent(h.options.Socket)
		}
		if err != nil {
			return nil, "", fmt.Errorf("cannot reach the agent on %s: %w", h.options.Socket, err)
		}
	}
	_ = conn.Conn.SetDeadline(time.Now().Add(6 * time.Second))
	if err := conn.WriteJSON(map[string]any{"type": "native-host-hello", "version": 1, "storeId": h.options.StoreID, "purpose": "control"}); err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	kind, payload, err := conn.Read()
	var ready map[string]any
	if err != nil || kind != frameText || json.Unmarshal(payload, &ready) != nil {
		_ = conn.Close()
		return nil, "", fmt.Errorf("no agent ready frame: %v", err)
	}
	_ = conn.Conn.SetDeadline(time.Time{})
	if ready["type"] != "native-host-ready" {
		_ = conn.Close()
		message, _ := ready["error"].(string)
		if message == "" {
			message = "the agent refused the app connection"
		}
		log.Printf("control: agent refused the app: %s", message)
		if initial {
			showError(h.options.Name, message)
		}
		return nil, "", errors.New(message)
	}
	if value, _ := ready["url"].(string); initial && value != "" && OriginKey(value) == h.policy.Origin {
		h.startURL = value
	}
	if initial && len(h.options.Files) > 0 {
		if err := queueOpenFiles(conn, h.options.Files); err != nil {
			_ = conn.Close()
			showError(h.options.Name, err.Error())
			return nil, "", err
		}
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return nil, "", errors.New("the app window is closed")
	}
	h.control = conn
	h.mu.Unlock()
	go h.readControl(conn)
	script, _ := ready["bootstrapScript"].(string)
	log.Printf("control: ready; bridge script %d bytes, start %s, capabilities %v", len(script), h.startURL, ready["capabilities"])
	return conn, script, nil
}

// startAgent runs `dynapp-shell-agent open-url dynapp://start`, which starts
// the installed agent (systemd user unit or binary) when none answers.
func startAgent() {
	agent, err := exec.LookPath("dynapp-shell-agent")
	if err != nil {
		executable, exeErr := os.Executable()
		if exeErr != nil {
			return
		}
		agent = filepath.Join(filepath.Dir(executable), "dynapp-shell-agent")
		if _, err := os.Stat(agent); err != nil {
			return
		}
	}
	command := exec.Command(agent, "open-url", "dynapp://start")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if command.Start() == nil {
		go func() { _ = command.Wait() }()
	}
}

func (h *host) readControl(conn *FrameConn) {
	defer conn.Close()
	for {
		kind, payload, err := conn.Read()
		if err != nil {
			h.mu.Lock()
			if h.control != conn {
				h.mu.Unlock()
				return
			}
			h.control = nil
			pending := h.reviews
			h.reviews = map[string]bool{}
			closed := h.closed
			h.mu.Unlock()
			for rid := range pending {
				if !strings.HasPrefix(rid, "menu-") {
					h.deliver(map[string]any{"rid": rid, "event": "review", "error": errAgentNotAvailable.Error()})
				}
			}
			if !closed {
				go h.reconnectControl()
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
			h.queuePrompt(frame, conn)
		case "native-host-open-files-result":
			if message, _ := frame["error"].(string); message != "" {
				dispatch(func() { showError(h.options.Name, message) })
			}
		case "native-host-review-result":
			id, _ := frame["id"].(string)
			h.mu.Lock()
			known := h.reviews[id]
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
				dispatch(func() { C.host_reload() })
			}
		}
	}
}

// reconnectControl restores the control channel after an agent restart, so
// file opens and Permissions… keep working in a running app.
func (h *host) reconnectControl() {
	for delay := 500 * time.Millisecond; ; delay = min(delay*2, 10*time.Second) {
		time.Sleep(delay)
		h.mu.Lock()
		closed := h.closed
		h.mu.Unlock()
		if closed {
			return
		}
		if _, _, err := h.acquireControl(false); err == nil {
			log.Printf("control: reconnected")
			return
		}
	}
}

func (h *host) requestReview(rid string) {
	control, _, err := h.acquireControl(false)
	if err != nil {
		if rid != "" {
			h.deliver(map[string]any{"rid": rid, "event": "review", "error": err.Error()})
		} else {
			dispatch(func() { showError(h.options.Name, err.Error()) })
		}
		return
	}
	id := rid
	if id == "" {
		id = fmt.Sprintf("menu-%d", time.Now().UnixNano())
	}
	h.mu.Lock()
	h.reviews[id] = true
	h.mu.Unlock()
	_ = control.WriteJSON(map[string]any{"type": "native-host-review", "id": id})
}

// ---- Page bridge ---------------------------------------------------------

func (h *host) handleBridge(message BridgeMessage) {
	switch message.Op {
	case "titlebar":
		if message.Enabled {
			C.host_titlebar(1)
		} else {
			C.host_titlebar(0)
		}
	case "titlebarDrag":
		C.host_titlebar_drag()
	case "titlebarZoom":
		C.host_titlebar_zoom()
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
				_ = conn.Write(frameBinary, data)
			}
		} else {
			_ = conn.Write(frameText, []byte(message.Text))
		}
	case "close":
		h.endSocket(message.SID, message.Code, message.Reason)
	case "review":
		if message.RID != "" {
			go h.requestReview(message.RID)
		}
	default:
		if strings.HasPrefix(message.Op, "wt-") {
			h.wt.Handle(message)
		}
	}
}

// openSocket bridges one page socket to a new agent connection.
func (h *host) openSocket(sid string) {
	conn, err := DialAgent(h.options.Socket)
	if err != nil {
		log.Printf("socket %s: cannot reach the agent: %v", sid, err)
		h.deliver(map[string]any{"sid": sid, "event": "error", "error": errAgentNotAvailable.Error()})
		h.deliver(map[string]any{"sid": sid, "event": "close", "code": 1006, "reason": errAgentNotAvailable.Error()})
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = conn.Close()
		return
	}
	h.sockets[sid] = conn
	h.mu.Unlock()
	_ = conn.WriteJSON(map[string]any{"type": "native-host-hello", "version": 1, "storeId": h.options.StoreID, "purpose": "app"})
	ready := false
	for {
		kind, payload, err := conn.Read()
		if err != nil {
			h.endSocket(sid, 1006, "The DynApp agent connection closed")
			return
		}
		// The agent writes JSON with sorted keys, so "type" is rarely first.
		if kind == frameText && (!ready || strings.Contains(string(payload), `"type":"native-host-`)) {
			var frame map[string]any
			if json.Unmarshal(payload, &frame) == nil {
				if name, _ := frame["type"].(string); strings.HasPrefix(name, "native-host-") {
					switch name {
					case "native-host-ready":
						ready = true
						h.deliver(map[string]any{"sid": sid, "event": "open"})
					case "native-host-permission-request":
						h.queuePrompt(frame, conn)
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
	_ = conn.Close()
	if code == 0 {
		code = 1000
	}
	h.deliver(map[string]any{"sid": sid, "event": "close", "code": code, "reason": reason})
}

func (h *host) closeAll() {
	h.wt.Shutdown()
	h.mu.Lock()
	h.closed = true
	sockets := h.sockets
	h.sockets = map[string]*FrameConn{}
	control := h.control
	h.control = nil
	h.mu.Unlock()
	for _, conn := range sockets {
		_ = conn.Close()
	}
	if control != nil {
		_ = control.Close()
	}
}

// deliver calls the bridge's receiver in the page. Messages keep their order
// because GLib runs idle callbacks first in, first out.
func (h *host) deliver(message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	script := "window.__dynappNativeHostDeliver && window.__dynappNativeHostDeliver(" + string(data) + ")"
	dispatch(func() { withCString(script, func(value *C.char) { C.host_eval(value) }) })
}

// deliverWebTransport calls the WebTransport stand-in's receiver in the page.
func (h *host) deliverWebTransport(message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	script := "window.__dynappWebTransportDeliver && window.__dynappWebTransportDeliver(" + string(data) + ")"
	dispatch(func() { withCString(script, func(value *C.char) { C.host_eval(value) }) })
}

// ---- Permission prompts --------------------------------------------------

// queuePrompt shows trusted permission prompts one at a time, each in its
// own web view outside the app's page.
func (h *host) queuePrompt(frame map[string]any, conn *FrameConn) {
	id, _ := frame["requestId"].(string)
	page, _ := frame["page"].(string)
	h.mu.Lock()
	if id == "" || h.prompts[id] {
		h.mu.Unlock()
		return
	}
	h.prompts[id] = true
	h.mu.Unlock()
	if page == "" {
		// Every agent that serves Linux hosts sends the page; without one
		// nothing is recorded and the request comes back later.
		_ = conn.WriteJSON(map[string]any{"type": "native-host-permission-decision", "requestId": id, "decision": "dismissed", "capabilities": nil, "dontAsk": false})
		return
	}
	dispatch(func() {
		h.promptQueue = append(h.promptQueue, pendingPrompt{id: id, page: page, conn: conn})
		h.showNextPrompt()
	})
}

func (h *host) showNextPrompt() {
	if h.showing != nil || len(h.promptQueue) == 0 {
		return
	}
	next := h.promptQueue[0]
	h.promptQueue = h.promptQueue[1:]
	h.showing = &next
	withCString(next.id, func(id *C.char) {
		withCString(next.page, func(page *C.char) { C.host_show_permission_page(id, page) })
	})
}

func (h *host) promptMessage(id, raw string) {
	if h.showing == nil || h.showing.id != id {
		return
	}
	answer, ok := ParsePromptAnswer(raw)
	if !ok {
		return
	}
	if answer.Decision == "" {
		log.Printf("prompt %s: page height %.0f", id, answer.Resize)
		withCString(id, func(value *C.char) { C.host_resize_permission_page(value, C.int(answer.Resize)) })
		return
	}
	prompt := h.showing
	h.showing = nil
	withCString(id, func(value *C.char) { C.host_close_permission_page(value) })
	reply := map[string]any{"type": "native-host-permission-decision", "requestId": id, "decision": answer.Decision, "dontAsk": answer.DontAsk, "capabilities": nil}
	if answer.Decision == "allow" {
		reply["capabilities"] = answer.Capabilities
	}
	go func() { _ = prompt.conn.WriteJSON(reply) }()
	h.showNextPrompt()
}

// ---- Files and storage ---------------------------------------------------

// queueOpenFiles hands launch or file-manager files to the agent and waits
// for its acknowledgement.
func queueOpenFiles(conn *FrameConn, paths []string) error {
	if len(paths) > 64 {
		return errors.New("too many files to open")
	}
	files := make([]string, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		files = append(files, absolute)
	}
	if conn == nil {
		return errAgentNotAvailable
	}
	// The control reader reports native-host-open-files-result errors.
	return conn.WriteJSON(map[string]any{"type": "native-host-open-files", "id": "launch-files", "paths": files})
}

// webDataDirs keeps each app's web storage separate, next to its icon.
func webDataDirs(options Options) (string, string) {
	base := filepath.Dir(options.Icon)
	if options.Icon == "" || base == "." {
		data := os.Getenv("XDG_DATA_HOME")
		if !filepath.IsAbs(data) {
			home, _ := os.UserHomeDir()
			data = filepath.Join(home, ".local", "share")
		}
		owner, slug, _ := strings.Cut(options.StoreID, "/")
		base = filepath.Join(data, "dynapp", "native", safeSegment(owner), safeSegment(slug))
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(cache) {
		home, _ := os.UserHomeDir()
		cache = filepath.Join(home, ".cache")
	}
	owner, slug, _ := strings.Cut(options.StoreID, "/")
	return filepath.Join(base, "webkit"), filepath.Join(cache, "dynapp", "native", safeSegment(owner), safeSegment(slug))
}

func safeSegment(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r == '.' || r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			builder.WriteRune(r)
		} else {
			builder.WriteRune('-')
		}
	}
	result := strings.Trim(builder.String(), ".-")
	if result == "" {
		return "app"
	}
	return result
}

// openHostLog writes host.log next to the app's icon; with
// DYNAPP_HOST_DEBUG=1 the log also goes to stderr.
func openHostLog(options Options) func() {
	dir, _ := webDataDirs(options)
	dir = filepath.Dir(dir)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "host.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		_ = os.Remove(path)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}
	}
	if os.Getenv("DYNAPP_HOST_DEBUG") == "1" {
		log.SetOutput(ioMulti{file, os.Stderr})
	} else {
		log.SetOutput(file)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return func() { _ = file.Close() }
}

type ioMulti [2]*os.File

func (m ioMulti) Write(data []byte) (int, error) {
	_, _ = m[1].Write(data)
	return m[0].Write(data)
}
