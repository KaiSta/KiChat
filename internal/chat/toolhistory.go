package chat

import (
	"fmt"
	"strings"

	"kichat/internal/llm"
	"kichat/internal/tools"
)

// A tool call has to name a tool. Two things can break that: a model that mangles
// the name (Qwen and Llama builds on Groq have been seen folding an extra word into
// `function.name` — the observed failure quotes `scr scrape_url` where the request
// declared `scrape_url`), and a stored history that carries such a name forward.
//
// Groq refuses the *generated* call with HTTP 400 "attempted to call tool … which
// was not in request.tools" before a token streams, so nothing reaches the app and no
// history is written — the turn simply fails. A backend that does not validate (a
// local llama.cpp, LM Studio, Ollama) lets the same call through, and there the app
// decides: it executes the name it was given, answers `unbekanntes Tool`, and stores
// the call in the assistant row every later turn replays.
//
// This file is that decision: a mangled name is repaired where the intent is
// unambiguous, dropped where it is not, and a history that already holds a name the
// app cannot execute is demoted to plain text rather than sent as a tool call.
const (
	// renamedToolNote is the warning a repaired name produces, so a call that ran
	// under a name the model invented is explained rather than silently corrected.
	renamedToolNote = "Der Tool-Aufruf „%s“ wurde als „%s“ ausgeführt — das Modell hatte den Namen verfälscht."
	// aliasedToolNote is the same for a name that is not mangled but borrowed: `bash`
	// where this app says `run_command`. It says which convention the name came from,
	// because "verfälscht" would be wrong about a name the model wrote exactly right.
	aliasedToolNote = "Der Tool-Aufruf „%s“ wurde als „%s“ ausgeführt — „%s“ ist der Name, den andere Assistenten dafür verwenden."
	// droppedToolNote is the warning for a name nothing matches.
	droppedToolNote = "Der Tool-Aufruf „%s“ wurde verworfen: diesen Namen kennt die App nicht."
	// droppedToolText is what the transcript says when no call survived, so a turn
	// does not end on an empty answer nobody can explain.
	droppedToolText = "[Der Modell-Aufruf „%s“ wurde verworfen: diesen Namen kennt die App nicht.]"
)

// toolNameSet turns names into a lookup.
func toolNameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = true
		}
	}
	return set
}

// definitionNames is the names of the tool definitions, in the form the repair and the
// sanitiser both want.
func definitionNames(defs []llm.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	return names
}

// toolAliases are the names *other* tool conventions use for tools this app ships.
//
// They exist because a model that has been trained or prompted on another harness writes
// `bash` where this app's request declares `run_command` — a name it got exactly right, in
// a vocabulary this app never taught it. The repair below is about a name the model mangled
// (`scr scrape_url`); this table is about a name it borrowed, and the two arrive through
// the same door, so they are resolved in one place.
//
// Two rules keep it from becoming a way around the tool list. A target resolves only when
// this request actually offers it, so an alias can never hand a model a tool the turn
// withheld — `bash` in a research chat, where `run_command` is not offered, stays an
// unknown name. And a name with more than one plausible target is not listed at all: `find`
// could mean find_files or search_files, and a wrong tool with the right arguments is the
// expensive shape of that mistake.
var toolAliases = map[string][]string{
	"bash":       {tools.ToolRunCommand},
	"shell":      {tools.ToolRunCommand},
	"sh":         {tools.ToolRunCommand},
	"terminal":   {tools.ToolRunCommand},
	"command":    {tools.ToolRunCommand},
	"run":        {tools.ToolRunCommand},
	"read":       {tools.ToolReadFile},
	"cat":        {tools.ToolReadFile},
	"write":      {tools.ToolWriteFile},
	"edit":       {tools.ToolEditFile},
	"patch":      {tools.ToolEditFile},
	"ls":         {tools.ToolListDir},
	"list":       {tools.ToolListDir},
	"grep":       {tools.ToolSearchFiles},
	"rg":         {tools.ToolSearchFiles},
	"glob":       {tools.ToolFindFiles},
	"fetch":      {tools.ToolScrapeURL},
	"browse":     {tools.ToolScrapeURL},
	"web_search": {tools.ToolSearchWeb},
}

// repairToolName answers the name a model's mangled (or borrowed) one probably meant, or
// "" when nothing can be said with confidence. The second value says the name was an alias,
// which only changes the sentence the user is shown.
//
// The rule is deliberately conservative. An exact name wins; a name that differs only
// in case or surrounding whitespace resolves; a name whose word-tokens contain
// exactly one known tool name resolves to it; and a name from another convention's
// vocabulary resolves through toolAliases. Two candidates mean the model meant
// something this function cannot know, and a wrong tool with the right arguments is
// worse than a refused call — `generate_image` handed a scrape's URL as its prompt is
// the expensive shape of that mistake.
func repairToolName(name string, known map[string]bool) (string, bool) {
	trimmed := strings.TrimSpace(strings.Trim(name, "`\"'"))
	if trimmed == "" {
		return "", false
	}
	if known[trimmed] {
		return trimmed, false
	}
	lower := strings.ToLower(trimmed)
	for candidate := range known {
		if strings.ToLower(candidate) == lower {
			return candidate, false
		}
	}
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	})
	match := ""
	for _, word := range words {
		if !known[word] {
			continue
		}
		if match != "" && match != word {
			return "", false
		}
		match = word
	}
	if match != "" {
		return match, false
	}
	for _, target := range toolAliases[lower] {
		if known[target] {
			return target, true
		}
	}
	return "", false
}

