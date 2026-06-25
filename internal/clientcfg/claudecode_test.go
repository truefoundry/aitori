package clientcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func readObj(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return obj
}

var testWant = map[string]string{
	"HTTP_PROXY":          "http://127.0.0.1:8080",
	"HTTPS_PROXY":         "http://127.0.0.1:8080",
	"NO_PROXY":            "localhost,127.0.0.1,::1",
	"NODE_EXTRA_CA_CERTS": "/home/u/.aitori/ca.pem",
}

// When settings.json did not exist, inject creates it and revert removes it.
func TestInjectRevert_NoPriorFile(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "settings.json")
	bp := filepath.Join(dir, "backup.json")

	if err := injectAt(sp, bp, testWant, -1, -1); err != nil {
		t.Fatal(err)
	}
	obj := readObj(t, sp)
	env := obj["env"].(map[string]any)
	if env["HTTPS_PROXY"] != "http://127.0.0.1:8080" {
		t.Fatalf("HTTPS_PROXY not set: %v", env["HTTPS_PROXY"])
	}

	if err := revertAt(sp, bp, -1, -1); err != nil {
		t.Fatal(err)
	}
	if fileExists(sp) {
		t.Fatal("settings.json should have been removed (it did not exist before inject)")
	}
	if fileExists(bp) {
		t.Fatal("backup should have been removed")
	}
}

// Inject must preserve unrelated settings and unrelated env vars; revert must
// restore the file to exactly its prior state.
func TestInjectRevert_PreservesAndRestores(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "settings.json")
	bp := filepath.Join(dir, "backup.json")

	orig := map[string]any{
		"model": "claude-opus-4-8",
		"env": map[string]any{
			"FOO":         "bar",
			"HTTPS_PROXY": "http://corp-proxy:3128", // a pre-existing managed key
		},
	}
	data, _ := json.MarshalIndent(orig, "", "  ")
	if err := os.WriteFile(sp, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := injectAt(sp, bp, testWant, -1, -1); err != nil {
		t.Fatal(err)
	}
	obj := readObj(t, sp)
	if obj["model"] != "claude-opus-4-8" {
		t.Fatalf("unrelated top-level setting lost: %v", obj["model"])
	}
	env := obj["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Fatalf("unrelated env var lost: %v", env["FOO"])
	}
	if env["HTTPS_PROXY"] != "http://127.0.0.1:8080" {
		t.Fatalf("HTTPS_PROXY not overridden: %v", env["HTTPS_PROXY"])
	}

	if err := revertAt(sp, bp, -1, -1); err != nil {
		t.Fatal(err)
	}
	obj = readObj(t, sp)
	env = obj["env"].(map[string]any)
	if obj["model"] != "claude-opus-4-8" || env["FOO"] != "bar" {
		t.Fatalf("revert clobbered unrelated settings: %v", obj)
	}
	if env["HTTPS_PROXY"] != "http://corp-proxy:3128" {
		t.Fatalf("revert did not restore prior HTTPS_PROXY: %v", env["HTTPS_PROXY"])
	}
	if _, ok := env["NODE_EXTRA_CA_CERTS"]; ok {
		t.Fatalf("revert left an aitori-added key: %v", env)
	}
	if fileExists(bp) {
		t.Fatal("backup should have been removed")
	}
}

// target prefers a writable managed path, else the per-user file.
func TestAppTarget(t *testing.T) {
	managedDir := t.TempDir() // writable
	a := app{id: "x", managedPath: filepath.Join(managedDir, "managed-settings.json"), userRel: ".x/settings.json"}
	if got := a.target("/home/u"); got != a.managedPath {
		t.Fatalf("writable managed should win: %q", got)
	}

	// Managed parent is a regular file -> MkdirAll fails even as root -> user path.
	notDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/u", ".x/settings.json")
	a2 := app{id: "x", managedPath: filepath.Join(notDir, "managed-settings.json"), userRel: ".x/settings.json"}
	if got := a2.target("/home/u"); got != want {
		t.Fatalf("unwritable managed should fall back to user: got %q want %q", got, want)
	}

	// No managed path configured -> user path.
	a3 := app{id: "x", managedPath: "", userRel: ".x/settings.json"}
	if got := a3.target("/home/u"); got != want {
		t.Fatalf("no managed -> user: %q", got)
	}
}

func TestDefaultAppAndAbsoluteUserTarget(t *testing.T) {
	if _, ok := defaultApp("claude-code"); !ok {
		t.Fatal("claude-code should have a built-in default")
	}
	if _, ok := defaultApp("nope"); ok {
		t.Fatal("unknown app should have no default")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX absolute path / separators; managed-path logic is exercised on Unix")
	}
	// An absolute override user path is used verbatim (no managed path).
	a := app{id: "x", userRel: "/tmp/abs/settings.json"}
	if got := a.target("/home/u"); got != "/tmp/abs/settings.json" {
		t.Fatalf("absolute userRel target = %q", got)
	}
}

// Revert is a no-op when nothing was injected.
func TestRevert_NoBackup(t *testing.T) {
	dir := t.TempDir()
	if err := revertAt(filepath.Join(dir, "settings.json"), filepath.Join(dir, "backup.json"), -1, -1); err != nil {
		t.Fatalf("revert without backup should be a no-op, got %v", err)
	}
}
