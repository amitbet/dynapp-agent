//go:build !windows && (!darwin || !cgo)

package desktop

import "errors"

func presentationSupported() bool  { return false }
func screenCaptureSupported() bool { return false }
func launchPresentation(string, []string, string) (func(), error) {
	return nil, errors.New("native presentation unsupported")
}
func runNativePresentation(<-chan presentationCommand, func(presentationReply)) error {
	return errors.New("native presentation unsupported")
}
