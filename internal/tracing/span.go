package tracing

import (
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/truefoundry/aitori/internal/config"
	"github.com/truefoundry/aitori/internal/genai"
)

// GenAI semantic-convention attribute keys. These live in the separate
// open-telemetry/semantic-conventions-genai registry (Development status) and
// have no stable Go helper, so they are declared here as string keys.
const (
	attrGenAIProvider         = "gen_ai.provider.name"
	attrGenAIOperation        = "gen_ai.operation.name"
	attrGenAIRequestModel     = "gen_ai.request.model"
	attrGenAIRequestMaxToken  = "gen_ai.request.max_tokens"
	attrGenAIRequestTemp      = "gen_ai.request.temperature"
	attrGenAIRequestTopP      = "gen_ai.request.top_p"
	attrGenAIRequestStream    = "gen_ai.request.stream"
	attrGenAIResponseModel    = "gen_ai.response.model"
	attrGenAIResponseID       = "gen_ai.response.id"
	attrGenAIFinishReasons    = "gen_ai.response.finish_reasons"
	attrGenAIInputTokens      = "gen_ai.usage.input_tokens"
	attrGenAIOutputTokens     = "gen_ai.usage.output_tokens"
	attrGenAIReasoningTokens  = "gen_ai.usage.reasoning.output_tokens"
	attrGenAICacheReadTokens  = "gen_ai.usage.cache_read.input_tokens"
	attrGenAICacheCreatTokens = "gen_ai.usage.cache_creation.input_tokens"
	attrGenAIInputMessages    = "gen_ai.input.messages"
	attrGenAIOutputMessages   = "gen_ai.output.messages"
	attrGenAISystemPrompt     = "gen_ai.system_instructions"
)

// aitori-specific attribution keys (namespaced so they don't collide with
// convention attributes).
const (
	attrApp        = "aitori.app"
	attrCategory   = "aitori.category"
	attrAction     = "aitori.action"
	attrPID        = "aitori.pid"
	attrRerouted   = "aitori.rerouted"
	attrGatewayGap = "aitori.gateway_gap"
)

// User-attribution keys. attrUserID is the vendor-neutral OpenTelemetry general
// attribute; the tfy.request.* keys mirror the gateway's created_by_subject
// shape so aitori spans group by user in the TrueFoundry UI.
const (
	attrUserID              = "user.id"
	attrTFYCreatedBySubject = "tfy.request.created_by_subject"
	attrTFYSubjectTeams     = "tfy.request.created_by_subject_teams"
	attrTFYTenant           = "tfy.request.tenant_name"
)

// TrueFoundry native span-attribute schema (tfy.*). Emitted alongside the
// gen_ai.* conventions so spans render in the TrueFoundry tracing UI identically
// to gateway-produced ones. Keys mirror TFY_TRACE_ATTRIBUTES in tfy-llm-gateway.
const (
	attrTFYSpanType       = "tfy.span_type"
	attrTFYInput          = "tfy.input"
	attrTFYOutput         = "tfy.output"
	attrTFYErrorType      = "tfy.error_type"
	attrTFYErrorMessage   = "tfy.error_message"
	attrTFYRequestModel   = "tfy.request.model_name"
	attrTFYRequestMeta    = "tfy.request.metadata"
	attrTFYModelName      = "tfy.model.name"
	attrTFYModelReqURL    = "tfy.model.request_url"
	attrTFYModelReqType   = "tfy.model.request_type"
	attrTFYModelStreaming = "tfy.model.streaming"
	attrTFYMetricInTok    = "tfy.model.metric.input_tokens"
	attrTFYMetricOutTok   = "tfy.model.metric.output_tokens"
	attrTFYMetricCacheR   = "tfy.model.metric.cache_read_input_tokens"
	attrTFYMetricCacheC   = "tfy.model.metric.cache_creation_input_tokens"
	attrTFYMetricLatency  = "tfy.model.metric.latency_in_ms"
	attrTFYMCPMethod      = "tfy.mcp_server.method"
	attrTFYMCPURL         = "tfy.mcp_server.url"
)

