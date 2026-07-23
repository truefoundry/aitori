package genai

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"strings"
)

// maxLineBuffer caps a single SSE line so a pathological unterminated line can't
// grow without bound. Beyond it the current line is dropped, not buffered.
const maxLineBuffer = 1 * 1024 * 1024 // 1 MiB per SSE line

// DefaultCaptureLimit is the default bound (bytes) on how much of a response is
// held in memory to extract token usage. Streaming (SSE) responses are scanned
// line by line and never fully held; this bounds only the non-streaming JSON
// buffer, which is truncated (and then not parsed) once the limit is reached.
const DefaultCaptureLimit = 256 * 1024 // 256 KiB

// maxDecodedScan bounds how many DECODED bytes are scanned from a compressed
// body, so a decompression bomb (tiny gzip -> huge output) can't burn unbounded
// CPU. It is generous enough to reach the usage event at the end of a long SSE
// stream while still capping worst-case work.
const maxDecodedScan = 8 * 1024 * 1024 // 8 MiB

// ResponseAccumulator incrementally consumes a response body as it streams to
// the client and extracts GenAI metadata (model, token usage, finish reasons,
// and — when enabled — completion text). It is fed via Write from the proxy's
// verbatim copy loop, so it never buffers or delays the streamed response; for
// SSE it only assembles one line at a time.
//
// The proxy forwards the response to the client verbatim (still compressed),
// but the accumulator must parse text, so when the body is gzip/deflate-encoded
// it is decoded here via a streaming pipe before being scanned. (The proxy
// normalizes traced requests to Accept-Encoding: gzip so the upstream does not
// pick br/zstd, which the standard library cannot decode.)
type ResponseAccumulator struct {
	provider       string
	captureContent bool
	sse            bool
	limit          int    // max bytes buffered for a non-streaming JSON body
	encoding       string // response Content-Encoding, lower-cased ("" = identity)

	// Streaming decompression pipeline (started lazily on first Write when the
	// body is compressed). Write pushes raw bytes into pw; a goroutine reads the
	// decoded stream and calls ingest. Result closes pw and waits on done.
	pw   *io.PipeWriter
	done chan struct{}

	line      bytes.Buffer // partial SSE line
	json      bytes.Buffer // non-SSE JSON body, buffered up to limit then truncated
	jsonOver  bool         // limit exceeded: buffering stopped, body not parsed
	content   strings.Builder
	sawSSEEnd bool

	raw     bytes.Buffer // raw body prefix for tfy.output (content capture only)
	rawOver bool         // raw buffer hit the limit; a truncated prefix is kept

	info ResponseInfo
}

// NewResponseAccumulator builds an accumulator for a response with the given
// provider, Content-Type, and Content-Encoding. SSE is detected from the content
// type. limit bounds the non-streaming JSON buffer (bytes); <=0 uses
// DefaultCaptureLimit. A gzip/deflate contentEncoding is transparently decoded;
// any other non-empty encoding (e.g. br/zstd) is treated as un-inspectable and
// the body is skipped rather than captured as binary.
func NewResponseAccumulator(provider, contentType, contentEncoding string, captureContent bool, limit int) *ResponseAccumulator {
	if limit <= 0 {
		limit = DefaultCaptureLimit
	}
	return &ResponseAccumulator{
		provider:       provider,
		captureContent: captureContent,
		limit:          limit,
		encoding:       strings.ToLower(strings.TrimSpace(contentEncoding)),
		sse:            strings.Contains(strings.ToLower(contentType), "text/event-stream"),
	}
}

// decodable reports whether the response encoding can be decoded with the
// standard library. Identity (no encoding) is trivially "decodable".
func (a *ResponseAccumulator) decodable() bool {
	switch a.encoding {
	case "", "gzip", "x-gzip", "deflate":
		return true
	default:
		return false
	}
}

// Write feeds streamed response bytes. It never returns an error and never
// blocks the response path: parsing failures are ignored. Compressed bodies are
// routed through the decompression pipeline; identity bodies are ingested
// directly.
func (a *ResponseAccumulator) Write(p []byte) {
	if a == nil || len(p) == 0 {
		return
	}
	if !a.decodable() {
		// Unknown compression (br/zstd): we can't turn this into text, so don't
		// capture it as binary — skip entirely (metadata like status/latency
		// still comes from the exchange, not the body).
		return
	}
	if a.encoding == "" {
		a.ingest(p)
		return
	}
	// Compressed: push into the decompression pipe; a goroutine ingests decoded.
	if a.pw == nil {
		a.startDecompressor()
	}
	_, _ = a.pw.Write(p) // pipe write only errors after Close/CloseWithError
}

