package connect

func Entries(value any) []string { return httpConnectEntries(value) }

func Patterns(value any, scheme string) ([]string, error) { return httpConnectPatterns(value, scheme) }

func URLAllowed(raw string, patterns []string) bool { return urlAllowedByHTTPConnect(raw, patterns) }

func AssertURLAllowed(raw string, patterns []string) error {
	return assertURLAllowedByHTTPConnect(raw, patterns)
}
