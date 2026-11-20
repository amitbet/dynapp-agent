package shellagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// usePackagedInstall makes the agent look apt-installed at a temp path whose
// version the test controls.
func usePackagedInstall(t *testing.T, version string) (executable string, setVersion func(string)) {
	t.Helper()
	executable = filepath.Join(t.TempDir(), "dynapp-shell-agent")
	if err := os.WriteFile(executable, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := version
	previousDetect, previousVersion, previousInterval := detectPackagedInstall, installedAgentVersion, packagedCheckInterval
	detectPackagedInstall = func() (packagedInstall, bool) {
		return packagedInstall{Manager: "apt", Executable: executable}, true
	}
	installedAgentVersion = func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if current == "" {
			return "", errors.New("unreadable")
		}
		return current, nil
	}
	packagedCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		detectPackagedInstall, installedAgentVersion, packagedCheckInterval = previousDetect, previousVersion, previousInterval
	})
	return executable, func(next string) {
		mu.Lock()
		current = next
		mu.Unlock()
	}
}

func TestPackagedAgentRestartsIntoTheVersionAptInstalled(t *testing.T) {
	executable, setVersion := usePackagedInstall(t, "1.2.3")
	restarted := make(chan string, 4)
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: SelfUpdateConfig{
		Enabled: true, Version: "1.2.3", Repository: "amitbet/dynapp-agent", Interval: time.Hour,
		OnUpdate: func(context.Context, string, string) error {
			t.Error("an apt install must not replace its own binary")
			return nil
		},
		OnRestart: func(_ context.Context, version string) error { restarted <- version; return nil },
	}}
	agent.startSelfUpdater()
	defer agent.selfUpdateCancel()
	select {
	case version := <-restarted:
		t.Fatalf("restarted into %s while the installed version is unchanged", version)
	case <-time.After(150 * time.Millisecond):
	}
	setVersion("1.2.4")
	if err := os.WriteFile(executable, []byte("v2, longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case version := <-restarted:
		if version != "1.2.4" {
			t.Fatalf("restarted into %s", version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not restart into the upgraded binary")
	}
}

func TestPackagedAgentRetriesAFailedRestart(t *testing.T) {
	_, _ = usePackagedInstall(t, "1.2.4")
	attempts := make(chan struct{}, 8)
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: SelfUpdateConfig{
		Enabled: true, Version: "1.2.3", Interval: time.Hour,
		OnUpdate: func(context.Context, string, string) error { return nil },
		OnRestart: func(context.Context, string) error {
			attempts <- struct{}{}
			return errors.New("exec failed")
		},
	}}
	agent.startSelfUpdater()
	defer agent.selfUpdateCancel()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("a failed restart was not retried")
		}
	}
	if got := agent.updateStatus().Error; !strings.Contains(got, "exec failed") {
		t.Fatalf("status error = %q", got)
	}
}

func TestUpdateNowOnAnAptInstallPointsAtTheUpgradeService(t *testing.T) {
	_, setVersion := usePackagedInstall(t, "1.2.3")
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	restarted := make(chan string, 1)
	config := release.config("1.2.3", func(context.Context, string, string) error {
		t.Error("an apt install must not replace its own binary")
		return nil
	})
	config.OnRestart = func(_ context.Context, version string) error { restarted <- version; return nil }
	agent := &Server{StateDir: t.TempDir(), selfUpdateConfig: config, selfUpdateCancel: func() {},
		packaged: &packagedInstall{Manager: "apt", Executable: "/usr/bin/dynapp-shell-agent"}}
	recorder := postUpdateNow(t, agent)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "1.2.4 is available") ||
		!strings.Contains(recorder.Body.String(), "sudo systemctl start dynapp-agent-upgrade.service") {
		t.Fatalf("no newer install: %d %s", recorder.Code, recorder.Body)
	}
	// apt already installed a newer agent: update restarts into it.
	setVersion("1.2.4")
	recorder = postUpdateNow(t, agent)
	var result UpdateNowResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil || !result.Updating || result.Latest != "1.2.4" {
		t.Fatalf("newer install: %d %s", recorder.Code, recorder.Body)
	}
	select {
	case version := <-restarted:
		if version != "1.2.4" {
			t.Fatalf("restarted into %s", version)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update did not restart into the installed version")
	}
}

func TestUpdateStatusReportsAFailedUpdate(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	config := release.config("1.2.3", func(context.Context, string, string) error {
		return errors.New("prepare replacement binary: permission denied")
	})
	previous := updateNowVerifier
	updateNowVerifier = func(string) error { return nil }
	t.Cleanup(func() { updateNowVerifier = previous })
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: config, selfUpdateConfig: config, selfUpdateCancel: func() {}}
	if recorder := postUpdateNow(t, agent); recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := agent.updateStatus()
		if !status.Updating && status.Error != "" {
			if !strings.Contains(status.Error, "permission denied") {
				t.Fatalf("status error = %q", status.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never reported the failure: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
