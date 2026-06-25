package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandAppPaths(t *testing.T) {
	app := AppProfile{Match: AppMatch{ExePaths: []string{"~/.local/share/claude/versions", "/abs/bin", "rel"}}}
	ExpandAppPaths(&app)

	if strings.HasPrefix(app.Match.ExePaths[0], "~") {
		t.Fatalf("~ not expanded: %q", app.Match.ExePaths[0])
	}
	// ToSlash so the suffix check holds regardless of OS path separator.
	if !strings.HasSuffix(filepath.ToSlash(app.Match.ExePaths[0]), "/.local/share/claude/versions") {
		t.Fatalf("expanded path lost its suffix: %q", app.Match.ExePaths[0])
	}
	if app.Match.ExePaths[1] != "/abs/bin" || app.Match.ExePaths[2] != "rel" {
		t.Fatalf("non-tilde paths must be unchanged: %v", app.Match.ExePaths)
	}
}
