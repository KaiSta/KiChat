package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// openAICompatProvider talks to any server exposing OpenAI-style
// /chat/completions with SSE streaming. LM Studio is the primary user of this
// code; the generic "custom endpoint" provider reuses it verbatim.
//
// It prefers LM Studio's richer native namespace (/api/v0) when present, since
// that namespace reports tokens/second, time-to-first-token and the loaded
// context length directly. When the namespace is missing it degrades to
// /v1/chat/completions and computes the numbers itself.
type openAICompatProvider struct {
	id      string
	name    string
	baseURL string
	apiKey  string
	headers map[string]string

	// chatPaths are tried in order; a 404 retires the path permanently.
	chatPaths []string
	// modelsPath returns rich model metadata; empty if unsupported.
	modelsPath string
	// openAIModelsPath is the plain /v1/models fallback.
	openAIModelsPath string

	// requiresKey marks a backend that cannot answer without a credential. A
	// request without one is not attempted at all, so the user is told what is
	// missing instead of being handed the vendor's 401 body.
	requiresKey bool
	// modelsFlavour selects how the rich model listing is read. Both vendors
	// return a `data` array; they disagree about everything inside it.
	modelsFlavour modelListFlavour

	mu          sync.Mutex
	chatPathIdx int
}

// modelListFlavour names the shape of a model listing. The zero value is LM
// Studio's native namespace, which is what every existing configuration uses.
type modelListFlavour int

const (
	flavourLMStudio modelListFlavour = iota
	flavourDeepSeek
)

// keyCheck reports a backend that needs a credential and has none.
func (p *openAICompatProvider) keyCheck() error {
	if p.requiresKey && strings.TrimSpace(p.apiKey) == "" {
		return ErrMissingAPIKey
	}
	return nil
}

func newLMStudioProvider(baseURL, apiKey string) *openAICompatProvider {
	baseURL = strings.TrimRight(baseURL, "/")
	return &openAICompatProvider{
		id:      "lmstudio",
		name:    "LM Studio",
		baseURL: baseURL,
		apiKey:  apiKey,
		headers: map[string]string{},
		chatPaths: []string{
			baseURL + "/api/v0/chat/completions",
			baseURL + "/v1/chat/completions",
		},
		modelsPath:       baseURL + "/api/v0/models",
		openAIModelsPath: baseURL + "/v1/models",
	}
}

func (p *openAICompatProvider) ID() string   { return p.id }
func (p *openAICompatProvider) Name() string { return p.name }

func (p *openAICompatProvider) authHeaders() map[string]string {
	h := map[string]string{}
	for k, v := range p.headers {
		h[k] = v
	}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	return h
}

