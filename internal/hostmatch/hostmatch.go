// Package hostmatch implements the host-pattern matching used throughout
// aitori: the global selective-MITM gate (intercept_hosts), per-rule host
// lists, and gateway-host comparisons.
//
// Three pattern shapes are supported, all matched case-insensitively and with
// any :port stripped from both sides:
//
//   - exact:       "api.anthropic.com"  matches only that host.
//   - subdomain:   "*.anthropic.com"    matches "x.anthropic.com" and
//     "a.b.anthropic.com" but NOT the apex "anthropic.com".
//   - suffix:      ".anthropic.com"     matches the apex "anthropic.com" and
//     any subdomain of it.
package hostmatch

import "strings"

// Normalize lowercases a host and strips a trailing :port if present.
// IPv6 literals in brackets (e.g. "[::1]:443") are reduced to "::1".
func Normalize(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return host
	}
	// Bracketed IPv6, optionally with a port: [::1] or [::1]:443.
	if host[0] == '[' {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			return host[1:i]
		}
		return host
	}
	// Strip :port only when there is exactly one colon (i.e. not a bare IPv6).
	if strings.Count(host, ":") == 1 {
		if i := strings.IndexByte(host, ':'); i >= 0 {
			return host[:i]
		}
	}
	return host
}

// Match reports whether host matches a single pattern.
func Match(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	host = Normalize(host)
	if pattern == "" || host == "" {
		return false
	}

	switch {
	case strings.HasPrefix(pattern, "*."):
		base := pattern[2:]
		return strings.HasSuffix(host, "."+base)
	case strings.HasPrefix(pattern, "."):
		base := pattern[1:]
		return host == base || strings.HasSuffix(host, "."+base)
	default:
		return host == pattern
	}
}

// MatchAny reports whether host matches any of the patterns.
func MatchAny(patterns []string, host string) bool {
	for _, p := range patterns {
		if Match(p, host) {
			return true
		}
	}
	return false
}
