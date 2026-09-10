package catalog

func Normalize(permissions []string) []string { return normalizePermissionSet(permissions) }

func Suggested(declared []string) []string { return suggestedPermissions(declared) }

func IsDangerous(permission string) bool { return dangerousPermission(permission) }

func Danger(permission string) string { return permissionDanger(permission) }

func GroupID(permission string) string { return permissionGroupID(permission) }

func Title(permission string) string { return permissionTitle(permission) }

func Describe(permissions []string) []Descriptor { return describePermissions(permissions) }

func Group(permissions []string) []GroupSummary { return groupPermissions(permissions) }

type Descriptor = permissionDescriptor
type GroupSummary = permissionGroupSummary
