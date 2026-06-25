// Command aitori-gateway is a local stand-in for an AI gateway, for end-to-end
// testing and demos of the aitori agent (see docs/development.md).
//
// It implements the gateway side of the reroute contract:
//   - authenticates the user from x-tfy-api-key (any non-empty value here),
//   - logs the request,
//   - strips every x-tfy-* header,
//   - forwards to x-tfy-original-url with the original method/body/headers
//     (including the app's own provider credentials),
//   - streams the response back verbatim.
//
// Every proxied request is recorded as an OpenTelemetry span (method, original
// URL, app, category, status, latency, and request/response bodies as events)
// into a local SQLite database, and a built-in web UI (served on the same
// address under /ui) lets you browse and search the traces.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/truefoundry/aitori/tools/aitori-gateway/tracestore"
)

const (
	// maxLogBody caps how much rendered (decoded) body text we keep per span
	// event / log line.
	maxLogBody = 16 << 10 // 16 KiB
	// captureCap caps how many raw response bytes we buffer (larger than
	// maxLogBody so a gzip stream can still be decoded before truncation).
	captureCap = 256 << 10 // 256 KiB
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	debug := flag.Bool("debug", false, "also log request/response bodies to stderr")
	dbPath := flag.String("db", "aitori-gateway-traces.db", "SQLite path for trace storage")
	uiPath := flag.String("ui", "/ui", "URL path prefix for the trace UI")
	requireAuth := flag.Bool("require-auth", false, "reject reroutes missing x-tfy-api-key with 401 (default: accept, for aitori's auth-disabled mode)")
	flag.Parse()

	store, err := tracestore.Open(*dbPath)
	if err != nil {
		log.Fatalf("aitori-gateway: %v", err)
	}
	defer store.Close()

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(tracestore.NewExporter(store))),
	)
	defer func() {
		_ = tp.Shutdown(context.Background())
	}()
	tracer := tp.Tracer("aitori-gateway")

	transport := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	mux := http.NewServeMux()
	mux.Handle(*uiPath+"/", tracestore.NewUI(store, *uiPath))
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/", proxyHandler(tracer, transport, *debug, *requireAuth))

	log.Printf("aitori-gateway listening on http://%s (debug=%t, require-auth=%t)", *addr, *debug, *requireAuth)
	log.Printf("  trace UI:  http://%s%s/", *addr, *uiPath)
	log.Printf("  trace db:  %s", *dbPath)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func proxyHandler(tracer trace.Tracer, transport *http.Transport, debug, requireAuth bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("x-tfy-api-key")
		orig := r.Header.Get("x-tfy-original-url")

		// Per-request attribution and device identity travel in one JSON context
		// header (x-tfy-metadata) rather than separate x-tfy-* headers.
		var pc struct {
			App          string `json:"app"`
			PID          string `json:"pid"`
			Category     string `json:"category"`
			Host         string `json:"host"`
			OS           string `json:"os"`
			AgentVersion string `json:"agent_version"`
		}
		if raw := r.Header.Get("x-tfy-metadata"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &pc)
		}
		app, category := pc.App, pc.Category

		// Only genuine reroutes carry x-tfy-original-url. Health checks, favicons,
		// and stray requests to the gateway are answered without a span, so the
		// gateway never traces itself.
		if orig == "" {
			http.Error(w, "aitori-gateway: missing x-tfy-original-url", http.StatusBadRequest)
			return
		}

		ctx, span := tracer.Start(r.Context(), spanName(r.Method, orig), trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		span.SetAttributes(
			attribute.String(tracestore.AttrHTTPMethod, r.Method),
			attribute.String(tracestore.AttrURL, orig),
			attribute.String(tracestore.AttrApp, app),
			attribute.String(tracestore.AttrCategory, category),
		)
		// Top-level span attributes parsed from the context header.
		if pc.PID != "" {
			span.SetAttributes(attribute.String("tfy.pid", pc.PID))
		}
		if pc.Host != "" {
			span.SetAttributes(attribute.String("tfy.host", pc.Host))
		}
		if pc.OS != "" {
			span.SetAttributes(attribute.String("tfy.os", pc.OS))
		}
		if pc.AgentVersion != "" {
			span.SetAttributes(attribute.String("tfy.agent_version", pc.AgentVersion))
		}
		// Record every incoming request header as a span attribute (sensitive
		// values redacted), so the trace UI shows the full header set.
		span.SetAttributes(headerAttributes(r.Header)...)

		log.Printf("reroute: %s %s  app=%q category=%q token=%s", r.Method, orig, app, category, redact(token))

		// Auth mode is configurable. With -require-auth, a missing token is a 401
		// (mimics a token-authenticating gateway). By default the token is
		// optional, so aitori's gateway.auth.disabled / --no-auth path (where the
		// real gateway authenticates another way — mTLS, network ACL) works
		// against this local stand-in.
		if token == "" {
			if requireAuth {
				fail(w, span, http.StatusUnauthorized, "aitori-gateway: missing x-tfy-api-key (-require-auth)")
				return
			}
			log.Printf("  (no x-tfy-api-key — auth not required; forwarding anyway)")
		}

		// Buffer the request body so we can both forward it and record it as a
		// span event (capped/decoded). LLM request bodies are small.
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body.Close()
		}
		if len(reqBody) > 0 {
			rendered := renderBody(reqBody, r.Header, len(reqBody), false)
			span.AddEvent("request.body", trace.WithAttributes(attribute.String("body", capString(rendered))))
			if debug {
				log.Printf("  > request body (%d bytes):\n%s", len(reqBody), rendered)
			}
		}

		out, err := http.NewRequestWithContext(ctx, r.Method, orig, bytes.NewReader(reqBody))
		if err != nil {
			fail(w, span, http.StatusBadGateway, err.Error())
			return
		}
		for k, vs := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-tfy-") {
				continue // strip all x-tfy-* before forwarding upstream
			}
			for _, v := range vs {
				out.Header.Add(k, v)
			}
		}
		out.ContentLength = int64(len(reqBody))

		res, err := transport.RoundTrip(out)
		if err != nil {
			fail(w, span, http.StatusBadGateway, err.Error())
			return
		}
		defer res.Body.Close()

		for k, vs := range res.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		span.SetAttributes(attribute.Int(tracestore.AttrHTTPStatus, res.StatusCode))
		if res.StatusCode >= 400 {
			span.SetStatus(codes.Error, res.Status)
		} else {
			span.SetStatus(codes.Ok, "")
		}

		// Stream the response back while tee-ing a capped prefix for the span
		// event (so SSE/streaming cadence is preserved).
		respCap := &bytes.Buffer{}
		var total int
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := res.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				total += n
				if room := captureCap - respCap.Len(); room > 0 {
					respCap.Write(buf[:min(n, room)])
				}
			}
			if rerr != nil {
				break
			}
		}
		rendered := renderBody(respCap.Bytes(), res.Header, total, total > respCap.Len())
		span.AddEvent("response.body", trace.WithAttributes(attribute.String("body", capString(rendered))))
		if debug {
			log.Printf("  < response %d body (%d bytes):\n%s", res.StatusCode, total, rendered)
		}
	}
}

