//go:build linux

package desktop

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const chromiumPWARepairDebounce = 250 * time.Millisecond

func StartRepair(home string) context.CancelFunc {
	if !chromiumPWALaunchNeedsRepair() {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	repair := chromiumPWARepair{home: home}
	go repair.loop(ctx)
	return cancel
}

func chromiumPWALaunchNeedsRepair() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if os.Getenv("container") != "" {
		return true
	}
	return wrappedChromiumPath() != ""
}

func wrappedChromiumPath() string {
	path := "/usr/local/bin/wrapped-chromium"
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

func (repair chromiumPWARepair) loop(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("DynApp Shell agent: Chromium PWA shortcut watcher unavailable: %v", err)
		return
	}
	defer watcher.Close()
	repair.apply("startup")
	repair.watch(watcher)
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if chromiumPWAWatchEvent(event) {
				debounce.Reset(chromiumPWARepairDebounce)
			}
			if event.Has(fsnotify.Create) {
				repair.watch(watcher)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			if err != nil {
				log.Printf("DynApp Shell agent: Chromium PWA shortcut watch error: %v", err)
			}
		case <-debounce.C:
			repair.apply("watch")
			repair.watch(watcher)
		}
	}
}

func (repair chromiumPWARepair) apply(reason string) {
	if patched := repair.once(); patched > 0 {
		log.Printf("DynApp Shell agent: repaired %d Chromium PWA shortcut(s) after %s", patched, reason)
	}
}

func (repair chromiumPWARepair) watch(watcher *fsnotify.Watcher) {
	seen := map[string]bool{}
	for _, dir := range repair.dirs() {
		for _, path := range []string{dir, filepath.Dir(dir)} {
			if path == "" || path == "." || seen[path] {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				continue
			}
			seen[path] = true
			_ = watcher.Add(path)
		}
	}
}

func chromiumPWAWatchEvent(event fsnotify.Event) bool {
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		return false
	}
	name := filepath.Base(event.Name)
	if isChromiumPWADesktopName(name) {
		return true
	}
	return name == "Desktop" || name == "applications"
}
