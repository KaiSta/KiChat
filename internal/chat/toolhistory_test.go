package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/research"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
)

// TestRepairToolName pins the rule that decides what a mangled name meant. It is
// deliberately narrow: an exact name, a case or whitespace difference, or exactly one
// known name among the words — and nothing when two candidates compete, because a
// wrong tool with the right arguments is worse than a refused call.
func TestRepairToolName(t *testing.T) {
	known := toolNameSet([]string{"search_web", "scrape_url", "read_pages"})
	for _, tc := range []struct{ in, want string }{
		// The shape Groq refused: an extra word folded into the name.
		{"scr scrape_url", "scrape_url"},
		{"scrape_url", "scrape_url"},
		{"  scrape_url\n", "scrape_url"},
		{"Scrape_Url", "scrape_url"},
		{`"scrape_url"`, "scrape_url"},
		{"search_web / scrape_url", ""}, // two candidates: nothing is certain
		{"scrape", ""},
		{"", ""},
	} {
		if got, _ := repairToolName(tc.in, known, nil); got != tc.want {
			t.Errorf("repairToolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestANameFromAnotherConventionResolves pins the borrowed vocabulary — the report that
// started it: a local model wrote `bash` where the request declared `run_command` and the
// turn answered "diesen Namen kennt die App nicht". The table is the fix, and the second
// half is its limit: an alias resolves only to a tool this request offers, so it can never
// hand a turn a tool that was withheld from it.
func TestANameFromAnotherConventionResolves(t *testing.T) {
	project := toolNameSet([]string{"read_file", "run_command", "edit_file"})
	if got, aliased := repairToolName("bash", project, project); got != "run_command" || !aliased {
		t.Errorf("bash resolved to %q (aliased=%v), want run_command through the alias table", got, aliased)
	}
	for _, name := range []string{"read", "cat"} {
		if got, _ := repairToolName(name, project, project); got != "read_file" {
			t.Errorf("%s resolved to %q, want read_file", name, got)
		}
	}
	research := toolNameSet([]string{"search_web", "scrape_url"})
	if got, _ := repairToolName("bash", research, research); got != "" {
		t.Errorf("bash resolved to %q in a request that does not offer run_command", got)
	}
	// A mangled name is still reported as mangled: `verfälscht` would be the wrong sentence
	// for a name the model wrote exactly as it learned it.
	scrape := toolNameSet([]string{"scrape_url"})
	if _, aliased := repairToolName("scr scrape_url", scrape, scrape); aliased {
		t.Error("a repaired name was reported as an alias")
	}

	// And through the resolver a native call goes through, with the note the user sees. The
	// note is a dictionary code, so what is pinned is the code and its arguments rather than a
	// German sentence an English window would have shown as it was.
	kept, notes, dropped := resolveToolCalls([]llm.ToolCall{
		{ID: "call-1", Name: "bash", Arguments: `{"command":"echo hi"}`},
	}, []string{"run_command"}, []string{"run_command"}, true)
	if len(kept) != 1 || kept[0].Name != "run_command" || len(dropped) != 0 {
		t.Fatalf("a bash call was not resolved onto run_command: kept=%v dropped=%v", kept, dropped)
	}
	if kept[0].Arguments != `{"command":"echo hi"}` {
		t.Errorf("the arguments were rewritten: %q", kept[0].Arguments)
	}
	if len(notes) != 1 {
		t.Fatalf("a repaired name produced %d notes, want one", len(notes))
	}
	note, ok := uimsg.Parse(notes[0])
	if !ok || note.Code != "chat.aliasedTool" {
		t.Fatalf("the note is %q, want the chat.aliasedTool code", notes[0])
	}
	if len(note.Args) != 2 || note.Args[0] != "bash" || note.Args[1] != "run_command" {
		t.Errorf("the note does not say what runs as what: %v", note.Args)
	}
}

// TestABorrowedNameCannotReachAWithheldTool is the limit of the table, pinned where it can
// actually fail: `known` is every tool the app can execute, which is wider than the request's
// own list, so an alias resolved against that alone would hand a turn a tool it was never
// offered — `fetch` reaching a `scrape_url` whose scraper is switched off, or `bash` reaching a
// `run_command` in a project with nobody to ask.
func TestABorrowedNameCannotReachAWithheldTool(t *testing.T) {
	known := []string{"search_web", "scrape_url", "run_command"}
	withheld := []string{"search_web"}

	kept, notes, dropped := resolveToolCalls([]llm.ToolCall{
		{ID: "call-1", Name: "fetch", Arguments: `{"url":"https://example.com"}`},
		{ID: "call-2", Name: "bash", Arguments: `{"command":"echo hi"}`},
	}, known, withheld, true)
	if len(kept) != 0 || len(dropped) != 2 {
		t.Fatalf("a withheld tool was reached: kept=%v dropped=%v", kept, dropped)
	}
	for _, note := range notes {
		if message, ok := uimsg.Parse(note); !ok || message.Code != "chat.droppedTool" {
			t.Errorf("a withheld name was reported as %q, want the dropped code", note)
		}
	}

	// The same two calls against a request that offers them: both resolve.
	kept, _, _ = resolveToolCalls([]llm.ToolCall{
		{ID: "call-1", Name: "fetch", Arguments: `{"url":"https://example.com"}`},
		{ID: "call-2", Name: "bash", Arguments: `{"command":"echo hi"}`},
	}, known, known, true)
	if len(kept) != 2 || kept[0].Name != "scrape_url" || kept[1].Name != "run_command" {
		t.Errorf("offered names did not resolve: %v", kept)
	}
}

// TestAMangledNameIsRepairedBeforeTheCallIsStored is the fix end to end on the backend
// that lets such a call through: the model asks for "scr scrape_url", the app runs
// `scrape_url`, the tool result arrives, and — the part that outlives the turn — the
// assistant row records the real name, not the invented one.
func TestAMangledNameIsRepairedBeforeTheCallIsStored(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Bericht</title></head><body><p>Der Bericht.</p></body></html>`)
	}))
	t.Cleanup(srv.Close)

	call := fmt.Sprintf("```tool_call\n{\"name\": \"scr scrape_url\", \"arguments\": {\"url\": %q}}\n```", srv.URL)
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: call}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	scraper := research.NewScraper(st, research.ScrapeOptions{
		AllowPrivateHosts: true, RespectRobots: false, Timeout: 5 * time.Second,
	}, nil)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, scraper),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies den Bericht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	msgs, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	var stored []llm.ToolCall
	for _, m := range msgs {
		if m.Role != "assistant" || m.ToolCallsJSON == "" {
			continue
		}
		if err := json.Unmarshal([]byte(m.ToolCallsJSON), &stored); err != nil {
			t.Fatalf("stored tool calls are not readable: %v", err)
		}
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d tool calls, want one — the turn's call", len(stored))
	}
	if stored[0].Name != "scrape_url" {
		t.Fatalf("stored name = %q, want the repaired %q: a chat cannot replay what it cannot resolve",
			stored[0].Name, "scrape_url")
	}

	// The page was read, so the repair executed the tool rather than only renaming it.
	second := provider.requestAt(1)
	carried := ""
	for _, m := range second.Messages {
		if m.Role == llm.RoleTool {
			carried = m.Content
		}
	}
	if !strings.Contains(carried, "Der Bericht.") {
		t.Errorf("the model never read the page: %q", truncateForTest(carried, 200))
	}
	// And the user was told, rather than handed a call that silently changed name.
	warned := false
	for _, ev := range log.snapshot() {
		if ev.name != "chat:warning" {
			continue
		}
		if message, ok := uimsg.Parse(fmt.Sprint(ev.payload["warning"])); ok && message.Code == "chat.renamedTool" {
			warned = true
		}
	}
	if !warned {
		t.Error("the repaired name was not reported")
	}
}

// TestABorrowedNameAndItsOwnFieldNamesStillWork drives the pair end to end, and the pair is
// the point: the *name* is resolved in this package and the *fields* in internal/tools, so a
// repair that did only the first would tell the user the call ran while the model was told it
// had named no path — the worst of both answers.
func TestABorrowedNameAndItsOwnFieldNamesStillWork(t *testing.T) {
	st := newTestStore(t)
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateChat(store.Chat{
		ID: "chat-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model", SystemPrompt: "Du bist ein Assistent.",
		Temperature: 0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: folder,
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools}},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: "read", Arguments: `{"file_path":"main.go"}`,
			}}},
			{{Type: llm.EventDelta, Text: "Gelesen."}, {Type: llm.EventDone}},
		},
	}
	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, (&eventLog{}).emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Lies main.go."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	carried := ""
	for _, m := range provider.requestAt(1).Messages {
		if m.Role == llm.RoleTool {
			carried = m.Content
		}
	}
	if !strings.Contains(carried, "package main") {
		t.Errorf("a borrowed name with its own field names did not reach the tool: %q",
			truncateForTest(carried, 200))
	}
}

