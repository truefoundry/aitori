// Package liveui is an optional, embedded live-traffic view for the aitori
// agent. It implements sink.Sink (Record/Close): every proxied exchange is kept
// in a bounded in-memory ring and broadcast to connected browsers over
// Server-Sent Events, and a single self-contained HTML page renders the feed.
//
// It is a debug/demo aid, NOT the governance path, and is off by default. Like
// the other sinks it only ever sees capture.Exchange, which is secret-free; with
// redaction on (the default) the query string is dropped too. Record never
// blocks the request path — a slow or stuck browser is dropped, not waited on.
package liveui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	_ "embed"

	"github.com/truefoundry/aitori/internal/capture"
)

//go:embed index.html
var indexHTML []byte

// ringCap bounds how many recent exchanges are retained for new page loads.
const ringCap = 500

// subBuffer is the per-subscriber buffer; once full, events are dropped for that
// subscriber rather than blocking Record.
const subBuffer = 256

// Server is the live-traffic sink + HTTP handler.
type Server struct {
	redact bool

	mu     sync.Mutex
	ring   []capture.Exchange // oldest first, len <= ringCap
	subs   map[chan capture.Exchange]struct{}
	closed bool
}

// New returns a live-traffic server. When redact is true (recommended) the
// query string is stripped from every record, matching the file/stdout sinks.
func New(redact bool) *Server {
	return &Server{
		redact: redact,
		ring:   make([]capture.Exchange, 0, ringCap),
		subs:   make(map[chan capture.Exchange]struct{}),
	}
}

// Record stores the exchange and fans it out to live subscribers. It is
// non-blocking: a subscriber whose buffer is full misses the event.
func (s *Server) Record(e capture.Exchange) {
	if s.redact {
		e = e.Redacted()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.ring = append(s.ring, e)
	if len(s.ring) > ringCap {
		// Re-slice off the oldest; append above reallocates periodically so the
		// backing array stays bounded.
		s.ring = s.ring[len(s.ring)-ringCap:]
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop, never block the request path
		}
	}
}

// Close stops the server: subscriber channels are closed (ending their SSE
// streams) and further Records are ignored.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for ch := range s.subs {
		close(ch)
		delete(s.subs, ch)
	}
	return nil
}

// Handler returns the HTTP handler: the page at "/", a JSON snapshot at
// "/events", and the SSE feed at "/stream".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// handleEvents returns the current ring as a JSON array, newest first.
func (s *Server) handleEvents(w http.ResponseWriter, _ *http.Request) {
	snap := s.snapshot()
	// Reverse to newest-first so the page can prepend in the same order as SSE.
	for i, j := 0, len(snap)-1; i < j; i, j = i+1, j-1 {
		snap[i], snap[j] = snap[j], snap[i]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, ok := s.subscribe()
	if !ok {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return // server closed
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) snapshot() []capture.Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capture.Exchange, len(s.ring))
	copy(out, s.ring)
	return out
}

func (s *Server) subscribe() (chan capture.Exchange, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	ch := make(chan capture.Exchange, subBuffer)
	s.subs[ch] = struct{}{}
	return ch, true
}

func (s *Server) unsubscribe(ch chan capture.Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}
