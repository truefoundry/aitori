// Package config defines aitori's configuration schema and the logic to load,
// validate, and overlay it. The schema is intentionally declarative: new apps
// and endpoints are governed by editing config, not code (see project plan §8).
//
// The model has three independent concerns: intercept_hosts (what to decrypt),
// apps (how to tag/attribute a decrypted request), and rules (path-level policy
// overrides). Apps no longer gate interception — they are labels.
package config

import (
	"time"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only config major version this build understands.
// The loader rejects any other version (plan §18: "loader rejects unknown
// major versions").
const SchemaVersion = 1

// Config is the top-level agent configuration.
type Config struct {
	Version int           `yaml:"version"`
	Proxy   ProxyConfig   `yaml:"proxy"`
	Gateway GatewayConfig `yaml:"gateway"`
	// InterceptHosts is the selective-MITM allowlist: the hosts to decrypt and
	// govern. Each entry is a bare host pattern, or {host, action, category} to
	// attach a default policy to that host. Everything not listed is raw-spliced.
	InterceptHosts InterceptHosts `yaml:"intercept_hosts"`
	// Apps attribute/tag a decrypted request to an app (by process or, for
	// browsers, by host). They are labels, not interception gates.
	Apps []AppProfile `yaml:"apps"`
	// Rules are optional path-level policy overrides, matched against the request
	// (host/path/method/body), independent of which app made it. Evaluated before
	// per-host actions and the body heuristic.
	Rules []Rule `yaml:"rules,omitempty"`
	// Inject is the opt-in, per-app client-config injection list (settings-env or
	// base-URL), for clients that ignore the system proxy. Applied on `up`,
	// reverted on `down`. Built-in defaults are overlaid by app id.
	Inject []InjectEntry `yaml:"inject,omitempty"`
	Sinks  []SinkConfig  `yaml:"sinks"` // optional local debug
	// Tracing is the optional OpenTelemetry tracing exporter. When enabled,
	// aitori turns each intercepted LLM/MCP call into an OTel span (GenAI
	// semantic conventions) and exports it to a configured OTLP receiver via a
	// batch processor — an alternative to a gateway for capturing spans.
	Tracing TracingConfig `yaml:"tracing,omitempty"`
	// UI is the optional embedded live-traffic view (off by default). When
	// enabled, the agent serves a self-contained page showing exchanges flowing
	// through it — useful as a no-gateway demo / local observability.
	UI UIConfig `yaml:"ui,omitempty"`
	// BuiltinProfiles controls whether the embedded default app profiles and
	// intercept hosts (internal/profiles/builtin.yaml) are overlaid under this
	// config. Default true. Set false to govern ONLY the apps/hosts listed here
	// — otherwise the built-ins union in extra hosts (e.g. *.cursor.sh) and
	// apps you may not want intercepted.
	BuiltinProfiles *bool `yaml:"builtin_profiles,omitempty"`
}

// UseBuiltinProfiles reports whether the built-in profile overlay should be
// applied. It defaults to true when unset (back-compat).
func (c *Config) UseBuiltinProfiles() bool {
	return c.BuiltinProfiles == nil || *c.BuiltinProfiles
}

// InterceptHosts is the decrypt allowlist. Entries unmarshal from either a bare
// string ("api.anthropic.com") or a mapping ({host, action, category}).
type InterceptHosts []InterceptHost

// InterceptHost is one allowlist entry: a host pattern with an optional default
// policy. action/category are applied to requests on that host unless a
// top-level rule matches first. When paths (glob patterns) and/or methods are
// set, the policy applies ONLY to matching requests, and every other path on
// that host passes through — so the entry doubles as the per-host trace scope
// and no separate passthrough rule is needed.
type InterceptHost struct {
	Host     string   `yaml:"host"`
	Action   string   `yaml:"action,omitempty"`   // reroute | passthrough | block
	Category string   `yaml:"category,omitempty"` // llm | mcp | other
	Paths    []string `yaml:"paths,omitempty"`    // glob path patterns (* = one segment, ** = any)
	Methods  []string `yaml:"methods,omitempty"`  // HTTP methods (case-insensitive)
}

// Scoped reports whether the entry restricts its policy to specific
// paths/methods (vs. applying to the whole host).
func (h InterceptHost) Scoped() bool { return len(h.Paths) > 0 || len(h.Methods) > 0 }

// UnmarshalYAML accepts a bare string or a {host, action, category, paths,
// methods} mapping.
func (h *InterceptHost) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&h.Host)
	}
	type raw InterceptHost
	var r raw
	if err := node.Decode(&r); err != nil {
		return err
	}
	*h = InterceptHost(r)
	return nil
}

// Hosts builds an InterceptHosts from bare host patterns (no per-host policy).
func Hosts(patterns ...string) InterceptHosts {
	out := make(InterceptHosts, len(patterns))
	for i, p := range patterns {
		out[i] = InterceptHost{Host: p}
	}
	return out
}

