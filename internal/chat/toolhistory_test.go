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
	// The clock of the request rides on this message; the payload is what has to survive.
	if got := withoutTurnState(result); got != "Der Seiteninhalt." {
		t.Errorf("the tool result did not stay a tool turn: %q", got)
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

// readResult is one file-read result of a size worth ageing: a header line and enough body
// that the estimator charges about two thousand tokens for it.
func readResult(name string) string {
	return "[read_file " + name + "]\n" + strings.Repeat("x = 1\n", 1200)
}

// TestOldToolResultsAreAgedOutOfTheRequest is the cost property behind the ageing rule: a
// reply's tool results are re-sent on every later round, so only the newest ones stay verbatim
// and the rest become a notice the model can act on.
func TestOldToolResultsAreAgedOutOfTheRequest(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "arbeite das ab"}}
	current := []bool{true}
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("call-%d", i)
		name := fmt.Sprintf("datei%d.go", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: id, Name: tools.ToolReadFile,
				Arguments: fmt.Sprintf(`{"path":%q}`, name),
			}}},
			llm.Message{Role: llm.RoleTool, Name: tools.ToolReadFile, ToolCallID: id,
				Content: readResult(name)},
		)
		current = append(current, false, true)
	}

	aged, dropped, saved := ageToolResults(msgs, current, ranAll(msgs))

	if len(dropped) == 0 || saved <= 0 {
		t.Fatalf("nothing was aged out: %d dropped, %d tokens saved", len(dropped), saved)
	}
	// The newest result is what the round is working from, so it is never the one dropped.
	newest := msgs[len(msgs)-1].Content
	if aged[len(aged)-1].Content != newest {
		t.Error("the newest tool result was aged out; the round would start blind")
	}
	// Every dropped result is older than every kept one: an age rule that dropped a recent
	// result while keeping an older one would be incoherent. The stubs therefore come first.
	seenKept := false
	for i := range aged {
		if aged[i].Role != llm.RoleTool {
			continue
		}
		stubbed := strings.HasPrefix(aged[i].Content, "[Tool-Ergebnis von read_file:")
		if !stubbed {
			seenKept = true
			continue
		}
		if seenKept {
			t.Errorf("message %d was dropped although a newer result was kept", i)
		}
	}
	// The files named are the old ones, and the model is told how to get them back. The first
	// tool result is at index 2: a user turn, then the assistant call that answers it.
	for _, path := range dropped {
		if !strings.Contains(strings.Join(dropped, ","), path) {
			t.Errorf("dropped path %q is not reported", path)
		}
	}
	if !strings.Contains(aged[2].Content, "rufe das Tool bei Bedarf erneut auf") {
		t.Errorf("a dropped result does not say it can be fetched again:\n%s", aged[2].Content)
	}
	// Assistant turns are not touched: the calls stay, so the results still pair with them.
	if len(aged[1].ToolCalls) != 1 || aged[1].ToolCalls[0].ID != "call-0" {
		t.Error("a dropped result lost the tool call that answers it")
	}
}

// TestAToolResultFromAnEarlierTurnIsNotAged keeps the rule scoped to the reply: a previous
// answer's evidence is compaction's to summarise, and rewriting it would change what an
// earlier turn can still be shown to have been based on.
func TestAToolResultFromAnEarlierTurnIsNotAged(t *testing.T) {
	old := readResult("alt.go")
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "erste Frage"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "old-1", Name: tools.ToolReadFile,
			Arguments: `{"path":"alt.go"}`}}},
		{Role: llm.RoleTool, Name: tools.ToolReadFile, ToolCallID: "old-1", Content: old},
		{Role: llm.RoleUser, Content: "zweite Frage"},
	}
	// Only the last message belongs to the reply being answered.
	aged, dropped, saved := ageToolResults(msgs, []bool{false, false, false, true}, ranAll(msgs))

	if len(dropped) != 0 || saved != 0 {
		t.Errorf("an earlier turn was aged out: %v, %d tokens", dropped, saved)
	}
	if aged[2].Content != old {
		t.Error("the previous turn's tool result was rewritten")
	}
}

