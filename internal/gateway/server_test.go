package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/llm"
)

type fakeProvider struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
	reply    func(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error)
}

func (*fakeProvider) ID() string                                          { return "fake" }
func (*fakeProvider) Name() string                                        { return "fake" }
func (*fakeProvider) Health(context.Context) error                        { return nil }
func (*fakeProvider) ListModels(context.Context) ([]llm.ModelInfo, error) { return nil, nil }
func (p *fakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if p.reply != nil {
		return p.reply(ctx, req)
	}
	return events(llm.StreamEvent{Type: llm.EventDelta, Text: "回答"}, llm.StreamEvent{Type: llm.EventDone}), nil
}
func events(es ...llm.StreamEvent) <-chan llm.StreamEvent {
	c := make(chan llm.StreamEvent, len(es))
	for _, e := range es {
		c <- e
	}
	close(c)
	return c
}
func setup(p *fakeProvider) (*Server, *Options) {
	o := &Options{Key: "test-key", Model: "upstream", Scope: "test", ContextLimit: 8192, ReserveTokens: 512, Compact: true}
	return New(func(context.Context) (llm.Provider, Options, error) { return p, *o, nil }), o
}
func send(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestClientToolsRoundTrip(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[streaming], func(t *testing.T) {
			p := &fakeProvider{}
			p.reply = func(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
				if len(req.Messages) == 1 {
					return events(
						llm.StreamEvent{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "player_state", Arguments: "{}"}},
						llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{PromptTokens: 80, CompletionTokens: 10, TotalTokens: 90}},
						llm.StreamEvent{Type: llm.EventDone}), nil
				}
				return events(llm.StreamEvent{Type: llm.EventDelta, Text: "好感 42"}, llm.StreamEvent{Type: llm.EventDone}), nil
			}
			s, _ := setup(p)
			body := `{"model":"kichat","stream":STREAM,"stream_options":{"include_usage":true},"temperature":0,"messages":[{"role":"user","content":"現在呢？"}],"tools":[{"type":"function","function":{"name":"player_state","parameters":{"type":"object"}}}]}`
			body = strings.Replace(body, "STREAM", map[bool]string{false: "false", true: "true"}[streaming], 1)
			w := send(s, body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"tool_calls"`) || !strings.Contains(w.Body.String(), `"player_state"`) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if streaming && (!strings.Contains(w.Body.String(), "data: [DONE]") || !strings.Contains(w.Body.String(), `"prompt_tokens":80`)) {
				t.Fatal(w.Body.String())
			}
			if !p.requests[0].TemperatureSet || p.requests[0].Temperature != 0 || len(p.requests[0].Tools) != 1 {
				t.Fatal("lost client settings or tools")
			}
			if got := s.Status().Last; !got.UsageReported || got.PromptTokens != 80 {
				t.Fatalf("report: %+v", got)
			}
			w = send(s, `{"model":"kichat","messages":[{"role":"user","content":"現在呢？"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"player_state","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"好感 42"}]}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "好感 42") || len(p.requests) != 2 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if got := p.requests[1].Messages[2]; got.Role != llm.RoleTool || got.ToolCallID != "c1" || got.Content != "好感 42" {
				t.Fatalf("lost result: %+v", got)
			}
		})
	}
}

