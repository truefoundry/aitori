// Package proxy implements aitori's selective-MITM HTTP(S) proxy.
//
// In explicit-proxy mode (Tier 1) the client issues CONNECT for HTTPS. The
// proxy decrypts ONLY allowlisted hosts (intercept_hosts, minus the gateway);
// every other host is spliced through as raw bytes and never decrypted
// (plan §3, §9). Decrypted requests run through the pipeline and are either
// rerouted to the gateway or passed through to the original upstream, with the
// response streamed back verbatim.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truefoundry/aitori/internal/ca"
	"github.com/truefoundry/aitori/internal/capture"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/hostmatch"
	"github.com/truefoundry/aitori/internal/pipeline"
	"github.com/truefoundry/aitori/internal/router"
)

// ProcInfo describes the local process that originated a connection.
type ProcInfo struct {
	PID      int
	Name     string
	Exe      string
	BundleID string
}

// ProcResolver maps a connection (local = agent side, remote = client side) to
// the originating process, for desktop-app attribution. ok=false falls back to
// host-based attribution.
type ProcResolver func(local, remote netip.AddrPort) (ProcInfo, bool)

// Recorder consumes per-exchange records for optional local observability.
type Recorder interface{ Record(capture.Exchange) }

// Options configures a Proxy.
type Options struct {
	Config    *config.Config
	CA        *ca.CA
	Router    *router.Router
	Pipeline  *pipeline.Pipeline
	Transport http.RoundTripper // agent egress; built from Config when nil
	Logger    *slog.Logger
	Proc      ProcResolver // optional desktop-app attribution
	Recorder  Recorder     // optional local observability sink
	// DialContext, if set, is used for the raw CONNECT splice of
	// non-allowlisted hosts (defaults to a net.Dialer). It is also the hook
	// for transparent-mode self-exclusion (M5).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// reloadable bundles the config-derived state that SIGHUP hot-reload swaps
// atomically. In-flight requests finish on the set they started with; new
// requests pick up the new set. The listener, CA/MITM config, transport, and
// OS state (system proxy, injects) are untouched by a reload.
type reloadable struct {
	cfg            *config.Config
	router         *router.Router
	pipeline       *pipeline.Pipeline
	interceptHosts []string
	gatewayHost    string
	maxBody        int64
}

// Proxy is the selective-MITM proxy server.
type Proxy struct {
	ca          *ca.CA
	transport   http.RoundTripper
	log         *slog.Logger
	proc        ProcResolver
	recorder    Recorder
	mitmTLS     *tls.Config
	dialTimeout time.Duration
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	coverage    *coverage

	// live holds the hot-reloadable, config-derived state, swapped atomically by
	// Reload and read (lock-free) on every request.
	live atomic.Pointer[reloadable]

	mu     sync.Mutex
	server *http.Server
	tln    net.Listener // transparent listener (Tier 2)
}

// New builds a Proxy from Options.
func New(o Options) *Proxy {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dialTimeout := 5 * time.Second
	if o.Config != nil {
		dialTimeout = o.Config.DialTimeout()
	}
	transport := o.Transport
	if transport == nil {
		transport = defaultTransport(dialTimeout, o.Config != nil && o.Config.Proxy.ForceHTTP11)
	}

	p := &Proxy{
		ca:          o.CA,
		transport:   transport,
		log:         logger,
		proc:        o.Proc,
		recorder:    o.Recorder,
		dialTimeout: dialTimeout,
		dialContext: o.DialContext,
		coverage:    newCoverage(),
	}
	if p.dialContext == nil {
		p.dialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	p.live.Store(newReloadable(o.Config, o.Router, o.Pipeline))
	if o.CA != nil {
		forceHTTP11 := o.Config != nil && o.Config.Proxy.ForceHTTP11
		p.mitmTLS = o.CA.TLSConfig(forceHTTP11)
	}
	return p
}

// newReloadable derives the hot-swappable state from a config + its freshly
// built router and pipeline.
func newReloadable(cfg *config.Config, rtr *router.Router, pl *pipeline.Pipeline) *reloadable {
	rl := &reloadable{cfg: cfg, router: rtr, pipeline: pl}
	if cfg != nil {
		rl.interceptHosts = cfg.InterceptHosts.Patterns()
		rl.gatewayHost = hostmatch.Normalize(cfg.GatewayHost())
		if cfg.Proxy.MaxBodyKB > 0 {
			rl.maxBody = int64(cfg.Proxy.MaxBodyKB) * 1024
		}
	}
	return rl
}

// ReloadOptions carries freshly-built, config-derived components for a SIGHUP
// hot-reload.
type ReloadOptions struct {
	Config   *config.Config
	Router   *router.Router
	Pipeline *pipeline.Pipeline
}

// Reload atomically swaps the config-derived state (router, pipeline, intercept
// set, body cap). The listener and OS state are untouched. Safe to call
// concurrently with Serve.
func (p *Proxy) Reload(o ReloadOptions) {
	p.live.Store(newReloadable(o.Config, o.Router, o.Pipeline))
}

// ListenAndServe binds the configured listen address and serves until Shutdown.
func (p *Proxy) ListenAndServe() error {
	addr := "127.0.0.1:8080"
	if cfg := p.live.Load().cfg; cfg != nil && cfg.Proxy.Listen != "" {
		addr = cfg.Proxy.Listen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return p.Serve(ln)
}

// Serve serves the proxy on ln until Shutdown/Close.
func (p *Proxy) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 30 * time.Second,
		// No ReadTimeout/WriteTimeout: streamed bodies are long-lived.
		IdleTimeout: 90 * time.Second,
		ErrorLog:    log.New(io.Discard, "", 0),
		ConnContext: p.connContext,
	}
	p.mu.Lock()
	p.server = srv
	p.mu.Unlock()
	return srv.Serve(ln)
}

// Shutdown gracefully stops accepting new connections and closes the listener.
// Hijacked tunnels close when their clients disconnect (fail-open).
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	srv, tln := p.server, p.tln
	p.mu.Unlock()
	if tln != nil {
		tln.Close()
	}
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// Close immediately stops the proxy.
func (p *Proxy) Close() error {
	p.mu.Lock()
	srv, tln := p.server, p.tln
	p.mu.Unlock()
	if tln != nil {
		tln.Close()
	}
	if srv == nil {
		return nil
	}
	return srv.Close()
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Plain HTTP proxy request (absolute-form). No decryption needed.
	p.serve(w, r, false)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	hostport := r.Host
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "aitori: hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}

	// Log the connection lifecycle (open -> close) and how long it stayed
	// active. MITM'd (intercepted) connections log at Info; raw splices of
	// non-allowlisted hosts are high-volume under a system-wide proxy, so they
	// log at Debug (-v) to avoid drowning the per-request app logs.
	mitm := p.shouldMITM(hostport)
	level := slog.LevelInfo
	if !mitm {
		level = slog.LevelDebug
	}
	client := conn.RemoteAddr().String()
	start := time.Now()
	p.log.Log(context.Background(), level, "connection open", "host", hostport, "client", client, "mitm", mitm)
	defer func() {
		p.log.Log(context.Background(), level, "connection closed",
			"host", hostport, "client", client, "mitm", mitm, "duration", time.Since(start).String())
	}()

	if !mitm {
		// Non-allowlisted (or the gateway): raw splice, never decrypted.
		p.splice(conn, hostport)
		return
	}

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		conn.Close()
		return
	}
	tlsConn := tls.Server(conn, p.mitmTLS)
	if err := tlsConn.Handshake(); err != nil {
		// Handshake failed: either the client doesn't trust our CA (fixable) or
		// it pins certs (uncoverable via MITM). Fail-open by closing; the app
		// sees a connection error. noteHandshakeFailure distinguishes the two.
		p.noteHandshakeFailure(hostport, err)
		tlsConn.Close()
		return
	}
	p.serveMITM(tlsConn, hostport)
}

