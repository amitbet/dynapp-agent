package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amitbet/dynapp-agent/shellagent"
)

func TestParseAgentURLAcceptsOnlyStart(t *testing.T) {
	for _, raw := range []string{"dynapp://start", "DYNAPP://start/", "dynapp:start", "dynapp://"} {
		if action, err := parseAgentURL(raw); err != nil || action != "start" {
			t.Fatalf("%q = %q, %v", raw, action, err)
		}
	}
	for _, raw := range []string{"dynapp://install?app=evil/app", "https://start", "dynapp://open/amit-bet/notes", ""} {
		if _, err := parseAgentURL(raw); err == nil {
			t.Fatalf("%q must be rejected", raw)
		}
	}
}

func TestOpenURLLeavesARunningAgentAlone(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()
	address := strings.TrimPrefix(agent.URL, "http://")
	start := time.Now()
	if err := runOpenURL("dynapp://start", address); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("open-url waited although the agent answered")
	}
	if err := runOpenURL("dynapp://install", address); err == nil {
		t.Fatal("an unsupported action must fail before touching the agent")
	}
}

func TestInteractiveUsersGetOnlyServiceStart(t *testing.T) {
	defaultSDDL := "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;BA)(A;;CCLCSWLOCRRC;;;IU)(A;;CCLCSWLOCRRC;;;SU)S:(AU;FA;CCDCLCSWRPWPDTLOCRSDRCWDWO;;;WD)"
	updated := withInteractiveStartRight(defaultSDDL)
	if !strings.Contains(updated, "(A;;CCLCSWLOCRRCRP;;;IU)") {
		t.Fatalf("IU ACE = %s", updated)
	}
	if strings.Replace(updated, "(A;;CCLCSWLOCRRCRP;;;IU)", "(A;;CCLCSWLOCRRC;;;IU)", 1) != defaultSDDL {
		t.Fatalf("other entries changed: %s", updated)
	}
	if withInteractiveStartRight(updated) != updated {
		t.Fatal("adding the right twice changed the descriptor")
	}
	without := "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)S:(AU;FA;CC;;;WD)"
	if got := withInteractiveStartRight(without); got != "D:(A;;CCLCSWRPWPDTLOCRRC;;;SY)(A;;LCRPLO;;;IU)S:(AU;FA;CC;;;WD)" {
		t.Fatalf("added ACE = %s", got)
	}
	if withInteractiveStartRight("") != "" {
		t.Fatal("an empty descriptor must stay empty")
	}
}

// A helper that dies right after it starts (blocked by antivirus) must leave
// the agent serving and the update files cleaned up.
func TestBlockedUpdateHelperKeepsTheAgentRunning(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "dynapp-shell-agent")
	downloaded := filepath.Join(dir, "updates", "new-agent")
	if err := os.MkdirAll(filepath.Dir(downloaded), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{executable: "old", downloaded: "new"} {
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previousCommand, previousExit := helperCommand, osExit
	t.Cleanup(func() { helperCommand, osExit = previousCommand, previousExit })
	helperCommand = func(string, []string) *exec.Cmd { return blockedHelper() }
	osExit = func(code int) { t.Fatalf("the agent exited (%d) after a blocked helper", code) }
	p := &program{server: &shellagent.Server{StateDir: dir}, serviceMode: true}
	err := p.scheduleHelperUpdate(downloaded, executable, "9.9.9")
	if err == nil || !strings.Contains(err.Error(), "exited before taking over") {
		t.Fatalf("scheduleHelperUpdate = %v", err)
	}
	if data, _ := os.ReadFile(executable); string(data) != "old" {
		t.Fatalf("the installed agent changed: %q", data)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "updates", ".dynapp-update-helper-*"))
	prepared, _ := filepath.Glob(executable + ".prepared-*")
	if len(leftovers)+len(prepared) != 0 {
		t.Fatalf("update artifacts left behind: %v %v", leftovers, prepared)
	}
}

func TestStateDirFromServiceCommandLine(t *testing.T) {
	cases := map[string]string{
		`"C:\Program Files\DynApp\dynapp-shell-agent.exe" --address 127.0.0.1:9011 --state-dir "C:\Users\AMNON\AppData\Roaming\DynApp\shell-agent"`: `C:\Users\AMNON\AppData\Roaming\DynApp\shell-agent`,
		`C:\DynApp\agent.exe --state-dir C:\data\agent --relay`:                                                                                     `C:\data\agent`,
		`C:\DynApp\agent.exe --address 127.0.0.1:9011`:                                                                                            "",
	}
	for commandLine, want := range cases {
		if got := stateDirFromCommandLine(commandLine); got != want {
			t.Fatalf("%s => %q, want %q", commandLine, got, want)
		}
	}
}
