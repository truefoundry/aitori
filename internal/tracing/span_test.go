package tracing

import (
	"encoding/json"
	"testing"

	"github.com/truefoundry/aitori/internal/genai"
)

// The GenAI content attributes must follow the new structured message shape:
// messages are {role, parts:[{type,content}]} and system_instructions is an
// array of {type,content} parts.
func TestInputMessagesShape(t *testing.T) {
	msgs := inputMessages([]genai.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	})
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}
	parts, ok := got[1]["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("message parts malformed: %v", got[1])
	}
	p := parts[0].(map[string]any)
	if p["type"] != "text" || p["content"] != "hi" {
		t.Errorf("part = %v", p)
	}
}

func TestOutputMessagesShape(t *testing.T) {
	msgs := outputMessages("hello", []string{"stop", "length"})
	if len(msgs) != 1 || msgs[0].Role != "assistant" || msgs[0].FinishReason != "stop" {
		t.Fatalf("output message = %+v", msgs)
	}
	if len(msgs[0].Parts) != 1 || msgs[0].Parts[0].Content != "hello" {
		t.Fatalf("output parts = %+v", msgs[0].Parts)
	}
}

func TestSystemInstructionsShape(t *testing.T) {
	parts := textParts("you are helpful")
	if len(parts) != 1 || parts[0].Type != "text" || parts[0].Content != "you are helpful" {
		t.Fatalf("system parts = %+v", parts)
	}
}