// startDecompressor spins up the goroutine that decodes the compressed body and
// feeds the decoded bytes to ingest, bounded by maxDecodedScan.
func (a *ResponseAccumulator) startDecompressor() {
	pr, pw := io.Pipe()
	a.pw = pw
	a.done = make(chan struct{})
	go func() {
		defer close(a.done)
		// Always drain the pipe so Write never blocks, even after we stop.
		defer io.Copy(io.Discard, pr) //nolint:errcheck

		r, err := newDecoder(a.encoding, pr)
		if err != nil {
			return
		}
		if c, ok := r.(io.Closer); ok {
			defer c.Close()
		}
		buf := make([]byte, 32*1024)
		var scanned int
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				if scanned+n > maxDecodedScan {
					n = maxDecodedScan - scanned
				}
				if n > 0 {
					a.ingest(buf[:n])
					scanned += n
				}
				if scanned >= maxDecodedScan {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
}

// newDecoder returns a reader that decodes the given Content-Encoding. "deflate"
// is ambiguous on the wire (zlib-wrapped vs raw); zlib is tried first (what most
// servers actually send) with a raw-flate fallback.
func newDecoder(encoding string, r io.Reader) (io.Reader, error) {
	switch encoding {
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "deflate":
		br := bufio.NewReader(r)
		if zr, err := zlib.NewReader(br); err == nil {
			return zr, nil
		}
		return flate.NewReader(br), nil
	default:
		return r, nil
	}
}

// ingest folds a chunk of DECODED (or identity) response bytes into the buffers:
// the bounded raw body (for tfy.output), plus the SSE line assembler or the
// non-streaming JSON buffer.
func (a *ResponseAccumulator) ingest(p []byte) {
	// Retain the raw body (for tfy.output) up to the limit, then keep the
	// truncated prefix. Only when content capture is on. Works for both SSE and
	// JSON so streamed responses still surface an (bounded) output payload.
	if a.captureContent && !a.rawOver {
		if a.raw.Len()+len(p) > a.limit {
			if n := a.limit - a.raw.Len(); n > 0 {
				a.raw.Write(p[:n])
			}
			a.rawOver = true
		} else {
			a.raw.Write(p)
		}
	}
	if a.sse {
		a.feedSSE(p)
		return
	}
	if a.jsonOver {
		return
	}
	// Bound the buffer: once the limit is reached, truncate — stop buffering and
	// mark the body as over-limit so it is never parsed (and never fully held).
	if a.json.Len()+len(p) > a.limit {
		a.jsonOver = true
		a.json.Reset() // release the partial buffer; it can't be parsed anyway
		return
	}
	a.json.Write(p)
}

func (a *ResponseAccumulator) feedSSE(p []byte) {
	for _, b := range p {
		if b == '\n' {
			a.consumeLine(a.line.Bytes())
			a.line.Reset()
			continue
		}
		if a.line.Len() < maxLineBuffer {
			a.line.WriteByte(b)
		}
	}
}

func (a *ResponseAccumulator) consumeLine(raw []byte) {
	line := bytes.TrimRight(raw, "\r")
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		a.sawSSEEnd = true
		return
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	a.mergeEvent(ev)
}

// Result finalizes parsing (for non-SSE bodies) and returns the extracted info.
// If a decompression pipeline is running it is closed and drained first, so all
// decoded bytes have been ingested before the buffers are read.
func (a *ResponseAccumulator) Result() ResponseInfo {
	if a == nil {
		return ResponseInfo{}
	}
	if a.pw != nil {
		_ = a.pw.Close() // signal EOF to the decoder
		<-a.done         // wait until the goroutine has ingested everything
		a.pw = nil
	}
	if !a.sse && !a.jsonOver && a.json.Len() > 0 {
		var m map[string]any
		if err := json.Unmarshal(a.json.Bytes(), &m); err == nil {
			a.mergeEvent(m)
		}
	}
	if a.captureContent {
		a.info.Content = clamp(a.content.String(), maxContentField)
		a.info.RawBody = a.raw.String()
	}
	return a.info
}

// mergeEvent folds one parsed JSON object (a full response, or one SSE event)
// into the accumulated info. It handles both OpenAI and Anthropic shapes by
// probing for their respective keys.
func (a *ResponseAccumulator) mergeEvent(m map[string]any) {
	if s := toString(m["id"]); s != "" && a.info.ResponseID == "" {
		a.info.ResponseID = s
	}
	if s := toString(m["model"]); s != "" {
		a.info.Model = s
	}

	// Anthropic: message_start / message_delta wrap a nested "message".
	if msg, ok := m["message"].(map[string]any); ok {
		if s := toString(msg["model"]); s != "" {
			a.info.Model = s
		}
		if s := toString(msg["id"]); s != "" && a.info.ResponseID == "" {
			a.info.ResponseID = s
		}
		a.mergeUsage(msg["usage"])
		a.addFinishReason(toString(msg["stop_reason"]))
	}

	// Usage can appear at the top level (OpenAI non-stream/final chunk,
	// Anthropic message_delta).
	a.mergeUsage(m["usage"])

	// Anthropic top-level stop_reason (non-stream response + message_delta.delta).
	a.addFinishReason(toString(m["stop_reason"]))
	if delta, ok := m["delta"].(map[string]any); ok {
		a.addFinishReason(toString(delta["stop_reason"]))
		if a.captureContent {
			a.appendContent(toString(delta["text"]))
		}
	}

	// Anthropic streamed text: content_block_delta.
	if a.captureContent {
		if cb, ok := m["content_block"].(map[string]any); ok {
			a.appendContent(toString(cb["text"]))
		}
	}

	// OpenAI: choices[].{finish_reason, message.content, delta.content}.
	if choices, ok := m["choices"].([]any); ok {
		for _, c := range choices {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			a.addFinishReason(toString(cm["finish_reason"]))
			if a.captureContent {
				if msg, ok := cm["message"].(map[string]any); ok {
					a.appendContent(flattenContent(msg["content"]))
				}
				if delta, ok := cm["delta"].(map[string]any); ok {
					a.appendContent(toString(delta["content"]))
				}
			}
		}
	}

	// Anthropic non-stream response: top-level content blocks.
	if a.captureContent {
		if content, ok := m["content"].([]any); ok {
			a.appendContent(flattenBlocks(content))
		}
	}
}

// mergeUsage reads token counts from an OpenAI (prompt_tokens/completion_tokens)
// or Anthropic (input_tokens/output_tokens/cache_*) usage object. For streamed
// responses the latest non-zero value wins.
func (a *ResponseAccumulator) mergeUsage(v any) {
	u, ok := v.(map[string]any)
	if !ok {
		return
	}
	if n, ok := toInt(u["input_tokens"]); ok && n > 0 {
		a.info.InputTokens = n
	}
	if n, ok := toInt(u["prompt_tokens"]); ok && n > 0 {
		a.info.InputTokens = n
	}
	if n, ok := toInt(u["output_tokens"]); ok && n > 0 {
		a.info.OutputTokens = n
	}
	if n, ok := toInt(u["completion_tokens"]); ok && n > 0 {
		a.info.OutputTokens = n
	}
	if n, ok := toInt(u["cache_read_input_tokens"]); ok && n > 0 {
		a.info.CacheReadInputTokens = n
	}
	if n, ok := toInt(u["cache_creation_input_tokens"]); ok && n > 0 {
		a.info.CacheCreationInputTokens = n
	}
	// OpenAI nested reasoning tokens.
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		if n, ok := toInt(d["reasoning_tokens"]); ok && n > 0 {
			a.info.ReasoningOutputTokens = n
		}
	}
}

func (a *ResponseAccumulator) addFinishReason(reason string) {
	if reason == "" || containsFold(a.info.FinishReasons, reason) {
		return
	}
	a.info.FinishReasons = append(a.info.FinishReasons, reason)
}

func (a *ResponseAccumulator) appendContent(s string) {
	if s == "" || a.content.Len() >= maxContentField {
		return
	}
	a.content.WriteString(s)
}
