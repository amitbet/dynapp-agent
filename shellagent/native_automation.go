package shellagent

// Native app automation (docs/native-automation.md): a coding agent on this
// computer drives an installed native app through the agent. The agent listens
// on a user-only local socket, checks that the user allowed automation for the
// app, and forwards each command to the app's host over its control channel.
// Nothing here is reachable from web pages, Dyner, or other machines.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

const (
	automationFeature        = "automation"
	automationConsoleSize    = 2000
	automationDefaultWait    = 30 * time.Second
	automationMaxWait        = 10 * time.Minute
	automationConsentWait    = 3 * time.Minute
	automationMaxRequest     = 4 * 1024 * 1024
	automationConsoleTextMax = 8000
)

// nativeAutomation is the agent-side state: one control channel per running
// app, console history, and console followers.
type nativeAutomation struct {
	mu        sync.Mutex
	listener  net.Listener
	controls  map[string]*automationControl
	console   map[string]*automationConsole
	sequence  atomic.Uint64
	listening bool
}

type automationControl struct {
	socket    *nativeFrameSocket
	supported bool
	connected time.Time
	mu        sync.Mutex
	pending   map[string]chan automationReply
	closed    bool
}

type automationReply struct {
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

type automationConsoleEntry struct {
	Seq    uint64 `json:"seq"`
	Time   int64  `json:"time"`
	Level  string `json:"level"`
	Source string `json:"source,omitempty"`
	Text   string `json:"text"`
}

type automationConsole struct {
	entries   []automationConsoleEntry
	next      uint64
	followers map[chan automationConsoleEntry]struct{}
}

func automationEndpoint(stateDir string) string {
	if platform := nativePlatformName(); platform != "macos" && platform != "windows" {
		return ""
	}
	native := nativeEndpoint(stateDir)
	index := strings.LastIndex(native, "native-host")
	if index < 0 {
		return ""
	}
	return native[:index] + "automation" + native[index+len("native-host"):]
}

// startAutomation opens the automation socket. Like the native endpoint, a
// failure is logged and never stops the agent.
func (s *Server) startAutomation() {
	endpoint := automationEndpoint(s.StateDir)
	if endpoint == "" {
		return
	}
	listener, err := nativeListen(endpoint)
	if err != nil {
		log.Printf("DynApp Shell agent: native automation endpoint unavailable: %v", err)
		return
	}
	s.automation.mu.Lock()
	s.automation.listener = listener
	s.automation.mu.Unlock()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			go s.serveAutomationClient(conn)
		}
	}()
}

func (s *Server) stopAutomation() {
	s.automation.mu.Lock()
	listener := s.automation.listener
	s.automation.listener = nil
	s.automation.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
}

func (s *Server) registerAutomationControl(storeID string, socket *nativeFrameSocket, features []string) *automationControl {
	control := &automationControl{socket: socket, connected: time.Now(), pending: map[string]chan automationReply{}}
	for _, feature := range features {
		if feature == automationFeature {
			control.supported = true
		}
	}
	s.automation.mu.Lock()
	if s.automation.controls == nil {
		s.automation.controls = map[string]*automationControl{}
	}
	s.automation.controls[storeID] = control
	s.automation.mu.Unlock()
	return control
}

func (s *Server) unregisterAutomationControl(storeID string, control *automationControl) {
	s.automation.mu.Lock()
	if s.automation.controls[storeID] == control {
		delete(s.automation.controls, storeID)
	}
	s.automation.mu.Unlock()
	control.mu.Lock()
	control.closed = true
	pending := control.pending
	control.pending = map[string]chan automationReply{}
	control.mu.Unlock()
	for _, reply := range pending {
		reply <- automationReply{Error: "the app closed"}
	}
}

func (s *Server) automationControl(storeID string) *automationControl {
	s.automation.mu.Lock()
	defer s.automation.mu.Unlock()
	return s.automation.controls[storeID]
}

