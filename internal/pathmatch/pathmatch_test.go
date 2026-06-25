package pathmatch

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		// exact
		{"/v1/messages", "/v1/messages", true},
		{"/v1/messages", "/v1/messages/", false},
		{"/v1/messages", "/v1/complete", false},
		// single star = one segment, does not span "/"
		{"/api/organizations/*/chat_conversations/*/completion",
			"/api/organizations/7f7d-ed95/chat_conversations/abc-123/completion", true},
		{"/api/organizations/*/settings", "/api/organizations/7f7d/settings", true},
		{"/api/organizations/*/settings", "/api/organizations/7f7d/sync/settings", false}, // * can't span /
		{"/v1/*", "/v1/messages", true},
		{"/v1/*", "/v1/messages/batches", false},
		// trailing single star
		{"/v1/messages*", "/v1/messages", true},
		{"/v1/messages*", "/v1/messages2", true},
		{"/v1/messages*", "/v1/messages/x", false},
		// double star spans everything
		{"/backend-api/codex/**", "/backend-api/codex/responses", true},
		{"/backend-api/codex/**", "/backend-api/codex/a/b/c", true},
		{"/backend-api/codex/**", "/backend-api/codex/", true},
		{"/aiserver.v1.ChatService/**", "/aiserver.v1.ChatService/StreamChat", true},
		{"**/completion", "/api/organizations/x/chat_conversations/y/completion", true},
		// interior "**" spans one or more segments (literal "** = any run incl /")
		{"/a/**/d", "/a/b/c/d", true},
		{"/a/**/*.go", "/a/b/c/x.go", true},   // the real fix: ** then * both backtrack
		{"/a/**/*.go", "/a/b/c/x.txt", false}, // wrong suffix
		{"/a/**/d", "/a/b/c/e", false},        // wrong tail
		// "**" between slashes needs >=1 segment under literal semantics
		{"/a/**/d", "/a/d", false},
		{"/foo/**/bar", "/foo/bar", false},
		// no match
		{"/backend-api/conversation", "/backend-api/me", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"/v1/chat/completions", "/v1/responses", "/v1/completions"}
	if !MatchAny(pats, "/v1/responses") {
		t.Error("expected /v1/responses to match")
	}
	if MatchAny(pats, "/v1/models") {
		t.Error("did not expect /v1/models to match")
	}
}
