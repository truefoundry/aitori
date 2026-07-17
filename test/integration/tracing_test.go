// This file drives real requests through the aitori proxy with OpenTelemetry
// tracing enabled, against a real OTLP/HTTP receiver, and asserts that the
// exported spans decode correctly and carry the right GenAI attributes and
// token usage — for both a non-streaming JSON response and a streamed SSE one.
package integration

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/ca"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/pipeline"
	"github.com/truefoundry/aitori/internal/proxy"
	"github.com/truefoundry/aitori/internal/router"
	"github.com/truefoundry/aitori/internal/tracing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const llmHost = "api.openai.test" // contains "openai" so provider detection => openai

// --- a minimal OTLP/HTTP trace receiver ---

type otlpReceiver struct {
	url string

	mu        sync.Mutex
	spans     []*tracepb.Span
	resources []*commonpb.KeyValue // resource attrs from the last ResourceSpans
	headers   http.Header
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	rec := &otlpReceiver{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		body, err := readMaybeGzip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.headers = r.Header.Clone()
		for _, rs := range req.ResourceSpans {
			if rs.Resource != nil {
				rec.resources = rs.Resource.Attributes
			}
			for _, ss := range rs.ScopeSpans {
				rec.spans = append(rec.spans, ss.Spans...)
			}
		}
		rec.mu.Unlock()

		// A correct OTLP/HTTP success is a 200 with an (empty) protobuf response.
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	rec.url = "http://" + ln.Addr().String()
	return rec
}

func readMaybeGzip(r *http.Request) ([]byte, error) {
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	return io.ReadAll(reader)
}

// waitForSpans polls until at least n spans have arrived or the deadline passes.
func (rec *otlpReceiver) waitForSpans(t *testing.T, n int) []*tracepb.Span {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		got := len(rec.spans)
		rec.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]*tracepb.Span, len(rec.spans))
	copy(out, rec.spans)
	return out
}

