package images

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A FLUX 3 draw does not take the request a user typed. BFL trained the model on
// captions written by a prompt upsampler — a 9 B vision-language model they call the
// PUP — and their README says not to run the model without it: a raw prompt gives
// "noticeably weaker" results, and `--no-pup` is how their own CLI spells the weaker
// path. The upsampler's model card says the model itself is not required: "Any other
// LLM can do the job with the system prompts in system_prompts.json". That is the
// opening this file takes: the chat model that already calls generate_image writes
// the caption, and KiChat never downloads the 18.8 GB PUP.
//
// The text below is that system prompt, copied verbatim from
// https://github.com/black-forest-labs/flux-3-image-klein
// (`src/flux3/pup_prompts.json`, also published as `system_prompts.json` beside the
// PUP weights), which is Apache-2.0. Do not paraphrase it: a paraphrase is a different
// prompt with different behaviour, and calling it BFL's would be a lie.
//
// The `i2i` prompt is deliberately not here yet. It captions an *edit*, and KiChat has
// no way to hand an image model a reference until the request carries one.
const flux3CaptionPrompt = `You are the prompt upsampler of a text-to-image model. Expand the user's short request into ONE detailed, self-contained caption: the exact text the image model receives. Output only the caption, with no greetings, explanations, refusals, questions, Markdown, headings, JSON, coordinates, ids or angle-bracket tokens, and no quotes around it. If the input is not clearly an image request, caption a visualization of its concept. The request may begin with a "TARGET CANVAS: W:H" line: compose for that canvas (wide spreads content sideways, tall stacks it) and never mention the ratio or the canvas.

Fidelity and planning: keep every explicit subject, attribute, count, spatial constraint, style cue and piece of wording; never contradict, substitute, drop or narrow the request, and keep an abstract concept abstract. Decide everything left open as ONE concrete, consistent plan; never offer alternatives or hedge. Never re-express a stated constraint through another mechanism; add camera optics (depth of field, bokeh, blur) only when asked for or implied. Without a stated style, plan an actual photograph and open with the medium convention ("Full-color outdoor photograph, ...", "Full-color indoor photograph, ...", "Studio photograph, ...", "Close-up photograph, ..."); keep "Photorealistic" for renders, and turn "photorealistic" or "realistic" requests for photos into the photograph convention. Give a photograph a few ordinary, non-ideal specifics drawn from the scene's own objects instead of the stock-photo ideal. Do not invent names, brands, logos or celebrities; keep user-named ones. A long request keeps its content but not its wording: drop appraisal and sales vocabulary, turn moods into observable physical facts, and add only new concrete content with positions. Time of day is lighting, not rendered text.

Caption format: one continuous prose block in the generative voice, never "this image shows", "we can see" or "is visible". Lead with medium and style, then scene, lighting and camera, then the subjects with concrete colors, materials and state. Every person in focus gets an observable profile: ethnicity, gender, age range, skin tone ("fair skin", "brown skin"), hair and clothing. Make the layout reconstructable from words: frame-relative positions (frame-left, frame-right, upper, lower, foreground, midground, background), sentence-initial anchors ("On the left, ...", "In the background, ..."), relations (holds, faces, behind) and a consistent order; describe up to about ten instances one by one, beyond that give the exact count and arrangement. Out-of-focus or distant planes are shapes, colors and silhouettes, never identities or readable text, and the background takes about a sixth of the caption. Be definite: no hedging adverbs, no quality or emotion judgments, no closing sentence about mood; state negations as what is present. Rendered text: transcribe every wording verbatim inside double quotes in its original language and script, keep the line breaks of multi-line text, and give its typography, color and placement; author concrete, domain-correct wording when the request needs text it does not supply, and name canonical structures (calendar, keyboard, periodic table) exactly with their full layout. All prose is English; only quoted text keeps its language. Typical length is 100 to 350 words, more for complex scenes.

Output only the caption.`

// CaptionMaxTokens is what BFL's own usage passes for the upsampler: 3,584 new
// tokens. A cap well below it truncates exactly the long, complex captions the prompt
// asks for, and a truncated caption is indistinguishable from a short one.
const CaptionMaxTokens = 3584

// CaptionMinWords is where a request stops being a request. BFL's prompt asks for
// 100-350 words, so a prompt already that long is a caption; captioning it again
// would expand an expansion and drift away from what the user asked for.
const CaptionMinWords = 120

// CaptionMaxWords is where a caption stops being one. A model asked for 100-350 words
// that returns this many is writing something else, and a value that long is more
// likely a runaway generation than a better prompt — so it is refused, which falls
// back to the user's own words, rather than trimmed mid-sentence.
const CaptionMaxWords = 800

// ErrEmptyCaption marks a caption request that produced no usable text. It is its own
// error because the caller's answer is not "fail the draw": it is to draw the user's
// own request instead, and say that the caption did not arrive.
var ErrEmptyCaption = errors.New("the caption model returned no caption")

