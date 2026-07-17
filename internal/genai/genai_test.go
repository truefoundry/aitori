package genai

import "testing"

func TestProviderFor(t *testing.T) {
	cases := map[string]string{
		"api.anthropic.com":                 ProviderAnthropic,
		"api.openai.com":                    ProviderOpenAI,
		"my-resource.openai.azure.com":      ProviderAzure,
		"generativelanguage.googleapis.com": ProviderGoogle,
		"gateway.internal.example.com":      "gateway.internal.example.com",
	}
	for host, want := range cases {
		if got := ProviderFor(host); got != want {
			t.Errorf("ProviderFor(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestOperationFor(t *testing.T) {
	cases := map[string]string{
		"/v1/chat/completions": OpChat,
		"/v1/messages":         OpChat,
		"/v1/responses":        OpChat,
		"/v1/embeddings":       OpEmbeddings,
		"/v1/complete":         OpTextGen,
	}
	for path, want := range cases {
		if got := operationFor(path); got != want {
			t.Errorf("operationFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestParseRequest_OpenAIChat(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o-mini",
		"max_tokens": 256,
		"temperature": 0.7,
		"top_p": 0.9,
		"stream": true,
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": "hi"}
		]
	}`)
	info := ParseRequest("api.openai.com", "/v1/chat/completions", body, true)
	if info.Provider != ProviderOpenAI {
		t.Errorf("provider = %q", info.Provider)
	}
	if info.Operation != OpChat {
		t.Errorf("operation = %q", info.Operation)
	}
	if info.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", info.Model)
	}
	if info.MaxTokens != 256 {
		t.Errorf("max_tokens = %d", info.MaxTokens)
	}
	if info.Temperature == nil || *info.Temperature != 0.7 {
		t.Errorf("temperature = %v", info.Temperature)
	}
	if !info.Stream {
		t.Error("stream should be true")
	}
	if len(info.Messages) != 2 || info.Messages[1].Content != "hi" {
		t.Errorf("messages = %+v", info.Messages)
	}
}

func TestParseRequest_ContentGate(t *testing.T) {
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"secret"}]}`)
	info := ParseRequest("api.anthropic.com", "/v1/messages", body, false)
	if len(info.Messages) != 0 {
		t.Errorf("content must not be captured when disabled: %+v", info.Messages)
	}
	if info.Model != "claude-3" {
		t.Errorf("metadata should still parse: model=%q", info.Model)
	}
}

func TestParseRequest_MCP(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`)
	info := ParseRequest("mcp.example.com", "/mcp", body, false)
	if info.RPCMethod != "tools/call" {
		t.Errorf("rpc method = %q", info.RPCMethod)
	}
}

func TestParseRequest_AnthropicSystemBlocks(t *testing.T) {
	body := []byte(`{"model":"claude-3","system":[{"type":"text","text":"you are helpful"}],"messages":[]}`)
	info := ParseRequest("api.anthropic.com", "/v1/messages", body, true)
	if info.System != "you are helpful" {
		t.Errorf("system = %q", info.System)
	}
}
