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

// ollamaProvider speaks Ollama's native API. The native API is used rather than
// the OpenAI-compatible shim because it exposes the real context length, the
// model's capability list, and exact eval timings.
type ollamaProvider struct {
	baseURL string

	mu        sync.Mutex
	showCache map[string]ollamaShowInfo
	showAt    map[string]time.Time
}

type ollamaShowInfo struct {
	ContextLength int
	MaxContext    int
	Capabilities  []string
	Arch          string
	Quant         string
	ParamSize     string
}

const ollamaShowTTL = 10 * time.Minute

func newOllamaProvider(baseURL string) *ollamaProvider {
	return &ollamaProvider{
		baseURL:   strings.TrimRight(baseURL, "/"),
		showCache: map[string]ollamaShowInfo{},
		showAt:    map[string]time.Time{},
	}
}

func (p *ollamaProvider) ID() string   { return "ollama" }
func (p *ollamaProvider) Name() string { return "Ollama" }

// ReplaysReasoning reports that every Ollama request carries an assistant reply's reasoning:
// toOllamaMessages fills the message's `thinking` field from the stored row, tools or not.
func (p *ollamaProvider) ReplaysReasoning(bool) bool { return true }

func (p *ollamaProvider) Health(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.baseURL+"/api/version", nil, nil, &v); err != nil {
		// The cause is wrapped rather than printed so callers can inspect it;
		// the message is unchanged.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// --- model discovery -------------------------------------------------------

type ollamaTagEntry struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	Details    struct {
		Family            string `json:"family"`
		ParameterSize     string `json:"parameter_size"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

type ollamaPsEntry struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Size          int64  `json:"size"`
	SizeVRAM      int64  `json:"size_vram"`
	ContextLength int    `json:"context_length"`
}

func (p *ollamaProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var tags struct {
		Models []ollamaTagEntry `json:"models"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.baseURL+"/api/tags", nil, nil, &tags); err != nil {
		return nil, err
	}

	// Running models tell us the *effective* context length, which is what the
	// meter should compare against. Failure here is not fatal.
	running := map[string]ollamaPsEntry{}
	var ps struct {
		Models []ollamaPsEntry `json:"models"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodGet, p.baseURL+"/api/ps", nil, nil, &ps); err == nil {
		for _, m := range ps.Models {
			running[m.Name] = m
			running[m.Model] = m
		}
	}

	out := make([]ModelInfo, 0, len(tags.Models))
	for _, t := range tags.Models {
		info := ModelInfo{
			ID:             t.Name,
			Backend:        "ollama",
			Arch:           t.Details.Family,
			Quantization:   t.Details.QuantizationLevel,
			ParameterCount: t.Details.ParameterSize,
			State:          StateNotLoaded,
			Type:           "llm",
		}
		if _, ok := running[t.Name]; ok {
			info.State = StateLoaded
		}
		if sh, ok := p.cachedShow(t.Name); ok {
			applyOllamaShow(&info, sh)
		}
		if r, ok := running[t.Name]; ok && r.ContextLength > 0 {
			info.ContextLimit = r.ContextLength
		}
		out = append(out, info)
	}

	// Enrich anything missing capability data from /api/show, bounded so a large
	// model library does not stall startup.
	p.enrichShows(ctx, out)

	for i := range out {
		if sh, ok := p.cachedShow(out[i].ID); ok {
			applyOllamaShow(&out[i], sh)
		}
		if r, ok := running[out[i].ID]; ok && r.ContextLength > 0 {
			out[i].ContextLimit = r.ContextLength
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func applyOllamaShow(info *ModelInfo, sh ollamaShowInfo) {
	if sh.ContextLength > 0 {
		info.MaxContext = sh.ContextLength
		if info.ContextLimit == 0 {
			info.ContextLimit = sh.ContextLength
		}
	}
	if len(sh.Capabilities) > 0 {
		info.Capabilities = sh.Capabilities
	}
	if sh.Arch != "" {
		info.Arch = sh.Arch
	}
	if sh.Quant != "" {
		info.Quantization = sh.Quant
	}
	if sh.ParamSize != "" {
		info.ParameterCount = sh.ParamSize
	}
}

// InvalidateCache drops the cached /api/show detail so the next listing reads it
// again. Needed because ollamaShowTTL is long enough that a model reloaded with a
// different num_ctx would otherwise keep reporting its previous window.
func (p *ollamaProvider) InvalidateCache() {
	p.mu.Lock()
	p.showCache = map[string]ollamaShowInfo{}
	p.showAt = map[string]time.Time{}
	p.mu.Unlock()
}

func (p *ollamaProvider) cachedShow(model string) (ollamaShowInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.showCache[model]
	if !ok {
		return ollamaShowInfo{}, false
	}
	if time.Since(p.showAt[model]) > ollamaShowTTL {
		return ollamaShowInfo{}, false
	}
	return info, true
}

func (p *ollamaProvider) enrichShows(ctx context.Context, models []ModelInfo) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range models {
		if _, ok := p.cachedShow(models[i].ID); ok {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			_, _ = p.Show(ctx, id)
		}(models[i].ID)
	}
	wg.Wait()
}

// Show fetches and caches per-model detail from /api/show.
func (p *ollamaProvider) Show(ctx context.Context, model string) (ollamaShowInfo, error) {
	var resp struct {
		Details struct {
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
		ModelInfo    map[string]any `json:"model_info"`
		Capabilities []string       `json:"capabilities"`
	}
	if err := doJSON(ctx, jsonClient, http.MethodPost, p.baseURL+"/api/show", nil, map[string]string{"model": model}, &resp); err != nil {
		return ollamaShowInfo{}, err
	}

	info := ollamaShowInfo{
		Arch:         resp.Details.Family,
		Quant:        resp.Details.QuantizationLevel,
		ParamSize:    resp.Details.ParameterSize,
		Capabilities: mapOllamaCapabilities(resp.Capabilities),
	}
	// The context length key is namespaced by architecture ("llama.context_length",
	// "qwen3.context_length", …) so we cannot hard-code one key.
	for k, v := range resp.ModelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		n, ok := v.(float64)
		if !ok {
			continue
		}
		if int(n) > info.ContextLength {
			info.ContextLength = int(n)
		}
	}

	p.mu.Lock()
	p.showCache[model] = info
	p.showAt[model] = time.Now()
	p.mu.Unlock()
	return info, nil
}

func mapOllamaCapabilities(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		switch c {
		case "tools":
			out = append(out, CapTools)
		case "vision":
			out = append(out, CapVision)
		case "thinking":
			out = append(out, CapThink)
		case "completion":
			// implicit
		default:
			out = append(out, c)
		}
	}
	return out
}

// --- chat ------------------------------------------------------------------

type ollamaMessage struct {
	Role      string   `json:"role"`
	Content   string   `json:"content"`
	Thinking  string   `json:"thinking,omitempty"`
	Images    []string `json:"images,omitempty"`
	ToolCalls []struct {
		Function struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
}

type ollamaChatResponse struct {
	Model      string        `json:"model"`
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
	Error      string        `json:"error"`

	TotalDuration         int64 `json:"total_duration"`
	LoadDuration          int64 `json:"load_duration"`
	PromptEvalCount       int64 `json:"prompt_eval_count"`
	PromptEvalCachedCount int64 `json:"prompt_eval_cached_count"`
	PromptEvalDuration    int64 `json:"prompt_eval_duration"`
	EvalCount             int64 `json:"eval_count"`
	EvalDuration          int64 `json:"eval_duration"`
}

// toOllamaMessages maps our messages onto Ollama's wire shape.
//
// A stored tool call whose arguments are not valid JSON is replayed verbatim
// under _raw_arguments rather than as an empty object. An empty object tells the
// model its own earlier call took no arguments, which is a different story from
// "the arguments are unreadable" — and a conversation is replayed on every turn,
// so a rewritten turn would follow the model through the rest of the chat.
func toOllamaMessages(in []Message) []ollamaMessage {
	msgs := make([]ollamaMessage, 0, len(in))
	for _, m := range in {
		om := ollamaMessage{
			Role:     string(m.Role),
			Content:  m.Content,
			Thinking: m.Reasoning,
		}
		// A picture is a user turn's part, the same rule the OpenAI wire follows:
		// a generated picture is filed on the assistant row that asked for it, and
		// no provider's assistant turn takes an image (GROQ.md §6).
		if m.Role != RoleAssistant {
			om.Images = m.Images
		}
		if m.Role == RoleTool {
			om.ToolName = m.Name
		}
		for _, tc := range m.ToolCalls {
			var call struct {
				Function struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			}
			call.Function.Name = tc.Name
			args := map[string]any{}
			if tc.Arguments != "" {
				if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
					args = map[string]any{"_raw_arguments": tc.Arguments}
				}
			}
			call.Function.Arguments = args
			om.ToolCalls = append(om.ToolCalls, call)
		}
		msgs = append(msgs, om)
	}
	return msgs
}

// ollamaOptions is the options block of a request. It is a function rather than a few
// lines inside Chat so the one rule here that is easy to get wrong can be tested without
// a server: a temperature of zero goes on the wire only when the caller said it meant
// zero. Ollama reads a missing key as "use the model's own", and for greedy decoding
// that is the wrong answer — the modelfile's temperature applies instead, which makes two
// captions of one request differ for no reason the caller can see.
func ollamaOptions(req ChatRequest) map[string]any {
	options := map[string]any{}
	if req.Temperature > 0 || req.TemperatureSet {
		options["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		options["top_p"] = req.TopP
	}
	if req.NumCtx > 0 {
		options["num_ctx"] = req.NumCtx
	}
	if req.MaxTokens > 0 {
		options["num_predict"] = req.MaxTokens
	}
	if req.Seed != nil {
		options["seed"] = *req.Seed
	}
	return options
}

func (p *ollamaProvider) Chat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	msgs := toOllamaMessages(req.Messages)

	options := ollamaOptions(req)

	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}
	if len(options) > 0 {
		body["options"] = options
	}
	if req.KeepAlive != "" {
		body["keep_alive"] = req.KeepAlive
	}
	// Ollama is the one backend with a documented thinking switch, so a disabled
	// summariser call skips thinking entirely rather than spending its budget on it.
	if req.Think != nil {
		body["think"] = *req.Think
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Schema,
				},
			})
		}
		body["tools"] = tools
	}

	rc, err := postStream(ctx, p.baseURL+"/api/chat", nil, body)
	if err != nil {
		return nil, err
	}

	events := make(chan StreamEvent, 64)
	go p.consume(ctx, rc, events, req.Model)
	return events, nil
}

func (p *ollamaProvider) consume(ctx context.Context, rc io.ReadCloser, events chan<- StreamEvent, model string) {
	defer close(events)
	defer rc.Close()

	start := time.Now()
	var (
		ttft          time.Duration
		sawFirstToken bool
		textLen       int
	)

	send := func(ev StreamEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	reader := newNDJSONReader(rc)
	for {
		if ctx.Err() != nil {
			return
		}
		var resp ollamaChatResponse
		if err := reader.Next(&resp); err != nil {
			// As in the OpenAI-compatible provider: anything other than a clean end
			// of the body is a broken answer, not a finished one.
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				send(StreamEvent{Type: EventError, Error: fmt.Sprintf("Die Verbindung wurde unterbrochen: %s", err)})
				return
			}
			break
		}
		if resp.Error != "" {
			send(StreamEvent{Type: EventError, Error: resp.Error})
			return
		}

		if resp.Message.Thinking != "" {
			if !sawFirstToken {
				ttft = time.Since(start)
				sawFirstToken = true
			}
			if !send(StreamEvent{Type: EventReasoning, Text: resp.Message.Thinking}) {
				return
			}
		}
		if resp.Message.Content != "" {
			if !sawFirstToken {
				ttft = time.Since(start)
				sawFirstToken = true
			}
			textLen += len(resp.Message.Content)
			if !send(StreamEvent{Type: EventDelta, Text: resp.Message.Content}) {
				return
			}
		}
		for i, tc := range resp.Message.ToolCalls {
			args, _ := json.Marshal(tc.Function.Arguments)
			call := &ToolCall{
				ID:        fmt.Sprintf("call_%s_%d_%d", model, time.Now().UnixNano(), i),
				Name:      tc.Function.Name,
				Arguments: string(args),
			}
			if !send(StreamEvent{Type: EventToolCall, ToolCall: call}) {
				return
			}
		}

		if resp.Done {
			usage := &Usage{
				PromptTokens:       int(resp.PromptEvalCount),
				CompletionTokens:   int(resp.EvalCount),
				CachedPromptTokens: int(resp.PromptEvalCachedCount),
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			if usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
				usage.CompletionTokens = EstimateTokens(strings.Repeat("x", textLen))
				usage.TotalTokens = usage.CompletionTokens
			}

			stats := &GenerationStats{
				TimeToFirstTokenMs: ttft.Milliseconds(),
				TotalMs:            resp.TotalDuration / int64(time.Millisecond),
				LoadMs:             resp.LoadDuration / int64(time.Millisecond),
				GenerationMs:       resp.EvalDuration / int64(time.Millisecond),
				StopReason:         resp.DoneReason,
				Source:             SourceOllama,
			}
			// Documented formula: eval_count / eval_duration * 10^9.
			if resp.EvalDuration > 0 && resp.EvalCount > 0 {
				stats.TokensPerSecond = float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
			} else if stats.GenerationMs > 0 && usage.CompletionTokens > 0 {
				stats.TokensPerSecond = float64(usage.CompletionTokens) / (float64(stats.GenerationMs) / 1000)
				stats.Source = SourceEstimated
			}
			if stats.TimeToFirstTokenMs == 0 && resp.LoadDuration > 0 {
				stats.TimeToFirstTokenMs = (resp.LoadDuration + resp.PromptEvalDuration) / int64(time.Millisecond)
			}

			send(StreamEvent{Type: EventUsage, Usage: usage})
			send(StreamEvent{Type: EventDone, Stats: stats})
			return
		}
	}
}