func TestRejectsInvalidRequestsBeforeGeneration(t *testing.T) {
	p := &fakeProvider{}
	s, _ := setup(p)
	for _, body := range []string{
		`{"model":"kichat","messages":[]}`,
		`{"model":"kichat","messages":[{"role":"tool","tool_call_id":"missing","content":"x"}]}`,
		`{"model":"kichat","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"script","arguments":"{}"}}]}]}`,
		`{"model":"kichat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		`{"model":"kichat","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
	} {
		if w := send(s, body); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r.Header.Set("Authorization", "test-key")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("accepted key without Bearer scheme")
	}
	r.Header.Set("Authorization", "Bearer test-key")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"kichat"`) {
		t.Fatal(w.Body.String())
	}
	if len(p.requests) != 0 {
		t.Fatal("invalid request reached provider")
	}
}

func TestCompactionProtectsCurrentEvidenceAndDoesNotResurrectMemory(t *testing.T) {
	p := &fakeProvider{reply: func(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
		return events(llm.StreamEvent{Type: llm.EventDelta, Text: "Earlier: affection was 10."}, llm.StreamEvent{Type: llm.EventDone}), nil
	}}
	s, opt := setup(p)
	req := llm.ChatRequest{Model: "upstream", Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "persona"}, {Role: llm.RoleUser, Content: strings.Repeat("old dialogue ", 5000)},
		{Role: llm.RoleAssistant, Content: "old reply"}, {Role: llm.RoleUser, Content: "current question"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "now", Name: "player_state", Arguments: "{}"}}},
		{Role: llm.RoleTool, ToolCallID: "now", Content: "current affection 42"},
	}}
	var report Report
	got, err := s.prepare(context.Background(), p, req, *opt, &report)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Compacted || report.After >= report.Before {
		t.Fatalf("%+v", report)
	}
	if !reflect.DeepEqual(got.Messages[0], req.Messages[0]) || !reflect.DeepEqual(got.Messages[len(got.Messages)-3:], req.Messages[3:]) {
		t.Fatal("modified protected evidence")
	}
	n := len(p.requests)
	report = Report{}
	if _, err = s.prepare(context.Background(), p, req, *opt, &report); err != nil || !report.CacheHit || len(p.requests) != n {
		t.Fatalf("cache: %+v %v", report, err)
	}
	req.Messages[0].Content = "different persona"
	report = Report{}
	if _, err = s.prepare(context.Background(), p, req, *opt, &report); err != nil || report.CacheHit || len(p.requests) == n {
		t.Fatalf("stale summary reused: %v", err)
	}
	req.Messages = []llm.Message{{Role: llm.RoleSystem, Content: "persona"}, {Role: llm.RoleUser, Content: "loaded a different save"}}
	report = Report{}
	got, err = s.prepare(context.Background(), p, req, *opt, &report)
	if err != nil || !reflect.DeepEqual(got.Messages, req.Messages) {
		t.Fatal("history from previous save was injected")
	}
}

func TestOversizedCurrentTurnFailsWithoutSummarisingIt(t *testing.T) {
	p := &fakeProvider{}
	s, opt := setup(p)
	req := llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: strings.Repeat("current evidence ", 10000)}}}
	var report Report
	if _, err := s.prepare(context.Background(), p, req, *opt, &report); err == nil {
		t.Fatal("accepted oversized current turn")
	}
	if len(p.requests) != 0 {
		t.Fatal("summarised current turn")
	}
}

func TestCurrentReasoningMustFitBeforeCallingTheProvider(t *testing.T) {
	p := &fakeProvider{}
	s, opt := setup(p)
	opt.Compact = false
	request := `{"model":"kichat","messages":[{"role":"user","content":"continue"},{"role":"assistant","content":"answer","reasoning_content":"` + strings.Repeat("reasoning ", 4000) + `"}]}`
	w := send(s, request)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "context") {
		t.Fatalf("oversized reasoning was not rejected: %d %s", w.Code, w.Body)
	}
	if len(p.requests) != 0 {
		t.Fatal("oversized reasoning reached the provider")
	}
}

func TestSummaryBatchesConsumeAllUnicodeSource(t *testing.T) {
	var observed strings.Builder
	p := &fakeProvider{reply: func(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
		text := req.Messages[len(req.Messages)-1].Content
		start := strings.Index(text, "<conversation>\n")
		end := strings.LastIndex(text, "\n</conversation>")
		if start < 0 || end < start {
			t.Fatal("summary request did not contain the source")
			return nil, nil
		}
		observed.WriteString(text[start+len("<conversation>\n") : end])
		return events(llm.StreamEvent{Type: llm.EventDelta, Text: "A bounded memory."}, llm.StreamEvent{Type: llm.EventDone}), nil
	}}
	source := strings.Repeat("人物：這段證據有分支條件。\n", 3000)
	if _, err := summarise(context.Background(), p, "upstream", 8192, source); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 || observed.String() != source {
		t.Fatal("summary batching clipped or changed source text")
	}
}

func TestDisconnectAndStopCancelGeneration(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "stop"}[stop], func(t *testing.T) {
			started, cancelled := make(chan struct{}), make(chan struct{})
			p := &fakeProvider{reply: func(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
				c := make(chan llm.StreamEvent)
				go func() {
					defer close(c)
					c <- llm.StreamEvent{Type: llm.EventDelta, Text: "start"}
					close(started)
					<-ctx.Done()
					close(cancelled)
				}()
				return c, nil
			}}
			s, _ := setup(p)
			if err := s.Start(0); err != nil {
				t.Fatal(err)
			}
			defer s.Stop()
			r, _ := http.NewRequest("POST", s.Status().URL+"/chat/completions", strings.NewReader(`{"model":"kichat","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Authorization", "Bearer test-key")
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-started
			if stop {
				if err := s.Stop(); err != nil {
					t.Fatal(err)
				}
			} else {
				resp.Body.Close()
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream was not cancelled")
			}
		})
	}
}

