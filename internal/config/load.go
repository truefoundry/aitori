package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/truefoundry/aitori/internal/hostmatch"
	"github.com/truefoundry/aitori/internal/sysuser"
	"gopkg.in/yaml.v3"
)

// Load reads a YAML config file, overlays it on top of Default(), expands
// "~" home-relative paths, and validates the result. A nil/empty path returns
// the validated defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		// KnownFields rejects typos in the config, which is friendlier than
		// silently ignoring a misspelled key.
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
	}
	cfg.expandPaths()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Overrides carries command-line overrides for a loaded config. Empty string
// fields are ignored (the file/default value wins); NoAuth only ever forces
// auth off (it cannot re-enable a config that disables it).
type Overrides struct {
	GatewayURL  string
	HeaderCtx   string
	TokenFile   string
	Listen      string
	CADir       string
	Transparent *bool
	NoAuth      bool
	UIEnabled   *bool
	UIListen    string
}

// Apply overlays non-empty CLI overrides onto c, re-expanding "~" in any
// path-valued fields it touches. Callers should Validate afterwards.
func (c *Config) Apply(o Overrides) {
	if o.GatewayURL != "" {
		c.Gateway.URL = o.GatewayURL
	}
	if o.HeaderCtx != "" {
		c.Gateway.HeaderCtx = o.HeaderCtx
	}
	if o.TokenFile != "" {
		c.Gateway.Auth.TokenFile = ExpandPath(o.TokenFile)
	}
	if o.Listen != "" {
		c.Proxy.Listen = o.Listen
	}
	if o.CADir != "" {
		c.Proxy.CADir = ExpandPath(o.CADir)
	}
	if o.Transparent != nil {
		c.Proxy.Transparent = *o.Transparent
	}
	if o.NoAuth {
		c.Gateway.Auth.Disabled = true
	}
	if o.UIEnabled != nil {
		c.UI.Enabled = *o.UIEnabled
	}
	if o.UIListen != "" {
		c.UI.Listen = o.UIListen
	}
}

func (c *Config) expandPaths() {
	c.Proxy.CADir = ExpandPath(c.Proxy.CADir)
	c.Gateway.Auth.TokenFile = ExpandPath(c.Gateway.Auth.TokenFile)
	for i := range c.Sinks {
		c.Sinks[i].Path = ExpandPath(c.Sinks[i].Path)
	}
	for i := range c.Apps {
		ExpandAppPaths(&c.Apps[i])
	}
}

// ExpandAppPaths expands a leading "~" in an app profile's path-valued match
// fields (exe_paths), so a profile can name a home-relative install location
// (e.g. ~/.local/share/claude/versions) that resolves to the invoking user at
// load/up time — device-independent, and correct under sudo via sysuser.
func ExpandAppPaths(app *AppProfile) {
	for j := range app.Match.ExePaths {
		app.Match.ExePaths[j] = ExpandPath(app.Match.ExePaths[j])
	}
}

// ExpandPath expands a leading "~" to the invoking user's home directory.
// Under sudo this resolves SUDO_USER's home (via sysuser), not root's, so the
// CA dir, token file, and sinks land in — and point at — the real user's home.
func ExpandPath(p string) string {
	return sysuser.Expand(p)
}

