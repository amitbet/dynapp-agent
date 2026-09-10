package shellagent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// httpAllowlistFor returns the connect patterns for a request. Paired sockets
// use the identity's approved allowlist; sockets without an identity (relay,
// direct handler calls) fall back to the request's declared list.
func httpAllowlistFor(request message, scheme string, options map[string]any) ([]string, error) {
	if request.auth != nil && request.auth.keyID != "" {
		patterns := request.auth.connect[scheme]
		if len(patterns) == 0 {
			return nil, fmt.Errorf("HTTP destination is not in the app's declared connect list")
		}
		return patterns, nil
	}
	return httpConnectPatterns(options["connect"], scheme)
}

// hostNamedByAllowlist reports whether a connect pattern names this exact
// host. Only then may a loopback, link-local, or private address be reached.
func hostNamedByAllowlist(hostname string, patterns []string) bool {
	hostname = strings.ToLower(strings.Trim(hostname, "[]"))
	for _, pattern := range patterns {
		parsed, err := url.Parse(pattern)
		if err != nil {
			continue
		}
		if strings.ToLower(strings.Trim(parsed.Hostname(), "[]")) == hostname {
			return true
		}
	}
	return false
}

// restrictedDialIP covers loopback, link-local (169.254/16, fe80::/10),
// RFC1918 private ranges, and unspecified addresses.
func restrictedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func restrictedHostname(hostname string) bool {
	if loopbackHostname(hostname) {
		return true
	}
	if ip := net.ParseIP(strings.Trim(hostname, "[]")); ip != nil {
		return restrictedDialIP(ip)
	}
	return false
}

// assertHTTPTargetAllowed applies the allowlist and the SSRF denylist to one
// URL (initial request or redirect hop).
func assertHTTPTargetAllowed(target *url.URL, patterns []string) error {
	if err := assertURLAllowedByHTTPConnect(target.String(), patterns); err != nil {
		return err
	}
	if restrictedHostname(target.Hostname()) && !hostNamedByAllowlist(target.Hostname(), patterns) {
		return fmt.Errorf("HTTP destination %s is a local or private address that the app did not declare", target.Hostname())
	}
	return nil
}

// safeHTTPTransport re-checks the resolved addresses at dial time so a public
// hostname cannot resolve to a private address (DNS rebinding).
func safeHTTPTransport(patterns []string) *http.Transport {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			named := hostNamedByAllowlist(host, patterns)
			var last error
			for _, candidate := range addresses {
				if restrictedDialIP(candidate.IP) && !named {
					last = fmt.Errorf("HTTP destination %s resolves to a local or private address", host)
					continue
				}
				connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
				if err == nil {
					return connection, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("HTTP destination %s has no usable address", host)
			}
			return nil, last
		},
	}
}

func (s *Server) handleHTTPRPC(ctx context.Context, request message) (any, error) {
	options := objectArg(request.Args, 0)
	parsed, err := url.Parse(stringValue(options["url"]))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("HTTP URL is invalid")
	}
	patterns, err := httpAllowlistFor(request, parsed.Scheme, options)
	if err != nil {
		return nil, err
	}
	if err := assertHTTPTargetAllowed(parsed, patterns); err != nil {
		return nil, err
	}
	if request.Method == "download" && !request.allows("fs.writeBase64") {
		return nil, errors.New("http.download requires fs.writeBase64")
	}
	timeout := time.Duration(integer64(options["timeoutMs"])) * time.Millisecond
	if timeout < time.Second {
		timeout = 30 * time.Second
	}
	maximum := 60 * time.Second
	if request.Method == "download" {
		maximum = 10 * time.Minute
	}
	if timeout > maximum {
		timeout = maximum
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body := strings.NewReader(stringValue(options["body"]))
	if body.Size() > 1024*1024 {
		return nil, errors.New("HTTP request body is limited to 1 MiB")
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, strings.ToUpper(stringValue(options["method"])), parsed.String(), body)
	if err != nil {
		return nil, err
	}
	if httpRequest.Method == "" {
		httpRequest.Method = http.MethodGet
	}
	if headers, ok := options["headers"].(map[string]any); ok {
		for key, value := range headers {
			httpRequest.Header.Set(key, stringValue(value))
		}
	}
	transport := safeHTTPTransport(patterns)
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many HTTP redirects")
			}
			return assertHTTPTargetAllowed(req.URL, patterns)
		},
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	headers := map[string][]string(response.Header)
	if request.Method == "request" {
		binary, _ := options["binary"].(bool)
		limit := 8 * 1024 * 1024
		if binary {
			limit = 32 * 1024 * 1024
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
		if err != nil {
			return nil, err
		}
		if len(data) > limit {
			return nil, errors.New("HTTP response is too large")
		}
		result := map[string]any{"status": response.StatusCode, "ok": response.StatusCode >= 200 && response.StatusCode < 300, "url": response.Request.URL.String(), "headers": headers, "text": string(data)}
		if binary {
			result["text"] = ""
			result["base64"] = base64.StdEncoding.EncodeToString(data)
		}
		return result, nil
	}
	if request.Method != "download" {
		return nil, errors.New("unsupported HTTP method")
	}
	target := stringValue(options["targetPath"])
	if !filepath.IsAbs(target) {
		return nil, errors.New("download target path must be absolute")
	}
	maximumBytes := integer64(options["maxBytes"])
	if maximumBytes <= 0 || maximumBytes > 512*1024*1024 {
		maximumBytes = 512 * 1024 * 1024
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".download-*")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, err := io.Copy(temporary, io.LimitReader(response.Body, maximumBytes+1))
	closeErr := temporary.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if written > maximumBytes {
		return nil, errors.New("HTTP download exceeds its size limit")
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return nil, err
	}
	return map[string]any{"status": response.StatusCode, "url": response.Request.URL.String(), "headers": headers, "bytes": written, "path": target}, nil
}
