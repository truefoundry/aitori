package tracestore

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

var funcMap = template.FuncMap{
	"nanoTime": func(n int64) string {
		if n == 0 {
			return ""
		}
		return time.Unix(0, n).Format("2006-01-02 15:04:05.000")
	},
	"statusClass": func(code int) string {
		switch {
		case code >= 500:
			return "err"
		case code >= 400:
			return "warn"
		case code > 0:
			return "ok"
		default:
			return ""
		}
	},
	// prettyJSON indents a JSON object/array for display; non-JSON is returned
	// unchanged (e.g. SSE event streams, plain text, binary summaries).
	"prettyJSON": prettyJSON,
}

var tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html"))

type uiServer struct {
	store  *Store
	prefix string
}

// NewUI returns an http.Handler serving the trace UI under prefix (e.g. "/ui").
// Mount it on the gateway's mux at prefix + "/".
func NewUI(store *Store, prefix string) http.Handler {
	u := &uiServer{store: store, prefix: prefix}
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/trace/", u.handleTrace)
	mux.HandleFunc(prefix+"/span/", u.handleSpan)
	mux.HandleFunc(prefix+"/stream", u.handleStream)
	mux.HandleFunc(prefix+"/", u.handleList)
	return mux
}

type listView struct {
	Prefix     string
	Filter     Filter
	Apps       []string
	Categories []string
	Spans      []Span
	TraceID    string
	Live       bool // stream new spans in (only on the unfiltered main list)
}

type kv struct{ K, V string }

type detailView struct {
	Prefix string
	Span   Span
	Attrs  []kv
}

func (u *uiServer) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{App: q.Get("app"), Category: q.Get("category"), Text: q.Get("q")}
	if s := q.Get("status"); s != "" {
		f.MinHTTP, _ = strconv.Atoi(s)
	}
	spans, err := u.store.Query(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	apps, _ := u.store.Apps(r.Context())
	cats, _ := u.store.Categories(r.Context())
	live := f.App == "" && f.Category == "" && f.Text == "" && f.MinHTTP == 0
	u.render(w, "list", listView{
		Prefix: u.prefix, Filter: f, Apps: apps, Categories: cats, Spans: spans, Live: live,
	})
}

func (u *uiServer) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, u.prefix+"/trace/")
	spans, err := u.store.Trace(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.render(w, "list", listView{Prefix: u.prefix, Spans: spans, TraceID: id})
}

func (u *uiServer) handleSpan(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, u.prefix+"/span/")
	sp, ok, err := u.store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	keys := make([]string, 0, len(sp.Attributes))
	for k := range sp.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]kv, 0, len(keys))
	for _, k := range keys {
		attrs = append(attrs, kv{K: k, V: sp.Attributes[k]})
	}
	u.render(w, "detail", detailView{Prefix: u.prefix, Span: sp, Attrs: attrs})
}

// streamRow is the slim JSON pushed to the UI over SSE for each new span.
type streamRow struct {
	SpanID   string  `json:"spanID"`
	TraceID  string  `json:"traceID"`
	Time     string  `json:"time"`
	App      string  `json:"app"`
	Category string  `json:"category"`
	Method   string  `json:"method"`
	URL      string  `json:"url"`
	Status   int     `json:"status"`
	MS       float64 `json:"ms"`
}

// handleStream pushes newly-inserted spans to the browser as Server-Sent Events.
func (u *uiServer) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch, cancel := u.store.Subscribe()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case sp, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(streamRow{
				SpanID: sp.SpanID, TraceID: sp.TraceID,
				Time:     time.Unix(0, sp.StartNano).Format("15:04:05.000"),
				App:      sp.App,
				Category: sp.Category,
				Method:   sp.HTTPMethod,
				URL:      sp.HTTPURL,
				Status:   sp.HTTPStatus,
				MS:       sp.DurationMS,
			})
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

func (u *uiServer) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func prettyJSON(s string) string {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return s
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}