// TrueFoundry span_type / model_request_type values (subset of SpanType in
// tfy-llm-gateway that aitori can produce).
const (
	tfySpanChatCompletion = "ChatCompletion"
	tfySpanMessages       = "Messages"
	tfySpanEmbedding      = "Embedding"
	tfySpanCompletion     = "Completion"
	tfySpanMCP            = "MCP"
	tfySpanAIProxy        = "AIProxy"
)

// setAttributes populates a span from the observation and returns the applied
// attributes (so callers can log them under debug). Content-bearing attributes
// (prompt/completion) are present only when the proxy captured content (config
// tracing.capture_content), so they are set whenever populated. When tfy is set,
// the TrueFoundry tfy.* schema is emitted alongside the gen_ai.* conventions.
func setAttributes(span trace.Span, o genai.Observation, tfy bool, attr Attribution) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.ServerAddress(o.Host),
		attribute.String(attrCategory, o.Category),
		attribute.String(attrAction, o.Action),
		attribute.Bool(attrRerouted, o.Rerouted),
	}
	// User attribution: vendor-neutral user.id on every span.
	if attr.SubjectSlug != "" {
		attrs = append(attrs, attribute.String(attrUserID, attr.SubjectSlug))
	}
	if o.Method != "" {
		attrs = append(attrs, semconv.HTTPRequestMethodKey.String(o.Method))
	}
	if o.Path != "" {
		attrs = append(attrs, semconv.URLPath(o.Path))
	}
	if o.Status > 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(o.Status))
	}
	if o.App != "" {
		attrs = append(attrs, attribute.String(attrApp, o.App))
	}
	if o.PID > 0 {
		attrs = append(attrs, attribute.Int(attrPID, o.PID))
	}
	if o.FailOpen {
		attrs = append(attrs, attribute.Bool(attrGatewayGap, true))
	}

	switch o.Category {
	case config.CategoryLLM:
		attrs = append(attrs, genAIAttributes(o)...)
	case config.CategoryMCP:
		attrs = append(attrs, mcpAttributes(o)...)
	}

	if tfy {
		attrs = append(attrs, tfyAttributes(o, attr)...)
	}

	span.SetAttributes(attrs...)
	return attrs
}

// tfyAttribution emits the gateway's created_by_subject shape (subject JSON,
// tenant, teams) so aitori spans attribute to the invoking user in the
// TrueFoundry tracing UI.
func tfyAttribution(attr Attribution) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if attr.SubjectSlug != "" || attr.SubjectID != "" {
		subj := map[string]string{
			"subjectId":   attr.SubjectID,
			"subjectSlug": attr.SubjectSlug,
			"subjectType": attr.SubjectType,
			"tenantName":  attr.Tenant,
		}
		if b, err := json.Marshal(subj); err == nil {
			attrs = append(attrs, attribute.String(attrTFYCreatedBySubject, string(b)))
		}
	}
	if attr.Tenant != "" {
		attrs = append(attrs, attribute.String(attrTFYTenant, attr.Tenant))
	}
	if len(attr.Teams) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrTFYSubjectTeams, attr.Teams))
	}
	return attrs
}

// tfySpanType maps an observation onto a TrueFoundry span_type / request_type.
func tfySpanType(o genai.Observation) string {
	switch o.Category {
	case config.CategoryMCP:
		return tfySpanMCP
	case config.CategoryLLM:
		switch o.Request.Operation {
		case genai.OpEmbeddings:
			return tfySpanEmbedding
		case genai.OpTextGen:
			return tfySpanCompletion
		}
		// Anthropic's /v1/messages surface maps to the Messages request type.
		if o.Request.Provider == genai.ProviderAnthropic {
			return tfySpanMessages
		}
		return tfySpanChatCompletion
	default:
		return tfySpanAIProxy
	}
}

