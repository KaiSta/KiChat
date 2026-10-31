package chat

import (
	"context"
	"testing"
	"time"

	"kichat/internal/events"
	"kichat/internal/llm"
)

// plainProvider is a backend that cannot state a request allowance at all, which is every
// provider whose request rate nobody measured a number for.
type plainProvider struct{}

func (plainProvider) ID() string   { return "plain" }
func (plainProvider) Name() string { return "Plain" }

func (plainProvider) Health(context.Context) error { return nil }

func (plainProvider) ListModels(context.Context) ([]llm.ModelInfo, error) { return nil, nil }

func (plainProvider) Chat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	return nil, nil
}

// TestATurnWithoutARateIsNotPlanned is the boundary that keeps this from touching every
// backend and every turn: a provider that cannot state an allowance gets no plan, one that
// can but has stated nothing gets no plan, and neither is invented from a window or a guess.
func TestATurnWithoutARateIsNotPlanned(t *testing.T) {
	if plan := planTurnRequests(plainProvider{}, "m", 4); len(plan.at) != 0 {
		t.Errorf("a backend with no allowance was planned: %v", plan.at)
	}

	// A provider that implements the interface but has nothing stated — which is the state
	// of a model nobody has answered a request for.
	silent := &fakeProvider{}
	if plan := planTurnRequests(silent, "m", 4); len(plan.at) != 0 {
		t.Errorf("an allowance nobody stated was planned: %v", plan.at)
	}

	// And a turn with no rounds to make.
	stated := &fakeProvider{allowance: llm.Allowance{Limit: 2, Spent: 2, Free: []time.Duration{time.Second}}}
	if plan := planTurnRequests(stated, "m", 0); len(plan.at) != 0 {
		t.Errorf("a turn with no rounds was planned: %v", plan.at)
	}
}

// TestATurnThatFitsIsNotSpaced is the other half of the boundary, and the reason this is not
// simply a delay: inside the minute's room the earliest request is the best one, so a turn
// that fits is not held back by even a millisecond. Only the turn that needs more requests
// than the minute has left is spread.
func TestATurnThatFitsIsNotSpaced(t *testing.T) {
	room := &fakeProvider{allowance: llm.Allowance{Limit: 30, Spent: 0}}
	for _, rounds := range []int{1, 3, 30} {
		if plan := planTurnRequests(room, "m", rounds); len(plan.at) != 0 {
			t.Errorf("a %d-round turn that fits its minute was planned: %v", rounds, plan.at)
		}
	}
	// A cap above the allowance is a ceiling rather than a forecast, and it must not turn
	// every turn into a paced one: coding mode's 60 rounds against a whole minute of thirty is
	// still a turn that fits, and spacing it would put two seconds in front of every round of
	// a three-round answer.
	if plan := planTurnRequests(room, "m", 60); len(plan.at) != 0 {
		t.Errorf("a cap above the allowance was planned against a whole minute: %v", plan.at)
	}

	// One round past what the minute has left, and the turn is planned.
	const limit = 30
	free := make([]time.Duration, limit)
	for i := range free {
		free[i] = 59*time.Second + time.Duration(i)*time.Millisecond
	}
	tight := &fakeProvider{allowance: llm.Allowance{Limit: limit, Spent: limit, Free: free}}
	plan := planTurnRequests(tight, "m", 2)
	if len(plan.at) != 2 {
		t.Fatalf("a two-round turn against a spent minute was not planned: %v", plan.at)
	}
	// The first round waits for the slot the minute frees, and the second is one round's
	// share of the minute behind it — 59 s and 61 s here, because the allowance's own
	// period is one request every two seconds.
	if wait := plan.waitFor(0, time.Now()); wait <= 0 || wait > time.Minute {
		t.Errorf("the first round waits %s, want the remainder of the oldest request's minute", wait)
	}
	if gap := plan.at[1].Sub(plan.at[0]); gap < 2*time.Second {
		t.Errorf("the second round follows the first by %s, want the allowance's own period", gap)
	}

	// And a cap past the allowance is planned for one window, not for the ceiling: past that
	// the schedule is the window's own refill, and the per-request guard holds the rest.
	if plan := planTurnRequests(tight, "m", 60); len(plan.at) != limit {
		t.Errorf("a 60-round cap against a spent minute planned %d requests, want the %d of one window",
			len(plan.at), limit)
	}
}

