package hostmatch

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"API.Anthropic.com":  "api.anthropic.com",
		"api.openai.com:443": "api.openai.com",
		"127.0.0.1:8080":     "127.0.0.1",
		"[::1]:443":          "::1",
		"[2001:db8::1]":      "2001:db8::1",
		"  claude.ai  ":      "claude.ai",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"api.anthropic.com", "api.anthropic.com", true},
		{"api.anthropic.com", "API.Anthropic.com:443", true},
		{"api.anthropic.com", "anthropic.com", false},
		{"*.anthropic.com", "api.anthropic.com", true},
		{"*.anthropic.com", "a.b.anthropic.com", true},
		{"*.anthropic.com", "anthropic.com", false},
		{"*.anthropic.com", "notanthropic.com", false},
		{".anthropic.com", "anthropic.com", true},
		{".anthropic.com", "api.anthropic.com", true},
		{".anthropic.com", "evil.com", false},
		{"claude.ai", "claude.ai", true},
		{"", "claude.ai", false},
		{"claude.ai", "", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.host); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	patterns := []string{"api.anthropic.com", "*.anthropic.com", "claude.ai"}
	if !MatchAny(patterns, "claude.ai") {
		t.Error("expected claude.ai to match")
	}
	if !MatchAny(patterns, "sub.anthropic.com") {
		t.Error("expected sub.anthropic.com to match")
	}
	if MatchAny(patterns, "openai.com") {
		t.Error("did not expect openai.com to match")
	}
}
