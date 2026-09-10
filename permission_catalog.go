package shellagent

import (
	"sort"
	"strings"
)

// This file mirrors shared/permissions/catalog.js. Keep the two in sync: the
// JavaScript catalog is the source of truth for the Dyner permission screen
// and this table only annotates the agent's `permissions.list` payload.

// DangerousPermissions are ordinary capabilities that the Dyner permission
// screen leaves unchecked by default. The agent never suggests them.
var DangerousPermissions = []string{
	"secrets.manage", "agent.session", "apps.publish",
}

func dangerousPermission(permission string) bool {
	for _, value := range DangerousPermissions {
		if value == permission {
			return true
		}
	}
	return false
}

// suggestedPermissions is the declared set minus the dangerous set.
func suggestedPermissions(declared []string) []string {
	result := []string{}
	for _, permission := range declared {
		if !dangerousPermission(permission) {
			result = append(result, permission)
		}
	}
	sort.Strings(result)
	return result
}

// permissionDescriptor is one `declared` entry in `permissions.list`.
type permissionDescriptor struct {
	Permission  string `json:"permission"`
	Group       string `json:"group"`
	Danger      string `json:"danger"`
	Description string `json:"description"`
}

func describePermissions(permissions []string) []permissionDescriptor {
	result := make([]permissionDescriptor, 0, len(permissions))
	for _, permission := range normalizePermissionSet(permissions) {
		groupID := permissionGroupID(permission)
		description := permissionTitle(permission)
		for _, group := range permissionGroups {
			if group.ID == groupID {
				description = permissionTitle(permission) + ". " + group.Summary
			}
		}
		result = append(result, permissionDescriptor{Permission: permission, Group: groupID, Danger: permissionDanger(permission), Description: description})
	}
	return result
}

// normalizePermissionSet trims, de-duplicates, and sorts permission ids.
func normalizePermissionSet(permissions []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, permission := range permissions {
		permission = strings.TrimSpace(permission)
		if permission == "" || seen[permission] {
			continue
		}
		seen[permission] = true
		result = append(result, permission)
	}
	sort.Strings(result)
	return result
}

type permissionGroup struct {
	ID, Title, Summary string
}

var permissionGroups = []permissionGroup{
	{"files", "Files you choose", "Open and save files through the system picker. The app only sees what you pick."},
	{"filesystem", "Files on this computer", "Read and change files and folders without asking each time."},
	{"execute", "Run programs", "Start other programs or stop processes on this computer."},
	{"network", "Network", "Talk to servers, websites, and devices on the network."},
	{"workspaces", "Workspaces", "Keep documents on this device and optionally share them with your other devices."},
	{"remote", "Remote environments", "Use a paired computer for files, network, and tools."},
	{"store", "Apps and account", "Browse, install, launch, and publish DynApps with your Dyner account."},
	{"agent", "Coding agent", "Let an assistant inspect and change this app on your behalf."},
	{"secrets", "Secrets", "Read and store credentials for this app."},
	{"device", "This device", "Clipboard, notifications, shortcuts, calendar, and window controls."},
}

var veryHighPermissions = map[string]bool{
	"screen.capture": true, "secrets.manage": true, "apps.publish": true,
	"apps.detach": true, "agent.session": true, "system.processes.terminate": true, "workspaces.shared.manage": true,
	"permissions.manage": true,
}

var highPermissions = map[string]bool{
	"fs.exec": true, "fs.execFile": true, "fs.execTerminal": true,
	"net.tcp.connect": true, "net.udp.connect": true, "net.tcp.listen": true, "net.udp.bind": true,
	"globalShortcut": true, "tray.manage": true,
}

var highPermissionPrefixes = []string{
	"fs.write", "fs.copy", "fs.pack", "fs.move", "fs.trash", "fs.remove", "fs.mkdir", "dialog.save",
	"apps.install", "apps.uninstall", "workspaces.shared.write", "shortcut.", "fileAssociations.",
}