// handleAutomationFrame consumes host frames that belong to automation and
// reports whether it did.
func (s *Server) handleAutomationFrame(storeID, kind string, data []byte) bool {
	switch kind {
	case "native-host-automation-result":
		var frame struct {
			ID    string          `json:"id"`
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		}
		if json.Unmarshal(data, &frame) != nil {
			return true
		}
		if control := s.automationControl(storeID); control != nil {
			control.mu.Lock()
			reply := control.pending[frame.ID]
			delete(control.pending, frame.ID)
			control.mu.Unlock()
			if reply != nil {
				reply <- automationReply{Value: frame.Value, Error: frame.Error}
			}
		}
		return true
	case "native-host-console":
		var frame struct {
			Entries []struct {
				Time   float64 `json:"time"`
				Level  string  `json:"level"`
				Source string  `json:"source"`
				Text   string  `json:"text"`
			} `json:"entries"`
		}
		if json.Unmarshal(data, &frame) == nil {
			if app, ok := s.nativeApp(storeID); ok && app.Automation {
				entries := make([]automationConsoleEntry, 0, len(frame.Entries))
				for _, entry := range frame.Entries {
					entries = append(entries, automationConsoleEntry{Time: int64(entry.Time), Level: entry.Level, Source: entry.Source, Text: entry.Text})
				}
				s.appendAutomationConsole(storeID, entries)
			}
		}
		return true
	case "native-host-automation-stop":
		// The user chose "Stop Automation" in the app's own menu.
		_ = s.setNativeAutomation(storeID, false)
		return true
	}
	return false
}

func (s *Server) appendAutomationConsole(storeID string, entries []automationConsoleEntry) {
	s.automation.mu.Lock()
	defer s.automation.mu.Unlock()
	if s.automation.console == nil {
		s.automation.console = map[string]*automationConsole{}
	}
	history := s.automation.console[storeID]
	if history == nil {
		history = &automationConsole{followers: map[chan automationConsoleEntry]struct{}{}}
		s.automation.console[storeID] = history
	}
	if len(entries) > 200 {
		entries = entries[:200]
	}
	for _, entry := range entries {
		history.next++
		entry.Seq = history.next
		if entry.Time == 0 {
			entry.Time = time.Now().UnixMilli()
		}
		entry.Level = strings.ToLower(strings.TrimSpace(entry.Level))
		if entry.Level == "" {
			entry.Level = "log"
		}
		if len(entry.Text) > automationConsoleTextMax {
			entry.Text = entry.Text[:automationConsoleTextMax] + "…"
		}
		history.entries = append(history.entries, entry)
		for follower := range history.followers {
			select {
			case follower <- entry:
			default: // a slow follower misses entries rather than stalling the app
			}
		}
	}
	if extra := len(history.entries) - automationConsoleSize; extra > 0 {
		history.entries = append([]automationConsoleEntry(nil), history.entries[extra:]...)
	}
}