// Patterns returns the host patterns (for the decrypt gate, loop-prevention,
// status).
func (hs InterceptHosts) Patterns() []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		if h.Host != "" {
			out = append(out, h.Host)
		}
	}
	return out
}

// ProxyConfig controls the local listener and MITM behavior.
type ProxyConfig struct {
	Listen string `yaml:"listen"` // e.g. 127.0.0.1:8080
	CADir  string `yaml:"ca_dir"` // default ~/.aitori
	// Transparent opts into transparent capture (Linux): the OS redirects
	// outbound traffic to the proxy instead of an explicit system-proxy setting.
	Transparent bool `yaml:"transparent"`
	MaxBodyKB   int  `yaml:"max_body_kb"`  // request-body buffer cap (classification + fail-open retry)
	ForceHTTP11 bool `yaml:"force_http11"` // advertise only http/1.1 on the MITM leg
	// DrainTimeout bounds how long a graceful shutdown waits for in-flight
	// requests before forcing exit and reverting OS state (e.g. "5s").
	DrainTimeout string `yaml:"drain_timeout,omitempty"`
}

// UIConfig configures the embedded live-traffic view. Listen is a host:port for
// a small HTTP server separate from the proxy listener.
type UIConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	Listen  string `yaml:"listen,omitempty"` // e.g. 127.0.0.1:9100
}

// GatewayConfig describes the AI gateway dependency.
type GatewayConfig struct {
	URL         string `yaml:"url"`             // https://gateway.example.com/api/llm
	HeaderToken string `yaml:"header_token"`    // default "x-tfy-api-key"
	HeaderOrig  string `yaml:"header_orig_url"` // default "x-tfy-original-url"
	HeaderCtx   string `yaml:"header_ctx"`      // consolidated context header; default "x-tfy-metadata"
	OnError     string `yaml:"on_error"`        // fail_open (default) | fail_closed
	DialTimeout string `yaml:"dial_timeout"`    // e.g. "5s"
	// Auth holds the gateway credential. The token is the gateway's identity and
	// is meaningful only on the reroute leg, so it lives here rather than at the
	// top level.
	Auth AuthConfig `yaml:"auth"`
	// Headers are extra static headers added to every rerouted request on the
	// agent->gateway leg (e.g. a tenant id or routing hint). They never reach a
	// passthrough/direct request, and the x-tfy-* identity headers take
	// precedence over any colliding entry here.
	Headers map[string]string `yaml:"headers,omitempty"`
	// No overall request timeout for streaming bodies; idle timeouts only.
}

// AuthConfig holds the gateway credential: a token file written by the external
// sign-in component. When Disabled, the agent reroutes without requiring (or
// sending) a token — for gateways that authenticate the agent some other way
// (mTLS, network ACL) or local testing.
type AuthConfig struct {
	TokenFile string `yaml:"token_file"`
	Disabled  bool   `yaml:"disabled,omitempty"`
}

// AppProfile tags a decrypted request with an app id. It does not gate
// interception (intercept_hosts does that) nor define policy (top-level rules
// do that).
type AppProfile struct {
	ID    string   `yaml:"id"`
	Match AppMatch `yaml:"match"`
}

// AppMatch attributes a flow to an app, by process identity or (for browsers)
// by host.
type AppMatch struct {
	ProcessNames []string `yaml:"process_names,omitempty"`
	BundleID     string   `yaml:"bundle_id,omitempty"` // macOS
	ExePaths     []string `yaml:"exe_paths,omitempty"` // win/linux
	Browser      bool     `yaml:"browser,omitempty"`   // attribute by host, not PID
	Hosts        []string `yaml:"hosts,omitempty"`     // host patterns for browser tagging
}

// Rule classifies a request and decides its action.
type Rule struct {
	Name         string         `yaml:"name"`
	Hosts        []string       `yaml:"hosts,omitempty"`
	PathPrefixes []string       `yaml:"path_prefixes,omitempty"` // literal prefix match
	PathPatterns []string       `yaml:"path_patterns,omitempty"` // glob match (* = one segment, ** = any)
	Methods      []string       `yaml:"methods,omitempty"`
	MatchBody    map[string]any `yaml:"match_body,omitempty"` // shallow JSON key presence/value
	Category     string         `yaml:"category"`             // llm|mcp|other
	Action       string         `yaml:"action,omitempty"`     // reroute (default for llm/mcp) | passthrough
}

// Inject mode constants.
const (
	InjectModeSettings = "settings" // patch the client's settings.json env block
)

