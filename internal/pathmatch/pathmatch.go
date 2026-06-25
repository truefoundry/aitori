// Package pathmatch implements the glob matching used for per-host/path rule
// scoping (intercept_hosts[].paths and rules[].path_patterns).
//
// Two wildcards are supported; matching is case-sensitive and anchored (the
// whole path must match the whole pattern):
//
//   - "*"  matches any run of non-separator characters (one path segment).
//   - "**" matches any run of characters, including "/".
//
// Examples:
//
//	"/v1/messages"                                  exact
//	"/api/organizations/*/chat_conversations/*/x"   each * is one UUID segment
//	"/backend-api/codex/**"                         the whole subtree
package pathmatch

import "strings"

// Match reports whether path matches the glob pattern.
func Match(pattern, path string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == path
	}
	return glob(pattern, path)
}

// MatchAny reports whether path matches any of the patterns.
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

// glob matches pat against s with full backtracking. Recursion (not a single
// backtrack slot) is what lets a "**" and a later "*" each try every split
// independently — e.g. "/a/**/*.go" vs "/a/b/c/x.go", where "**" must consume
// "b/c" while the trailing "*" consumes "x". Paths and patterns are short, so
// the branching is bounded in practice.
//
//   - "**" matches any run of characters, including "/" and the empty run.
//   - "*"  matches a run of non-"/" characters (one path segment), incl. empty.
func glob(pat, s string) bool {
	for len(pat) > 0 {
		if pat[0] == '*' {
			if len(pat) > 1 && pat[1] == '*' {
				// "**": try consuming 0..len(s) chars (any char) for the rest.
				rest := pat[2:]
				for i := 0; ; i++ {
					if glob(rest, s[i:]) {
						return true
					}
					if i >= len(s) {
						return false
					}
				}
			}
			// "*": try consuming 0..n chars, stopping at the first "/".
			rest := pat[1:]
			for i := 0; ; i++ {
				if glob(rest, s[i:]) {
					return true
				}
				if i >= len(s) || s[i] == '/' {
					return false
				}
			}
		}
		if len(s) == 0 || pat[0] != s[0] {
			return false
		}
		pat, s = pat[1:], s[1:]
	}
	return len(s) == 0
}