func (s *Server) automationConsoleSince(storeID string, since uint64, limit int) ([]automationConsoleEntry, uint64) {
	s.automation.mu.Lock()
	defer s.automation.mu.Unlock()
	history := s.automation.console[storeID]
	if history == nil {
		return []automationConsoleEntry{}, 0
	}
	result := []automationConsoleEntry{}
	for _, entry := range history.entries {
		if entry.Seq > since {
			result = append(result, entry)
		}
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result, history.next
}

func (s *Server) followAutomationConsole(storeID string) (chan automationConsoleEntry, func()) {
	channel := make(chan automationConsoleEntry, 256)
	s.automation.mu.Lock()
	if s.automation.console == nil {
		s.automation.console = map[string]*automationConsole{}
	}
	history := s.automation.console[storeID]
	if history == nil {
		history = &automationConsole{followers: map[chan automationConsoleEntry]struct{}{}}
		s.automation.console[storeID] = history
	}
	history.followers[channel] = struct{}{}
	s.automation.mu.Unlock()
	return channel, func() {
		s.automation.mu.Lock()
		delete(history.followers, channel)
		s.automation.mu.Unlock()
	}
}

// callHost sends one automation op to the app's host and waits for its reply.
func (s *Server) callHost(ctx context.Context, storeID, op string, args map[string]any, timeout time.Duration) (json.RawMessage, error) {
	control := s.automationControl(storeID)
	if control == nil {
		return nil, errors.New("the app is not running; start it with the launch command")
	}
	if !control.supported {
		return nil, errors.New("this app's installed copy predates automation; install it again from Dyner")
	}
	id := fmt.Sprintf("a%d", s.automation.sequence.Add(1))
	reply := make(chan automationReply, 1)
	control.mu.Lock()
	if control.closed {
		control.mu.Unlock()
		return nil, errors.New("the app closed")
	}
	control.pending[id] = reply
	control.mu.Unlock()
	frame := map[string]any{}
	for key, value := range args {
		frame[key] = value
	}
	frame["type"], frame["id"], frame["op"] = "native-host-automation", id, op
	if err := control.socket.Write(ctx, websocket.MessageText, mustJSON(frame)); err != nil {
		control.mu.Lock()
		delete(control.pending, id)
		control.mu.Unlock()
		return nil, fmt.Errorf("the app did not take the command: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-reply:
		if result.Error != "" {
			return nil, errors.New(result.Error)
		}
		return result.Value, nil
	case <-timer.C:
	case <-ctx.Done():
	}
	control.mu.Lock()
	delete(control.pending, id)
	control.mu.Unlock()
	return nil, fmt.Errorf("the app did not answer %s within %s", op, timeout)
}

// setNativeAutomation stores the user's choice and tells a running host.
func (s *Server) setNativeAutomation(storeID string, enabled bool) error {
	app, ok := s.nativeApp(storeID)
	if !ok {
		return errors.New("this app is not installed as a native app")
	}
	if app.Automation != enabled {
		app.Automation = enabled
		s.upsertNativeApp(app)
	}
	if control := s.automationControl(storeID); control != nil && control.supported {
		_ = control.socket.Write(context.Background(), websocket.MessageText, mustJSON(map[string]any{
			"type": "native-host-automation-state", "enabled": enabled,
		}))
	}
	if !enabled {
		s.automation.mu.Lock()
		delete(s.automation.console, storeID)
		s.automation.mu.Unlock()
	}
	return nil
}

// resolveAutomationApp accepts a store id ("owner/slug"), a slug, or an
// app name, as long as it names exactly one installed native app.
func (s *Server) resolveAutomationApp(name string) (NativeApp, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return NativeApp{}, errors.New("name the app, for example fastlane or amit-bet/fastlane")
	}
	s.mu.Lock()
	apps := append([]NativeApp(nil), s.Config.NativeApps...)
	s.mu.Unlock()
	var matches []NativeApp
	for _, app := range apps {
		if app.StoreID == name {
			return app, nil
		}
		_, slug, _ := strings.Cut(app.StoreID, "/")
		if strings.EqualFold(slug, name) || strings.EqualFold(app.Name, name) {
			matches = append(matches, app)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return NativeApp{}, fmt.Errorf("no installed native app is called %q; run the apps command to list them", name)
	default:
		ids := make([]string, 0, len(matches))
		for _, app := range matches {
			ids = append(ids, app.StoreID)
		}
		return NativeApp{}, fmt.Errorf("%q matches %s; use the full store id", name, strings.Join(ids, ", "))
	}
}

// automationRequest is one line on the automation socket.
type automationRequest struct {
	ID         any      `json:"id"`
	Command    string   `json:"cmd"`
	App        string   `json:"app"`
	Code       string   `json:"code"`
	Selector   string   `json:"selector"`
	Expression string   `json:"expression"`
	URL        string   `json:"url"`
	Text       string   `json:"text"`
	Key        string   `json:"key"`
	Modifiers  []string `json:"modifiers"`
	X          *float64 `json:"x"`
	Y          *float64 `json:"y"`
	Width      *float64 `json:"width"`
	Height     *float64 `json:"height"`
	Count      int      `json:"count"`
	Window     bool     `json:"window"`
	Focus      bool     `json:"focus"`
	Since      uint64   `json:"since"`
	Limit      int      `json:"limit"`
	Follow     bool     `json:"follow"`
	Wait       bool     `json:"wait"`
	TimeoutMS  int      `json:"timeoutMs"`
}

func (request automationRequest) timeout(fallback time.Duration) time.Duration {
	if request.TimeoutMS <= 0 {
		return fallback
	}
	wait := time.Duration(request.TimeoutMS) * time.Millisecond
	if wait > automationMaxWait {
		return automationMaxWait
	}
	return wait
}

type automationWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (writer *automationWriter) send(value any) error {
	data, err := marshalPlain(value, "")
	if err != nil {
		return err
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_, err = writer.conn.Write(data)
	return err
}

// marshalPlain encodes JSON without HTML escaping (page values often hold
// markup) and ends it with a newline.
func marshalPlain(value any, indent string) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(buffer.String()), nil
}

func (s *Server) serveAutomationClient(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &automationWriter{conn: conn}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), automationMaxRequest)
	var running sync.WaitGroup
	defer running.Wait()
	for scanner.Scan() {
		var request automationRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			_ = writer.send(map[string]any{"ok": false, "error": "the request is not valid JSON"})
			continue
		}
		running.Add(1)
		go func() {
			defer running.Done()
			result, err := s.runAutomation(ctx, request, func(entry automationConsoleEntry) error {
				return writer.send(map[string]any{"id": request.ID, "event": entry})
			})
			if err != nil {
				_ = writer.send(map[string]any{"id": request.ID, "ok": false, "error": err.Error()})
				return
			}
			_ = writer.send(map[string]any{"id": request.ID, "ok": true, "result": result})
		}()
	}
	cancel()
}

