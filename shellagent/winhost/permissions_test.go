package winhost

import (
	"reflect"
	"testing"
)

func TestPermissionDefaults(t *testing.T) {
	cases := []struct {
		name, kind string
		group      permissionGroup
		checked    bool
	}{
		{"suggested pairing", "pairing", permissionGroup{Suggested: true, Danger: "medium"}, true},
		{"optional pairing", "pairing", permissionGroup{Danger: "medium"}, false},
		{"suggested new access", "delta", permissionGroup{Suggested: true, Danger: "high"}, true},
		{"very high pairing requires choice", "pairing", permissionGroup{Suggested: true, Danger: "very-high"}, false},
		{"very high new access requires choice", "delta", permissionGroup{Granted: true, Suggested: true, Danger: "very-high"}, false},
		{"review keeps very high grant", "review", permissionGroup{Granted: true, Danger: "very-high"}, true},
		{"review does not add suggested access", "review", permissionGroup{Suggested: true, Danger: "medium"}, false},
		{"review keeps ordinary grant", "review", permissionGroup{Granted: true, Danger: "medium"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := permissionInitiallyChecked(tc.kind, tc.group); got != tc.checked {
				t.Fatalf("checked=%v, want %v", got, tc.checked)
			}
		})
	}
}

func TestPermissionSelection(t *testing.T) {
	groups := []permissionGroup{
		{Permissions: []string{"fs.read", "fs.list"}},
		{Permissions: []string{"fs.exec"}},
		{Permissions: []string{"net.tcp.connect"}},
	}
	got := selectedCapabilities(groups, func(i int) bool { return i != 1 })
	want := []string{"fs.read", "fs.list", "net.tcp.connect"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected capabilities=%v, want %v", got, want)
	}
	got = selectedCapabilities(groups, func(int) bool { return false })
	if got == nil || len(got) != 0 {
		t.Fatalf("saving no permissions must be an explicit empty grant: %#v", got)
	}
}