// untrustedCA reports whether a handshake error is the client's "unknown
// certificate authority" TLS alert — i.e. the client does not trust the aitori
// CA. This is a trust-install problem, NOT cert pinning.
func untrustedCA(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unknown certificate authority") || strings.Contains(s, "unknown ca")
}

// noteHandshakeFailure classifies a failed MITM handshake on an allowlisted host
// and emits an actionable warning. A CA-trust failure is fixable (install/trust
// the CA in the client) and the host stays coverable, so it is NOT recorded as
// pinned; a genuine pinning failure is recorded and recommends Tier-0.
func (p *Proxy) noteHandshakeFailure(hostport string, err error) {
	host := hostmatch.Normalize(hostport)
	app := p.appForHost(host)

	if untrustedCA(err) {
		// Not pinning: the client rejected our leaf because it doesn't trust the
		// aitori CA. Browsers like Firefox keep their own trust store, separate
		// from the OS keychain that `aitori up` installs into.
		p.log.Warn("client does not trust the aitori CA (not cert pinning); install/trust the CA in this client — browsers such as Firefox use their own trust store, and an already-open app may need a restart",
			"host", host, "app", app, "err", err)
		return
	}

	p.coverage.recordPinning(host)
	if app != "" {
		p.log.Warn("TLS handshake failed (likely cert pinning); host is uncoverable via MITM",
			"host", host, "app", app, "err", err)
	} else {
		p.log.Debug("TLS handshake failed (likely cert pinning)", "host", host, "err", err)
	}
}

