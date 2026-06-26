package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/circuit"
	"github.com/truefoundry/aitori/internal/token"
)

func gwURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://gateway.truefoundry.example")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mkReq(t *testing.T, method, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRewriteHappyPath(t *testing.T) {
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("secret-tok"), AgentVersion: "1.2.3"})

	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages?beta=1")
	req.Header.Set("Authorization", "Bearer provider-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	if !r.Rewrite(req, "claude-desktop", "llm", 0) {
		t.Fatal("expected reroute=true")
	}

	if req.URL.Host != "gateway.truefoundry.example" || req.Host != "gateway.truefoundry.example" {
		t.Errorf("request not pointed at gateway: url.Host=%q host=%q", req.URL.Host, req.Host)
	}
	if got := req.Header.Get("x-tfy-original-url"); got != "https://api.anthropic.com/v1/messages?beta=1" {
		t.Errorf("x-tfy-original-url = %q", got)
	}
	if got := req.Header.Get("x-tfy-api-key"); got != "secret-tok" {
		t.Errorf("x-tfy-api-key = %q", got)
	}
	// Attribution + agent version ride inside the consolidated ctx header now,
	// not as separate x-tfy-* headers.
	for _, h := range []string{"x-tfy-app", "x-tfy-category", "x-tfy-pid", "x-tfy-agent-version"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("individual header %s should not be sent, got %q", h, v)
		}
	}
	var ctx struct {
		App          string `json:"app"`
		Category     string `json:"category"`
		AgentVersion string `json:"agent_version"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get(HeaderCtx)), &ctx); err != nil {
		t.Fatalf("ctx header not valid JSON: %v", err)
	}
	if ctx.App != "claude-desktop" || ctx.Category != "llm" || ctx.AgentVersion != "1.2.3" {
		t.Errorf("ctx = %+v, want app=claude-desktop category=llm agent_version=1.2.3", ctx)
	}
	// Provider credentials must be untouched.
	if req.Header.Get("Authorization") != "Bearer provider-key" {
		t.Error("provider Authorization header was modified")
	}
	if req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Error("provider anthropic-version header was modified")
	}
	// Path/query preserved.
	if req.URL.RequestURI() != "/v1/messages?beta=1" {
		t.Errorf("path/query altered: %q", req.URL.RequestURI())
	}
}

func TestRewriteGatewayPathVerbatim(t *testing.T) {
	for _, tc := range []struct{ gw, wantURI string }{
		// The gateway path is used verbatim as the endpoint; the request's own
		// path (/v1/messages) is NOT appended. Query is preserved.
		{"https://gateway.truefoundry.example/api/llm/ai-proxy", "/api/llm/ai-proxy?beta=1"},
		{"https://gateway.truefoundry.example/ai-proxy/", "/ai-proxy/?beta=1"}, // trailing slash kept as-is
		// A query on gateway.url is preserved, merged ahead of the request's query.
		{"https://gateway.truefoundry.example/edge?tenant=acme", "/edge?tenant=acme&beta=1"},
	} {
		u, err := url.Parse(tc.gw)
		if err != nil {
			t.Fatal(err)
		}
		r := New(Options{GatewayURL: u, Token: token.NewStatic("tok")})
		req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages?beta=1")

		if !r.Rewrite(req, "claude-code", "llm", 0) {
			t.Fatalf("[%s] expected reroute=true", tc.gw)
		}
		if got := req.URL.RequestURI(); got != tc.wantURI {
			t.Errorf("[%s] RequestURI = %q, want %q", tc.gw, got, tc.wantURI)
		}
		if req.URL.Host != "gateway.truefoundry.example" {
			t.Errorf("[%s] url.Host = %q", tc.gw, req.URL.Host)
		}
		// The real destination is unchanged — it travels in the header, not the
		// gateway-leg path.
		if got := req.Header.Get("x-tfy-original-url"); got != "https://api.anthropic.com/v1/messages?beta=1" {
			t.Errorf("[%s] x-tfy-original-url = %q", tc.gw, got)
		}
	}
}

func TestRewriteCustomHeaders(t *testing.T) {
	r := New(Options{
		GatewayURL: gwURL(t),
		Token:      token.NewStatic("real-tok"),
		Headers: map[string]string{
			"x-tfy-tenant":  "acme",
			"x-route-hint":  "us-east",
			"x-tfy-api-key": "should-not-win", // collides with the identity header
		},
	})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")

	if !r.Rewrite(req, "claude-code", "llm", 0) {
		t.Fatal("expected reroute=true")
	}
	if got := req.Header.Get("x-tfy-tenant"); got != "acme" {
		t.Errorf("x-tfy-tenant = %q, want acme", got)
	}
	if got := req.Header.Get("x-route-hint"); got != "us-east" {
		t.Errorf("x-route-hint = %q, want us-east", got)
	}
	// The identity header must win over a colliding custom header.
	if got := req.Header.Get("x-tfy-api-key"); got != "real-tok" {
		t.Errorf("x-tfy-api-key = %q, want real-tok (identity must take precedence)", got)
	}
}

func TestRewriteCtxHeaderPayload(t *testing.T) {
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok")})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")

	if !r.Rewrite(req, "claude-code", "llm", 4242) {
		t.Fatal("expected reroute=true")
	}
	// No separate x-tfy-pid header — pid is inside the ctx payload.
	if req.Header.Get("x-tfy-pid") != "" {
		t.Error("x-tfy-pid header should not be sent (pid is in the ctx header)")
	}
	ctx := req.Header.Get(HeaderCtx)
	if ctx == "" {
		t.Fatalf("context header %q not set", HeaderCtx)
	}
	var got struct {
		App      string `json:"app"`
		PID      string `json:"pid"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(ctx), &got); err != nil {
		t.Fatalf("ctx header not valid JSON: %v (%q)", err, ctx)
	}
	if got.App != "claude-code" || got.PID != "4242" || got.Category != "llm" {
		t.Fatalf("ctx = %+v, want {claude-code 4242 llm}", got)
	}
	// pid 0 -> omitted from the ctx JSON.
	req2 := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	r.Rewrite(req2, "claude-code", "llm", 0)
	var got2 struct {
		PID string `json:"pid"`
	}
	_ = json.Unmarshal([]byte(req2.Header.Get(HeaderCtx)), &got2)
	if got2.PID != "" {
		t.Errorf("pid should be omitted in ctx when 0, got %q", got2.PID)
	}
}

