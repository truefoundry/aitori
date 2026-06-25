package appresolve

import (
	"testing"

	"github.com/truefoundry/aitori/internal/config"
)

func testApps() []config.AppProfile {
	return []config.AppProfile{
		{
			ID:    "claude-desktop",
			Match: config.AppMatch{ProcessNames: []string{"Claude"}, BundleID: "com.anthropic.claudefordesktop"},
		},
		{
			ID:    "claude-web",
			Match: config.AppMatch{Browser: true, Hosts: []string{"claude.ai"}},
		},
	}
}

func TestResolveByProcess(t *testing.T) {
	r := New(testApps())
	app := r.Resolve(Flow{Host: "api.anthropic.com", HasProcess: true, ProcessName: "Claude"})
	if app == nil || app.ID != "claude-desktop" {
		t.Fatalf("got %v, want claude-desktop", app)
	}
}

func TestResolveByBundleID(t *testing.T) {
	r := New(testApps())
	app := r.Resolve(Flow{HasProcess: true, BundleID: "com.anthropic.claudefordesktop"})
	if app == nil || app.ID != "claude-desktop" {
		t.Fatalf("got %v, want claude-desktop", app)
	}
}

func TestResolveBySubBundleID(t *testing.T) {
	r := New(testApps())
	// Electron helper processes report a child bundle id; they must attribute
	// to the same profile as the main app's configured bundle id.
	app := r.Resolve(Flow{HasProcess: true, BundleID: "com.anthropic.claudefordesktop.helper.renderer"})
	if app == nil || app.ID != "claude-desktop" {
		t.Fatalf("got %v, want claude-desktop", app)
	}
}

func TestResolveUnrelatedBundleNoMatch(t *testing.T) {
	r := New(testApps())
	// A bundle that merely shares a prefix substring (not a dotted child) must
	// NOT match, and an unrelated bundle falls through to (here, no) host match.
	if app := r.Resolve(Flow{HasProcess: true, BundleID: "com.anthropic.claudefordesktopX"}); app != nil {
		t.Fatalf("expected no match for sibling-prefixed bundle, got %v", app)
	}
}

func TestResolveByExePrefix(t *testing.T) {
	apps := []config.AppProfile{{
		ID:    "claude-code",
		Match: config.AppMatch{ExePaths: []string{"/home/u/.local/share/claude/versions"}},
	}}
	r := New(apps)
	// A binary under the configured versioned directory matches.
	app := r.Resolve(Flow{HasProcess: true, ExePath: "/home/u/.local/share/claude/versions/2.1.183"})
	if app == nil || app.ID != "claude-code" {
		t.Fatalf("got %v, want claude-code", app)
	}
	// A sibling directory sharing a prefix substring must NOT match.
	if app := r.Resolve(Flow{HasProcess: true, ExePath: "/home/u/.local/share/claudeX/bin"}); app != nil {
		t.Fatalf("unexpected match for sibling path: %v", app)
	}
}

func TestResolveByHostBrowser(t *testing.T) {
	r := New(testApps())
	app := r.Resolve(Flow{Host: "claude.ai"})
	if app == nil || app.ID != "claude-web" {
		t.Fatalf("got %v, want claude-web", app)
	}
}

func TestResolveNoMatch(t *testing.T) {
	r := New(testApps())
	if app := r.Resolve(Flow{Host: "example.com"}); app != nil {
		t.Fatalf("expected no match, got %v", app)
	}
}
