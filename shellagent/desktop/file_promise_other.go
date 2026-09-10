//go:build !darwin && !windows

package desktop

func platformFilePromisesSupported() bool { return false }
func RunFilePromiseHelper() error         { return nil }