func (s *Server) runAutomation(ctx context.Context, request automationRequest, stream func(automationConsoleEntry) error) (any, error) {
	if request.Command == "apps" {
		return s.automationApps(), nil
	}
	app, err := s.resolveAutomationApp(request.App)
	if err != nil {
		return nil, err
	}
	switch request.Command {
	case "launch":
		return s.automationLaunch(ctx, app, request)
	case "enable":
		return s.automationEnable(ctx, app)
	case "disable":
		if err := s.setNativeAutomation(app.StoreID, false); err != nil {
			return nil, err
		}
		return map[string]any{"storeId": app.StoreID, "automation": false}, nil
	}
	if !app.Automation {
		return nil, fmt.Errorf("automation is off for %s; run the enable command and allow it in the app's window", app.StoreID)
	}
	switch request.Command {
	case "eval":
		return s.automationEval(ctx, app, request.Code, request.timeout(automationDefaultWait))
	case "wait":
		return s.automationWaitFor(ctx, app, request)
	case "console":
		return s.automationConsoleCommand(ctx, app, request, stream)
	case "snapshot":
		args := map[string]any{"window": request.Window}
		if request.Width != nil && *request.Width > 0 {
			args["width"] = *request.Width
		}
		return rawResult(s.callHost(ctx, app.StoreID, "snapshot", args, request.timeout(automationDefaultWait)))
	case "reload":
		return rawResult(s.callHost(ctx, app.StoreID, "reload", nil, automationDefaultWait))
	case "navigate":
		target, err := automationNavigationURL(app, request.URL)
		if err != nil {
			return nil, err
		}
		return rawResult(s.callHost(ctx, app.StoreID, "navigate", map[string]any{"url": target}, automationDefaultWait))
	case "click":
		return s.automationClick(ctx, app, request)
	case "type":
		if request.Selector != "" {
			if _, err := s.automationFocus(ctx, app, request.Selector); err != nil {
				return nil, err
			}
		}
		if request.Text == "" {
			return nil, errors.New("give the text to type")
		}
		return rawResult(s.callHost(ctx, app.StoreID, "type", map[string]any{"text": request.Text}, automationDefaultWait))
	case "key":
		if strings.TrimSpace(request.Key) == "" {
			return nil, errors.New("give a key, for example Enter, Escape, ArrowDown or a")
		}
		return rawResult(s.callHost(ctx, app.StoreID, "key", map[string]any{"key": request.Key, "modifiers": normalizeModifiers(request.Modifiers)}, automationDefaultWait))
	case "window":
		args := map[string]any{"focus": request.Focus}
		for key, value := range map[string]*float64{"x": request.X, "y": request.Y, "width": request.Width, "height": request.Height} {
			if value != nil {
				args[key] = *value
			}
		}
		return rawResult(s.callHost(ctx, app.StoreID, "window", args, automationDefaultWait))
	default:
		return nil, fmt.Errorf("unknown command %q", request.Command)
	}
}