// TestARepeatedReadReplacesTheOneBeforeIt pins the re-read rule: the newest read of a window is
// the only copy of it in the request, even while the verbatim budget still has room for all of
// them. A reply that reads one file five times otherwise carries five windows, four of them the
// text the model has already replaced — the cost, and the wrong version standing beside the
// right one.
func TestARepeatedReadReplacesTheOneBeforeIt(t *testing.T) {
	const path = "internal/chat/engine.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "ändere das"}}
	current := []bool{true}
	for i := 0; i < 5; i++ {
		msgs, current = appendRead(msgs, current, fmt.Sprintf("call-%d", i), path,
			readArgs(path, 0, 0), readResult(path)+fmt.Sprintf("// runde %d\n", i))
	}

	aged, dropped, saved := ageToolResults(msgs, current, ranAll(msgs))

	superseded := 0
	for _, m := range aged {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, "ältere Stand weggelassen") {
			superseded++
		}
	}
	if superseded != 4 {
		t.Errorf("%d of the five reads were replaced, want four", superseded)
	}
	if got := llm.EstimateTokens(readResult(path)); saved < 3*got {
		t.Errorf("saved %d tokens of about %d in four windows", saved, 4*got)
	}
	// The newest read is the file as it is, and it is what the round works from.
	if !strings.Contains(aged[len(aged)-1].Content, "x = 1") ||
		!strings.Contains(aged[len(aged)-1].Content, "// runde 4") {
		t.Error("the newest read was replaced instead of the older ones")
	}
	// Nothing is forgotten while a copy of the file is still sent: the tool's "already read in
	// this turn" answer is true about this request, and a re-read would cost a round to fetch
	// text the model has.
	if len(dropped) != 0 {
		t.Errorf("a file whose text is still in the request was reported as dropped: %v", dropped)
	}
}

// TestOnlyACoveringReadReplacesAnEarlierOne keeps the rule about coverage rather than paths: a
// 2 000-line file is read in windows, and the window at line 1 001 is not a replacement for the
// window at line 1. An outline is not a window at all — it is the way back to the text.
func TestOnlyACoveringReadReplacesAnEarlierOne(t *testing.T) {
	const path = "internal/chat/generate.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "ändere das"}}
	current := []bool{true}
	msgs, current = appendRead(msgs, current, "call-1", path, readArgs(path, 1, 1000), readResult(path))
	msgs, current = appendRead(msgs, current, "call-2", path, readArgs(path, 1001, 1000), readResult(path))
	// An outline of the same file: a declaration list with line numbers, not a window.
	msgs, current = appendRead(msgs, current, "call-3", path,
		fmt.Sprintf(`{"path":%q,"outline":true}`, path), "1: func main()\n40: func run()\n")

	aged, dropped, saved := ageToolResults(msgs, current, ranAll(msgs))
	if saved != 0 || len(dropped) != 0 {
		t.Fatalf("two windows and an outline were not left alone: saved %d, dropped %v", saved, dropped)
	}
	for _, i := range []int{2, 4} {
		if !strings.Contains(aged[i].Content, "x = 1") {
			t.Errorf("message %d lost its window although nothing covered it", i)
		}
	}

	// A read wide enough to cover the first window — but not the second — replaces only that
	// one, and names the lines it stands in for.
	msgs, current = appendRead(msgs, current, "call-4", path, readArgs(path, 1, 1500), readResult(path))
	aged, dropped, saved = ageToolResults(msgs, current, ranAll(msgs))
	if saved <= 0 || len(dropped) != 0 {
		t.Fatalf("a covering read replaced nothing: saved %d, dropped %v", saved, dropped)
	}
	if !strings.Contains(aged[2].Content, "Zeilen 1–1000") {
		t.Errorf("the first window was not the one replaced:\n%s", aged[2].Content)
	}
	if !strings.Contains(aged[4].Content, "x = 1") {
		t.Error("the second window was replaced by a read that does not cover it")
	}
	if aged[6].Content != "1: func main()\n40: func run()\n" {
		t.Error("an outline was replaced by a window that happens to cover its lines")
	}
	if !strings.Contains(aged[8].Content, "x = 1") {
		t.Error("the newest window was replaced")
	}
}

