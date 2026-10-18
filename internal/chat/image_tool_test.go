package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/llm"
	"kichat/internal/store"
	"kichat/internal/tools"
)

const chatImageToolID = "chat-image-tool"

// stubImageProvider stands in for a configured picture server. It never draws:
// these tests are about whether the tool is *advertised*, which is decided before
// any request reaches a server.
type stubImageProvider struct{}

func (stubImageProvider) ID() string                   { return "stub" }
func (stubImageProvider) Name() string                 { return "Stub WebUI" }
func (stubImageProvider) Health(context.Context) error { return nil }
func (stubImageProvider) Models(context.Context) ([]images.Model, error) {
	return nil, nil
}
func (stubImageProvider) Samplers(context.Context) ([]string, error) { return nil, nil }
func (stubImageProvider) Generate(context.Context, images.Request) (images.Result, error) {
	return images.Result{}, nil
}

// TestTheImageToolIsInvisibleUntilAServerIsConfigured is the contract the Settings
// panel's off state rests on: with no picture server configured, `generate_image`
// must not be visible to the model anywhere. A tool the model can see but never use
// costs a tool round every time it tries, and its complaint about the failure reads
// as a broken feature rather than as a setting that is switched off.
//
// Three separate places could leak it, and each is asserted rather than assumed. The
// native tool definitions are one. The system prompt's `{{tool_list}}` line is
// another, and it is the one a filter in toolDefinitions alone would miss — it is
// rendered from the same filtered slice, and this is what keeps the two from drifting
// apart. The text-protocol hint is the third, and it is a constant of its own that
// has to travel with the tool.
//
// Every one is asserted from both sides. "Absent when off" would also pass if the
// tool were never advertised at all, which is precisely the regression this pins, so
// the configured case has to show the tool in each place the off case hides it.
func TestTheImageToolIsInvisibleUntilAServerIsConfigured(t *testing.T) {
	states := []struct {
		name        string
		provider    images.Provider
		wantVisible bool
	}{
		{name: "off", provider: nil, wantVisible: false},
		{name: "configured", provider: stubImageProvider{}, wantVisible: true},
	}
	// The two paths advertise the tool differently — capabilities decide which is in
	// play — so both are run for both states.
	paths := []struct {
		name         string
		capabilities []string
		native       bool
	}{
		{name: "native tools", capabilities: []string{llm.CapTools}, native: true},
		{name: "text protocol", capabilities: nil, native: false},
	}

	for _, state := range states {
		for _, path := range paths {
			t.Run(state.name+", "+path.name, func(t *testing.T) {
				provider, engine := imageToolEngine(t, path.capabilities, state.provider)

				if err := engine.Send(TurnRequest{ChatID: chatImageToolID, Text: "Hallo"}); err != nil {
					t.Fatalf("Send: %v", err)
				}
				if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning(chatImageToolID) }) {
					t.Fatal("the turn did not finish")
				}

				req := provider.lastRequest()
				if len(req.Messages) == 0 {
					t.Fatal("no request was recorded")
				}
				system := string(req.Messages[0].Content)
				if !strings.Contains(system, "Verfügbare Tools:") {
					t.Fatalf("the fixture's tool_list variable was not rendered, so this test "+
						"would prove nothing either way:\n%s", system)
				}

				// The hint is the text path's own copy of the tool list: it is what tells
				// a model without native tool calling that the tool exists at all.
				if got := strings.Contains(system, tools.ImageProtocolHint); got != (state.wantVisible && !path.native) {
					t.Errorf("the image hint is in the system prompt = %v, want %v:\n%s",
						got, state.wantVisible && !path.native, system)
				}
				// A native model gets the restriction as a system rule of its own: it
				// never reads the text-protocol hint, and the tool description is only
				// consulted while it is already choosing a tool. Off, and on the text
				// path (where the hint carries the same rule), the line must be absent
				// rather than merely redundant.
				if got := strings.Contains(system, tools.ImageUseHint); got != (state.wantVisible && path.native) {
					t.Errorf("the image use rule is in the system prompt = %v, want %v:\n%s",
						got, state.wantVisible && path.native, system)
				}
				// Whichever path is in play, the ordinary tool list may not name it.
				if got := strings.Contains(system, tools.ToolGenerateImage); got != state.wantVisible {
					t.Errorf("the system prompt mentions %q = %v, want %v:\n%s",
						tools.ToolGenerateImage, got, state.wantVisible, system)
				}

				var inTools bool
				for _, def := range req.Tools {
					if def.Name == tools.ToolGenerateImage {
						inTools = true
					}
				}
				// The definitions only carry it on the native path, and only when a
				// server is configured; the text path sends no tools at all.
				if want := state.wantVisible && path.native; inTools != want {
					t.Errorf("the request's tool definitions carry %q = %v, want %v (tools: %v)",
						tools.ToolGenerateImage, inTools, want, toolDefinitionNames(req.Tools))
				}
			})
		}
	}
}

