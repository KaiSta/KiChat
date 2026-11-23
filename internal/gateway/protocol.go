// Package gateway serves external chat clients without executing their tools or
// importing their conversations into KiChat's own chat history.
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"kichat/internal/llm"
)

const ModelName = "kichat"

type function struct {
	Name        string         `json:"name"`
	Arguments   string         `json:"arguments,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}
type wireCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function function `json:"function"`
}
type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"`
	ToolCalls  []wireCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}
type request struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []struct {
		Type     string   `json:"type"`
		Function function `json:"function"`
	} `json:"tools,omitempty"`
	ToolChoice    string `json:"tool_choice,omitempty"`
	Stream        bool   `json:"stream,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens           int      `json:"max_tokens,omitempty"`
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	Seed                *int     `json:"seed,omitempty"`
	ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
}

func decode(r io.Reader) (request, llm.ChatRequest, error) {
	var in request
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, llm.ChatRequest{}, fmt.Errorf("invalid request: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return in, llm.ChatRequest{}, fmt.Errorf("expected one JSON object")
	}
	out, err := in.normalise()
	return in, out, err
}

func (in request) normalise() (llm.ChatRequest, error) {
	out := llm.ChatRequest{Seed: in.Seed}
	bad := func(s string) (llm.ChatRequest, error) { return out, fmt.Errorf("%s", s) }
	if strings.TrimSpace(in.Model) == "" {
		return bad("model must name the alias configured in KiChat settings")
	}
	if len(in.Messages) == 0 {
		return bad("messages must not be empty")
	}
	if in.MaxTokens < 0 || in.MaxCompletionTokens < 0 || (in.MaxTokens > 0 && in.MaxCompletionTokens > 0) {
		return bad("supply one positive output limit")
	}
	out.MaxTokens = max(in.MaxTokens, in.MaxCompletionTokens)
	if in.Temperature != nil {
		if *in.Temperature < 0 || *in.Temperature > 2 {
			return bad("temperature must be between 0 and 2")
		}
		out.Temperature, out.TemperatureSet = *in.Temperature, true
	}
	if in.TopP != nil {
		if *in.TopP <= 0 || *in.TopP > 1 {
			return bad("top_p must be greater than 0 and at most 1")
		}
		out.TopP = *in.TopP
	}
	switch in.ReasoningEffort {
	case "":
	case "none":
		v := false
		out.Think = &v
	case "low", "medium", "high":
		v := true
		out.Think = &v
	default:
		return bad("unsupported reasoning_effort")
	}
	switch in.ToolChoice {
	case "", "auto", "none":
	default:
		return bad("only auto and none tool_choice are supported")
	}
	seenTools := map[string]bool{}
	for _, t := range in.Tools {
		if t.Type != "function" || strings.TrimSpace(t.Function.Name) == "" || seenTools[t.Function.Name] {
			return bad("tools must have unique function names")
		}
		seenTools[t.Function.Name] = true
		if in.ToolChoice != "none" {
			out.Tools = append(out.Tools, llm.ToolDefinition{Name: t.Function.Name, Description: t.Function.Description, Schema: t.Function.Parameters})
		}
	}
	pending, ids := map[string]bool{}, map[string]bool{}
	for _, m := range in.Messages {
		text, err := textContent(m.Content)
		if err != nil {
			return bad(err.Error())
		}
		msg := llm.Message{Role: llm.Role(m.Role), Content: text, Name: m.Name, Reasoning: m.Reasoning, ToolCallID: m.ToolCallID}
		if m.Role != "tool" && len(pending) > 0 {
			return bad("every tool call must have its result before the next message")
		}
		switch msg.Role {
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant:
			if m.ToolCallID != "" {
				return bad("tool_call_id is only valid on tool messages")
			}
		case llm.RoleTool:
			if !pending[m.ToolCallID] {
				return bad("tool result has no matching pending call")
			}
			delete(pending, m.ToolCallID)
		default:
			return bad("supported roles are system, user, assistant and tool")
		}
		if len(m.ToolCalls) > 0 && msg.Role != llm.RoleAssistant {
			return bad("only assistant messages can contain tool_calls")
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" || ids[c.ID] || c.Type != "function" || c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments)) {
				return bad("invalid or duplicate function tool call")
			}
			ids[c.ID], pending[c.ID] = true, true
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
		}
		out.Messages = append(out.Messages, msg)
	}
	if len(pending) > 0 {
		return bad("tool results are missing; execute the requested tools before continuing")
	}
	return out, nil
}

func textContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&parts); err != nil {
		return "", fmt.Errorf("content must be text or text parts; images are not supported")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("only text content parts are supported")
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func callsForWire(calls []llm.ToolCall) []wireCall {
	out := make([]wireCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, wireCall{ID: c.ID, Type: "function", Function: function{Name: c.Name, Arguments: c.Arguments}})
	}
	return out
}
