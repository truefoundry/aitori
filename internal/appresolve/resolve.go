// Package appresolve attributes a decrypted flow to a configured app
// (plan §8 "appresolve"). Desktop apps are attributed by process identity
// (PID -> process name / bundle id / exe path); browser apps are attributed by
// host. In milestone M1 (explicit proxy, no PID), only host attribution is
// available; PID attribution is wired in M2+.
package appresolve

import (
	"path/filepath"
	"strings"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/hostmatch"
)

// Flow carries the signals available to attribute a request to an app.
type Flow struct {
	Host string // request host (always available)

	// Process identity (available once PID attribution is implemented).
	HasProcess  bool
	PID         int
	ProcessName string
	BundleID    string
	ExePath     string
}

// Resolver matches flows to app profiles.
type Resolver struct {
	apps []config.AppProfile
}

// New returns a Resolver over the given app profiles.
func New(apps []config.AppProfile) *Resolver { return &Resolver{apps: apps} }

// Resolve returns the app a flow belongs to, or nil if none matches.
//
// Order of preference:
//  1. process identity, when known;
//  2. host attribution, preferring browser-typed apps to reduce misattribution
//     between a desktop app and its web counterpart that share a host.
func (r *Resolver) Resolve(f Flow) *config.AppProfile {
	if f.HasProcess {
		for i := range r.apps {
			if matchProcess(&r.apps[i].Match, f) {
				return &r.apps[i]
			}
		}
	}

	if f.Host != "" {
		if app := r.hostMatch(f.Host, true); app != nil {
			return app
		}
		if app := r.hostMatch(f.Host, false); app != nil {
			return app
		}
	}
	return nil
}

func (r *Resolver) hostMatch(host string, browserOnly bool) *config.AppProfile {
	for i := range r.apps {
		app := &r.apps[i]
		if browserOnly != app.Match.Browser {
			continue
		}
		// Tag by the app's match.hosts.
		if len(app.Match.Hosts) > 0 && hostmatch.MatchAny(app.Match.Hosts, host) {
			return app
		}
	}
	return nil
}

func matchProcess(m *config.AppMatch, f Flow) bool {
	if m.BundleID != "" && f.BundleID != "" {
		// Match the exact bundle id, or any sub-bundle of it. Electron apps run
		// their network/renderer work in helper processes whose bundle id is a
		// child of the main app's (e.g. com.anthropic.claudefordesktop ->
		// com.anthropic.claudefordesktop.helper.renderer); those connections
		// must attribute to the same profile as the main app.
		want := strings.ToLower(m.BundleID)
		got := strings.ToLower(f.BundleID)
		if got == want || strings.HasPrefix(got, want+".") {
			return true
		}
	}
	for _, name := range m.ProcessNames {
		if strings.EqualFold(name, f.ProcessName) {
			return true
		}
	}
	for _, p := range m.ExePaths {
		if pathMatch(p, f.ExePath) {
			return true
		}
	}
	return false
}

// pathMatch reports whether exe equals pattern, or lives under pattern as a
// directory prefix. The prefix form matches apps installed under a versioned
// directory — e.g. Claude Code at ~/.local/share/claude/versions/<version>,
// whose binary is named after the version — so the rule survives upgrades.
func pathMatch(pattern, exe string) bool {
	if pattern == "" || exe == "" {
		return false
	}
	pattern = filepath.Clean(pattern)
	exe = filepath.Clean(exe)
	return pattern == exe || strings.HasPrefix(exe, pattern+string(filepath.Separator))
}
