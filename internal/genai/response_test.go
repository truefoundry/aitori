package genai

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestResponseAccumulator_GzipJSON verifies a gzip-encoded JSON body is decoded
// before parsing, so token usage and the raw body are captured as text.
func TestResponseAccumulator_GzipJSON(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`
	gz := gzipBytes(t, body)

	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "gzip", true, 0)
	// Feed the compressed bytes in awkward chunks, as the proxy would.
	for i := 0; i < len(gz); i += 5 {
		end := i + 5
		if end > len(gz) {
			end = len(gz)
		}
		a.Write(gz[i:end])
	}
	r := a.Result()
	if r.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (body should be gunzipped)", r.Model)
	}
	if r.InputTokens != 8 || r.OutputTokens != 2 {
		t.Errorf("tokens in=%d out=%d, want 8/2", r.InputTokens, r.OutputTokens)
	}
	if r.Content != "hi" {
		t.Errorf("content = %q, want hi", r.Content)
	}
	// RawBody (tfy.output) must be the DECODED text, not the gzip bytes.
	if r.RawBody != body {
		t.Errorf("RawBody = %q, want decoded body", r.RawBody)
	}
}

// TestResponseAccumulator_GzipSSE verifies a gzip-encoded SSE stream is decoded
// incrementally so streaming usage is still extracted.
func TestResponseAccumulator_GzipSSE(t *testing.T) {
	stream := "" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	gz := gzipBytes(t, stream)

	a := NewResponseAccumulator(ProviderOpenAI, "text/event-stream", "gzip", true, 0)
	a.Write(gz)
	r := a.Result()
	if r.InputTokens != 4 || r.OutputTokens != 1 {
		t.Errorf("tokens in=%d out=%d, want 4/1", r.InputTokens, r.OutputTokens)
	}
	if r.Content != "Hi" {
		t.Errorf("content = %q, want Hi", r.Content)
	}
}

// TestResponseAccumulator_UndecodableEncodingSkipped verifies that an encoding
// the stdlib cannot decode (e.g. br) is skipped entirely rather than captured as
// binary — nothing is parsed and no (garbage) raw body is retained.
func TestResponseAccumulator_UndecodableEncodingSkipped(t *testing.T) {
	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "br", true, 0)
	a.Write([]byte{0x1b, 0x2c, 0x00, 0x00, 0xff, 0xfe}) // arbitrary non-UTF-8 bytes
	r := a.Result()
	if r.RawBody != "" {
		t.Errorf("RawBody should be empty for undecodable encoding, got %q", r.RawBody)
	}
	if r.Model != "" || r.InputTokens != 0 {
		t.Errorf("nothing should parse from an undecodable body")
	}
}

func TestResponseAccumulator_OpenAIJSON(t *testing.T) {
	body := `{
		"id": "chatcmpl-123",
		"model": "gpt-4o-mini",
		"choices": [{"finish_reason": "stop", "message": {"role":"assistant","content":"hello there"}}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17,
			"completion_tokens_details": {"reasoning_tokens": 2}}
	}`
	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "", true, 0)
	a.Write([]byte(body))
	r := a.Result()
	if r.Model != "gpt-4o-mini" || r.ResponseID != "chatcmpl-123" {
		t.Errorf("model/id = %q/%q", r.Model, r.ResponseID)
	}
	if r.InputTokens != 12 || r.OutputTokens != 5 {
		t.Errorf("tokens in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if r.ReasoningOutputTokens != 2 {
		t.Errorf("reasoning tokens = %d", r.ReasoningOutputTokens)
	}
	if len(r.FinishReasons) != 1 || r.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", r.FinishReasons)
	}
	if r.Content != "hello there" {
		t.Errorf("content = %q", r.Content)
	}
	// RawBody carries the verbatim response for tfy.output when content capture is on.
	if r.RawBody != body {
		t.Errorf("RawBody = %q, want the full raw body", r.RawBody)
	}
}

func TestResponseAccumulator_RawBodyGatedAndBounded(t *testing.T) {
	body := `{"model":"gpt-4o","usage":{"prompt_tokens":1,"completion_tokens":1}}`

	// Content capture OFF: no raw body retained.
	off := NewResponseAccumulator(ProviderOpenAI, "application/json", "", false, 0)
	off.Write([]byte(body))
	if r := off.Result(); r.RawBody != "" {
		t.Errorf("RawBody should be empty when capture off, got %q", r.RawBody)
	}

	// Content capture ON but tiny limit: raw body is truncated to a prefix.
	small := NewResponseAccumulator(ProviderOpenAI, "application/json", "", true, 10)
	small.Write([]byte(body))
	r := small.Result()
	if len(r.RawBody) != 10 {
		t.Errorf("RawBody len = %d, want 10 (bounded prefix)", len(r.RawBody))
	}
	if r.RawBody != body[:10] {
		t.Errorf("RawBody = %q, want prefix %q", r.RawBody, body[:10])
	}
}

func TestResponseAccumulator_RawBodySSE(t *testing.T) {
	stream := "data: {\"model\":\"gpt-4o\"}\n\ndata: [DONE]\n\n"
	a := NewResponseAccumulator(ProviderOpenAI, "text/event-stream", "", true, 0)
	a.Write([]byte(stream))
	if r := a.Result(); r.RawBody != stream {
		t.Errorf("SSE RawBody = %q, want the full stream text", r.RawBody)
	}
}

func TestResponseAccumulator_OpenAISSE(t *testing.T) {
	stream := "" +
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	a := NewResponseAccumulator(ProviderOpenAI, "text/event-stream; charset=utf-8", "", true, 0)
	// Feed in awkward chunk boundaries to exercise line reassembly.
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		a.Write([]byte(stream[i:end]))
	}
	r := a.Result()
	if r.Model != "gpt-4o" {
		t.Errorf("model = %q", r.Model)
	}
	if r.InputTokens != 9 || r.OutputTokens != 3 {
		t.Errorf("tokens in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if len(r.FinishReasons) != 1 || r.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", r.FinishReasons)
	}
	if r.Content != "Hello" {
		t.Errorf("content = %q", r.Content)
	}
}

func TestResponseAccumulator_AnthropicSSE(t *testing.T) {
	stream := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":25,\"cache_read_input_tokens\":10}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	a := NewResponseAccumulator(ProviderAnthropic, "text/event-stream", "", true, 0)
	a.Write([]byte(stream))
	r := a.Result()
	if r.Model != "claude-sonnet-4-5" || r.ResponseID != "msg_1" {
		t.Errorf("model/id = %q/%q", r.Model, r.ResponseID)
	}
	if r.InputTokens != 25 || r.OutputTokens != 7 {
		t.Errorf("tokens in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if r.CacheReadInputTokens != 10 {
		t.Errorf("cache read = %d", r.CacheReadInputTokens)
	}
	if len(r.FinishReasons) != 1 || r.FinishReasons[0] != "end_turn" {
		t.Errorf("finish reasons = %v", r.FinishReasons)
	}
	if r.Content != "Hi" {
		t.Errorf("content = %q", r.Content)
	}
}

func TestResponseAccumulator_JSONTruncatedBeyondLimit(t *testing.T) {
	// A JSON body larger than the limit is truncated and not parsed, so no
	// usage is extracted — but the accumulator never holds the whole body.
	big := make([]byte, 0, 4096)
	big = append(big, []byte(`{"model":"gpt-4o","usage":{"prompt_tokens":9,"completion_tokens":9},"pad":"`)...)
	for len(big) < 4096 {
		big = append(big, 'x')
	}
	big = append(big, []byte(`"}`)...)

	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "", false, 512)
	// Feed in chunks to mimic streaming.
	for i := 0; i < len(big); i += 128 {
		end := i + 128
		if end > len(big) {
			end = len(big)
		}
		a.Write(big[i:end])
	}
	r := a.Result()
	if r.InputTokens != 0 || r.OutputTokens != 0 {
		t.Errorf("expected no usage after truncation, got in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
	if a.json.Len() != 0 {
		t.Errorf("truncated buffer should be released, held %d bytes", a.json.Len())
	}
}

func TestResponseAccumulator_JSONWithinLimit(t *testing.T) {
	body := `{"model":"gpt-4o","usage":{"prompt_tokens":9,"completion_tokens":4}}`
	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "", false, 512)
	a.Write([]byte(body))
	r := a.Result()
	if r.InputTokens != 9 || r.OutputTokens != 4 {
		t.Errorf("usage under limit should parse: in=%d out=%d", r.InputTokens, r.OutputTokens)
	}
}

func TestResponseAccumulator_NoContentCapture(t *testing.T) {
	body := `{"model":"gpt-4o","choices":[{"message":{"content":"secret answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	a := NewResponseAccumulator(ProviderOpenAI, "application/json", "", false, 0)
	a.Write([]byte(body))
	r := a.Result()
	if r.Content != "" {
		t.Errorf("content must be empty when capture disabled: %q", r.Content)
	}
	if r.InputTokens != 1 {
		t.Errorf("usage should still parse: %d", r.InputTokens)
	}
}
