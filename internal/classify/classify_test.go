package classify

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/truefoundry/aitori/internal/config"
)

func mkReq(method, rawurl string) *http.Request {
	u, _ := url.Parse(rawurl)
	return &http.Request{Method: method, URL: u, Host: u.Host, Header: http.Header{}}
}

func TestRuleMatchHostPathMethod(t *testing.T) {
	rules := []config.Rule{{
		Name:         "messages",
		Hosts:        []string{"api.anthropic.com"},
		PathPrefixes: []string{"/v1/messages"},
		Methods:      []string{"POST"},
		Category:     config.CategoryLLM,
	}}
	d := Classify(rules, mkReq("POST", "https://api.anthropic.com/v1/messages"), nil)
	if !d.Matched || d.Category != config.CategoryLLM || d.Action != config.ActionReroute {
		t.Fatalf("got %+v, want matched llm reroute", d)
	}

	// Wrong method should not match the rule; falls to heuristic -> other/passthrough.
	d = Classify(rules, mkReq("GET", "https://api.anthropic.com/v1/messages"), nil)
	if d.Matched {
		t.Fatalf("did not expect rule match for GET, got %+v", d)
	}
	if d.Action != config.ActionPassthrough {
		t.Fatalf("expected passthrough for non-matching request, got %+v", d)
	}
}

func TestRuleMatchBodyPresenceAndValue(t *testing.T) {
	rules := []config.Rule{{
		Name:      "remote-mcp",
		MatchBody: map[string]any{"jsonrpc": "2.0"},
		Category:  config.CategoryMCP,
	}}
	body := []byte(`{"jsonrpc":"2.0","method":"tools/list","id":1}`)
	d := Classify(rules, mkReq("POST", "https://example.com/mcp"), body)
	if !d.Matched || d.Category != config.CategoryMCP {
		t.Fatalf("got %+v, want mcp", d)
	}

	// Different value must not match.
	bad := []byte(`{"jsonrpc":"1.0"}`)
	d = Classify(rules, mkReq("POST", "https://example.com/mcp"), bad)
	if d.Matched {
		t.Fatalf("did not expect match for jsonrpc 1.0, got %+v", d)
	}
}

func TestPresenceOnlyMatcher(t *testing.T) {
	rules := []config.Rule{{
		Name:      "any-llm",
		MatchBody: map[string]any{"model": ""},
		Category:  config.CategoryLLM,
	}}
	d := Classify(rules, mkReq("POST", "https://x/y"), []byte(`{"model":"gpt-4o","input":"hi"}`))
	if !d.Matched {
		t.Fatalf("expected presence match on model, got %+v", d)
	}
}

func TestHeuristicLLM(t *testing.T) {
	d := Classify(nil, mkReq("POST", "https://x/y"), []byte(`{"model":"claude-3","messages":[]}`))
	if d.Category != config.CategoryLLM || d.Matched {
		t.Fatalf("got %+v, want heuristic llm", d)
	}
}

func TestHeuristicMCP(t *testing.T) {
	d := Classify(nil, mkReq("POST", "https://x/y"), []byte(`{"jsonrpc":"2.0","method":"initialize"}`))
	if d.Category != config.CategoryMCP {
		t.Fatalf("got %+v, want heuristic mcp", d)
	}
}

func TestHeuristicOther(t *testing.T) {
	d := Classify(nil, mkReq("GET", "https://x/telemetry"), []byte(`{"event":"ping"}`))
	if d.Category != config.CategoryOther || d.Action != config.ActionPassthrough {
		t.Fatalf("got %+v, want other/passthrough", d)
	}
}

func TestCatchAllRule(t *testing.T) {
	rules := []config.Rule{{Name: "all", Category: config.CategoryLLM}}
	d := Classify(rules, mkReq("GET", "https://anything/here"), nil)
	if !d.Matched || d.Category != config.CategoryLLM {
		t.Fatalf("got %+v, want catch-all llm", d)
	}
}
