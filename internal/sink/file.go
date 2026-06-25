package sink

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/truefoundry/aitori/internal/capture"
)

// File appends exchanges as JSON lines to a file.
type File struct {
	redact bool
	mu     sync.Mutex
	f      *os.File
	enc    *json.Encoder
}

// NewFile opens (creating/appending) a file sink at path.
func NewFile(path string, redact bool) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("file sink requires a path")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &File{redact: redact, f: f, enc: json.NewEncoder(f)}, nil
}

func (s *File) Record(e capture.Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(maybeRedact(e, s.redact))
}

func (s *File) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
