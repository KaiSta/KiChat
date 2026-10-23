package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolArgumentsSurviveTheRoundTripToOllama is the ordinary case: a stored
// call's arguments reach the wire as the same object.
func TestToolArgumentsSurviveTheRoundTripToOllama(t *testing.T) {
	msgs := toOllamaMessages([]Message{{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:        "call-1",
			Name:      "search_web",
			Arguments: `{"query":"Mietrecht","count":5}`,
		}},
	}})
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("mapped to %+v, want one message with one call", msgs)
	}
	got := msgs[0].ToolCalls[0].Function.Arguments
	if got["query"] != "Mietrecht" {
		t.Errorf("query = %v, want Mietrecht", got["query"])
	}
	if _, ok := got["_raw_arguments"]; ok {
		t.Error("valid arguments were wrapped as unreadable")
	}
}

// TestUnreadableToolArgumentsAreNotReplacedByNothing pins what the model must not
// be told. Sending an empty object would say "this call took no arguments", which
// is a claim about the model's own earlier turn — so the unreadable text is passed
// through instead, and a conversation replayed every turn keeps its meaning.
func TestUnreadableToolArgumentsAreNotReplacedByNothing(t *testing.T) {
	msgs := toOllamaMessages([]Message{{
		Role: RoleAssistant,
		ToolCalls: []ToolCall{{
			ID:        "call-2",
			Name:      "search_web",
			Arguments: `{"query": "Mietrecht"`, // truncated JSON, as a damaged row would hold
		}},
	}})
	got := msgs[0].ToolCalls[0].Function.Arguments
	if len(got) == 0 {
		t.Fatal("unreadable arguments became an empty object, which reads as a call with no arguments")
	}
	raw, _ := got["_raw_arguments"].(string)
	if !strings.Contains(raw, "Mietrecht") {
		t.Errorf("the original argument text was lost: %v", got)
	}

	// And it still has to marshal, because that is what goes on the wire.
	if _, err := json.Marshal(msgs); err != nil {
		t.Fatalf("the mapped message does not encode: %v", err)
	}
}

// TestOllamaNeverSeesAnImageOnAnAssistantTurn is the same rule the shared OpenAI
// wire enforces: an image is a user turn's part. A generated picture is filed as an
// attachment of the assistant row that asked for it, so it must not reach the wire
// there — no provider's assistant turn takes one.
func TestOllamaNeverSeesAnImageOnAnAssistantTurn(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	msgs := toOllamaMessages([]Message{
		{Role: RoleAssistant, Images: []string{png}},
		{Role: RoleUser, Content: "Was ist das?", Images: []string{png}},
	})
	if len(msgs[0].Images) != 0 {
		t.Errorf("the assistant turn carried an image: %v", msgs[0].Images)
	}
	if len(msgs[1].Images) != 1 {
		t.Errorf("the user's picture was dropped: %v", msgs[1].Images)
	}
}

// TestEmptyArgumentsStayEmpty keeps the two cases apart: a call that genuinely
// took no arguments must not pick up the unreadable marker.
func TestEmptyArgumentsStayEmpty(t *testing.T) {
	msgs := toOllamaMessages([]Message{{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call-3", Name: "list_documents", Arguments: ""}},
	}})
	got := msgs[0].ToolCalls[0].Function.Arguments
	if len(got) != 0 {
		t.Errorf("arguments = %v, want an empty object", got)
	}
}
