//go:build linux && !android

package shellagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackagedInstallFollowsDpkgFileList(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	previous := dpkgInfoDir
	dpkgInfoDir = t.TempDir()
	t.Cleanup(func() { dpkgInfoDir = previous })
	if _, ok := packagedInstallForExecutable(); ok {
		t.Fatal("detected apt without a dpkg list")
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dpkgInfoDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dynapp-shell-agent-other.list", executable+"\n")
	write("dynapp-shell-agent.list", "/.\n/usr\n/usr/bin\n/usr/bin/dynapp-shell-agent\n")
	if _, ok := packagedInstallForExecutable(); ok {
		t.Fatal("an agent outside the package was taken for the packaged one")
	}
	write("dynapp-shell-agent:amd64.list", "/usr/bin\n"+executable+"\n")
	install, ok := packagedInstallForExecutable()
	if !ok || install.Manager != "apt" || install.Executable != executable {
		t.Fatalf("install = %+v %v", install, ok)
	}
}