func toolDefinitionNames(defs []llm.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}

// TestTheCaptionRuleTravelsOnlyInModelMode pins the one instruction the caption modes
// differ by. `model` has to be in force while the model writes the tool argument; `auto`
// must not be, or the tool would ask for a caption on top of one the model already wrote;
// off must not be either, because that pipeline was not trained this way.
//
// Both paths are covered because they carry rules in different places: a model without
// native tool calling reads only the text-protocol hint, while a native one gets the rule
// as a system line beside `ImageUseHint`.
func TestTheCaptionRuleTravelsOnlyInModelMode(t *testing.T) {
	paths := []struct {
		name         string
		capabilities []string
	}{
		{name: "native tools", capabilities: []string{llm.CapTools}},
		{name: "text protocol", capabilities: nil},
	}
	// Five states, because the switch has four values and the empty one is resolved by the
	// profile: a FLUX 3 profile captions in `auto` mode, which must not also put the writing
	// rule in the system prompt.
	modes := []struct {
		name string
		cfg  config.ImageSettings
		want bool
	}{
		{name: "off", cfg: config.ImageSettings{}},
		{name: "auto", cfg: config.ImageSettings{Captioning: config.ImageCaptioningAuto}},
		{name: "model", cfg: config.ImageSettings{Captioning: config.ImageCaptioningModel}, want: true},
		{name: "profile captions", cfg: config.ImageSettings{Pipeline: config.ImagePipelineFlux3Klein}},
		{name: "profile overridden to model", cfg: config.ImageSettings{
			Pipeline: config.ImagePipelineFlux3Klein, Captioning: config.ImageCaptioningModel,
		}, want: true},
	}
	for _, path := range paths {
		for _, mode := range modes {
			t.Run(path.name+", "+mode.name, func(t *testing.T) {
				provider, engine := imageToolEngineWith(t, path.capabilities, stubImageProvider{}, mode.cfg)

				if err := engine.Send(TurnRequest{ChatID: chatImageToolID, Text: "Hallo"}); err != nil {
					t.Fatalf("Send: %v", err)
				}
				if !waitFor(t, 3*time.Second, func() bool { return !engine.IsRunning(chatImageToolID) }) {
					t.Fatal("the turn did not finish")
				}

				system := string(provider.lastRequest().Messages[0].Content)
				want := mode.want
				if got := strings.Contains(system, tools.ImageCaptionHint); got != want {
					t.Errorf("the caption rule is in the system prompt = %v, want %v:\n%s", got, want, system)
				}
			})
		}
	}
}

// imageToolEngine builds a real store, a chat whose system prompt carries
// `{{tool_list}}`, and an engine wired to those services.
//
// The system prompt is the point of the fixture. Without the variable the rendered
// prompt carries no tool list at all, and "the tool is absent" would pass without
// proving anything — which is why the test also asserts the list was rendered.
func imageToolEngine(t *testing.T, capabilities []string, imageProvider images.Provider) (*fakeProvider, *Engine) {
	t.Helper()
	return imageToolEngineWith(t, capabilities, imageProvider, config.ImageSettings{})
}

// imageToolEngineWith is the same fixture with the picture settings spelled out, which is
// how a test reaches the caption modes.
func imageToolEngineWith(t *testing.T, capabilities []string, imageProvider images.Provider, imageCfg config.ImageSettings) (*fakeProvider, *Engine) {
	t.Helper()
	st := newTestStore(t)
	chat := store.Chat{
		ID: chatImageToolID, Title: "Test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Backend: "fake", Model: "fake-model",
		SystemPrompt: "Du bist ein Assistent.\nVerfügbare Tools: {{tool_list}}",
		Temperature:  0.7, TopP: 0.9, AutoCompact: false,
	}
	if err := st.CreateChat(chat); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	provider := &fakeProvider{
		model: llm.ModelInfo{
			ID: "fake-model", Backend: "fake", ContextLimit: 8192,
			State: llm.StateLoaded, Capabilities: capabilities,
		},
		replies: [][]llm.StreamEvent{{{Type: llm.EventDelta, Text: "ok"}}},
	}
	registry := llm.NewRegistry(nil)
	registry.Register(provider)

	settings := config.Default()
	settings.AutoTitle = false
	settings.ToolIterations = 3
	settings.Images = imageCfg

	// Only the store and the picture provider: the search and scrape tools filter
	// themselves out on their own missing dependencies, which keeps the rendered list
	// short and the assertion about one name precise.
	service := &tools.Service{Store: st, Images: imageProvider}

	log := &eventLog{}
	return provider, NewEngine(st, registry, service, func() config.Settings { return settings }, log.emit)
}
