//go:build !darwin && !windows

package shellagent

func platformFilePromisesSupported() bool { return false }
func RunFilePromiseHelper() error         { return nil }
