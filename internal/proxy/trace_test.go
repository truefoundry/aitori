package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/genai"
	"github.com/truefoundry/aitori/internal/pipeline"
	"github.com/truefoundry/aitori/internal/router"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type captureTracer struct {
	mu  sync.Mutex
	obs []genai.Observation
}

func (c *captureTracer) Emit(o genai.Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.obs = append(c.obs, o)
}

func (c *captureTracer) last() (genai.Observation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.obs) == 0 {
		return genai.Observation{}, false
	}
	return c.obs[len(c.obs)-1], true
}

// tracingProxy builds a proxy with tracing enabled for llm and a fake upstream
// that returns resp for every request.
func tracingProxy(t *testing.T, resp *http.Response, tr *captureTracer) *Proxy {
	t.Helper()
	cfg := &config.Config{
		Version:        config.SchemaVersion,
		Proxy:          config.ProxyConfig{MaxBodyKB: 1024},
		InterceptHosts: config.Hosts("api.openai.com"),
		Tracing:        config.TracingConfig{Enabled: true, Categories: []string{config.CategoryLLM}},
	}
	return New(Options{
		Config:    cfg,
		Pipeline:  pipeline.New(cfg),
		Router:    router.New(router.Options{}), // no gateway: reroute declines, forwards upstream
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return resp, nil }),
		Tracer:    tr,
	})
}

func newResp(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestServeEmitsGenAISpan_JSON(t *testing.T) {
	respBody := `{"model":"gpt-4o-mini","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`
	tr := &captureTracer{}
	p := tracingProxy(t, newResp(200, "application/json", respBody), tr)

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest("POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	p.serve(w, r, true)

	o, ok := tr.last()
	if !ok {
		t.Fatal("no span emitted")
	}
	if o.Category != config.CategoryLLM {
		t.Errorf("category = %q", o.Category)
	}
	if o.Request.Model != "gpt-4o-mini" {
		t.Errorf("request model = %q", o.Request.Model)
	}
	if o.Request.Provider != genai.ProviderOpenAI {
		t.Errorf("provider = %q", o.Request.Provider)
	}
	if o.Response.InputTokens != 11 || o.Response.OutputTokens != 4 {
		t.Errorf("tokens in=%d out=%d", o.Response.InputTokens, o.Response.OutputTokens)
	}
	if o.Status != 200 {
		t.Errorf("status = %d", o.Status)
	}
	// The client must still receive the body verbatim.
	if got := w.Body.String(); got != respBody {
		t.Errorf("client body altered:\n got %q", got)
	}
}

func TestServeEmitsGenAISpan_SSEStreamsVerbatim(t *testing.T) {
	sse := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	tr := &captureTracer{}
	p := tracingProxy(t, newResp(200, "text/event-stream", sse), tr)

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest("POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()
	p.serve(w, r, true)

	o, ok := tr.last()
	if !ok {
		t.Fatal("no span emitted")
	}
	if o.Response.OutputTokens != 2 || o.Response.InputTokens != 7 {
		t.Errorf("tokens in=%d out=%d", o.Response.InputTokens, o.Response.OutputTokens)
	}
	if len(o.Response.FinishReasons) != 1 || o.Response.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", o.Response.FinishReasons)
	}
	// The streamed SSE must reach the client byte-for-byte.
	if got := w.Body.String(); got != sse {
		t.Errorf("SSE not streamed verbatim:\n got %q\nwant %q", got, sse)
	}
	// Content must NOT be captured (capture_content defaults off).
	if o.Response.Content != "" || len(o.Request.Messages) != 0 {
		t.Errorf("content captured despite capture_content=false: resp=%q req=%v", o.Response.Content, o.Request.Messages)
	}
}

// A non-traced category (other) must not emit a span or buffer differently.
func TestServeNoSpanForOtherCategory(t *testing.T) {
	tr := &captureTracer{}
	p := tracingProxy(t, newResp(200, "application/json", `{"ok":true}`), tr)

	r := httptest.NewRequest("GET", "https://api.openai.com/v1/models", http.NoBody)
	w := httptest.NewRecorder()
	p.serve(w, r, true)

	if _, ok := tr.last(); ok {
		t.Error("no span expected for non-llm/mcp request")
	}
}
