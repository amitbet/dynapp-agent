package shellagent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func postUpdateNow(t *testing.T, agent *Server) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9011/api/settings/update", strings.NewReader("{}"))
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	agent.handleSettingsUpdate(recorder, request)
	return recorder
}

func TestUpdateNowInstallsANewerRelease(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	applied := make(chan string, 1)
	config := release.config("1.2.3", func(_ context.Context, _, version string) error {
		applied <- version
		return nil
	})
	previous := updateNowVerifier
	updateNowVerifier = func(string) error { return nil }
	t.Cleanup(func() { updateNowVerifier = previous })
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: config, selfUpdateConfig: config, selfUpdateCancel: func() {}}
	// A manual request ignores the backoff after an unfinished attempt.
	agent.recordUpdateAttempt("1.2.4")
	recorder := postUpdateNow(t, agent)
	var result UpdateNowResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if result != (UpdateNowResult{Current: "1.2.3", Latest: "1.2.4", Updating: true}) {
		t.Fatalf("result = %+v", result)
	}
	select {
	case version := <-applied:
		if version != "1.2.4" {
			t.Fatalf("applied %s", version)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the update was not applied")
	}
}

func TestUpdateNowReportsUpToDate(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.3", []byte("same agent"), sign, true)
	var applied atomic.Int64
	config := release.config("1.2.3", func(context.Context, string, string) error { applied.Add(1); return nil })
	agent := &Server{StateDir: t.TempDir(), selfUpdateConfig: config, selfUpdateCancel: func() {}}
	recorder := postUpdateNow(t, agent)
	var result UpdateNowResult
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil || result.Updating || result.Latest != "1.2.3" {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if applied.Load() != 0 {
		t.Fatal("applied an update to the same version")
	}
}

func TestUpdateNowNeedsSelfUpdateAndAJSONRequest(t *testing.T) {
	agent := &Server{StateDir: t.TempDir()}
	if recorder := postUpdateNow(t, agent); recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "self-update is off") {
		t.Fatalf("disabled agent: %d %s", recorder.Code, recorder.Body)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9011/api/settings/update", strings.NewReader("{}"))
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	agent.handleSettingsUpdate(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request: %d", recorder.Code)
	}
}

func TestUpdateNowWaitsForBridgesAndReportsItUntilForced(t *testing.T) {
	sign := useTestReleaseKey(t)
	release := newFakeRelease(t, "1.2.4", []byte("new shell agent"), sign, true)
	applied := make(chan string, 1)
	config := release.config("1.2.3", func(_ context.Context, _, version string) error {
		applied <- version
		return nil
	})
	previous := updateNowVerifier
	updateNowVerifier = func(string) error { return nil }
	t.Cleanup(func() { updateNowVerifier = previous })
	agent := &Server{StateDir: t.TempDir(), SelfUpdate: config, selfUpdateConfig: config, selfUpdateCancel: func() {}}
	bridges := newBridgeSet(&agent.activeBridges)
	client, remote := net.Pipe()
	defer client.Close()
	bridges.put("tcp_1", remote, nil)
	defer func() { _ = bridges.take("tcp_1").Close() }()

	var result UpdateNowResult
	recorder := postUpdateNow(t, agent)
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body)
	}
	if !result.Updating || result.ActiveBridges != 1 {
		t.Fatalf("result = %+v", result)
	}
	var status UpdateStatus
	deadline := time.Now().Add(10 * time.Second)
	for !status.WaitingForBridges {
		if time.Now().After(deadline) {
			t.Fatalf("status never reported the bridge wait: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
		status = getUpdateStatus(t, agent)
	}
	if status.ActiveBridges != 1 || !status.Updating || time.Until(status.BridgeWaitUntil) < time.Hour {
		t.Fatalf("status = %+v", status)
	}
	select {
	case <-applied:
		t.Fatal("applied the update while a bridge was open")
	default:
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9011/api/settings/update", strings.NewReader(`{"force":true}`))
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Content-Type", "application/json")
	agent.handleSettingsUpdate(httptest.NewRecorder(), request)
	select {
	case version := <-applied:
		if version != "1.2.4" {
			t.Fatalf("applied %s", version)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a forced update kept waiting for the bridge")
	}
}

func getUpdateStatus(t *testing.T, agent *Server) UpdateStatus {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9011/api/settings/update", nil)
	request.RemoteAddr = "127.0.0.1:50000"
	recorder := httptest.NewRecorder()
	agent.handleSettingsUpdate(recorder, request)
	var status UpdateStatus
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &status) != nil {
		t.Fatalf("GET status %d: %s", recorder.Code, recorder.Body)
	}
	return status
}
