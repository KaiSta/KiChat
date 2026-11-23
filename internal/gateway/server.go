package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"kichat/internal/llm"
)

type Options struct {
	Key           string
	Model         string
	ClientModel   string
	Scope         string
	ContextLimit  int
	ReserveTokens int
	Compact       bool
}

func (o Options) clientModel() string {
	if o.ClientModel == "" {
		return ModelName
	}
	return o.ClientModel
}

// Resolve snapshots the configured provider and limits after a request acquires
// the generation slot, so queued requests do not use a replaced model.
type Resolve func(context.Context) (llm.Provider, Options, error)

type Report struct {
	ID               string `json:"id"`
	Before           int    `json:"before"`
	After            int    `json:"after"`
	Compacted        bool   `json:"compacted"`
	CacheHit         bool   `json:"cacheHit"`
	CompactMS        int64  `json:"compactMs"`
	GenerationMS     int64  `json:"generationMs"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
	UsageReported    bool   `json:"usageReported"`
	Error            string `json:"error"`
}

type Status struct {
	Running bool    `json:"running"`
	URL     string  `json:"url"`
	Busy    bool    `json:"busy"`
	Error   string  `json:"error"`
	Last    *Report `json:"last,omitempty"`
}

type Server struct {
	mu      sync.Mutex
	http    *http.Server
	cancel  context.CancelFunc
	status  Status
	resolve Resolve
	gate    chan struct{}
	// Only the holder of gate touches summaries. No dialogue is persisted.
	cache map[[32]byte]string
}

func New(resolve Resolve) *Server {
	return &Server{resolve: resolve, gate: make(chan struct{}, 1), cache: make(map[[32]byte]string)}
}

func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.status
	if v.Last != nil {
		r := *v.Last
		v.Last = &r
	}
	return v
}

func (s *Server) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return nil
	}
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		s.status.Error = err.Error()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }, MaxHeaderBytes: 32 << 10}
	s.http = srv
	s.status = Status{Running: true, URL: "http://" + l.Addr().String() + "/v1"}
	go func() {
		err := srv.Serve(l)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.http != srv {
			return
		}
		s.http = nil
		s.status.Running = false
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.status.Error = err.Error()
		}
	}()
	return nil
}

func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.http
	if s.cancel != nil {
		s.cancel()
	}
	s.http = nil
	s.status.Running = false
	s.mu.Unlock()
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	p, opt, err := s.resolve(ctx)
	if err != nil {
		fail(w, 503, "backend_unavailable", err.Error())
		return
	}
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || opt.Key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(opt.Key)) != 1 {
		fail(w, 401, "invalid_api_key", "a valid KiChat API key is required")
		return
	}
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		if err := writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{"id": opt.clientModel(), "object": "model", "created": 0, "owned_by": "kichat"}}}); err != nil {
			s.mu.Lock()
			s.status.Error = err.Error()
			s.mu.Unlock()
		}
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		fail(w, 404, "not_found", "endpoint not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	in, req, err := decode(r.Body)
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		fail(w, 408, "request_cancelled", ctx.Err().Error())
		return
	}
	report := Report{ID: "chatcmpl-" + uuid.New().String()}
	s.mu.Lock()
	s.status.Busy = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.status.Busy = false; s.status.Last = &report; s.mu.Unlock() }()
	p, opt, err = s.resolve(ctx)
	if err != nil {
		report.Error = err.Error()
		fail(w, 503, "backend_unavailable", report.Error)
		return
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(opt.Key)) != 1 {
		report.Error = "API key changed"
		fail(w, 401, "invalid_api_key", report.Error)
		return
	}
	if in.Model != opt.clientModel() {
		report.Error = fmt.Sprintf("unknown model %q; use %q from KiChat settings", in.Model, opt.clientModel())
		fail(w, 404, "model_not_found", report.Error)
		return
	}
	req.Model = opt.Model
	req, err = s.prepare(ctx, p, req, opt, &report)
	if err != nil {
		report.Error = err.Error()
		fail(w, 400, "context_length_exceeded", report.Error)
		return
	}
	started := time.Now()
	defer func() { report.GenerationMS = time.Since(started).Milliseconds() }()
	events, err := p.Chat(ctx, req)
	if err != nil {
		report.Error = err.Error()
		fail(w, 502, "upstream_error", report.Error)
		return
	}
	s.respond(ctx, w, in, events, &report)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	if err := writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": code, "code": code}}); err != nil {
		log.Printf("[gateway] could not write error response: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(body)
}

func (s *Server) respond(ctx context.Context, w http.ResponseWriter, in request, events <-chan llm.StreamEvent, report *Report) {
	created := time.Now().Unix()
	started := false
	var content, reasoning strings.Builder
	var calls []llm.ToolCall
	var usage *llm.Usage
	reason := "stop"
	done := false
	frame := func(choices []any, u any) map[string]any {
		v := map[string]any{"id": report.ID, "object": "chat.completion.chunk", "created": created, "model": in.Model, "choices": choices}
		if u != nil {
			v["usage"] = u
		}
		return v
	}
	emit := func(body any) bool {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			started = true
		}
		raw, err := json.Marshal(body)
		if err == nil {
			_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		if err != nil {
			report.Error = err.Error()
			return false
		}
		return true
	}
	delta := func(d any) bool {
		return emit(frame([]any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}}, nil))
	}
	for !done {
		select {
		case <-ctx.Done():
			report.Error = ctx.Err().Error()
			done = true
		case ev, ok := <-events:
			if !ok {
				report.Error = "upstream ended without a completion marker"
				done = true
				break
			}
			switch ev.Type {
			case llm.EventDelta:
				content.WriteString(ev.Text)
				if in.Stream && !delta(map[string]any{"role": "assistant", "content": ev.Text}) {
					return
				}
			case llm.EventReasoning:
				reasoning.WriteString(ev.Text)
				if in.Stream && !delta(map[string]any{"reasoning_content": ev.Text}) {
					return
				}
			case llm.EventToolCall:
				if ev.ToolCall != nil {
					index := len(calls)
					calls = append(calls, *ev.ToolCall)
					c := callsForWire([]llm.ToolCall{*ev.ToolCall})[0]
					if in.Stream && !delta(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": c.ID, "type": c.Type, "function": c.Function}}}) {
						return
					}
				}
			case llm.EventUsage:
				usage = ev.Usage
			case llm.EventError:
				report.Error = ev.Error
				if report.Error == "" {
					report.Error = "upstream generation failed"
				}
				done = true
			case llm.EventDone:
				if ev.Stats != nil && llm.Truncated(ev.Stats.StopReason) {
					reason = "length"
				}
				done = true
			}
		}
	}
	if report.Error != "" {
		if started {
			emit(map[string]any{"error": map[string]string{"type": "upstream_error", "message": report.Error}})
		} else {
			fail(w, 502, "upstream_error", report.Error)
		}
		return
	}
	if len(calls) > 0 && reason != "length" {
		reason = "tool_calls"
	}
	var u any
	if usage != nil {
		report.PromptTokens, report.CompletionTokens, report.UsageReported = usage.PromptTokens, usage.CompletionTokens, true
		u = map[string]any{"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens,
			"prompt_tokens_details": map[string]int{"cached_tokens": usage.CachedPromptTokens}, "completion_tokens_details": map[string]int{"reasoning_tokens": usage.ReasoningTokens}}
	}
	if in.Stream {
		if !emit(frame([]any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}, nil)) {
			return
		}
		if in.StreamOptions != nil && in.StreamOptions.IncludeUsage && u != nil {
			if !emit(frame([]any{}, u)) {
				return
			}
		}
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
			report.Error = err.Error()
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			report.Error = err.Error()
		}
		return
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		msg["tool_calls"] = callsForWire(calls)
	}
	result := map[string]any{"id": report.ID, "object": "chat.completion", "created": created, "model": in.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": reason}}}
	if u != nil {
		result["usage"] = u
	}
	if err := writeJSON(w, 200, result); err != nil {
		report.Error = err.Error()
	}
}
