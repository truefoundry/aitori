package tracestore

import (
	"context"
	"strconv"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Well-known attribute keys the exporter denormalizes into searchable columns.
const (
	AttrApp        = "tfy.app"
	AttrCategory   = "tfy.category"
	AttrHTTPMethod = "http.request.method"
	AttrURL        = "url.full"
	AttrHTTPStatus = "http.response.status_code"
)

// Exporter is an OpenTelemetry SpanExporter that persists spans to a Store.
type Exporter struct{ store *Store }

// NewExporter returns a SpanExporter backed by store.
func NewExporter(store *Store) *Exporter { return &Exporter{store: store} }

// ExportSpans implements sdktrace.SpanExporter.
func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, ro := range spans {
		if err := e.store.Insert(ctx, fromReadOnly(ro)); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown implements sdktrace.SpanExporter.
func (e *Exporter) Shutdown(context.Context) error { return nil }

func fromReadOnly(ro sdktrace.ReadOnlySpan) Span {
	sc := ro.SpanContext()
	attrs := make(map[string]string, len(ro.Attributes()))
	for _, kv := range ro.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}

	sp := Span{
		SpanID:     sc.SpanID().String(),
		TraceID:    sc.TraceID().String(),
		Name:       ro.Name(),
		Kind:       ro.SpanKind().String(),
		StartNano:  ro.StartTime().UnixNano(),
		EndNano:    ro.EndTime().UnixNano(),
		DurationMS: float64(ro.EndTime().Sub(ro.StartTime()).Microseconds()) / 1000.0,
		StatusCode: ro.Status().Code.String(),
		StatusMsg:  ro.Status().Description,
		Attributes: attrs,
		App:        attrs[AttrApp],
		Category:   attrs[AttrCategory],
		HTTPMethod: attrs[AttrHTTPMethod],
		HTTPURL:    attrs[AttrURL],
	}
	if p := ro.Parent(); p.HasSpanID() {
		sp.ParentSpanID = p.SpanID().String()
	}
	if v := attrs[AttrHTTPStatus]; v != "" {
		sp.HTTPStatus, _ = strconv.Atoi(v)
	}
	for _, ev := range ro.Events() {
		e := Event{Name: ev.Name, TimeNano: ev.Time.UnixNano()}
		if len(ev.Attributes) > 0 {
			e.Attrs = make(map[string]string, len(ev.Attributes))
			for _, kv := range ev.Attributes {
				e.Attrs[string(kv.Key)] = kv.Value.Emit()
			}
		}
		sp.Events = append(sp.Events, e)
	}
	return sp
}
