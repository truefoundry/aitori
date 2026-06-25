// Package integration drives real requests through the aitori proxy in
// explicit-proxy mode against a mock gateway and a fake upstream, asserting the
// reroute contract (plan §4, §17): correct x-tfy-* headers on the agent->gateway
// leg, provider credentials preserved end-to-end, no x-tfy-* leaking to the
// upstream, byte-identical streamed responses, and fail-open behavior when the
// gateway dies.
package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/ca"
	"github.com/truefoundry/aitori/internal/circuit"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/pipeline"
	"github.com/truefoundry/aitori/internal/proxy"
	"github.com/truefoundry/aitori/internal/router"
	"github.com/truefoundry/aitori/internal/token"
)

const (
	upstreamName = "api.upstream.test"
	gatewayName  = "gw.test"
	otherName    = "other.test" // NOT in intercept_hosts: must be spliced raw
	providerAuth = "Bearer provider-secret"
	userToken    = "test-token"
)

// --- test PKI helpers ---

func genCert(t *testing.T, dnsName string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// nameDialer maps test hostnames to 127.0.0.1 while keeping the ephemeral port.
func nameDialer(m map[string]string) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if ip, ok := m[host]; ok {
				addr = net.JoinHostPort(ip, port)
			}
		}
		return d.DialContext(ctx, network, addr)
	}
}

// serveTLS starts an HTTP server with the given cert on 127.0.0.1:0 and returns
// its host:port and the server (so a test can stop it). It is torn down on test
// cleanup.
func serveTLS(t *testing.T, cert tls.Certificate, h http.Handler) (string, *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tln := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	srv := &http.Server{Handler: h}
	go srv.Serve(tln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), srv
}

func hasXTFY(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-tfy-") {
			return true
		}
	}
	return false
}

// --- the harness ---

type harness struct {
	client      *http.Client
	upstreamURL string // https://api.upstream.test:<port>
	otherURL    string // https://other.test:<port>
	otherX509   *x509.Certificate

	xtfyLeaked  atomic.Bool // upstream ever saw an x-tfy-* header
	badAuth     atomic.Bool // upstream saw wrong/missing provider auth
	gwReroutes  atomic.Int64
	upstreamHit atomic.Int64
	gotApp      atomic.Value // last x-tfy-app the gateway received

	// procInfo, when set, is returned by the proxy's process resolver to
	// simulate desktop-app PID attribution.
	procInfo atomic.Pointer[proxy.ProcInfo]

	stopGateway func()
}

