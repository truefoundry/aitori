// Package tracing turns intercepted LLM/MCP exchanges into OpenTelemetry spans
// and exports them to a configured OTLP receiver, as an alternative to routing
// calls through a gateway for capture. It is the only place in the agent that
// depends on the OpenTelemetry SDK; the parsing that feeds it lives in the
// dependency-free internal/genai package.
//
// Spans follow the OpenTelemetry GenAI semantic conventions (gen_ai.*) for LLM
// calls, and carry JSON-RPC attributes for MCP calls. Export goes through a
// batch span processor so the request path is never blocked on the network.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/genai"
)

// scopeName identifies aitori as the instrumentation scope on every span.
const scopeName = "github.com/truefoundry/aitori"

// Provider owns the OpenTelemetry tracer provider and exports spans built from
// genai.Observation records. It satisfies the proxy's Tracer interface (Emit).
type Provider struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
	log    *slog.Logger
}

// New builds a Provider from the tracing config. It configures the OTLP
// exporter (HTTP or gRPC), a batch span processor, and a resource describing
// this agent. The caller must call Shutdown to flush buffered spans on exit.
func New(ctx context.Context, cfg config.TracingConfig, serviceVersion, deviceHost string, log *slog.Logger) (*Provider, error) {
	if log == nil {
		log = slog.Default()
	}
	// Route the SDK's internal errors (dropped spans, full queue, export
	// failures the batcher would otherwise swallow) to our logger, so a
	// misconfigured/unreachable collector is visible instead of silent.
	setErrorHandler(log)

	exporter, err := newExporter(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	// Wrap the exporter so every batch export is logged with the span count,
	// latency, and — crucially — whether the collector accepted it. This is the
	// signal for "are my traces getting through?".
	logged := &loggingExporter{
		SpanExporter: exporter,
		log:          log.With("otel_endpoint", displayEndpoint(cfg), "otel_protocol", cfg.ProtocolOrDefault()),
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceNameOrDefault()),
		semconv.ServiceVersion(serviceVersion),
		semconv.HostName(deviceHost),
	))
	if err != nil {
		// A schema-URL conflict is non-fatal: fall back to just our attributes.
		res = resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceNameOrDefault()),
			semconv.ServiceVersion(serviceVersion),
			semconv.HostName(deviceHost),
		)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler(cfg.SampleRatio)),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(logged, batchOptions(cfg.Batch)...),
	)
	logged.log.Info("otel tracing exporter ready",
		"service_name", cfg.ServiceNameOrDefault(), "host_name", deviceHost)

	return &Provider{
		tp:     tp,
		tracer: tp.Tracer(scopeName),
		log:    log,
	}, nil
}

// newExporter builds the OTLP span exporter for the configured protocol. When
// the logger has debug enabled and the protocol is HTTP, the exporter is given a
// client that logs each request/response to the collector (headers redacted).
func newExporter(ctx context.Context, cfg config.TracingConfig, log *slog.Logger) (*otlptrace.Exporter, error) {
	switch cfg.ProtocolOrDefault() {
	case config.TracingProtocolGRPC:
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(grpcEndpoint(cfg.Endpoint)),
			otlptracegrpc.WithHeaders(cfg.Headers),
			otlptracegrpc.WithTimeout(cfg.TimeoutOrDefault()),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	default: // http
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(httpEndpoint(cfg.Endpoint)),
			otlptracehttp.WithHeaders(cfg.Headers),
			otlptracehttp.WithTimeout(cfg.TimeoutOrDefault()),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if log.Enabled(ctx, slog.LevelDebug) {
			base := http.DefaultTransport.(*http.Transport).Clone()
			opts = append(opts, otlptracehttp.WithHTTPClient(&http.Client{
				Timeout:   cfg.TimeoutOrDefault(),
				Transport: &loggingRoundTripper{base: base, log: log},
			}))
		}
		return otlptracehttp.New(ctx, opts...)
	}
}

// loggingRoundTripper logs the OTLP/HTTP export request sent to the collector
// (and the response line) at debug level, so the exact URL, headers, and body
// size can be inspected when a collector rejects traces. Sensitive header
// values (auth, api keys, cookies) are redacted.
type loggingRoundTripper struct {
	base http.RoundTripper
	log  *slog.Logger
}

func (l *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	l.log.Debug("otel http export request",
		"method", req.Method,
		"url", req.URL.String(),
		"content_type", req.Header.Get("Content-Type"),
		"content_encoding", req.Header.Get("Content-Encoding"),
		"body_bytes", req.ContentLength,
		"headers", redactHeaders(req.Header),
	)
	res, err := l.base.RoundTrip(req)
	if err != nil {
		l.log.Debug("otel http export response", "err", err)
		return res, err
	}
	l.log.Debug("otel http export response", "status", res.Status)
	return res, nil
}

// sensitiveHeaders whose values must never be logged.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"x-api-key":           true,
	"api-key":             true,
	"cookie":              true,
	"proxy-authorization": true,
}

// redactHeaders returns header names with sensitive values masked, so a debug
// dump can confirm which headers are sent (e.g. TFY-Tracing-Project) without
// leaking credentials.
func redactHeaders(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, vs := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			m[k] = "<redacted>"
			continue
		}
		m[k] = strings.Join(vs, ",")
	}
	return m
}

