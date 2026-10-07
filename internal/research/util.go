package research

import (
	"html"
	"regexp"
	"strings"
)

var (
	whitespaceRun = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankLines    = regexp.MustCompile(`\n{3,}`)
	htmlTag       = regexp.MustCompile(`<[^>]*>`)
)

// cleanText normalises whitespace, strips markup and unescapes HTML entities so
// snippets and titles are safe to drop straight into a prompt. Search providers
// routinely return <strong> highlights in descriptions, which would otherwise be
// fed to the model as noise.
func cleanText(s string) string {
	// Strip tags before unescaping: that way an escaped "&lt;b&gt;" in the source
	// stays literal text instead of being mistaken for markup.
	s = htmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = whitespaceRun.ReplaceAllString(s, " ")
	s = blankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func cleanTextSlice(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if c := cleanText(s); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncate cuts a string to at most n runes, appending an ellipsis when it had
// to cut. Rune-aware so multi-byte German text is never split mid-character.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
