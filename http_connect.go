package shellagent

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func parseHTTPConnectPattern(value string, expectedScheme string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme+":" != expectedScheme || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("HTTP connect pattern is invalid")
	}
	if strings.Contains(parsed.Hostname(), "*") {
		return "", fmt.Errorf("HTTP connect pattern is invalid")
	}
	pathname := parsed.Path
	if pathname == "" {
		pathname = "/"
	}
	decoded, err := url.PathUnescape(pathname)
	if err != nil {
		return "", fmt.Errorf("HTTP connect pattern is invalid")
	}
	if !strings.HasPrefix(decoded, "/") {
		decoded = "/" + decoded
	}
	if strings.ContainsAny(decoded, "?*") {
		return "", fmt.Errorf("HTTP connect pattern is invalid")
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." {
			return "", fmt.Errorf("HTTP connect pattern is invalid")
		}
	}
	return parsed.Scheme + "://" + parsed.Host + decoded, nil
}

func httpConnectEntries(value any) []string {
	switch list := value.(type) {
	case []any:
		entries := make([]string, 0, len(list))
		for _, entry := range list {
			text, _ := entry.(string)
			entries = append(entries, text)
		}
		return entries
	case []string:
		return list
	default:
		return nil
	}
}

func httpConnectPatterns(value any, scheme string) ([]string, error) {
	list := httpConnectEntries(value)
	if len(list) == 0 {
		return nil, fmt.Errorf("HTTP destination is not in the app's declared connect list")
	}
	seen := map[string]struct{}{}
	var patterns []string
	for _, text := range list {
		pattern, err := parseHTTPConnectPattern(text, scheme+":")
		if err != nil {
			return nil, err
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return patterns, nil
}

func urlAllowedByHTTPConnect(raw string, patterns []string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil {
		return false
	}
	pathname := parsed.Path
	if pathname == "" {
		pathname = "/"
	}
	decoded, err := url.PathUnescape(pathname)
	if err != nil {
		return false
	}
	if !strings.HasPrefix(decoded, "/") {
		decoded = "/" + decoded
	}
	for _, part := range strings.Split(decoded, "/") {
		if part == ".." {
			return false
		}
	}
	origin := parsed.Scheme + "://" + parsed.Host
	for _, pattern := range patterns {
		allowed, err := url.Parse(pattern)
		if err != nil {
			continue
		}
		if allowed.Scheme+"://"+allowed.Host != origin {
			continue
		}
		prefix := allowed.Path
		if prefix == "" {
			prefix = "/"
		}
		if prefix == "/" || decoded == prefix {
			return true
		}
		if strings.HasSuffix(prefix, "/") && decoded == strings.TrimSuffix(prefix, "/") {
			return true
		}
		directory := prefix
		if !strings.HasSuffix(directory, "/") {
			directory += "/"
		}
		if strings.HasPrefix(decoded, directory) {
			return true
		}
	}
	return false
}

func assertURLAllowedByHTTPConnect(raw string, patterns []string) error {
	if urlAllowedByHTTPConnect(raw, patterns) {
		return nil
	}
	return fmt.Errorf("HTTP destination is not in the app's declared connect list: %s", raw)
}