func rawResult(value json.RawMessage, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return map[string]any{}, nil
	}
	return value, nil
}

func (s *Server) automationApps() []map[string]any {
	s.mu.Lock()
	apps := append([]NativeApp(nil), s.Config.NativeApps...)
	s.mu.Unlock()
	sort.Slice(apps, func(i, j int) bool { return apps[i].StoreID < apps[j].StoreID })
	result := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		entry := map[string]any{"storeId": app.StoreID, "name": app.Name, "origin": app.Origin, "automation": app.Automation, "running": false}
		if control := s.automationControl(app.StoreID); control != nil {
			entry["running"] = true
			entry["automationSupported"] = control.supported
		}
		result = append(result, entry)
	}
	return result
}

func (s *Server) automationLaunch(ctx context.Context, app NativeApp, request automationRequest) (any, error) {
	if s.automationControl(app.StoreID) == nil {
		app, _ = s.upgradeNativeHost(app)
		if err := nativeLaunch(app, s.nativeEndpoint()); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(request.timeout(20 * time.Second))
	for time.Now().Before(deadline) {
		if control := s.automationControl(app.StoreID); control != nil {
			return map[string]any{"storeId": app.StoreID, "running": true, "automationSupported": control.supported, "automation": app.Automation}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil, errors.New("the app did not connect to the agent in time")
}

// automationEnable asks the user in the app's own window. A coding agent can
// start the request but cannot answer it.
func (s *Server) automationEnable(ctx context.Context, app NativeApp) (any, error) {
	if app.Automation {
		return map[string]any{"storeId": app.StoreID, "automation": true}, nil
	}
	value, err := s.callHost(ctx, app.StoreID, "consent", map[string]any{"appName": app.Name}, automationConsentWait)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Approved bool `json:"approved"`
	}
	_ = json.Unmarshal(value, &answer)
	if !answer.Approved {
		return nil, errors.New("the user did not allow automation for " + app.Name)
	}
	if err := s.setNativeAutomation(app.StoreID, true); err != nil {
		return nil, err
	}
	return map[string]any{"storeId": app.StoreID, "automation": true}, nil
}

func (s *Server) automationConsoleCommand(ctx context.Context, app NativeApp, request automationRequest, stream func(automationConsoleEntry) error) (any, error) {
	if !request.Follow {
		entries, cursor := s.automationConsoleSince(app.StoreID, request.Since, request.Limit)
		return map[string]any{"entries": entries, "cursor": cursor}, nil
	}
	channel, stop := s.followAutomationConsole(app.StoreID)
	defer stop()
	entries, cursor := s.automationConsoleSince(app.StoreID, request.Since, request.Limit)
	for _, entry := range entries {
		if err := stream(entry); err != nil {
			return nil, err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return map[string]any{"cursor": cursor}, nil
		case entry := <-channel:
			if entry.Seq <= cursor {
				continue
			}
			cursor = entry.Seq
			if err := stream(entry); err != nil {
				return nil, err
			}
		}
	}
}

// automationNavigationURL keeps navigation inside the app: the host would
// open any other origin in the default browser.
func automationNavigationURL(app NativeApp, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("give a URL or a path such as /#/settings")
	}
	base, err := url.Parse(app.Origin + "/")
	if err != nil {
		return "", err
	}
	if current, err := url.Parse(app.URL); err == nil && current.Host == base.Host {
		base = current
	}
	target, err := base.Parse(raw)
	if err != nil {
		return "", err
	}
	if originOf(target) != strings.ToLower(strings.TrimRight(app.Origin, "/")) {
		return "", fmt.Errorf("automation can only navigate inside %s", app.Origin)
	}
	return target.String(), nil
}

func originOf(target *url.URL) string {
	scheme, host := strings.ToLower(target.Scheme), strings.ToLower(target.Host)
	if (scheme == "https" && strings.HasSuffix(host, ":443")) || (scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return scheme + "://" + host
}

func normalizeModifiers(values []string) []string {
	result := []string{}
	for _, value := range values {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '+' || r == ' ' }) {
			switch strings.ToLower(part) {
			case "cmd", "command", "meta", "super", "win":
				result = append(result, "meta")
			case "ctrl", "control":
				result = append(result, "control")
			case "alt", "option", "opt":
				result = append(result, "alt")
			case "shift":
				result = append(result, "shift")
			}
		}
	}
	return result
}

