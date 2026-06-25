// Package router implements the reroute rewriter (plan §4, §8.3). It mutates a
// request in place so it targets the gateway, carrying the original absolute
// URL and the user's token in additive x-tfy-* headers. The provider's own
// credentials are never touched, and the token is attached ONLY here, on the
// reroute path.
package router

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/truefoundry/aitori/internal/circuit"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/hostmatch"
	"github.com/truefoundry/aitori/internal/token"
)

// HeaderCtx is the DEFAULT name of the consolidated context header. On the
// agent->gateway leg aitori now sends only three additive headers: the auth
// token, the original-URL header, and this single context header (plus any
// statically configured gateway.headers). All per-request attribution
// (app/pid/category) and device identity (host/os) travel inside its JSON
// payload instead of separate x-tfy-* headers, keeping the header footprint
// small (avoids 431 Request Header Fields Too Large on strict gateways).
// Override the name via gateway.header_ctx. It is x-tfy-* so the gateway strips
// it before forwarding upstream.
const HeaderCtx = "x-tfy-metadata"

// proxyCtx is the JSON payload of the consolidated context header. App/PID/
// Category are per-request attribution; Host/OS identify the device aitori runs
// on; AgentVersion is the agent build. (The original URL rides in its own
// header, not here, so the gateway can route without parsing this payload.)
type proxyCtx struct {
	App          string `json:"app,omitempty"`
	PID          string `json:"pid,omitempty"`
	Category     string `json:"category,omitempty"`
	Host         string `json:"host,omitempty"`
	OS           string `json:"os,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// maxCtxValue bounds each context-header value, so a pathological app name or
// hostname can't bloat the header.
const maxCtxValue = 128

// clampCtx truncates s to at most maxCtxValue characters (runes), on a rune
// boundary so the result stays valid UTF-8.
func clampCtx(s string) string {
	if len(s) <= maxCtxValue { // byte length <= n implies rune count <= n
		return s
	}
	r := []rune(s)
	if len(r) <= maxCtxValue {
		return s
	}
	return string(r[:maxCtxValue])
}

// Router rewrites rerouted requests to target the gateway.
type Router struct {
	gatewayURL  *url.URL
	gatewayHost string
	token       token.Source
	breaker     *circuit.Breaker

	headerTok    string
	headerOrig   string
	headerCtx    string
	onError      string
	version      string
	authDisabled bool
	deviceHost   string
	deviceOS     string
	headers      map[string]string // extra static headers for the gateway leg
}

// Options configures a Router.
type Options struct {
	GatewayURL  *url.URL
	Token       token.Source
	Breaker     *circuit.Breaker
	HeaderToken string // default "x-tfy-api-key"
	HeaderOrig  string // default "x-tfy-original-url"
	HeaderCtx   string // consolidated context header; default "x-tfy-metadata"
	OnError     string // fail_open | fail_closed
	// AuthDisabled reroutes without requiring a token; the token header is still
	// attached if one happens to be available.
	AuthDisabled bool
	AgentVersion string
	Headers      map[string]string // extra static headers added on the gateway leg
	// DeviceHost and DeviceOS identify the machine aitori runs on; they ride in
	// the consolidated context header (HeaderCtx).
	DeviceHost string
	DeviceOS   string
}

// New constructs a Router.
func New(o Options) *Router {
	headerTok := o.HeaderToken
	if headerTok == "" {
		headerTok = "x-tfy-api-key"
	}
	headerOrig := o.HeaderOrig
	if headerOrig == "" {
		headerOrig = "x-tfy-original-url"
	}
	headerCtx := o.HeaderCtx
	if headerCtx == "" {
		headerCtx = HeaderCtx
	}
	onError := o.OnError
	if onError == "" {
		onError = config.FailOpen
	}
	br := o.Breaker
	if br == nil {
		br = circuit.New(0, 0)
	}
	r := &Router{
		gatewayURL:   o.GatewayURL,
		token:        o.Token,
		breaker:      br,
		headerTok:    headerTok,
		headerOrig:   headerOrig,
		headerCtx:    headerCtx,
		onError:      onError,
		version:      o.AgentVersion,
		authDisabled: o.AuthDisabled,
		deviceHost:   o.DeviceHost,
		deviceOS:     o.DeviceOS,
		headers:      o.Headers,
	}
	if o.GatewayURL != nil {
		r.gatewayHost = hostmatch.Normalize(o.GatewayURL.Host)
	}
	return r
}

// Rewrite mutates req in place to target the gateway. It returns false when the
// request must NOT be rerouted (no gateway, no token, breaker open, or a loop
// is detected) — in which case the caller forwards directly to the original
// upstream (fail-open). On the reroute path it returns true.
func (r *Router) Rewrite(req *http.Request, appID, category string, pid int) (rerouted bool) {
	if r.gatewayURL == nil {
		return false
	}

	// 0. Loop guard: never reroute a request that already targets the gateway.
	if r.gatewayHost != "" {
		if hostmatch.Normalize(req.Host) == r.gatewayHost || hostmatch.Normalize(req.URL.Host) == r.gatewayHost {
			return false
		}
	}

	// 1. No token -> fail open (the token is the identity; without it the
	//    gateway cannot attribute the call). Skipped when auth is disabled: the
	//    gateway authenticates the agent some other way, so reroute regardless.
	tok := ""
	if r.token != nil {
		tok = r.token.Get()
	}
	if tok == "" && !r.authDisabled {
		return false
	}

	// 2. Gateway unhealthy -> fail open.
	if r.breaker.Open() {
		return false
	}

	// 3. Capture the original absolute URL BEFORE mutating req.
	orig := absoluteURL(req)

	// 4. Additive headers. The provider's own auth headers are left untouched.
	//    Only three x-tfy-* headers go on the gateway leg — original URL, auth
	//    token, and the consolidated context header — plus any statically
	//    configured gateway headers. Configured statics are applied first so the
	//    x-tfy-* headers below take precedence over any collision.
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(r.headerOrig, orig)
	if tok != "" {
		req.Header.Set(r.headerTok, tok)
	}
	// Consolidated context header: all attribution (app/pid/category) and device
	// identity (host/os/agent version) in one JSON header rather than separate
	// x-tfy-* headers. All values are strings, each clamped to maxCtxValue chars
	// so an unusual app name / hostname can't bloat the header. Always set
	// (device fields are present even when a request is unattributed).
	pidStr := ""
	if pid > 0 {
		pidStr = strconv.Itoa(pid)
	}
	ctx := proxyCtx{
		App:          clampCtx(appID),
		PID:          pidStr,
		Category:     clampCtx(category),
		Host:         clampCtx(r.deviceHost),
		OS:           clampCtx(r.deviceOS),
		AgentVersion: clampCtx(r.version),
	}
	if b, err := json.Marshal(ctx); err == nil {
		req.Header.Set(r.headerCtx, string(b))
	}

	// 5. Point the upstream dial at the gateway. Scheme/host come from the
	//    gateway URL; when gateway.url includes a path (e.g.
	//    https://gw/api/llm/tf-edge-proxy) that path is used VERBATIM as the
	//    endpoint — the request's own path is NOT appended to it. The real
	//    destination travels in x-tfy-original-url, which the gateway reads. A
	//    root/empty gateway path preserves the original request path (back-compat
	//    with gateways routed purely by header, e.g. the mock gateway).
	req.URL.Scheme = r.gatewayURL.Scheme
	req.URL.Host = r.gatewayURL.Host
	req.Host = r.gatewayURL.Host
	if p := r.gatewayURL.Path; p != "" && p != "/" {
		req.URL.Path = p
		req.URL.RawPath = r.gatewayURL.RawPath
	}
	// Any query configured on gateway.url is preserved, merged ahead of the
	// request's own query. (The original URL + query also rides in
	// x-tfy-original-url, so the gateway has both.)
	if gq := r.gatewayURL.RawQuery; gq != "" {
		if req.URL.RawQuery == "" {
			req.URL.RawQuery = gq
		} else {
			req.URL.RawQuery = gq + "&" + req.URL.RawQuery
		}
	}

	return true
}

// absoluteURL reconstructs scheme://host[:port]/path?query from the (pre-mutation)
// request, defaulting the scheme to https for MITM'd connections.
func absoluteURL(req *http.Request) string {
	scheme := req.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	return scheme + "://" + host + req.URL.RequestURI()
}

// FailClosed reports whether the router is configured to fail closed.
func (r *Router) FailClosed() bool { return r.onError == config.FailClosed }

// Breaker returns the gateway circuit breaker.
func (r *Router) Breaker() *circuit.Breaker { return r.breaker }

// GatewayURL returns the configured gateway URL (may be nil).
func (r *Router) GatewayURL() *url.URL { return r.gatewayURL }

// NoteGatewaySuccess and NoteGatewayFailure update gateway health.
func (r *Router) NoteGatewaySuccess() { r.breaker.Success() }
func (r *Router) NoteGatewayFailure() { r.breaker.Failure() }
