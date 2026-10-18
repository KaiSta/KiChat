// Package llm defines the backend-agnostic chat model abstraction used by
// KiChat. LM Studio, Ollama and any OpenAI-compatible server are all exposed
// through the same Provider interface so the rest of the app never has to care
// which one is answering.
package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Role identifies who authored a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model's request to invoke one of our tools.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON object
}

// Message is one entry in a conversation as sent to a backend.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	// Images holds base64-encoded image payloads (no data: prefix) for models
	// that advertise vision support.
	Images []string `json:"images,omitempty"`
}

// Capability names advertised by backends.
const (
	CapTools  = "tool_use"
	CapVision = "vision"
	CapThink  = "thinking"
)

// ModelInfo describes a single model exposed by a backend.
type ModelInfo struct {
	ID             string   `json:"id"`
	Backend        string   `json:"backend"`
	Publisher      string   `json:"publisher,omitempty"`
	Arch           string   `json:"arch,omitempty"`
	Quantization   string   `json:"quantization,omitempty"`
	State          string   `json:"state,omitempty"` // loaded | not-loaded | loading
	Type           string   `json:"type,omitempty"`  // llm | vlm | embeddings
	ContextLimit   int      `json:"contextLimit"`
	MaxContext     int      `json:"maxContextLimit,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	ParameterCount string   `json:"parameterCount,omitempty"`
}

// Model state values.
const (
	StateLoaded    = "loaded"
	StateNotLoaded = "not-loaded"
	StateLoading   = "loading"
)

// Has reports whether the model advertises the given capability.
func (m ModelInfo) Has(capability string) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Ref is the fully-qualified identity of a model: backend plus model id.
type Ref struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

func (r Ref) String() string { return r.Backend + "/" + r.Model }

// Usage is the token accounting reported by a backend for one request.
type Usage struct {
	PromptTokens       int `json:"promptTokens"`
	CompletionTokens   int `json:"completionTokens"`
	TotalTokens        int `json:"totalTokens"`
	CachedPromptTokens int `json:"cachedPromptTokens,omitempty"`
	// ReasoningTokens is how much of the completion went to private thinking
	// rather than to the answer. LM Studio's native route reports it directly and
	// OpenAI-compatible servers put it in completion_tokens_details. It is the
	// number that separates "the model answered nothing" from "the model thought
	// until its budget ran out", which is a bug report and not a slow answer.
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
}

// GenerationStats captures speed measurements for one generation.
type GenerationStats struct {
	TokensPerSecond    float64 `json:"tokensPerSecond"`
	TimeToFirstTokenMs int64   `json:"ttftMs"`
	GenerationMs       int64   `json:"generationMs"`
	TotalMs            int64   `json:"totalMs,omitempty"`
	LoadMs             int64   `json:"loadMs,omitempty"`
	StopReason         string  `json:"stopReason,omitempty"`
	// Source records where the numbers came from. "estimated" means we timed it
	// ourselves because the backend reported nothing, and the UI badges it.
	Source string `json:"source"`
}

// Stats source values.
const (
	SourceLMStudio  = "lmstudio"
	SourceOllama    = "ollama"
	SourceLlamaCpp  = "llamacpp"
	SourceEstimated = "estimated"
)

// StreamEventType enumerates the events produced while streaming a reply.
type StreamEventType string

const (
	EventDelta     StreamEventType = "delta"
	EventReasoning StreamEventType = "reasoning"
	EventToolCall  StreamEventType = "tool_call"
	EventUsage     StreamEventType = "usage"
	EventDone      StreamEventType = "done"
	EventError     StreamEventType = "error"
	EventModelInfo StreamEventType = "model_info"
)

// StreamEvent is a single increment of a streamed generation.
type StreamEvent struct {
	Type      StreamEventType  `json:"type"`
	Text      string           `json:"text,omitempty"`
	ToolCall  *ToolCall        `json:"toolCall,omitempty"`
	Usage     *Usage           `json:"usage,omitempty"`
	Stats     *GenerationStats `json:"stats,omitempty"`
	ModelInfo *ModelInfo       `json:"modelInfo,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// ToolDefinition is a tool exposed to the model.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// ChatRequest is one generation request, normalised across backends.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolDefinition
	Temperature float64
	// TemperatureSet says the caller meant the temperature, zero included. Zero on
	// its own means "nobody chose one", because a backend's default is not this app's
	// to overwrite — but greedy decoding *is* zero, and a backend that drops a zero
	// leaves the model's own sampling in place. Ollama is that backend: it sends the
	// key only above zero, so a caller asking for greedy has to say so here as well.
	TemperatureSet bool
	TopP           float64
	MaxTokens      int
	// NumCtx overrides the context window for backends where it is a per-request
	// option (Ollama). Zero means "let the backend/modelfile decide".
	NumCtx    int
	Seed      *int
	KeepAlive string
	// Think requests model-side thinking when non-nil: false disables it, true
	// forces it, nil leaves the choice to the backend/model. Only a backend with a
	// documented control acts on it (Ollama's `think`); others ignore it, which is
	// why compaction also adapts its output budget rather than trusting this alone.
	Think *bool
}