// MARK: scripts

// automationScriptValue serializes a page value without throwing on DOM
// nodes, cycles, or huge collections. The body of the wrapper below.
const automationSerializer = `const __dynappSeen = new WeakSet();
const __dynappDescribe = (value, depth) => {
  if (value === null || typeof value === "boolean" || typeof value === "string") return value;
  if (typeof value === "number") return Number.isFinite(value) ? value : String(value);
  if (typeof value === "bigint") return value.toString() + "n";
  if (typeof value === "undefined") return null;
  if (typeof value === "function") return "[Function " + (value.name || "anonymous") + "]";
  if (typeof value === "symbol") return value.toString();
  if (typeof Element !== "undefined" && value instanceof Element) {
    const id = value.id ? "#" + value.id : "";
    const classes = typeof value.className === "string" && value.className.trim() ? "." + value.className.trim().split(/\s+/).join(".") : "";
    const text = (value.textContent || "").replace(/\s+/g, " ").trim().slice(0, 80);
    return "<" + value.tagName.toLowerCase() + id + classes + ">" + (text ? " " + JSON.stringify(text) : "");
  }
  if (typeof Node !== "undefined" && value instanceof Node) return "[" + value.nodeName + "]";
  if (value instanceof Error) return { name: value.name, message: value.message, stack: String(value.stack || "") };
  if (value instanceof Date) return value.toISOString();
  if (value instanceof RegExp) return String(value);
  if (depth > 8) return "[…]";
  if (__dynappSeen.has(value)) return "[Circular]";
  __dynappSeen.add(value);
  if (typeof value.toJSON === "function") { try { return __dynappDescribe(value.toJSON(), depth + 1); } catch {} }
  if (value instanceof Map) return { "[Map]": Array.from(value.entries()).slice(0, 500).map(([k, v]) => [__dynappDescribe(k, depth + 1), __dynappDescribe(v, depth + 1)]) };
  if (value instanceof Set || Array.isArray(value) || (typeof NodeList !== "undefined" && value instanceof NodeList) || (typeof HTMLCollection !== "undefined" && value instanceof HTMLCollection) || ArrayBuffer.isView(value)) {
    return Array.from(value).slice(0, 1000).map((item) => __dynappDescribe(item, depth + 1));
  }
  const result = {};
  let count = 0;
  for (const key in value) {
    if (++count > 500) { result["…"] = "more keys"; break; }
    try { result[key] = __dynappDescribe(value[key], depth + 1); } catch (error) { result[key] = "[threw " + String(error) + "]"; }
  }
  return result;
};`

