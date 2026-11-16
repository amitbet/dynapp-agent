package shellagent

import (
	"context"
	"errors"
	"testing"
)

func TestParseWindowsNetworkProfiles(t *testing.T) {
	single, err := parseWindowsNetworkProfiles([]byte(`{"Name":"Home","InterfaceAlias":"Ethernet","NetworkCategory":"Private"}`))
	if err != nil || len(single) != 1 || single[0].Category != "private" || single[0].Interface != "Ethernet" {
		t.Fatalf("single = %+v, %v", single, err)
	}
	many, err := parseWindowsNetworkProfiles([]byte(`[{"Name":"Corp","NetworkCategory":"DomainAuthenticated"},{"Name":"Cafe","NetworkCategory":0}]`))
	if err != nil || len(many) != 2 || many[0].Category != "domain" || many[1].Category != "public" {
		t.Fatalf("many = %+v, %v", many, err)
	}
	empty, err := parseWindowsNetworkProfiles([]byte("  \r\n"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

func TestNetworkSettingsStatusSuggestsLANOnlyOnTrustedNetworks(t *testing.T) {
	cases := []struct {
		name     string
		profiles []networkProfile
		mode     string
		category string
		suggest  bool
	}{
		{"private", []networkProfile{{Category: "private"}}, ListenerLocal, "private", true},
		{"domain", []networkProfile{{Category: "domain"}}, ListenerLocal, "domain", true},
		{"private and domain", []networkProfile{{Category: "private"}, {Category: "domain"}}, ListenerOff, "private", true},
		{"already LAN", []networkProfile{{Category: "private"}}, ListenerLAN, "private", false},
		{"public", []networkProfile{{Category: "public"}}, ListenerLocal, "public", false},
		{"private and public", []networkProfile{{Category: "private"}, {Category: "public"}}, ListenerLocal, "mixed", false},
		{"unknown category", []networkProfile{{Category: "private"}, {Category: ""}}, ListenerLocal, "mixed", false},
		{"offline", nil, ListenerLocal, "none", false},
	}
	for _, tc := range cases {
		status := networkSettingsStatus(tc.profiles, nil, tc.mode)
		if status["category"] != tc.category || status["suggestLAN"] != tc.suggest {
			t.Errorf("%s: got %v", tc.name, status)
		}
	}
	if status := networkSettingsStatus(nil, errNetworkCategoryUnsupported, ListenerLocal); status["supported"] != false || status["suggestLAN"] != nil {
		t.Errorf("unsupported: got %v", status)
	}
}

func TestNetworkCategoryCacheReusesRecentRead(t *testing.T) {
	calls := 0
	previous := readNetworkProfiles
	readNetworkProfiles = func(context.Context) ([]networkProfile, error) {
		calls++
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { readNetworkProfiles = previous })
	var cache networkCategoryCache
	for range 3 {
		if _, err := cache.get(context.Background()); err == nil {
			t.Fatal("expected the read error")
		}
	}
	if calls != 1 {
		t.Fatalf("reads = %d, want 1", calls)
	}
}
