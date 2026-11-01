package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/workspace"
)

// TestParseTextToolCallsHandlesBothProtocols covers the fallback used by models
// that cannot call functions natively.
func TestParseTextToolCallsHandlesBothProtocols(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantCalls   int
		wantTool    string
		wantArgSub  string
		wantVisible string
	}{
		{
			name:        "fenced tool_call block",
			input:       "Ich suche das.\n\n```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"Mietrecht 2027\"}}\n```",
			wantCalls:   1,
			wantTool:    "search_web",
			wantArgSub:  "Mietrecht 2027",
			wantVisible: "Ich suche das.",
		},
		{
			name:        "fenced json block",
			input:       "```json\n{\"name\":\"scrape_url\",\"arguments\":{\"url\":\"https://example.de\"}}\n```",
			wantCalls:   1,
			wantTool:    "scrape_url",
			wantArgSub:  "example.de",
			wantVisible: "",
		},
		{
			name:        "xml style block",
			input:       "Vorher.\n<tool_call>{\"name\": \"search_documents\", \"arguments\": {\"query\": \"Vertrag\"}}</tool_call>",
			wantCalls:   1,
			wantTool:    "search_documents",
			wantArgSub:  "Vertrag",
			wantVisible: "Vorher.",
		},
		{
			name:        "tool key instead of name",
			input:       "```tool_call\n{\"tool\": \"read_pages\", \"args\": {\"document\": \"bericht.pdf\"}}\n```",
			wantCalls:   1,
			wantTool:    "read_pages",
			wantArgSub:  "bericht.pdf",
			wantVisible: "",
		},
		{
			name:        "no arguments still yields a call",
			input:       "```tool_call\n{\"name\": \"search_web\"}\n```",
			wantCalls:   1,
			wantTool:    "search_web",
			wantArgSub:  "",
			wantVisible: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, visible := ParseTextToolCalls(tc.input)
			if len(calls) != tc.wantCalls {
				t.Fatalf("got %d calls, want %d (%+v)", len(calls), tc.wantCalls, calls)
			}
			if calls[0].Name != tc.wantTool {
				t.Errorf("tool = %q, want %q", calls[0].Name, tc.wantTool)
			}
			if tc.wantArgSub != "" && !strings.Contains(calls[0].Arguments, tc.wantArgSub) {
				t.Errorf("arguments = %q, want them to contain %q", calls[0].Arguments, tc.wantArgSub)
			}
			// The protocol must never reach the user's screen.
			if strings.Contains(visible, "tool_call") || strings.Contains(visible, "```") {
				t.Errorf("protocol leaked into the visible answer: %q", visible)
			}
			if strings.TrimSpace(visible) != tc.wantVisible {
				t.Errorf("visible = %q, want %q", strings.TrimSpace(visible), tc.wantVisible)
			}
		})
	}
}

// TestParseTextToolCallsIgnoresOrdinaryCode makes sure a normal answer that
// happens to contain JSON is not mistaken for a tool request.
func TestParseTextToolCallsIgnoresOrdinaryCode(t *testing.T) {
	inputs := []string{
		"Hier ist ein Beispiel:\n\n```json\n{\"key\": \"value\"}\n```\n\nDas war's.",
		"Nur Text, kein Tool.",
		"```go\nfunc main() {}\n```",
		"```json\n{\"name\": \"nicht-ganz\", \"arguments\": }\n```",
	}
	for _, in := range inputs {
		calls, visible := ParseTextToolCalls(in)
		if len(calls) != 0 {
			t.Errorf("input %q produced a spurious call: %+v", in, calls)
		}
		if visible != in {
			t.Errorf("text was modified despite no tool call:\n got %q\nwant %q", visible, in)
		}
	}
}

func TestParseTextToolCallsHandlesMultipleCalls(t *testing.T) {
	input := "```tool_call\n{\"name\":\"search_web\",\"arguments\":{\"query\":\"a\"}}\n```\n" +
		"```tool_call\n{\"name\":\"scrape_url\",\"arguments\":{\"url\":\"https://b.de\"}}\n```"
	calls, visible := ParseTextToolCalls(input)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Name != "search_web" || calls[1].Name != "scrape_url" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID == calls[1].ID {
		t.Error("tool call IDs must be unique")
	}
	if strings.TrimSpace(visible) != "" {
		t.Errorf("visible = %q, want empty", visible)
	}
}