var errAutomationNavigated = errors.New("the page navigated or reloaded before the script finished")

// automationScript builds the async function body a host runs: mark the
// call as started (Windows uses this to pick the fallback), run the code,
// and return a JSON string that never throws.
func automationScript(id, body string) string {
	marker, _ := json.Marshal(id)
	return `(globalThis[Symbol.for("dynapp.automation")] ??= new Set()).add(` + string(marker) + `);
` + automationSerializer + `
try {
  const __dynappValue = await (async () => {
` + body + `
  })();
  const __dynappText = JSON.stringify({ ok: true, undefined: __dynappValue === undefined, value: __dynappDescribe(__dynappValue, 0) });
  return __dynappText.length > 8000000 ? JSON.stringify({ ok: false, error: "the result is larger than 8 MB; return less" }) : __dynappText;
} catch (__dynappError) {
  return JSON.stringify({ ok: false, error: String(__dynappError && __dynappError.message || __dynappError), name: String(__dynappError && __dynappError.name || "Error"), stack: String(__dynappError && __dynappError.stack || "") });
}`
}

// statementStart marks code that is clearly statements. Anything else is
// tried as an expression first; a syntax error there falls back to statements.
var statementStart = regexp.MustCompile(`^(const|let|var|if|for|while|do|switch|try|throw|return|function|class|async\s+function)\b`)

// automationBodies returns the expression form first, and the statement
// form as a fallback for code the expression form cannot parse.
func automationBodies(code string) (string, string) {
	trimmed := strings.TrimSpace(code)
	statements := trimmed
	expression := "return (\n" + strings.TrimRight(trimmed, "; \n\t") + "\n);"
	if statementStart.MatchString(trimmed) {
		return statements, statements
	}
	return expression, statements
}

func (s *Server) automationEval(ctx context.Context, app NativeApp, code string, timeout time.Duration) (any, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("give JavaScript to run, for example document.title")
	}
	id := fmt.Sprintf("e%d", s.automation.sequence.Add(1))
	first, fallback := automationBodies(code)
	args := map[string]any{"script": automationScript(id, first)}
	if fallback != first {
		args["fallback"] = automationScript(id, fallback)
	}
	value, err := s.callHost(ctx, app.StoreID, "eval", args, timeout)
	if err != nil {
		// WebKit drops the completion of a script whose page went away.
		if strings.Contains(err.Error(), "no longer reachable") {
			return nil, errAutomationNavigated
		}
		return nil, err
	}
	return decodeEvalResult(value)
}

