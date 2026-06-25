//go:build !darwin

package proc

// bundleID is macOS-specific; on other platforms there is no app bundle.
func bundleID(string) string { return "" }