// TestAFileIsForgottenOnlyWhenNoCopyOfItIsSent is the other half of the rule: the tool's memory
// of what the turn has read has to follow the request. A file whose every copy is gone has to be
// readable again, and one whose copy is still there must not be — a "read it again" there would
// spend a round to fetch text the model was just sent.
func TestAFileIsForgottenOnlyWhenNoCopyOfItIsSent(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "arbeite das ab"}}
	current := []bool{true}
	// Two reads of one file, both long enough ago to be outside the newest windows once the rest
	// of the reply has been read.
	msgs, current = appendRead(msgs, current, "call-1", "alt.go", `{"path":"alt.go"}`, readResult("alt.go"))
	msgs, current = appendRead(msgs, current, "call-2", "alt.go", `{"path":"alt.go"}`, readResult("alt.go"))
	// Enough small results after them to spend the whole verbatim budget. Each one is under the
	// floor, so the budget pass keeps every filler verbatim and the cut lands on the two copies
	// of alt.go — one superseded, one aged, and no copy of the file left in the request.
	small := strings.Repeat("x = 1\n", 200)
	for i := 0; i < 66; i++ {
		name := fmt.Sprintf("datei%d.go", i)
		msgs, current = appendRead(msgs, current, fmt.Sprintf("call-%d", i+3), name,
			readArgs(name, 0, 0), small)
	}

	_, dropped, saved := ageToolResults(msgs, current, ranAll(msgs))
	if saved <= 0 {
		t.Fatal("nothing was left out, so the forgetting rule was not tested")
	}
	// Once, although both of its results were stubbed for different reasons: the caller turns
	// this into one ForgetReads per path, and a path is a path.
	if len(dropped) != 1 || dropped[0] != "alt.go" {
		t.Errorf("dropped = %v, want just alt.go", dropped)
	}
}

// appendRead adds one read_file call and the result that answers it, with the reply flags that
// say both belong to the reply being assembled.
func appendRead(msgs []llm.Message, current []bool, id, path, args, content string) ([]llm.Message, []bool) {
	return appendTool(msgs, current, id, tools.ToolReadFile, args, content)
}

// appendTool is the same for any tool: the call the model made, then the result that answers it.
func appendTool(msgs []llm.Message, current []bool, id, name, args, content string) ([]llm.Message, []bool) {
	msgs = append(msgs,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id,
			Name: name, Arguments: args}}},
		llm.Message{Role: llm.RoleTool, Name: name, ToolCallID: id, Content: content},
	)
	return msgs, append(current, false, true)
}

// TestAnEditMarksTheReadItMovedPast pins the other half of the staleness rule: a model that edits
// a window and never re-reads holds the file as it was for the rest of the reply, and the next
// thing it quotes from there is text it has already replaced. The window stays — the lines around
// the edit are still exactly right and are what the next edit is copied from — with one sentence
// in front of it saying the file has moved since.
func TestAnEditMarksTheReadItMovedPast(t *testing.T) {
	const path = "internal/chat/engine.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "ändere das"}}
	current := []bool{true}
	msgs, current = appendRead(msgs, current, "call-1", path, readArgs(path, 0, 0), readResult(path))
	msgs, current = appendTool(msgs, current, "call-2", tools.ToolEditFile,
		fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path), "1 Änderung.")
	msgs, current = appendRead(msgs, current, "call-3", "ander.go", readArgs("ander.go", 0, 0),
		readResult("ander.go"))

	aged, _, saved := ageToolResults(msgs, current, ranAll(msgs))

	if !strings.HasPrefix(aged[2].Content, "[Hinweis:") {
		t.Errorf("the read was not marked as pre-edit:\n%.120s", aged[2].Content)
	}
	// Marked, not evicted: the lines around the change are still there to copy the next edit from.
	if !strings.Contains(aged[2].Content, "x = 1") {
		t.Error("the window was replaced instead of marked; the next edit has nothing to copy from")
	}
	if !strings.Contains(aged[2].Content, path) {
		t.Error("the note does not name the file that changed")
	}
	// A read of another file, and one taken after the edit, are current.
	if aged[6].Content != readResult("ander.go") {
		t.Error("a read of an untouched file was marked")
	}
	// The note costs tokens, and the meter says so rather than reporting a saving.
	if saved >= 0 {
		t.Errorf("saved = %d, want the note charged", saved)
	}

	// A second edit of the same file does not stack a second note on the same window.
	msgs, current = appendTool(msgs, current, "call-4", tools.ToolEditFile,
		fmt.Sprintf(`{"path":%q,"old_string":"b","new_string":"c"}`, path), "1 Änderung.")
	aged, _, _ = ageToolResults(msgs, current, ranAll(msgs))
	notes := 0
	for _, m := range aged {
		notes += strings.Count(m.Content, "[Hinweis:")
	}
	if notes != 1 {
		t.Errorf("%d notes on one window, want one", notes)
	}
}