// decodeEvalResult turns the wrapper's JSON string into the reply.
func decodeEvalResult(value json.RawMessage) (any, error) {
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, fmt.Errorf("the app returned an unexpected result: %s", truncateText(string(value), 200))
	}
	var result struct {
		OK        bool            `json:"ok"`
		Undefined bool            `json:"undefined"`
		Value     json.RawMessage `json:"value"`
		Error     string          `json:"error"`
		Name      string          `json:"name"`
		Stack     string          `json:"stack"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, errors.New("the app returned a result that is not JSON")
	}
	if !result.OK {
		message := result.Error
		if result.Name != "" && !strings.HasPrefix(message, result.Name) {
			message = result.Name + ": " + message
		}
		if result.Stack != "" {
			message += "\n" + result.Stack
		}
		return nil, errors.New(message)
	}
	if result.Undefined {
		return map[string]any{"value": nil, "undefined": true}, nil
	}
	return map[string]any{"value": result.Value}, nil
}

// automationWaitFor polls inside the page until a selector matches (and is
// visible) or an expression is truthy.
func (s *Server) automationWaitFor(ctx context.Context, app NativeApp, request automationRequest) (any, error) {
	timeout := request.timeout(10 * time.Second)
	condition := ""
	switch {
	case request.Selector != "":
		selector, _ := json.Marshal(request.Selector)
		condition = `(() => { const element = document.querySelector(` + string(selector) + `); if (!element) return false; const box = element.getBoundingClientRect(); return box.width > 0 || box.height > 0 ? element : false; })()`
	case strings.TrimSpace(request.Expression) != "":
		condition = "(" + strings.TrimRight(strings.TrimSpace(request.Expression), "; \n\t") + ")"
	default:
		return nil, errors.New("give a selector or an expression to wait for")
	}
	code := `const __deadline = Date.now() + ` + fmt.Sprint(timeout.Milliseconds()) + `;
for (;;) {
  const __value = await (async () => ` + condition + `)();
  if (__value) return __value;
  if (Date.now() > __deadline) throw new Error("timed out after ` + timeout.String() + `");
  await new Promise((resolve) => setTimeout(resolve, 100));
}`
	// A page load cuts off a script that is still running; wait again on the
	// new page until the deadline.
	deadline := time.Now().Add(timeout)
	for {
		result, err := s.automationEval(ctx, app, code, timeout+5*time.Second)
		if err == nil || !errors.Is(err, errAutomationNavigated) || time.Now().After(deadline) {
			return result, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// automationTarget scrolls an element into view and returns its center in
// viewport CSS pixels.
func (s *Server) automationTarget(ctx context.Context, app NativeApp, selector string, focus bool) (float64, float64, error) {
	quoted, _ := json.Marshal(selector)
	focusCode := ""
	if focus {
		focusCode = "element.focus();"
	}
	result, err := s.automationEval(ctx, app, `const element = document.querySelector(`+string(quoted)+`);
if (!element) throw new Error("no element matches "+`+string(quoted)+`);
element.scrollIntoView({ block: "center", inline: "center" });
`+focusCode+`
const box = element.getBoundingClientRect();
if (box.width === 0 && box.height === 0) throw new Error("the element is not visible");
return { x: box.left + box.width / 2, y: box.top + box.height / 2 };`, automationDefaultWait)
	if err != nil {
		return 0, 0, err
	}
	var point struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	data, _ := json.Marshal(result.(map[string]any)["value"])
	if err := json.Unmarshal(data, &point); err != nil {
		return 0, 0, errors.New("could not locate the element")
	}
	return point.X, point.Y, nil
}

func (s *Server) automationFocus(ctx context.Context, app NativeApp, selector string) (any, error) {
	x, y, err := s.automationTarget(ctx, app, selector, false)
	if err != nil {
		return nil, err
	}
	// A real click gives the element focus the way a user would.
	return rawResult(s.callHost(ctx, app.StoreID, "click", map[string]any{"x": x, "y": y, "count": 1}, automationDefaultWait))
}

func (s *Server) automationClick(ctx context.Context, app NativeApp, request automationRequest) (any, error) {
	var x, y float64
	switch {
	case request.Selector != "":
		var err error
		if x, y, err = s.automationTarget(ctx, app, request.Selector, false); err != nil {
			return nil, err
		}
	case request.X != nil && request.Y != nil:
		x, y = *request.X, *request.Y
	default:
		return nil, errors.New("give a CSS selector or x and y viewport coordinates")
	}
	count := request.Count
	if count < 1 || count > 3 {
		count = 1
	}
	if _, err := s.callHost(ctx, app.StoreID, "click", map[string]any{"x": x, "y": y, "count": count}, automationDefaultWait); err != nil {
		return nil, err
	}
	return map[string]any{"x": x, "y": y, "count": count}, nil
}

// automationDialPath is exposed for the CLI.
func automationSocketPath(stateDir string) (string, error) {
	endpoint := automationEndpoint(stateDir)
	if endpoint == "" {
		return "", errors.New("native app automation is available on macOS and Windows only")
	}
	if !strings.HasPrefix(endpoint, `\\.\pipe\`) {
		endpoint = filepath.Clean(endpoint)
	}
	return endpoint, nil
}
