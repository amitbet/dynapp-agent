package shellagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestFileSearchRebuildQueryAndDetails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report-final.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{StateDir: t.TempDir()}
	rebuilt, err := server.handleFileSearchRPC(context.Background(), message{Method: "rebuild", Args: []any{map[string]any{"roots": []any{root}}}})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.(map[string]any)["count"].(int) < 2 {
		t.Fatalf("rebuild = %#v", rebuilt)
	}
	result, err := server.handleFileSearchRPC(context.Background(), message{Method: "search", Args: []any{"file:report ext:txt", map[string]any{"limit": 10.0}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["total"].(int) != 1 {
		t.Fatalf("search = %#v", result)
	}
	details, err := server.handleFileSearchRPC(context.Background(), message{Method: "details", Args: []any{[]any{filepath.Join(root, "report-final.txt")}}})
	if err != nil || details.([]map[string]any)[0]["exists"] != true {
		t.Fatalf("details = %#v, %v", details, err)
	}
}

func TestFileSearchLiveExactLookupFindsFileMissingFromIndex(t *testing.T) {
	root := t.TempDir()
	server := &Server{StateDir: t.TempDir()}
	if _, err := server.handleFileSearchRPC(context.Background(), message{Method: "rebuild", Args: []any{map[string]any{"roots": []any{root}}}}); err != nil {
		t.Fatal(err)
	}
	name := "newly-downloaded-video.mkv"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := server.handleFileSearchRPC(context.Background(), message{
		Method: "search",
		Args: []any{"file:newly-downloaded-video.mkv", map[string]any{
			"limit":     10.0,
			"exactName": name,
			"live":      true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	results := payload["results"].([]searchEntry)
	if payload["liveSearched"] != true || len(results) != 1 || results[0].Path != path {
		t.Fatalf("live search = %#v", payload)
	}
}

func TestFileSearchLiveExactLookupDoesNotRequireAnIndex(t *testing.T) {
	root := t.TempDir()
	server := &Server{StateDir: t.TempDir()}
	name := "first-dropped-video.mkv"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.saveSearchConfig(searchConfig{Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	result, err := server.handleFileSearchRPC(context.Background(), message{
		Method: "search",
		Args: []any{"file:first-dropped-video.mkv", map[string]any{
			"exactName": name,
			"live":      true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	results := payload["results"].([]searchEntry)
	if len(results) != 1 || results[0].Path != path {
		t.Fatalf("live search without index = %#v", payload)
	}
	_, indexPath, err := server.searchPaths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live exact lookup unexpectedly built index: %v", err)
	}
}

func TestWindowsFileSearchDefaultsUseEveryMountedDrive(t *testing.T) {
	roots := defaultFileSearchRootsFrom("windows", `C:\Users\Amit`, []map[string]string{
		{"path": `C:\Users\Amit`, "label": "~"},
		{"path": `C:\`, "label": "C:"},
		{"path": `D:\`, "label": "D:"},
	})
	if len(roots) != 2 || roots[0] != `C:\` || roots[1] != `D:\` {
		t.Fatalf("Windows defaults = %#v", roots)
	}
	if validExactFileName(`..\secret.mkv`) || validExactFileName("video.mkv/child") || !validExactFileName("video.mkv") {
		t.Fatal("exact filename validation accepted a path or rejected a filename")
	}
}

func TestExternalOpenQueueReturnsBoundedFileData(t *testing.T) {
	file := filepath.Join(t.TempDir(), "open.txt")
	_ = os.WriteFile(file, []byte("opened"), 0o600)
	server := &Server{}
	body := `{"appId":"viewer","paths":[` + strconv.Quote(file) + `]}`
	request := httptest.NewRequest("POST", "http://127.0.0.1/external-open", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.handleExternalOpenHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("queue status = %d", response.Code)
	}
	result, err := server.handleExternalOpenRPC(context.Background(), message{Method: "takeData", AppID: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	records := result.([]map[string]any)
	if len(records) != 1 || records[0]["content"] != "opened" {
		t.Fatalf("records = %#v", records)
	}
}

func TestExternalOpenLauncherPersistsUntilServiceStarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "launch.txt")
	if err := os.WriteFile(file, []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := QueueExternalOpenFallback(state, "viewer", []string{file}); err != nil {
		t.Fatal(err)
	}
	server := &Server{StateDir: state}
	result, err := server.handleExternalOpenRPC(context.Background(), message{Method: "takeData", AppID: "viewer"})
	if err != nil || len(result.([]map[string]any)) != 1 || result.([]map[string]any)[0]["content"] != "later" {
		t.Fatalf("records = %#v, %v", result, err)
	}
	again, err := server.handleExternalOpenRPC(context.Background(), message{Method: "takeData", AppID: "viewer"})
	if err != nil || len(again.([]map[string]any)) != 0 {
		t.Fatalf("fallback queue was not drained: %#v, %v", again, err)
	}
}

type testProtocolSocket struct{}

func (testProtocolSocket) Read(context.Context) (websocket.MessageType, []byte, error) {
	return 0, nil, context.Canceled
}
func (testProtocolSocket) Write(context.Context, websocket.MessageType, []byte) error { return nil }
func (testProtocolSocket) Close(websocket.StatusCode, string) error                   { return nil }

func TestAgentSessionSpeaksCodexAppServerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX launcher")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	bin := t.TempDir()
	fixture := filepath.Join(bin, "fixture.mjs")
	source := `import readline from "node:readline";if(process.argv.includes("--version")){console.log("codex 1.0");process.exit(0)}if(process.argv.includes("login")&&process.argv.includes("status")){process.exit(0)}const lines=readline.createInterface({input:process.stdin});for await(const line of lines){const m=JSON.parse(line);if(m.id===undefined)continue;let result={};if(m.method==="thread/start")result={thread:{id:"thread-1"}};if(m.method==="model/list")result={data:[{model:"test-model",displayName:"Test",isDefault:true,defaultReasoningEffort:"medium",supportedReasoningEfforts:[{reasoningEffort:"medium",description:"Balanced"}]}]};if(m.method==="turn/start")result={turn:{id:"turn-1"}};process.stdout.write(JSON.stringify({id:m.id,result})+"\n");}`
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(bin, "codex")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+shellQuote(node)+" "+shellQuote(fixture)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	service := newAgentService(&Server{StateDir: t.TempDir()}, testProtocolSocket{})
	defer service.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started, err := service.handle(ctx, message{Method: "start", AppID: "write", Args: []any{map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := started.(map[string]any)["sessionId"].(string)
	models, err := service.handle(ctx, message{Method: "listModels", SessionID: sessionID})
	if err != nil || len(models.([]map[string]any)) != 1 || models.([]map[string]any)[0]["id"] != "test-model" || len(models.([]map[string]any)[0]["efforts"].([]map[string]any)) != 1 {
		t.Fatalf("models = %#v, %v", models, err)
	}
	turn, err := service.handle(ctx, message{Method: "prompt", SessionID: sessionID, Args: []any{map[string]any{"text": "hello"}}})
	if err != nil || turn.(map[string]any)["turnId"] != "turn-1" {
		t.Fatalf("turn = %#v, %v", turn, err)
	}
}

func TestAssociationOptionsRejectUnsafeDefinitions(t *testing.T) {
	executable, _ := os.Executable()
	valid := AssociationOptions{AppID: "viewer", Name: "Viewer", URL: "https://dyner.example/app/a/viewer", Executable: executable, Extensions: []string{"txt"}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.Extensions = []string{"../bad"}
	if err := valid.Validate(); err == nil {
		t.Fatal("unsafe extension was accepted")
	}
}

func TestAssociationAppIDAcceptsHostedStoreID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "amit-bet/mdview", want: "mdview"},
		{input: "mdview", want: "mdview"},
	}
	for _, test := range tests {
		got, err := associationAppID(test.input)
		if err != nil || got != test.want {
			t.Fatalf("associationAppID(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"", "owner/bad app", "../mdview"} {
		if _, err := associationAppID(input); err == nil {
			t.Fatalf("associationAppID(%q) accepted an invalid id", input)
		}
	}
}

func TestAssociationExecutableIsPersistedOutsideTemporaryBuild(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "go-build", "dynapp-shell-agent")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("agent binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := persistAssociationExecutable(filepath.Join(root, "state"), source)
	if err != nil {
		t.Fatal(err)
	}
	if target == source || !strings.Contains(target, filepath.Join("os-integration", "file-associations")) {
		t.Fatalf("persistent executable path = %q", target)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "agent binary" {
		t.Fatalf("persistent executable = %q, %v", data, err)
	}
}

func TestAssociationFallbackReportsInstalledExtensions(t *testing.T) {
	// Never change the developer's default applications during tests.
	if runtime.GOOS == "darwin" {
		previous := runMacAssociationCommand
		t.Cleanup(func() { runMacAssociationCommand = previous })
		runMacAssociationCommand = func(args ...string) ([]byte, error) { return []byte(args[7]), nil }
	}
	if runtime.GOOS == "windows" {
		t.Skip("registry fallback is covered by cross-compilation")
	}
	t.Setenv("HOME", t.TempDir())
	executable, _ := os.Executable()
	options := AssociationOptions{AppID: "viewer", Name: "Viewer", URL: "https://dyner.example/app/a/viewer", Executable: executable, Extensions: []string{"txt", "md"}}
	if err := InstallAssociations(options); err != nil {
		t.Fatal(err)
	}
	state, err := AssociationState("viewer")
	if err != nil || state["applied"] != true || len(state["appliedExtensions"].([]string)) != 2 {
		t.Fatalf("state = %#v, %v", state, err)
	}
	options.Extensions = []string{"md"}
	if err := InstallAssociations(options); err != nil {
		t.Fatal(err)
	}
	state, err = AssociationState("viewer")
	applied := state["appliedExtensions"].([]string)
	if err != nil || len(applied) != 1 || applied[0] != "md" {
		t.Fatalf("customized state = %#v, %v", state, err)
	}
	if err := RemoveAssociations("viewer"); err != nil {
		t.Fatal(err)
	}
	state, err = AssociationState("viewer")
	if err != nil || state["applied"] != false {
		t.Fatalf("removed state = %#v, %v", state, err)
	}
}

func TestServiceResultJSONShapesRemainSerializable(t *testing.T) {
	value := secretMetadata{Name: "token", UpdatedAt: "now"}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
}

func TestDurableSessionsAreAppScopedAndPrunable(t *testing.T) {
	server := &Server{StateDir: t.TempDir()}
	record := map[string]any{"sessionId": "thread:1", "processId": "proc_1", "sequence": 2.0, "status": "idle", "value": map[string]any{"provider": "codex"}}
	if _, err := server.handleSessionsRPC(message{AppID: "dymaker", Method: "upsert", Args: []any{record}}); err != nil {
		t.Fatal(err)
	}
	got, err := server.handleSessionsRPC(message{AppID: "dymaker", Method: "get", Args: []any{"thread:1"}})
	if err != nil || got.(sessionRecord).Value["provider"] != "codex" {
		t.Fatalf("session = %#v, %v", got, err)
	}
	other, err := server.handleSessionsRPC(message{AppID: "other", Method: "list"})
	if err != nil || len(other.([]sessionRecord)) != 0 {
		t.Fatalf("other sessions = %#v, %v", other, err)
	}
	result, err := server.handleSessionsRPC(message{AppID: "dymaker", Method: "prune", Args: []any{float64(time.Now().Add(time.Minute).UnixMilli())}})
	if err != nil || len(result.(map[string]any)["removed"].([]string)) != 1 {
		t.Fatalf("prune = %#v, %v", result, err)
	}
}

func TestHTTPProviderBypassesBrowserCORSAndBoundsResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Provider", "go")
		_, _ = response.Write([]byte("local network"))
	}))
	defer upstream.Close()
	server := &Server{}
	result, err := server.handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{"url": upstream.URL, "method": "GET", "connect": []any{upstream.URL + "/"}}}})
	if err != nil || result.(map[string]any)["text"] != "local network" {
		t.Fatalf("request = %#v, %v", result, err)
	}
}

func TestHTTPProviderReturnsBinaryBodiesAsBase64(t *testing.T) {
	payload := []byte{0xff, 0xd8, 0xff, 0x00, 0x80}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "image/jpeg")
		_, _ = response.Write(payload)
	}))
	defer upstream.Close()
	server := &Server{}
	result, err := server.handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{"url": upstream.URL, "method": "GET", "binary": true, "connect": []any{upstream.URL + "/"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)
	if got["text"] != "" || got["base64"] != base64.StdEncoding.EncodeToString(payload) {
		t.Fatalf("binary request = %#v", got)
	}
}

func TestHTTPProviderEnforcesPathPrefixesAndRedirectHops(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/ok" {
			_, _ = response.Write([]byte("ok"))
			return
		}
		if request.URL.Path == "/start" {
			http.Redirect(response, request, "/secret", http.StatusFound)
			return
		}
		_, _ = response.Write([]byte("secret"))
	}))
	defer upstream.Close()
	server := &Server{}
	connect := []string{upstream.URL + "/api/v1/"}
	result, err := server.handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{
		"url": upstream.URL + "/api/v1/ok", "method": "GET", "connect": connect,
	}}})
	if err != nil || result.(map[string]any)["text"] != "ok" {
		t.Fatalf("allowed path = %#v, %v", result, err)
	}
	if _, err := server.handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{
		"url": upstream.URL + "/secret", "method": "GET", "connect": connect,
	}}}); err == nil {
		t.Fatal("expected sibling path to be denied")
	}
	if _, err := server.handleHTTPRPC(context.Background(), message{Method: "request", Args: []any{map[string]any{
		"url": upstream.URL + "/start", "method": "GET", "connect": []any{upstream.URL + "/start"},
	}}}); err == nil {
		t.Fatal("expected redirect hop outside the declared path to be denied")
	}
}

func TestSystemListenerParsersMatchProviderShape(t *testing.T) {
	lsof := "p123\ncnode\nf4\nPTCP\nn127.0.0.1:3000\nf5\nPTCP\nn127.0.0.1:3000\n"
	listeners := parseLsofPorts(lsof)
	if len(listeners) != 1 || listeners[0].Port != 3000 || listeners[0].PID == nil || *listeners[0].PID != 123 {
		t.Fatalf("lsof = %#v", listeners)
	}
	ss := `tcp LISTEN 0 128 0.0.0.0:8080 0.0.0.0:* users:(("java",pid=20501,fd=4))`
	listeners = parseSSPorts(ss)
	if len(listeners) != 1 || listeners[0].ProcessName != "java" || listeners[0].Port != 8080 {
		t.Fatalf("ss = %#v", listeners)
	}
}
