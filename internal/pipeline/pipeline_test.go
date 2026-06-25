package pipeline

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/truefoundry/aitori/internal/appresolve"
	"github.com/truefoundry/aitori/internal/config"
)

func mkReq(method, raw string, hdr http.Header) *http.Request {
	u, _ := url.Parse(raw)
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Request{Method: method, URL: u, Host: u.Host, Header: hdr}
}

func TestBodyMatters(t *testing.T) {
	// No match_body rules: a host/path/method decision needs no body.
	if New(webCfg()).BodyMatters() {
		t.Error("webCfg has no match_body rule; BodyMatters should be false")
	}
	// A match_body rule (e.g. MCP tools/call) forces body inspection.
	cfg := &config.Config{
		InterceptHosts: config.Hosts("claude.ai"),
		Rules: []config.Rule{
			{Name: "mcp", MatchBody: map[string]any{"jsonrpc": "2.0", "method": "tools/call"}, Category: config.CategoryMCP, Action: config.ActionReroute},
		},
	}
	if !New(cfg).BodyMatters() {
		t.Error("config has a match_body rule; BodyMatters should be true")
	}
}

// webCfg tags claude.ai as claude-web (by host) and reroutes its llm traffic via
// a top-level rule — the canonical host-centric shape.
func webCfg() *config.Config {
	return &config.Config{
		InterceptHosts: config.Hosts("claude.ai"),
		Apps:           []config.AppProfile{{ID: "claude-web", Match: config.AppMatch{Browser: true, Hosts: []string{"claude.ai"}}}},
		Rules:          []config.Rule{{Name: "completion", Hosts: []string{"claude.ai"}, Category: config.CategoryLLM}},
	}
}

// A host scoped to specific path globs + methods reroutes only matching
// requests; everything else on that host passes through automatically (no
// explicit passthrough rule).
func TestDecidePerHostPathScoping(t *testing.T) {
	cfg := &config.Config{
		InterceptHosts: config.InterceptHosts{
			{
				Host:     "claude.ai",
				Paths:    []string{"/api/organizations/*/chat_conversations/*/completion"},
				Methods:  []string{"POST"},
				Category: config.CategoryLLM,
				Action:   config.ActionReroute,
			},
		},
		Apps: []config.AppProfile{{ID: "claude-web", Match: config.AppMatch{Browser: true, Hosts: []string{"claude.ai"}}}},
	}
	p := New(cfg)
	llm := []byte(`{"prompt":"hi"}`)

	// Matching path + method -> rerouted, tagged.
	d := p.Decide(mkReq("POST", "https://claude.ai/api/organizations/7f7d/chat_conversations/abc/completion", nil),
		appresolve.Flow{Host: "claude.ai"}, llm)
	if !d.Reroute() || d.AppID != "claude-web" {
		t.Fatalf("completion: got %+v, want reroute tagged claude-web", d)
	}

	// Same path, wrong method -> auto passthrough.
	d = p.Decide(mkReq("GET", "https://claude.ai/api/organizations/7f7d/chat_conversations/abc/completion", nil),
		appresolve.Flow{Host: "claude.ai"}, nil)
	if d.Action != config.ActionPassthrough {
		t.Fatalf("GET completion: got %+v, want passthrough", d)
	}

	// Non-matching path with an llm-looking body -> still auto passthrough (the
	// scoping suppresses the heuristic; no separate passthrough rule needed).
	d = p.Decide(mkReq("GET", "https://claude.ai/api/organizations/7f7d/cowork_settings", nil),
		appresolve.Flow{Host: "claude.ai"}, llm)
	if d.Action != config.ActionPassthrough {
		t.Fatalf("settings: got %+v, want passthrough", d)
	}
}

// A scoped host with no explicit action/category still governs (reroutes) the
// matching paths — "trace these endpoints" — while everything else passes through.
func TestDecideScopedDefaultsToReroute(t *testing.T) {
	cfg := &config.Config{
		InterceptHosts: config.InterceptHosts{
			{Host: "api.foo.com", Paths: []string{"/v1/chat"}, Methods: []string{"POST"}},
		},
	}
	p := New(cfg)

	// Matching scoped path (no action/category set) -> rerouted, not passthrough.
	d := p.Decide(mkReq("POST", "https://api.foo.com/v1/chat", nil), appresolve.Flow{Host: "api.foo.com"}, nil)
	if !d.Reroute() {
		t.Fatalf("scoped path w/o action: got %+v, want reroute", d)
	}
	// Off-target path -> passthrough.
	d = p.Decide(mkReq("GET", "https://api.foo.com/v1/models", nil), appresolve.Flow{Host: "api.foo.com"}, nil)
	if d.Action != config.ActionPassthrough {
		t.Fatalf("off-target: got %+v, want passthrough", d)
	}
}