func TestStreamErrorIsNotSuccessfulCompletion(t *testing.T) {
	p := &fakeProvider{reply: func(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
		return events(llm.StreamEvent{Type: llm.EventDelta, Text: "partial"}, llm.StreamEvent{Type: llm.EventError, Error: "failed"}), nil
	}}
	s, _ := setup(p)
	w := send(s, `{"model":"kichat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "[DONE]") || s.Status().Last.Error == "" {
		t.Fatal(w.Body.String())
	}
}

func TestResponseIsValidJSON(t *testing.T) {
	s, _ := setup(&fakeProvider{})
	w := send(s, `{"model":"kichat","messages":[{"role":"user","content":"hi"}]}`)
	var v map[string]any
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(w.Body); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredAliasIsListedValidatedAndReturned(t *testing.T) {
	p := &fakeProvider{}
	s, opt := setup(p)
	opt.ClientModel = "qwen4b-q4"
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"qwen4b-q4"`) {
		t.Fatalf("model listing: %d %s", w.Code, w.Body)
	}
	w = send(s, `{"model":"kichat","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 404 || len(p.requests) != 0 {
		t.Fatalf("old alias accepted: %d %s", w.Code, w.Body)
	}
	for _, stream := range []string{"false", "true"} {
		w = send(s, `{"model":"qwen4b-q4","stream":`+stream+`,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"qwen4b-q4"`) {
			t.Fatalf("response: %d %s", w.Code, w.Body)
		}
		if p.requests[len(p.requests)-1].Model != "upstream" {
			t.Fatal("public alias replaced the actual upstream model")
		}
	}
	opt.ClientModel = "renamed"
	w = send(s, `{"model":"qwen4b-q4","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 404 || len(p.requests) != 2 {
		t.Fatal("alias setting did not take effect")
	}
}

// Exercise the actual OpenAI-compatible provider as well as the public server:
// a Mod receives a split tool call, executes it, and sends the result back.
func TestOpenAIProviderBridgeWithClientExecutedTool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model    string            `json:"model"`
			Messages []wireMessage     `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", 400)
			return
		}
		if body.Model != "qwen-test" {
			t.Error("model alias was not resolved")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{}
		if len(body.Messages) == 1 {
			if len(body.Tools) != 1 {
				t.Error("client tool schema was dropped")
			}
			frames = []string{
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"state-1","type":"function","function":{"name":"player_state","arguments":"{"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`,
			}
		} else {
			last := body.Messages[len(body.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "state-1" || string(last.Content) != `"好感 42"` {
				t.Errorf("lost client evidence: %+v", last)
			}
			frames = []string{`{"choices":[{"index":0,"delta":{"content":"可以繼續"},"finish_reason":"stop"}]}`}
		}
		for _, frame := range frames {
			if _, err := io.WriteString(w, "data: "+frame+"\n\n"); err != nil {
				t.Error(err)
				return
			}
		}
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	registry := llm.NewRegistry([]llm.BackendConfig{{ID: "local", Kind: "custom", BaseURL: upstream.URL, Enabled: true}})
	p, ok := registry.Provider("local")
	if !ok {
		t.Fatal("missing provider")
	}
	s := New(func(context.Context) (llm.Provider, Options, error) {
		return p, Options{Key: "test-key", Model: "qwen-test", ContextLimit: 8192, ReserveTokens: 512}, nil
	})
	w := send(s, `{"model":"kichat","stream":true,"messages":[{"role":"user","content":"我現在能繼續嗎？"}],"tools":[{"type":"function","function":{"name":"player_state","parameters":{"type":"object"}}}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"arguments":"{}"`) || !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = send(s, `{"model":"kichat","messages":[{"role":"user","content":"我現在能繼續嗎？"},{"role":"assistant","content":null,"tool_calls":[{"id":"state-1","type":"function","function":{"name":"player_state","arguments":"{}"}}]},{"role":"tool","tool_call_id":"state-1","content":"好感 42"}]}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "可以繼續") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