func TestLooseIntAcceptsStringsAndFloats(t *testing.T) {
	cases := []struct {
		in       any
		fallback int
		want     int
	}{
		{nil, 8, 8},
		{float64(12), 8, 12},
		{12, 8, 12},
		{"5", 8, 5},
		{" 7 ", 8, 7},
		{"nonsense", 8, 8},
		{true, 3, 3},
	}
	for _, tc := range cases {
		if got := looseInt(tc.in, tc.fallback); got != tc.want {
			t.Errorf("looseInt(%#v, %d) = %d, want %d", tc.in, tc.fallback, got, tc.want)
		}
	}
}

// TestDecodeLooseToleratesEmptyAndPartialArguments covers smaller models that
// emit an empty arguments string or a blank object.
func TestDecodeLooseToleratesEmptyAndPartialArguments(t *testing.T) {
	var args searchArgs
	for _, raw := range []string{"", "   ", "{}", `{"query":"x"}`} {
		if err := decodeLoose(raw, &args); err != nil {
			t.Errorf("decodeLoose(%q) failed: %v", raw, err)
		}
	}
	// A count sent as a string is common; it must not fail the whole call.
	args = searchArgs{}
	if err := decodeLoose(`{"query":"x","count":"4"}`, &args); err != nil {
		t.Fatalf("decodeLoose failed: %v", err)
	}
	if args.Query != "x" || looseInt(args.Count, 8) != 4 {
		t.Errorf("args = %+v", args)
	}
}

func TestDefinitionsAreValidJSONSchemaShaped(t *testing.T) {
	svc := &Service{}
	defs := svc.Definitions()
	if len(defs) < 4 {
		t.Fatalf("expected at least 4 tools, got %d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		if d.Name == "" {
			t.Error("tool without a name")
		}
		if names[d.Name] {
			t.Errorf("duplicate tool name %q", d.Name)
		}
		names[d.Name] = true
		if d.Description == "" {
			t.Errorf("tool %q has no description; the model cannot use it well", d.Name)
		}
		// The schema must survive JSON encoding because it is sent over the wire.
		raw, err := json.Marshal(d.Schema)
		if err != nil {
			t.Errorf("tool %q schema does not encode: %v", d.Name, err)
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Errorf("tool %q schema does not round-trip: %v", d.Name, err)
		}
		if decoded["type"] != "object" {
			t.Errorf("tool %q schema type = %v, want object", d.Name, decoded["type"])
		}
		if _, ok := decoded["properties"].(map[string]any); !ok {
			t.Errorf("tool %q schema has no properties", d.Name)
		}
	}
	for _, want := range []string{ToolSearchWeb, ToolScrapeURL, ToolSearchDocs, ToolReadPages} {
		if !names[want] {
			t.Errorf("tool %q is missing from the definitions", want)
		}
	}
}

// TestTextProtocolHintIsSyntaxOnly pins the split that the withheld-tool bug needed:
// the constant carries the block syntax and nothing about which tools exist, because
// that is a property of the request (`TextProtocolTools`).
func TestTextProtocolHintIsSyntaxOnly(t *testing.T) {
	if !strings.Contains(TextProtocolHint(config.LangEnglish), "```tool_call") {
		t.Error("the hint must show the exact block syntax the parser expects")
	}
	for _, name := range []string{ToolSearchWeb, ToolScrapeURL, ToolSearchDocs, ToolReadPages} {
		if strings.Contains(TextProtocolHint(config.LangEnglish), name) {
			t.Errorf("the syntax hint names %q; which tools exist is the request's answer", name)
		}
	}
}