// appForHost returns the first configured app that tags host (via match.hosts),
// used to make the cert-pinning / CA-trust warning actionable.
func (p *Proxy) appForHost(host string) (appID string) {
	cfg := p.live.Load().cfg
	if cfg == nil {
		return ""
	}
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		if len(app.Match.Hosts) > 0 && hostmatch.MatchAny(app.Match.Hosts, host) {
			return app.ID
		}
	}
	return ""
}

// CoverageSnapshot returns the per-host MITM coverage observed so far.
func (p *Proxy) CoverageSnapshot() []CoverageStatus { return p.coverage.snapshot() }

// shouldMITM reports whether a CONNECT host must be decrypted: it must be in
// intercept_hosts and must not be the gateway (loop prevention, plan §13).
func (p *Proxy) shouldMITM(hostport string) bool {
	host := hostmatch.Normalize(hostport)
	if host == "" {
		return false
	}
	rl := p.live.Load()
	if rl.gatewayHost != "" && host == rl.gatewayHost {
		return false
	}
	return hostmatch.MatchAny(rl.interceptHosts, host)
}

// splice tunnels raw bytes between the client and the upstream without ever
// decrypting them.
func (p *Proxy) splice(client net.Conn, hostport string) {
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout)
	defer cancel()
	upstream, err := p.dialContext(ctx, "tcp", withDefaultPort(hostport, "443"))
	if err != nil {
		// Best-effort failure response, then drop.
		io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	pipe(client, upstream)
	// Closing both conns (deferred) unblocks the other copy.
}

// pipe copies bytes bidirectionally between a and b until either side closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// ServeTransparent serves Tier-2 transparent capture on ln: connections are
// redirected to the proxy by the OS (e.g. nftables REDIRECT). There is no
// CONNECT; the proxy peeks the TLS SNI to decide MITM-vs-splice (plan §9). For
// spliced (non-allowlisted) hosts it forwards to the original destination,
// recovered via SO_ORIGINAL_DST.
func (p *Proxy) ServeTransparent(ln net.Listener) error {
	p.mu.Lock()
	p.tln = ln
	p.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go p.handleTransparent(conn)
	}
}

