package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chunkReader hands out its data in fixed-size pieces, so a decoder is tested
// against payloads split at every possible offset rather than only at
// convenient newline boundaries.
type chunkReader struct {
	data []byte
	size int
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.size
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

const sampleSSE = ": keep-alive\n\n" +
	"event: message\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"Hal\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"lo \\\"Welt\\\"\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"reasoning\":\"denke nach\"}}]}\n\n" +
	": another comment\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

// TestSSEReaderSurvivesEveryChunkBoundary feeds the same stream at every chunk
// size and asserts the parsed frames are identical. This is the single most
// important streaming test: naive clients break when a JSON object straddles a
// network read.
func TestSSEReaderSurvivesEveryChunkBoundary(t *testing.T) {
	want := []string{
		`{"choices":[{"delta":{"content":"Hal"}}]}`,
		`{"choices":[{"delta":{"content":"lo \"Welt\""}}]}`,
		`{"choices":[{"delta":{"reasoning":"denke nach"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		`[DONE]`,
	}
	for size := 1; size <= len(sampleSSE)+2; size++ {
		reader := newSSEReader(&chunkReader{data: []byte(sampleSSE), size: size})
		var got []string
		for {
			payload, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("chunk size %d: unexpected error: %v", size, err)
			}
			got = append(got, string(payload))
		}
		if len(got) != len(want) {
			t.Fatalf("chunk size %d: got %d frames, want %d (%v)", size, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("chunk size %d, frame %d:\n got %q\nwant %q", size, i, got[i], want[i])
			}
		}
	}
}

// TestSSEReaderJoinsMultilineData verifies that an event whose payload spans
// several data: lines is reassembled.
func TestSSEReaderJoinsMultilineData(t *testing.T) {
	stream := "data: {\"a\":\ndata: 1}\n\n"
	reader := newSSEReader(strings.NewReader(stream))
	payload, err := reader.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := string(payload), "{\"a\":\n1}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestSSEReaderFlushesFinalEventWithoutBlankLine covers servers that end the
// stream without a trailing blank line.
func TestSSEReaderFlushesFinalEventWithoutBlankLine(t *testing.T) {
	reader := newSSEReader(strings.NewReader("data: {\"final\":true}"))
	payload, err := reader.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(payload) != `{"final":true}` {
		t.Fatalf("got %q", payload)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

// TestNDJSONReaderSurvivesChunkBoundaries does the same for Ollama's format.
func TestNDJSONReaderSurvivesChunkBoundaries(t *testing.T) {
	stream := `{"message":{"content":"Hallo"},"done":false}
{"message":{"content":" Welt"},"done":false}
{"done":true,"eval_count":2,"eval_duration":1000000000}
`
	for size := 1; size <= len(stream)+2; size++ {
		reader := newNDJSONReader(&chunkReader{data: []byte(stream), size: size})
		var contents []string
		done := false
		for {
			var resp ollamaChatResponse
			if err := reader.Next(&resp); err != nil {
				break
			}
			if resp.Message.Content != "" {
				contents = append(contents, resp.Message.Content)
			}
			if resp.Done {
				done = true
				if resp.EvalCount != 2 {
					t.Fatalf("chunk size %d: eval_count = %d, want 2", size, resp.EvalCount)
				}
			}
		}
		if joined := strings.Join(contents, ""); joined != "Hallo Welt" {
			t.Fatalf("chunk size %d: content = %q", size, joined)
		}
		if !done {
			t.Fatalf("chunk size %d: never saw done", size)
		}
	}
}

// TestOpenAICompatStreamParsesDeltasAndStats exercises the whole consumer against
// a real HTTP server, including tool-call fragment assembly and LM Studio stats.
func TestOpenAICompatStreamParsesDeltasAndStats(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"reasoning":"Denke nach. "}}]}`,
		`{"choices":[{"delta":{"content":"Der Vertrag "}}]}`,
		`{"choices":[{"delta":{"content":"gilt ab "}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search_","arguments":"{\"query\":\"Miet"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"web","arguments":"recht\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":9,"total_tokens":51},` +
			`"stats":{"tokens_per_second":31.5,"time_to_first_token":0.25,"generation_time":1.5,"stop_reason":"eosFound"},` +
			`"model_info":{"context_length":48128,"arch":"qwen35","quant":"IQ3_S"}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider := newLMStudioProvider(server.URL, "")
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "qwen3.8-27b-gsq-rco",
		Messages: []Message{{Role: RoleUser, Content: "Test"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		calls     []ToolCall
		usage     *Usage
		stats     *GenerationStats
	)
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventReasoning:
			reasoning.WriteString(ev.Text)
		case EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			stats = ev.Stats
		case EventError:
			t.Fatalf("stream error: %s", ev.Error)
		}
	}

	if got, want := content.String(), "Der Vertrag gilt ab "; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "Denke nach. "; got != want {
		t.Errorf("reasoning = %q, want %q", got, want)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	// The tool name and arguments arrive split across two frames and must be
	// concatenated, not replaced.
	if calls[0].Name != "search_web" {
		t.Errorf("tool name = %q, want search_web", calls[0].Name)
	}
	if calls[0].Arguments != `{"query":"Mietrecht"}` {
		t.Errorf("tool arguments = %q", calls[0].Arguments)
	}
	if usage == nil || usage.PromptTokens != 42 || usage.CompletionTokens != 9 {
		t.Errorf("usage = %+v", usage)
	}
	if stats == nil {
		t.Fatal("expected stats")
	}
	if stats.TokensPerSecond != 31.5 {
		t.Errorf("tokens/s = %v, want 31.5", stats.TokensPerSecond)
	}
	// LM Studio reports these in seconds; they must be converted to milliseconds.
	if stats.TimeToFirstTokenMs != 250 {
		t.Errorf("ttft = %d ms, want 250", stats.TimeToFirstTokenMs)
	}
	if stats.GenerationMs != 1500 {
		t.Errorf("generation = %d ms, want 1500", stats.GenerationMs)
	}
	if stats.Source != SourceLMStudio {
		t.Errorf("source = %q", stats.Source)
	}
}

// TestOpenAICompatToleratesARepeatedToolName covers the other half of tool-call
// streaming: a server that repeats the whole function name in every delta. The
// name is accumulated because it may arrive in fragments, so a repeat used to
// produce "search_websearch_web" and the call no longer resolved to a tool.
func TestOpenAICompatToleratesARepeatedToolName(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"search_web","arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"search_web","arguments":"\"Mietrecht\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider := newLMStudioProvider(server.URL, "")
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "Test"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var calls []ToolCall
	for ev := range events {
		if ev.Type == EventToolCall {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (%+v)", len(calls), calls)
	}
	if calls[0].Name != "search_web" {
		t.Errorf("tool name = %q, want one search_web", calls[0].Name)
	}
	if calls[0].Arguments != `{"query":"Mietrecht"}` {
		t.Errorf("tool arguments = %q", calls[0].Arguments)
	}
}

// TestOpenAICompatReportsATruncatedStream is the regression test for a reply that
// stops mid-answer: the body fails before the stream ends, which used to look
// exactly like the model finishing. Half a reply was then stored as a complete
// one, with the turn reported as a success.
func TestOpenAICompatReportsATruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Promise more than is written: the body then fails while being read
		// instead of ending cleanly, which is how a dropped connection looks.
		w.Header().Set("Content-Length", "4096")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Halbe Ant"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider := newLMStudioProvider(server.URL, "")
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "Test"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		content   strings.Builder
		streamErr string
		sawDone   bool
	)
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventError:
			streamErr = ev.Error
		case EventDone:
			sawDone = true
		}
	}
	if content.String() != "Halbe Ant" {
		t.Errorf("the partial text should still reach the caller, got %q", content.String())
	}
	if streamErr == "" {
		t.Error("a truncated stream must be reported as an error")
	}
	if sawDone {
		t.Error("a truncated stream must not be reported as a finished generation")
	}
}

// TestOllamaReportsATruncatedStream is the same contract for the native Ollama
// provider, which has its own reader.
func TestOllamaReportsATruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Length", "4096")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, `{"model":"m","message":{"content":"Halbe Ant"},"done":false}`+"\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	provider := newOllamaProvider(server.URL)
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "Test"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		content   strings.Builder
		streamErr string
		sawDone   bool
	)
	for ev := range events {
		switch ev.Type {
		case EventDelta:
			content.WriteString(ev.Text)
		case EventError:
			streamErr = ev.Error
		case EventDone:
			sawDone = true
		}
	}
	if content.String() != "Halbe Ant" {
		t.Errorf("the partial text should still reach the caller, got %q", content.String())
	}
	if streamErr == "" {
		t.Error("a truncated stream must be reported as an error")
	}
	if sawDone {
		t.Error("a truncated stream must not be reported as a finished generation")
	}
}

// TestOllamaHonoursDisableThinking pins the summariser's escape from the
// reasoning-budget problem: when a call asks for thinking off, the native
// provider sends Ollama's `think: false` — and when it does not, the field is
// absent so the model's own default applies.
func TestOllamaHonoursDisableThinking(t *testing.T) {
	// capture runs one request against a throwaway server and returns its body.
	capture := func(t *testing.T, req ChatRequest) map[string]any {
		t.Helper()
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			fmt.Fprint(w, `{"model":"m","message":{"content":"ok"},"done":true,"done_reason":"stop"}`+"\n")
		}))
		defer server.Close()
		events, err := newOllamaProvider(server.URL).Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		for range events {
		}
		return body
	}

	off := false
	body := capture(t, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Think:    &off,
	})
	if got, ok := body["think"].(bool); !ok || got {
		t.Errorf("think = %v (present=%v), want the boolean false", body["think"], ok)
	}

	body = capture(t, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if v, ok := body["think"]; ok {
		t.Errorf("think = %v, want the field omitted when the request leaves it to the model", v)
	}
}

// TestLMStudioSendsEnableThinkingFalse pins the LM Studio lever for the
// summariser: LM Studio has no reasoning parameter, but its request accepts the
// in-body `enable_thinking` toggle the Qwen-style chat template reads. Without it
// a reasoning model spends the whole output budget thinking and returns no
// summary at all.
func TestLMStudioSendsEnableThinkingFalse(t *testing.T) {
	if !newLMStudioProvider("http://unused", "").CanToggleThinking() {
		t.Fatal("LM Studio should advertise the thinking toggle")
	}

	capture := func(t *testing.T, req ChatRequest) map[string]any {
		t.Helper()
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer server.Close()
		events, err := newLMStudioProvider(server.URL, "").Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		for range events {
		}
		return body
	}

	off := false
	body := capture(t, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Think:    &off,
	})
	if got, ok := body["enable_thinking"].(bool); !ok || got {
		t.Errorf("enable_thinking = %v (present=%v), want the boolean false", body["enable_thinking"], ok)
	}

	body = capture(t, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if v, ok := body["enable_thinking"]; ok {
		t.Errorf("enable_thinking = %v present without an explicit request", v)
	}
}

// TestOpenAICompatFallsBackToV1On404 verifies the endpoint retirement path for
// LM Studio builds without the native namespace.
func TestOpenAICompatFallsBackToV1On404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newLMStudioProvider(server.URL, "")
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var content strings.Builder
	for ev := range events {
		if ev.Type == EventDelta {
			content.WriteString(ev.Text)
		}
	}
	if content.String() != "ok" {
		t.Fatalf("content = %q", content.String())
	}
	if got := provider.currentChatPath(); !strings.HasSuffix(got, "/v1/chat/completions") {
		t.Fatalf("expected the native path to be retired, still using %q", got)
	}
}

// TestOpenAICompatEstimatesStatsWhenAbsent makes sure the UI never receives a
// fabricated-looking zero: a backend that reports nothing must be labelled
// "estimated".
func TestOpenAICompatEstimatesStatsWhenAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, word := range []string{"ein ", "zwei ", "drei"} {
			payload, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": word}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	provider := newLMStudioProvider(server.URL, "")
	events, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var stats *GenerationStats
	var usage *Usage
	for ev := range events {
		if ev.Type == EventDone {
			stats = ev.Stats
		}
		if ev.Type == EventUsage {
			usage = ev.Usage
		}
	}
	if stats == nil || usage == nil {
		t.Fatal("expected stats and usage")
	}
	if stats.Source != SourceEstimated {
		t.Errorf("source = %q, want %q", stats.Source, SourceEstimated)
	}
	if usage.CompletionTokens == 0 {
		t.Error("expected a non-zero estimated completion token count")
	}
	if stats.TokensPerSecond <= 0 {
		t.Errorf("tokens/s = %v, want > 0", stats.TokensPerSecond)
	}
}

// TestParseOllamaCapabilitiesAndContext checks the namespaced context-length key
// lookup and capability mapping used by /api/show.
func TestOllamaCapabilityMapping(t *testing.T) {
	got := mapOllamaCapabilities([]string{"completion", "tools", "vision", "thinking"})
	want := map[string]bool{CapTools: true, CapVision: true, CapThink: true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, c := range got {
		if !want[c] && c != "completion" {
			t.Errorf("unexpected capability %q", c)
		}
	}
	found := false
	for _, c := range got {
		if c == CapTools {
			found = true
		}
	}
	if !found {
		t.Errorf("tool capability missing from %v", got)
	}
}

// TestEstimateTokensIsMonotonic guards the meter's predicted value against
// jumping around as text is appended.
func TestEstimateTokensIsMonotonic(t *testing.T) {
	samples := []string{
		"Der Vertrag gilt ab dem 01.01.2027 für alle Mieter in Baden-Württemberg.",
		"```go\nfunc main() { fmt.Println(\"hallo\") }\n```",
		"利率与通货膨胀的关系需要进一步分析。",
		strings.Repeat("a", 500),
		"2026-10-06T23:45:00Z https://example.com/pfad?x=1&y=2",
	}
	for _, sample := range samples {
		runes := []rune(sample)
		prev := 0
		// Grow by whole runes: the estimator is never called with a partial
		// multi-byte character in practice, and slicing bytes here would feed it
		// invalid UTF-8 and test nothing real.
		for i := 1; i <= len(runes); i++ {
			got := EstimateTokens(string(runes[:i]))
			if got < prev {
				t.Fatalf("token estimate decreased from %d to %d at rune %d of %q",
					prev, got, i, sample)
			}
			prev = got
		}
		if prev == 0 {
			t.Errorf("no tokens estimated for %q", sample)
		}
	}
	if EstimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
}