// TestTextProtocolToolsListsOnlyWhatIsOffered is the fix for a hint that promised tools
// the engine had withheld: the model called one, and the only possible answer was an
// error. The list is rendered from the definitions the request declares, so a tool that
// was filtered out cannot appear.
func TestTextProtocolToolsListsOnlyWhatIsOffered(t *testing.T) {
	service := &Service{}
	all := TextProtocolTools(config.LangEnglish, service.Definitions())
	for _, name := range []string{ToolSearchWeb, ToolScrapeURL, ToolSearchDocs, ToolReadPages} {
		if !strings.Contains(all, name) {
			t.Errorf("the rendered hint does not mention %q:\n%s", name, all)
		}
	}
	// The arguments come from the schema, required first and optional marked.
	if !strings.Contains(all, `"query": string`) {
		t.Errorf("required arguments are not rendered:\n%s", all)
	}
	if !strings.Contains(all, `"count"?: integer`) {
		t.Errorf("optional arguments are not marked with ?:\n%s", all)
	}

	// The withheld case: the same renderer, one tool fewer, and the missing one is absent.
	filtered := make([]llm.ToolDefinition, 0, len(service.Definitions()))
	for _, def := range service.Definitions() {
		if def.Name != ToolSearchDocs {
			filtered = append(filtered, def)
		}
	}
	without := TextProtocolTools(config.LangEnglish, filtered)
	if strings.Contains(without, ToolSearchDocs) {
		t.Errorf("a tool that is not offered is still promised:\n%s", without)
	}
	if !strings.Contains(without, ToolSearchWeb) {
		t.Errorf("the remaining tools are not listed:\n%s", without)
	}

	// No tools at all: no line, so the prompt does not promise a capability it lacks.
	if got := TextProtocolTools(config.LangEnglish, nil); got != "" {
		t.Errorf("an empty tool set rendered %q, want no line at all", got)
	}
}

// TestOneResultCannotExceedTheCap is the bound every tool passes through, whichever tool it
// was: a result that is too big is paid for again in every later round of the reply and every
// later turn of the chat, so no single call may produce one.
func TestOneResultCannotExceedTheCap(t *testing.T) {
	s := &Service{}
	const head, tail = "START-OF-RESULT", "END-OF-RESULT"
	body := head + strings.Repeat("m", 4*MaxToolResultBytes) + tail

	got := s.limitResult(body)
	if len(got) > MaxToolResultBytes {
		t.Fatalf("a result came out at %d bytes, the cap is %d", len(got), MaxToolResultBytes)
	}
	// Both ends survive: they are what a model uses — a read's header and continuation note,
	// a command's first error and its last line.
	if !strings.HasPrefix(got, head) {
		t.Error("the beginning of the result was cut")
	}
	if !strings.HasSuffix(got, tail) {
		t.Error("the end of the result was cut")
	}
	// The cut says what happened at the place it happened, so a model can tell a whole result
	// from a shortened one instead of building on what it thinks it read.
	if !strings.Contains(got, "was left out") {
		t.Errorf("the result does not say it was cut:\n%q", got[max(0, len(got)/2-200):len(got)/2+200])
	}
	// The note is the only thing between the ends: nearly the whole cap is still payload.
	if len(got) < MaxToolResultBytes*9/10 {
		t.Errorf("the cut kept only %d of %d bytes", len(got), MaxToolResultBytes)
	}
	// And the note names the amount it was given, rather than saying "truncated".
	if note := elisionNote(config.LangEnglish, 123456); !strings.Contains(note, humanBytes(123456)) {
		t.Errorf("the note does not name the size it was given: %q", note)
	}
	// A result inside the cap is untouched, byte for byte.
	if small := strings.Repeat("x", MaxToolResultBytes); s.limitResult(small) != small {
		t.Error("a result exactly at the cap was rewritten")
	}
	if small := "short enough"; s.limitResult(small) != small {
		t.Error("a small result was rewritten")
	}
}

// TestTheCutDoesNotBreakACharacter keeps the payload valid UTF-8: the result travels in an
// event whose payload is a string, and a card that renders a half character shows a
// replacement mark in the middle of a word.
func TestTheCutDoesNotBreakACharacter(t *testing.T) {
	s := &Service{}
	// Every character is three bytes, so a cut at the middle of the body lands inside one.
	body := strings.Repeat("日", MaxToolResultBytes)
	got := s.limitResult(body)
	if !utf8.ValidString(got) {
		t.Error("the cut left an incomplete character in the result")
	}
	if len(got) > MaxToolResultBytes {
		t.Errorf("result is %d bytes, the cap is %d", len(got), MaxToolResultBytes)
	}
}

// TestTheReadWindowFitsUnderTheCap ties the two numbers together: a read carries a header and
// a continuation note around its text, and a window that reached the cap itself would have
// that framing cut off — leaving a model told to continue from a line it never saw.
func TestTheReadWindowFitsUnderTheCap(t *testing.T) {
	const framing = 2 << 10
	if workspace.MaxReadBytes+framing > MaxToolResultBytes {
		t.Fatalf("a read window of %d bytes leaves no room under the %d-byte result cap for its framing",
			workspace.MaxReadBytes, MaxToolResultBytes)
	}
}