// tfyAttributes emits the TrueFoundry tfy.* schema so the span renders in the
// TrueFoundry tracing UI like a gateway-produced one.
func tfyAttributes(o genai.Observation, attr Attribution) []attribute.KeyValue {
	req, resp := o.Request, o.Response
	spanType := tfySpanType(o)
	attrs := []attribute.KeyValue{attribute.String(attrTFYSpanType, spanType)}

	if u := requestURL(o); u != "" {
		attrs = append(attrs, attribute.String(attrTFYModelReqURL, u))
	}
	// Latency is always known.
	attrs = append(attrs, attribute.Float64(attrTFYMetricLatency, latencyMS(o)))
	// User attribution (created_by_subject, tenant, teams).
	attrs = append(attrs, tfyAttribution(attr)...)
	// Metadata merges the resolving app with any configured attribution metadata.
	if meta := requestMetadata(o, attr); len(meta) > 0 {
		if b, err := json.Marshal(meta); err == nil {
			attrs = append(attrs, attribute.String(attrTFYRequestMeta, string(b)))
		}
	}
	if o.Err != "" {
		attrs = append(attrs,
			attribute.String(attrTFYErrorMessage, o.Err),
			attribute.String(attrTFYErrorType, "proxy_error"),
		)
	}
	// Raw request/response bodies (content capture only). Guard against invalid
	// UTF-8: OTLP string fields must be valid UTF-8, and a single bad byte would
	// otherwise make the exporter reject the whole batch (dropping all spans).
	if s, ok := validUTF8(req.RawBody); ok {
		attrs = append(attrs, attribute.String(attrTFYInput, s))
	}
	if s, ok := validUTF8(resp.RawBody); ok {
		attrs = append(attrs, attribute.String(attrTFYOutput, s))
	}

	if o.Category == config.CategoryMCP {
		if req.RPCMethod != "" {
			attrs = append(attrs, attribute.String(attrTFYMCPMethod, req.RPCMethod))
		}
		if u := requestURL(o); u != "" {
			attrs = append(attrs, attribute.String(attrTFYMCPURL, u))
		}
		return attrs
	}

	// LLM (model-request) attributes.
	model := req.Model
	if model == "" {
		model = resp.Model
	}
	if model != "" {
		attrs = append(attrs, attribute.String(attrTFYModelName, model))
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(attrTFYRequestModel, req.Model))
	}
	attrs = append(attrs,
		attribute.String(attrTFYModelReqType, spanType),
		attribute.Bool(attrTFYModelStreaming, req.Stream),
	)
	if resp.InputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrTFYMetricInTok, resp.InputTokens))
	}
	if resp.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrTFYMetricOutTok, resp.OutputTokens))
	}
	if resp.CacheReadInputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrTFYMetricCacheR, resp.CacheReadInputTokens))
	}
	if resp.CacheCreationInputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrTFYMetricCacheC, resp.CacheCreationInputTokens))
	}
	return attrs
}

// validUTF8 returns s and true when s is non-empty and valid UTF-8. A non-empty
// body that is not valid UTF-8 (e.g. an undecoded binary payload) is dropped
// rather than emitted, since OTLP string attributes must be valid UTF-8 — one
// invalid byte would cause the collector export to reject the entire batch.
func validUTF8(s string) (string, bool) {
	if s == "" || !utf8.ValidString(s) {
		return "", false
	}
	return s, true
}

// requestMetadata merges the configured attribution metadata with the resolving
// app, forming the tfy.request.metadata JSON object. Configured keys win over
// the built-in aitori.app so a deployment can override it deliberately.
func requestMetadata(o genai.Observation, attr Attribution) map[string]string {
	if o.App == "" && len(attr.Metadata) == 0 {
		return nil
	}
	meta := make(map[string]string, len(attr.Metadata)+1)
	if o.App != "" {
		meta["aitori.app"] = o.App
	}
	for k, v := range attr.Metadata {
		meta[k] = v
	}
	return meta
}

// requestURL reconstructs the client-visible request URL from the observation.
func requestURL(o genai.Observation) string {
	if o.Host == "" {
		return ""
	}
	scheme := o.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + o.Host + o.Path
}

// latencyMS returns the exchange duration in milliseconds, rounded to 2 dp.
func latencyMS(o genai.Observation) float64 {
	ms := float64(o.Duration().Microseconds()) / 1000.0
	return math.Round(ms*100) / 100
}