func newHarness(t *testing.T) *harness {
	h := &harness{}

	upCert, upX509 := genCert(t, upstreamName)
	gwCert, gwX509 := genCert(t, gatewayName)

	// --- fake upstream ---
	upMux := http.NewServeMux()
	upMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.upstreamHit.Add(1)
		if hasXTFY(r.Header) {
			h.xtfyLeaked.Store(true)
		}
		if r.Header.Get("Authorization") != providerAuth {
			h.badAuth.Store(true)
		}
		switch r.URL.Path {
		case "/v1/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			for i := 0; i < 5; i++ {
				fmt.Fprintf(w, "data: chunk-%d\n\n", i)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(5 * time.Millisecond)
			}
		case "/telemetry":
			io.WriteString(w, "telemetry-ok")
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true,"path":"`+r.URL.Path+`"}`)
		}
	})
	upAddr, _ := serveTLS(t, upCert, upMux)
	_, upPort, _ := net.SplitHostPort(upAddr)
	h.upstreamURL = "https://" + net.JoinHostPort(upstreamName, upPort)

	// --- non-allowlisted host (must be spliced, never decrypted) ---
	otherCert, otherX509 := genCert(t, otherName)
	h.otherX509 = otherX509
	otherAddr, _ := serveTLS(t, otherCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "other-ok")
	}))
	_, otherPort, _ := net.SplitHostPort(otherAddr)
	h.otherURL = "https://" + net.JoinHostPort(otherName, otherPort)

	nameMap := map[string]string{upstreamName: "127.0.0.1", gatewayName: "127.0.0.1", otherName: "127.0.0.1"}

	// --- mock gateway: assert x-tfy-*, strip them, forward to original URL ---
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upX509)
	gwTransport := &http.Transport{
		DialContext:     nameDialer(nameMap),
		TLSClientConfig: &tls.Config{RootCAs: upstreamPool},
	}
	gwHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-tfy-api-key") != userToken {
			http.Error(w, "gateway: missing/incorrect x-tfy-api-key", http.StatusUnauthorized)
			return
		}
		orig := r.Header.Get("x-tfy-original-url")
		if orig == "" {
			http.Error(w, "gateway: missing x-tfy-original-url", http.StatusBadRequest)
			return
		}
		h.gwReroutes.Add(1)
		// Attribution now arrives inside the consolidated ctx header, not a
		// separate x-tfy-app header.
		var pc struct {
			App string `json:"app"`
		}
		_ = json.Unmarshal([]byte(r.Header.Get("x-tfy-metadata")), &pc)
		h.gotApp.Store(pc.App)

		out, err := http.NewRequest(r.Method, orig, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, vs := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-tfy-") { // strip all x-tfy-*
				continue
			}
			for _, v := range vs {
				out.Header.Add(k, v)
			}
		}
		out.ContentLength = r.ContentLength

		res, err := gwTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		for k, vs := range res.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, rerr := res.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}
	})
	gwAddr, gwSrv := serveTLS(t, gwCert, gwHandler)
	_, gwPort, _ := net.SplitHostPort(gwAddr)
	gatewayURL := "https://" + net.JoinHostPort(gatewayName, gwPort)

	// --- the aitori agent ---
	cfg := config.Default()
	cfg.Gateway.URL = gatewayURL
	cfg.InterceptHosts = config.Hosts(upstreamName)
	cfg.Apps = []config.AppProfile{
		// Browser-origin traffic tagged by host.
		{ID: "test-app", Match: config.AppMatch{Browser: true, Hosts: []string{upstreamName}}},
		// Desktop app attributed by process identity (PID attribution).
		{ID: "desktop-app", Match: config.AppMatch{ProcessNames: []string{"TestDesktop"}}},
	}
	// Policy is top-level, decoupled from apps.
	cfg.Rules = []config.Rule{
		{Name: "llm", Hosts: []string{upstreamName}, PathPrefixes: []string{"/v1/"}, Category: config.CategoryLLM, Action: config.ActionReroute},
		{Name: "telemetry", Hosts: []string{upstreamName}, PathPrefixes: []string{"/telemetry"}, Category: config.CategoryOther, Action: config.ActionPassthrough},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}

	agentCA, err := ca.LoadOrCreate(t.TempDir(), ca.Options{Organization: "aitori-test"})
	if err != nil {
		t.Fatal(err)
	}

	// Agent egress trusts both the gateway and the upstream certs.
	egressPool := x509.NewCertPool()
	egressPool.AddCert(gwX509)
	egressPool.AddCert(upX509)
	agentTransport := &http.Transport{
		Proxy:           nil,
		DialContext:     nameDialer(nameMap),
		TLSClientConfig: &tls.Config{RootCAs: egressPool},
	}

	gwURL, _ := url.Parse(gatewayURL)
	rtr := router.New(router.Options{
		GatewayURL:  gwURL,
		Token:       token.NewStatic(userToken),
		Breaker:     circuit.New(3, 50*time.Millisecond),
		HeaderToken: cfg.Gateway.HeaderToken,
		HeaderOrig:  cfg.Gateway.HeaderOrig,
		OnError:     cfg.Gateway.OnError,
	})

	p := proxy.New(proxy.Options{
		Config:      &cfg,
		CA:          agentCA,
		Router:      rtr,
		Pipeline:    pipeline.New(&cfg),
		Transport:   agentTransport,
		DialContext: nameDialer(nameMap), // used by the raw splice path
		Proc: func(_, _ netip.AddrPort) (proxy.ProcInfo, bool) {
			if pi := h.procInfo.Load(); pi != nil {
				return *pi, true
			}
			return proxy.ProcInfo{}, false
		},
	})

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(pln)
	t.Cleanup(func() { p.Close() })

	// --- the client: uses the agent as its proxy and trusts the agent CA ---
	clientPool := x509.NewCertPool()
	clientPool.AddCert(agentCA.Cert())
	clientPool.AddCert(otherX509) // client trusts the real other.test cert (spliced)
	proxyURL, _ := url.Parse("http://" + pln.Addr().String())
	h.client = &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			TLSClientConfig:   &tls.Config{RootCAs: clientPool},
			ForceAttemptHTTP2: false,
		},
		Timeout: 10 * time.Second,
	}

	h.stopGateway = func() { gwSrv.Close() }
	return h
}

func (h *harness) do(t *testing.T, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.upstreamURL+path, strings.NewReader(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", providerAuth)
	req.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

func readClose(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --- tests ---

func TestRerouteHappyPath(t *testing.T) {
	h := newHarness(t)
	res := h.do(t, "POST", "/v1/messages")
	body := readClose(t, res)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%q", res.StatusCode, body)
	}
	if !strings.Contains(body, `"path":"/v1/messages"`) {
		t.Errorf("unexpected body: %q", body)
	}
	if h.gwReroutes.Load() != 1 {
		t.Errorf("gateway reroutes = %d, want 1", h.gwReroutes.Load())
	}
	if h.xtfyLeaked.Load() {
		t.Error("x-tfy-* header leaked to upstream")
	}
	if h.badAuth.Load() {
		t.Error("provider Authorization not preserved to upstream")
	}
}

func TestPassthroughNoToken(t *testing.T) {
	h := newHarness(t)
	res := h.do(t, "GET", "/telemetry")
	body := readClose(t, res)

	if body != "telemetry-ok" {
		t.Errorf("body = %q, want telemetry-ok", body)
	}
	if h.gwReroutes.Load() != 0 {
		t.Errorf("passthrough must not hit the gateway, got %d", h.gwReroutes.Load())
	}
	if h.xtfyLeaked.Load() {
		t.Error("passthrough request carried x-tfy-* headers")
	}
}

func TestStreamingSSEByteIdentical(t *testing.T) {
	h := newHarness(t)
	res := h.do(t, "POST", "/v1/sse")
	body := readClose(t, res)

	want := ""
	for i := 0; i < 5; i++ {
		want += fmt.Sprintf("data: chunk-%d\n\n", i)
	}
	if body != want {
		t.Errorf("SSE body mismatch:\n got %q\nwant %q", body, want)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if h.gwReroutes.Load() != 1 {
		t.Errorf("expected SSE request to be rerouted once, got %d", h.gwReroutes.Load())
	}
}

func TestPIDAttribution(t *testing.T) {
	h := newHarness(t)

	// No process info: host attribution picks the browser app.
	readClose(t, h.do(t, "POST", "/v1/messages"))
	if got := h.gotApp.Load(); got != "test-app" {
		t.Fatalf("host attribution: x-tfy-app = %v, want test-app", got)
	}

	// With process info matching the desktop profile, PID attribution wins.
	// Process attribution is resolved once per connection (at connection setup),
	// so force a fresh connection after changing the resolver — a real client's
	// owning process never changes mid-connection.
	h.procInfo.Store(&proxy.ProcInfo{Name: "TestDesktop"})
	h.client.CloseIdleConnections()
	readClose(t, h.do(t, "POST", "/v1/messages"))
	if got := h.gotApp.Load(); got != "desktop-app" {
		t.Fatalf("PID attribution: x-tfy-app = %v, want desktop-app", got)
	}
}

func TestNonAllowlistedHostSpliced(t *testing.T) {
	h := newHarness(t)

	req, _ := http.NewRequest("GET", h.otherURL+"/", nil)
	res, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("request to non-allowlisted host failed: %v", err)
	}
	body := readClose(t, res)
	if body != "other-ok" {
		t.Errorf("body = %q, want other-ok", body)
	}
	// Proof it was NOT decrypted: the client must see the real server cert, not
	// an aitori-minted leaf.
	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		t.Fatal("no TLS peer certificates")
	}
	if !res.TLS.PeerCertificates[0].Equal(h.otherX509) {
		t.Error("non-allowlisted host was MITM'd; client did not see the real cert")
	}
	if h.gwReroutes.Load() != 0 {
		t.Errorf("spliced host must not reach the gateway, got %d", h.gwReroutes.Load())
	}
}

func TestFailOpenWhenGatewayDown(t *testing.T) {
	h := newHarness(t)

	// Sanity: gateway works first.
	readClose(t, h.do(t, "POST", "/v1/messages"))
	if h.gwReroutes.Load() != 1 {
		t.Fatalf("precondition: expected 1 reroute, got %d", h.gwReroutes.Load())
	}

	// Kill the gateway. A rerouted request must still complete against the
	// upstream (fail-open), and no x-tfy-* may leak.
	h.stopGateway()

	res := h.do(t, "POST", "/v1/messages")
	body := readClose(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fail-open status = %d, body=%q", res.StatusCode, body)
	}
	if !strings.Contains(body, `"path":"/v1/messages"`) {
		t.Errorf("fail-open body unexpected: %q", body)
	}
	if h.gwReroutes.Load() != 1 {
		t.Errorf("gateway should not have served while down, reroutes=%d", h.gwReroutes.Load())
	}
	if h.xtfyLeaked.Load() {
		t.Error("x-tfy-* leaked to upstream during fail-open")
	}
	if h.badAuth.Load() {
		t.Error("provider auth lost during fail-open")
	}
}