// TestAssembleMarksAReadAnEditMovedPast is the same rule through the real entry point. The
// applied flag is a third array index-aligned with the messages, and one that drifted by a
// position would switch the rule off without failing anything else — the same reason the ageing
// boundary has its own assembly test.
func TestAssembleMarksAReadAnEditMovedPast(t *testing.T) {
	const path = "internal/chat/engine.go"
	assemble := func(editStatus string) Assembly {
		messages := []store.Message{
			{ID: "m1", Seq: 1, Role: "user", Content: "ändere das", Status: store.StatusComplete},
			{ID: "m2", Seq: 2, Role: "assistant", Status: store.StatusComplete,
				ToolCallsJSON: fmt.Sprintf(`[{"id":"call-1","name":"read_file","arguments":%q}]`,
					readArgs(path, 0, 0))},
			{ID: "m3", Seq: 3, Role: "tool", Name: tools.ToolReadFile, ToolCallID: "call-1",
				Content: readResult(path), Status: store.ToolOK},
			{ID: "m4", Seq: 4, Role: "assistant", Status: store.StatusComplete,
				ToolCallsJSON: fmt.Sprintf(`[{"id":"call-2","name":"edit_file","arguments":%q}]`,
					fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path))},
			{ID: "m5", Seq: 5, Role: "tool", Name: tools.ToolEditFile, ToolCallID: "call-2",
				Content: "1 Änderung.", Status: editStatus},
		}
		return Assemble(AssemblyInput{
			Chat:       asmChat("Du bist ein Assistent."),
			Messages:   messages,
			KnownTools: []string{tools.ToolReadFile, tools.ToolEditFile},
			Limit:      200_000, Reserve: 1024,
		})
	}

	marked := assemble(store.ToolOK)
	found := false
	for _, m := range marked.Messages {
		if strings.Contains(m.Content, "[Hinweis:") {
			found = true
		}
	}
	if !found {
		t.Error("an edit that ran left the read it moved past unmarked")
	}
	for _, m := range assemble(store.ToolDenied).Messages {
		if strings.Contains(m.Content, "[Hinweis:") {
			t.Error("a denied edit marked a read as stale")
		}
	}
}

// TestAWholeFileRewriteReplacesTheRead is the case where nothing is lost by evicting: write_file
// replaces every line, and the new text is already in the request as the call's own arguments —
// which is how the model sees its own write. So the old read goes, like a superseded one.
func TestAWholeFileRewriteReplacesTheRead(t *testing.T) {
	const path = "internal/chat/engine.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "schreib das neu"}}
	current := []bool{true}
	msgs, current = appendRead(msgs, current, "call-1", path, readArgs(path, 0, 0), readResult(path))
	msgs, current = appendTool(msgs, current, "call-2", tools.ToolWriteFile,
		fmt.Sprintf(`{"path":%q,"content":"package chat\n"}`, path), "geschrieben")

	aged, dropped, saved := ageToolResults(msgs, current, ranAll(msgs))

	if strings.Contains(aged[2].Content, "x = 1") {
		t.Error("a read from before a whole-file rewrite was still sent in full")
	}
	if !strings.Contains(aged[2].Content, tools.ToolWriteFile) ||
		!strings.Contains(aged[2].Content, "vollständig neu geschrieben") {
		t.Errorf("the stub does not say where the new text is:\n%s", aged[2].Content)
	}
	if saved <= 0 {
		t.Errorf("saved = %d, want the window's tokens back", saved)
	}
	// No copy of the file is sent, so the tool must let the model read it again.
	if len(dropped) != 1 || dropped[0] != path {
		t.Errorf("dropped = %v, want the rewritten file", dropped)
	}
}

