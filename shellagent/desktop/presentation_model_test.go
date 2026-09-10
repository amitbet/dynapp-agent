package desktop

import (
	"reflect"
	"strings"
	"testing"
)

type fakePresentation struct {
	registered map[uint32]presentationKey
	tray       *trayOptions
	reject     bool
}

func (f *fakePresentation) setTray(options trayOptions) error { f.tray = &options; return nil }
func (f *fakePresentation) destroyTray()                      { f.tray = nil }
func (f *fakePresentation) balloon(string, string) bool       { return f.tray != nil }
func (f *fakePresentation) register(id uint32, key presentationKey) bool {
	if f.reject {
		return false
	}
	if f.registered == nil {
		f.registered = map[uint32]presentationKey{}
	}
	f.registered[id] = key
	return true
}
func (f *fakePresentation) unregister(id uint32) { delete(f.registered, id) }

func TestPresentationShortcutParsing(t *testing.T) {
	for _, entry := range []struct {
		input, platform string
		key             presentationKey
	}{
		{"CommandOrControl+Shift+A", "darwin", presentationKey{65, 12}},
		{"CmdOrCtrl+Alt+F19", "windows", presentationKey{130, 3}},
		{"Control+Option+Space", "darwin", presentationKey{32, 3}},
		{"Super+PageDown", "windows", presentationKey{34, 8}},
	} {
		got, err := parsePresentationKey(entry.input, entry.platform)
		if err != nil || got != entry.key {
			t.Errorf("%s = %+v, %v", entry.input, got, err)
		}
	}
	for _, input := range []string{"A", "Ctrl+", "Unknown+A", "Ctrl+A+B", "Ctrl+F21", "Ctrl+MediaPlayPause"} {
		if _, err := parsePresentationKey(input, "windows"); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
func TestPresentationShortcutOwnershipAndFailure(t *testing.T) {
	native := &fakePresentation{}
	p := &presentationController{native: native, platform: "darwin"}
	call := func(method, key string) any {
		t.Helper()
		result, err := p.handle(presentationCommand{Service: "globalShortcut", Method: method, Args: []any{key}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if call("register", "Cmd+Shift+A") != true {
		t.Fatal("registration failed")
	}
	if call("register", "Command+Shift+A") != false {
		t.Fatal("alias double registration succeeded")
	}
	call("unregister", "Cmd+Shift+B")
	if len(native.registered) != 1 {
		t.Fatal("unregister removed another shortcut")
	}
	if p.hotkey(1) == nil || p.hotkey(2) != nil {
		t.Fatal("event ownership mismatch")
	}
	call("unregister", "Command+Shift+A")
	if len(native.registered) != 0 {
		t.Fatal("alias unregister failed")
	}
	native.reject = true
	if call("register", "Ctrl+B") != false || len(p.shortcuts) != 0 {
		t.Fatal("failed native registration retained state")
	}
	native.reject = false
	call("register", "Ctrl+C")
	p.close()
	if len(native.registered) != 0 {
		t.Fatal("close leaked shortcut")
	}
}
func TestPresentationTrayValidationAndUpdates(t *testing.T) {
	native := &fakePresentation{}
	p := &presentationController{native: native, platform: "windows"}
	call := func(method string, value any) error {
		_, err := p.handle(presentationCommand{Service: "tray", Method: method, Args: []any{value}})
		return err
	}
	if err := call("create", map[string]any{"tooltip": "Original", "menu": []any{map[string]any{"id": "toggle", "label": "Toggle", "type": "checkbox"}}}); err != nil {
		t.Fatal(err)
	}
	if err := call("setMenu", []any{map[string]any{"id": "same"}, map[string]any{"id": "same"}}); err == nil {
		t.Fatal("duplicate ids accepted")
	}
	if native.tray.Menu[0].ID != "toggle" {
		t.Fatal("invalid update changed native menu")
	}
	if err := call("setTitle", "Title"); err != nil {
		t.Fatal(err)
	}
	if native.tray.Tooltip != "Original" || len(native.tray.Menu) != 1 {
		t.Fatal("partial update discarded tray state")
	}
	event := p.menuClick("toggle")
	if !reflect.DeepEqual(event, map[string]any{"id": "toggle", "checked": true}) {
		t.Fatalf("event=%v", event)
	}
	if p.menuClick("other") != nil {
		t.Fatal("unknown menu event delivered")
	}
	if err := validateTray(trayOptions{Menu: make([]trayItem, 129)}); err == nil {
		t.Fatal("oversized menu accepted")
	}
	if err := call("create", map[string]any{"tooltip": strings.Repeat("x", 1025)}); err == nil {
		t.Fatal("oversized text accepted")
	}
	p.close()
	if native.tray != nil {
		t.Fatal("close leaked tray")
	}
}
