package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/workspace"
)

// projectEngine builds a store, an engine with a tool service that can reach a folder,
// and the two chats the gate is about: a project whose folder holds an AGENTS.md, and a
// research chat with no folder at all.
func projectEngine(t *testing.T) (*Engine, store.Chat, store.Chat) {
	t.Helper()
	folder := t.TempDir()
	guidance := "Run `make check` before claiming a change works.\n"
	if err := os.WriteFile(filepath.Join(folder, workspace.AgentsFileName), []byte(guidance), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The package caches a guidance file by path, size and modification time, and this
	// test's folder is new each run; clearing it keeps a previous test's entry from
	// answering for this one.
	workspace.ForgetAgentDocs()

	st := newTestStore(t)
	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: folder,
	}
	plain := store.Chat{
		ID: "chat-1", Title: "Chat", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeChat,
	}
	// The research chat is given the project's folder anyway, which is the case a
	// gate that trusted the folder rather than the mode would let through.
	plain.Workspace = folder
	for _, c := range []store.Chat{project, plain} {
		if err := st.CreateChat(c); err != nil {
			t.Fatalf("create chat %s: %v", c.ID, err)
		}
	}

	settings := config.Default()
	settings.AutoTitle = false
	// The confirmer is what the app installs (App.confirmChange); here it stands for a
	// person who says yes, because this engine is built to answer questions about the
	// mode gate, not about approvals. Without one the write tools are withheld by design
	// and the gate below would look narrower than it is.
	service := &tools.Service{Store: st,
		Confirm: func(context.Context, *tools.ApprovalRequest) error { return nil }}
	return NewEngine(st, llm.NewRegistry(nil), service,
		func() config.Settings { return settings }, (&eventLog{}).emit), project, plain
}

// systemMessage is the first message of an assembly, which is where the app puts
// everything the prompt carries beyond the conversation.
func systemMessage(t *testing.T, e *Engine, c store.Chat) string {
	t.Helper()
	settings := config.Default()
	assembly, _, err := e.buildAssembly(context.Background(), &c, c.Backend, c.Model,
		settings, llm.ModelInfo{ID: c.Model, Backend: c.Backend, ContextLimit: 8192}, "")
	if err != nil {
		t.Fatalf("buildAssembly: %v", err)
	}
	if len(assembly.Messages) == 0 || assembly.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("the assembly does not open with a system message: %+v", assembly.Messages)
	}
	return assembly.Messages[0].Content
}

// TestAProjectsGuidanceReachesTheSystemPrompt is the whole point of reading AGENTS.md
// rather than asking the user to paste it into a setting: the file the repository already
// carries is the one the model is given.
func TestAProjectsGuidanceReachesTheSystemPrompt(t *testing.T) {
	e, project, plain := projectEngine(t)

	system := systemMessage(t, e, project)
	if !strings.Contains(system, "<project_agents_md") {
		t.Fatalf("a project's system prompt carries no guidance block:\n%s", system)
	}
	if !strings.Contains(system, "make check") {
		t.Errorf("the guidance block is there but the file's text is not:\n%s", system)
	}
	if !strings.Contains(system, `path="AGENTS.md"`) {
		t.Errorf("the block does not name the file it came from:\n%s", system)
	}
	// The projects tools are advertised in the same prompt, which is what a model needs
	// before it can act on the guidance.
	if !strings.Contains(system, tools.ToolReadFile) {
		t.Errorf("a project's prompt does not offer the file tools:\n%s", system)
	}

	// And a chat that is not a project never reads the folder, even one pointing at it:
	// the mode is what decides, so a research conversation that happens to carry a path
	// cannot pull a repository's instructions into its prompt.
	plainSystem := systemMessage(t, e, plain)
	if strings.Contains(plainSystem, "<project_agents_md") {
		t.Errorf("a chat with no coding mode got the project's guidance:\n%s", plainSystem)
	}

	// The `{{tool_list}}` line is where the model learns a tool exists, so it is the place
	// the extra tools would be visible outside a project even though the request's `tools`
	// array withheld them. It is rendered from the same filtered slice as the definitions,
	// and this is what holds the two together: a filter applied to one and not the other
	// would leave the model reading a name it can never call.
	for _, name := range tools.ProjectToolNames() {
		if !strings.Contains(system, name) {
			t.Errorf("the project's tool list does not name %s:\n%s", name, system)
		}
		if strings.Contains(plainSystem, name) {
			t.Errorf("a chat with no coding mode is told about %s:\n%s", name, plainSystem)
		}
	}
	// And the chat's line is not simply empty, which is what would make the loop above
	// pass without proving anything. The document tools are used rather than `search_web`
	// because this engine has no search chain, so the web tool is withheld from both chats
	// and a check on it would fail for a reason that has nothing to do with the mode.
	if !strings.Contains(plainSystem, tools.ToolSearchDocs) {
		t.Errorf("the chat's prompt carries no tool list at all:\n%s", plainSystem)
	}
}