func spanByName(spans []*tracepb.Span, name string) *tracepb.Span {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func attrs(s *tracepb.Span) map[string]*commonpb.AnyValue {
	m := make(map[string]*commonpb.AnyValue, len(s.Attributes))
	for _, kv := range s.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

func attrStr(m map[string]*commonpb.AnyValue, k string) string {
	if v, ok := m[k]; ok {
		return v.GetStringValue()
	}
	return ""
}

func attrInt(m map[string]*commonpb.AnyValue, k string) int64 {
	if v, ok := m[k]; ok {
		return v.GetIntValue()
	}
	return 0
}

// --- the tracing harness: proxy + fake LLM upstream + OTLP receiver ---

type traceHarness struct {
	client   *http.Client
	baseURL  string
	receiver *otlpReceiver
	tracer   *tracing.Provider
}

func newTraceHarness(t *testing.T, cfgMut func(*config.TracingConfig)) *traceHarness {
	t.Helper()
	rec := newOTLPReceiver(t)

	// --- fake LLM upstream ---
	upCert, upX509 := genCert(t, llmHost)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi there"}}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`)
	})
	mux.HandleFunc("/v1/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		lines := []string{
			`data: {"model":"gpt-4o","choices":[{"delta":{"content":"He"}}]}`,
			`data: {"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
			`data: {"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
			`data: [DONE]`,
		}
		for _, ln := range lines {
			fmt.Fprintf(w, "%s\n\n", ln)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(3 * time.Millisecond)
		}
	})
	upAddr, _ := serveTLS(t, upCert, mux)
	_, upPort, _ := net.SplitHostPort(upAddr)
	baseURL := "https://" + net.JoinHostPort(llmHost, upPort)

	nameMap := map[string]string{llmHost: "127.0.0.1"}

	// --- tracing provider pointed at the OTLP receiver ---
	tcfg := config.TracingConfig{
		Enabled:  true,
		Endpoint: rec.url,
		Protocol: config.TracingProtocolHTTP,
		Insecure: true,
		Headers:  map[string]string{"X-Test-Header": "abc123"},
		Batch:    config.BatchConfig{MaxExportBatchSize: 1, BatchTimeout: "50ms"},
	}
	if cfgMut != nil {
		cfgMut(&tcfg)
	}
	tp, err := tracing.New(context.Background(), tcfg, "test", "dev-host", nil)
	if err != nil {
		t.Fatalf("tracing.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})

	// --- aitori agent (no gateway: llm reroute declines and forwards upstream) ---
	cfg := config.Default()
	cfg.InterceptHosts = config.Hosts(llmHost)
	cfg.Tracing = tcfg
	cfg.Apps = []config.AppProfile{{ID: "test-llm-app", Match: config.AppMatch{Browser: true, Hosts: []string{llmHost}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	agentCA, err := ca.LoadOrCreate(t.TempDir(), ca.Options{Organization: "aitori-test"})
	if err != nil {
		t.Fatal(err)
	}
	egressPool := x509.NewCertPool()
	egressPool.AddCert(upX509)
	agentTransport := &http.Transport{
		Proxy:           nil,
		DialContext:     nameDialer(nameMap),
		TLSClientConfig: &tls.Config{RootCAs: egressPool},
	}

	p := proxy.New(proxy.Options{
		Config:      &cfg,
		CA:          agentCA,
		Router:      router.New(router.Options{}), // no gateway
		Pipeline:    pipeline.New(&cfg),
		Transport:   agentTransport,
		DialContext: nameDialer(nameMap),
		Tracer:      tp,
	})
	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(pln)
	t.Cleanup(func() { p.Close() })

	clientPool := x509.NewCertPool()
	clientPool.AddCert(agentCA.Cert())
	proxyURL, _ := url.Parse("http://" + pln.Addr().String())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			TLSClientConfig:   &tls.Config{RootCAs: clientPool},
			ForceAttemptHTTP2: false,
		},
		Timeout: 10 * time.Second,
	}

	return &traceHarness{client: client, baseURL: baseURL, receiver: rec, tracer: tp}
}

func (h *traceHarness) post(t *testing.T, path, body string) string {
	t.Helper()
	req, err := http.NewRequest("POST", h.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer provider-secret")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// --- tests ---

func TestTracingExportsGenAISpan_JSON(t *testing.T) {
	h := newTraceHarness(t, nil)

	h.post(t, "/v1/chat/completions", `{"model":"gpt-4o-mini","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)

	spans := h.receiver.waitForSpans(t, 1)
	s := spanByName(spans, "chat gpt-4o-mini")
	if s == nil {
		t.Fatalf("no 'chat gpt-4o-mini' span; got %d spans: %v", len(spans), spanNames(spans))
	}
	a := attrs(s)
	if got := attrStr(a, "gen_ai.provider.name"); got != "openai" {
		t.Errorf("provider = %q, want openai", got)
	}
	if got := attrStr(a, "gen_ai.operation.name"); got != "chat" {
		t.Errorf("operation = %q, want chat", got)
	}
	if got := attrStr(a, "gen_ai.request.model"); got != "gpt-4o-mini" {
		t.Errorf("request.model = %q", got)
	}
	if got := attrInt(a, "gen_ai.request.max_tokens"); got != 128 {
		t.Errorf("max_tokens = %d, want 128", got)
	}
	if got := attrInt(a, "gen_ai.usage.input_tokens"); got != 11 {
		t.Errorf("input_tokens = %d, want 11", got)
	}
	if got := attrInt(a, "gen_ai.usage.output_tokens"); got != 4 {
		t.Errorf("output_tokens = %d, want 4", got)
	}
	if got := attrStr(a, "aitori.app"); got != "test-llm-app" {
		t.Errorf("aitori.app = %q, want test-llm-app", got)
	}
	if got := attrStr(a, "gen_ai.response.model"); got != "gpt-4o-mini" {
		t.Errorf("response.model = %q", got)
	}
	// capture_content defaults off: no content attributes.
	if _, ok := a["gen_ai.input.messages"]; ok {
		t.Error("input messages captured despite capture_content=false")
	}
	// Custom export header must have reached the receiver.
	h.receiver.mu.Lock()
	hdr := h.receiver.headers.Get("X-Test-Header")
	res := h.receiver.resources
	h.receiver.mu.Unlock()
	if hdr != "abc123" {
		t.Errorf("export header X-Test-Header = %q, want abc123", hdr)
	}
	// service.name (traceloop's app_name) rides on the RESOURCE, not the span.
	if got := resourceAttr(res, "service.name"); got != "aitori" {
		t.Errorf("resource service.name = %q, want aitori (this is traceloop's app_name)", got)
	}
	if got := resourceAttr(res, "host.name"); got != "dev-host" {
		t.Errorf("resource host.name = %q, want dev-host", got)
	}
}

func resourceAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

func TestTracingExportsGenAISpan_SSE(t *testing.T) {
	h := newTraceHarness(t, nil)

	body := h.post(t, "/v1/sse", `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	// The streamed response must reach the client intact.
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, `"delta":{"content":"He"}`) {
		t.Fatalf("SSE body not streamed intact: %q", body)
	}

	spans := h.receiver.waitForSpans(t, 1)
	s := spanByName(spans, "chat gpt-4o")
	if s == nil {
		t.Fatalf("no 'chat gpt-4o' span; got %d: %v", len(spans), spanNames(spans))
	}
	a := attrs(s)
	if got := attrInt(a, "gen_ai.usage.input_tokens"); got != 7 {
		t.Errorf("input_tokens = %d, want 7 (extracted from stream)", got)
	}
	if got := attrInt(a, "gen_ai.usage.output_tokens"); got != 3 {
		t.Errorf("output_tokens = %d, want 3 (extracted from stream)", got)
	}
	if v, ok := a["gen_ai.response.finish_reasons"]; !ok || len(v.GetArrayValue().GetValues()) == 0 {
		t.Error("finish_reasons missing")
	} else if v.GetArrayValue().Values[0].GetStringValue() != "stop" {
		t.Errorf("finish_reason = %q, want stop", v.GetArrayValue().Values[0].GetStringValue())
	}
	if v, ok := a["gen_ai.request.stream"]; !ok || !v.GetBoolValue() {
		t.Error("gen_ai.request.stream should be true")
	}
}

func TestTracingCapturesContentWhenEnabled(t *testing.T) {
	h := newTraceHarness(t, func(c *config.TracingConfig) { c.CaptureContent = true })

	h.post(t, "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello world"}]}`)

	spans := h.receiver.waitForSpans(t, 1)
	s := spanByName(spans, "chat gpt-4o-mini")
	if s == nil {
		t.Fatalf("no span; got %d: %v", len(spans), spanNames(spans))
	}
	a := attrs(s)
	in := attrStr(a, "gen_ai.input.messages")
	if !strings.Contains(in, `"role":"user"`) || !strings.Contains(in, `"content":"hello world"`) {
		t.Errorf("input.messages missing/structured wrong: %q", in)
	}
	if !strings.Contains(in, `"parts"`) || !strings.Contains(in, `"type":"text"`) {
		t.Errorf("input.messages not in structured GenAI shape: %q", in)
	}
	out := attrStr(a, "gen_ai.output.messages")
	if !strings.Contains(out, `"content":"hi there"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("output.messages missing/structured wrong: %q", out)
	}
}

func spanNames(spans []*tracepb.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}
