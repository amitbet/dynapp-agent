package shellagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Dyner's prompt is the browser twin of the native one: Don't Allow with
// "Don't ask again" stops a browser app from raising the request again.
func TestDynerRefusalWithDontAskIsRecordedOnTheBrowserIdentity(t *testing.T) {
	agent := newTestServer(t)
	agent.autoApprovePairings = false
	identity := BrowserIdentity{
		KeyID: "key-1", Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes",
		ApprovedAt: time.Now().UnixMilli(), Capabilities: []string{"fs.readText"},
	}
	agent.upsertBrowserIdentity(identity)
	auth := &socketAuthentication{
		keyID: identity.KeyID, origin: identity.Origin, storeID: identity.StoreID, appName: "Notes",
		declared: []string{"fs.readText", "fs.writeText"},
	}
	if !agent.permissionAskable(auth, "fs.writeText") {
		t.Fatal("fs.writeText should be askable before the decision")
	}
	pending, err := agent.requestBrowserPermissions(auth, []string{"fs.writeText"})
	if err != nil || pending == nil {
		t.Fatalf("request = %#v, %v", pending, err)
	}
	if _, err := agent.decidePermissions(map[string]any{"id": pending.ID, "capabilities": nil, "dontAsk": true}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for agent.permissionAskable(auth, "fs.writeText") {
		select {
		case <-ctx.Done():
			t.Fatal("Don't ask again was not recorded")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stored, _ := agent.findIdentity(identity.KeyID, identity.Origin, identity.StoreID)
	if strings.Join(stored.Denied, ",") != "fs.writeText" || strings.Join(stored.DontAsk, ",") != "fs.writeText" {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestDynerRefusalWithoutDontAskStaysAskable(t *testing.T) {
	agent := newTestServer(t)
	agent.autoApprovePairings = false
	identity := BrowserIdentity{
		KeyID: "key-1", Origin: "https://amitbet-notes.dynapp.io", StoreID: "amit-bet/notes",
		ApprovedAt: time.Now().UnixMilli(),
	}
	agent.upsertBrowserIdentity(identity)
	auth := &socketAuthentication{keyID: identity.KeyID, origin: identity.Origin, storeID: identity.StoreID, declared: []string{"fs.writeText"}}
	pending, _ := agent.requestBrowserPermissions(auth, []string{"fs.writeText"})
	if _, err := agent.decidePermissions(map[string]any{"id": pending.ID, "capabilities": nil}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, _ := agent.findIdentity(identity.KeyID, identity.Origin, identity.StoreID)
		if len(stored.Denied) > 0 {
			if len(stored.DontAsk) != 0 || !agent.permissionAskable(auth, "fs.writeText") {
				t.Fatalf("a plain refusal must stay askable: %#v", stored)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("refusal was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A browser reaching this machine through Dyner is known only by the pairing
// Dyner synced, which names no app. Its app can still ask, and the grant
// outlives the next identity sync.
func TestSyncedPairingCanAskAndKeepsTheGrantAcrossSyncs(t *testing.T) {
	agent := newTestServer(t)
	agent.autoApprovePairings = false
	synced := BrowserIdentity{KeyID: "key-1", Origin: "https://amitbet-formfuel.dynapp.io", Capabilities: []string{"fs.readText"}}
	agent.mu.Lock()
	agent.Config.BrowserIdentities = mergeSyncedIdentities(nil, []BrowserIdentity{synced}, nil)
	agent.mu.Unlock()
	auth := &socketAuthentication{
		keyID: synced.KeyID, origin: synced.Origin, storeID: "amit-bet/formfuel", appName: "Form & Fuel",
		declared: []string{"fs.readText", "servers.connect"}, capabilities: []string{"fs.readText"},
	}
	if !agent.permissionAskable(auth, "servers.connect") {
		t.Fatal("servers.connect should be askable for a synced pairing")
	}
	pending, err := agent.requestBrowserPermissions(auth, []string{"servers.connect"})
	if err != nil || pending == nil {
		t.Fatalf("request = %#v, %v", pending, err)
	}
	if _, err := agent.decidePermissions(map[string]any{"id": pending.ID, "capabilities": []any{"servers.connect"}}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, ok := agent.findIdentity(synced.KeyID, synced.Origin, "amit-bet/formfuel")
		if ok && stringSliceHas(stored.Capabilities, "servers.connect") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("grant was not stored for the app")
		}
		time.Sleep(10 * time.Millisecond)
	}
	agent.mu.Lock()
	agent.Config.BrowserIdentities = mergeSyncedIdentities(agent.Config.BrowserIdentities, []BrowserIdentity{synced}, nil)
	identities := append([]BrowserIdentity(nil), agent.Config.BrowserIdentities...)
	agent.mu.Unlock()
	hello := message{KeyID: synced.KeyID, App: &directLocalApp{StoreID: "amit-bet/formfuel"}}
	grant := relayGrantFor(identities, hello, sessionSubject{}, func(string) (resolvedApp, bool) {
		return resolvedApp{Declared: []string{"fs.readText", "servers.connect"}}, true
	})
	if !socketAllows(grant.capabilities, "servers.connect") {
		t.Fatalf("relay grant after sync = %v", grant.capabilities)
	}
}

func TestAgentAdvertisesServersConnect(t *testing.T) {
	if !stringSliceHas(DefaultListenerCapabilities(), "servers.connect") {
		t.Fatal("Dyner clamps remote grants to the advertised ceiling; servers.connect must be in it")
	}
}
