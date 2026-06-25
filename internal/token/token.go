// Package token reads the gateway token written by the external sign-in
// component and keeps it fresh in memory by watching the file.
//
// The token is a secret: it is never logged, never placed in a URL, and only
// ever attached to a rerouted request (see internal/router).
package token

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// State describes the loaded token's status, surfaced by `aitori status`.
type State string

const (
	StateOK      State = "ok"
	StateNoToken State = "no-token"
)

// Data is the parsed contents of the token file. The token file holds the bare
// gateway token (the secret attached as the x-tfy-api-key header on a reroute).
type Data struct {
	Token string // the bearer token (secret)
}

// Source provides the current token data. Implementations are safe for
// concurrent use.
type Source interface {
	// Get returns the current token, or "" if none is available.
	Get() string
	// Data returns the full parsed token data.
	Data() Data
	// State reports whether a token is currently available.
	State() State
}

// Static is an in-memory Source, primarily for tests.
type Static struct {
	mu sync.RWMutex
	d  Data
}

// NewStatic returns a Static source seeded with the given token.
func NewStatic(tok string) *Static { return &Static{d: Data{Token: tok}} }

// Set replaces the static source's data.
func (s *Static) Set(d Data) {
	s.mu.Lock()
	s.d = d
	s.mu.Unlock()
}

func (s *Static) Get() string { return s.Data().Token }

func (s *Static) Data() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d
}

func (s *Static) State() State {
	if s.Get() == "" {
		return StateNoToken
	}
	return StateOK
}

// FileSource reads the token from a file and refreshes it on change.
type FileSource struct {
	path string

	mu sync.RWMutex
	d  Data

	watcher *fsnotify.Watcher
	done    chan struct{}

	// onChange is invoked after each successful reload (used by tests).
	onChange func(Data)
}

var _ Source = (*FileSource)(nil)

// NewFileSource reads the token file at path once and starts watching it (and
// its parent directory, to catch atomic rename-based writes). A missing file is
// not an error: the source simply reports StateNoToken until the file appears.
func NewFileSource(path string) (*FileSource, error) {
	fs := &FileSource{
		path: filepath.Clean(path),
		done: make(chan struct{}),
	}
	fs.reload()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fs.watcher = w

	// Watch the parent directory so we observe create/rename/remove of the
	// token file even when it does not yet exist. Watching the file directly
	// would miss editors that write via a temp file + rename.
	dir := filepath.Dir(fs.path)
	if dir == "" {
		dir = "."
	}
	if err := w.Add(dir); err != nil {
		// Directory may not exist yet; degrade to a non-watching source rather
		// than failing startup.
		w.Close()
		fs.watcher = nil
		return fs, nil
	}

	go fs.watchLoop()
	return fs, nil
}

func (fs *FileSource) watchLoop() {
	for {
		select {
		case <-fs.done:
			return
		case ev, ok := <-fs.watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(ev.Name) == fs.path {
				fs.reload()
			}
		case _, ok := <-fs.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

func (fs *FileSource) reload() {
	raw, err := os.ReadFile(fs.path)
	if err != nil {
		fs.set(Data{})
		return
	}
	fs.set(parse(raw))
}

func (fs *FileSource) set(d Data) {
	fs.mu.Lock()
	fs.d = d
	cb := fs.onChange
	fs.mu.Unlock()
	if cb != nil {
		cb(d)
	}
}

func (fs *FileSource) Get() string { return fs.Data().Token }

func (fs *FileSource) Data() Data {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.d
}

func (fs *FileSource) State() State {
	if fs.Get() == "" {
		return StateNoToken
	}
	return StateOK
}

// Close stops watching the token file.
func (fs *FileSource) Close() error {
	select {
	case <-fs.done:
	default:
		close(fs.done)
	}
	if fs.watcher != nil {
		return fs.watcher.Close()
	}
	return nil
}

// parse reads the token file as a bare token (whitespace-trimmed).
func parse(raw []byte) Data {
	return Data{Token: string(bytes.TrimSpace(raw))}
}
