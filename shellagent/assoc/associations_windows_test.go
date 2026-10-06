//go:build windows

package assoc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

func TestWindowsAssociationRemovalIsIdempotent(t *testing.T) {
	// Isolate all Classes writes from the user's actual file associations.
	path := fmt.Sprintf(`Software\DynAppAssociationTests\%d`, time.Now().UnixNano())
	root, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registry.DeleteKey(root, `Software\Classes`)
		registry.DeleteKey(root, `Software`)
		root.Close()
		registry.DeleteKey(registry.CURRENT_USER, path)
		registry.DeleteKey(registry.CURRENT_USER, `Software\DynAppAssociationTests`)
	})
	if err := removeWindowsAssociationsAt(root, "viewer"); err != nil {
		t.Fatalf("removal before first install: %v", err)
	}
	base := `Software\Classes\DynApp.viewer`
	key, _, err := registry.CreateKey(root, base, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.SetStringsValue("Extensions", []string{"md"}); err != nil {
		key.Close()
		t.Fatal(err)
	}
	key.Close()
	command, _, err := registry.CreateKey(root, base+`\shell\open\command`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	command.Close()
	extension, _, err := registry.CreateKey(root, `Software\Classes\.md`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.SetStringValue("", "DynApp.viewer"); err != nil {
		extension.Close()
		t.Fatal(err)
	}
	extension.Close()
	for i := 0; i < 2; i++ {
		if err := removeWindowsAssociationsAt(root, "viewer"); err != nil {
			t.Fatalf("removal attempt %d: %v", i+1, err)
		}
	}
	for _, removed := range []string{base, `Software\Classes\.md`} {
		key, err := registry.OpenKey(root, removed, registry.QUERY_VALUE)
		if err == nil {
			key.Close()
			t.Fatalf("registration still exists: %s", removed)
		}
		if !errors.Is(err, registry.ErrNotExist) {
			t.Fatal(err)
		}
	}
}