// fail records an error status on the span and writes an HTTP error.
func fail(w http.ResponseWriter, span trace.Span, status int, msg string) {
	span.SetAttributes(attribute.Int(tracestore.AttrHTTPStatus, status))
	span.SetStatus(codes.Error, msg)
	http.Error(w, msg, status)
}

// spanName builds a concise span name from the original URL.
func spanName(method, orig string) string {
	if u, err := url.Parse(orig); err == nil && u.Host != "" {
		return method + " " + u.Host + u.Path
	}
	return method + " (gateway)"
}

// capString trims s to maxLogBody for storage as a span attribute.
func capString(s string) string {
	if len(s) > maxLogBody {
		return s[:maxLogBody] + "\n... (truncated)"
	}
	return s
}

func redact(tok string) string {
	if tok == "" {
		return "(none)"
	}
	return "***redacted***"
}

// sensitiveHeader reports whether a header's value must be redacted in traces.
func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "x-tfy-api-key", "cookie", "set-cookie":
		return true
	}
	return false
}

// headerAttributes returns one span attribute per request header, keyed
// "http.request.header.<lowercased-name>", with multi-values joined and
// sensitive values redacted.
func headerAttributes(h http.Header) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(h))
	for k, vs := range h {
		val := "***redacted***"
		if !sensitiveHeader(k) {
			val = capString(strings.Join(vs, ", "))
		}
		attrs = append(attrs, attribute.String("http.request.header."+strings.ToLower(k), val))
	}
	return attrs
}

// renderBody returns a human-readable rendering of a body: it decompresses gzip,
// prints only valid UTF-8 text (summarizing binary content), and notes capture
// truncation. total is the full body size; raw may be a capped prefix of it.
func renderBody(raw []byte, h http.Header, total int, captureCapped bool) string {
	if total == 0 {
		return "(empty)"
	}
	enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	ctype := h.Get("Content-Type")

	data := raw
	switch enc {
	case "", "identity":
	case "gzip", "x-gzip":
		if gz, gerr := gzip.NewReader(bytes.NewReader(raw)); gerr == nil {
			if dec, _ := io.ReadAll(gz); len(dec) > 0 {
				data = dec
			}
		}
	default:
		return fmt.Sprintf("(%d bytes, content-type=%q, content-encoding=%q — not decoded)", total, ctype, enc)
	}

	if !isProbablyText(data) {
		return fmt.Sprintf("(binary body, %d bytes, content-type=%q, content-encoding=%q)", total, ctype, enc)
	}
	s := string(data)
	if captureCapped {
		s += fmt.Sprintf("\n... (capture capped; %d bytes streamed total)", total)
	}
	return s
}

func isProbablyText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}