func (p *openAICompatProvider) Health(ctx context.Context) error {
	_, err := p.ListModels(ctx)
	if err != nil {
		// Both are wrapped, not just printed: a caller that wants to know whether
		// the backend is down or merely missing a key has to be able to ask, and
		// "%s" buried the cause in the text where errors.Is cannot see it.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// --- model discovery -------------------------------------------------------

type lmStudioModelEntry struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Type              string   `json:"type"`
	Publisher         string   `json:"publisher"`
	Arch              string   `json:"arch"`
	CompatibilityType string   `json:"compatibility_type"`
	Quantization      string   `json:"quantization"`
	State             string   `json:"state"`
	MaxContextLength  int      `json:"max_context_length"`
	LoadedContextLen  int      `json:"loaded_context_length"`
	Capabilities      []string `json:"capabilities"`
}

func (p *openAICompatProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if err := p.keyCheck(); err != nil {
		return nil, err
	}
	if p.modelsPath != "" {
		// Decoded entry by entry rather than into one vendor's struct: the two
		// listings share only the envelope, so each entry is read in full by the
		// mapper that knows its shape.
		var resp struct {
			Data []json.RawMessage `json:"data"`
		}
		err := doJSON(ctx, jsonClient, http.MethodGet, p.modelsPath, p.authHeaders(), nil, &resp)
		if err == nil {
			return p.mapModelEntries(resp.Data), nil
		}
		if he, ok := err.(*HTTPError); !ok || he.StatusCode != 404 {
			// A real failure (connection refused, auth) — surface it.
			return nil, err
		}
		// 404: this build predates the native namespace. Fall through.
	}

	return p.listOpenAIModels(ctx)
}

// mapModelEntries reads a rich listing with the mapper for its flavour. An entry
// that cannot be decoded is skipped rather than failing the listing: one
// unexpected model must not hide the others, the same way a dead backend does
// not hide a live one.
func (p *openAICompatProvider) mapModelEntries(raw []json.RawMessage) []ModelInfo {
	out := make([]ModelInfo, 0, len(raw))
	for _, entry := range raw {
		if p.modelsFlavour == flavourDeepSeek {
			var m deepSeekModelEntry
			if err := json.Unmarshal(entry, &m); err != nil {
				continue
			}
			out = append(out, p.fromDeepSeekEntry(m))
			continue
		}
		var m lmStudioModelEntry
		if err := json.Unmarshal(entry, &m); err != nil {
			continue
		}
		out = append(out, p.fromLMStudioEntry(m))
	}
	return out
}

func (p *openAICompatProvider) fromLMStudioEntry(m lmStudioModelEntry) ModelInfo {
	ctxLimit := m.MaxContextLength
	if m.LoadedContextLen > 0 {
		ctxLimit = m.LoadedContextLen
	}
	state := m.State
	if state == "" {
		state = StateNotLoaded
	}
	return ModelInfo{
		ID:           m.ID,
		Backend:      p.id,
		Publisher:    m.Publisher,
		Arch:         m.Arch,
		Quantization: m.Quantization,
		State:        state,
		Type:         m.Type,
		ContextLimit: ctxLimit,
		MaxContext:   m.MaxContextLength,
		Capabilities: m.Capabilities,
	}
}

// listOpenAIModels is the lowest-common-denominator listing. It cannot report a
// context window or tool support, so context limits must come from the user's
// per-model overrides.
func (p *openAICompatProvider) listOpenAIModels(ctx context.Context) ([]ModelInfo, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.openAIModelsPath, p.authHeaders(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, ModelInfo{
			ID:      m.ID,
			Backend: p.id,
			State:   StateNotLoaded,
		})
	}
	return out, nil
}