// InjectEntry toggles client-config injection for one app. The only mode is
// "settings": patch the app's managed/user settings env with proxy + CA vars so
// a proxy-ignoring client (e.g. Claude Code) routes through the Tier-1 proxy.
type InjectEntry struct {
	App      string          `yaml:"app"`
	Enabled  *bool           `yaml:"enabled,omitempty"` // nil = on (use built-in default)
	Mode     string          `yaml:"mode,omitempty"`    // settings (default)
	Settings *SettingsInject `yaml:"settings,omitempty"`
}

// On reports whether the entry is enabled (defaults to true when unset).
func (e InjectEntry) On() bool { return e.Enabled == nil || *e.Enabled }

// SettingsInject overrides the settings-file paths for settings-mode injection.
// Empty fields fall back to the built-in per-app defaults (clientcfg).
type SettingsInject struct {
	ManagedPath string `yaml:"managed_path,omitempty"`
	UserPath    string `yaml:"user_path,omitempty"`
}

// TracingConfig configures the OpenTelemetry tracing exporter. It is
// deliberately open-ended (endpoint + protocol + headers + batch tuning) so a
// fleet deployment can point it at any OTLP receiver without a code change. All
// header values support ${ENV} / ${ENV:-default} expansion, so credentials
// (e.g. a bearer token) can be supplied by the environment rather than written
// into the config file that ships to a laptop.
type TracingConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// Endpoint is the OTLP receiver. For protocol "http" it is the full base
	// URL of the collector (e.g. https://host/api/otel); the standard
	// "/v1/traces" signal path is appended unless the URL already ends in it.
	// For "grpc" it is a host:port.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Protocol selects the OTLP transport: "http" (OTLP/HTTP protobuf, default)
	// or "grpc".
	Protocol string `yaml:"protocol,omitempty"`
	// Insecure disables TLS (grpc) / uses http:// (http). Default false.
	Insecure bool `yaml:"insecure,omitempty"`
	// ServiceName is the resource service.name reported to the collector
	// (default "aitori").
	ServiceName string `yaml:"service_name,omitempty"`
	// Headers are static headers sent on every OTLP export (e.g. an auth token,
	// a tenant/project selector). Values support ${ENV} expansion.
	Headers map[string]string `yaml:"headers,omitempty"`
	// Timeout bounds a single export attempt (e.g. "10s"). Default 10s.
	Timeout string `yaml:"timeout,omitempty"`
	// CaptureContent records prompt/completion content on spans. OFF by default:
	// on a shared/employee device the message content is sensitive, so only the
	// metadata (model, token counts, latency) is captured unless this is set.
	CaptureContent bool `yaml:"capture_content,omitempty"`
	// MaxCaptureKB bounds how much of a non-streaming response body is held in
	// memory to extract token usage; beyond it the body is truncated (and then
	// not parsed). Streaming (SSE) responses are scanned incrementally and never
	// fully held. 0 uses the built-in default (256 KiB).
	MaxCaptureKB int `yaml:"max_capture_kb,omitempty"`
	// Categories selects which classified categories are turned into spans.
	// Default ["llm", "mcp"].
	Categories []string `yaml:"categories,omitempty"`
	// SampleRatio is the head-based sampling ratio in [0,1]. 0 (unset) means
	// always sample.
	SampleRatio float64 `yaml:"sample_ratio,omitempty"`
	// TruefoundryAttributes also stamps each span with TrueFoundry's native
	// tfy.* attribute schema (tfy.span_type, tfy.model.*, tfy.input/output,
	// tfy.model.metric.*) in addition to the OpenTelemetry gen_ai.* semantic
	// conventions, so spans render in the TrueFoundry tracing UI identically to
	// gateway-produced ones. Defaults to true; set false to emit only gen_ai.*.
	TruefoundryAttributes *bool `yaml:"truefoundry_attributes,omitempty"`
	// Attribution stamps who the traffic belongs to (subject/user, tenant,
	// teams) onto every span, mirroring the gateway's created_by_subject. On a
	// laptop the subject defaults to the invoking OS user; a fleet deployment
	// can override it with the employee's identity via ${ENV} expansion.
	Attribution AttributionConfig `yaml:"attribution,omitempty"`
	// Batch tunes the batch span processor.
	Batch BatchConfig `yaml:"batch,omitempty"`
}

// AttributionConfig identifies the human/principal the captured traffic belongs
// to, so spans carry the same created_by_subject shape the gateway records. All
// string values support ${ENV} / ${ENV:-default} expansion so a fleet MDM can
// inject the employee's identity from the environment rather than the config
// file. When Subject is empty it defaults to the invoking OS user.
type AttributionConfig struct {
	// Subject is the subject slug (typically an email or username). Defaults to
	// the invoking OS user when empty.
	Subject string `yaml:"subject,omitempty"`
	// SubjectID is a stable subject identifier. Defaults to Subject when empty.
	SubjectID string `yaml:"subject_id,omitempty"`
	// SubjectType categorizes the subject (e.g. "user", "serviceaccount").
	// Defaults to "user".
	SubjectType string `yaml:"subject_type,omitempty"`
	// Tenant is the TrueFoundry tenant/control-plane name.
	Tenant string `yaml:"tenant,omitempty"`
	// Teams are the subject's team slugs.
	Teams []string `yaml:"teams,omitempty"`
	// Metadata are arbitrary key/value pairs merged into tfy.request.metadata.
	Metadata map[string]string `yaml:"metadata,omitempty"`
}

