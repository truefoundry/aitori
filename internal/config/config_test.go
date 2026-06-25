package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	// configs/conversations.yaml lives at the repo root.
	path := filepath.Join("..", "..", "configs", "conversations.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(conversations.yaml) failed: %v", err)
	}
	if cfg.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", cfg.Version, SchemaVersion)
	}
	if cfg.Gateway.HeaderToken != "x-tfy-api-key" {
		t.Errorf("header_token = %q", cfg.Gateway.HeaderToken)
	}
	if len(cfg.Apps) == 0 {
		t.Error("expected apps in example config")
	}
}

func TestDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "min.yaml")
	// Minimal config: defaults should fill the rest.
	if err := os.WriteFile(p, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}
	if cfg.Proxy.Listen != "127.0.0.1:8080" {
		t.Errorf("default listen not applied: %q", cfg.Proxy.Listen)
	}
	if cfg.Gateway.OnError != FailOpen {
		t.Errorf("default on_error not applied: %q", cfg.Gateway.OnError)
	}
	if cfg.DialTimeout().String() != "5s" {
		t.Errorf("default dial_timeout = %s", cfg.DialTimeout())
	}
}

func TestDrainTimeout(t *testing.T) {
	if got := (&Config{}).DrainTimeout(); got.String() != "5s" {
		t.Errorf("unset drain_timeout = %s, want 5s", got)
	}
	c := &Config{Proxy: ProxyConfig{DrainTimeout: "20s"}}
	if got := c.DrainTimeout(); got.String() != "20s" {
		t.Errorf("drain_timeout = %s, want 20s", got)
	}
	bad := &Config{Proxy: ProxyConfig{DrainTimeout: "nonsense"}}
	if got := bad.DrainTimeout(); got.String() != "5s" {
		t.Errorf("invalid drain_timeout = %s, want 5s fallback", got)
	}
}

func TestValidateRejectsBadVersion(t *testing.T) {
	c := Default()
	c.Version = 2
	if err := c.Validate(); err == nil {
		t.Error("expected error for unsupported version")
	}
}

func TestValidateRejectsGatewayInInterceptHosts(t *testing.T) {
	c := Default()
	c.Gateway.URL = "https://gw.example.com"
	c.InterceptHosts = Hosts("*.example.com")
	if err := c.Validate(); err == nil {
		t.Error("expected loop-prevention error when gateway host matches intercept_hosts")
	}
}

func TestValidateRejectsBadCategory(t *testing.T) {
	c := Default()
	c.Rules = []Rule{{Name: "r", Category: "bogus"}}
	if err := c.Validate(); err == nil {
		t.Error("expected error for invalid category")
	}
}

func TestValidateRejectsDuplicateAppID(t *testing.T) {
	c := Default()
	c.Apps = []AppProfile{
		{ID: "dup", Match: AppMatch{Browser: true}},
		{ID: "dup", Match: AppMatch{Browser: true}},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for duplicate app id")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := ExpandPath("~/.aitori"); got != filepath.Join(home, ".aitori") {
		t.Errorf("ExpandPath(~/.aitori) = %q", got)
	}
	if got := ExpandPath("/abs/path"); got != "/abs/path" {
		t.Errorf("ExpandPath(/abs/path) = %q", got)
	}
}
