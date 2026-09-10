//go:build windows && !cgo

package desktop

// Keep non-CGo Windows builds usable. Production Windows builds that want
// delayed clipboard streams must enable CGo so the COM helper is linked in;
// this fallback makes the agent retain its existing eager clipboard path.
func platformFilePromisesSupported() bool { return false }
func RunFilePromiseHelper() error         { return nil }
