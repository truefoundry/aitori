package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/truefoundry/aitori/internal/appresolve"
	"github.com/truefoundry/aitori/internal/capture"
	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/hostmatch"
	"github.com/truefoundry/aitori/internal/pipeline"
)

// serve handles a single decrypted (mitm=true) or plain-HTTP (mitm=false)
// proxied request: classify, decide, forward (reroute or passthrough), and
// stream the response back verbatim.
func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, mitm bool) {
	scheme := "https"
	if !mitm {
		scheme = r.URL.Scheme
		if scheme == "" {
			scheme = "http"
		}
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	// Snapshot the hot-reloadable state once so this request is served entirely
	// on one config generation, even if a SIGHUP reload swaps it mid-flight.
	rl := p.live.Load()

	flow := appresolve.Flow{Host: hostmatch.Normalize(host)}
	// Process attribution is resolved once per connection (see connContext) and
	// read here from the request context — no per-request system-table scan.
	if info, ok := procFromContext(r.Context()); ok {
		flow.HasProcess = true
		flow.PID = info.PID
		flow.ProcessName = info.Name
		flow.ExePath = info.Exe
		flow.BundleID = info.BundleID
	}

	// Read the request body only when it can change the verdict. The body affects
	// classification solely via match_body rules or the heuristic fallback (run
	// when no host/path/method rule matched). When no rule matches on the body,
	// a body-free decision that an explicit rule resolved to PASSTHROUGH is final
	// — so the body streams straight to the upstream instead of being buffered
	// (lower latency, notably for large uploads on passthrough paths). Reroute
	// still buffers, both to classify and to replay the body on a fail-open retry.
	var (
		bodyBytes []byte
		full      bool // body buffered & replayable
		dec       pipeline.Decision
	)
	if rl.pipeline.BodyMatters() {
		bodyBytes, full = drainBody(r, rl.maxBody)
		dec = rl.pipeline.Decide(r, flow, bodyBytes)
	} else {
		dec = rl.pipeline.Decide(r, flow, nil)
		if !(dec.Matched && dec.Action == config.ActionPassthrough) {
			bodyBytes, full = drainBody(r, rl.maxBody)
			dec = rl.pipeline.Decide(r, flow, bodyBytes)
		}
		// else: body-independent passthrough — leave r.Body as a live stream.
	}

	oc := &outcome{}
	start := time.Now()
	defer p.record(start, r, scheme, host, dec, oc)

	contentLength := r.ContentLength
	if full {
		contentLength = int64(len(bodyBytes))
	}
	newBody := func() io.ReadCloser {
		if full {
			if len(bodyBytes) == 0 {
				return http.NoBody
			}
			return io.NopCloser(bytes.NewReader(bodyBytes))
		}
		if r.Body == nil {
			return http.NoBody
		}
		return r.Body // single-use stream
	}

	log := p.log.With(
		"host", host,
		"mitm", mitm,
		"app", dec.AppID,
		"category", dec.Category,
		"action", dec.Action,
		"rule", dec.Rule,
		// Process attribution inputs: present even when no app matched, so an
		// empty "app" can be diagnosed (e.g. a request owned by an Electron
		// helper whose bundle id didn't match any profile).
		"proc", flow.ProcessName,
		"bundle", flow.BundleID,
	)
	log.Info("request", "method", r.Method, "path", r.URL.Path)

	if dec.Action == config.ActionBlock {
		log.Info("blocked by policy")
		oc.status = http.StatusForbidden
		oc.errMsg = "aitori: blocked by policy"
		http.Error(w, oc.errMsg, http.StatusForbidden)
		return
	}

	if dec.Reroute() {
		gwReq := p.newOutgoing(r, scheme, host, newBody(), contentLength)
		if rl.router.Rewrite(gwReq, dec.AppID, dec.Category, dec.PID) {
			res, err := p.transport.RoundTrip(gwReq)
			if err == nil {
				rl.router.NoteGatewaySuccess()
				oc.rerouted = true
				log.Debug("rerouted via gateway")
				p.copyResponse(w, res, oc)
				return
			}
			rl.router.NoteGatewayFailure()
			oc.gatewayGap = true
			// Governance gap: the request will not be governed for this call.
			log.Warn("gateway error: governance gap", "err", err)
			if rl.router.FailClosed() {
				writeError(w, oc, "aitori: gateway unavailable (fail_closed)")
				return
			}
			if !full {
				// Body already (partially) consumed by the failed attempt; a
				// non-idempotent replay is unsafe (plan §13).
				writeError(w, oc, "aitori: gateway unavailable and request not replayable")
				return
			}
			// Fail-open: forward directly to the original upstream.
			p.forwardUpstream(w, p.newOutgoing(r, scheme, host, newBody(), contentLength), log, oc)
			return
		}
		// Rewrite declined (no gateway / no token / breaker open / loop): the call
		// is gateway-bound but won't be rerouted this time. We still MITM'd and
		// classified it — "gateway-mitm" — then fall through to the upstream.
		log.Debug("gateway-mitm: classified for the gateway but not rerouted (no gateway/token/breaker); inspected and passed through")
	}

	// Passthrough or fail-open: forward to the original upstream, no token.
	p.forwardUpstream(w, p.newOutgoing(r, scheme, host, newBody(), contentLength), log, oc)
}

// outcome records what happened to a request, for observability.
type outcome struct {
	status     int
	bytes      int64
	rerouted   bool
	gatewayGap bool
	errMsg     string
}

func writeError(w http.ResponseWriter, oc *outcome, msg string) {
	oc.status = http.StatusBadGateway
	oc.errMsg = msg
	http.Error(w, msg, http.StatusBadGateway)
}

func (p *Proxy) record(start time.Time, r *http.Request, scheme, host string, dec pipeline.Decision, oc *outcome) {
	if p.recorder == nil {
		return
	}
	p.recorder.Record(capture.Exchange{
		Time:       start,
		App:        dec.AppID,
		Category:   dec.Category,
		Action:     dec.Action,
		Rerouted:   oc.rerouted,
		Method:     r.Method,
		Scheme:     scheme,
		Host:       hostmatch.Normalize(host),
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Status:     oc.status,
		Bytes:      oc.bytes,
		DurationMS: time.Since(start).Milliseconds(),
		GatewayGap: oc.gatewayGap,
		Error:      oc.errMsg,
	})
}

func (p *Proxy) forwardUpstream(w http.ResponseWriter, req *http.Request, log *slog.Logger, oc *outcome) bool {
	res, err := p.transport.RoundTrip(req)
	if err != nil {
		log.Warn("upstream error", "err", err)
		writeError(w, oc, "aitori: upstream error")
		return false
	}
	p.copyResponse(w, res, oc)
	return true
}

// newOutgoing clones r into a client request targeting scheme://host, with the
// provided body. Hop-by-hop headers are stripped.
func (p *Proxy) newOutgoing(r *http.Request, scheme, host string, body io.ReadCloser, contentLength int64) *http.Request {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = scheme
	out.URL.Host = host
	out.Host = host
	out.Body = body
	out.ContentLength = contentLength
	if contentLength == 0 {
		out.TransferEncoding = nil
	}
	removeHopByHop(out.Header)
	return out
}

// copyResponse streams res back to w verbatim: status, headers (incl.
// Set-Cookie), body (flushed promptly for SSE), and announced trailers.
func (p *Proxy) copyResponse(w http.ResponseWriter, res *http.Response, oc *outcome) {
	defer res.Body.Close()

	dst := w.Header()
	removeHopByHop(res.Header)
	copyHeader(dst, res.Header)

	// Announce trailers so the client expects them, then send values after body.
	trailerKeys := make([]string, 0, len(res.Trailer))
	for k := range res.Trailer {
		trailerKeys = append(trailerKeys, k)
	}
	if len(trailerKeys) > 0 {
		dst.Set("Trailer", strings.Join(trailerKeys, ", "))
	}

	w.WriteHeader(res.StatusCode)
	oc.status = res.StatusCode

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if nw, werr := w.Write(buf[:n]); werr != nil {
				oc.bytes += int64(nw)
				return
			} else {
				oc.bytes += int64(nw)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}

	// Trailer values are populated only after the body is fully read.
	for k, vals := range res.Trailer {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// drainBody reads up to limit bytes of r.Body. If the body fits, it returns the
// bytes with full=true and leaves r.Body replayable. If it exceeds the limit,
// it returns full=false and reconstructs r.Body as a single-use stream
// (buffered prefix + remainder). limit <= 0 disables buffering entirely.
func drainBody(r *http.Request, limit int64) (buf []byte, full bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	if limit <= 0 {
		return nil, false // stream through, no classification body, single attempt
	}

	prefix, _ := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if int64(len(prefix)) <= limit {
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(prefix))
		return prefix, true
	}
	// Exceeded: stitch the prefix back in front of the unread remainder.
	r.Body = &joinedBody{Reader: io.MultiReader(bytes.NewReader(prefix), r.Body), c: r.Body}
	return nil, false
}

type joinedBody struct {
	io.Reader
	c io.Closer
}

func (b *joinedBody) Close() error { return b.c.Close() }

// hopByHop are connection-scoped headers that must not be forwarded (RFC 7230).
var hopByHop = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func removeHopByHop(h http.Header) {
	// Drop any header named in the Connection header first.
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
