package token

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseBare(t *testing.T) {
	d := parse([]byte("  secret-token\n"))
	if d.Token != "secret-token" {
		t.Errorf("Token = %q, want secret-token", d.Token)
	}
}

func TestParseEmpty(t *testing.T) {
	if d := parse([]byte("   \n")); d.Token != "" {
		t.Errorf("expected empty token, got %q", d.Token)
	}
}

func TestFileSourceMissing(t *testing.T) {
	fs, err := NewFileSource(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if fs.State() != StateNoToken {
		t.Errorf("state = %q, want no-token", fs.State())
	}
}

func TestFileSourceLiveReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	fs, err := NewFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	if fs.Get() != "first" {
		t.Fatalf("initial token = %q, want first", fs.Get())
	}

	changed := make(chan struct{}, 1)
	fs.mu.Lock()
	fs.onChange = func(d Data) {
		if d.Token == "second" {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
	fs.mu.Unlock()

	// Atomic write via temp file + rename, the common pattern for secret writers.
	tmp := filepath.Join(dir, "token.tmp")
	if err := os.WriteFile(tmp, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatalf("token did not refresh; still %q", fs.Get())
	}
	if fs.Get() != "second" {
		t.Errorf("after reload Get() = %q, want second", fs.Get())
	}
}
