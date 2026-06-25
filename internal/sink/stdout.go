package sink

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/truefoundry/aitori/internal/capture"
)

// Stdout writes exchanges as JSON lines to stderr.
type Stdout struct {
	redact bool
	mu     sync.Mutex
	enc    *json.Encoder
}

// NewStdout returns a stdout (stderr) sink.
func NewStdout(redact bool) *Stdout {
	return &Stdout{redact: redact, enc: json.NewEncoder(os.Stderr)}
}

func (s *Stdout) Record(e capture.Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(maybeRedact(e, s.redact))
}

func (s *Stdout) Close() error { return nil }
