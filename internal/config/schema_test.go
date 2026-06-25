package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInterceptHostsUnmarshal(t *testing.T) {
	var c Config
	y := `
intercept_hosts:
  - api.anthropic.com
  - { host: "*.cursor.sh", action: passthrough }
`
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.InterceptHosts) != 2 {
		t.Fatalf("got %d entries", len(c.InterceptHosts))
	}
	if c.InterceptHosts[0].Host != "api.anthropic.com" || c.InterceptHosts[0].Action != "" {
		t.Fatalf("bare string entry: %+v", c.InterceptHosts[0])
	}
	if c.InterceptHosts[1].Host != "*.cursor.sh" || c.InterceptHosts[1].Action != ActionPassthrough {
		t.Fatalf("object entry: %+v", c.InterceptHosts[1])
	}
	if got := c.InterceptHosts.Patterns(); !reflect.DeepEqual(got, []string{"api.anthropic.com", "*.cursor.sh"}) {
		t.Fatalf("Patterns() = %v", got)
	}
}

func TestInjectEntryOnAndValidation(t *testing.T) {
	fa := false
	if !(InjectEntry{App: "x"}).On() {
		t.Error("unset enabled should default to On")
	}
	if (InjectEntry{App: "x", Enabled: &fa}).On() {
		t.Error("enabled:false should be off")
	}

	good := Default()
	good.Inject = []InjectEntry{{App: "claude-code", Mode: InjectModeSettings}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid inject rejected: %v", err)
	}
	bad := Default()
	bad.Inject = []InjectEntry{{App: "x", Mode: "bogus"}}
	if err := bad.Validate(); err == nil {
		t.Error("invalid inject mode should be rejected")
	}
	noApp := Default()
	noApp.Inject = []InjectEntry{{Mode: InjectModeSettings}}
	if err := noApp.Validate(); err == nil {
		t.Error("inject entry without app should be rejected")
	}
}

// Deprecated fields (capture_mode, capture_tier, app-scoped rules) are no longer
// part of the schema; with KnownFields(true) a config carrying them is rejected.
func TestDeprecatedFieldsRejected(t *testing.T) {
	for _, y := range []string{
		"version: 1\nproxy: {listen: 127.0.0.1:8080, capture_mode: tier2}\n",
		"version: 1\nproxy: {listen: 127.0.0.1:8080}\napps:\n  - {id: x, match: {browser: true}, capture_tier: tier1}\n",
		"version: 1\nproxy: {listen: 127.0.0.1:8080}\napps:\n  - id: x\n    match: {browser: true}\n    rules: [{name: r, category: llm}]\n",
	} {
		dec := yaml.NewDecoder(strings.NewReader(y))
		dec.KnownFields(true)
		var c Config
		if err := dec.Decode(&c); err == nil {
			t.Errorf("expected strict decode to reject deprecated field in:\n%s", y)
		}
	}
}