// SubjectTypeOrDefault returns the subject type, defaulting to "user".
func (a AttributionConfig) SubjectTypeOrDefault() string {
	if a.SubjectType == "" {
		return "user"
	}
	return a.SubjectType
}

// BatchConfig tunes the OpenTelemetry batch span processor. Zero fields fall
// back to the SDK defaults (queue 2048, batch 512, timeout 5s, export 30s).
type BatchConfig struct {
	MaxQueueSize       int    `yaml:"max_queue_size,omitempty"`
	MaxExportBatchSize int    `yaml:"max_export_batch_size,omitempty"`
	BatchTimeout       string `yaml:"batch_timeout,omitempty"`
	ExportTimeout      string `yaml:"export_timeout,omitempty"`
}

// TracingProtocol constants.
const (
	TracingProtocolHTTP = "http"
	TracingProtocolGRPC = "grpc"
)

// ProtocolOrDefault returns the configured OTLP protocol, defaulting to http.
func (t TracingConfig) ProtocolOrDefault() string {
	if t.Protocol == "" {
		return TracingProtocolHTTP
	}
	return t.Protocol
}

// ServiceNameOrDefault returns the resource service name, defaulting to aitori.
func (t TracingConfig) ServiceNameOrDefault() string {
	if t.ServiceName == "" {
		return "aitori"
	}
	return t.ServiceName
}

// TruefoundryAttributesEnabled reports whether spans should also carry the
// TrueFoundry tfy.* attribute schema. Defaults to true when unset.
func (t TracingConfig) TruefoundryAttributesEnabled() bool {
	return t.TruefoundryAttributes == nil || *t.TruefoundryAttributes
}

// CategoriesOrDefault returns the categories to trace, defaulting to llm+mcp.
func (t TracingConfig) CategoriesOrDefault() []string {
	if len(t.Categories) == 0 {
		return []string{CategoryLLM, CategoryMCP}
	}
	return t.Categories
}

// CaptureLimitBytes returns the response-body capture bound in bytes, or 0 to
// let the accumulator use its built-in default.
func (t TracingConfig) CaptureLimitBytes() int {
	if t.MaxCaptureKB > 0 {
		return t.MaxCaptureKB * 1024
	}
	return 0
}

// TimeoutOrDefault returns the export attempt timeout, defaulting to 10s.
func (t TracingConfig) TimeoutOrDefault() time.Duration {
	if d, err := time.ParseDuration(t.Timeout); err == nil && d > 0 {
		return d
	}
	return 10 * time.Second
}

// SinkConfig configures an optional local observability sink (debug only).
type SinkConfig struct {
	Type   string `yaml:"type"`             // stdout|file
	Path   string `yaml:"path"`             // file sinks only
	Redact *bool  `yaml:"redact,omitempty"` // redact secrets; defaults to true when unset
}

// RedactOn reports whether secrets should be redacted in this sink's output.
// Redaction is on by default (safer for a security tool); set `redact: false`
// to disable it.
func (s SinkConfig) RedactOn() bool { return s.Redact == nil || *s.Redact }

// Category constants.
const (
	CategoryLLM   = "llm"
	CategoryMCP   = "mcp"
	CategoryOther = "other"
)

// Action constants.
const (
	ActionReroute     = "reroute"
	ActionPassthrough = "passthrough"
	ActionBlock       = "block"
)

// On-error mode constants.
const (
	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
)

// Default returns a Config populated with the documented defaults (plan §10).
// File contents are unmarshaled on top of these.
func Default() Config {
	return Config{
		Version: SchemaVersion,
		Proxy: ProxyConfig{
			Listen:      "127.0.0.1:8080",
			CADir:       "~/.aitori",
			MaxBodyKB:   1024, // 1 MiB request-body buffer for classification + fail-open retry
			ForceHTTP11: true,
		},
		Gateway: GatewayConfig{
			HeaderToken: "x-tfy-api-key",
			HeaderOrig:  "x-tfy-original-url",
			HeaderCtx:   "x-tfy-metadata",
			OnError:     FailOpen,
			DialTimeout: "5s",
			Auth: AuthConfig{
				TokenFile: "~/.aitori/token",
			},
		},
		UI: UIConfig{
			Listen: "127.0.0.1:9100",
		},
	}
}
