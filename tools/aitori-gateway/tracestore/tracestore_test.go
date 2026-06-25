package tracestore

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "traces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStoreInsertQuery(t *testing.T) {
	st := openTemp(t)
	ctx := context.Background()
	sp := Span{
		SpanID: "s1", TraceID: "t1", Name: "POST api.anthropic.com/v1/messages",
		StartNano: 100, EndNano: 200, DurationMS: 0.1,
		App: "claude-code", Category: "llm", HTTPMethod: "POST",
		HTTPURL: "https://api.anthropic.com/v1/messages", HTTPStatus: 200,
		Attributes: map[string]string{"tfy.app": "claude-code"},
		Events:     []Event{{Name: "request.body", Attrs: map[string]string{"body": "hello"}}},
	}
	if err := st.Insert(ctx, sp); err != nil {
		t.Fatal(err)
	}

	got, err := st.Query(ctx, Filter{App: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SpanID != "s1" {
		t.Fatalf("query by app: %+v", got)
	}
	if got[0].Attributes["tfy.app"] != "claude-code" {
		t.Fatal("attributes not round-tripped")
	}
	if len(got[0].Events) != 1 || got[0].Events[0].Attrs["body"] != "hello" {
		t.Fatalf("events not round-tripped: %+v", got[0].Events)
	}

	// Filters.
	if g, _ := st.Query(ctx, Filter{App: "nope"}); len(g) != 0 {
		t.Fatal("app filter should exclude")
	}
	if g, _ := st.Query(ctx, Filter{Text: "v1/messages"}); len(g) != 1 {
		t.Fatal("text filter should match url")
	}
	if g, _ := st.Query(ctx, Filter{MinHTTP: 400}); len(g) != 0 {
		t.Fatal("status filter should exclude 200")
	}

	// Trace / Get / Apps.
	if g, _ := st.Trace(ctx, "t1"); len(g) != 1 {
		t.Fatal("trace lookup")
	}
	if _, ok, _ := st.Get(ctx, "s1"); !ok {
		t.Fatal("get existing")
	}
	if _, ok, _ := st.Get(ctx, "missing"); ok {
		t.Fatal("get missing should be ok=false")
	}
	if a, _ := st.Apps(ctx); len(a) != 1 || a[0] != "claude-code" {
		t.Fatalf("apps: %v", a)
	}
}

// End-to-end: a real OTel span exported through the SQLite exporter must persist
// with its denormalized fields and body event.
func TestExporterRoundTrip(t *testing.T) {
	st := openTemp(t)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(NewExporter(st))),
	)
	_, span := tp.Tracer("test").Start(context.Background(), "POST api.anthropic.com/v1/messages",
		trace.WithSpanKind(trace.SpanKindServer))
	span.SetAttributes(
		attribute.String(AttrApp, "claude-code"),
		attribute.String(AttrCategory, "llm"),
		attribute.String(AttrHTTPMethod, "POST"),
		attribute.String(AttrURL, "https://api.anthropic.com/v1/messages"),
		attribute.Int(AttrHTTPStatus, 200),
	)
	span.AddEvent("request.body", trace.WithAttributes(attribute.String("body", "hi")))
	span.End()
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := st.Query(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 span, got %d", len(got))
	}
	g := got[0]
	if g.TraceID == "" || g.SpanID == "" {
		t.Fatal("trace/span ids should be set")
	}
	if g.App != "claude-code" || g.Category != "llm" || g.HTTPMethod != "POST" || g.HTTPStatus != 200 {
		t.Fatalf("denormalized attributes wrong: %+v", g)
	}
	if g.HTTPURL != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("url = %q", g.HTTPURL)
	}
	if len(g.Events) != 1 || g.Events[0].Attrs["body"] != "hi" {
		t.Fatalf("event not persisted: %+v", g.Events)
	}
}
