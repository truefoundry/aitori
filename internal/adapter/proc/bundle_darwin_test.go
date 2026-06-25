//go:build darwin

package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBundleID(t *testing.T) {
	plist := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>Claude</string>
	<key>CFBundleIdentifier</key>
	<string>com.anthropic.claudefordesktop</string>
</dict>
</plist>`)
	if got := parseBundleID(plist); got != "com.anthropic.claudefordesktop" {
		t.Errorf("parseBundleID = %q", got)
	}
}

func TestParseBundleIDMissing(t *testing.T) {
	if got := parseBundleID([]byte(`<plist><dict></dict></plist>`)); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBundleIDFromAppBundle(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Claude.app")
	contents := filepath.Join(app, "Contents")
	macos := filepath.Join(contents, "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<plist><dict><key>CFBundleIdentifier</key><string>com.anthropic.claudefordesktop</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(macos, "Claude")
	if got := bundleID(exe); got != "com.anthropic.claudefordesktop" {
		t.Errorf("bundleID(%q) = %q", exe, got)
	}
}

func TestAppBundleDirNotABundle(t *testing.T) {
	if got := appBundleDir("/usr/bin/curl"); got != "" {
		t.Errorf("expected empty for non-bundle, got %q", got)
	}
}
