package genai

import (
	"encoding/json"
	"strings"
)

// maxContentField bounds how much of any single captured content string we keep,
// so a very large prompt/completion can't bloat a span attribute.
const maxContentField = 8 * 1024

// ProviderFor infers the GenAI provider from the request host. It is a
// best-effort mapping; an unknown host returns the host itself so the span still
// carries a provider-ish label.
func ProviderFor(host string) string {
	h := strings.ToLower(host)
	switch {
	case strings.Contains(h, "anthropic"):
		return ProviderAnthropic
	case strings.Contains(h, "openai.azure.com"), strings.Contains(h, "cognitiveservices.azure.com"):
		return ProviderAzure
	case strings.Contains(h, "openai"):
		return ProviderOpenAI
	case strings.Contains(h, "generativelanguage.googleapis.com"), strings.Contains(h, "aiplatform.googleapis.com"):
		return ProviderGoogle
	default:
		return host
	}
}

// operationFor infers the gen_ai.operation.name from the request path.
func operationFor(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/embeddings"):
		return OpEmbeddings
	case strings.HasSuffix(p, "/complete"), strings.HasSuffix(p, "/completions") && !strings.Contains(p, "/chat/"):
		return OpTextGen
	default:
		// chat/completions, messages, responses, and unknown LLM endpoints.
		return OpChat
	}
}

// ParseRequest extracts request metadata from an LLM/MCP request body. host and
// path steer provider/operation detection. When captureContent is false, no
// message text (prompt) is retained. A non-JSON or unrecognized body yields a
// RequestInfo with just the provider/operation set.
func ParseRequest(host, path string, body []byte, captureContent bool) RequestInfo {
	info := RequestInfo{
		Provider:  ProviderFor(host),
		Operation: operationFor(path),
	}
	if len(body) == 0 {
		return info
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return info
	}

	// JSON-RPC (MCP) request.
	if v, ok := m["jsonrpc"]; ok && toString(v) == "2.0" {
		if method, ok := m["method"]; ok {
			info.RPCMethod = toString(method)
		}
	}

	if s := toString(m["model"]); s != "" {
		info.Model = s
	}
	if n, ok := toInt(m["max_tokens"]); ok {
		info.MaxTokens = n
	} else if n, ok := toInt(m["max_completion_tokens"]); ok {
		info.MaxTokens = n
	}
	if f, ok := toFloat(m["temperature"]); ok {
		info.Temperature = &f
	}
	if f, ok := toFloat(m["top_p"]); ok {
		info.TopP = &f
	}
	if b, ok := m["stream"].(bool); ok {
		info.Stream = b
	}

	if captureContent {
		if s := extractSystem(m["system"]); s != "" {
			info.System = clamp(s, maxContentField)
		}
		info.Messages = extractMessages(m)
	}
	return info
}

// extractSystem flattens an Anthropic top-level "system" field, which may be a
// string or an array of content blocks.
func extractSystem(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		return flattenBlocks(t)
	default:
		return ""
	}
}

// extractMessages flattens the "messages" (chat) or "input"/"prompt" fields into
// role/content pairs, capturing content up to a per-field cap.
func extractMessages(m map[string]any) []Message {
	arr, ok := m["messages"].([]any)
	if !ok {
		// Fall back to a single "input"/"prompt" string (Responses / legacy).
		if s := toString(m["input"]); s != "" {
			return []Message{{Role: "user", Content: clamp(s, maxContentField)}}
		}
		if s := toString(m["prompt"]); s != "" {
			return []Message{{Role: "user", Content: clamp(s, maxContentField)}}
		}
		return nil
	}
	out := make([]Message, 0, len(arr))
	for _, e := range arr {
		mm, ok := e.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, Message{
			Role:    toString(mm["role"]),
			Content: clamp(flattenContent(mm["content"]), maxContentField),
		})
	}
	return out
}

// flattenContent renders a message "content" value (string, or array of blocks
// with {type:text,text:...}) into plain text.
func flattenContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		return flattenBlocks(t)
	default:
		return ""
	}
}

func flattenBlocks(blocks []any) string {
	var b strings.Builder
	for _, blk := range blocks {
		bm, ok := blk.(map[string]any)
		if !ok {
			if s, ok := blk.(string); ok {
				b.WriteString(s)
			}
			continue
		}
		if s := toString(bm["text"]); s != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s)
		}
	}
	return b.String()
}
