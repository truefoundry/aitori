package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTracingLoadAndEnvExpansion(t *testing.T) {
	t.Setenv("TFY_API_KEY", "secret-token")
	path := writeCfg(t, `
version: 1
tracing:
  enabled: true
  endpoint: https://internal.example.com/api/otel
  protocol: http
  service_name: aitori
  headers:
    Authorization: "Bearer ${TFY_API_KEY}"
    TFY-Tracing-Project: "truefoundry:tracing-project:demo"
  categories: [llm, mcp]
  batch:
    max_export_batch_size: 128
    batch_timeout: 3s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Tracing.Enabled {
		t.Fatal("tracing should be enabled")
	}
	if got := cfg.Tracing.Headers["Authorization"]; got != "Bearer secret-token" {
		t.Errorf("env expansion failed: %q", got)
	}
	if cfg.Tracing.ProtocolOrDefault() != TracingProtocolHTTP {
		t.Errorf("protocol = %q", cfg.Tracing.ProtocolOrDefault())
	}
}

func TestTracingEnvDefault(t *testing.T) {
	os.Unsetenv("MISSING_TOKEN")
	path := writeCfg(t, `
version: 1
tracing:
  enabled: true
  endpoint: https://internal.example.com/api/otel
  headers:
    Authorization: "Bearer ${MISSING_TOKEN:-fallback}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Tracing.Headers["Authorization"]; got != "Bearer fallback" {
		t.Errorf("default expansion failed: %q", got)
	}
}

func TestTracingValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"missing endpoint", "version: 1\ntracing:\n  enabled: true\n", true},
		{"bad protocol", "version: 1\ntracing:\n  enabled: true\n  endpoint: https://h/otel\n  protocol: kafka\n", true},
		{"bad category", "version: 1\ntracing:\n  enabled: true\n  endpoint: https://h/otel\n  categories: [nope]\n", true},
		{"bad sample ratio", "version: 1\ntracing:\n  enabled: true\n  endpoint: https://h/otel\n  sample_ratio: 2\n", true},
		{"disabled ignores missing endpoint", "version: 1\ntracing:\n  enabled: false\n", false},
		{"valid", "version: 1\ntracing:\n  enabled: true\n  endpoint: https://h/otel\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
