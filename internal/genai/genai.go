// Package genai extracts GenAI (LLM) and MCP call metadata from intercepted
// request and response bodies, provider-agnostically. It has NO OpenTelemetry
// dependency: it produces plain data (an Observation) that the tracing package
// maps onto OpenTelemetry spans. Keeping the parsing here means the proxy — the
// correctness-critical hot path — can populate spans without importing the OTel
// SDK.
//
// Supported request/response shapes today: the OpenAI Chat Completions /
// Embeddings / Responses APIs, the Anthropic Messages API, and JSON-RPC 2.0
// (MCP). Parsing is best-effort: an unrecognized body yields empty fields
// rather than an error, so a span is still emitted with whatever is known.
package genai

import "time"

// Provider names (aligned with OpenTelemetry gen_ai.provider.name values where
// one exists).
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGoogle    = "gcp.gemini"
	ProviderAzure     = "azure.ai.openai"
)

// Operation names (gen_ai.operation.name).
const (
	OpChat       = "chat"
	OpEmbeddings = "embeddings"
	OpTextGen    = "text_completion"
)

// Message is one chat message, captured only when content capture is enabled.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RequestInfo is what we learned from the request body/URL.
type RequestInfo struct {
	Provider    string
	Operation   string
	Model       string
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stream      bool
	System      string    // optional system prompt (content capture only)
	Messages    []Message // populated only when content capture is enabled
	RPCMethod   string    // JSON-RPC method, for MCP calls
}

// ResponseInfo is what we learned from the response body/stream.
type ResponseInfo struct {
	Model                    string
	ResponseID               string
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	ReasoningOutputTokens    int
	FinishReasons            []string
	Content                  string // completion text, content capture only
}

// Observation is the full record of one intercepted exchange handed to a
// Tracer. It intentionally may carry content (unlike capture.Exchange), so it
// is only ever sent to the tracer — never to the local sinks/UI.
type Observation struct {
	Start time.Time
	End   time.Time

	App      string
	Category string // llm | mcp | other
	Action   string // reroute | passthrough | block
	Rerouted bool
	FailOpen bool // gateway gap (reroute fell back to upstream)

	Method string
	Scheme string
	Host   string
	Path   string
	PID    int

	Status int    // HTTP response status (0 if none)
	Err    string // transport/proxy error, if any

	Request  RequestInfo
	Response ResponseInfo
}

// Duration returns the exchange wall-clock duration.
func (o Observation) Duration() time.Duration { return o.End.Sub(o.Start) }
