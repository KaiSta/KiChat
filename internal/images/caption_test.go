package images

import (
	"errors"
	"strings"
	"testing"
)

// TestTheCaptionPromptIsBFLsVerbatim pins the shape of the prompt rather than its hash.
//
// A hash would fail on a whitespace edit that changes nothing, while the failures worth
// catching are structural: a paraphrased prompt is a different prompt, and calling it
// BFL's would be a lie; a backtick would break the raw string it lives in; and a word
// count far off the published 506 means the wrong file was copied.
func TestTheCaptionPromptIsBFLsVerbatim(t *testing.T) {
	prompt := CaptionSystemPrompt()
	for _, want := range []string{
		"You are the prompt upsampler of a text-to-image model.",
		"TARGET CANVAS",
		"Output only the caption.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt no longer contains %q", want)
		}
	}
	if strings.Contains(prompt, "`") {
		t.Error("the prompt contains a backtick, which ends the raw string it is stored in")
	}
	if n := WordCount(prompt); n < 400 || n > 600 {
		t.Errorf("the prompt is %d words; the published t2i prompt is 506, so this looks like another file", n)
	}
	if strings.Contains(prompt, "bbox") {
		t.Error("the bounding-box prompt is not the t2i prompt; boxes are a separate file until a build is measured reading one")
	}
}

// TestCleanCaptionStripsTheWrapping is the reason CleanCaption exists: asked for the
// caption only, a model still wraps it, and every wrapper reaches the image model as
// literal text.
func TestCleanCaptionStripsTheWrapping(t *testing.T) {
	const body = "A red fox stands in deep snow at golden hour, its breath visible in the cold air."
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", body, body},
		{"fenced", "```\n" + body + "\n```", body},
		{"fenced with a language", "```text\n" + body + "\n```", body},
		{"double quotes", `"` + body + `"`, body},
		{"curly quotes", "\u201c" + body + "\u201d", body},
		{"json object", `{"caption": "` + body + `"}`, body},
		{"preamble line", "Here is the caption:\n" + body, body},
		{"preamble in German", "Bildbeschreibung:\n" + body, body},
		{"newlines collapsed", "A red fox\nstands in deep snow", "A red fox stands in deep snow"},
		{"surrounding whitespace", "\n\n  " + body + "  \n", body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanCaption(tc.in)
			if err != nil {
				t.Fatalf("CleanCaption: %v", err)
			}
			if got != tc.want {
				t.Errorf("CleanCaption(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCleanCaptionRefusesWhatItCannotUse pins the two failures that must not become a
// prompt: nothing at all, and a runaway generation. An empty `-p` draws an arbitrary
// picture, and a caption of several hundred words is more likely a model that never
// stopped than a better prompt.
func TestCleanCaptionRefusesWhatItCannotUse(t *testing.T) {
	for _, in := range []string{"", "   \n\t ", "```\n\n```", `"   "`} {
		if _, err := CleanCaption(in); !errors.Is(err, ErrEmptyCaption) {
			t.Errorf("CleanCaption(%q) = %v, want ErrEmptyCaption", in, err)
		}
	}
	long := strings.TrimSpace(strings.Repeat("word ", CaptionMaxWords+1))
	if _, err := CleanCaption(long); err == nil {
		t.Error("a caption past the word bound was accepted")
	}
}

// TestCanvasLineIsAT2IFact: the target-canvas line belongs above the request, and only
// when both sides are known — the caption prompt has no such convention for an edit.
func TestCanvasLineIsAT2IFact(t *testing.T) {
	if got := CanvasLine(1024, 768); got != "TARGET CANVAS: 1024:768" {
		t.Errorf("CanvasLine = %q", got)
	}
	if got := CanvasLine(0, 768); got != "" {
		t.Errorf("CanvasLine with no width = %q, want empty", got)
	}
	msg := CaptionMessage(CaptionRequest{Prompt: "a cat in a hat", Width: 512, Height: 512})
	if msg != "TARGET CANVAS: 512:512\n\na cat in a hat" {
		t.Errorf("CaptionMessage = %q", msg)
	}
	if msg := CaptionMessage(CaptionRequest{Prompt: "a cat"}); msg != "a cat" {
		t.Errorf("CaptionMessage without a size = %q", msg)
	}
}

// TestIsCaptionIsAWordCount is the drift guard: captioning a caption expands an
// expansion. The bound is deliberately a count rather than a guess at style, because a
// count is what can be explained to a user whose prompt was passed through untouched.
func TestIsCaptionIsAWordCount(t *testing.T) {
	below := strings.TrimSpace(strings.Repeat("word ", CaptionMinWords-1))
	at := strings.TrimSpace(strings.Repeat("word ", CaptionMinWords))
	if IsCaption(below) {
		t.Errorf("%d words were treated as a caption", CaptionMinWords-1)
	}
	if !IsCaption(at) {
		t.Errorf("%d words were not treated as a caption", CaptionMinWords)
	}
}

// TestEffectivePromptPrefersTheCaption is what every backend reads: the caption when a
// model wrote one, the user's own words otherwise.
func TestEffectivePromptPrefersTheCaption(t *testing.T) {
	plain := Request{Prompt: "  a cat in a hat  "}
	if got := plain.EffectivePrompt(); got != "a cat in a hat" {
		t.Errorf("EffectivePrompt without a caption = %q", got)
	}
	captioned := Request{Prompt: "a cat in a hat", Caption: "  A ginger cat in a knitted top hat.  "}
	if got := captioned.EffectivePrompt(); got != "A ginger cat in a knitted top hat." {
		t.Errorf("EffectivePrompt with a caption = %q", got)
	}
}
