package shellagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Another machine's grant for an app lives in Dyner, and its owner decides.
// When the owner runs the app as a native app, the question is asked in the
// app's own window like every other permission: the runtime names the Dyner
// request it filed, this agent reads that request from Dyner (never from the
// app), shows its prompt, and records the answer with its device credential.
// Dyner accepts that only for the owner's own request, for the origin of the
// app this agent hosts, on a machine the same owner has.

var remotePermissionRequestID = regexp.MustCompile(`^rpr_[A-Za-z0-9-]{8,64}$`)

type remotePermissionRequest struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	Origin       string   `json:"origin"`
	AppName      string   `json:"appName"`
	Capabilities []string `json:"capabilities"`
	Environment  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"environment"`
}

func remotePermissionURL(config Config, requestID string) (string, error) {
	origin := config.dynerBaseOrigin()
	if origin == "" {
		return "", errors.New("this agent has no Dyner address")
	}
	base, err := url.Parse(origin)
	if err != nil {
		return "", errors.New("this agent has no Dyner address")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/remote-environments/" +
		url.PathEscape(config.EnvironmentID) + "/permission-requests/" + url.PathEscape(requestID)
	return base.String(), nil
}

func dynerDeviceJSON(ctx context.Context, client *http.Client, config Config, method, target string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(mustJSON(body))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "DynApp-Device "+config.DeviceCredential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Dyner permission request failed: HTTP %d", response.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out)
}

// handleRemotePermissionRPC implements `permissionRequest.remote`
// {requestId}: the native app's user answers another machine's request in
// the app's window. The result's status is "approved", "denied" or
// "pending" (dismissed, or not this agent's to ask).
func (s *Server) handleRemotePermissionRPC(ctx context.Context, request message) (any, error) {
	auth := request.auth
	if auth == nil || !auth.native || auth.storeID == "" {
		return nil, errors.New("only a native app asks about another machine here")
	}
	requestID := strings.TrimSpace(fmt.Sprint(objectArg(request.Args, 0)["requestId"]))
	if !remotePermissionRequestID.MatchString(requestID) {
		return nil, errors.New("invalid permission request id")
	}
	app, ok := s.nativeApp(auth.storeID)
	if !ok {
		return nil, errors.New("this app is no longer installed")
	}
	s.mu.Lock()
	config := s.Config.relayTicketConfig()
	s.mu.Unlock()
	if config.EnvironmentID == "" || config.DeviceCredential == "" {
		return nil, errors.New("this machine is not registered with Dyner")
	}
	target, err := remotePermissionURL(config, requestID)
	if err != nil {
		return nil, err
	}
	var read struct {
		Request remotePermissionRequest `json:"request"`
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = dynerDeviceJSON(readCtx, s.DynerHTTPClient, config, http.MethodGet, target+"?origin="+url.QueryEscape(app.Origin), nil, &read)
	cancel()
	if err != nil {
		return nil, err
	}
	asked := normalizePermissionSet(read.Request.Capabilities)
	if read.Request.Status != "pending" || len(asked) == 0 {
		return map[string]any{"status": read.Request.Status}, nil
	}
	control := s.nativeControl(app.StoreID)
	if control == nil {
		return map[string]any{"status": "pending"}, nil
	}
	name := auth.appName
	if name == "" {
		name = app.Name
	}
	pending, err := s.registerPending(&pendingRequest{
		Kind: "required", AppName: name, StoreID: app.StoreID, Origin: app.Origin, Declared: asked,
	})
	if err != nil {
		return nil, err
	}
	machine := read.Request.Environment.Name
	if machine == "" {
		machine = "another machine"
	}
	groups := nativePermissionGroups(asked, nil, map[string]string{})
	frame := nativePermissionRequestFrame(pending.ID, "required", name, app.Origin, groups, nil)
	if page := nativePermissionPageWith("required", name, app.Origin, groups, nil, map[string]any{"machine": machine}); page != "" {
		frame["page"] = page
	}
	if !send(control.socket, ctx, frame) {
		s.unregisterPending(pending.ID)
		return map[string]any{"status": "pending"}, nil
	}
	waitCtx, stop := context.WithTimeout(ctx, requiredPermissionTimeout)
	granted, _ := s.awaitDecision(waitCtx, pending)
	stop()
	if !pending.wasResolved() {
		return map[string]any{"status": "pending"}, nil
	}
	outcome, _ := pending.Outcome()
	decision := ""
	switch {
	case outcome == decisionAllow && len(granted) > 0:
		decision = "allow"
	case outcome == decisionDeny:
		decision = "deny"
	default:
		return map[string]any{"status": "pending"}, nil
	}
	body := map[string]any{"origin": app.Origin, "decision": decision}
	if decision == "allow" {
		body["capabilities"] = intersectStrings(granted, asked)
	}
	var decided struct {
		Status  string   `json:"status"`
		Granted []string `json:"granted"`
	}
	decideCtx, cancelDecide := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelDecide()
	if err := dynerDeviceJSON(decideCtx, s.DynerHTTPClient, config, http.MethodPost, target, body, &decided); err != nil {
		return nil, err
	}
	return map[string]any{"status": decided.Status, "granted": decided.Granted}, nil
}
