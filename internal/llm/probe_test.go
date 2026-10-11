package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// probeFixture answers one chat request per model the way build.nvidia.com does:
// it streams for a model this account may call, and refuses with the status that
// says which kind of refusal it is.
type probeFixture struct {
	mu       sync.Mutex
	asked    []string
	attempts map[string]int
	bodies   []map[string]any
}

func (f *probeFixture) counts() (asked []string, attempts map[string]int, bodies []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	attempts = make(map[string]int, len(f.attempts))
	for k, v := range f.attempts {
		attempts[k] = v
	}
	return append([]string(nil), f.asked...), attempts, append([]map[string]any(nil), f.bodies...)
}

func (f *probeFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)

		f.mu.Lock()
		f.asked = append(f.asked, model)
		if f.attempts == nil {
			f.attempts = map[string]int{}
		}
		f.attempts[model]++
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()

		switch model {
		case "good":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "not-entitled":
			// The shape the live service uses for a model the account may not
			// call: 404 with application/problem+json and a detail naming the
			// account. 45 of the 80 models answered like this.
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":404,"title":"Not Found","detail":"Function '8f1e': Not found for account 'abc'"}`)
		case "no-chat-route":
			// The other 404: bare text, i.e. this model has no chat route at all.
			// Nine models answer like this (the embed/retrieve entries, and
			// bigcode/starcoder2-15b), and a filter has to treat it as "not for
			// chat" rather than as an unknown.
			http.NotFound(w, r)
		case "throttled":
			w.WriteHeader(http.StatusTooManyRequests)
		case "broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "slow":
			// Never answers. Six of the live catalogue's models never answered
			// within 90 seconds, which is what the per-model timeout is for.
			<-r.Context().Done()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The distinction the whole feature rests on: a refusal is evidence about the
// model, and a throttle, a server error or a timeout is not. Treating any of the
// latter as a verdict would blacklist a working model because the backend
// hiccuped, and the list would then be wrong until someone remembered to undo it.
func TestProbeSeparatesARefusalFromATemporaryFailure(t *testing.T) {
	fixture := &probeFixture{}
	srv := fixture.serve(t)
	// The vendor's own provider, so the single chat path and the key requirement
	// are the real ones: a 404 here is a model, never a route.
	provider := newNvidiaProvider("nvidia", "NVIDIA", srv.URL+"/v1", "nvapi-test")

	models := []ModelInfo{
		{ID: "good"}, {ID: "not-entitled"}, {ID: "no-chat-route"},
		{ID: "throttled"}, {ID: "broken"}, {ID: "slow"},
	}
	// The per-model budget has to outlast the provider's own retry of a retryable
	// status (three attempts, 250 ms + 500 ms of waiting), or the classification
	// under test would race the deadline: only the never-answering model is meant
	// to come out as a timeout.
	probes := ProbeModels(context.Background(), provider, models, ProbeOptions{
		Workers:  6,
		PerModel: 1200 * time.Millisecond,
	}, nil)

	want := map[string]string{
		"good":          ProbeOK,
		"not-entitled":  ProbeRefused,
		"no-chat-route": ProbeRefused,
		"throttled":     ProbeInconclusive,
		"broken":        ProbeInconclusive,
		"slow":          ProbeTimeout,
	}
	for i, p := range probes {
		if p.Model != models[i].ID {
			t.Errorf("probe %d = %q, want %q: the results have to keep the listing's order",
				i, p.Model, models[i].ID)
		}
		if got := p.Kind; got != want[p.Model] {
			t.Errorf("%s: kind = %q, want %q (detail %q)", p.Model, got, want[p.Model], p.Detail)
		}
	}

	refused := 0
	for _, p := range probes {
		if p.Refused() {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("refused = %d, want only the two 404s: a throttled or broken backend is not "+
			"a statement about the model", refused)
	}

	// The cost contract. One token is what makes a sweep of a whole catalogue
	// affordable, and a refusal that was retried would be a second request for an
	// answer that cannot change.
	asked, attempts, bodies := fixture.counts()
	if len(asked) == 0 {
		t.Fatal("nothing was asked: the provider never reached the fixture")
	}
	for _, body := range bodies {
		if got, _ := body["max_tokens"].(float64); got != 1 {
			t.Errorf("max_tokens = %v, want 1: the probe asks for the smallest answer there is", body["max_tokens"])
		}
	}
	if n := attempts["good"]; n != 1 {
		t.Errorf("the answering model was asked %d times, want once", n)
	}
	if n := attempts["not-entitled"]; n != 1 {
		t.Errorf("a refused model was asked %d times, want once: a 404 is not retryable", n)
	}
	if n := attempts["throttled"]; n < 2 {
		t.Errorf("a throttled model was asked %d times, want the provider's own retries: "+
			"it is retryable, which is exactly why it must not be read as a refusal", n)
	}
}

// A sweep that was stopped before it asked anything must not look like a verdict.
// The models it never reached carry no kind at all, which is what keeps a
// cancelled check from emptying the picker.
func TestAStoppedProbeIsNotAVerdict(t *testing.T) {
	fixture := &probeFixture{}
	srv := fixture.serve(t)
	provider := newNvidiaProvider("nvidia", "NVIDIA", srv.URL+"/v1", "nvapi-test")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	probes := ProbeModels(ctx, provider, []ModelInfo{{ID: "good"}, {ID: "not-entitled"}}, ProbeOptions{}, nil)
	for _, p := range probes {
		if p.Kind != "" {
			t.Errorf("%s: kind = %q, want no verdict for a model that was never asked", p.Model, p.Kind)
		}
		if p.Refused() {
			t.Errorf("%s was reported as refused without being asked", p.Model)
		}
	}
	if asked, _, _ := fixture.counts(); len(asked) != 0 {
		t.Errorf("a cancelled sweep sent %d requests: %v", len(asked), asked)
	}
}