// Exercises the host-centric model: per-host action, top-level rule precedence,
// the block action, and tagging by process exe and by browser host.
func TestDecideHostCentricModel(t *testing.T) {
	cfg := &config.Config{
		InterceptHosts: config.InterceptHosts{
			{Host: "api.anthropic.com"},
			{Host: "*.cursor.sh", Action: config.ActionPassthrough},
		},
		Rules: []config.Rule{
			{Name: "block-auth", Hosts: []string{"*.cursor.sh"}, PathPrefixes: []string{"/auth"}, Action: config.ActionBlock},
		},
		Apps: []config.AppProfile{
			{ID: "claude-code", Match: config.AppMatch{ExePaths: []string{"/x/versions"}}},
			{ID: "claude-web", Match: config.AppMatch{Browser: true, Hosts: []string{"claude.ai"}}},
		},
	}
	p := New(cfg)
	llmBody := []byte(`{"model":"x","messages":[]}`)

	// Plain host + llm body -> heuristic reroute; tagged claude-code by exe path.
	d := p.Decide(mkReq("POST", "https://api.anthropic.com/v1/messages", nil),
		appresolve.Flow{HasProcess: true, ExePath: "/x/versions/2.1.0"}, llmBody)
	if !d.Reroute() || d.AppID != "claude-code" {
		t.Fatalf("anthropic llm: got %+v, want reroute tagged claude-code", d)
	}

	// Per-host action: cursor host passes through despite an llm-looking body.
	d = p.Decide(mkReq("POST", "https://api.cursor.sh/v1/x", nil),
		appresolve.Flow{Host: "api.cursor.sh"}, llmBody)
	if d.Action != config.ActionPassthrough {
		t.Fatalf("cursor host: got %+v, want passthrough (per-host action)", d)
	}

	// Top-level rule wins over the per-host action: /auth on cursor is blocked.
	d = p.Decide(mkReq("POST", "https://api.cursor.sh/auth/profile", nil),
		appresolve.Flow{Host: "api.cursor.sh"}, nil)
	if d.Action != config.ActionBlock {
		t.Fatalf("cursor /auth: got %+v, want block (top-level rule precedence)", d)
	}

	// Browser tagging by match.hosts.
	d = p.Decide(mkReq("POST", "https://claude.ai/api/append_message", nil),
		appresolve.Flow{Host: "claude.ai"}, llmBody)
	if d.AppID != "claude-web" {
		t.Fatalf("claude.ai: got %+v, want tagged claude-web", d)
	}
}

func TestDecideReroute(t *testing.T) {
	p := New(webCfg())
	d := p.Decide(mkReq("POST", "https://claude.ai/api/append_message", nil), appresolve.Flow{Host: "claude.ai"}, nil)
	if d.AppID != "claude-web" || !d.Reroute() || d.Category != "llm" {
		t.Fatalf("got %+v, want claude-web llm reroute", d)
	}
}

func TestDecideWebSocketForcedPassthrough(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("Upgrade", "websocket")
	hdr.Set("Connection", "Upgrade")
	p := New(webCfg())
	d := p.Decide(mkReq("GET", "https://claude.ai/api/append_message", hdr), appresolve.Flow{Host: "claude.ai"}, nil)
	if d.Reroute() {
		t.Fatalf("websocket upgrade must not be rerouted: %+v", d)
	}
	if d.Rule != "websocket-passthrough" {
		t.Errorf("rule = %q, want websocket-passthrough", d.Rule)
	}
}

func TestDecideUnknownHostPassthrough(t *testing.T) {
	p := New(webCfg())
	d := p.Decide(mkReq("GET", "https://example.com/telemetry", nil), appresolve.Flow{Host: "example.com"}, nil)
	if d.Reroute() {
		t.Fatalf("unknown host should pass through: %+v", d)
	}
}
