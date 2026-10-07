// Package llm defines the backend-agnostic chat model abstraction used by
// KiChat. LM Studio, Ollama and any OpenAI-compatible server are all exposed
// through the same Provider interface so the rest of the app never has to care
// which one is answering.
package llm

import (
	"context"
	"errors"
	"fmt"
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
	TopP        float64
	MaxTokens   int
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