func TestRewriteNoTokenFailsOpen(t *testing.T) {
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("")})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if r.Rewrite(req, "app", "llm", 0) {
		t.Fatal("expected reroute=false with no token")
	}
	if req.Header.Get("x-tfy-api-key") != "" {
		t.Error("token must never be attached when not rerouting")
	}
	if req.URL.Host != "api.anthropic.com" {
		t.Error("request URL should be unchanged when not rerouting")
	}
}

func TestRewriteBreakerOpenFailsOpen(t *testing.T) {
	br := circuit.New(1, time.Hour)
	br.Failure() // trip
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok"), Breaker: br})
	req := mkReq(t, "POST", "https://api.openai.com/v1/chat/completions")
	if r.Rewrite(req, "app", "llm", 0) {
		t.Fatal("expected reroute=false when breaker open")
	}
	if req.Header.Get("x-tfy-api-key") != "" {
		t.Error("token attached despite breaker open")
	}
}

func TestRewriteLoopGuard(t *testing.T) {
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok")})
	req := mkReq(t, "POST", "https://gateway.truefoundry.example/v1/messages")
	if r.Rewrite(req, "app", "llm", 0) {
		t.Fatal("expected reroute=false for request already targeting gateway")
	}
}

func TestRewriteNoGateway(t *testing.T) {
	r := New(Options{Token: token.NewStatic("tok")})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if r.Rewrite(req, "app", "llm", 0) {
		t.Fatal("expected reroute=false with no gateway configured")
	}
}