// TestGuidanceCostIsOnTheMeter keeps the injected file visible: a prompt nobody can
// account for is the kind of context that grows without anyone deciding it should.
func TestGuidanceCostIsOnTheMeter(t *testing.T) {
	e, project, _ := projectEngine(t)
	settings := config.Default()
	assembly, _, err := e.buildAssembly(context.Background(), &project, project.Backend, project.Model,
		settings, llm.ModelInfo{ID: project.Model, Backend: project.Backend, ContextLimit: 8192}, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, seg := range assembly.Budget.Segments {
		if seg.Kind == SegmentProject {
			found = true
			if seg.Tokens <= 0 {
				t.Errorf("the guidance segment reports %d tokens", seg.Tokens)
			}
			if seg.Label != workspace.AgentsFileName {
				t.Errorf("the guidance segment is labelled %q, want %q", seg.Label, workspace.AgentsFileName)
			}
		}
	}
	if !found {
		t.Errorf("the injected guidance is missing from the context budget: %+v", assembly.Budget.Segments)
	}
}

// gitFolder makes a repository with one commit and one uncommitted edit, and returns its path.
//
// The host's git configuration is cleared so a `commit.gpgsign`, an `init.defaultBranch` or an
// alias cannot change what the test reads — the same isolation the workspace's own git tests
// use. A machine without git skips: the block is an addition to the prompt, not a prerequisite.
func gitFolder(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "first")
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAProjectsPromptCarriesItsGitState is the block's whole purpose: the model is told which
// branch it is on and what has changed since the last commit, on the turn it starts, without
// spending a tool round and a confirmation card on `git status`. A chat that is not a project
// gets nothing, even when it is pointed at the same repository, and a project folder that is not
// a repository is silent rather than reported as a failure.
func TestAProjectsPromptCarriesItsGitState(t *testing.T) {
	folder := gitFolder(t)

	st := newTestStore(t)
	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: folder,
	}
	plain := store.Chat{
		ID: "chat-1", Title: "Chat", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		// The same repository, deliberately: the mode is what decides.
		Mode: store.ModeChat, Workspace: folder,
	}
	for _, c := range []store.Chat{project, plain} {
		if err := st.CreateChat(c); err != nil {
			t.Fatalf("create chat %s: %v", c.ID, err)
		}
	}

	settings := config.Default()
	settings.AutoTitle = false
	engine := NewEngine(st, llm.NewRegistry(nil), nil,
		func() config.Settings { return settings }, (&eventLog{}).emit)

	system := systemMessage(t, engine, project)
	if !strings.Contains(system, `<project_git branch="main"`) {
		t.Fatalf("a project's system prompt carries no git state:\n%s", system)
	}
	if !strings.Contains(system, "Änderungen seit dem letzten Commit (1)") {
		t.Errorf("the block does not say how much changed:\n%s", system)
	}
	// The change itself, with the half of it that is staged and the size of the diff: that is what
	// makes the block useful rather than a branch name.
	if !strings.Contains(system, "main.go: nicht gestaged (geändert) +2 −0") {
		t.Errorf("the block does not describe the modified file:\n%s", system)
	}

	if plainSystem := systemMessage(t, engine, plain); strings.Contains(plainSystem, "<project_git") {
		t.Errorf("a chat with no coding mode got the project's git state:\n%s", plainSystem)
	}

	// A project folder that is nobody's repository: no block, no error, no warning.
	plainFolder := store.Chat{
		ID: "project-2", Title: "Ordner", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: t.TempDir(),
	}
	if err := st.CreateChat(plainFolder); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if scratch := systemMessage(t, engine, plainFolder); strings.Contains(scratch, "<project_git") {
		t.Errorf("a folder that is not a repository got a git block:\n%s", scratch)
	}

	// And it is on the meter: a block nobody can account for is context that grows without
	// anyone deciding it should.
	assembly, _, err := engine.buildAssembly(context.Background(), &project, project.Backend, project.Model,
		settings, llm.ModelInfo{ID: project.Model, Backend: project.Backend, ContextLimit: 8192}, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, seg := range assembly.Budget.Segments {
		if seg.Kind != SegmentGit {
			continue
		}
		found = true
		if seg.Tokens <= 0 {
			t.Errorf("the git segment reports %d tokens", seg.Tokens)
		}
		if !strings.Contains(seg.Detail, "main") {
			t.Errorf("the git segment is described as %q, want it to name the branch", seg.Detail)
		}
	}
	if !found {
		t.Errorf("the git state is missing from the context budget: %+v", assembly.Budget.Segments)
	}
}

// TestProjectToolsAreOfferedOnlyToAProject is the gate itself, asked of both halves:
// what the request declares, and what the app would *execute* if the model named it.
//
// The second half is the one that matters for safety. `KnownTools` decides whether a
// remembered or hallucinated call is replayed rather than dropped, so a research chat
// whose history contains `read_file` must not have it executed against a folder.
func TestProjectToolsAreOfferedOnlyToAProject(t *testing.T) {
	e, project, plain := projectEngine(t)
	settings := config.Default()
	model := llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192}

	offered := func(c store.Chat) map[string]bool {
		names := map[string]bool{}
		for _, def := range e.toolDefinitions(context.Background(), &c, model, settings) {
			names[def.Name] = true
		}
		// The tool list is rendered into the prompt, so a name that is absent from the
		// definitions but present in {{tool_list}} would be the same bug one step later.
		return names
	}

	projectTools := offered(project)
	for _, name := range tools.ProjectToolNames() {
		if !projectTools[name] {
			t.Errorf("a project is not offered %s", name)
		}
	}

	chatTools := offered(plain)
	for _, name := range tools.ProjectToolNames() {
		if chatTools[name] {
			t.Errorf("a chat with no coding mode is offered %s", name)
		}
	}

	// The known-name set: what the app can answer a call with.
	for _, name := range tools.ProjectToolNames() {
		if !containsName(engineKnownTools(e, &project), name) {
			t.Errorf("a project cannot answer %s, so a stored call to it would be demoted", name)
		}
		if containsName(engineKnownTools(e, &plain), name) {
			t.Errorf("a chat with no coding mode would execute %s", name)
		}
	}
	// The research tools stay on both sides: this gate narrows one group, it does not
	// replace the tool set with a different one.
	if !containsName(engineKnownTools(e, &plain), tools.ToolSearchDocs) {
		t.Error("the mode gate removed the document tools from a research chat")
	}
}

