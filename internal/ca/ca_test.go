package ca

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreatePersists(t *testing.T) {
	dir := t.TempDir()
	c1, err := LoadOrCreate(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, certFile)); err != nil {
		t.Errorf("ca cert not written: %v", err)
	}
	// Key file must be 0600 (POSIX permission bits; NTFS doesn't map them).
	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("ca key perm = %o, want 600", perm)
		}
	}

	// Reloading must return the same certificate.
	c2, err := LoadOrCreate(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.CertPEM()) != string(c2.CertPEM()) {
		t.Error("reloaded CA differs from persisted one")
	}
}

func TestLeafChainsToCA(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadOrCreate(dir, Options{Organization: "aitori-test"})
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := c.LeafForName("api.anthropic.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf.VerifyHostname("api.anthropic.com") != nil {
		t.Error("leaf does not verify hostname")
	}

	roots := x509.NewCertPool()
	roots.AddCert(c.Cert())
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: roots}); err != nil {
		t.Errorf("leaf does not chain to CA: %v", err)
	}
}

func TestLeafForIP(t *testing.T) {
	c, err := LoadOrCreate(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := c.LeafForName("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf.VerifyHostname("127.0.0.1") != nil {
		t.Error("IP leaf does not verify")
	}
}

func TestLeafCacheReturnsSame(t *testing.T) {
	c, err := LoadOrCreate(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.LeafForName("claude.ai")
	b, _ := c.LeafForName("claude.ai")
	if a != b {
		t.Error("expected cached leaf to be reused")
	}
}
