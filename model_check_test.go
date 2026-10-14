package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/ingest"
	"kichat/internal/llm"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// nvidiaBackend is an NVIDIA-shaped backend: a four-field listing that says
// nothing about what a model can do, and a single chat route whose answer depends
// only on which model was asked. That is the service this feature exists for —
// the listing is the global catalogue, and whether a given key may call a model
// is only discoverable by asking.
type nvidiaBackend struct {
	mu       sync.Mutex
	verdicts map[string]int
	hits     map[string]int
}

func newNvidiaBackend(verdicts map[string]int) *nvidiaBackend {
	return &nvidiaBackend{verdicts: verdicts, hits: map[string]int{}}
}

// set changes one model's answer, which is how a sweep is made to differ from the
// last one — an account's tier, or a vendor's capacity, is not permanent.
func (b *nvidiaBackend) set(model string, status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.verdicts[model] = status
}

func (b *nvidiaBackend) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		models := make([]string, 0, len(b.verdicts))
		for id := range b.verdicts {
			models = append(models, id)
		}
		b.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			entries := make([]map[string]any, 0, len(models))
			for _, id := range models {
				publisher, _, _ := strings.Cut(id, "/")
				entries = append(entries, map[string]any{
					"id": id, "object": "model", "created": 735790403, "owned_by": publisher,
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
			status := b.verdicts[body.Model]
			b.hits[body.Model]++
			b.mu.Unlock()
			if status == http.StatusOK {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
				return
			}
			if status == 0 {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newModelCheckApp is an App whose only backend is that fixture, rooted in temp
// directories: no keyring, no real ~/.config/kichat, and initOnce burned so
// ensure never reaches either.
func newModelCheckApp(t *testing.T, baseURL string) *App {
	t.Helper()
	return newModelCheckAppWith(t, llm.BackendConfig{
		ID: "nvidia", Name: "NVIDIA", Kind: "nvidia",
		BaseURL: baseURL, APIKey: "nvapi-test", Enabled: true,
	})
}

// newModelCheckAppWith is the same App with a backend of the caller's choosing,
// so a sweep can be driven against another kind's answers. The spellings that make
// a probe a refusal are per backend — a 404 on one, a 400 only this client can
// provoke on another — and what the check does with them has to hold either way.
func newModelCheckAppWith(t *testing.T, backend llm.BackendConfig) *App {
	t.Helper()
	settings := config.Default()
	settings.AutoTitle = false
	settings.Backends = []llm.BackendConfig{backend}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	secrets, err := secret.NewIsolated(t.TempDir())
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}

	a := newCleanupTestApp(t, settings, t.TempDir())
	a.store, a.extractor, a.secrets = st, ingest.New(), secrets
	a.rebuildServices(settings)
	return a
}

// startSweep runs one availability check for a backend and waits for it to
// finish. The bound call returns while the sweep is still running — a sweep of a
// whole catalogue does not fit in a call — so waiting is what a test does instead
// of the polling the picker does.
func startSweep(t *testing.T, a *App, backend string) ModelCheckStatus {
	t.Helper()
	if _, err := a.StartModelCheck(backend); err != nil {
		t.Fatalf("StartModelCheck: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status := a.GetModelCheckStatus(); !status.Busy {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the sweep never finished")
	return ModelCheckStatus{}
}

// offered is what the picker would show, which is the only list the filter acts on.
func offered(t *testing.T, a *App) map[string]bool {
	t.Helper()
	models, err := a.ListModels(false)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	out := map[string]bool{}
	for _, m := range models {
		out[m.ID] = true
	}
	return out
}

// The whole feature, through the interface the picker uses. build.nvidia.com
// lists models it will refuse with 404 for the key in use — `nemotron-4-340b-reward`
// is the one that was reported — and that refusal is invisible in every metadata
// route, so the only way to keep them out of the picker is to ask each model once.
func TestTheAvailabilityCheckHidesWhatTheAccountCannotCall(t *testing.T) {
	backend := newNvidiaBackend(map[string]int{
		"meta/llama-3.1-8b-instruct":    200,
		"nvidia/nemotron-4-340b-reward": 404,
		"meta/busy-model":               429,
	})
	a := newModelCheckApp(t, backend.start(t).URL)

	// Before the check, all three are offered. That is the state the unfiltered
	// catalogue produces, and the one the check exists to improve.
	if got := offered(t, a); len(got) != 3 {
		t.Fatalf("before the check the picker offers %v, want all three", got)
	}

	status := startSweep(t, a, "nvidia")
	if status.Error != "" {
		t.Fatalf("the sweep reported an error: %s", status.Error)
	}
	if status.Total != 3 || status.Done != 3 {
		t.Errorf("the sweep reported %d of %d models, want all three", status.Done, status.Total)
	}

	got := offered(t, a)
	if got["nvidia/nemotron-4-340b-reward"] {
		t.Error("the model the backend refuses with 404 is still offered")
	}
	if !got["meta/llama-3.1-8b-instruct"] {
		t.Error("the model that answered was hidden as well")
	}
	// The rule that makes an automatic filter safe rather than dangerous: a 429
	// says the backend is busy, not that the model is unusable. Hiding on it would
	// remove a working model until someone noticed.
	if !got["meta/busy-model"] {
		t.Error("a throttled model was hidden")
	}
	if status.HiddenTotal != 1 {
		t.Errorf("hiddenTotal = %d, want the one refused model", status.HiddenTotal)
	}

	// The provider still reports it. Hiding is a statement about the picker, so a
	// chat already pinned to the model keeps resolving it and keeps working.
	reg := map[string]bool{}
	for _, m := range a.currentRegistry().ListModels(context.Background(), false) {
		reg[m.ID] = true
	}
	if !reg["nvidia/nemotron-4-340b-reward"] {
		t.Error("the backend stopped reporting the hidden model, which would break a chat pinned to it")
	}

	// And it is reversible from the UI, because a verdict about an account can
	// expire.
	if _, err := a.ClearHiddenModels("nvidia"); err != nil {
		t.Fatalf("ClearHiddenModels: %v", err)
	}
	if !offered(t, a)["nvidia/nemotron-4-340b-reward"] {
		t.Error("the hidden model did not come back after the blacklist was cleared")
	}
}

// A model that was refused can become reachable — an upgraded account, added
// capacity — so a later sweep has to be able to take an entry off the list. Only a
// sweep that actually reached the model may do it.
func TestASecondSweepOffersAModelThatAnswersNow(t *testing.T) {
	backend := newNvidiaBackend(map[string]int{"meta/good": 200, "meta/later": 404})
	a := newModelCheckApp(t, backend.start(t).URL)

	if status := startSweep(t, a, "nvidia"); status.Error != "" {
		t.Fatalf("first sweep: %s", status.Error)
	}
	if offered(t, a)["meta/later"] {
		t.Fatal("the refused model was not hidden")
	}

	backend.set("meta/later", 200)
	if status := startSweep(t, a, "nvidia"); status.Error != "" {
		t.Fatalf("second sweep: %s", status.Error)
	}
	if !offered(t, a)["meta/later"] {
		t.Error("a model that answers now is still hidden: only a sweep that reached it may decide")
	}
	if status := a.GetModelCheckStatus(); status.HiddenTotal != 0 {
		t.Errorf("hiddenTotal = %d after the model started answering, want 0", status.HiddenTotal)
	}
}

// The guard against the one reading that would be destructive. A wrong address or
// a key without entitlement refuses *every* model, and that is indistinguishable
// per model from a catalogue this account may not call — so a sweep in which
// everything was refused is reported as a problem and applies nothing. Committing
// it would empty the picker over a misconfiguration.
//
// It is the same reasoning as the Health check that runs before the sweep, one
// level down: there a whole backend goes unanswered, here every *model* is refused.
func TestASweepThatRefusesEverythingHidesNothing(t *testing.T) {
	backend := newNvidiaBackend(map[string]int{"a/one": 404, "b/two": 404})
	a := newModelCheckApp(t, backend.start(t).URL)

	status := startSweep(t, a, "nvidia")
	if status.Error == "" {
		t.Error("a sweep in which every model was refused reported no error: that is an " +
			"address or a key, and it must not be applied as a judgement about the models")
	}
	// Reason is the half the UI switches on: this verdict has a sentence per
	// language, and the raw text above deliberately does not.
	if status.Reason != ReasonAllRefused {
		t.Errorf("reason = %q, want %q", status.Reason, ReasonAllRefused)
	}
	if status.HiddenTotal != 0 {
		t.Errorf("hiddenTotal = %d, want nothing hidden", status.HiddenTotal)
	}
	if got := offered(t, a); len(got) != 2 {
		t.Errorf("the picker offers %v, want both models: nothing may be hidden on this evidence", got)
	}
}

// The blacklist has to survive a restart, and the row it survives in is the
// settings blob — the one place a backend key must never be. The sweep writes it
// from its own goroutine rather than through SaveSettings, so it is exactly the
// write that could skip the sanitising step.
func TestTheBlacklistIsStoredWithoutABackendKey(t *testing.T) {
	backend := newNvidiaBackend(map[string]int{"meta/good": 200, "meta/refused": 404})
	a := newModelCheckApp(t, backend.start(t).URL)
	if status := startSweep(t, a, "nvidia"); status.Error != "" {
		t.Fatalf("sweep: %s", status.Error)
	}

	var blob config.Settings
	found, err := a.store.GetSetting("settings", &blob)
	if err != nil || !found {
		t.Fatalf("read the settings row: found=%v err=%v", found, err)
	}
	if len(blob.HiddenModels) != 1 || blob.HiddenModels[0] != "nvidia/meta/refused" {
		t.Errorf("stored blacklist = %v, want the refused model: the verdict has to survive a restart",
			blob.HiddenModels)
	}
	for _, b := range blob.Backends {
		if b.APIKey != "" {
			t.Errorf("the stored settings blob carries a key for %q: secrets live in the secret store, "+
				"and this row is where the blacklist was just written", b.ID)
		}
	}
}

// The panel has no control that edits the blacklist, so no draft it sends carries
// one. SaveSettings reads the recorded list instead, or the first save of an
// unrelated change would put every hidden model back.
func TestAnUnrelatedSaveKeepsTheHiddenModels(t *testing.T) {
	backend := newNvidiaBackend(map[string]int{"meta/good": 200, "meta/refused": 404})
	a := newModelCheckApp(t, backend.start(t).URL)

	if status := startSweep(t, a, "nvidia"); status.Error != "" {
		t.Fatalf("sweep: %s", status.Error)
	}
	if !offered(t, a)["meta/good"] || offered(t, a)["meta/refused"] {
		t.Fatal("the sweep did not hide what it should have")
	}

	// A draft as the bridge hands it out and takes it back: no blacklist in it.
	draft, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	draft.HiddenModels = nil
	draft.AutoTitle = !draft.AutoTitle
	if _, err := a.SaveSettings(draft); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	if got := offered(t, a); got["meta/refused"] {
		t.Error("an unrelated save put the hidden model back into the picker")
	}
	if got := a.GetModelCheckStatus().HiddenTotal; got != 1 {
		t.Errorf("hiddenTotal = %d after the save, want 1", got)
	}
}
