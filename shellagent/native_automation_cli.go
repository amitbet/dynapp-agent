package shellagent

// Command-line and MCP clients for native app automation
// (docs/native-automation.md). Both talk to the running agent's automation
// socket; neither needs a Dyner login.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const nativeAutomationUsage = `Drive an installed DynApp native app (docs/native-automation.md).

Usage: dynapp-shell-agent native <command> [arguments]

  apps                                   list installed native apps
  launch   <app>                         start the app and wait for it
  enable   <app>                         ask the user, in the app's window, to allow automation
  disable  <app>                         turn automation off
  eval     <app> <code | ->              run JavaScript in the page and print the result
  wait     <app> --selector S | --expr E wait until an element is visible or an expression is truthy
  snapshot <app> [file.png] [--width N] [--window]
                                         save a PNG (1 image pixel = 1 CSS pixel by default)
  console  <app> [--follow] [--since N] [--limit N]
  reload   <app>
  navigate <app> <url | /path | #/route>
  click    <app> <selector> | --x X --y Y [--count 2]
  type     <app> <text> [--selector S]   type with real key events
  key      <app> <key> [--mods cmd,shift] press Enter, Escape, ArrowDown, a, F5, ...
  window   <app> [--width W --height H] [--x X --y Y] [--focus]
  mcp                                    serve these commands as MCP tools over stdio

