//go:build darwin

package assoc

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OpenHostedApp launches the installed Chrome PWA for the URL. File
// associations must not silently fall back to the user's default browser:
// doing so makes Finder appear to be opening the wrong application.
func OpenHostedApp(appURL string, appName ...string) error {
	if bundleID := installedChromePWA(appURL, appName...); bundleID != "" {
		return exec.Command("open", "-b", bundleID, appURL).Start()
	}
	return fmt.Errorf("installed Chrome PWA not found for %s", appURL)
}

func installedChromePWA(appURL string, appName ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	patterns := []string{
		filepath.Join(home, "Applications", "Chrome Apps.localized", "*.app"),
		filepath.Join("/Applications", "Chrome Apps.localized", "*.app"),
		filepath.Join(home, "Applications", "*.app"),
		filepath.Join("/Applications", "*.app"),
	}
	displayName := ""
	if len(appName) > 0 {
		displayName = normalizeAppName(appName[0])
	}
	nameMatch := ""
	for _, pattern := range patterns {
		apps, _ := filepath.Glob(pattern)
		for _, app := range apps {
			plist := filepath.Join(app, "Contents", "Info.plist")
			shortcutURL, urlErr := plistRawValue(plist, "CrAppModeShortcutURL")
			bundleID, err := plistRawValue(plist, "CFBundleIdentifier")
			if err != nil || !strings.HasPrefix(bundleID, "com.google.Chrome.app.") {
				continue
			}
			if urlErr == nil && sameHostedAppURL(shortcutURL, appURL) {
				return bundleID
			}
			if displayName != "" && nameMatch == "" {
				for _, key := range []string{"CFBundleDisplayName", "CFBundleName"} {
					candidate, readErr := plistRawValue(plist, key)
					if readErr == nil && normalizeAppName(candidate) == displayName {
						nameMatch = bundleID
						break
					}
				}
			}
		}
	}
	return nameMatch
}

func normalizeAppName(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(value), "_", " ")), " "))
}

func plistRawValue(path, key string) (string, error) {
	output, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", path).Output()
	return strings.TrimSpace(string(output)), err
}

func sameHostedAppURL(left, right string) bool {
	a, errA := url.Parse(left)
	b, errB := url.Parse(right)
	if errA != nil || errB != nil {
		return false
	}
	for _, candidateA := range hostedURLCandidates(a) {
		for _, candidateB := range hostedURLCandidates(b) {
			if strings.EqualFold(candidateA.Scheme, candidateB.Scheme) &&
				strings.EqualFold(candidateA.Host, candidateB.Host) &&
				strings.TrimRight(candidateA.Path, "/") == strings.TrimRight(candidateB.Path, "/") {
				return true
			}
		}
	}
	return false
}

func hostedURLCandidates(value *url.URL) []*url.URL {
	base := *value
	base.RawQuery = ""
	base.Fragment = ""
	candidates := []*url.URL{&base}
	host := strings.ToLower(base.Hostname())
	if (host == "dynapp.io" || host == "www.dynapp.io") && strings.HasPrefix(base.Path, "/app/") {
		parts := strings.Split(strings.Trim(base.Path, "/"), "/")
		if len(parts) == 3 && parts[1] != "" && parts[2] != "" {
			canonical := base
			canonical.Host = strings.ReplaceAll(strings.ToLower(parts[1]), "-", "") + "-" + strings.ToLower(parts[2]) + ".dynapp.io"
			canonical.Path = "/"
			candidates = append(candidates, &canonical)
		}
	}
	return candidates
}