// TestAnUnresolvableNativeNameIsNotStored is the other half for a native call: when no
// known tool fits, it is dropped instead of executed or recorded. A native name is the
// vendor's to validate, so what reaches the app is either a refusal (nothing stored) or
// a backend that tolerates anything — and there the app is the only check that exists.
// The assistant row is what every later turn replays, so a name nothing can answer must
// not enter it, and the transcript says what happened instead of ending on emptiness.
func TestAnUnresolvableNativeNameIsNotStored(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools}},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: "scr nonsense", Arguments: `{"query":"mietrecht"}`,
			}}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	log := &eventLog{}
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, log.emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Recherchiere Mietrecht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	msgs, err := st.ListMessages("chat-1")
	if err != nil {
		t.Fatal(err)
	}
	sawNote := false
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		if m.ToolCallsJSON != "" {
			t.Errorf("an unresolvable call was stored: %s", m.ToolCallsJSON)
		}
		if strings.Contains(m.Content, "verworfen") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Error("the transcript does not say that the call was dropped")
	}
	warned := false
	for _, ev := range log.snapshot() {
		if ev.name != "chat:warning" {
			continue
		}
		if message, ok := uimsg.Parse(fmt.Sprint(ev.payload["warning"])); ok && message.Code == "chat.droppedTool" {
			warned = true
		}
	}
	if !warned {
		t.Error("a dropped call was not reported")
	}
}