<app> is a store id (owner/slug), a slug, or the app's name.
Common flags: --timeout 30s, --state-dir DIR.
`

// RunNativeAutomationCLI runs `dynapp-shell-agent native ...` and returns the
// process exit code.
func RunNativeAutomationCLI(args []string, stateDir string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, nativeAutomationUsage)
		return 0
	}
	command, rest := args[0], args[1:]
	options, positional, err := parseAutomationFlags(rest)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if value := options["state-dir"]; value != "" {
		stateDir = value
	}
	client := automationClient{stateDir: stateDir}
	if command == "mcp" {
		if err := client.serveMCP(os.Stdin, stdout); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	request, output, err := automationCLIRequest(command, options, positional)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if request.Command == "console" && request.Follow {
		err = client.follow(request, func(entry automationConsoleEntry) {
			fmt.Fprintln(stdout, formatConsoleEntry(entry))
		})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	result, err := client.call(request)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := printAutomationResult(stdout, request, result, output); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

var automationBoolFlags = map[string]bool{"follow": true, "window": true, "focus": true, "json": true}

// parseAutomationFlags accepts flags before or after positional arguments.
func parseAutomationFlags(args []string) (map[string]string, []string, error) {
	options := map[string]string{}
	positional := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if automationBoolFlags[name] {
			if !hasValue {
				value = "true"
			}
			options[name] = value
			continue
		}
		if !hasValue {
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("--%s needs a value", name)
			}
			index++
			value = args[index]
		}
		options[name] = value
	}
	return options, positional, nil
}

func automationCLIRequest(command string, options map[string]string, positional []string) (automationRequest, string, error) {
	request := automationRequest{Command: command}
	if command != "apps" {
		if len(positional) == 0 {
			return request, "", fmt.Errorf("%s needs an app; run `dynapp-shell-agent native apps`", command)
		}
		request.App, positional = positional[0], positional[1:]
	}
	if value := options["timeout"]; value != "" {
		wait, err := time.ParseDuration(value)
		if err != nil {
			return request, "", fmt.Errorf("--timeout: %w", err)
		}
		request.TimeoutMS = int(wait.Milliseconds())
	}
	number := func(name string) (*float64, error) {
		value, ok := options[name]
		if !ok {
			return nil, nil
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("--%s must be a number", name)
		}
		return &parsed, nil
	}
	var err error
	if request.X, err = number("x"); err != nil {
		return request, "", err
	}
	if request.Y, err = number("y"); err != nil {
		return request, "", err
	}
	if request.Width, err = number("width"); err != nil {
		return request, "", err
	}
	if request.Height, err = number("height"); err != nil {
		return request, "", err
	}
	request.Window = options["window"] == "true"
	request.Focus = options["focus"] == "true"
	request.Follow = options["follow"] == "true"
	request.Selector = options["selector"]
	request.Expression = options["expr"]
	if value := options["since"]; value != "" {
		request.Since, _ = strconv.ParseUint(value, 10, 64)
	}
	if value := options["limit"]; value != "" {
		request.Limit, _ = strconv.Atoi(value)
	}
	if value := options["count"]; value != "" {
		request.Count, _ = strconv.Atoi(value)
	}
	if value := options["mods"]; value != "" {
		request.Modifiers = []string{value}
	}
	output := ""
	switch command {
	case "apps", "launch", "enable", "disable", "reload", "window", "console", "wait":
	case "eval":
		if len(positional) == 0 {
			return request, "", errors.New("eval needs JavaScript, or - to read it from stdin")
		}
		code := strings.Join(positional, " ")
		if code == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return request, "", err
			}
			code = string(data)
		}
		request.Code = code
	case "snapshot":
		if len(positional) > 0 {
			output = positional[0]
		}
	case "navigate":
		if len(positional) == 0 {
			return request, "", errors.New("navigate needs a URL or path")
		}
		request.URL = positional[0]
	case "click":
		if len(positional) > 0 {
			request.Selector = strings.Join(positional, " ")
		}
	case "type":
		request.Text = strings.Join(positional, " ")
	case "key":
		if len(positional) == 0 {
			return request, "", errors.New("key needs a key name")
		}
		request.Key = positional[0]
	default:
		return request, "", fmt.Errorf("unknown command %q\n\n%s", command, nativeAutomationUsage)
	}
	return request, output, nil
}

func printAutomationResult(stdout io.Writer, request automationRequest, result json.RawMessage, output string) error {
	switch request.Command {
	case "eval", "wait":
		var reply struct {
			Value     json.RawMessage `json:"value"`
			Undefined bool            `json:"undefined"`
		}
		_ = json.Unmarshal(result, &reply)
		if reply.Undefined {
			fmt.Fprintln(stdout, "undefined")
			return nil
		}
		var text string
		if json.Unmarshal(reply.Value, &text) == nil {
			fmt.Fprintln(stdout, text)
			return nil
		}
		return printJSON(stdout, reply.Value)
	case "snapshot":
		path, info, err := saveSnapshot(result, output, request.App)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s (%vx%v)\n", path, info["width"], info["height"])
		return nil
	case "console":
		var reply struct {
			Entries []automationConsoleEntry `json:"entries"`
			Cursor  uint64                   `json:"cursor"`
		}
		if err := json.Unmarshal(result, &reply); err != nil {
			return err
		}
		for _, entry := range reply.Entries {
			fmt.Fprintln(stdout, formatConsoleEntry(entry))
		}
		fmt.Fprintf(stdout, "-- cursor %d (pass --since %d for newer entries)\n", reply.Cursor, reply.Cursor)
		return nil
	default:
		return printJSON(stdout, result)
	}
}

func printJSON(stdout io.Writer, value json.RawMessage) error {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		_, err = fmt.Fprintln(stdout, string(value))
		return err
	}
	data, err := marshalPlain(decoded, "  ")
	if err != nil {
		return err
	}
	_, err = stdout.Write(data)
	return err
}

func formatConsoleEntry(entry automationConsoleEntry) string {
	stamp := time.UnixMilli(entry.Time).Format("15:04:05.000")
	level := entry.Level
	if entry.Source != "" && entry.Source != "console" {
		level += "/" + entry.Source
	}
	return fmt.Sprintf("%s [%s] %s", stamp, level, entry.Text)
}

func saveSnapshot(result json.RawMessage, output, app string) (string, map[string]any, error) {
	var info map[string]any
	if err := json.Unmarshal(result, &info); err != nil {
		return "", nil, err
	}
	encoded, _ := info["png"].(string)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return "", nil, errors.New("the app returned no image")
	}
	if output == "" {
		slug := app
		if _, after, ok := strings.Cut(app, "/"); ok {
			slug = after
		}
		output = filepath.Join(os.TempDir(), fmt.Sprintf("dynapp-%s-%s.png", slug, time.Now().Format("150405.000")))
	}
	if err := os.WriteFile(output, data, 0o600); err != nil {
		return "", nil, err
	}
	delete(info, "png")
	absolute, _ := filepath.Abs(output)
	return absolute, info, nil
}

// automationClient sends requests to the agent's automation socket.
type automationClient struct {
	stateDir string
}

func (client automationClient) dial() (net.Conn, error) {
	endpoint, err := automationSocketPath(client.stateDir)
	if err != nil {
		return nil, err
	}
	conn, err := dialAutomation(endpoint)
	if err != nil {
		return nil, fmt.Errorf("the DynApp agent is not running or predates automation (%s): %w", endpoint, err)
	}
	return conn, nil
}

type automationResponse struct {
	OK     bool                    `json:"ok"`
	Result json.RawMessage         `json:"result"`
	Error  string                  `json:"error"`
	Event  *automationConsoleEntry `json:"event"`
}

func (client automationClient) call(request automationRequest) (json.RawMessage, error) {
	conn, err := client.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	request.ID = 1
	if err := writeAutomationLine(conn, request); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, errors.New("the agent closed the connection")
		}
		var response automationResponse
		if err := json.Unmarshal(line, &response); err != nil {
			return nil, err
		}
		if response.Event != nil {
			continue
		}
		if !response.OK {
			return nil, errors.New(response.Error)
		}
		return response.Result, nil
	}
}

func (client automationClient) follow(request automationRequest, onEntry func(automationConsoleEntry)) error {
	conn, err := client.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	go func() {
		<-interrupt
		_ = conn.Close()
	}()
	request.ID = 1
	if err := writeAutomationLine(conn, request); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil
		}
		var response automationResponse
		if err := json.Unmarshal(line, &response); err != nil {
			return err
		}
		if response.Event != nil {
			onEntry(*response.Event)
			continue
		}
		if !response.OK {
			return errors.New(response.Error)
		}
		return nil
	}
}

func writeAutomationLine(conn net.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(data, '\n'))
	return err
}

// MARK: MCP

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpSchema(required []string, properties map[string]any) map[string]any {
	properties["app"] = map[string]any{"type": "string", "description": "Installed app: store id (owner/slug), slug, or name"}
	return map[string]any{"type": "object", "properties": properties, "required": append([]string{"app"}, required...)}
}

func automationMCPTools() []mcpTool {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	num := func(description string) map[string]any {
		return map[string]any{"type": "number", "description": description}
	}
	boolean := func(description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description}
	}
	return []mcpTool{
		{"apps", "List DynApp apps installed as native desktop apps, whether each is running, and whether automation is allowed.", map[string]any{"type": "object", "properties": map[string]any{}}},
		{"launch", "Start a native app (if it is not running) and wait until it is connected.", mcpSchema(nil, map[string]any{})},
		{"enable", "Ask the user, in the app's own window, to allow automation. Blocks until they answer (up to 3 minutes). Required once per install before any other tool.", mcpSchema(nil, map[string]any{})},
		{"eval", "Run JavaScript in the app's page (main world, so app globals and dev hooks are visible) and return the result as JSON. Pass an expression, or statements that use return. await works. Not limited by the page's CSP.", mcpSchema([]string{"code"}, map[string]any{"code": str("JavaScript expression or function body"), "timeoutMs": num("Default 30000")})},
		{"wait", "Wait until a CSS selector matches a visible element, or a JavaScript expression is truthy.", mcpSchema(nil, map[string]any{"selector": str("CSS selector"), "expression": str("JavaScript expression"), "timeoutMs": num("Default 10000")})},
		{"snapshot", "Screenshot the app's page. By default one image pixel is one CSS pixel, so positions in the image can be passed to click.", mcpSchema(nil, map[string]any{"width": num("Output width in pixels"), "window": boolean("Include the window title bar")})},
		{"console", "Read console output and uncaught errors captured since automation was enabled. Pass the returned cursor as since to get only newer entries.", mcpSchema(nil, map[string]any{"since": num("Cursor from a previous call"), "limit": num("Most recent N entries")})},
		{"click", "Click with a real (trusted) mouse event, at a CSS selector's center or at viewport coordinates.", mcpSchema(nil, map[string]any{"selector": str("CSS selector"), "x": num("Viewport x in CSS pixels"), "y": num("Viewport y in CSS pixels"), "count": num("2 for a double click")})},
		{"type", "Type text with real key events into the focused element, or click selector first.", mcpSchema([]string{"text"}, map[string]any{"text": str("Text to type"), "selector": str("Element to click before typing")})},
		{"key", "Press one key, optionally with modifiers. Keys: Enter, Tab, Escape, Backspace, Delete, Space, ArrowUp/Down/Left/Right, Home, End, PageUp, PageDown, F1-F12, or a character.", mcpSchema([]string{"key"}, map[string]any{"key": str("Key name"), "modifiers": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"meta", "control", "alt", "shift"}}}})},
		{"reload", "Reload the app's page.", mcpSchema(nil, map[string]any{})},
		{"navigate", "Navigate the page to a URL or path inside the app's origin.", mcpSchema([]string{"url"}, map[string]any{"url": str("Absolute URL on the app origin, or a path such as /#/settings")})},
		{"window", "Read the window's position, size, viewport, URL and focus state; optionally resize (viewport width/height), move (top-left x/y on screen) or focus it.", mcpSchema(nil, map[string]any{"width": num("Viewport width"), "height": num("Viewport height"), "x": num("Screen x"), "y": num("Screen y from the top"), "focus": boolean("Bring the window to the front")})},
		{"disable", "Turn automation off for the app.", mcpSchema(nil, map[string]any{})},
	}
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (client automationClient) serveMCP(input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	write := func(value any) {
		data, _ := marshalPlain(value, "")
		_, _ = output.Write(data)
	}
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var message mcpMessage
			if json.Unmarshal(line, &message) != nil {
				write(map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
			} else if len(message.ID) > 0 {
				result, rpcErr := client.handleMCP(message)
				if rpcErr != nil {
					write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": rpcErr})
				} else {
					write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (client automationClient) handleMCP(message mcpMessage) (any, map[string]any) {
	switch message.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(message.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "dynapp-native", "version": AgentVersion},
			"instructions": "Drive DynApp apps installed as native desktop apps (WKWebView on macOS, WebView2 on Windows). " +
				"Call apps first, launch the app if it is not running, and call enable once so the user can allow automation in the app's window. " +
				"Prefer the app's dev hook (window.__<app>) through eval to reach a screen, then snapshot to check it.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": automationMCPTools()}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return nil, map[string]any{"code": -32602, "message": "invalid params"}
		}
		return client.callMCPTool(params.Name, params.Arguments), nil
	default:
		return nil, map[string]any{"code": -32601, "message": "method not found: " + message.Method}
	}
}

func (client automationClient) callMCPTool(name string, arguments map[string]any) map[string]any {
	failure := func(err error) map[string]any {
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}
	}
	data, _ := json.Marshal(arguments)
	var request automationRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return failure(fmt.Errorf("invalid arguments: %w", err))
	}
	request.Command = name
	request.Follow = false
	known := false
	for _, tool := range automationMCPTools() {
		known = known || tool.Name == name
	}
	if !known {
		return failure(fmt.Errorf("unknown tool %q", name))
	}
	result, err := client.call(request)
	if err != nil {
		return failure(err)
	}
	if name == "snapshot" {
		var info map[string]any
		if err := json.Unmarshal(result, &info); err != nil {
			return failure(err)
		}
		encoded, _ := info["png"].(string)
		delete(info, "png")
		details, _ := json.Marshal(info)
		return map[string]any{"content": []any{
			map[string]any{"type": "image", "data": encoded, "mimeType": "image/png"},
			map[string]any{"type": "text", "text": string(details)},
		}}
	}
	var pretty any
	text := string(result)
	if json.Unmarshal(result, &pretty) == nil {
		if formatted, err := marshalPlain(pretty, "  "); err == nil {
			text = strings.TrimSpace(string(formatted))
		}
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}
