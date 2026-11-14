//go:build !windows

package shellagent

import "os"

func replaceConfigFile(from, to string) error { return os.Rename(from, to) }