// TestAnUnresolvableTextCallReachesTheModelAsAnError pins the other side of the rule: a
// name written in prose by the model itself is answered, not dropped. No vendor sees it,
// the error names what was asked for, and the model gets the round it needs to correct
// itself — which is what a text-protocol fallback is for.
func TestAnUnresolvableTextCallReachesTheModelAsAnError(t *testing.T) {
	st := newTestStore(t)
	makeChat(t, st)

	call := "```tool_call\n{\"name\": \"scr nonsense\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	provider := &fakeProvider{
		model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 131072, State: llm.StateLoaded},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventDelta, Text: call}},
			{{Type: llm.EventDelta, Text: "Antwort"}, {Type: llm.EventDone}},
		},
	}

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	registry := llm.NewRegistry(nil)
	registry.Register(provider)
	engine := NewEngine(st, registry, tools.New(st, nil, nil),
		func() config.Settings { return settings }, (&eventLog{}).emit)

	if err := engine.Send(TurnRequest{ChatID: "chat-1", Text: "Recherchiere Mietrecht."}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return !engine.IsRunning("chat-1") }) {
		t.Fatal("the turn did not finish")
	}

	// The error is what the model reads on the next round, so it must be the tool's own
	// answer to the name rather than the app's silence. It travels as text rather than as
	// a tool turn (the call it answers names nothing the app knows), which is why the
	// assertion is on what the request carries, not on the role carrying it.
	// The sentence the model reads is the tool's detail, not the code the window
	// translates: a model gets no help from `i18n:{…}`.
	carried := ""
	for _, m := range provider.requestAt(1).Messages {
		if strings.Contains(m.Content, "no tool named") {
			carried = m.Content
		}
	}
	if !strings.Contains(carried, "scr nonsense") {
		t.Fatalf("the model was not told what it asked for: %q", truncateForTest(carried, 200))
	}
}

// storedToolTurn is a history as an earlier turn wrote it: a question, a native call,
// and the result that answers it.
func storedToolTurn(name, content string) []store.Message {
	calls, _ := json.Marshal([]llm.ToolCall{{
		ID: "call-1", Name: name, Arguments: `{"url":"https://example.com/a"}`,
	}})
	return []store.Message{
		{ID: "m1", ChatID: "chat-1", Seq: 1, Role: "user", Content: "Lies die Seite."},
		{ID: "m2", ChatID: "chat-1", Seq: 2, Role: "assistant", ToolCallsJSON: string(calls)},
		{ID: "m3", ChatID: "chat-1", Seq: 3, Role: "tool", Name: name,
			ToolCallID: "call-1", Content: content},
	}
}