// projectTurnEngine builds a project chat with a scripted provider, so a test can read the
// request the model was actually handed — which is where the note about a revert has to be.
func projectTurnEngine(t *testing.T, replies ...[]llm.StreamEvent) (*Engine, *fakeProvider, store.Chat) {
	t.Helper()
	folder := t.TempDir()
	workspace.ForgetAgentDocs()

	st := newTestStore(t)
	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
		Mode: store.ModeCoding, Workspace: folder,
	}
	if err := st.CreateChat(project); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools},
		},
		replies: replies,
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	engine := NewEngine(st, registry, &tools.Service{Store: st},
		func() config.Settings { return settings }, (&eventLog{}).emit)
	return engine, provider, project
}

// answer is a reply with no tool call in it: the shortest turn there is.
func answer(text string) []llm.StreamEvent {
	return []llm.StreamEvent{{Type: llm.EventDelta, Text: text}, {Type: llm.EventDone}}
}

// systemOf is the request's first message, which is where everything the prompt carries
// beyond the conversation arrives.
func systemOf(t *testing.T, req llm.ChatRequest) string {
	t.Helper()
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("the request does not open with a system message: %+v", req.Messages)
	}
	return req.Messages[0].Content
}

// turn runs one turn and waits for it, so the next read of the provider is the request that
// turn sent rather than a race.
func turn(t *testing.T, e *Engine, chatID, text string) {
	t.Helper()
	if err := e.Send(TurnRequest{ChatID: chatID, Text: text}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !e.IsRunning(chatID) }) {
		t.Fatal("the turn did not finish")
	}
}

