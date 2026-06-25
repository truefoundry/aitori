//go:build darwin

package proc

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var bundleIDRe = regexp.MustCompile(`(?s)<key>CFBundleIdentifier</key>\s*<string>([^<]+)</string>`)

// bundleID derives the macOS bundle identifier for an executable inside a .app
// by reading CFBundleIdentifier from the bundle's Info.plist. Returns "" when
// the executable is not part of an app bundle.
func bundleID(exe string) string {
	app := appBundleDir(exe)
	if app == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	return parseBundleID(data)
}

// appBundleDir walks up from an executable path to the nearest *.app directory.
func appBundleDir(exe string) string {
	dir := exe
	for dir != "" && dir != "/" && dir != "." {
		if strings.HasSuffix(dir, ".app") {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func parseBundleID(plist []byte) string {
	m := bundleIDRe.FindSubmatch(plist)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}