// TestAStoredMangledCallIsRepairedInTheHistory covers chats written before the fix: a
// call that named "scr scrape_url" is sent as `scrape_url`, so the result keeps its
// matching tool turn instead of being demoted for a name nothing recognises.
//
// The result's own name travels with it. A backend that carries the tool name on the result
// (Ollama sends `tool_name`) would otherwise replay a call named `scrape_url` answered by a
// tool turn named `scr scrape_url` — a pair that disagrees about which tool ran.
func TestAStoredMangledCallIsRepairedInTheHistory(t *testing.T) {
	assembly := Assemble(AssemblyInput{
		Chat:       store.Chat{ID: "chat-1", SystemPrompt: "Du bist ein Assistent."},
		Messages:   storedToolTurn("scr scrape_url", "Der Seiteninhalt."),
		KnownTools: []string{"scrape_url"},
		Limit:      100000, LimitSource: "backend", Reserve: 1000,
	})

	var names []string
	result, resultName := "", ""
	for _, m := range assembly.Messages {
		for _, c := range m.ToolCalls {
			names = append(names, c.Name)
		}
		if m.Role == llm.RoleTool {
			result, resultName = m.Content, m.Name
		}
	}
	if len(names) != 1 || names[0] != "scrape_url" {
		t.Fatalf("history carried tool calls %v, want the repaired scrape_url", names)
	}
	if result != "Der Seiteninhalt." {
		t.Errorf("the tool result did not stay a tool turn: %q", result)
	}
	if resultName != "scrape_url" {
		t.Errorf("the tool result is named %q, want the repaired scrape_url", resultName)
	}
}

// TestAStoredBorrowedNameIsRepairedInTheHistory is the same replay for the vocabulary another
// convention taught the model: a call stored as `bash` runs as `run_command`, and its result
// says so too — the transcript of an older turn is the one place the two names can disagree.
func TestAStoredBorrowedNameIsRepairedInTheHistory(t *testing.T) {
	assembly := Assemble(AssemblyInput{
		Chat:       store.Chat{ID: "chat-1", SystemPrompt: "Du bist ein Assistent."},
		Messages:   storedToolTurn("bash", "hallo"),
		KnownTools: []string{"run_command"},
		Limit:      100000, LimitSource: "backend", Reserve: 1000,
	})

	var names []string
	resultName := ""
	for _, m := range assembly.Messages {
		for _, c := range m.ToolCalls {
			names = append(names, c.Name)
		}
		if m.Role == llm.RoleTool {
			resultName = m.Name
		}
	}
	if len(names) != 1 || names[0] != "run_command" {
		t.Fatalf("history carried tool calls %v, want the repaired run_command", names)
	}
	if resultName != "run_command" {
		t.Errorf("the tool result is named %q, want the repaired run_command", resultName)
	}
}

// TestAStoredUnknownCallIsDemotedToText is the history a laxer backend could have
// written: a name no tool answers. It travels as text, because a tool call the request
// cannot resolve is the shape a strict backend refuses — and the result follows it as a
// user turn, since a tool message without its call is just as invalid.
func TestAStoredUnknownCallIsDemotedToText(t *testing.T) {
	assembly := Assemble(AssemblyInput{
		Chat:       store.Chat{ID: "chat-1", SystemPrompt: "Du bist ein Assistent."},
		Messages:   storedToolTurn("scr nonsense", "Der Seiteninhalt."),
		KnownTools: []string{"scrape_url"},
		Limit:      100000, LimitSource: "backend", Reserve: 1000,
	})

	for _, m := range assembly.Messages[1:] {
		if len(m.ToolCalls) > 0 {
			t.Fatalf("an unresolvable call reached the request: %+v", m.ToolCalls)
		}
		if m.Role == llm.RoleTool {
			t.Fatalf("a tool turn survived without the call it answers: %q", truncateForTest(m.Content, 120))
		}
	}
	if !messagesContain(assembly.Messages, "Der Seiteninhalt.") {
		t.Error("the demoted result was lost instead of carried as text")
	}
}

// TestAHistoryWithoutAKnownToolListIsLeftAlone pins the default: a caller that does not
// hand Assemble the tool set — a preview, a test building an assembly by hand — is not
// rewritten, so a method that never learned about tools cannot mangle a history.
func TestAHistoryWithoutAKnownToolListIsLeftAlone(t *testing.T) {
	assembly := Assemble(AssemblyInput{
		Chat:     store.Chat{ID: "chat-1", SystemPrompt: "Du bist ein Assistent."},
		Messages: storedToolTurn("scr nonsense", "Der Seiteninhalt."),
		Limit:    100000, LimitSource: "backend", Reserve: 1000,
	})
	found := false
	for _, m := range assembly.Messages {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].Name == "scr nonsense" {
			found = true
		}
	}
	if !found {
		t.Error("a history was rewritten although the caller stated no tool set")
	}
}

func messagesContain(msgs []llm.Message, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}