func (p *Proxy) handleTransparent(conn net.Conn) {
	// Recover the original destination before any reads consume the socket.
	dst, dstErr := originalDst(conn)

	sni, replay, err := peekClientHello(conn)
	if err == nil && sni != "" && p.shouldMITM(sni) {
		host := hostmatch.Normalize(sni)
		tlsConn := tls.Server(replay, p.mitmTLS)
		if herr := tlsConn.Handshake(); herr != nil {
			p.noteHandshakeFailure(sni, herr)
			tlsConn.Close()
			return
		}
		p.serveMITM(tlsConn, net.JoinHostPort(host, "443"))
		return
	}

	// Splice raw to the original destination, never decrypted.
	if dstErr != nil {
		conn.Close()
		return
	}
	p.spliceTo(replay, dst)
}

func (p *Proxy) spliceTo(client net.Conn, dst string) {
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout)
	defer cancel()
	upstream, err := p.dialContext(ctx, "tcp", dst)
	if err != nil {
		return
	}
	defer upstream.Close()
	pipe(client, upstream)
}

func (p *Proxy) serveMITM(tlsConn net.Conn, hostport string) {
	ln := &oneShotListener{
		ch:   make(chan net.Conn, 1),
		done: make(chan struct{}),
		addr: tlsConn.LocalAddr(),
	}
	// When the server finishes with the connection it closes it; that closes
	// the one-shot listener so http.Serve returns instead of leaking a
	// goroutine blocked on Accept.
	nc := &notifyConn{Conn: tlsConn, onClose: ln.Close}
	ln.ch <- nc

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.serve(w, r, true)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnContext:       p.connContext,
	}
	_ = srv.Serve(ln)
}

// procCtxKey keys the per-connection resolved process info on a connection's
// base context (set via http.Server.ConnContext). Resolving once per
// connection — rather than per request — avoids re-scanning the whole system
// TCP table on every decrypted request, which dominated latency on busy MITM'd
// hosts. The client's 4-tuple is constant for a keep-alive connection, so all
// its requests share one lookup.
type procCtxKey struct{}

type procResult struct {
	info ProcInfo
	ok   bool
}

// connContext resolves the owning process once for an accepted connection and
// stashes it on the connection's context. http.Server invokes this on the
// per-connection goroutine before the first request, so the (potentially slow)
// lookup is paid once per connection, not once per request.
func (p *Proxy) connContext(ctx context.Context, c net.Conn) context.Context {
	if p.proc == nil {
		return ctx
	}
	local, lok := addrPort(c.LocalAddr())
	remote, rok := addrPort(c.RemoteAddr())
	if !lok || !rok {
		return ctx
	}
	info, ok := p.proc(local, remote)
	return context.WithValue(ctx, procCtxKey{}, procResult{info: info, ok: ok})
}

// procFromContext returns the per-connection resolved process info, if any.
func procFromContext(ctx context.Context) (ProcInfo, bool) {
	pr, ok := ctx.Value(procCtxKey{}).(procResult)
	if !ok {
		return ProcInfo{}, false
	}
	return pr.info, pr.ok
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	if a == nil {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

func defaultTransport(dialTimeout time.Duration, forceHTTP11 bool) *http.Transport {
	t := &http.Transport{
		// Never proxy the agent's own egress: this is the primary loop guard
		// for the agent->gateway and agent->upstream legs (plan §9).
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     !forceHTTP11,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if forceHTTP11 {
		// Disable HTTP/2 on the upstream/gateway leg as well, keeping the whole
		// path on HTTP/1.1 to avoid h2 streaming edge cases.
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return t
}

func withDefaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, port)
}

// oneShotListener delivers a single pre-accepted connection to http.Serve.
type oneShotListener struct {
	ch   chan net.Conn
	done chan struct{}
	addr net.Addr
	once sync.Once
}

func (l *oneShotListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneShotListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *oneShotListener) Addr() net.Addr { return l.addr }

// notifyConn invokes onClose exactly once when the connection is closed.
type notifyConn struct {
	net.Conn
	once    sync.Once
	onClose func() error
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { _ = c.onClose() })
	return c.Conn.Close()
}
