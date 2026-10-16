package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// jsonClient is used for the short metadata calls (model listings, health).
var jsonClient = &http.Client{Timeout: 15 * time.Second}

// streamClient has no total timeout: generations can legitimately run for
// minutes. Cancellation is driven entirely by the request context.
var streamClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	},
}

// Retrying is only ever done on a request whose response has not been handed to
// a caller yet. That is why the streaming reads are not covered: re-running one
// would replay tokens the user has already seen. Everything below is safe to
// repeat — a metadata call, or the request phase of a stream that failed before
// its body was returned.
const (
	maxAttempts   = 3
	retryBaseWait = 250 * time.Millisecond
	retryMaxWait  = 2 * time.Second
	// retryHintCeiling is the longest wait a backend's own hint is obeyed for.
	//
	// The budgets that throttle this way are defined per *minute* — Groq's
	// input-token limit is the one this exists for — so a hint up to one window is
	// the vendor stating the truth and is waited out. Longer than that the request
	// would be parked for more time than a turn can justify, and the refusal that
	// follows states the wait in its own words anyway.
	retryHintCeiling = time.Minute
)

// retryable reports whether err is worth another attempt.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller's decision, not a hiccup.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A refusal caused by the request's own size is a verdict, not a hiccup: the
	// same body produces the same answer, so there is nothing to wait for. Groq's
	// oversized request arrives as a 413 (never retryable anyway), but a vendor that
	// spells the same verdict with a 429 would otherwise spend all three attempts
	// against a budget the request itself exceeds.
	if _, tooLarge := RequestTooLarge(err); tooLarge {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	// A transport failure — a backend still starting up refusing connections, a
	// reset connection — is usually transient.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// retryAfterInBody reads the wait a refusal writes into its own sentence.
//
// Groq's rate-limit envelope is the reason it is read at all: the account's
// per-minute input-token limit answers HTTP 429 with `type: "tokens"`,
// `code: "rate_limit_exceeded"`, and the wait in prose —
//
//	Rate limit reached for model `qwen/qwen3.8-27b` in organization `org_…` service
//	tier `on_demand` on input tokens per minute (ITPM): Limit 7000, Used 1911,
//	Requested 6248. Please try again in 9.934285714s.
//
// The header is sent alongside it, but not by every backend that throttles this
// way, and the sentence is the number the vendor is willing to commit to.
var retryAfterInBody = regexp.MustCompile(`(?i)\b(?:try again|retry|available) in ([0-9]+(?:\.[0-9]+)?)\s*(ms|milliseconds?|s|sec|seconds?|m|min|minutes?)\b`)

// retryHint is how long a backend asked to be left alone, or 0 when it said
// nothing. Both spellings of `Retry-After` are accepted — delta-seconds, which is
// what these APIs send, and the HTTP date the RFC also allows — and the body is
// read as well rather than instead of the header.
func retryHint(header http.Header, body string) time.Duration {
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		// Delta-seconds is the documented form, and these APIs send it
		// fractionally, so it is parsed as a float rather than as an int.
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
			if seconds > 0 {
				return time.Duration(seconds * float64(time.Second))
			}
		} else if when, err := http.ParseTime(raw); err == nil {
			if wait := time.Until(when); wait > 0 {
				return wait
			}
		}
	}
	m := retryAfterInBody.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil || value <= 0 {
		return 0
	}
	unit := time.Second
	switch lower := strings.ToLower(m[2]); {
	case strings.HasPrefix(lower, "ms"), strings.HasPrefix(lower, "milli"):
		unit = time.Millisecond
	case strings.HasPrefix(lower, "m"):
		unit = time.Minute
	}
	return time.Duration(value * float64(unit))
}

// retryWait is how long to wait before the next attempt: what the backend asked
// for when it said so, and a doubling backoff when it did not.
//
// The hint is obeyed because not obeying it is what this used to do, and it does
// not work: two retries inside a second against a limit defined per minute spend
// the whole attempt budget inside one window, so the throttle is reported as a
// failure. The case this was written for asked for 9.93 seconds.
func retryWait(err error, attemptNo int) time.Duration {
	if named := namedWait(err); named > 0 {
		return named
	}
	wait := retryBaseWait << attemptNo
	if wait > retryMaxWait {
		wait = retryMaxWait
	}
	return wait
}

// namedWait is the wait a backend asked for itself, capped, or 0 when it named none.
//
// It is separate from retryWait because "the backend said so" is the distinction a
// caller with a window to paint cares about: see waitBeforeRetry.
func namedWait(err error) time.Duration {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.RetryAfter <= 0 {
		return 0
	}
	if httpErr.RetryAfter > retryHintCeiling {
		return retryHintCeiling
	}
	return httpErr.RetryAfter
}

