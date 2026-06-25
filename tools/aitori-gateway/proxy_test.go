package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
)

// TestProxyHandlerAuthModes covers the token × require-auth matrix and the
// missing-original-URL guard. With -require-auth off (the default), a reroute
// with no x-tfy-api-key must still be forwarded — that's aitori's
// gateway.auth.disabled / --no-auth path.
func TestProxyHandlerAuthModes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()

	tracer := noop.NewTracerProvider().Tracer("test")
	transport := &http.Transport{}

	cases := []struct {
		name        string
		requireAuth bool
		token       string // "" => header omitted
		orig        string // "" => header omitted
		want        int
	}{
		{"missing original-url -> 400", false, "tok", "", http.StatusBadRequest},
		{"require-auth + no token -> 401", true, "", upstream.URL + "/v1/x", http.StatusUnauthorized},
		{"require-auth + token -> forwarded", true, "tok", upstream.URL + "/v1/x", http.StatusOK},
		{"no-auth + no token -> forwarded", false, "", upstream.URL + "/v1/x", http.StatusOK},
		{"no-auth + token -> forwarded", false, "tok", upstream.URL + "/v1/x", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := proxyHandler(tracer, transport, false, c.requireAuth)
			req := httptest.NewRequest("POST", "http://gateway.local/", strings.NewReader(`{"model":"x"}`))
			if c.token != "" {
				req.Header.Set("x-tfy-api-key", c.token)
			}
			if c.orig != "" {
				req.Header.Set("x-tfy-original-url", c.orig)
			}
			rec := httptest.NewRecorder()

			h(rec, req)

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, c.want, rec.Body.String())
			}
			if c.want == http.StatusOK && rec.Body.String() != "upstream-ok" {
				t.Fatalf("body = %q, want upstream-ok", rec.Body.String())
			}
		})
	}
}
