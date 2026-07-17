package proxy

import "github.com/truefoundry/aitori/internal/genai"

// Tracer receives one observation per intercepted exchange whose category is
// selected for tracing, and turns it into a span exported to an OTLP receiver.
// It is satisfied by *tracing.Provider. The interface lives here (rather than
// the proxy importing the tracing package) so the correctness-critical proxy
// core stays free of the OpenTelemetry SDK: it produces plain genai.Observation
// records and the concrete tracer, wired in by main, does the export.
//
// Emit must not block the request path: the tracing provider enqueues the span
// on a batch processor and returns immediately.
type Tracer interface {
	Emit(genai.Observation)
}
