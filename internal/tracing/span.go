package tracing

import (
	"encoding/json"
	"strconv"

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

// setAttributes populates a span from the observation and returns the applied
// attributes (so callers can log them under debug). Content-bearing attributes
// (prompt/completion) are present only when the proxy captured content (config
// tracing.capture_content), so they are set whenever populated.
func setAttributes(span trace.Span, o genai.Observation) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.ServerAddress(o.Host),
		attribute.String(attrCategory, o.Category),
		attribute.String(attrAction, o.Action),
		attribute.Bool(attrRerouted, o.Rerouted),
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

	span.SetAttributes(attrs...)
	return attrs
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