// resolveToolCalls repairs a mangled name where the intent is unambiguous, and decides
// what happens to a call no tool answers.
//
// dropUnknown is the one thing the two tool modes disagree about, and it follows where
// the name came from. A native call is generated against a `tools` array the vendor
// validates: either the request never reached the app (Groq answers 400 before a token
// streams) or it reached a backend that tolerates any name (llama.cpp), and the app is
// then the only check there is — so the call is dropped, because executing a tool on a
// guessed name is worse than refusing it.
//
// A text-protocol call is written into prose by a model the app itself asked to write
// it, and no vendor sees the name. There the error is the useful answer: it names what
// the model asked for, the model can correct itself on the next round, and a history
// that still carries the name is demoted to text when it is replayed (sanitiseToolHistory).
func resolveToolCalls(calls []llm.ToolCall, known []string, dropUnknown bool) (kept []llm.ToolCall,
	notes []string, dropped []string) {

	names := toolNameSet(known)
	for _, call := range calls {
		if names[call.Name] {
			kept = append(kept, call)
			continue
		}
		if repaired, aliased := repairToolName(call.Name, names); repaired != "" {
			if aliased {
				notes = append(notes, fmt.Sprintf(aliasedToolNote, call.Name, repaired, call.Name))
			} else {
				notes = append(notes, fmt.Sprintf(renamedToolNote, call.Name, repaired))
			}
			call.Name = repaired
			kept = append(kept, call)
			continue
		}
		if !dropUnknown {
			kept = append(kept, call)
			continue
		}
		notes = append(notes, fmt.Sprintf(droppedToolNote, call.Name))
		dropped = append(dropped, call.Name)
	}
	return kept, notes, dropped
}

// renderToolCallText is how a native call reads once it is history rather than a
// request field: the text protocol's own convention, which every model in the app has
// been taught.
func renderToolCallText(call llm.ToolCall) string {
	args := call.Arguments
	if args == "" {
		args = "{}"
	}
	return fmt.Sprintf("[Tool-Aufruf: %s %s]", call.Name, args)
}

// renderToolResultText marks a result whose call is no longer part of the request.
// The result itself is kept — it may be the only record of what a page said — but it
// travels as a user turn, because a `tool` message without the call it answers is the
// one shape a chat template rejects outright.
func renderToolResultText(name, content string) string {
	label := name
	if label == "" {
		label = "unbekannt"
	}
	return fmt.Sprintf("[Tool-Ergebnis von %s]\n%s", label, content)
}

// sanitiseToolHistory rewrites an assembled history so that no tool call in it names
// a tool the app cannot execute, and returns the token delta the rewrite costs so the
// budget stays honest.
//
// Only *unknown* names are touched. A known tool that is not offered in this
// particular request — `search_web` while the search providers are down, `read_pages`
// on a store without the index — is left alone: the app can still answer that call,
// and rewriting a history because a setting is momentarily different would lose the
// record of what was actually read.
//
// Messages are in conversation order, so a call is seen before the result that answers
// it; a result whose call was never seen is demoted for the same reason.
func sanitiseToolHistory(msgs []llm.Message, known map[string]bool) ([]llm.Message, int) {
	delta := 0
	kept := map[string]bool{}
	for i := range msgs {
		m := &msgs[i]
		before := llm.EstimateTokens(m.Content)

		if m.Role == llm.RoleTool {
			if !kept[m.ToolCallID] {
				m.Role = llm.RoleUser
				m.Content = renderToolResultText(m.Name, m.Content)
				m.Name, m.ToolCallID = "", ""
				delta += llm.EstimateTokens(m.Content) - before
			}
			continue
		}
		if len(m.ToolCalls) == 0 {
			continue
		}
		callKept := make([]llm.ToolCall, 0, len(m.ToolCalls))
		for _, call := range m.ToolCalls {
			name := call.Name
			if !known[name] {
				name, _ = repairToolName(name, known)
			}
			if name == "" {
				m.Content = joinText(m.Content, fmt.Sprintf(droppedToolText, call.Name))
				continue
			}
			call.Name = name
			kept[call.ID] = true
			callKept = append(callKept, call)
		}
		m.ToolCalls = callKept
		delta += llm.EstimateTokens(m.Content) - before
	}
	return msgs, delta
}
