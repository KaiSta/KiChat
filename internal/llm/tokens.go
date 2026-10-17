package llm

import (
	"encoding/json"
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

// imageTokensEach is the conservative per-image cost. A data URL of an image
// costs roughly (bytes/3) tokens for the base64 characters plus the image patch
// tokens, which dominate; a flat figure per image is used instead of pretending
// to be precise. It is shared with the budget accounting so the two cannot drift.
const imageTokensEach = 1024

// EstimateImageTokens returns the conservative token cost of n image payloads.
func EstimateImageTokens(n int) int { return n * imageTokensEach }

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
		// A flat conservative figure per image, for the reason above.
		total += EstimateImageTokens(len(m.Images))
	}
	return total
}

// EstimateToolTokens approximates the cost of the tool schemas injected into
// every request.
//
// The schema is priced as the JSON it is sent as rather than as a flat allowance,
// because a property's own description travels on the wire too. This app's tools are
// the case that showed it: the image tool documents its size bounds, step bounds,
// sampler names and guidance scale in the schema, and at a 128k-vocabulary tokenizer
// the five shipped tools cost 1385 tokens where the old name + description + 96
// figure said 1261. That undercount rides *every* request, including the one after a
// fold, where nothing else corrects it — which is why the JSON estimate (1537, the
// safe direction) is used instead.
func EstimateToolTokens(tools []ToolDefinition) int {
	total := 0
	for _, t := range tools {
		total += EstimateTokens(t.Name) + EstimateTokens(t.Description)
		if t.Schema != nil {
			if raw, err := json.Marshal(t.Schema); err == nil {
				total += EstimateTokens(string(raw))
			} else {
				// A schema that will not marshal is not the shape a request can carry
				// anyway; the flat figure keeps this a measurement of something.
				total += 96
			}
		}
		// The wire wrapper around it: {"type":"function","function":{…}}.
		total += 8
	}
	return total
}
