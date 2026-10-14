package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kichat/internal/llm"
)

// groqCheckBackend is a Groq-shaped backend: the listing publishes each model's
// window, modalities and features, so the provider has to keep the two
// classification rows the vendor lists as chat models, and the chat route refuses
// them only because this client always streams.
//
// The 400 below is the vendor's, word for word: measured, `llama-prompt-guard-2-*`
// answers a call *without* `stream` with HTTP 200 and a score, and refuses the
// streaming one with "text classification models do not support streaming".
type groqCheckBackend struct {
	// classifier is the model the chat route refuses with the streaming 400.
	classifier string
	// chat is the model that answers.
	chat string
	// hits is written by the server goroutines, so it is guarded: the sweep runs
	// its requests concurrently, and an unsynchronised map here would fail only
	// under `go test -race` on the run that happened to overlap.
	mu   sync.Mutex
	hits map[string]int
}

// asked reports whether a model was driven, once the sweep has finished.
func (b *groqCheckBackend) asked(model string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits[model]
}

func (b *groqCheckBackend) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			entries := make([]map[string]any, 0, 3)
			for _, spec := range []struct {
				id         string
				features   []string
				modalities []string
			}{
				{b.chat, []string{"tools", "json_mode", "structured_outputs", "reasoning"}, []string{"text"}},
				{b.classifier, nil, []string{"text"}},
				// Dropped by the provider on its own `output_modalities`, so the
				// fixture also shows that a sweep is never asked about it.
				{"whisper-large-v3", nil, []string{"transcription"}},
			} {
				entries = append(entries, map[string]any{
					"id": spec.id, "object": "model", "owned_by": "OpenAI", "active": true,
					"context_window": 131072, "input_modalities": []string{"text"},
					"output_modalities": spec.modalities, "supported_features": spec.features,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.mu.Lock()
			b.hits[body.Model]++
			b.mu.Unlock()
			if body.Model == b.classifier {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"message":"text classification models do not support streaming","type":"invalid_request_error"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The picker's way out of the one row the Groq provider deliberately keeps.
//
// `fromGroqEntry` offers the two prompt-guard classifiers because nothing the
// listing publishes says what they are, and a filter written here would be a rule
// about their ids (GROQ.md §3). What makes that acceptable is this: the app's own
// check asks every model once with the streaming call the chat makes, and the
// vendor's 400 — a non-retryable status — is the one verdict `applyModelProbes`
// acts on. So the rows leave the picker on the API's own answer instead of on a
// guess, and this test is what keeps the two halves from drifting apart: a probe
// that started treating 400 as inconclusive would leave the user with two rows that
// can only fail and no way to clear them.
func TestTheAvailabilityCheckHidesGroqsStreamingRefusal(t *testing.T) {
	backend := &groqCheckBackend{classifier: "meta-llama/llama-prompt-guard-2-22m", chat: "openai/gpt-oss-20b", hits: map[string]int{}}
	a := newModelCheckAppWith(t, llm.BackendConfig{
		ID: "groq", Name: "Groq", Kind: "groq",
		BaseURL: backend.start(t).URL + "/openai/v1", APIKey: "gsk-test", Enabled: true,
	})

	// Before the check, both chat and classifier rows are offered — the classifier
	// because the listing gives no reason to drop it, the speech model never
	// because its output modalities name no text.
	before := offered(t, a)
	if !before["openai/gpt-oss-20b"] || !before["meta-llama/llama-prompt-guard-2-22m"] {
		t.Fatalf("before the check the picker offers %v, want the chat model and the classifier", before)
	}
	if before["whisper-large-v3"] {
		t.Fatal("the transcription model was offered before any check ran")
	}

	status := startSweep(t, a, "groq")
	if status.Error != "" {
		t.Fatalf("the sweep reported an error: %s", status.Error)
	}
	// Both models the provider keeps are asked; the dropped one is not, because a
	// model that never reaches the picker must not spend a request either.
	if status.Total != 2 || status.Done != 2 {
		t.Errorf("the sweep reported %d of %d models, want the two chat rows", status.Done, status.Total)
	}
	if backend.asked("whisper-large-v3") != 0 {
		t.Error("the sweep drove a model the listing itself says is not a chat model")
	}

	after := offered(t, a)
	if after["meta-llama/llama-prompt-guard-2-22m"] {
		t.Error("the model whose streaming call the vendor refuses is still offered")
	}
	if !after["openai/gpt-oss-20b"] {
		t.Error("the model that answered was hidden as well: a refusal has to be per model")
	}
	if status.HiddenTotal != 1 {
		t.Errorf("hiddenTotal = %d, want the one refused model", status.HiddenTotal)
	}

	// Reversible from the UI, like any other verdict about a model: an API can be
	// changed under the app, and a blacklist nobody can clear would outlive it.
	if _, err := a.ClearHiddenModels("groq"); err != nil {
		t.Fatalf("ClearHiddenModels: %v", err)
	}
	if !offered(t, a)["meta-llama/llama-prompt-guard-2-22m"] {
		t.Error("the classifier did not come back after the blacklist was cleared")
	}
}
