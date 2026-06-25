package sink

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/truefoundry/aitori/internal/capture"
	"github.com/truefoundry/aitori/internal/config"
)

func TestFileSinkWritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exchanges.jsonl")
	s, err := NewFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(capture.Exchange{Time: time.Now(), App: "claude-web", Action: "reroute", Host: "claude.ai", Status: 200, Query: "a=b"})
	s.Record(capture.Exchange{Time: time.Now(), App: "x", Action: "passthrough", Host: "example.com", Status: 204})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	f, _ := os.Open(path)
	defer f.Close()
	var n int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e capture.Exchange
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d not valid JSON: %v", n, err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("got %d records, want 2", n)
	}
}

func TestFileSinkRedactsQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ex.jsonl")
	s, _ := NewFile(path, true)
	s.Record(capture.Exchange{Host: "claude.ai", Query: "secret=123"})
	s.Close()

	data, _ := os.ReadFile(path)
	var e capture.Exchange
	json.Unmarshal(data, &e)
	if e.Query != "" {
		t.Errorf("query not redacted: %q", e.Query)
	}
}

func TestBuildNoSinks(t *testing.T) {
	s, err := Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Error("expected nil sink when none configured")
	}
}

func TestBuildStdout(t *testing.T) {
	s, err := Build([]config.SinkConfig{{Type: "stdout"}}) // redact defaults on
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("expected a sink")
	}
	s.Close()
}
