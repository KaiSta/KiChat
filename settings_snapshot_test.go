package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"kichat/internal/config"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
)

// TestSavingAModelWhileTheStateCallbackSavesIsNotARace is the reported race, driven at the
// two ends that produce it rather than through a real llama-server: the window is what
// matters, and calling the callback in a loop is what makes that window repeatable.
//
// Both ends are ordinary ones. `SetManagedModel` writes an entry into the map inside the
// settings value (`Runtime.Models`) and then persists the settings; a managed server
// publishing the address it came up on calls `onManagedState`, which persists the settings
// too. Persisting marshals the *whole* settings value, so as soon as either side hands out a
// shallow copy — which is what `settings := a.settings` is — one goroutine marshals the live
// map while the other writes it. Under `-race` that is a reported race; without it, it is a
// settings blob written from a map another goroutine is halfway through updating.
func TestSavingAModelWhileTheStateCallbackSavesIsNotARace(t *testing.T) {
	settings := config.Default()
	settings.Runtime.Models = map[string]config.ManagedModel{
		"llamacpp": {Enabled: true, ModelPath: "/models/first.gguf"},
	}
	a := newCleanupTestApp(t, settings, t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a.store, a.extractor = st, ingest.New()

	// Enough crossings that the two sides overlap; the number is not a measurement, it is
	// what makes the overlap certain rather than likely.
	const rounds = 200

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// A different address every time, because the callback saves only what
			// changed: its own early return would otherwise stop the marshalling after
			// the first call and leave the window unwatched.
			a.onManagedState("llamacpp", rt.Status{
				State: rt.StateRunning,
				URL:   fmt.Sprintf("http://127.0.0.1:%d", 18000+i),
			})
		}
	}()

	for i := 0; i < rounds; i++ {
		mm := config.ManagedModel{
			Enabled:   true,
			ModelPath: fmt.Sprintf("/models/model-%d.gguf", i),
			CtxSize:   4096,
		}
		if err := a.SetManagedModel("llamacpp", mm); err != nil {
			t.Fatalf("SetManagedModel: %v", err)
		}
	}
	wg.Wait()

	// And the save that ended it all still describes the last write: a snapshot taken
	// while another goroutine was writing the same map would be a settings blob that
	// disagrees with the app that wrote it.
	stored, err := a.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got := stored.Runtime.Models["llamacpp"].ModelPath; got != "/models/model-199.gguf" {
		t.Errorf("stored model path = %q, want the last saved one", got)
	}
}
