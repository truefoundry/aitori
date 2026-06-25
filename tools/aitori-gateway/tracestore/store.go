// Package tracestore persists OpenTelemetry spans into a SQLite database and
// serves a small web UI to browse and search them. It is used by the mock
// gateway to give a self-contained, dependency-light trace viewer for local
// testing and demos.
//
// SQLite access uses the pure-Go modernc.org/sqlite driver, so the binary still
// builds with CGO_ENABLED=0.
package tracestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Event is a span event (e.g. a captured request/response body).
type Event struct {
	Name     string            `json:"name"`
	TimeNano int64             `json:"time_unix_nano"`
	Attrs    map[string]string `json:"attributes,omitempty"`
}

// Span is a persisted span. The denormalized HTTP/app fields are extracted from
// the OTel attributes so the UI can filter on them cheaply.
type Span struct {
	SpanID       string
	TraceID      string
	ParentSpanID string
	Name         string
	Kind         string
	StartNano    int64
	EndNano      int64
	DurationMS   float64
	StatusCode   string
	StatusMsg    string
	App          string
	Category     string
	HTTPMethod   string
	HTTPURL      string
	HTTPStatus   int
	Attributes   map[string]string
	Events       []Event
}

// Store is a SQLite-backed span store. It also fans out inserted spans to live
// subscribers (the SSE stream in the UI).
type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	subs map[chan Span]struct{}
}

const schema = `
CREATE TABLE IF NOT EXISTS spans (
  span_id         TEXT PRIMARY KEY,
  trace_id        TEXT NOT NULL,
  parent_span_id  TEXT,
  name            TEXT NOT NULL,
  kind            TEXT,
  start_unix_nano INTEGER NOT NULL,
  end_unix_nano   INTEGER NOT NULL,
  duration_ms     REAL NOT NULL,
  status_code     TEXT,
  status_msg      TEXT,
  app             TEXT,
  category        TEXT,
  http_method     TEXT,
  http_url        TEXT,
  http_status     INTEGER,
  attributes_json TEXT,
  events_json     TEXT
);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id);
CREATE INDEX IF NOT EXISTS idx_spans_start ON spans(start_unix_nano);
CREATE INDEX IF NOT EXISTS idx_spans_app   ON spans(app);
`

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	// Serialize writes (SQLite is single-writer); a busy timeout avoids spurious
	// "database is locked" under brief contention.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db, subs: map[chan Span]struct{}{}}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Subscribe returns a channel that receives every span inserted after the call,
// and a cancel func to unsubscribe. Used by the UI's SSE stream.
func (s *Store) Subscribe() (<-chan Span, func()) {
	ch := make(chan Span, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

// publish fans a span out to subscribers, dropping it for any slow subscriber.
func (s *Store) publish(sp Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- sp:
		default:
		}
	}
}

// Insert upserts a span.
func (s *Store) Insert(ctx context.Context, sp Span) error {
	attrs, _ := json.Marshal(sp.Attributes)
	events, _ := json.Marshal(sp.Events)
	_, err := s.db.ExecContext(ctx, `
INSERT OR REPLACE INTO spans (
  span_id, trace_id, parent_span_id, name, kind,
  start_unix_nano, end_unix_nano, duration_ms, status_code, status_msg,
  app, category, http_method, http_url, http_status, attributes_json, events_json
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sp.SpanID, sp.TraceID, sp.ParentSpanID, sp.Name, sp.Kind,
		sp.StartNano, sp.EndNano, sp.DurationMS, sp.StatusCode, sp.StatusMsg,
		sp.App, sp.Category, sp.HTTPMethod, sp.HTTPURL, sp.HTTPStatus, string(attrs), string(events))
	if err != nil {
		return fmt.Errorf("insert span: %w", err)
	}
	s.publish(sp)
	return nil
}

// Filter selects spans for the list view.
type Filter struct {
	App      string // exact app match
	Category string // exact category match
	Text     string // substring match against name/url
	MinHTTP  int    // minimum http status (e.g. 400 to find errors)
	Limit    int    // default 200
}

// Query returns spans matching the filter, newest first.
func (s *Store) Query(ctx context.Context, f Filter) ([]Span, error) {
	var where []string
	var args []any
	if f.App != "" {
		where = append(where, "app = ?")
		args = append(args, f.App)
	}
	if f.Category != "" {
		where = append(where, "category = ?")
		args = append(args, f.Category)
	}
	if f.Text != "" {
		where = append(where, "(http_url LIKE ? OR name LIKE ?)")
		like := "%" + f.Text + "%"
		args = append(args, like, like)
	}
	if f.MinHTTP > 0 {
		where = append(where, "http_status >= ?")
		args = append(args, f.MinHTTP)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := "SELECT " + selectCols + " FROM spans"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY start_unix_nano DESC LIMIT ?"
	args = append(args, limit)
	return s.queryRows(ctx, q, args...)
}

// Trace returns every span in a trace, oldest first (call order).
func (s *Store) Trace(ctx context.Context, traceID string) ([]Span, error) {
	return s.queryRows(ctx,
		"SELECT "+selectCols+" FROM spans WHERE trace_id = ? ORDER BY start_unix_nano ASC", traceID)
}

// Get returns a single span by id (ok=false if not found).
func (s *Store) Get(ctx context.Context, spanID string) (Span, bool, error) {
	rows, err := s.queryRows(ctx, "SELECT "+selectCols+" FROM spans WHERE span_id = ?", spanID)
	if err != nil {
		return Span{}, false, err
	}
	if len(rows) == 0 {
		return Span{}, false, nil
	}
	return rows[0], true, nil
}

// Apps returns the distinct app names seen, for the filter dropdown.
func (s *Store) Apps(ctx context.Context) ([]string, error) { return s.distinct(ctx, "app") }

// Categories returns the distinct categories seen, for the filter dropdown.
func (s *Store) Categories(ctx context.Context) ([]string, error) { return s.distinct(ctx, "category") }

// distinct returns the distinct non-empty values of a column (col is a fixed
// internal identifier, never user input).
func (s *Store) distinct(ctx context.Context, col string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT "+col+" FROM spans WHERE "+col+" <> '' ORDER BY "+col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const selectCols = `span_id, trace_id, parent_span_id, name, kind,
  start_unix_nano, end_unix_nano, duration_ms, status_code, status_msg,
  app, category, http_method, http_url, http_status, attributes_json, events_json`

func (s *Store) queryRows(ctx context.Context, q string, args ...any) ([]Span, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query spans: %w", err)
	}
	defer rows.Close()

	var out []Span
	for rows.Next() {
		var sp Span
		var attrs, events string
		if err := rows.Scan(
			&sp.SpanID, &sp.TraceID, &sp.ParentSpanID, &sp.Name, &sp.Kind,
			&sp.StartNano, &sp.EndNano, &sp.DurationMS, &sp.StatusCode, &sp.StatusMsg,
			&sp.App, &sp.Category, &sp.HTTPMethod, &sp.HTTPURL, &sp.HTTPStatus, &attrs, &events,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(attrs), &sp.Attributes)
		_ = json.Unmarshal([]byte(events), &sp.Events)
		out = append(out, sp)
	}
	return out, rows.Err()
}
