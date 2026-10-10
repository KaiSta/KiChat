package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// flakyProvider answers a listing until it is told to fail, so a test can pin what
// a listing in which every backend failed does to the catalogue that was working.
type flakyProvider struct {
	mu     sync.Mutex
	models []ModelInfo
	fail   bool
	calls  int
}

func (p *flakyProvider) ID() string   { return "flaky" }
func (p *flakyProvider) Name() string { return "Flaky" }
func (p *flakyProvider) Health(context.Context) error {
	return nil
}
func (p *flakyProvider) Chat(context.Context, ChatRequest) (<-chan StreamEvent, error) {
	return nil, errors.New("not used by this test")
}
func (p *flakyProvider) ListModels(context.Context) ([]ModelInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail {
		return nil, errors.New("connection refused")
	}
	return append([]ModelInfo(nil), p.models...), nil
}

func (p *flakyProvider) setFail(v bool) {
	p.mu.Lock()
	p.fail = v
	p.mu.Unlock()
}

func (p *flakyProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// TestAFailedListingKeepsTheCatalogueThatWasWorking: caching the merge of a round
// in which every backend failed replaced a working catalogue with "no models", so
// one network blip emptied the picker for the whole thirty-second window. The
// timestamp still moves, so the failed round is not retried on every call.
func TestAFailedListingKeepsTheCatalogueThatWasWorking(t *testing.T) {
	r := NewRegistry(nil)
	p := &flakyProvider{models: []ModelInfo{{ID: "m1", Backend: "flaky", ContextLimit: 4096}}}
	r.Register(p)

	if got := r.ListModels(context.Background(), false); len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("first listing = %+v, want the one model", got)
	}

	p.setFail(true)
	got := r.ListModels(context.Background(), true)
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("a round in which every backend failed dropped the catalogue: %+v", got)
	}
	if calls := p.callCount(); calls != 2 {
		t.Errorf("provider calls = %d, want the forced refresh to have been attempted", calls)
	}

	// The snapshot is the cache, and reaches nothing.
	before := p.callCount()
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].ID != "m1" {
		t.Errorf("Snapshot = %+v, want the cached catalogue", snap)
	}
	if p.callCount() != before {
		t.Error("Snapshot reached the provider")
	}
}
