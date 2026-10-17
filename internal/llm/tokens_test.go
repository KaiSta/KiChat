package llm

import (
	"strings"
	"testing"
)

// TestAToolSchemaIsPricedByItsOwnText keeps the tool block honest about what it costs.
//
// A schema travels as JSON: every property name, type and description is on the wire
// and is counted by the backend's tokenizer. The estimate used to add a flat 96 per
// schema, which undercounted this app's own tools — the image tool documents its size,
// step, sampler and guidance bounds in property descriptions — and that undercount
// rode every request, including the one a fold exists to make small enough. The
// measured case: five shipped tools cost 1385 tokens at a 128k tokenizer against the
// 1261 this returned.
func TestAToolSchemaIsPricedByItsOwnText(t *testing.T) {
	short := ToolDefinition{Name: "tool", Description: "does a thing", Schema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
	}}
	detailed := ToolDefinition{Name: "tool", Description: "does a thing", Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{"a": map[string]any{
			"type":        "string",
			"description": strings.Repeat("Eine lange Beschreibung der Eigenschaft. ", 40),
		}},
	}}

	grown := EstimateToolTokens([]ToolDefinition{detailed}) - EstimateToolTokens([]ToolDefinition{short})
	if grown < 100 {
		t.Fatalf("a 2000-character property description added %d tokens, want it counted", grown)
	}
	// The wrapper around the schema is priced too, so a tool with no schema at all is
	// still more than its name and description alone.
	bare := EstimateToolTokens([]ToolDefinition{{Name: "tool", Description: "does a thing"}})
	if bare <= EstimateTokens("tool")+EstimateTokens("does a thing") {
		t.Errorf("a schema-less tool = %d tokens, want the wire wrapper counted too", bare)
	}
}
