package fs

func CreateZip(sources []string, target string, options map[string]any) error {
	return createZip(sources, target, options)
}

func WatchSnapshot(root string) (map[string]any, error) { return watchSnapshot(root) }

func OpenPath(path string) error { return openDesktopPath(path) }

func OpenPathWith(path, opener string) error { return openDesktopPathWith(path, opener) }

func OpenTerminal(command, cwd string) error { return openTerminal(command, cwd) }