// TestAToolHeavyTurnIsSpacedOverTheMinute is the shape the whole reading exists for: a turn
// whose rounds exceed what the minute has left is spread at the allowance's rate, so the
// requests go out one per slot instead of in a burst that then sits at the wall.
//
// The arithmetic is the one the plan rests on: no round follows the one before it by less
// than the allowance's period, whatever the window is doing.
func TestAToolHeavyTurnIsSpacedOverTheMinute(t *testing.T) {
	const (
		limit  = 30
		rounds = 8
	)
	// Two slots left of the minute, and the requests holding them run out over the next
	// minute: the turn cannot fit, so it is spread.
	free := make([]time.Duration, limit-2)
	for i := range free {
		free[i] = 30*time.Second + time.Duration(i)*time.Second
	}
	provider := &fakeProvider{allowance: llm.Allowance{Limit: limit, Spent: len(free), Free: free}}

	plan := planTurnRequests(provider, "m", rounds)
	if len(plan.at) != rounds {
		t.Fatalf("the plan covers %d requests, want the %d the turn may make", len(plan.at), rounds)
	}
	if wait := plan.waitFor(0, time.Now()); wait > 0 {
		t.Errorf("the first round was held back %s while the minute had room for it", wait)
	}
	period := 60 * time.Second / limit
	for i := 1; i < len(plan.at); i++ {
		if gap := plan.at[i].Sub(plan.at[i-1]); gap < period {
			t.Errorf("round %d follows round %d by %s, closer than the allowance's %s a request",
				i, i-1, gap, period)
		}
	}
	// Past the plan there is nothing to wait for, and the entries are how the wait is
	// consumed as the turn goes: a planned moment that has passed is a request that may go.
	if wait := plan.waitFor(rounds, time.Now()); wait != 0 {
		t.Errorf("round %d, past the plan, waits %s", rounds, wait)
	}
	if wait := plan.waitFor(0, plan.at[len(plan.at)-1].Add(time.Hour)); wait != 0 {
		t.Errorf("a planned moment long past still waits %s", wait)
	}
}

// TestAToolHeavyTurnIsSpacedAndSaysSo drives the whole path: a tool-heavy turn against a
// minute with no room is spaced, the wait is reported as its own phase rather than as a
// refusal's or a spent allowance's, and the turn still makes every round it was going to
// make. The numbers are scaled down — a minute's rate of a thousand is arithmetic, not an
// account — so the test does not sit through the wait it is asserting.
func TestAToolHeavyTurnIsSpacedAndSaysSo(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	// A text-protocol tool call, which is what keeps the loop going: the model asks for the
	// same tool every round, so the turn makes as many requests as its cap allows.
	call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	reply := []llm.StreamEvent{{Type: llm.EventDelta, Text: call}}

	const limit = 1000
	free := make([]time.Duration, limit)
	for i := range free {
		free[i] = 20*time.Millisecond + time.Duration(i)*100*time.Microsecond
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded,
			Capabilities: []string{"vision"},
		},
		replies: [][]llm.StreamEvent{reply, reply, reply},
		// Every slot of the minute is spent and the oldest frees in 20 ms, so the turn has
		// to wait for it and then one period (60 ms here) between rounds.
		allowance: llm.Allowance{Limit: limit, Spent: limit, Free: free},
	}
	engine, captured := newTestEngine(t, st, provider)

	started := time.Now()
	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Recherchiere Mietrecht"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}
	elapsed := time.Since(started)

	// The turn still makes every round: being spaced is a schedule, not a refusal.
	if n := provider.requestCount(); n != 3 {
		t.Errorf("generations = %d, want the 3-iteration cap", n)
	}
	// And it took the time the plan asked for: the first round waits for the slot the minute
	// frees, and each of the others a period behind it.
	if elapsed < 100*time.Millisecond {
		t.Errorf("the turn took %s, want the ~140 ms its three spaced rounds need", elapsed)
	}

	var waits []int64
	for _, ev := range captured.snapshot() {
		if ev.name != events.Status || ev.payload["phase"] != spreadPhase {
			continue
		}
		if ms, ok := ev.payload["waitMs"].(int64); ok {
			waits = append(waits, ms)
		}
	}
	if len(waits) == 0 {
		t.Fatal("a spaced turn never said it was spaced")
	}
	// The wait is published with its number, like every other wait in a turn: "waiting"
	// alone does not say whether this is 20 ms or a minute.
	if waits[0] <= 0 || waits[0] > 200 {
		t.Errorf("the first wait shown was %d ms, want the ~20 ms the plan named", waits[0])
	}
}

// TestATurnThatFitsIsNotHeldBack is the same turn with a minute that has room, and it is the
// regression this could otherwise introduce: a plan that tidied every turn into a pause would
// add its latency to every answer, not only to the tool-heavy ones.
func TestATurnThatFitsIsNotHeldBack(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	reply := []llm.StreamEvent{{Type: llm.EventDelta, Text: call}}
	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded,
			Capabilities: []string{"vision"},
		},
		replies:   [][]llm.StreamEvent{reply, reply, reply},
		allowance: llm.Allowance{Limit: 30, Spent: 1},
	}
	engine, captured := newTestEngine(t, st, provider)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Recherchiere Mietrecht"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	if n := provider.requestCount(); n != 3 {
		t.Errorf("generations = %d, want the 3-iteration cap", n)
	}
	for _, ev := range captured.snapshot() {
		if ev.name == events.Status && ev.payload["phase"] == spreadPhase {
			t.Errorf("a turn with room in its minute was spaced: %v", ev.payload)
		}
	}
}