// Validate checks the config for internal consistency. It returns a single
// error describing the first problem found.
func (c *Config) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("unsupported config version %d (this build understands version %d)", c.Version, SchemaVersion)
	}
	if c.Proxy.Listen == "" {
		return fmt.Errorf("proxy.listen must be set")
	}
	if c.UI.Enabled {
		if c.UI.Listen == "" {
			return fmt.Errorf("ui.listen must be set when ui.enabled is true")
		}
		if c.UI.Listen == c.Proxy.Listen {
			return fmt.Errorf("ui.listen %q must differ from proxy.listen", c.UI.Listen)
		}
	}

	switch c.Gateway.OnError {
	case FailOpen, FailClosed:
	default:
		return fmt.Errorf("gateway.on_error %q invalid (want fail_open|fail_closed)", c.Gateway.OnError)
	}
	if c.Gateway.HeaderToken == "" || c.Gateway.HeaderOrig == "" || c.Gateway.HeaderCtx == "" {
		return fmt.Errorf("gateway.header_token, gateway.header_orig_url, and gateway.header_ctx must be set")
	}
	for k := range c.Gateway.Headers {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("gateway.headers contains an empty header name")
		}
	}

	var gwHost string
	if c.Gateway.URL != "" {
		u, err := url.Parse(c.Gateway.URL)
		if err != nil {
			return fmt.Errorf("gateway.url %q invalid: %w", c.Gateway.URL, err)
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("gateway.url %q must be http or https", c.Gateway.URL)
		}
		if u.Host == "" {
			return fmt.Errorf("gateway.url %q must include a host", c.Gateway.URL)
		}
		gwHost = u.Hostname()
	}
	if _, err := time.ParseDuration(c.DialTimeoutOrDefault()); err != nil {
		return fmt.Errorf("gateway.dial_timeout %q invalid: %w", c.Gateway.DialTimeout, err)
	}

	// Loop prevention (plan §13): the gateway host must never be in the
	// selective-MITM allowlist, or the agent would intercept its own
	// gateway-bound traffic.
	if gwHost != "" && hostmatch.MatchAny(c.InterceptHosts.Patterns(), gwHost) {
		return fmt.Errorf("gateway host %q must not appear in intercept_hosts (loop prevention)", gwHost)
	}
	for i := range c.InterceptHosts {
		h := &c.InterceptHosts[i]
		if h.Host == "" {
			return fmt.Errorf("intercept_hosts[%d]: host must be set", i)
		}
		if err := validateAction(fmt.Sprintf("intercept_hosts[%d]", i), h.Action); err != nil {
			return err
		}
		if err := validateCategory(fmt.Sprintf("intercept_hosts[%d]", i), h.Category, true); err != nil {
			return err
		}
	}

	for i := range c.Rules {
		if err := validateRule(fmt.Sprintf("rules[%d]", i), &c.Rules[i]); err != nil {
			return err
		}
	}

	for i := range c.Inject {
		e := &c.Inject[i]
		if e.App == "" {
			return fmt.Errorf("inject[%d]: app must be set", i)
		}
		switch e.Mode {
		case "", InjectModeSettings:
		default:
			return fmt.Errorf("inject[%d] (%s): mode %q invalid (want settings)", i, e.App, e.Mode)
		}
	}

	seen := make(map[string]bool, len(c.Apps))
	for i := range c.Apps {
		app := &c.Apps[i]
		if app.ID == "" {
			return fmt.Errorf("apps[%d]: id must be set", i)
		}
		if seen[app.ID] {
			return fmt.Errorf("duplicate app id %q", app.ID)
		}
		seen[app.ID] = true
	}

	for i, s := range c.Sinks {
		switch s.Type {
		case "stdout", "file":
		default:
			return fmt.Errorf("sinks[%d]: type %q invalid (want stdout|file)", i, s.Type)
		}
		if s.Type == "file" && s.Path == "" {
			return fmt.Errorf("sinks[%d]: file sink requires a path", i)
		}
	}
	return nil
}

func validateRule(where string, r *Rule) error {
	// Category is optional: a rule may set only an action and let the heuristic
	// (or the rule's own action) classify. If set it must be valid.
	if err := validateCategory(where, r.Category, true); err != nil {
		return err
	}
	if r.Category == "" && r.Action == "" {
		return fmt.Errorf("%s: a rule must set category and/or action", where)
	}
	return validateAction(where, r.Action)
}

func validateAction(where, action string) error {
	switch action {
	case "", ActionReroute, ActionPassthrough, ActionBlock:
		return nil
	default:
		return fmt.Errorf("%s: action %q invalid (want reroute|passthrough|block)", where, action)
	}
}

// validateCategory checks a category value; optional=true allows "".
func validateCategory(where, category string, optional bool) error {
	switch category {
	case CategoryLLM, CategoryMCP, CategoryOther:
		return nil
	case "":
		if optional {
			return nil
		}
		return fmt.Errorf("%s: category must be set (llm|mcp|other)", where)
	default:
		return fmt.Errorf("%s: category %q invalid", where, category)
	}
}

// DialTimeoutOrDefault returns the configured dial timeout string, or "5s".
func (c *Config) DialTimeoutOrDefault() string {
	if c.Gateway.DialTimeout == "" {
		return "5s"
	}
	return c.Gateway.DialTimeout
}

// DialTimeout returns the parsed gateway dial timeout. Validate guarantees it
// parses, so callers may ignore the (always-nil) error in practice.
func (c *Config) DialTimeout() time.Duration {
	d, err := time.ParseDuration(c.DialTimeoutOrDefault())
	if err != nil {
		return 5 * time.Second
	}
	return d
}

// DrainTimeout returns the parsed graceful-shutdown drain timeout, defaulting
// to 5s when unset or unparseable.
func (c *Config) DrainTimeout() time.Duration {
	if c.Proxy.DrainTimeout == "" {
		return 5 * time.Second
	}
	d, err := time.ParseDuration(c.Proxy.DrainTimeout)
	if err != nil || d <= 0 {
		return 5 * time.Second
	}
	return d
}

// GatewayURL parses the configured gateway URL. It returns (nil, nil) when no
// gateway is configured.
func (c *Config) GatewayURL() (*url.URL, error) {
	if c.Gateway.URL == "" {
		return nil, nil
	}
	return url.Parse(c.Gateway.URL)
}

// GatewayHost returns the gateway hostname (no port), or "".
func (c *Config) GatewayHost() string {
	u, err := c.GatewayURL()
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}