var mediumPermissionPrefixes = []string{
	"fs.", "file.", "externalOpen.", "sessions.", "workspaces.", "remoteEnv.", "search.", "clipboard.",
	"calendar.", "system.ports.", "net.", "web.", "dialog.open", "dialog.pick",
}

var dangerRank = map[string]int{"low": 0, "medium": 1, "high": 2, "very-high": 3}

func permissionDanger(permission string) string {
	if veryHighPermissions[permission] {
		return "very-high"
	}
	if highPermissions[permission] {
		return "high"
	}
	for _, prefix := range highPermissionPrefixes {
		if strings.HasPrefix(permission, prefix) {
			return "high"
		}
	}
	for _, prefix := range mediumPermissionPrefixes {
		if strings.HasPrefix(permission, prefix) {
			return "medium"
		}
	}
	return "low"
}

func permissionGroupID(permission string) string {
	switch permission {
	case "fs.exec", "fs.execFile", "fs.execTerminal", "system.processes.terminate":
		return "execute"
	case "dialog.pickPath":
		return "filesystem"
	}
	switch {
	case strings.HasPrefix(permission, "fs."), strings.HasPrefix(permission, "search."):
		return "filesystem"
	case strings.HasPrefix(permission, "dialog.open"), strings.HasPrefix(permission, "dialog.save"),
		strings.HasPrefix(permission, "file."), strings.HasPrefix(permission, "externalOpen."):
		return "files"
	case strings.HasPrefix(permission, "net."):
		return "network"
	case strings.HasPrefix(permission, "workspaces."):
		return "workspaces"
	case strings.HasPrefix(permission, "remoteEnv."):
		return "remote"
	case strings.HasPrefix(permission, "apps."), strings.HasPrefix(permission, "oauth."), strings.HasPrefix(permission, "permissions."):
		return "store"
	case strings.HasPrefix(permission, "agent."):
		return "agent"
	case strings.HasPrefix(permission, "secrets."):
		return "secrets"
	}
	return "device"
}

func permissionTitle(permission string) string {
	var builder strings.Builder
	previousLower := false
	startOfWord := true
	for _, r := range permission {
		switch {
		case r == '.':
			builder.WriteByte(' ')
			startOfWord = true
			previousLower = false
			continue
		case r >= 'A' && r <= 'Z' && previousLower:
			builder.WriteByte(' ')
			startOfWord = true
		}
		if startOfWord && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		builder.WriteRune(r)
		previousLower = r >= 'a' && r <= 'z'
		startOfWord = false
	}
	return builder.String()
}

type permissionGroupSummary struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Danger      string   `json:"danger"`
	Permissions []string `json:"permissions"`
}

// groupPermissions buckets permission ids by user-facing group in catalog
// order, carrying the highest danger tier of each bucket.
func groupPermissions(permissions []string) []permissionGroupSummary {
	buckets := map[string]*permissionGroupSummary{}
	for _, permission := range permissions {
		permission = strings.TrimSpace(permission)
		if permission == "" {
			continue
		}
		groupID := permissionGroupID(permission)
		bucket := buckets[groupID]
		if bucket == nil {
			for _, group := range permissionGroups {
				if group.ID == groupID {
					bucket = &permissionGroupSummary{ID: group.ID, Title: group.Title, Summary: group.Summary, Danger: "low"}
				}
			}
			if bucket == nil {
				continue
			}
			buckets[groupID] = bucket
		}
		danger := permissionDanger(permission)
		if dangerRank[danger] > dangerRank[bucket.Danger] {
			bucket.Danger = danger
		}
		bucket.Permissions = append(bucket.Permissions, permission)
	}
	result := make([]permissionGroupSummary, 0, len(buckets))
	for _, group := range permissionGroups {
		if bucket := buckets[group.ID]; bucket != nil {
			sort.Strings(bucket.Permissions)
			result = append(result, *bucket)
		}
	}
	return result
}