// Provider is a chat backend.
type Provider interface {
	// ID is the stable key used to qualify model references.
	ID() string
	// Name is the human-readable backend name.
	Name() string
	// Health returns nil when the backend is reachable.
	Health(ctx context.Context) error
	// ListModels enumerates the models the backend knows about, including their
	// context window and capabilities.
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Chat starts a generation. The returned channel is closed when the stream
	// ends. Cancelling ctx aborts the generation.
	Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}

// CacheInvalidator is implemented by a provider that caches per-model metadata
// beyond a single listing. An explicit re-check has to reach those caches too:
// Ollama keeps its /api/show detail for ten minutes, so dropping only the
// registry's catalogue would hand back the very number the user asked to have
// re-read, and the re-check would look broken.
//
// It is a separate interface rather than a method on Provider so a provider
// without such a cache does not have to implement an empty one.
type CacheInvalidator interface {
	InvalidateCache()
}

// ThinkingToggle is implemented by a provider that can express "do not think" in
// the request it sends. It lets compaction ask for no thinking on a backend whose
// model metadata advertises no thinking capability: LM Studio reports none, yet
// its request accepts `enable_thinking`. A provider that does not implement it is
// left to the model's own default.
type ThinkingToggle interface {
	// CanToggleThinking reports whether this provider can turn thinking off in the
	// requests it sends.
	CanToggleThinking() bool
}

// Ceiling is what a backend has published about the largest single request it accepts
// for one model.
type Ceiling struct {
	// Tokens is the limit as the backend stated it, or 0 when it has stated none.
	Tokens int
	// Source names where the number came from, so that a panel can say why it believes
	// it rather than presenting it as a property of the model. It is empty exactly
	// when Tokens is 0.
	Source string
}

// Where a per-request ceiling came from. Both are the vendor's own number, read from
// two different places; they name one limit seen at two moments.
const (
	// CeilingFromRefusal: a refusal stated it in its own sentence, which is how a
	// request that was already too large teaches the app the number.
	CeilingFromRefusal = "refusal"
	// CeilingFromHeader: a response published it. This is the better of the two, and
	// the reason it is read at all: the ceiling is then known from an ordinary answer
	// instead of from a failure, so the next oversized request is folded before it is
	// sent rather than after it is refused.
	CeilingFromHeader = "header"
	// CeilingFromManual: the user typed it in after reading the backend's own limits
	// page. It is the only source that speaks before the backend does, which is why it
	// exists — but it is a person's transcription rather than a vendor statement, so a
	// number the backend states later wins when it is the smaller one.
	CeilingFromManual = "manual"
)

// RequestCeiling is implemented by a provider that knows of a ceiling on the size
// of one request, smaller than the model's context window.
//
// Groq is why it exists. Its per-minute token allowance is also the largest single
// request the account may send, so a conversation that fits the 131k window of
// `openai/gpt-oss-20b` perfectly is refused outright once it passes that allowance:
// the request is bigger than a whole minute of budget, and no amount of waiting
// makes it smaller. The window the listing publishes is honest and useless as a
// plan, so the ceiling is planned against too — and it is the vendor's own number
// rather than a guess, read from its refusal and from the budget header of every
// response (internal/llm/openai.go, internal/chat/oversize.go, internal/chat/context.go).
//
// It is a separate interface rather than a method on Provider so a backend that
// publishes no such limit does not have to implement an empty one.
type RequestCeiling interface {
	// RequestCeiling is the largest single request this provider will accept for a
	// model and where that number came from, or the zero value when it knows of none
	// — which is the answer of every backend that publishes no such limit. A number
	// here is always the backend's own, never an estimate: an invented ceiling would
	// compact a chat the backend would have accepted.
	RequestCeiling(model string) Ceiling
}

