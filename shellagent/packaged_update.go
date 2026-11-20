package shellagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A package manager (apt) that installed the agent under /usr/bin owns the
// binary: the agent runs as the user and cannot replace it. The package ships
// dynapp-agent-upgrade.timer, which runs apt as root; the agent watches its
// executable and restarts into each newer version apt puts there.

type packagedInstall struct {
	Manager    string // "apt"
	Executable string
}

const packagedUpgradeHint = "this agent was installed with apt, which upgrades it: dynapp-agent-upgrade.timer checks hourly. To upgrade now, run: sudo systemctl start dynapp-agent-upgrade.service"

// packagedCheckInterval is how often the agent looks for a binary apt
// replaced. A stat; the version is read only when the file changed.
var packagedCheckInterval = time.Minute

// detectPackagedInstall and installedAgentVersion are replaceable in tests.
var detectPackagedInstall = packagedInstallForExecutable
var installedAgentVersion = readInstalledAgentVersion

func readInstalledAgentVersion(ctx context.Context, executable string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", executable, err)
	}
	return normalizedReleaseVersion(string(output)), nil
}

func (s *Server) packagedInstall() *packagedInstall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packaged
}

func (s *Server) runPackagedWatcher(ctx context.Context, config SelfUpdateConfig, install packagedInstall) {
	var seen os.FileInfo
	check := func() {
		info, err := os.Stat(install.Executable)
		if err != nil {
			return // apt is between unpacking and renaming
		}
		if seen != nil && os.SameFile(seen, info) && info.ModTime().Equal(seen.ModTime()) && info.Size() == seen.Size() {
			return
		}
		seen = info
		if s.restartIntoPackagedUpgrade(ctx, config, install) {
			seen = nil // look again next tick
		}
	}
	check()
	ticker := time.NewTicker(packagedCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// restartIntoPackagedUpgrade restarts the agent when the installed binary is
// newer than the running one. It reports whether to retry later.
func (s *Server) restartIntoPackagedUpgrade(ctx context.Context, config SelfUpdateConfig, install packagedInstall) (retry bool) {
	version, err := installedAgentVersion(ctx, install.Executable)
	if err != nil {
		s.updateFailed("read the version %s installed: %v", install.Manager, err)
		return true
	}
	if !newerRelease(version, config.Version) {
		return false
	}
	log.Printf("self-update: %s installed %s; running %s", install.Manager, version, config.Version)
	force, proceed := s.waitForIdleBridges(ctx, config.bridgeUpdateDelay())
	if !proceed {
		return true
	}
	if !s.tryBeginUpdate(force) {
		return true
	}
	if err := config.OnRestart(ctx, version); err != nil {
		s.cancelUpdate()
		s.updateFailed("restarting into %s failed: %v", version, err)
		return true
	}
	return false
}

func (s *Server) updateFailed(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	log.Printf("DynApp Shell agent: %s", message)
	s.setUpdateError(message)
}

func (s *Server) setUpdateError(message string) {
	s.updateMu.Lock()
	s.updateError = strings.TrimSpace(message)
	s.updateMu.Unlock()
}

func (s *Server) lastUpdateError() string {
	s.updateMu.RLock()
	defer s.updateMu.RUnlock()
	return s.updateError
}
