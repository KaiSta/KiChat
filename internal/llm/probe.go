package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Probe outcomes. Only ProbeRefused is a finding about the model itself, which
// is why it is the only one a caller may act on destructively (see ProbeModels).
const (
	// ProbeOK marks a model that answered.
	ProbeOK = "ok"
	// ProbeRefused marks a request the backend rejected with a reason that asking
	// again will not change: no such model on any route, or an account that may
	// not call it. It is what a published catalogue cannot tell you — build.nvidia.com
	// lists 80 models and answers 404 for those the key is not entitled to — and
	// the reason this probe exists.
	ProbeRefused = "refused"
	// ProbeTimeout marks a request that did not finish in the time allowed. A
	// cold or busy model answers the same request a minute later, so this says
	// nothing about whether the model can be used.
	ProbeTimeout = "timeout"
	// ProbeInconclusive marks a throttle, a server-side failure or a broken
	// connection: the backend is in trouble, not this model.
	ProbeInconclusive = "inconclusive"
)

// ModelProbe is the outcome of asking one model to answer.
type ModelProbe struct {
	Model  string `json:"model"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// Refused reports whether this probe is evidence that the model cannot be used.
//
// It is deliberately narrow. Only a non-retryable HTTP refusal counts: a
// throttle (429), a backend error (5xx), a connection failure and a timeout are
// all things that happen to a working model, and treating any of them as a
// verdict would blacklist a good model for the rest of the session because the
// network hiccuped — the failure mode that makes an automatic filter worse than
// no filter.
func (p ModelProbe) Refused() bool { return p.Kind == ProbeRefused }

// ProbeOptions tunes a sweep.
type ProbeOptions struct {
	// Workers is how many requests are in flight at once. Six measured at ~52
	// seconds for NVIDIA's 80 models; one worker takes seven minutes and gains
	// nothing, since the cost is the round trip and not the token.
	Workers int
	// PerModel bounds one model's request. It is generous rather than tight: a
	// model that is loading answers slowly, and waiting ten extra seconds costs
	// nothing while a false "slow" costs a re-run.
	PerModel time.Duration
}

// Defaults for ProbeOptions.
const (
	DefaultProbeWorkers = 6
	DefaultProbeTimeout = 25 * time.Second
)

// ProbeModels asks every model in the list to answer one token and reports what
// came back. It answers the only question a model listing cannot: which of these
// the account may actually call.
//
// The request is a real generation rather than a metadata lookup because nothing
// else works. On build.nvidia.com a GET of a single model returns HTTP 200 with
// the same four fields for every entry — with no API key at all — including for
// models whose chat route answers 404 for the key in use, so no metadata call can
// predict it, and no entitlement endpoint is documented or observed. One token is
// the cheapest request that does: a model the account may not call is refused
// before it generates anything, so the 80-model sweep measured a few dozen tokens
// in total rather than a few thousand.
//
// Results keep the order of the input, so a sweep is reproducible. onResult, when
// given, is called as each answer arrives — it runs on the worker goroutines and
// has to be safe for concurrent use.
func ProbeModels(ctx context.Context, p Provider, models []ModelInfo, opts ProbeOptions, onResult func(ModelProbe)) []ModelProbe {
	workers := opts.Workers
	if workers < 1 {
		workers = DefaultProbeWorkers
	}
	if workers > len(models) && len(models) > 0 {
		workers = len(models)
	}
	perModel := opts.PerModel
	if perModel <= 0 {
		perModel = DefaultProbeTimeout
	}

	probes := make([]ModelProbe, len(models))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, m := range models {
		// A cancelled sweep stops launching rather than starting requests it
		// would only have to abandon. The models it never asked keep the zero
		// ModelProbe, i.e. an empty Kind, which no caller may read as a finding:
		// see ModelProbe.Refused.
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, model string) {
			defer wg.Done()
			defer func() { <-sem }()
			res := probeOne(ctx, p, model, perModel)
			mu.Lock()
			probes[i] = res
			mu.Unlock()
			if onResult != nil {
				onResult(res)
			}
		}(i, m.ID)
	}
	wg.Wait()
	return probes
}

// probeOne is one model's request, drained to the end of its stream.
func probeOne(ctx context.Context, p Provider, model string, timeout time.Duration) ModelProbe {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// One token, and a prompt no model can refuse for what it is asked to do:
	// the question is whether this account may call the model at all, so the work
	// asked of it has to be trivial. Thinking is left at the model's own default,
	// because a thinking model that spends its single token thinking has still
	// answered — the stream ends normally, which is the signal being read here.
	events, err := p.Chat(pctx, ChatRequest{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: "hi"}},
		MaxTokens: 1,
	})
	if err != nil {
		// A refusal arrives here synchronously: the provider returns the failed
		// POST rather than a stream, which is exactly the case a 404 model is.
		return classifyProbeFailure(ctx, model, err)
	}
	for ev := range events {
		if ev.Type == EventError {
			return classifyProbeFailure(ctx, model, errors.New(ev.Error))
		}
	}
	// The channel closed without an error event: the model answered. It does not
	// matter whether it produced a token or only ended the stream — a reasoning
	// model that thought its one token through has answered just as clearly as one
	// that replied.
	return ModelProbe{Model: model, Kind: ProbeOK}
}

// classifyProbeFailure decides what a failed request says about the model.
func classifyProbeFailure(parent context.Context, model string, err error) ModelProbe {
	// The caller's context is the sweep being cancelled, not the model's answer.
	if parent.Err() != nil {
		return ModelProbe{Model: model, Kind: ProbeInconclusive, Detail: "the check was stopped"}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ModelProbe{Model: model, Kind: ProbeTimeout, Detail: "no answer within the time allowed"}
	}
	var he *HTTPError
	if errors.As(err, &he) {
		if he.Retryable() {
			// 429 or 5xx: the backend is throttling the sweep or failing. Both
			// look identical to an account-scoped refusal only if you read the
			// status as a boolean, which is why this is checked first.
			return ModelProbe{Model: model, Kind: ProbeInconclusive, Detail: fmt.Sprintf("HTTP %d", he.StatusCode)}
		}
		// A non-retryable status: 400, 403, 404. This is the model this account
		// may not call, and the case the whole probe exists for.
		return ModelProbe{Model: model, Kind: ProbeRefused, Detail: fmt.Sprintf("HTTP %d", he.StatusCode)}
	}
	// Not an HTTP answer at all — a refused connection, a truncated stream. The
	// backend is unreachable rather than this model being unavailable.
	return ModelProbe{Model: model, Kind: ProbeInconclusive, Detail: err.Error()}
}
