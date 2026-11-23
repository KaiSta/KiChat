package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The two bodies Groq answers the same limit with, measured on the live API and
// reported in the wild. They share a status family, a `type` and a `code`, and they
// mean opposite things: the first is a request that is larger than a whole minute of
// the account's allowance (so it can never succeed, however long the client waits),
// and the second is an allowance that is merely spent (so waiting is exactly right).
// Telling them apart is the whole job of classifyRequestTooLarge.
const (
	groqOversizedMessage = "Request too large for model `moonshotai/kimi-k2-instruct` in organization " +
		"`org_01kz7a9rsze9da1b4q1yv7w8bx` service tier `on_demand` on tokens per minute (TPM): Limit 10000, " +
		"Requested 15088, please reduce your message size and try again. Need more tokens? Upgrade to Dev " +
		"Tier today at https://console.groq.com/settings/billing"
	groqThrottledMessage = "Rate limit reached for model `qwen/qwen3.8-27b` in organization " +
		"`org_01kz7a9rsze9da1b4q1yv7w8bx` service tier `on_demand` on input tokens per minute (ITPM): " +
		"Limit 7000, Used 1911, Requested 6248. Please try again in 9.934285714s. Need more tokens? " +
		"Upgrade to Dev Tier today at https://console.groq.com/settings/billing"
)

// tokensRefusal builds Groq's refusal envelope around a message.
func tokensRefusal(message string) string {
	return fmt.Sprintf(`{"error":{"message":%q,"type":"tokens","code":"rate_limit_exceeded"}}`, message)
}

// TestAnOversizedRequestIsToldApartFromAThrottle is the classifier's whole contract:
// which refusals are a verdict on the request's size, what they say about the limit,
// and — just as important — which ones are left alone.
//
// The negatives are the half with teeth. A throttle is retried and its wait is
// obeyed, so a classifier that claimed it would take a working retry away and make a
// busy account look broken; and a 5xx is the vendor's incident, not the request's
// fault, so a phrase that turned up in a proxy's error page must not make a server
// error unretryable.
func TestAnOversizedRequestIsToldApartFromAThrottle(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		want      bool
		limit     int
		requested int
		window    int
	}{
		{
			name:      "groq: the request alone exceeds the allowance",
			status:    http.StatusRequestEntityTooLarge,
			body:      tokensRefusal(groqOversizedMessage),
			want:      true,
			limit:     10000,
			requested: 15088,
		},
		{
			name:   "groq: the allowance is merely spent",
			status: http.StatusTooManyRequests,
			body:   tokensRefusal(groqThrottledMessage),
			want:   false,
		},
		{
			name:   "the model's own window was exceeded",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"Please reduce the length of the messages or completion.",` +
				`"type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			want: true,
		},
		{
			name:   "the documented request-body 413",
			status: http.StatusRequestEntityTooLarge,
			body:   `{"error":{"message":"The request body is too large. Please reduce the size of the request body."}}`,
			want:   true,
		},
		{
			// Measured on a b12024 llama-server loaded with `-c 16384`, asked for
			// 20016 tokens. It states both numbers in its own sentence and puts the
			// limit second, which is why the Limit/Requested pair above finds nothing.
			name:      "llama.cpp: the loaded window was exceeded",
			status:    http.StatusBadRequest,
			body:      `{"error":{"code":400,"message":"request (20016 tokens) exceeds the available context size (16384 tokens), try increasing it","type":"exceed_context_size_error"}}`,
			want:      true,
			limit:     16384,
			requested: 20016,
			window:    16384,
		},
		{
			// vLLM's sentence is what a hosted deployment that publishes no window
			// on its listing still says when a request is too big. The window is the
			// answer; there is no per-request budget in it, so Limit stays 0.
			name:   "vllm: the deployed window was named",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"This model's maximum context length is 131072 tokens. Please reduce the length of the messages or completion.","type":"BadRequestError","code":400}}`,
			want:   true,
			window: 131072,
		},
		{
			// An older vLLM spells the same number inside parentheses, beside the
			// max_tokens value that made the request too large.
			name:   "vllm: the window in parentheses",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"'max_tokens' (2049) cannot be greater than the model's maximum context length (2048). (parameter=max_tokens, value=2049)","type":"BadRequestError","code":400}}`,
			want:   true,
			window: 2048,
		},
		{
			// The number after "requested" in this sentence is the output cap, not
			// the vendor's count of the request. No limit is stated here, so
			// Requested has to stay 0 or the recovery fold calibrates on it.
			name:   "vllm: the output cap is not the request count",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"This model's maximum context length is 131072 tokens. However, you requested 65535 output tokens and your prompt contains at least 62466 input tokens, for a total of at least 128001 tokens.","type":"BadRequestError","code":400}}`,
			want:   true,
			window: 131072,
		},
		{
			name:   "an invalid key",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"Invalid API Key","type":"invalid_request_error","code":"invalid_api_key"}}`,
			want:   false,
		},
		{
			name:   "a model that cannot chat",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"The model ` + "`whisper-large-v3`" + ` does not support chat completions"}}`,
			want:   false,
		},
		{
			name:   "the vendor's own outage",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"message":"Request too large","type":"server_error"}}`,
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			he := &HTTPError{StatusCode: tc.status, Status: fmt.Sprint(tc.status), URL: "https://example.invalid/v1", Body: tc.body}
			err := classifyRequestTooLarge(he)
			too, ok := RequestTooLarge(err)
			if ok != tc.want {
				t.Fatalf("classified as an oversized request = %v, want %v (%v)", ok, tc.want, err)
			}
			if !tc.want {
				// Every other failure has to arrive exactly as it was: a wrapper here
				// would hide the status from every caller that reads one.
				if err != he {
					t.Fatalf("a non-oversized refusal was replaced by %T", err)
				}
				return
			}
			if !errors.Is(err, ErrRequestTooLarge) {
				t.Error("errors.Is(err, ErrRequestTooLarge) is false for a classified refusal")
			}
			if too.Limit != tc.limit || too.Requested != tc.requested || too.Window != tc.window {
				t.Errorf("limit/requested/window = %d/%d/%d, want %d/%d/%d",
					too.Limit, too.Requested, too.Window, tc.limit, tc.requested, tc.window)
			}
			// The vendor's own words travel with it: the chat layer replaces the
			// message it shows, and a log still has to carry what the backend said.
			// The whole body is checked rather than a word from it, because the
			// wording is the vendor's and llama.cpp spells the same verdict without
			// ever saying "too large" or "reduce".
			if !strings.Contains(err.Error(), he.Body) {
				t.Errorf("error text = %q, want the vendor's body %q kept", err.Error(), he.Body)
			}
			var unwrapped *HTTPError
			if !errors.As(err, &unwrapped) || unwrapped != he {
				t.Error("the HTTP error is no longer reachable through the wrapper")
			}
		})
	}
}

