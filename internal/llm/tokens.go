package llm

import (
	"math"
	"unicode"
)

// KiChat deliberately does not ship a BPE tokenizer. Local models (Qwen, Gemma,
// Llama) each use their own vocabulary, so an OpenAI tokenizer would report
// confidently wrong context fullness. Instead the backend's own reported
// prompt_tokens is treated as authoritative, and the estimator below is only
// used to predict usage *before* a request has been sent.

// EstimateTokens approximates the token count of a string. It is tuned for the
// mixed German/English prose and code that this app deals with.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var words, wordChars, digits, punct, spaces, newlines, wide int
	inWord := false
	for _, r := range text {
		switch {
		case r == '\n':
			newlines++
			inWord = false
		case r == ' ' || r == '\t' || r == '\r':
			spaces++
			inWord = false
		case r > unicode.MaxASCII:
			wide++
			inWord = false
		case unicode.IsDigit(r):
			digits++
			inWord = true
		case unicode.IsLetter(r):
			if !inWord {
				words++
			}
			inWord = true
			wordChars++
		default:
			punct++
			inWord = false
		}
	}
	// Word length has to be counted, not just word count: a long identifier, a
	// base64 blob or a minified JSON fragment is one "word" but many tokens. A
	// count-only model would claim a 500-character token costs a single token,
	// which is exactly the kind of error that makes a context meter lie.
	tokens := float64(wordChars)/4.2 +
		float64(words)*0.15 +
		float64(digits)*0.34 +
		float64(punct)*0.7 +
		float64(spaces)*0.25 +
		float64(newlines)*0.6 +
		float64(wide)*0.8
	n := int(math.Ceil(tokens))
	if n < 1 {
		n = 1
	}
	return n
}

// perMessageOverhead accounts for the role and delimiter tokens a chat template
// adds around each message.
const perMessageOverhead = 4

// EstimateMessagesTokens approximates the token cost of a full message list,
// including the image payloads that are sent as data URLs.
func EstimateMessagesTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += perMessageOverhead
		total += EstimateTokens(m.Content)
		total += EstimateTokens(m.Reasoning) / 2 // reasoning is often trimmed
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Name) + EstimateTokens(tc.Arguments) + 8
		}
		// A data URL of an image costs roughly (bytes/3) tokens for the base64
		// characters plus the image patch tokens, which dominate. We use a flat
		// conservative figure per image instead of pretending to be precise.
		total += len(m.Images) * 1024
	}
	return total
}

// EstimateToolTokens approximates the cost of the tool schemas injected into
// every request.
func EstimateToolTokens(tools []ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += EstimateTokens(t.Name) + EstimateTokens(t.Description) + 64
		if t.Schema != nil {
			total += 96
		}
	}
	return total
}
