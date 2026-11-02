package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mlxListing is what `GET /v1/models` answers with, cut down from mlx_lm's
// server.py: a repo id, an object kind and a timestamp, and nothing else. The
// absence is the point — there is no context window and no capability list for
// this provider to map, so the test pins that it invents neither.
const mlxListing = `{"object":"list","data":[
	{"id":"mlx-community/Qwen3-4B-4bit","object":"model","created":1757141666},
	{"id":"/Users/me/models/my-mlx-model","object":"model","created":1757141666}
]}`

func TestMLXListingClaimsNothingItWasNotTold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, mlxListing)
	}))
	defer srv.Close()

	p := newMLXProvider("mlx", "MLX", srv.URL, "")
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("listed %d models, want 2: %+v", len(models), models)
	}
	for _, m := range models {
		if m.Backend != "mlx" {
			t.Errorf("%s backend = %q, want mlx", m.ID, m.Backend)
		}
		// A zero window is what sends the app to the user's override. Any other
		// number here would be this provider promising a window the server never
		// published, which then truncates a prompt from the front.
		if m.ContextLimit != 0 || m.MaxContext != 0 {
			t.Errorf("%s windows = %d/%d, want 0/0: mlx_lm.server publishes none",
				m.ID, m.ContextLimit, m.MaxContext)
		}
		if len(m.Capabilities) != 0 {
			t.Errorf("%s capabilities = %v, want none: a capability is the server's "+
				"claim or it is absent", m.ID, m.Capabilities)
		}
	}
}

// The chat stream carries the three things this provider must not lose: the
// `reasoning` delta (mlx-lm calls thinking that, not reasoning_content), a tool
// call split across two frames, and the usage frame with the cached prefix.
const mlxStream = `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"weigh"}}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":4}}}

data: [DONE]

`

func TestMLXStreamKeepsReasoningToolsAndUsage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mlxStream)
	}))
	defer srv.Close()

	p := newMLXProvider("mlx", "MLX", srv.URL, "")
	events, err := p.Chat(context.Background(), ChatRequest{
		Model:     "mlx-community/Qwen3-4B-4bit",
		Messages:  []Message{{Role: RoleUser, Content: "read notes.md"}},
		MaxTokens: 64,
		Tools: []ToolDefinition{{
			Name: "read_file", Description: "read a file", Schema: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var (
		reasoning  string
		text       string
		tool       *ToolCall
		usage      *Usage
		finishSeen bool
	)
	for ev := range events {
		switch ev.Type {
		case EventReasoning:
			reasoning += ev.Text
		case EventDelta:
			text += ev.Text
		case EventToolCall:
			if ev.ToolCall != nil {
				call := *ev.ToolCall
				tool = &call
			}
		case EventUsage:
			usage = ev.Usage
		case EventDone:
			finishSeen = true
		}
	}
	if reasoning != "weigh" {
		t.Errorf("reasoning = %q, want %q (mlx-lm sends `reasoning`, not `reasoning_content`)", reasoning, "weigh")
	}
	if text != "Hello" {
		t.Errorf("text = %q, want %q", text, "Hello")
	}
	if tool == nil || tool.Name != "read_file" {
		t.Fatalf("tool call = %+v, want read_file", tool)
	}
	if tool.Arguments != `{"path":"notes.md"}` {
		t.Errorf("tool arguments = %q, want the two fragments joined", tool.Arguments)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.CachedPromptTokens != 4 {
		t.Errorf("usage = %+v, want 10/2 with 4 cached", usage)
	}
	if !finishSeen {
		t.Error("the stream ended without a done event")
	}

	// The request asks for what the server needs from us: usage in the stream,
	// which is the only way the context meter gets a measured number.
	opts, _ := body["stream_options"].(map[string]any)
	if include, _ := opts["include_usage"].(bool); !include {
		t.Error("the request did not ask for usage in the stream")
	}
	if body["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v, want 64", body["max_tokens"])
	}
	if _, ok := body["tools"]; !ok {
		t.Error("the request dropped the tools")
	}
}

// A model that cannot load is answered with HTTP 404 and the Python exception's
// text. That is not "the route is missing", and a provider that treated it as one
// would retire the chat path and report a model problem as a URL problem.
func TestMLXLoadFailureKeepsTheServersOwnSentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"ValueError: Model type qwen3 not supported"}`)
	}))
	defer srv.Close()

	p := newMLXProvider("mlx", "MLX", srv.URL, "")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "not-a-model",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a load failure was reported as a successful stream")
	}
	if !strings.Contains(err.Error(), "Model type qwen3 not supported") {
		t.Errorf("error = %v, want the server's own sentence", err)
	}
}