// CaptionRequest is one caption asked of the model that is already loaded: the model
// that drove the tool call, on the turn's own backend. The size travels with it
// because the prompt asks the model to describe the layout of the target canvas.
type CaptionRequest struct {
	Prompt string
	Width  int
	Height int
}

// CaptionResult is a caption and where it came from. The model is recorded beside the
// picture so its card can say which model really drew it, which is the cure for "why
// does this look nothing like my prompt?".
type CaptionResult struct {
	Text  string
	Model string
}

// CaptionSystemPrompt returns BFL's published t2i prompt verbatim.
func CaptionSystemPrompt() string { return flux3CaptionPrompt }

// CanvasLine is the one line BFL's t2i prompt expects above the request. It is a t2i
// convention: the `i2i` prompt has no such line, because the reference image already
// says what size the result has to be.
func CanvasLine(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	return fmt.Sprintf("TARGET CANVAS: %d:%d", width, height)
}

// CaptionMessage is the user message of a caption request: the canvas line when the
// size is known, then the request itself.
func CaptionMessage(req CaptionRequest) string {
	parts := make([]string, 0, 2)
	if line := CanvasLine(req.Width, req.Height); line != "" {
		parts = append(parts, line)
	}
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		parts = append(parts, prompt)
	}
	return strings.Join(parts, "\n\n")
}

// CleanCaption turns what a chat model wrote into the single block of prose FLUX 3
// was trained on.
//
// The wrapping is the failure this exists for: asked for "the caption only", a model
// still answers with a code fence, a JSON object, quotes, or a line announcing what
// follows. All of them reach the image model as literal text — a picture of the words
// "Here is the caption:" is the least funny outcome a draw can have. Newlines are
// collapsed because BFL's prompt asks for one continuous caption.
func CleanCaption(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", ErrEmptyCaption
	}
	if inner, ok := unwrapJSONCaption(text); ok {
		text = inner
	}
	text = stripFence(text)
	text = strings.TrimSpace(text)
	text = trimWrappingQuotes(text)
	text = stripPreamble(text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", ErrEmptyCaption
	}
	if n := WordCount(text); n > CaptionMaxWords {
		return "", fmt.Errorf("die Bildbeschreibung ist mit %d Wörtern zu lang (höchstens %d)", n, CaptionMaxWords)
	}
	return text, nil
}

// IsCaption reports whether a request is already long enough to be a caption. It is
// a word count rather than a heuristic about style because the failure it prevents —
// expanding an expansion — is about length, and length is what a small model can be
// held to.
func IsCaption(s string) bool { return WordCount(s) >= CaptionMinWords }

// WordCount is the number of whitespace-separated fields, which is what both bounds
// above are written in.
func WordCount(s string) int { return len(strings.Fields(s)) }

// unwrapJSONCaption finds a caption inside a JSON object a model wrapped it in. The
// keys are the ones models reach for unprompted; anything else is left alone, because
// guessing at an arbitrary structure is how a caption becomes a fragment of one.
func unwrapJSONCaption(text string) (string, bool) {
	if !strings.HasPrefix(text, "{") {
		return "", false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return "", false
	}
	for _, key := range []string{"caption", "prompt", "instruction", "text", "description"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			return v, true
		}
	}
	return "", false
}

// stripFence removes a Markdown code fence, with or without a language tag. Only the
// outer pair is removed: a fence inside a caption is text the user may have asked for.
func stripFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	if idx := strings.Index(trimmed, "\n"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	} else {
		return strings.TrimPrefix(trimmed, "```")
	}
	if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimSpace(trimmed)
}

// trimWrappingQuotes removes one matched pair of quotes around the whole answer.
func trimWrappingQuotes(text string) string {
	text = strings.TrimSpace(text)
	pairs := [][2]string{{`"`, `"`}, {"\u201c", "\u201d"}, {"'", "'"}}
	for _, p := range pairs {
		if len(text) >= len(p[0])+len(p[1]) && strings.HasPrefix(text, p[0]) && strings.HasSuffix(text, p[1]) {
			return strings.TrimSpace(text[len(p[0]) : len(text)-len(p[1])])
		}
	}
	return text
}

// stripPreamble drops a leading "Here is the caption:" line. It is bounded by length
// and by a keyword so that a caption whose first line happens to end in a colon is
// not decapitated.
func stripPreamble(text string) string {
	line, rest, ok := strings.Cut(text, "\n")
	if !ok {
		return text
	}
	line = strings.TrimSpace(line)
	if len(line) > 80 || !strings.HasSuffix(line, ":") {
		return text
	}
	lower := strings.ToLower(line)
	for _, marker := range []string{"caption", "prompt", "description", "beschreibung", "here is", "here's"} {
		if strings.Contains(lower, marker) {
			return strings.TrimSpace(rest)
		}
	}
	return text
}
