//go:build !windows

package shellagent

import "context"

// macOS and Linux have no per-network trust setting the agent could read.
func readPlatformNetworkProfiles(context.Context) ([]networkProfile, error) {
	return nil, errNetworkCategoryUnsupported
}