func genAIAttributes(o genai.Observation) []attribute.KeyValue {
	req, resp := o.Request, o.Response
	attrs := []attribute.KeyValue{}
	if req.Provider != "" {
		attrs = append(attrs, attribute.String(attrGenAIProvider, req.Provider))
	}
	if req.Operation != "" {
		attrs = append(attrs, attribute.String(attrGenAIOperation, req.Operation))
	}
	if req.Model != "" {
		attrs = append(attrs, attribute.String(attrGenAIRequestModel, req.Model))
	}
	if req.MaxTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIRequestMaxToken, req.MaxTokens))
	}
	if req.Temperature != nil {
		attrs = append(attrs, attribute.Float64(attrGenAIRequestTemp, *req.Temperature))
	}
	if req.TopP != nil {
		attrs = append(attrs, attribute.Float64(attrGenAIRequestTopP, *req.TopP))
	}
	if req.Stream {
		attrs = append(attrs, attribute.Bool(attrGenAIRequestStream, true))
	}

	if resp.Model != "" {
		attrs = append(attrs, attribute.String(attrGenAIResponseModel, resp.Model))
	}
	if resp.ResponseID != "" {
		attrs = append(attrs, attribute.String(attrGenAIResponseID, resp.ResponseID))
	}
	if len(resp.FinishReasons) > 0 {
		attrs = append(attrs, attribute.StringSlice(attrGenAIFinishReasons, resp.FinishReasons))
	}
	if resp.InputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIInputTokens, resp.InputTokens))
	}
	if resp.OutputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIOutputTokens, resp.OutputTokens))
	}
	if resp.ReasoningOutputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAIReasoningTokens, resp.ReasoningOutputTokens))
	}
	if resp.CacheReadInputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAICacheReadTokens, resp.CacheReadInputTokens))
	}
	if resp.CacheCreationInputTokens > 0 {
		attrs = append(attrs, attribute.Int(attrGenAICacheCreatTokens, resp.CacheCreationInputTokens))
	}

	// Content attributes follow the new GenAI message structure
	// (role + typed parts), serialized to JSON since the OTel Go attribute API
	// cannot hold nested values. They are present only when the proxy captured
	// content (tracing.capture_content).
	if len(req.Messages) > 0 {
		if b, err := json.Marshal(inputMessages(req.Messages)); err == nil {
			attrs = append(attrs, attribute.String(attrGenAIInputMessages, string(b)))
		}
	}
	if req.System != "" {
		if b, err := json.Marshal(textParts(req.System)); err == nil {
			attrs = append(attrs, attribute.String(attrGenAISystemPrompt, string(b)))
		}
	}
	if resp.Content != "" {
		if b, err := json.Marshal(outputMessages(resp.Content, resp.FinishReasons)); err == nil {
			attrs = append(attrs, attribute.String(attrGenAIOutputMessages, string(b)))
		}
	}
	return attrs
}

// GenAI message structures per the semantic-conventions-genai registry:
// gen_ai.input.messages / gen_ai.output.messages are arrays of
// {role, parts:[{type, content}], finish_reason?}, and gen_ai.system_instructions
// is an array of {type, content} parts.
type genAIPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type genAIMessage struct {
	Role         string      `json:"role"`
	Parts        []genAIPart `json:"parts"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

func textParts(content string) []genAIPart {
	return []genAIPart{{Type: "text", Content: content}}
}

func inputMessages(msgs []genai.Message) []genAIMessage {
	out := make([]genAIMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, genAIMessage{Role: m.Role, Parts: textParts(m.Content)})
	}
	return out
}

func outputMessages(content string, finishReasons []string) []genAIMessage {
	finish := ""
	if len(finishReasons) > 0 {
		finish = finishReasons[0]
	}
	return []genAIMessage{{Role: "assistant", Parts: textParts(content), FinishReason: finish}}
}

func mcpAttributes(o genai.Observation) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("rpc.system", "jsonrpc")}
	if o.Request.RPCMethod != "" {
		attrs = append(attrs, attribute.String("rpc.jsonrpc.method", o.Request.RPCMethod))
	}
	return attrs
}

// setStatus marks the span as errored on a transport error or a >=400 status.
func setStatus(span trace.Span, o genai.Observation) {
	switch {
	case o.Err != "":
		span.SetStatus(codes.Error, o.Err)
		span.SetAttributes(attribute.String("error.type", "proxy_error"))
	case o.Status >= 400:
		span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(o.Status))
		span.SetAttributes(attribute.String("error.type", strconv.Itoa(o.Status)))
	}
}
