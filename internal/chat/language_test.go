package chat

import (
	"context"
	"strings"
	"testing"

	"kichat/internal/config"
	"kichat/internal/llm"
)

// TestPromptLanguageIsTheAppLanguage pins the one source of the summariser's and
// condenser's language: the setting, with English as the fallback for a value the
// app does not ship. It is deliberately not a guess at the conversation's language.
func TestPromptLanguageIsTheAppLanguage(t *testing.T) {
	cases := map[string]string{
		config.LangGerman:  config.LangGerman,
		config.LangEnglish: config.LangEnglish,
		config.LangChinese: config.LangChinese,
		"":                 config.LangEnglish,
		"fr":               config.LangEnglish,
	}
	for ui, want := range cases {
		if got := promptLanguage(ui); got != want {
			t.Errorf("promptLanguage(%q) = %q, want %q", ui, got, want)
		}
	}
}

// TestSummaryPromptSpeaksTheAppLanguage keeps the contract, its headings and the
// language it names in one language. A German contract over an English transcript is
// what produced a German memory, and the memory rides in message zero.
func TestSummaryPromptSpeaksTheAppLanguage(t *testing.T) {
	cases := []struct {
		lang   string
		marker string
		rule   string
	}{
		{config.LangEnglish, "## Established facts", "Write in English."},
		{config.LangGerman, "## Festgestellte Fakten", "Schreibe auf Deutsch."},
		{config.LangChinese, "## 已确认的事实", "用中文书写。"},
	}
	for _, tc := range cases {
		got := summarySystemPrompt(tc.lang)
		if !strings.Contains(got, tc.marker) || !strings.Contains(got, tc.rule) {
			t.Errorf("summary prompt for %q is missing %q or %q:\n%s", tc.lang, tc.marker, tc.rule, got)
		}
	}
	if strings.Contains(summarySystemPrompt(config.LangEnglish), "Festgestellte") {
		t.Error("the English contract still carries German headings")
	}
}

// TestSummariseWrapperSpeaksTheAppLanguage covers the user message around the
// transcript, which was German before the contract was translated and would otherwise
// leave a German instruction inside an English request.
func TestSummariseWrapperSpeaksTheAppLanguage(t *testing.T) {
	opts := SummarizeOptions{Transcript: "user: hello", Lang: config.LangEnglish}
	if got := summarisePrompt(opts); !strings.Contains(got, "Compact the following transcript") ||
		strings.Contains(got, "Komprimiere") {
		t.Errorf("English wrapper is not English:\n%s", got)
	}
	opts.Lang = config.LangGerman
	if got := summarisePrompt(opts); !strings.Contains(got, "Komprimiere den folgenden Verlauf") {
		t.Errorf("German wrapper lost its German:\n%s", got)
	}
	opts.Lang = config.LangChinese
	if got := summarisePrompt(opts); !strings.Contains(got, "将以下记录压缩") {
		t.Errorf("Chinese wrapper lost its Chinese:\n%s", got)
	}
}

func TestDigestPromptSpeaksTheAppLanguage(t *testing.T) {
	if got := digestSystemPrompt(config.LangEnglish); !strings.Contains(got, "Write in English.") ||
		strings.Contains(got, "Schreibe") {
		t.Errorf("English page contract is not English:\n%s", got)
	}
	if got := digestSystemPrompt(config.LangGerman); !strings.Contains(got, "Schreibe auf Deutsch.") {
		t.Errorf("German page contract lost its German:\n%s", got)
	}
	if got := digestSystemPrompt(config.LangChinese); !strings.Contains(got, "用中文书写。") {
		t.Errorf("Chinese page contract lost its Chinese:\n%s", got)
	}
	if got := readFileSystemPrompt(config.LangEnglish); !strings.Contains(got, "line numbers") ||
		strings.Contains(got, "Zeilennummern") {
		t.Errorf("English file contract is not English:\n%s", got)
	}
	if got := readFileSystemPrompt(config.LangGerman); !strings.Contains(got, "Zeilennummern") {
		t.Errorf("German file contract lost its German:\n%s", got)
	}
	if got := digestPrompt(config.LangEnglish, "text", 600); !strings.Contains(got, "Summarise the following page text") {
		t.Errorf("English page wrapper is not English:\n%s", got)
	}
	if got := digestPrompt(config.LangGerman, "text", 600); !strings.Contains(got, "Fasse den folgenden Seitentext") {
		t.Errorf("German page wrapper lost its German:\n%s", got)
	}
	if got := readFileDigestPrompt(config.LangEnglish, "a.go", "text", 600); !strings.Contains(got, "Summarise the following file content") {
		t.Errorf("English file wrapper is not English:\n%s", got)
	}
	if got := readFileDigestPrompt(config.LangChinese, "a.go", "text", 600); !strings.Contains(got, "总结以下文件内容") {
		t.Errorf("Chinese file wrapper lost its Chinese:\n%s", got)
	}
}

// TestSummarizeSpeaksTheAppLanguage is the option on the wire, not just the builder.
func TestSummarizeSpeaksTheAppLanguage(t *testing.T) {
	provider := &fakeProvider{replies: [][]llm.StreamEvent{{
		{Type: llm.EventDelta, Text: "## Established facts\n- ok"}, {Type: llm.EventDone},
	}}}
	if _, err := Summarize(context.Background(), provider, "fake-model", SummarizeOptions{
		Transcript: "user: hello", MaxTokens: 1200, Lang: config.LangEnglish,
	}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	system := provider.lastRequest().Messages[0].Content
	if !strings.Contains(system, "## Established facts") || !strings.Contains(system, "Write in English.") {
		t.Errorf("the summariser was not given the English contract:\n%s", system)
	}
}
