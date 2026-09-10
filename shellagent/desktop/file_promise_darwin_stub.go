//go:build darwin && !cgo

package desktop

// The release agent uses CGO_ENABLED=0 for portable cross-builds. Keep the
// eager clipboard path available when the AppKit delayed-file provider cannot
// be linked.
func platformFilePromisesSupported() bool { return false }
func RunFilePromiseHelper() error         { return nil }
