package liveui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/capture"
)

func ex(host string) capture.Exchange {
	return capture.Exchange{Time: time.Unix(0, 0).UTC(), Method: "POST", Host: host, Path: "/v1/messages", Action: "reroute", Status: 200, Query: "secret=1"}
}

// /events returns the ring newest-first, and redaction drops the query string.
func TestSnapshotAndRedaction(t *testing.T) {
	s := New(true)
	s.Record(ex("a.com"))
	s.Record(ex("b.com"))

	req := httptest.NewRequest("GET", "/events", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var got []capture.Exchange
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Host != "b.com" {
		t.Errorf("newest-first expected b.com first, got %q", got[0].Host)
	}
	if got[0].Query != "" {
		t.Errorf("query should be redacted, got %q", got[0].Query)
	}
}

// The ring is bounded; old entries are evicted.
func TestRingEviction(t *testing.T) {
	s := New(false)
	for i := 0; i < ringCap+50; i++ {
		s.Record(ex("h"))
	}
	if n := len(s.snapshot()); n != ringCap {
		t.Fatalf("ring len = %d, want %d", n, ringCap)
	}
}

// Record must never block, even with no reader draining a full subscriber.
func TestRecordNeverBlocks(t *testing.T) {
	s := New(false)
	ch, ok := s.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer s.unsubscribe(ch)

	done := make(chan struct{})
	go func() {
		// Far more than subBuffer, with nobody reading ch: must not deadlock.
		for i := 0; i < subBuffer*4; i++ {
			s.Record(ex("h"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full subscriber buffer")
	}
}

// The SSE stream emits a data: frame per recorded exchange.
func TestStreamSSE(t *testing.T) {
	s := New(false)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Give the subscription a moment to register, then record.
	time.Sleep(50 * time.Millisecond)
	s.Record(ex("stream.com"))

	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, "stream.com") {
				t.Fatalf("SSE frame missing host: %q", line)
			}
			return
		}
	}
	t.Fatal("no data frame received")
}

// After Close, subscribe fails and Record is a no-op (no panic).
func TestCloseStopsServer(t *testing.T) {
	s := New(false)
	ch, _ := s.subscribe()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Error("subscriber channel should be closed after Close")
	}
	if _, ok := s.subscribe(); ok {
		t.Error("subscribe should fail after Close")
	}
	s.Record(ex("h")) // must not panic
}