func TestRewriteAuthDisabledReroutesWithoutToken(t *testing.T) {
	// With auth disabled, a missing token must NOT fail open — the request is
	// rerouted, just without the identity header.
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic(""), AuthDisabled: true})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r.Rewrite(req, "claude-code", "llm", 0) {
		t.Fatal("expected reroute=true when auth is disabled")
	}
	if req.URL.Host != "gateway.truefoundry.example" {
		t.Errorf("request not pointed at gateway: %q", req.URL.Host)
	}
	if got := req.Header.Get("x-tfy-api-key"); got != "" {
		t.Errorf("no token header expected when none available, got %q", got)
	}
	// A token, if present, is still attached even with auth disabled.
	r2 := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok"), AuthDisabled: true})
	req2 := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r2.Rewrite(req2, "claude-code", "llm", 0) {
		t.Fatal("expected reroute=true")
	}
	if got := req2.Header.Get("x-tfy-api-key"); got != "tok" {
		t.Errorf("x-tfy-api-key = %q, want tok", got)
	}
}

func TestRewriteDeviceMetadata(t *testing.T) {
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok"), DeviceHost: "laptop-01", DeviceOS: "darwin"})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r.Rewrite(req, "claude-code", "llm", 7) {
		t.Fatal("expected reroute=true")
	}
	var got struct {
		App      string `json:"app"`
		Category string `json:"category"`
		Host     string `json:"host"`
		OS       string `json:"os"`
		PID      string `json:"pid"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get(HeaderCtx)), &got); err != nil {
		t.Fatalf("context header not valid JSON: %v", err)
	}
	if got.Host != "laptop-01" || got.OS != "darwin" {
		t.Errorf("device metadata = host:%q os:%q, want laptop-01/darwin", got.Host, got.OS)
	}
	if got.App != "claude-code" || got.PID != "7" || got.Category != "llm" {
		t.Errorf("attribution = %+v", got)
	}
}

func TestRewriteCtxValuesClampedTo128(t *testing.T) {
	longApp := strings.Repeat("a", 200)
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok"), DeviceHost: strings.Repeat("h", 300)})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r.Rewrite(req, longApp, "llm", 1) {
		t.Fatal("expected reroute=true")
	}
	var got struct {
		App  string `json:"app"`
		Host string `json:"host"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get(HeaderCtx)), &got); err != nil {
		t.Fatalf("ctx header not valid JSON: %v", err)
	}
	if len(got.App) != 128 {
		t.Errorf("app len = %d, want 128", len(got.App))
	}
	if len(got.Host) != 128 {
		t.Errorf("host len = %d, want 128", len(got.Host))
	}
}

func TestRewriteContextHeaderName(t *testing.T) {
	// Default header name.
	r := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok")})
	req := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r.Rewrite(req, "claude-code", "llm", 1) {
		t.Fatal("expected reroute=true")
	}
	if req.Header.Get("x-tfy-metadata") == "" {
		t.Errorf("default context header x-tfy-metadata not set: %v", req.Header)
	}

	// Custom header name overrides the default; the default name is not emitted.
	r2 := New(Options{GatewayURL: gwURL(t), Token: token.NewStatic("tok"), HeaderCtx: "x-acme-ctx"})
	req2 := mkReq(t, "POST", "https://api.anthropic.com/v1/messages")
	if !r2.Rewrite(req2, "claude-code", "llm", 1) {
		t.Fatal("expected reroute=true")
	}
	if req2.Header.Get("x-acme-ctx") == "" {
		t.Errorf("custom context header x-acme-ctx not set: %v", req2.Header)
	}
	if req2.Header.Get("x-tfy-metadata") != "" {
		t.Error("default context header should not be set when overridden")
	}
}