// --- chat ------------------------------------------------------------------

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIDelta struct {
	Role             string                `json:"role"`
	Content          string                `json:"content"`
	Reasoning        string                `json:"reasoning"`
	ReasoningContent string                `json:"reasoning_content"`
	Thinking         string                `json:"thinking"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
}

func (d openAIDelta) reasoningText() string {
	switch {
	case d.Reasoning != "":
		return d.Reasoning
	case d.ReasoningContent != "":
		return d.ReasoningContent
	default:
		return d.Thinking
	}
}

type openAIChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Delta        openAIDelta `json:"delta"`
		Message      openAIDelta `json:"message"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Stats *struct {
		TokensPerSecond  float64 `json:"tokens_per_second"`
		TimeToFirstToken float64 `json:"time_to_first_token"`
		GenerationTime   float64 `json:"generation_time"`
		StopReason       string  `json:"stop_reason"`
	} `json:"stats"`
	ModelInfo *struct {
		ContextLength int    `json:"context_length"`
		Arch          string `json:"arch"`
		Quant         string `json:"quant"`
	} `json:"model_info"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// wireMessage is the message shape sent to OpenAI-compatible servers.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func toWireMessages(messages []Message) []wireMessage {
	out := make([]wireMessage, 0, len(messages))
	for _, m := range messages {
		wm := wireMessage{
			Role:       string(m.Role),
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		if len(m.Images) > 0 {
			parts := []map[string]any{}
			if m.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:image/png;base64," + img},
				})
			}
			wm.Content = parts
		} else {
			wm.Content = m.Content
		}
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				var wt wireToolCall
				wt.ID = tc.ID
				wt.Type = "function"
				wt.Function.Name = tc.Name
				wt.Function.Arguments = tc.Arguments
				wm.ToolCalls = append(wm.ToolCalls, wt)
			}
		}
		out = append(out, wm)
	}
	return out
}

func (p *openAICompatProvider) currentChatPath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chatPathIdx >= len(p.chatPaths) {
		p.chatPathIdx = len(p.chatPaths) - 1
	}
	return p.chatPaths[p.chatPathIdx]
}

func (p *openAICompatProvider) retireChatPath() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.chatPathIdx < len(p.chatPaths)-1 {
		p.chatPathIdx++
	}
}

func (p *openAICompatProvider) Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if err := p.keyCheck(); err != nil {
		return nil, err
	}
	body := map[string]any{
		"model":       req.Model,
		"messages":    toWireMessages(req.Messages),
		"stream":      true,
		"temperature": req.Temperature,
		// OpenAI-compatible servers omit usage from a stream unless it is asked
		// for. Without prompt_tokens the context meter could never show a
		// measured value, only an estimate, so this is requested explicitly.
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var wt wireTool
			wt.Type = "function"
			wt.Function.Name = t.Name
			wt.Function.Description = t.Description
			wt.Function.Parameters = t.Schema
			tools = append(tools, wt)
		}
		body["tools"] = tools
	}

	path := p.currentChatPath()
	rc, err := postStream(ctx, path, p.authHeaders(), body)
	if err != nil {
		if he, ok := err.(*HTTPError); ok && he.StatusCode == 404 && len(p.chatPaths) > 1 {
			p.retireChatPath()
			rc, err = postStream(ctx, p.currentChatPath(), p.authHeaders(), body)
		}
		if err != nil {
			return nil, err
		}
	}

	events := make(chan StreamEvent, 64)
	go p.consume(ctx, rc, events, req.Model)
	return events, nil
}

// consume translates the SSE body into StreamEvents and closes the channel.
func (p *openAICompatProvider) consume(ctx context.Context, rc io.ReadCloser, events chan<- StreamEvent, model string) {
	defer close(events)
	defer rc.Close()

	start := time.Now()
	var (
		ttft          time.Duration
		sawFirstToken bool
		finishReason  string
		usage         *Usage
		stats         *GenerationStats
		ctxLength     int
		arch, quant   string
		textLen       int
	)

	toolCalls := map[int]*ToolCall{}

	send := func(ev StreamEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	reader := newSSEReader(rc)
	for {
		if ctx.Err() != nil {
			return
		}
		payload, err := reader.Next()
		if err != nil {
			// Only a clean end of the body finishes the answer. Anything else — a
			// reset connection, a truncated body — used to end the stream as if the
			// model had stopped, which stores half a reply as a complete one.
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				send(StreamEvent{Type: EventError, Error: fmt.Sprintf("Die Verbindung wurde unterbrochen: %s", err)})
				return
			}
			break
		}
		if len(payload) == 0 {
			continue
		}
		if string(payload) == "[DONE]" {
			break
		}
		var chunk openAIChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// A single malformed frame should not abort a long generation.
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			send(StreamEvent{Type: EventError, Error: chunk.Error.Message})
			return
		}
		if chunk.Usage != nil {
			u := &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
			if chunk.Usage.PromptTokensDetails != nil {
				u.CachedPromptTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			usage = u
		}
		if chunk.Stats != nil {
			stats = &GenerationStats{
				TokensPerSecond: chunk.Stats.TokensPerSecond,
				// LM Studio reports these in seconds.
				TimeToFirstTokenMs: int64(chunk.Stats.TimeToFirstToken * 1000),
				GenerationMs:       int64(chunk.Stats.GenerationTime * 1000),
				StopReason:         chunk.Stats.StopReason,
				Source:             SourceLMStudio,
			}
		}
		if chunk.ModelInfo != nil {
			ctxLength = chunk.ModelInfo.ContextLength
			arch = chunk.ModelInfo.Arch
			quant = chunk.ModelInfo.Quant
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if choice.Message.Content != "" || choice.Message.Reasoning != "" ||
				choice.Message.ReasoningContent != "" || choice.Message.Thinking != "" ||
				len(choice.Message.ToolCalls) > 0 {
				// Non-streaming server: treat the full message as one delta.
				delta = mergeDeltas(delta, choice.Message)
			}

			if r := delta.reasoningText(); r != "" {
				if !sawFirstToken {
					ttft = time.Since(start)
					sawFirstToken = true
				}
				if !send(StreamEvent{Type: EventReasoning, Text: r}) {
					return
				}
			}
			if delta.Content != "" {
				if !sawFirstToken {
					ttft = time.Since(start)
					sawFirstToken = true
				}
				textLen += len(delta.Content)
				if !send(StreamEvent{Type: EventDelta, Text: delta.Content}) {
					return
				}
			}
			for _, tc := range delta.ToolCalls {
				entry, ok := toolCalls[tc.Index]
				if !ok {
					entry = &ToolCall{}
					toolCalls[tc.Index] = entry
				}
				if tc.ID != "" {
					entry.ID = tc.ID
				}
				if tc.Function.Name != "" {
					// The name arrives as fragments, so it is accumulated — except when a
					// server repeats the whole name in every delta, where appending would
					// produce "search_websearch_web" and the call would no longer resolve.
					// A fragment that starts with what we have is the full name again.
					if strings.HasPrefix(tc.Function.Name, entry.Name) {
						entry.Name = tc.Function.Name
					} else {
						entry.Name += tc.Function.Name
					}
				}
				entry.Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	// Emit accumulated tool calls in index order so the engine can execute them.
	indexes := make([]int, 0, len(toolCalls))
	for i := range toolCalls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		tc := toolCalls[i]
		if tc.Name == "" {
			continue
		}
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", i, time.Now().UnixNano())
		}
		if tc.Arguments == "" {
			tc.Arguments = "{}"
		}
		if !send(StreamEvent{Type: EventToolCall, ToolCall: tc}) {
			return
		}
	}

	elapsed := time.Since(start)
	if usage == nil {
		// No usage from the server; derive a conservative estimate so the meter
		// still moves. Marked as estimated so the UI never presents it as fact.
		completion := EstimateTokens(strings.Repeat("x", textLen))
		usage = &Usage{CompletionTokens: completion, TotalTokens: completion}
	}
	if stats == nil || stats.TokensPerSecond == 0 {
		genMs := elapsed.Milliseconds()
		tps := 0.0
		// Measure in microseconds: a short or cached generation can complete
		// inside a single millisecond, and reporting 0 tokens/s for a response
		// that clearly produced tokens would be wrong.
		if micros := elapsed.Microseconds(); micros > 0 && usage.CompletionTokens > 0 {
			tps = float64(usage.CompletionTokens) / (float64(micros) / 1e6)
		}
		stats = &GenerationStats{
			TokensPerSecond:    tps,
			TimeToFirstTokenMs: ttft.Milliseconds(),
			GenerationMs:       genMs,
			StopReason:         finishReason,
			Source:             SourceEstimated,
		}
	} else if stats.TimeToFirstTokenMs == 0 {
		stats.TimeToFirstTokenMs = ttft.Milliseconds()
	}
	if stats.StopReason == "" {
		stats.StopReason = finishReason
	}
	stats.TotalMs = elapsed.Milliseconds()

	send(StreamEvent{Type: EventUsage, Usage: usage})
	if ctxLength > 0 {
		send(StreamEvent{Type: EventModelInfo, ModelInfo: &ModelInfo{
			ID:           model,
			Backend:      p.id,
			ContextLimit: ctxLength,
			Arch:         arch,
			Quantization: quant,
		}})
	}
	send(StreamEvent{Type: EventDone, Stats: stats})
}

func mergeDeltas(a, b openAIDelta) openAIDelta {
	if b.Content != "" {
		a.Content += b.Content
	}
	switch {
	case b.Reasoning != "":
		a.Reasoning += b.Reasoning
	case b.ReasoningContent != "":
		a.ReasoningContent += b.ReasoningContent
	case b.Thinking != "":
		a.Thinking += b.Thinking
	}
	if len(b.ToolCalls) > 0 {
		a.ToolCalls = append(a.ToolCalls, b.ToolCalls...)
	}
	return a
}
