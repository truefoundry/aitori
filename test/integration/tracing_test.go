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

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
	mux.HandleFunc("/v1/gzip", func(w http.ResponseWriter, r *http.Request) {
		// The proxy must have normalized Accept-Encoding to gzip for traced
		// requests (browsers advertise br/zstd, which we cannot decode).
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "expected gzip Accept-Encoding from proxy", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		io.WriteString(zw, `{"id":"chatcmpl-gz","model":"gpt-4o-mini","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"gzipped hi"}}],"usage":{"prompt_tokens":21,"completion_tokens":9}}`)
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
	tp, err := tracing.New(context.Background(), tcfg, "test", "dev-host", "alice", nil)
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
	// tfy schema is on by default, so the span name follows the gateway convention.
	s := spanByName(spans, "ChatCompletion: gpt-4o-mini")
	if s == nil {
		t.Fatalf("no 'ChatCompletion: gpt-4o-mini' span; got %d spans: %v", len(spans), spanNames(spans))
	}
	a := attrs(s)
	// --- TrueFoundry tfy.* schema (union with gen_ai.*) ---
	if got := attrStr(a, "tfy.span_type"); got != "ChatCompletion" {
		t.Errorf("tfy.span_type = %q, want ChatCompletion", got)
	}
	if got := attrStr(a, "tfy.model.name"); got != "gpt-4o-mini" {
		t.Errorf("tfy.model.name = %q, want gpt-4o-mini", got)
	}
	if got := attrStr(a, "tfy.model.request_type"); got != "ChatCompletion" {
		t.Errorf("tfy.model.request_type = %q", got)
	}
	if got := attrInt(a, "tfy.model.metric.input_tokens"); got != 11 {
		t.Errorf("tfy input_tokens = %d, want 11", got)
	}
	if got := attrInt(a, "tfy.model.metric.output_tokens"); got != 4 {
		t.Errorf("tfy output_tokens = %d, want 4", got)
	}
	if got := attrStr(a, "tfy.model.request_url"); !strings.Contains(got, "/v1/chat/completions") {
		t.Errorf("tfy.model.request_url = %q, want to contain /v1/chat/completions", got)
	}
	if _, ok := a["tfy.model.metric.latency_in_ms"]; !ok {
		t.Error("tfy.model.metric.latency_in_ms missing")
	}
	// --- gen_ai.* conventions ---
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
	// --- user attribution: subject defaults to the invoking OS user ("alice") ---
	if got := attrStr(a, "user.id"); got != "alice" {
		t.Errorf("user.id = %q, want alice", got)
	}
	if got := attrStr(a, "tfy.request.created_by_subject"); !strings.Contains(got, `"subjectSlug":"alice"`) || !strings.Contains(got, `"subjectType":"user"`) {
		t.Errorf("tfy.request.created_by_subject = %q, want subjectSlug=alice, subjectType=user", got)
	}
	// capture_content defaults off: no content attributes (gen_ai or tfy).
	if _, ok := a["gen_ai.input.messages"]; ok {
		t.Error("input messages captured despite capture_content=false")
	}
	if _, ok := a["tfy.input"]; ok {
		t.Error("tfy.input captured despite capture_content=false")
	}
	if _, ok := a["tfy.output"]; ok {
		t.Error("tfy.output captured despite capture_content=false")
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
	s := spanByName(spans, "ChatCompletion: gpt-4o")
	if s == nil {
		t.Fatalf("no 'ChatCompletion: gpt-4o' span; got %d: %v", len(spans), spanNames(spans))
	}
	a := attrs(s)
	// tfy streaming metrics extracted from the stream too.
	if got := attrInt(a, "tfy.model.metric.output_tokens"); got != 3 {
		t.Errorf("tfy output_tokens = %d, want 3", got)
	}
	if v, ok := a["tfy.model.streaming"]; !ok || !v.GetBoolValue() {
		t.Error("tfy.model.streaming should be true")
	}
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
	s := spanByName(spans, "ChatCompletion: gpt-4o-mini")
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
	// tfy.input/tfy.output carry the RAW request/response bodies.
	tin := attrStr(a, "tfy.input")
	if !strings.Contains(tin, `"model":"gpt-4o-mini"`) || !strings.Contains(tin, `"hello world"`) {
		t.Errorf("tfy.input should be the raw request body, got %q", tin)
	}
	tout := attrStr(a, "tfy.output")
	if !strings.Contains(tout, `"usage"`) || !strings.Contains(tout, `hi there`) {
		t.Errorf("tfy.output should be the raw response body, got %q", tout)
	}
}

// TestTracingTruefoundryAttributesDisabled verifies the flag turns off tfy.*.
func TestTracingTruefoundryAttributesDisabled(t *testing.T) {
	off := false
	h := newTraceHarness(t, func(c *config.TracingConfig) { c.TruefoundryAttributes = &off })

	h.post(t, "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	spans := h.receiver.waitForSpans(t, 1)
	// With tfy off, the span uses the gen_ai naming convention again.
	s := spanByName(spans, "chat gpt-4o-mini")
	if s == nil {
		t.Fatalf("no 'chat gpt-4o-mini' span; got %v", spanNames(spans))
	}
	a := attrs(s)
	if _, ok := a["tfy.span_type"]; ok {
		t.Error("tfy.span_type present despite truefoundry_attributes=false")
	}
	if _, ok := a["tfy.request.created_by_subject"]; ok {
		t.Error("tfy.request.created_by_subject present despite truefoundry_attributes=false")
	}
	// The vendor-neutral user.id is emitted regardless of the tfy schema flag.
	if got := attrStr(a, "user.id"); got != "alice" {
		t.Errorf("user.id = %q, want alice", got)
	}
	if got := attrStr(a, "gen_ai.request.model"); got != "gpt-4o-mini" {
		t.Errorf("gen_ai.request.model = %q, want gpt-4o-mini", got)
	}
}

// TestTracingAttributionOverride verifies config-supplied attribution (subject,
// tenant, teams, metadata) overrides the OS-user default and rides on the span.
func TestTracingAttributionOverride(t *testing.T) {
	h := newTraceHarness(t, func(c *config.TracingConfig) {
		c.Attribution = config.AttributionConfig{
			Subject:  "bob@corp.example",
			Tenant:   "acme",
			Teams:    []string{"platform", "ml"},
			Metadata: map[string]string{"cost_center": "research"},
		}
	})

	h.post(t, "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	spans := h.receiver.waitForSpans(t, 1)
	s := spanByName(spans, "ChatCompletion: gpt-4o-mini")
	if s == nil {
		t.Fatalf("no span; got %v", spanNames(spans))
	}
	a := attrs(s)
	if got := attrStr(a, "user.id"); got != "bob@corp.example" {
		t.Errorf("user.id = %q, want bob@corp.example", got)
	}
	subj := attrStr(a, "tfy.request.created_by_subject")
	if !strings.Contains(subj, `"subjectSlug":"bob@corp.example"`) || !strings.Contains(subj, `"tenantName":"acme"`) {
		t.Errorf("created_by_subject = %q, want subjectSlug=bob@corp.example, tenantName=acme", subj)
	}
	if got := attrStr(a, "tfy.request.tenant_name"); got != "acme" {
		t.Errorf("tfy.request.tenant_name = %q, want acme", got)
	}
	if v, ok := a["tfy.request.created_by_subject_teams"]; !ok {
		t.Error("tfy.request.created_by_subject_teams missing")
	} else if vals := v.GetArrayValue().GetValues(); len(vals) != 2 || vals[0].GetStringValue() != "platform" {
		t.Errorf("teams = %v, want [platform ml]", vals)
	}
	meta := attrStr(a, "tfy.request.metadata")
	if !strings.Contains(meta, `"cost_center":"research"`) || !strings.Contains(meta, `"aitori.app":"test-llm-app"`) {
		t.Errorf("tfy.request.metadata = %q, want cost_center + aitori.app", meta)
	}
}

// TestTracingDecodesGzipResponse drives a gzip-encoded upstream response through
// the proxy and asserts the span carries DECODED token usage and body — i.e. the
// compressed bytes were gunzipped for inspection (and the export was not dropped
// for invalid UTF-8), while the client still received a readable response.
func TestTracingDecodesGzipResponse(t *testing.T) {
	h := newTraceHarness(t, func(c *config.TracingConfig) { c.CaptureContent = true })

	body := h.post(t, "/v1/gzip", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, "gzipped hi") {
		t.Fatalf("client did not receive a decodable body: %q", body)
	}

	spans := h.receiver.waitForSpans(t, 1)
	s := spanByName(spans, "ChatCompletion: gpt-4o-mini")
	if s == nil {
		t.Fatalf("no span (export may have been dropped); got %v", spanNames(spans))
	}
	a := attrs(s)
	if got := attrInt(a, "tfy.model.metric.input_tokens"); got != 21 {
		t.Errorf("input_tokens = %d, want 21 (from the gunzipped body)", got)
	}
	if got := attrInt(a, "tfy.model.metric.output_tokens"); got != 9 {
		t.Errorf("output_tokens = %d, want 9 (from the gunzipped body)", got)
	}
	tout := attrStr(a, "tfy.output")
	if !strings.Contains(tout, "gzipped hi") || !strings.Contains(tout, `"usage"`) {
		t.Errorf("tfy.output should be the DECODED JSON body, got %q", tout)
	}
}

func spanNames(spans []*tracepb.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}
