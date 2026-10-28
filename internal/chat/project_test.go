package chat

import (
	"context"
	"os"
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

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