// Allowance is what a backend's minute has left for *requests*: the rate the vendor
// stated, and this app's own spend inside the trailing window.
//
// It exists because pacing one request is not a plan for a turn. llm.paceRequest holds a
// request back when the minute cannot pay for it, which is what keeps a slow chat
// working; a turn that makes many requests back to back — a tool loop — needs to know
// *before it starts* whether the minute has room for them, because the alternative is to
// spend every slot and then sit at the wall with the rest of the turn still to run
// (GROQ.md §6.4).
//
// Its two halves come from different places, which is the whole shape of this limit: the
// rate is the vendor's own number, learned from a refusal because no header of this API
// publishes the minute's request rate, while the spend is this app's own count of the
// responses it has had for the model — a lower bound on the account's, since a second
// client and this app's own listings are spent too and cannot be counted.
type Allowance struct {
	// Limit is the requests per minute the vendor stated, or 0 when nothing has stated
	// one. 0 is "unknown" rather than "none": nothing is spaced without a number.
	Limit int
	// Spent is how many of this model's requests this app has had answered inside the
	// trailing minute.
	Spent int
	// Free lists, oldest first, how long each of those spent requests still has to run:
	// Free[i] is when the i-th of them leaves the window and frees a slot. It is what
	// makes a plan exact rather than optimistic — a turn spaced against the rate alone
	// would put its next request where the window has no room for it.
	Free []time.Duration
}

// Known reports whether a rate has been stated for this model at all. An unknown
// allowance is not a spent one, and the difference matters: a full minute means wait, an
// unmeasured one means nothing has been published to wait for.
func (a Allowance) Known() bool { return a.Limit > 0 }

// Room is how many requests the minute can pay for right now. It is 0 both for a full
// minute and for one nobody has described, so a caller that cares reads Known.
func (a Allowance) Room() int {
	if a.Limit <= 0 || a.Spent >= a.Limit {
		return 0
	}
	return a.Limit - a.Spent
}

// RequestAllowance is implemented by a provider that can state what its minute has left
// for requests, and when the slots it has spent will free. llm.Allowance is the reading;
// this is the interface a caller plans against.
//
// It is a separate interface rather than a method on Provider, for the same reason
// RequestCeiling is: a backend whose request rate nobody measured a number for does not
// have to implement an empty one, and a caller that finds it unimplemented invents
// nothing — which is the answer for every backend but Groq, where the rate is a
// measurement of a vendor rather than a property of OpenAI-compatible servers.
type RequestAllowance interface {
	// RequestAllowance is what this provider's minute has left for the model, as the
	// vendor stated it and as this app has spent it. The zero value means "nothing has
	// been stated", and nothing may be paced or spaced against it.
	RequestAllowance(model string) Allowance
}

// BackendAPIKeyName qualifies a secret with the backend it belongs to, so each
// backend's key lives under its own name in the secret store without colliding
// with the Brave key or with another backend.
func BackendAPIKeyName(backendID string) string {
	return "backend:" + backendID
}

// ErrUnavailable marks a backend that could not be reached.
var ErrUnavailable = errors.New("backend unavailable")

// ErrModelNotFound marks a request for a model the backend does not have.
var ErrModelNotFound = errors.New("model not found")

// ErrContextExceeded marks a request that does not fit the model's context.
var ErrContextExceeded = errors.New("context window exceeded")

// HTTPError carries a backend response failure with enough detail to be useful
// in the UI.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
	// RetryAfter is how long the backend asked to be left alone before the next
	// attempt, and 0 when it asked for nothing. Both places it is said are read,
	// because a throttled request is told in either: the `Retry-After` header, and
	// the refusal's own message — Groq's per-minute input-token limit answers with
	// `type: "tokens"`, `code: "rate_limit_exceeded"` and the wait written into the
	// sentence: "Please try again in 9.934285714s."
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	return fmt.Sprintf("%s: %s %s", e.URL, e.Status, body)
}

// Retryable reports whether the failure is worth retrying.
func (e *HTTPError) Retryable() bool {
	return e.StatusCode == 429 || e.StatusCode >= 500
}