// TestAnOversizedRequestIsNeverRetriedEvenWhenTheStatusSuggestsIt may seem
// hypothetical, and it is the reason the check is explicit rather than left to the
// status: the verdict and the throttle share a code, and a vendor that spelled the
// verdict with a 429 would otherwise spend all three attempts re-sending a body that
// cannot fit — while the wait it is obeying is a minute of a budget the request
// itself exceeds.
func TestAnOversizedRequestIsNeverRetriedEvenWhenTheStatusSuggestsIt(t *testing.T) {
	too := &RequestTooLargeError{
		Cause:     &HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"},
		Limit:     7000,
		Requested: 8123,
	}
	if retryable(too) {
		t.Error("an oversized request was retried")
	}
	if !errors.Is(too, ErrRequestTooLarge) {
		t.Error("the sentinel is not reachable through the wrapper")
	}
}

// TestGroqKeepsTheCeilingItsRefusalNamed pins the measurement that makes the next
// turn cheap: the refusal states the account's real per-request ceiling, per model,
// and this is the only route of this API that states one — the listing publishes
// windows, not budgets.
//
// It also pins that the refusal is not retried: one request went out, not three.
func TestGroqKeepsTheCeilingItsRefusalNamed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, tokensRefusal(groqOversizedMessage))
	}))
	defer srv.Close()

	p := newGroqProvider("groq", "Groq", srv.URL+"/openai/v1", "test-key")
	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "moonshotai/kimi-k2-instruct",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("Chat error = %v, want an oversized-request refusal", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("the refusal was sent %d times, want 1 — a request that cannot fit is not retried", n)
	}
	if got := p.RequestCeiling("moonshotai/kimi-k2-instruct"); got.Tokens != 10000 {
		t.Errorf("ceiling = %d, want the 10000 the refusal named", got.Tokens)
	}
	if got := p.RequestCeiling("moonshotai/kimi-k2-instruct").Source; got != CeilingFromRefusal {
		t.Errorf("ceiling source = %q, want %q", got, CeilingFromRefusal)
	}
	if got := p.RequestCeiling("openai/gpt-oss-20b"); got.Tokens != 0 {
		t.Errorf("ceiling for a model that was never refused = %d, want 0", got.Tokens)
	}
}
