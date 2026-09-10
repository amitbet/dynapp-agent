package desktop

func Supported() bool { return presentationSupported() }

func Launch(exe string, args []string, token string) (func(), error) {
	return launchPresentation(exe, args, token)
}

func FilePromisesSupported() bool { return platformFilePromisesSupported() }

func ScreenCaptureSupported() bool { return screenCaptureSupported() }
