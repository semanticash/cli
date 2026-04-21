package platform

import (
	"net"
	"net/url"
	"strings"
)

// proxyEnvVars lists proxy URL environment variables. Matching is
// case-insensitive so detached children behave the same way across
// platforms, including Windows environments that preserve mixed-case
// names. NO_PROXY is omitted because it is an exclusion list, not a
// proxy URL.
var proxyEnvVars = map[string]struct{}{
	"HTTP_PROXY":  {},
	"HTTPS_PROXY": {},
	"ALL_PROXY":   {},
}

// WithoutLoopbackProxies returns a copy of env with any proxy variable
// whose value points at a literal loopback host removed. Other entries
// are preserved in their original order.
//
// This drops short-lived localhost proxies inherited from the parent
// process while keeping real forward proxies in place.
//
// Loopback recognition is syntactic only - no DNS resolution is
// performed, keeping the function deterministic and side-effect free
// on the spawn path. A host is treated as loopback when it is exactly
// "localhost" or when it parses as an IP inside 127.0.0.0/8 or equal
// to "::1". Scheme-less values like "127.0.0.1:64788" are parsed as
// if they had an "http://" prefix for inspection only.
func WithoutLoopbackProxies(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if isLoopbackProxyEntry(entry) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isLoopbackProxyEntry(entry string) bool {
	idx := strings.IndexByte(entry, '=')
	if idx <= 0 {
		return false
	}
	if _, ok := proxyEnvVars[strings.ToUpper(entry[:idx])]; !ok {
		return false
	}
	return isLoopbackProxyValue(entry[idx+1:])
}

func isLoopbackProxyValue(value string) bool {
	if value == "" {
		return false
	}
	host, ok := extractProxyHost(value)
	if !ok {
		return false
	}
	return isLoopbackHost(host)
}

// extractProxyHost returns the hostname portion of a proxy URL. It
// first tries to parse value as-is, then retries with an "http://"
// prefix to accommodate scheme-less forms such as "127.0.0.1:64788".
func extractProxyHost(value string) (string, bool) {
	if u, err := url.Parse(value); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Hostname(), true
	}
	if u, err := url.Parse("http://" + value); err == nil && u.Host != "" {
		return u.Hostname(), true
	}
	return "", false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}