// TestADeniedEditMarksNothing guards the status half of the rule. A card the user refused, or an
// edit whose passage was not found, changed no file: the read in front of it is still the current
// text, and marking it would tell the model that a change landed which never did.
func TestADeniedEditMarksNothing(t *testing.T) {
	const path = "internal/chat/engine.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "ändere das"}}
	current := []bool{true}
	msgs, current = appendRead(msgs, current, "call-1", path, readArgs(path, 0, 0), readResult(path))
	msgs, current = appendTool(msgs, current, "call-2", tools.ToolEditFile,
		fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path), "abgelehnt")

	applied := ranAll(msgs)
	applied[4] = false // the edit result: the card was denied, nothing was written
	aged, _, saved := ageToolResults(msgs, current, applied)

	if aged[2].Content != readResult(path) {
		t.Error("a read was marked for an edit that never ran")
	}
	if saved != 0 {
		t.Errorf("saved = %d, want nothing changed for a denied edit", saved)
	}
}

// ranAll is the applied flag for a history whose tool results all ran, which is what every test
// but the denied-edit one wants.
func ranAll(msgs []llm.Message) []bool {
	ran := make([]bool, len(msgs))
	for i := range ran {
		ran[i] = true
	}
	return ran
}

// TestAReadAfterTheEditIsCurrent keeps the direction of the rule: only a read the edit has moved
// past is marked, and when the model does re-read, the older copy is the one that goes.
func TestAReadAfterTheEditIsCurrent(t *testing.T) {
	const path = "internal/chat/engine.go"
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "ändere und prüfe"}}
	current := []bool{true}
	msgs, current = appendRead(msgs, current, "call-1", path, readArgs(path, 0, 0), readResult(path))
	msgs, current = appendTool(msgs, current, "call-2", tools.ToolEditFile,
		fmt.Sprintf(`{"path":%q,"old_string":"a","new_string":"b"}`, path), "1 Änderung.")
	msgs, current = appendRead(msgs, current, "call-3", path, readArgs(path, 0, 0),
		readResult(path)+"// nachher\n")

	aged, _, _ := ageToolResults(msgs, current, ranAll(msgs))

	// The re-read covers the first window, so it is gone: no note on text that is not sent, and
	// no duplicate of the file either.
	if strings.Contains(aged[2].Content, "x = 1") {
		t.Error("the pre-edit window was still sent, beside the copy taken after the edit")
	}
	if strings.Contains(aged[2].Content, "[Hinweis:") {
		t.Error("a stub was annotated for an edit it no longer stands in front of")
	}
	// The copy taken after the edit is current, so it is neither noted nor dropped.
	if !strings.Contains(aged[6].Content, "// nachher") || strings.Contains(aged[6].Content, "[Hinweis:") {
		t.Errorf("the re-read was not left alone:\n%.120s", aged[6].Content)
	}
}

// readArgs is the argument JSON of one read, with an absent window left absent the way a model
// writes it.
func readArgs(path string, from, limit int) string {
	args := fmt.Sprintf(`{"path":%q`, path)
	if from > 0 {
		args += fmt.Sprintf(`,"offset":%d`, from)
	}
	if limit > 0 {
		args += fmt.Sprintf(`,"limit":%d`, limit)
	}
	return args + "}"
}