// waitBeforeRetry sleeps that long, and returns false when the context ended
// first, so a Stop does not have to wait it out.
//
// A watcher on the context — see RetryWatcher — is told about the wait, because a
// retry that obeys a backend's own hint can sleep for most of a minute and a caller
// with a window to paint cannot tell that from a hang. A wait the client guessed at
// is not reported, so nothing is shown for a hiccup that resolves in a quarter of a
// second.
func waitBeforeRetry(ctx context.Context, err error, attemptNo int) bool {
	// Only a wait the backend named is reported. A plain backoff is 250 ms and then
	// 500 ms, and a line of status text that appears and disappears inside a blink
	// reads as a glitch rather than as an explanation — so a transient hiccup stays
	// silent, and what is shown is always the backend's own number.
	reportWait := namedWait(err) > 0
	wait := retryWait(err, attemptNo)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	if w := RetryWatcherOf(ctx); w != nil && reportWait {
		w.Waiting(wait, err)
		// Deferred rather than called on the timer path, because the wait also ends
		// when a Stop cancels it — and a phase left standing would say a throttle is
		// still being waited out after the turn is over.
		defer w.Waited()
	}
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// RetryWatcher is told when a request is about to wait out a retry, and when it has
// stopped waiting.
//
// Both calls happen on the goroutine doing the waiting, which is the one that will
// read the response, so a caller that shows something in between is showing it for
// exactly the interval in which nothing else is happening: no tokens arrive during
// it, by definition, and the phase cannot outlive it.
type RetryWatcher interface {
	// Waiting reports the wait about to be taken, and the failure that caused it — or
	// nil when there was no failure behind it, which is a wait taken *before* a
	// request rather than after one: a backend's token allowance paced rather than a
	// throttle obeyed (internal/llm/pace.go). The two are worth telling apart, and
	// the distinction is exactly this: a refusal leaves an error, and a request that
	// has not been sent yet cannot have one.
	Waiting(wait time.Duration, err error)
	// Waited reports the end of that wait, whether the timer woke the request or
	// the context ended during it. Implementations retract whatever Waiting said.
	Waited()
}

// retryWatcherKey carries a RetryWatcher through a request's context.
type retryWatcherKey struct{}

// WithRetryWatcher returns a context that reports every retry made with it to w.
//
// The watcher travels in the context rather than being configured on the provider
// because the retry belongs to the shared client, one layer below the provider,
// which has no idea which chat — or which window — asked. A context without one is
// silent, which is what a metadata call wants: a listing, a health probe or a model
// check has no turn to explain.
func WithRetryWatcher(ctx context.Context, w RetryWatcher) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, retryWatcherKey{}, w)
}

// RetryWatcherOf returns the watcher ctx carries, or nil.
//
// It is exported for the callers that have to behave like the client does: a test
// doubling for a throttling provider fires the watcher exactly where the real
// request would, so what the UI is shown is pinned without an HTTP server.
func RetryWatcherOf(ctx context.Context) RetryWatcher {
	w, _ := ctx.Value(retryWatcherKey{}).(RetryWatcher)
	return w
}

// responseWatcher is told the headers of every response a request receives, before
// its body is read — a successful one and a refusal alike.
//
// It exists for the numbers a backend publishes nowhere else. Groq's per-request
// token budget arrives this way: every response states the tokens-per-minute
// allowance that applies to the model that answered, and the vendor's listing
// reports windows instead, so the response is the only place the number a request
// has to fit under can be learned without first being refused.
//
// It travels in the request context rather than being configured on the provider for
// the same reason RetryWatcher does: the response is produced by the shared client,
// one layer below the provider. A context without one is silent, which is what a
// listing, a health probe or a model check wants — none of them is planning a
// request against a budget.
type responseWatcher interface {
	// ResponseHeaders reports the headers a backend sent with one response.
	ResponseHeaders(h http.Header)
}

// responseWatcherKey carries a responseWatcher through a request's context.
type responseWatcherKey struct{}

// withResponseWatcher returns a context that reports every response made with it to w.
func withResponseWatcher(ctx context.Context, w responseWatcher) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, responseWatcherKey{}, w)
}

// observeResponse hands a response's headers to the watcher ctx carries, if any.
func observeResponse(ctx context.Context, h http.Header) {
	if w, _ := ctx.Value(responseWatcherKey{}).(responseWatcher); w != nil {
		w.ResponseHeaders(h)
	}
}

