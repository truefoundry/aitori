package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/genai"
)

func TestHTTPEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://host/api/otel":           "https://host/api/otel/v1/traces",
		"https://host/api/otel/":          "https://host/api/otel/v1/traces",
		"https://host/api/otel/v1/traces": "https://host/api/otel/v1/traces",
		"https://host":                    "https://host/v1/traces",
		"http://localhost:4318":           "http://localhost:4318/v1/traces",
	}
	for in, want := range cases {
		if got := httpEndpoint(in); got != want {
			t.Errorf("httpEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGRPCEndpoint(t *testing.T) {
	if got := grpcEndpoint("https://collector:4317"); got != "collector:4317" {
		t.Errorf("grpcEndpoint = %q", got)
	}
	if got := grpcEndpoint("collector:4317"); got != "collector:4317" {
		t.Errorf("grpcEndpoint(authority) = %q", got)
	}
}

// TestNewAndEmit verifies a provider can be built with a batch processor and a
// span emitted (enqueued) without contacting a live collector. Shutdown bounds
// the export attempt so the test never hangs on the unreachable endpoint.
func TestNewAndEmit(t *testing.T) {
	cfg := config.TracingConfig{
		Enabled:  true,
		Endpoint: "http://127.0.0.1:4318",
		Protocol: config.TracingProtocolHTTP,
		Insecure: true,
		Batch:    config.BatchConfig{MaxExportBatchSize: 1, BatchTimeout: "1s"},
	}
	p, err := New(context.Background(), cfg, "test", "dev-host", "alice", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Emit(genai.Observation{
		Category: config.CategoryLLM,
		Method:   "POST",
		Host:     "api.openai.com",
		Path:     "/v1/chat/completions",
		Status:   200,
		Request:  genai.RequestInfo{Provider: genai.ProviderOpenAI, Operation: genai.OpChat, Model: "gpt-4o"},
		Response: genai.ResponseInfo{InputTokens: 10, OutputTokens: 5},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // bound shutdown: don't block on the unreachable collector
	_ = p.Shutdown(ctx)
}

// TestDebugLogsSpanAndHTTPRequest verifies that with a debug logger the exporter
// logs the span it emits and the actual HTTP request to the collector (with
// sensitive headers redacted), and that the debug-injected HTTP client still
// delivers the export.
func TestDebugLogsSpanAndHTTPRequest(t *testing.T) {
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := config.TracingConfig{
		Enabled:  true,
		Endpoint: srv.URL,
		Protocol: config.TracingProtocolHTTP,
		Insecure: true,
		Headers: map[string]string{
			"Authorization":       "Bearer super-secret",
			"TFY-Tracing-Project": "truefoundry:tracing-project:moh-trace",
		},
		Batch: config.BatchConfig{MaxExportBatchSize: 1, BatchTimeout: "50ms"},
	}
	p, err := New(context.Background(), cfg, "test", "dev-host", "alice", log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Emit(genai.Observation{
		Category: config.CategoryLLM,
		Method:   "POST",
		Host:     "api.openai.com",
		Path:     "/v1/chat/completions",
		Status:   200,
		Request:  genai.RequestInfo{Provider: genai.ProviderOpenAI, Operation: genai.OpChat, Model: "gpt-4o"},
		Response: genai.ResponseInfo{InputTokens: 10, OutputTokens: 5},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got.Load() == 0 {
		t.Fatal("collector received no export; debug HTTP client did not deliver")
	}

	out := buf.String()
	for _, want := range []string{
		"otel span emitted",
		"ChatCompletion: gpt-4o",
		"otel http export request",
		"/v1/traces",
		"Tfy-Tracing-Project",            // Go canonicalizes the header name
		"user.id",                        // vendor-neutral user attribution
		"tfy.request.created_by_subject", // gateway-shaped subject attribution
		"alice",                          // the resolved OS-user subject
	} {
		if !strings.Contains(out, want) {
			t.Errorf("debug log missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "super-secret") {
		t.Errorf("Authorization value leaked into debug log:\n%s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("expected redacted sensitive header; got:\n%s", out)
	}
}