// TestAShortToolResultIsNeverAged is the floor: an error line or a test summary is cheap to
// keep and is often the thing that matters, and a notice in its place would cost more
// explanation than it saved.
func TestAShortToolResultIsNeverAged(t *testing.T) {
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "arbeite das ab"}}
	current := []bool{true}
	// One short result first, then enough large ones to spend the whole budget.
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("call-%d", i)
		content := readResult(fmt.Sprintf("datei%d.go", i))
		if i == 0 {
			content = "exit status 1: no such file"
		}
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id,
				Name: tools.ToolReadFile, Arguments: fmt.Sprintf(`{"path":"datei%d.go"}`, i)}}},
			llm.Message{Role: llm.RoleTool, Name: tools.ToolReadFile, ToolCallID: id,
				Content: content},
		)
		current = append(current, false, true)
	}

	aged, _, saved := ageToolResults(msgs, current, ranAll(msgs))
	if saved <= 0 {
		t.Fatal("nothing was aged out, so the floor was not tested")
	}
	if aged[2].Content != "exit status 1: no such file" {
		t.Errorf("a short result was aged out:\n%s", aged[2].Content)
	}
}

// TestAssembleAgesTheReplyItIsAssembling is the same rule through the real entry point: the
// boundary between "this reply" and "an earlier turn" has to survive the assembly loop, which
// skips messages (an empty assistant turn is never sent) and inserts the system message at
// position zero. A flag array that drifted by one would age the wrong half of the conversation.
func TestAssembleAgesTheReplyItIsAssembling(t *testing.T) {
	var messages []store.Message
	seq := 1
	add := func(role, content string) {
		messages = append(messages, store.Message{ID: fmt.Sprintf("m%d", seq), Seq: seq,
			Role: role, Content: content})
		seq++
	}
	read := func(name string) {
		id := fmt.Sprintf("call-%d", seq)
		messages = append(messages, store.Message{
			ID: fmt.Sprintf("m%d", seq), Seq: seq, Role: "assistant",
			ToolCallsJSON: fmt.Sprintf(`[{"id":%q,"name":"read_file","arguments":"{\"path\":\"%s\"}"}]`,
				id, name),
		})
		seq++
		add("tool", readResult(name))
		messages[len(messages)-1].Name = tools.ToolReadFile
		messages[len(messages)-1].ToolCallID = id
	}

	// An earlier turn's read, which is not this reply's to rewrite.
	add("user", "erste Frage")
	read("alt.go")
	add("assistant", "erledigt")

	// The reply being assembled, long enough to outgrow the verbatim budget.
	add("user", "arbeite das ab")
	for i := 0; i < 16; i++ {
		read(fmt.Sprintf("datei%d.go", i))
	}

	assembly := Assemble(AssemblyInput{
		Chat:       asmChat("Du bist ein Assistent."),
		Messages:   messages,
		KnownTools: []string{tools.ToolReadFile},
		Limit:      200_000, Reserve: 1024,
	})

	if len(assembly.AgedReads) == 0 {
		t.Fatal("nothing was aged out of a reply that is far over the budget")
	}
	for _, path := range assembly.AgedReads {
		if path == "alt.go" {
			t.Error("the previous turn's read was aged out")
		}
	}
	// The current reply's oldest read is a notice, its newest is verbatim.
	var sawStub, sawNewest bool
	for _, m := range assembly.Messages {
		if strings.Contains(m.Content, "rufe das Tool bei Bedarf erneut auf") {
			sawStub = true
		}
		if strings.Contains(m.Content, "datei15.go") && strings.Contains(m.Content, "x = 1") {
			sawNewest = true
		}
		if strings.Contains(m.Content, "alt.go") && !strings.Contains(m.Content, "x = 1") {
			t.Error("the earlier turn's read content was rewritten")
		}
	}
	if !sawStub {
		t.Error("no result was replaced by a notice")
	}
	if !sawNewest {
		t.Error("the newest read was dropped as well")
	}
	// The meter describes what is sent: the dropped text is gone from the history segment.
	if assembly.Budget.History <= 0 {
		t.Errorf("history segment = %d tokens", assembly.Budget.History)
	}
}