// loggingExporter decorates a SpanExporter to log the result of each batch
// export: how many spans, how long it took, and whether the collector accepted
// them. A non-nil error means the batch was rejected/undeliverable (bad
// endpoint, auth failure, network error, non-2xx status) and those spans are
// dropped by the batch processor.
type loggingExporter struct {
	sdktrace.SpanExporter
	log *slog.Logger
}

func (e *loggingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	start := time.Now()
	err := e.SpanExporter.ExportSpans(ctx, spans)
	took := time.Since(start)
	if err != nil {
		e.log.Error("otel export failed (spans dropped)",
			"spans", len(spans), "took", took, "err", err)
		return err
	}
	e.log.Info("otel export ok (accepted by collector)",
		"spans", len(spans), "took", took)
	return nil
}

// errHandlerOnce guards installing the process-global OTel error handler so
// multiple providers (e.g. across reloads) don't stack handlers.
var errHandlerOnce sync.Once

func setErrorHandler(log *slog.Logger) {
	errHandlerOnce.Do(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			log.Error("otel sdk error", "err", err)
		}))
	})
}

// displayEndpoint returns the effective destination for logging, matching what
// the configured protocol will actually dial.
func displayEndpoint(cfg config.TracingConfig) string {
	if cfg.ProtocolOrDefault() == config.TracingProtocolGRPC {
		return grpcEndpoint(cfg.Endpoint)
	}
	return httpEndpoint(cfg.Endpoint)
}

// httpEndpoint builds the full OTLP/HTTP traces URL for otlptracehttp's
// WithEndpointURL, which uses the URL's path verbatim (it does NOT append the
// "/v1/traces" signal path). So we append it ourselves onto the configured base
// path — e.g. "https://host/api/otel" -> "https://host/api/otel/v1/traces" —
// unless the user already included it. This matches how OTLP base endpoints are
// specified (the traceloop/OTel SDKs append the same signal path to a base URL).
func httpEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	p := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(p, "/v1/traces") {
		p += "/v1/traces"
	}
	u.Path = p
	return u.String()
}

// grpcEndpoint reduces a URL-or-authority to the host:port gRPC wants.
func grpcEndpoint(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

func sampler(ratio float64) sdktrace.Sampler {
	if ratio <= 0 || ratio >= 1 {
		return sdktrace.AlwaysSample()
	}
	return sdktrace.TraceIDRatioBased(ratio)
}

func batchOptions(b config.BatchConfig) []sdktrace.BatchSpanProcessorOption {
	var opts []sdktrace.BatchSpanProcessorOption
	if b.MaxQueueSize > 0 {
		opts = append(opts, sdktrace.WithMaxQueueSize(b.MaxQueueSize))
	}
	if b.MaxExportBatchSize > 0 {
		opts = append(opts, sdktrace.WithMaxExportBatchSize(b.MaxExportBatchSize))
	}
	if d, err := time.ParseDuration(b.BatchTimeout); err == nil && d > 0 {
		opts = append(opts, sdktrace.WithBatchTimeout(d))
	}
	if d, err := time.ParseDuration(b.ExportTimeout); err == nil && d > 0 {
		opts = append(opts, sdktrace.WithExportTimeout(d))
	}
	return opts
}

// Shutdown flushes buffered spans and stops the exporter. Bounded by ctx.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// Emit builds and records one span from an observation. Recording a completed
// span with explicit start/end timestamps just enqueues it on the batch
// processor, so this never blocks on the network and is safe to call from the
// request goroutine.
func (p *Provider) Emit(o genai.Observation) {
	if p == nil || p.tp == nil {
		return
	}
	start := o.Start
	if start.IsZero() {
		start = time.Now()
	}
	end := o.End
	if end.IsZero() {
		end = time.Now()
	}
	name := spanName(o)
	_, span := p.tracer.Start(context.Background(), name,
		trace.WithTimestamp(start),
		trace.WithSpanKind(trace.SpanKindClient),
	)
	attrs := setAttributes(span, o)
	setStatus(span, o)
	span.End(trace.WithTimestamp(end))

	if p.log.Enabled(context.Background(), slog.LevelDebug) {
		p.log.Debug("otel span emitted",
			"name", name,
			"trace_id", span.SpanContext().TraceID().String(),
			"span_id", span.SpanContext().SpanID().String(),
			"duration", end.Sub(start),
			"attributes", attrMap(attrs),
		)
	}
}

// attrMap renders span attributes as a plain map for structured debug logging.
func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// spanName follows the GenAI convention "{operation} {model}" for LLM calls,
// "mcp {method}" for MCP, and a generic "{METHOD} {host}" otherwise.
func spanName(o genai.Observation) string {
	switch o.Category {
	case config.CategoryLLM:
		op := o.Request.Operation
		if op == "" {
			op = genai.OpChat
		}
		if o.Request.Model != "" {
			return op + " " + o.Request.Model
		}
		return op
	case config.CategoryMCP:
		if o.Request.RPCMethod != "" {
			return "mcp " + o.Request.RPCMethod
		}
		return "mcp"
	default:
		return fmt.Sprintf("%s %s", o.Method, o.Host)
	}
}
