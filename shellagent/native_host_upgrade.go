package shellagent

import (
	"log"
	"time"
)

// nativeHostUpgradeInterval is how often the agent retries bundles it skipped
// because the app was running.
var nativeHostUpgradeInterval = 5 * time.Minute

// upgradeNativeHosts brings every installed bundle that is not running to the
// host this agent embeds (macOS only; elsewhere nothing is outdated). It
// returns the number of bundles still waiting because their app is open.
func (s *Server) upgradeNativeHosts() int {
	s.mu.Lock()
	apps := append([]NativeApp(nil), s.Config.NativeApps...)
	s.mu.Unlock()
	waiting := 0
	for _, app := range apps {
		if !nativeHostOutdated(app) {
			continue
		}
		if _, upgraded := s.upgradeNativeHost(app); !upgraded && nativeHostOutdated(app) {
			waiting++
		}
	}
	return waiting
}

// upgradeNativeHost upgrades one app's bundle when its host is outdated and
// the app is not running, and records the new signature. It returns the
// current record and whether the bundle was rewritten.
func (s *Server) upgradeNativeHost(app NativeApp) (NativeApp, bool) {
	if !nativeHostOutdated(app) {
		return app, false
	}
	s.nativeInstallMu.Lock()
	defer s.nativeInstallMu.Unlock()
	// An install may have replaced the record while this waited for the lock.
	current, ok := s.nativeApp(app.StoreID)
	if !ok || !nativeHostOutdated(current) || nativeAppRunning(current) {
		if ok {
			return current, false
		}
		return app, false
	}
	result, err := nativeUpgradeHost(current)
	if err != nil {
		log.Printf("DynApp Shell agent: could not upgrade the app host of %s: %v", current.StoreID, err)
		return current, false
	}
	current.Path, current.Executable, current.CDHash, current.BundleID = result.Path, result.Executable, result.CDHash, result.BundleID
	s.upsertNativeApp(current)
	log.Printf("DynApp Shell agent: upgraded the app host of %s", current.StoreID)
	return current, true
}

// nativeHostQuitWait bounds how long the agent waits for a host process to
// exit after its control channel closed.
var nativeHostQuitWait = 15 * time.Second

// upgradeNativeHostAfterQuit upgrades an app's outdated host as soon as its
// process has exited. A closed control channel can also mean an agent restart
// or a host reconnect, so it gives up while the app keeps running.
func (s *Server) upgradeNativeHostAfterQuit(storeID string) {
	app, ok := s.nativeApp(storeID)
	if !ok || !nativeHostOutdated(app) {
		return
	}
	deadline := time.Now().Add(nativeHostQuitWait)
	for nativeAppRunning(app) {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.upgradeNativeHost(app)
}

// startNativeHostUpgrades runs upgrades now and then periodically until stop
// closes, retrying apps that were open on the previous pass.
func (s *Server) startNativeHostUpgrades(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(nativeHostUpgradeInterval)
		defer ticker.Stop()
		for {
			s.upgradeNativeHosts()
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}
