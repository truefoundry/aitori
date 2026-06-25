package profiles

import (
	"testing"

	"github.com/truefoundry/aitori/internal/config"
)

// Apply should overlay built-in apps/hosts/inject and let user entries win by id.
func TestApplyOverlaysAndMerges(t *testing.T) {
	dis := false
	cfg := &config.Config{
		// User overrides the claude-code inject entry (disable it).
		Inject: []config.InjectEntry{{App: "claude-code", Mode: config.InjectModeSettings, Enabled: &dis}},
	}
	if err := Apply(cfg); err != nil {
		t.Fatal(err)
	}

	// Built-in hosts + apps came in.
	if len(cfg.InterceptHosts) == 0 {
		t.Fatal("expected built-in intercept_hosts")
	}
	if findApp(cfg, "claude-desktop") == nil || findApp(cfg, "chatgpt-web") == nil {
		t.Fatal("expected built-in apps (claude-desktop, chatgpt-web)")
	}

	// The user's claude-code inject entry wins (stays disabled, not duplicated).
	cc := injectFor(cfg, "claude-code")
	if cc == nil || cc.On() {
		t.Fatalf("user claude-code inject should win and be disabled: %+v", cc)
	}
	if count := countInject(cfg, "claude-code"); count != 1 {
		t.Fatalf("claude-code inject entry duplicated: %d", count)
	}
}

// The built-ins now carry scoped endpoints (paths/methods) and reroute them, so
// common apps are governed out of the box without a config file.
func TestBuiltinScopedEndpointsReroute(t *testing.T) {
	cfg := &config.Config{}
	if err := Apply(cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"api.anthropic.com": false, "claude.ai": false, "chatgpt.com": false, "api.openai.com": false}
	for i := range cfg.InterceptHosts {
		h := cfg.InterceptHosts[i]
		if _, ok := want[h.Host]; !ok {
			continue
		}
		want[h.Host] = true
		if !h.Scoped() {
			t.Errorf("%s: expected scoped (paths/methods), got %+v", h.Host, h)
		}
		if h.Action != config.ActionReroute || h.Category != config.CategoryLLM {
			t.Errorf("%s: expected reroute/llm, got action=%q category=%q", h.Host, h.Action, h.Category)
		}
	}
	for host, found := range want {
		if !found {
			t.Errorf("built-in intercept host %q missing", host)
		}
	}
}

func findApp(cfg *config.Config, id string) *config.AppProfile {
	for i := range cfg.Apps {
		if cfg.Apps[i].ID == id {
			return &cfg.Apps[i]
		}
	}
	return nil
}

func injectFor(cfg *config.Config, app string) *config.InjectEntry {
	for i := range cfg.Inject {
		if cfg.Inject[i].App == app {
			return &cfg.Inject[i]
		}
	}
	return nil
}

func countInject(cfg *config.Config, app string) int {
	n := 0
	for _, e := range cfg.Inject {
		if e.App == app {
			n++
		}
	}
	return n
}
