package llm

import "testing"

// TestTruncatedStopReasons pins the vendor spellings the chat loop turns into a continuation.
// The list is deliberately short: a reason this function does not recognise is a reply that
// finished, because asking again on a stop the app does not understand is a loop.
func TestTruncatedStopReasons(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{"length", true},     // OpenAI-compatible: LM Studio, llama.cpp, Groq, DeepSeek, NVIDIA, Mistral
		{"length", true},     // Ollama's done_reason
		{"max_tokens", true}, // Anthropic-shaped APIs
		{" Length ", true},
		{"stop", false},
		{"tool_calls", false},
		{"content_filter", false},
		{"stream_error", false},
		{"", false},
	} {
		if got := Truncated(tc.reason); got != tc.want {
			t.Errorf("Truncated(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
