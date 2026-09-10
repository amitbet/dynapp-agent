package assoc

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestMacDefaultAssociationsCommand(t *testing.T) {
	previous := RunMacAssociationCommand
	t.Cleanup(func() { RunMacAssociationCommand = previous })
	RunMacAssociationCommand = func(args ...string) ([]byte, error) {
		if len(args) != 9 || args[0] != "-l" || args[1] != "JavaScript" || args[3] != macAssociationScript || args[5] != "apply" || args[6] != "test.viewer" || args[8] != "/Applications/Viewer.app" {
			t.Fatalf("unexpected command arguments: %#v", args)
		}
		var extensions []string
		if err := json.Unmarshal([]byte(args[7]), &extensions); err != nil || !reflect.DeepEqual(extensions, []string{"md"}) {
			t.Fatalf("extensions: %v, %v", extensions, err)
		}
		return []byte(`["md"]`), nil
	}
	got, err := macDefaultAssociations("apply", "test.viewer", []string{"md"}, "/Applications/Viewer.app")
	if err != nil || !reflect.DeepEqual(got, []string{"md"}) {
		t.Fatalf("result: %v, %v", got, err)
	}
	RunMacAssociationCommand = func(...string) ([]byte, error) { return []byte("denied"), errors.New("exit 1") }
	if _, err := macDefaultAssociations("apply", "test.viewer", []string{"md"}, "/Applications/Viewer.app"); err == nil {
		t.Fatal("command failure ignored")
	}
	RunMacAssociationCommand = func(...string) ([]byte, error) { return []byte("invalid"), nil }
	if _, err := macDefaultAssociations("get", "test.viewer", []string{"md"}); err == nil {
		t.Fatal("invalid result ignored")
	}
}