// TestATurnAfterARevertIsToldTheFileChanged is the other half of the undo. A tool result in the
// transcript says what the model wrote into a file, and it is replayed on every later turn; a
// revert makes that result false without touching it. The folder cannot correct the history, so
// the turn that follows has to say what changed — and say it once, because a note repeated on
// every request would be a permanent part of the prompt for a fact that stops being news.
func TestATurnAfterARevertIsToldTheFileChanged(t *testing.T) {
	e, provider, project := projectTurnEngine(t, answer("Gut."), answer("Weiter."))
	now := time.Now()
	if err := e.Store.PutFileSnapshot(store.FileSnapshot{
		ChatID: project.ID, Path: "internal/store/queries.go", BeforeText: "package store\n",
		Existed: true, AfterSHA: workspace.Digest("package store\n// rewritten\n"),
		Revisions: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("record the change: %v", err)
	}
	if err := e.Store.MarkFileSnapshotReverted(project.ID, "internal/store/queries.go"); err != nil {
		t.Fatalf("mark the revert: %v", err)
	}

	turn(t, e, project.ID, "Mach weiter.")
	system := systemOf(t, provider.lastRequest())
	if !strings.Contains(system, revertedOpen) {
		t.Fatalf("the turn after a revert carries no note:\n%s", system)
	}
	if !strings.Contains(system, "internal/store/queries.go") {
		t.Errorf("the note does not name the file that was put back:\n%s", system)
	}
	// The note has to say what to do, not just what happened: a model that reads "this was
	// reverted" and nothing else still has a false tool result in front of it.
	if !strings.Contains(system, "Lies") || !strings.Contains(system, "erneut") {
		t.Errorf("the note does not tell the model to read the file again:\n%s", system)
	}

	// Delivered once: the marker is gone, and the next turn's prompt is the ordinary one.
	if pending, err := e.Store.PendingReverts(project.ID); err != nil {
		t.Fatalf("PendingReverts: %v", err)
	} else if len(pending) != 0 {
		t.Errorf("%d notes survived the turn that carried them", len(pending))
	}
	turn(t, e, project.ID, "Und jetzt?")
	if next := systemOf(t, provider.lastRequest()); strings.Contains(next, revertedOpen) {
		t.Errorf("the note is repeated on a later turn:\n%s", next)
	}
}

// TestARevertDoesNotReachAChatThatIsNotAProject keeps the note inside the mode gate the rest of
// the coding layer is behind: a research conversation cannot have changed files, and a marker
// that somehow pointed at one still must not put a folder's paths into its prompt.
func TestARevertDoesNotReachAChatThatIsNotAProject(t *testing.T) {
	e, project, plain := projectEngine(t)
	now := time.Now()
	for _, chat := range []store.Chat{project, plain} {
		if err := e.Store.PutFileSnapshot(store.FileSnapshot{
			ChatID: chat.ID, Path: "main.go", BeforeText: "package main\n",
			Existed: true, AfterSHA: workspace.Digest("package main\n// x\n"),
			Revisions: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("record the change: %v", err)
		}
		if err := e.Store.MarkFileSnapshotReverted(chat.ID, "main.go"); err != nil {
			t.Fatalf("mark the revert: %v", err)
		}
	}
	if note := e.revertNote(plain); note != "" {
		t.Errorf("a chat with no coding mode was told about a reverted file:\n%s", note)
	}
	// The project's note, in contrast, is exactly what the case above is not.
	if note := e.revertNote(project); !strings.Contains(note, "main.go") {
		t.Errorf("the project's note does not name the file:\n%s", note)
	}

	// And the gate is on the other note too, which matters more here than for a revert: both
	// chats are given the same folder, so a check that trusted the workspace rather than the
	// mode would put its paths into a research prompt. A second file holds text the app did
	// not write, and unlike the reverted one it is still in the folder — the panel's list, not
	// the revert marker, is what this note is read from.
	if err := os.WriteFile(filepath.Join(project.Workspace, "second.go"),
		[]byte("package main\n// hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []store.Chat{project, plain} {
		if err := e.Store.PutFileSnapshot(store.FileSnapshot{
			ChatID: chat.ID, Path: "second.go", Existed: true,
			AfterSHA:  workspace.Digest("package main\n// model\n"),
			Revisions: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("record the change: %v", err)
		}
	}
	if note, _ := e.externalNote(plain); note != "" {
		t.Errorf("a chat with no coding mode was told about a file changed outside the app:\n%s", note)
	}
	if note, _ := e.externalNote(project); !strings.Contains(note, "second.go") {
		t.Errorf("the project was not told the file differs from what the app wrote:\n%s", note)
	}
}

// TestATurnIsToldWhenAFileChangedOutsideTheApp is the other way a tool result in the transcript
// goes false. The app wrote a file and recorded the digest of what it wrote; somebody then
// edited it somewhere the app cannot see — their own editor, a formatter in another window, a
// checkout that replaced the tree — and the history still says the model's text is in there. The
// folder is right and the transcript is wrong, so the correction travels beside the history.
//
// It is not the revert note with another trigger, and the difference is what a test has to keep:
// nothing was undone, the text is the user's own, and a note that suggested otherwise would send
// the model looking for a change its user deliberately kept.
func TestATurnIsToldWhenAFileChangedOutsideTheApp(t *testing.T) {
	e, provider, project := projectTurnEngine(t, answer("Gut."), answer("Gut."), answer("Gut."),
		answer("Gut."), answer("Gut."))
	path := filepath.Join(project.Workspace, "main.go")
	written := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.Store.PutFileSnapshot(store.FileSnapshot{
		ChatID: project.ID, Path: "main.go", BeforeText: "package main\n", Existed: true,
		AfterSHA: workspace.Digest(written), Revisions: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("record the change: %v", err)
	}

	// The file still holds what the app wrote, so there is nothing to announce. This is the
	// ordinary turn of a project, and it must stay as cheap and as quiet as it was.
	turn(t, e, project.ID, "Alles gut?")
	if system := systemOf(t, provider.lastRequest()); strings.Contains(system, diskOpen) {
		t.Fatalf("a file nobody touched was announced as changed:\n%s", system)
	}

	// The user's own editor, which this app never sees.
	outside := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	if err := os.WriteFile(path, []byte(outside), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t, e, project.ID, "Mach weiter.")
	system := systemOf(t, provider.lastRequest())
	if !strings.Contains(system, diskOpen) {
		t.Fatalf("the turn after an outside edit carries no note:\n%s", system)
	}
	if !strings.Contains(system, "main.go") {
		t.Errorf("the note does not name the file that changed:\n%s", system)
	}
	// The action, not just the news: the model's memory of the file is what is wrong.
	if !strings.Contains(system, "Lies") || !strings.Contains(system, "erneut") {
		t.Errorf("the note does not tell the model to read the file again:\n%s", system)
	}
	// And it is not a revert: the change on disk is the user's, and a model told it was put
	// back would go looking for the version it never restored.
	if strings.Contains(system, revertedOpen) {
		t.Errorf("an outside edit was announced as a revert:\n%s", system)
	}

	// Told once per outside version. The same text is not news twice, so the next turn's
	// prompt is the ordinary one again.
	turn(t, e, project.ID, "Und jetzt?")
	if next := systemOf(t, provider.lastRequest()); strings.Contains(next, diskOpen) {
		t.Errorf("the same outside version was announced twice:\n%s", next)
	}

	// A version edited after the announcement is a new fact, and speaks for itself — which is
	// what the marker is for rather than a single "told" bit.
	if err := os.WriteFile(path, []byte(outside+"// noch eins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn(t, e, project.ID, "Weiter.")
	if again := systemOf(t, provider.lastRequest()); !strings.Contains(again, diskOpen) {
		t.Errorf("a second outside edit was not announced:\n%s", again)
	}

	// Deleting the file outside the app is the same kind of fact and the sharpest one: every
	// line the model thinks is in there is gone.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	turn(t, e, project.ID, "Noch da?")
	gone := systemOf(t, provider.lastRequest())
	if !strings.Contains(gone, diskOpen) || !strings.Contains(gone, "nicht mehr vorhanden") {
		t.Errorf("a file removed outside the app was not reported as gone:\n%s", gone)
	}
}

// TestAProjectRepliesOnItsOwnRoundBudget pins the split between the two round figures. A
// project was being cut at ten rounds because the only figure there was — the chat's — said
// ten; the project now reads its own, and a chat keeps its own even when the project's is
// the larger one. The numbers are small on purpose: ten and fifteen, with more scripted
// tool calls than either, so the loop can be counted rather than inferred.
func TestAProjectRepliesOnItsOwnRoundBudget(t *testing.T) {
	folder := t.TempDir()
	workspace.ForgetAgentDocs()

	st := newTestStore(t)
	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9,
		Mode: store.ModeCoding, Workspace: folder,
	}
	plain := store.Chat{
		ID: "chat-1", Title: "Chat", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.",
		Temperature:  0.7, TopP: 0.9,
	}
	for _, chat := range []store.Chat{project, plain} {
		if err := st.CreateChat(chat); err != nil {
			t.Fatalf("create chat: %v", err)
		}
	}

	// A tool both modes offer, so each loop is counted for its own budget rather than
	// stopping because the tool it named is unknown in one of them.
	call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
	reply := []llm.StreamEvent{{Type: llm.EventDelta, Text: call}}
	replies := make([][]llm.StreamEvent, 30)
	for i := range replies {
		replies[i] = reply
	}
	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192,
			// Capabilities without tools keep the text protocol in play.
			State: llm.StateLoaded, Capabilities: []string{"vision"},
		},
		replies: replies,
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 10
	settings.CodingToolIterations = 15
	engine := NewEngine(st, registry, &tools.Service{Store: st},
		func() config.Settings { return settings }, (&eventLog{}).emit)

	turn(t, engine, project.ID, "Recherchiere Mietrecht.")
	if n := provider.requestCount(); n != 15 {
		t.Errorf("a project's reply took %d rounds, want its own 15 rather than the chat's 10", n)
	}

	before := provider.requestCount()
	turn(t, engine, plain.ID, "Recherchiere Mietrecht.")
	if n := provider.requestCount() - before; n != 10 {
		t.Errorf("a chat's reply took %d rounds, want its own 10 rather than the project's 15", n)
	}
}

// TestAProjectCarriesMoreToolResultThanItsChatBudget is the other half of the same split:
// the text a tool returns is budgeted by mode too. A source file that fits the project's
// figure but not the chat's arrives whole in a project — the failure the shared figure
// caused was a project reading notices with most of its rounds still unspent, because the
// chat's 60 000 tokens are two whole reads of a file the size of `app.go`.
func TestAProjectCarriesMoreToolResultThanItsChatBudget(t *testing.T) {
	folder := t.TempDir()
	workspace.ForgetAgentDocs()
	// About 3 700 tokens: past a 100-token chat budget, and small against this window.
	body := "package main\n" +
		strings.Repeat("// eine Zeile Inhalt, die gelesen werden muss\n", 300) +
		"// ZULETZT\n"
	if err := os.WriteFile(filepath.Join(folder, "main.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	project := store.Chat{
		ID: "project-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9,
		Mode: store.ModeCoding, Workspace: folder,
	}
	plain := store.Chat{
		ID: "chat-1", Title: "Chat", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.",
		Temperature:  0.7, TopP: 0.9,
	}
	for _, chat := range []store.Chat{project, plain} {
		if err := st.CreateChat(chat); err != nil {
			t.Fatalf("create chat: %v", err)
		}
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 32768,
			State: llm.StateLoaded, Capabilities: []string{llm.CapTools},
		},
		replies: [][]llm.StreamEvent{
			{{Type: llm.EventToolCall, ToolCall: &llm.ToolCall{
				ID: "call-1", Name: tools.ToolReadFile, Arguments: `{"path":"main.go"}`,
			}}},
			{{Type: llm.EventDelta, Text: "Gelesen."}, {Type: llm.EventDone}},
		},
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolTokenBudget = 100
	settings.CodingToolTokenBudget = config.DefaultCodingToolTokenBudget
	engine := NewEngine(st, registry, &tools.Service{Store: st},
		func() config.Settings { return settings }, (&eventLog{}).emit)

	turn(t, engine, project.ID, "Lies main.go.")

	messages, err := st.ListMessages(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	var carried string
	for _, m := range messages {
		if m.Role == string(llm.RoleTool) && m.Name == tools.ToolReadFile {
			carried = m.Content
		}
	}
	if carried == "" {
		t.Fatal("the read's result was not kept as a tool message")
	}
	if !strings.Contains(carried, "ZULETZT") {
		t.Errorf("the project read %d characters, not the whole file — the chat's 100-token budget was used",
			len(carried))
	}
	if strings.Contains(carried, "gekürzt") {
		t.Errorf("the project's result was cut although its own budget is %d tokens: %q",
			settings.CodingToolTokenBudget, carried[:min(len(carried), 200)])
	}

	// The other direction, asked of the selector: a chat has no tool in this test that can
	// return more than 100 tokens, so its figure is pinned where the mode is decided.
	if got := maxToolResultTokens(plain, settings); got != settings.ToolTokenBudget {
		t.Errorf("a chat's budget = %d, want its own %d", got, settings.ToolTokenBudget)
	}
}

// TestAProjectChecksBetweenRoundsWithoutTheChatSwitch pins the project's half of the
// between-rounds fold. `Compact.MidTurnCheck` is a choice a *chat* can make, because a chat
// can end the answer and be folded before the next message; a project's reply is one piece
// of work that grows a tool result at a time, and the round it is in the middle of is that
// work — the assembly only measures an over-full request, it never trims one. So a project
// asks its threshold between rounds with the switch where it ships, and the master switch is
// still the master switch.
func TestAProjectChecksBetweenRoundsWithoutTheChatSwitch(t *testing.T) {
	// run plays one project turn whose first reply asks for a tool *and* writes a long
	// report, so the round after it is the one that can outgrow the threshold. With no
	// published ceiling — every local backend's shape — the threshold is the only bound
	// that can fold it.
	run := func(t *testing.T, configure func(*config.Settings)) (summaries int, carriedOldest bool) {
		t.Helper()
		folder := t.TempDir()
		workspace.ForgetAgentDocs()

		st := newTestStore(t)
		project := store.Chat{
			// chat-1 because the history and summary helpers are written against it.
			ID: "chat-1", Title: "Projekt", CreatedAt: time.Now(), UpdatedAt: time.Now(),
			Backend: "fake", Model: "fake-model",
			SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
			Temperature:  0.7, TopP: 0.9, AutoCompact: true,
			Mode: store.ModeCoding, Workspace: folder,
		}
		if err := st.CreateChat(project); err != nil {
			t.Fatalf("create chat: %v", err)
		}
		// More turns than KeepRecentTurns, or the planner has nothing older than this turn
		// to fold and the assertion would pass for the wrong reason.
		seedLongHistory(t, st, 8)

		call := "```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"mietrecht\"}}\n```"
		report := strings.Repeat("Der Bericht ist lang und wird länger. ", 450)
		provider := &fakeProvider{
			// No ceiling: this is the backend shape only the threshold can fold for.
			model: llm.ModelInfo{ID: "fake-model", Backend: "fake", ContextLimit: 8192, State: llm.StateLoaded},
			replies: [][]llm.StreamEvent{
				{{Type: llm.EventDelta, Text: call + "\n\n" + report}},
				{{Type: llm.EventDelta, Text: "Zusammenfassung"}, {Type: llm.EventDone}},
				{{Type: llm.EventDelta, Text: "Fertig."}, {Type: llm.EventDone}},
			},
		}
		registry := llm.NewRegistry(nil)
		registry.Register(provider)

		settings := config.Default()
		settings.AutoTitle = false
		// The chat switch, left exactly where it ships.
		settings.Compact.MidTurnCheck = false
		settings.Compact.Threshold = 0.8
		configure(&settings)
		engine := NewEngine(st, registry, &tools.Service{Store: st},
			func() config.Settings { return settings }, (&eventLog{}).emit)

		// The check before the turn has to stay quiet, or the fold measured below would be
		// that one and the test would prove nothing about the mid-turn path.
		before, _, err := engine.buildAssembly(context.Background(), &project, "fake", "fake-model",
			settings, provider.model, tools.TextProtocolHint)
		if err != nil {
			t.Fatalf("buildAssembly: %v", err)
		}
		if before.Budget.ProjectedRatio >= settings.Compact.Threshold {
			t.Fatalf("the turn's own check already folds at %.2f", before.Budget.ProjectedRatio)
		}

		turn(t, engine, project.ID, "Eine neue Frage")
		last := provider.requestAt(provider.requestCount() - 1)
		for _, m := range last.Messages {
			if strings.Contains(m.Content, "Frage 0:") {
				carriedOldest = true
			}
		}
		return len(summariesOf(t, st)), carriedOldest
	}

	summaries, carriedOldest := run(t, func(*config.Settings) {})
	if summaries != 1 {
		t.Fatalf("compactions in a project with the chat switch off = %d, want the one the second round asked for",
			summaries)
	}
	if carriedOldest {
		t.Error("the round after the fold still carried the oldest turn verbatim")
	}

	// Not an override of the feature: automatic compaction switched off is still no fold.
	if summaries, _ := run(t, func(s *config.Settings) { s.Compact.Enabled = false }); summaries != 0 {
		t.Fatalf("compactions with automatic compaction off = %d, want none", summaries)
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