// withRetry runs attempt until it succeeds, fails unrecoverably, or runs out of
// attempts. The last real error is reported rather than the cancellation that
// ended a wait, because "the backend returned 503 twice" is the actionable fact.
func withRetry[T any](ctx context.Context, attempt func() (T, error)) (T, error) {
	var (
		lastErr error
		zero    T
	)
	for i := 0; i < maxAttempts; i++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, ctxErr
		}
		value, err := attempt()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable(err) || i == maxAttempts-1 {
			break
		}
		if !waitBeforeRetry(ctx, err, i) {
			break
		}
	}
	return zero, lastErr
}

// credentialHeaders are the request headers whose value must never survive into
// an error message. A gateway or reverse proxy occasionally echoes the request
// back in its error page, and HTTPError.Body carries that text verbatim to a
// toast or a log.
var credentialHeaders = map[string]bool{
	"authorization":        true,
	"x-subscription-token": true,
	"api-key":              true,
	"x-api-key":            true,
	"x-goog-api-key":       true,
}

// redactHeaders removes every credential we sent from an error body, so a
// provider cannot leak its own key by repeating what it was told.
func redactHeaders(err error, headers map[string]string) error {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Body == "" {
		return err
	}
	for name, value := range headers {
		if !credentialHeaders[strings.ToLower(name)] {
			continue
		}
		for _, secret := range []string{value, strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))} {
			// A short value is a generic word, not a key; redacting it would
			// mangle the message without protecting anything.
			if len(secret) < 8 {
				continue
			}
			httpErr.Body = strings.ReplaceAll(httpErr.Body, secret, "[redacted]")
		}
	}
	return err
}

// doJSON performs a request, decodes the JSON body into out, and converts
// non-2xx responses into *HTTPError so callers can react to status codes. A
// transient failure is retried before it is reported.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, doJSONOnce(ctx, client, method, url, headers, body, out)
	})
	return err
}

func doJSONOnce(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Before the status is judged, because a refusal publishes the same budget a
	// success does, and it is the refusal that needs the number most.
	observeResponse(ctx, resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		body := strings.TrimSpace(string(raw))
		return classifyRequestTooLarge(redactHeaders(&HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       body,
			URL:        url,
			RetryAfter: retryHint(resp.Header, body),
		}, headers))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postStream issues a streaming POST and hands back the raw response body. The
// caller owns closing it.
//
// Only the request phase is retried, and it completes before the caller starts
// reading, so a retry cannot duplicate output. A response lost after the backend
// had already begun generating would cost one wasted generation, which is the
// price of not failing the turn outright.
func postStream(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
	return withRetry(ctx, func() (io.ReadCloser, error) {
		return postStreamOnce(ctx, url, headers, body)
	})
}

func postStreamOnce(ctx context.Context, url string, headers map[string]string, body any) (io.ReadCloser, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	// The headers are read before anything is decided about the status: Groq's
	// budget travels on the answer and on the refusal alike.
	observeResponse(ctx, resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		body := strings.TrimSpace(string(raw))
		return nil, classifyRequestTooLarge(redactHeaders(&HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       body,
			URL:        url,
			RetryAfter: retryHint(resp.Header, body),
		}, headers))
	}
	return resp.Body, nil
}

// sseReader parses a text/event-stream body. It is deliberately tolerant of
// payloads split at arbitrary byte boundaries, which is the failure mode that
// breaks naive streaming clients.
type sseReader struct {
	br *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the data payload of the next event. It returns io.EOF once the
// stream is exhausted.
func (s *sseReader) Next() ([]byte, error) {
	var data [][]byte
	for {
		line, err := s.br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(string(line), "\r\n")
			switch {
			case trimmed == "":
				// Blank line terminates an event.
				if len(data) > 0 {
					return bytes.Join(data, []byte("\n")), nil
				}
			case strings.HasPrefix(trimmed, ":"):
				// Comment / keep-alive.
			case strings.HasPrefix(trimmed, "data:"):
				payload := strings.TrimPrefix(trimmed, "data:")
				payload = strings.TrimPrefix(payload, " ")
				data = append(data, []byte(payload))
			default:
				// event:, id:, retry: — not needed by these backends.
			}
		}
		if err != nil {
			if err == io.EOF && len(data) > 0 {
				return bytes.Join(data, []byte("\n")), nil
			}
			return nil, err
		}
	}
}

// ndjsonReader decodes newline-delimited JSON, tolerating any chunking because
// json.Decoder buffers internally.
type ndjsonReader struct {
	dec *json.Decoder
}

func newNDJSONReader(r io.Reader) *ndjsonReader {
	return &ndjsonReader{dec: json.NewDecoder(bufio.NewReaderSize(r, 64*1024))}
}

// Next decodes the next JSON value into out. Returns io.EOF at the end.
func (n *ndjsonReader) Next(out any) error {
	return n.dec.Decode(out)
}
