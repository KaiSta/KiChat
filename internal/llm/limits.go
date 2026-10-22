package llm

import (
	"errors"
	"kichat/internal/uimsg"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// ErrRequestTooLarge marks a request a backend refused because the payload itself
// does not fit: the conversation is larger than the largest single request this
// account may send, or larger than the model's own window. It is a verdict on the
// request and not a hiccup — the same body produces the same refusal — which is why
// retryable() never retries one, and why the chat engine folds the history and sends
// the request once more instead of ending the turn (internal/chat/oversize.go).
//
// Groq is the backend that made the distinction necessary. A request bigger than the
// account's per-minute token allowance is refused with HTTP 413 whose envelope says
// `{"type":"tokens","code":"rate_limit_exceeded"}` — the same words as the throttle
// it also sends — so a client that reads only the code treats a permanently oversized
// request as a rate limit and burns its retry budget against a limit the request
// itself exceeds. Measured, the two are told apart by the message:
//
//	Request too large for model `moonshotai/kimi-k2-instruct` in organization `org_…`
//	service tier `on_demand` on tokens per minute (TPM): Limit 10000, Requested 15088,
//	please reduce your message size and try again.
//
// against the transient shape, which reports what is already spent and a wait:
//
//	Rate limit reached for model `qwen/qwen3.8-27b` … on input tokens per minute
//	(ITPM): Limit 7000, Used 1911, Requested 6248. Please try again in 9.934285714s.
var ErrRequestTooLarge = uimsg.Errorf("llm.requestTooLarge")

// RequestTooLargeError is a refusal caused by the size of the request itself.
//
// Cause is the backend's own error, kept whole so the vendor's words still travel
// with it: the chat layer replaces this error's *message* for the user, and a log or
// a bug report still carries the status, the URL and the body that produced it.
type RequestTooLargeError struct {
	Cause error
	// Limit is the largest single request the account may send, as the refusal
	// stated it ("Limit 10000"), or 0 when it stated none.
	//
	// It is the part worth keeping: Groq's catalogues publish each model's context
	// window and never this number, so the refusal is the only place the real
	// per-request ceiling is stated — and a client that has it can plan the next
	// request against it instead of spending another refusal to rediscover it.
	Limit int
	// Requested is what this request was counted at, or 0 when the refusal did not
	// say.
	Requested int
}

func (e *RequestTooLargeError) Error() string { return e.Cause.Error() }

func (e *RequestTooLargeError) Unwrap() error { return e.Cause }

// Is makes errors.Is(err, ErrRequestTooLarge) answer for the sentinel while
// Unwrap still hands the vendor's own error to errors.As.
func (e *RequestTooLargeError) Is(target error) bool { return target == ErrRequestTooLarge }

// RequestTooLarge reports whether err is a refusal caused by the request's own size,
// and what the backend said about it.
func RequestTooLarge(err error) (*RequestTooLargeError, bool) {
	var too *RequestTooLargeError
	if errors.As(err, &too) {
		return too, true
	}
	return nil, false
}

// The two phrases a vendor uses for "this request is too big", and the numbers it
// writes beside them. Phrase matching rather than a code: Groq's oversized request
// and its throttle share a code, and its context-window refusal shares one with
// every other invalid request.
var (
	// "Request too large for model `x` …" — Groq's own words for a request that is
	// bigger than a whole minute of its token allowance.
	tooLargePhrase = regexp.MustCompile(`(?i)\brequest too large\b`)
	// The shapes that name the fix instead: Groq's context-window refusal ("Please
	// reduce the length of the messages or completion.", code
	// `context_length_exceeded`), its documented 413 ("reduce the size of the
	// request body"), and the equivalent sentences other OpenAI-compatible
	// servers send.
	reducePhrase = regexp.MustCompile(`(?i)\breduce (?:your message size|the (?:length|size) of the (?:messages|request))|maximum context length|\bcontext_length_exceeded\b`)
	// "Limit 10000, Requested 15088" / "Limit: 7000, Used 1911, Requested 6248".
	limitInBody     = regexp.MustCompile(`(?i)\blimit:?\s*([0-9]+)`)
	requestedInBody = regexp.MustCompile(`(?i)\brequested:?\s*([0-9]+)`)
)

// malformedToolCallPhrase matches the refusal a backend returns when the call the
// *model* generated names a tool the request did not declare. Groq's wording, measured
// through this app on `qwen/qwen3.8-27b`:
//
//	tool call validation failed: attempted to call tool 'scr scrape_url' which was not
//	in request.tools
//
// The name in the sentence is the model's own — the app sends `scrape_url` verbatim —
// and the request is refused before a token streams, so there is nothing to repair on
// this side. What is worth doing is telling the user that, instead of quoting a sentence
// that reads as though the app had offered a tool list it never offered.
var (
	malformedToolCallPhrase = regexp.MustCompile(`(?i)tool call validation failed`)
	notInRequestTools       = regexp.MustCompile(`(?i)which was not in request\.tools`)
)

// MalformedToolCall reports whether err is a backend refusing a tool call the model
// generated with a name the request never declared.
//
// Narrow on purpose: a 400 whose body carries one of the two phrases Groq uses for this
// one failure. Both are read because the sentence has been seen with and without the
// prefix, and a 5xx is a different matter entirely.
func MalformedToolCall(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusBadRequest {
		return false
	}
	return malformedToolCallPhrase.MatchString(he.Body) || notInRequestTools.MatchString(he.Body)
}

// classifyRequestTooLarge turns a refusal caused by the request's own size into a
// RequestTooLargeError and leaves every other failure exactly as it was.
//
// It is deliberately narrow about *which* failures it claims: only a client error
// (4xx), and only one whose body says the request is too big. A 5xx is a hiccup on
// the vendor's side and stays retryable, and a phrase that turns up in a proxy's
// error page must not take a working retry away from a request that failed for a
// different reason.
func classifyRequestTooLarge(err error) error {
	he, ok := err.(*HTTPError)
	if !ok || he.StatusCode < 400 || he.StatusCode >= 500 {
		return err
	}
	body := he.Body
	if !tooLargePhrase.MatchString(body) && !reducePhrase.MatchString(body) {
		return err
	}
	return &RequestTooLargeError{
		Cause:     err,
		Limit:     firstNumber(limitInBody, body),
		Requested: firstNumber(requestedInBody, body),
	}
}

// A requests-per-minute refusal is the same envelope as the token throttle and a
// different limit, and — unlike the oversized request of ErrRequestTooLarge — it is a
// genuine throttle: the fix is to wait, which the shared client already does. What is
// worth keeping is the number, because nothing else states it. Measured:
//
//	{"error":{"message":"Rate limit reached for model `llama-3.1-8b-instant` in
//	organization `org_01ht7j91w5fjx8yabp2xh9hq37` on requests per minute (RPM): Limit
//	30, Used 30, Requested 1. Please try again in 1.969s. Visit
//	https://console.groq.com/docs/rate-limits for more information.","type":"requests",
//	"code":"rate_limit_exceeded"}}
//
// against the token throttle, which says `type: "tokens"` and names input or output
// tokens per minute instead. Two things tell them apart and both are read, because one
// of them is a word and the other a field: the sentence spells out `requests per minute
// (RPM)`, and `type` says `requests`. The `code` is `rate_limit_exceeded` for all of
// them, which is why nothing here reads it.
var (
	// "… on requests per minute (RPM): Limit 30 …". The period and the parenthesised
	// unit are what keep it off the token shapes — `input tokens per minute (ITPM)` and
	// `output tokens per minute (OTPM)` both contain "per minute" and neither contains
	// this phrase.
	requestRatePhrase = regexp.MustCompile(`(?i)\brequests? per minute\s*\(RPM\)`)
	// The envelope's own name for the same limit, so a vendor that rewords its sentence
	// still teaches the number.
	requestRateType = regexp.MustCompile(`(?i)"type"\s*:\s*"requests"`)
)

// requestsPerMinuteLimit reads the requests-per-minute allowance out of a refusal that
// names one, or reports that this failure is not that refusal.
//
// The number is worth keeping because this API publishes the *day's* request allowance
// in a header (`x-ratelimit-limit-requests`) and the minute's request rate only in a
// table in its console, so a refusal is the one place on the wire where the rate a
// tool-heavy turn runs into is stated. Narrow like classifyRequestTooLarge: only a
// client error, and only one whose body names this limit with a positive number.
func requestsPerMinuteLimit(err error) (int, bool) {
	he, ok := err.(*HTTPError)
	if !ok || he.StatusCode < 400 || he.StatusCode >= 500 {
		return 0, false
	}
	body := he.Body
	if !requestRatePhrase.MatchString(body) && !requestRateType.MatchString(body) {
		return 0, false
	}
	limit := firstNumber(limitInBody, body)
	if limit <= 0 {
		return 0, false
	}
	return limit, true
}

// rateLimitTokensHeader is the header every Groq response carries, success or
// refusal: the tokens-per-minute allowance that applies to the model that answered.
//
// Groq documents the header family as referring "always" to requests per day and
// tokens per minute (https://console.groq.com/docs/rate-limits), so the number is a
// minute's budget rather than a window. That the same number is also the largest
// *single request* the account may send is a measurement of this app's rather than a
// documented rule — the 413 in GROQ.md §6.1 refused a `Requested 15088` request with
// `Limit 10000` on TPM, which is one statement of both quantities at once.
//
// The same note is why the *request* headers beside it are not read at all: they are
// the day's, not the minute's, and the minute's request rate (30 for the models here)
// is published in the console and in no header — which is why the rate a turn is paced
// against is learned from a refusal's own sentence instead (requestsPerMinuteLimit,
// internal/llm/pace.go).
//
// Reading it is therefore inference from that measurement, kept in the safe
// direction: a ceiling learned this way may only *tighten* what the app already
// knows (noteRequestCeiling keeps the smaller number), so a header that turned out to
// be looser than the real per-request maximum costs at most the fold-and-retry the
// refusal path already performs, while a header that is tighter than the window is
// what stops the first oversized request of a session from being sent at all.
const rateLimitTokensHeader = "X-Ratelimit-Limit-Tokens"

// ceilingFromHeaders reads that budget out of a response, or 0 when the response
// carries none or carries one that is not a positive number.
func ceilingFromHeaders(h http.Header) int {
	raw := strings.TrimSpace(h.Get(rateLimitTokensHeader))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// firstNumber reads the first number a pattern found in a refusal body, or 0.
func firstNumber(re *regexp.Regexp, body string) int {
	m := re.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(m[1]))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
