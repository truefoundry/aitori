// Package profiles provides the built-in default app profiles, embedded into
// the binary. MDM-managed config is overlaid on top of these defaults so that,
// in the common case, an operator only needs to push a gateway URL and token
// path (plan §14).
package profiles

import (
	_ "embed"
	"fmt"

	"github.com/truefoundry/aitori/internal/config"
	"gopkg.in/yaml.v3"
)

//go:embed builtin.yaml
var builtinYAML []byte

// builtin is the parsed fragment: intercept_hosts, apps, rules, and inject are
// meaningful.
type builtin struct {
	InterceptHosts config.InterceptHosts `yaml:"intercept_hosts"`
	Apps           []config.AppProfile   `yaml:"apps"`
	Rules          []config.Rule         `yaml:"rules"`
	Inject         []config.InjectEntry  `yaml:"inject"`
}

// Apply overlays the embedded built-in profiles onto cfg: it unions
// intercept_hosts (by host pattern), appends built-in rules, and adds any
// built-in app whose ID is not already present. User/managed config always wins
// on conflicts.
func Apply(cfg *config.Config) error {
	var b builtin
	if err := yaml.Unmarshal(builtinYAML, &b); err != nil {
		return fmt.Errorf("parse builtin profiles: %w", err)
	}

	have := make(map[string]bool, len(cfg.Apps))
	for _, a := range cfg.Apps {
		have[a.ID] = true
	}
	for _, a := range b.Apps {
		if !have[a.ID] {
			// Built-ins are added after config.Load's path expansion, so expand
			// their home-relative exe_paths here.
			config.ExpandAppPaths(&a)
			cfg.Apps = append(cfg.Apps, a)
		}
	}

	seen := make(map[string]bool, len(cfg.InterceptHosts))
	for _, h := range cfg.InterceptHosts {
		seen[h.Host] = true
	}
	for _, h := range b.InterceptHosts {
		if !seen[h.Host] {
			cfg.InterceptHosts = append(cfg.InterceptHosts, h)
			seen[h.Host] = true
		}
	}

	cfg.Rules = append(cfg.Rules, b.Rules...)

	haveInject := make(map[string]bool, len(cfg.Inject))
	for _, e := range cfg.Inject {
		haveInject[e.App] = true
	}
	for _, e := range b.Inject {
		if !haveInject[e.App] {
			cfg.Inject = append(cfg.Inject, e)
		}
	}
	return nil
}
