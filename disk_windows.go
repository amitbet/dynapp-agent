//go:build windows

package shellagent

// Windows volume queries are added with the Windows installer/provider work.
// Returning null matches the existing Shell when statfs is unavailable.
func diskUsage(path string) (any, error) { return nil, nil }
