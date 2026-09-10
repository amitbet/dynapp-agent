package shellagent

import "github.com/amitbet/dynapp-agent/shellagent/desktop"

func presentationSupported() bool { return desktop.Supported() }

func launchPresentation(exe string, args []string, token string) (func(), error) {
	return desktop.Launch(exe, args, token)
}

func platformFilePromisesSupported() bool { return desktop.FilePromisesSupported() }

func RunFilePromiseHelper() error { return desktop.RunFilePromiseHelper() }

func RunPresentationHelper(address, token string) error {
	return desktop.RunHelper(address, token)
}

func (s *Server) startChromiumDesktopRepair() {
	cancel := desktop.StartRepair(userHome())
	if cancel == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chromiumRepairCancel != nil {
		cancel()
		return
	}
	s.chromiumRepairCancel = cancel
}

func screenCaptureSupported() bool { return desktop.ScreenCaptureSupported() }
