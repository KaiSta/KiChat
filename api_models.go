package main

import (
	"context"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/chat"
	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/llm"
	"kichat/internal/uimsg"
)

// PickImageFile asks for one of the CLI backend's paths: the binary, or one of the model
// files a pipeline is made of.
//
// One binding for both rather than two, because the difference is only the filter: a model
// file is `*.gguf`/`*.safetensors`, while the executable has no extension at all on Linux and
// `.exe` on Windows, so an empty pattern list — "all files" — is the only filter that shows
// either. Getting that wrong hides the very file the user was told to pick.
//
// Patterns reach Wails as one semicolon-separated string, which is its own format rather than
// glob alternation, and the title comes from the UI so it follows the interface language.
// The dialog opens in the model folder the app already knows about: the Qwen-Image triple is
// the shape this backend exists for, and it is downloaded to one folder.
func (a *App) PickImageFile(title string, patterns []string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	opts := runtime.OpenDialogOptions{Title: title, DefaultDirectory: a.defaultModelDir()}
	if len(patterns) > 0 {
		opts.Filters = []runtime.FileFilter{{DisplayName: "Modelldateien", Pattern: strings.Join(patterns, ";")}}
	}
	return runtime.OpenFileDialog(a.ctx, opts)
}

// PickImageFolder asks for the folder a stable-diffusion.cpp release unpacked into.
//
// A folder rather than a file, because that is the shape a download arrives in: the
// archive holds the program, its backend libraries and its DLLs or `.so` files
// together, and asking a user to find `sd-cli` inside it is asking them to open
// folders until they see a name with no extension. What the app does with the answer
// is find the program itself (images.FindProgram), including the single root directory
// an archive lays down one level below.
//
// The choice is checked before it is returned: a folder with no program in it is a
// mistake the user can still correct, and saying so here is one sentence instead of a
// red badge on the next reload. An empty answer is a cancelled dialog, which is
// neither success nor failure and is left for the caller to ignore.
func (a *App) PickImageFolder(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", nil
	}
	if _, err := images.FindProgram(dir, config.ModelLanguage(a.Settings().Language)); err != nil {
		return "", err
	}
	return dir, nil
}

// ListModels returns the merged cross-backend model catalogue.
func (a *App) ListModels(forceRefresh bool) ([]llm.ModelInfo, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if forceRefresh {
		a.currentRegistry().Invalidate()
	}
	return a.visibleModels(ctx), nil
}

// SetModelOverride stores a per-model context window, capability correction or
// per-request ceiling.
func (a *App) SetModelOverride(backend, model string, ov llm.ModelOverride) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.settingsMu.Lock()
	if a.settings.ModelOverrides == nil {
		a.settings.ModelOverrides = map[string]llm.ModelOverride{}
	}
	key := backend + "/" + model
	// Every field counts as content: an override that carried only a ceiling must not
	// be deleted for lacking a window, which is the state Groq models are in (they
	// report a window and publish no budget).
	if ov.ContextLimit == 0 && ov.RequestCeiling == 0 && len(ov.Capabilities) == 0 {
		delete(a.settings.ModelOverrides, key)
	} else {
		a.settings.ModelOverrides[key] = ov
	}
	settings := a.settings.Clone()
	a.settingsMu.Unlock()

	if err := a.store.PutModelOverride(backend, model, ov); err != nil {
		return err
	}
	a.currentRegistry().Invalidate()
	// Sanitised, because this is the one save that writes the *in-memory* snapshot
	// rather than a draft: no control in the panel edits a model override through
	// the settings form, so the snapshot is all this call has. SaveSettings can
	// persist its draft as it stands precisely because a draft never carries a key;
	// this one carries every key the app holds, and writing it as it stood put all
	// of them into the settings row.
	return a.sanitize(settings).Save(a.store)
}

// ModelCheckStatus is a model availability sweep as the picker reads it.
type ModelCheckStatus struct {
	Backend string `json:"backend,omitempty"`
	Busy    bool   `json:"busy"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	// Hidden is how many models the sweep that just finished put on the
	// blacklist, and HiddenTotal is the whole blacklist across every backend. The
	// picker shows the second: a user who switched to another backend would
	// otherwise see no sign that a list is shorter than the catalogue says.
	Hidden      int `json:"hidden"`
	HiddenTotal int `json:"hiddenTotal"`
	// Error is why a sweep could not run, or why it declined to act. It is a
	// reading rather than a failed call, because it is a row in the picker.
	Error string `json:"error,omitempty"`
	// Reason names the failure when the UI has its own sentence for it. Error is
	// the backend's own text and is passed through untranslated — that is what
	// every other Go error in this app does — but the one verdict the sweep itself
	// reaches has to be translatable, or a Chinese or English window would show a
	// German sentence in a row that stays on screen.
	Reason string `json:"reason,omitempty"`
}

// ReasonAllRefused is the sweep's own verdict that it declined to act: every
// model it reached was refused, which is what a wrong address or a key without
// entitlement looks like from here. The UI has a sentence of its own for it.
const ReasonAllRefused = "all-refused"

// modelCheckTimeout bounds a whole sweep. It is generous because it is the outer
// bound only: the per-model timeout is what a single hanging model costs, and six
// of NVIDIA's 80 models hang rather than answer, so a tight total would cut the
// sweep short and leave the models after it unasked.
const modelCheckTimeout = 10 * time.Minute

// StartModelCheck asks every model of one backend to answer one token, so the
// ones this account cannot call can be kept out of the picker.
//
// It returns as soon as the sweep is running rather than when it finishes. The
// sweep for build.nvidia.com's 80 models takes about a minute, and a bound call
// that blocked for it would look like a hang with nothing to show; the UI follows
// it by polling GetModelCheckStatus, the same way it follows a runtime download.
func (a *App) StartModelCheck(backend string) (ModelCheckStatus, error) {
	if err := a.ensure(); err != nil {
		return ModelCheckStatus{}, err
	}
	a.checkMu.Lock()
	if a.checkBusy {
		// A second click while one is running is the same request, not a new one:
		// two sweeps at once would double the requests and race on the result.
		status := a.checkStatusLocked()
		a.checkMu.Unlock()
		return status, nil
	}
	a.checkMu.Unlock()

	provider, ok := a.currentRegistry().Provider(backend)
	if !ok {
		return ModelCheckStatus{}, uimsg.Errorf("model.backendOff", backend)
	}

	// The sweep outlives the call that started it, so it cannot borrow the call's
	// context: Wails cancels that the moment this returns. The app's own context
	// bounds it instead, which is what makes a sweep stop when the window closes.
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, modelCheckTimeout)

	// Reachability is settled before the sweep rather than during it. A backend
	// that answers nothing at all refuses every model it is asked about, and that
	// reading is indistinguishable from a catalogue this account may not call — so
	// it would blacklist the whole list because a server was down.
	if err := provider.Health(ctx); err != nil {
		cancel()
		return ModelCheckStatus{}, uimsg.Errorf("model.backendUnreachable", backend, err)
	}

	// The listing is asked of the provider directly rather than read from the
	// visible catalogue: a re-check has to see the models the last sweep hid, or a
	// model wrongly kept out could never come back.
	models, err := provider.ListModels(ctx)
	if err != nil {
		cancel()
		return ModelCheckStatus{}, uimsg.Errorf("model.listFailed", backend, err)
	}
	if len(models) == 0 {
		cancel()
		return ModelCheckStatus{}, uimsg.Errorf("model.noModelsReported", backend)
	}

	a.checkMu.Lock()
	a.checkBackend, a.checkBusy, a.checkErr, a.checkReason = backend, true, "", ""
	a.checkDone, a.checkTotal, a.checkHidden = 0, len(models), 0
	status := a.checkStatusLocked()
	a.checkMu.Unlock()

	go func() {
		defer cancel()
		probes := llm.ProbeModels(ctx, provider, models, llm.ProbeOptions{}, func(llm.ModelProbe) {
			a.checkMu.Lock()
			a.checkDone++
			a.checkMu.Unlock()
		})
		hidden, reason, err := a.applyModelProbes(backend, probes)
		a.checkMu.Lock()
		a.checkBusy = false
		a.checkReason = reason
		if err != nil {
			a.checkErr = err.Error()
		} else {
			a.checkHidden = hidden
		}
		a.checkMu.Unlock()
	}()
	return status, nil
}

// GetModelCheckStatus reports the sweep in progress, or the last one's result.
// The picker polls it while a sweep runs and once after.
func (a *App) GetModelCheckStatus() ModelCheckStatus {
	settings := a.Settings()
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	status := a.checkStatusLocked()
	status.HiddenTotal = len(settings.HiddenModels)
	return status
}

// checkStatusLocked reads the sweep state; the caller holds checkMu.
func (a *App) checkStatusLocked() ModelCheckStatus {
	return ModelCheckStatus{
		Backend: a.checkBackend,
		Busy:    a.checkBusy,
		Done:    a.checkDone,
		Total:   a.checkTotal,
		Hidden:  a.checkHidden,
		Error:   a.checkErr,
		Reason:  a.checkReason,
	}
}

// applyModelProbes writes a sweep's verdicts into the blacklist and returns how
// many models of that backend it hides.
//
// Two rules keep it from being destructive on weak evidence. A sweep in which
// every model was refused is discarded rather than applied — that is what a wrong
// address or a key without entitlement looks like from here, and committing it
// would empty the picker. And a model the sweep could not judge (a timeout, a
// throttle) keeps the verdict it already had, so only a sweep that really reached
// a model can add it to the list or take it off.
func (a *App) applyModelProbes(backend string, probes []llm.ModelProbe) (int, string, error) {
	reached := map[string]bool{}
	refused := map[string]bool{}
	for _, p := range probes {
		if p.Model == "" || p.Kind == "" {
			continue // never asked: the sweep was stopped before it got here
		}
		reached[p.Model] = true
		if p.Refused() {
			refused[p.Model] = true
		}
	}
	switch {
	case len(reached) == 0:
		return 0, "", uimsg.Errorf("model.checkNone")
	case len(refused) == len(reached):
		return 0, ReasonAllRefused, uimsg.Errorf("model.checkAllRefused", len(reached))
	}

	settings := a.Settings()
	prefix := backend + "/"
	out := make([]string, 0, len(settings.HiddenModels)+len(refused))
	seen := map[string]bool{}
	add := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	for _, key := range settings.HiddenModels {
		model, ok := strings.CutPrefix(key, prefix)
		if !ok {
			add(key) // another backend's entry, untouched
			continue
		}
		if refused[model] || !reached[model] {
			// Either still refused, or a model this sweep could not judge — in
			// which case the earlier verdict stands rather than being dropped by a
			// timeout that says nothing about the model.
			add(key)
		}
	}
	for _, p := range probes {
		if p.Refused() {
			add(prefix + p.Model)
		}
	}

	a.settingsMu.Lock()
	a.settings.HiddenModels = out
	updated := a.settings.Clone()
	a.settingsMu.Unlock()
	// Sanitised, the way SaveSettings persists: the in-memory snapshot keeps the
	// real backend keys so the registry can authenticate, and the blob must not.
	if err := a.sanitize(updated).Save(a.store); err != nil {
		return 0, "", err
	}

	// Hiding is a statement about the picker, so a chat already pinned to one of
	// these models keeps working and keeps its trigger label; only the listing
	// changes.
	hidden := 0
	for _, key := range out {
		if strings.HasPrefix(key, prefix) {
			hidden++
		}
	}
	return hidden, "", nil
}

// ClearHiddenModels forgets the blacklist, for one backend or — with an empty id
// — for every backend.
//
// A sweep measures one moment: an account can be upgraded, and NVIDIA adds
// capacity behind a model that 404s today. A verdict therefore has to be
// reversible from the UI rather than by hand-editing the settings blob.
func (a *App) ClearHiddenModels(backend string) (ModelCheckStatus, error) {
	if err := a.ensure(); err != nil {
		return ModelCheckStatus{}, err
	}
	settings := a.Settings()
	kept := make([]string, 0, len(settings.HiddenModels))
	for _, key := range settings.HiddenModels {
		if backend == "" || !strings.HasPrefix(key, backend+"/") {
			kept = append(kept, key)
		}
	}

	a.settingsMu.Lock()
	a.settings.HiddenModels = kept
	a.settingsMu.Unlock()
	a.checkMu.Lock()
	a.checkBackend, a.checkHidden, a.checkErr, a.checkReason = "", 0, "", ""
	a.checkMu.Unlock()

	// Sanitised for the same reason as in applyModelProbes: the blob carries the
	// blacklist, never a key.
	if err := a.sanitize(a.Settings()).Save(a.store); err != nil {
		return ModelCheckStatus{}, err
	}
	return a.GetModelCheckStatus(), nil
}

// --- prompt presets --------------------------------------------------------

// PromptVariable describes a template variable available in system prompts.
//
// Key is the i18n key the settings view translates and Description is the
// untranslated fallback, the same pair a built-in preset name uses: the list sits
// directly under the prompt editor, so leaving it in one language made the screen
// read as English above and German below.
type PromptVariable struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	Description string `json:"description"`
}

// GetPromptVariables lists the supported template variables.
func (a *App) GetPromptVariables() []PromptVariable {
	return []PromptVariable{
		{Name: "datetime", Key: "promptvar.datetime", Description: "Aktuelles Datum, Uhrzeit und Wochentag"},
		{Name: "date", Key: "promptvar.date", Description: "Nur das Datum (TT.MM.JJJJ)"},
		{Name: "time", Key: "promptvar.time", Description: "Nur die Uhrzeit (HH:MM)"},
		{Name: "weekday", Key: "promptvar.weekday", Description: "Wochentag ausgeschrieben"},
		{Name: "location", Key: "promptvar.location", Description: "Der in den Einstellungen hinterlegte Standort"},
		{Name: "model", Key: "promptvar.model", Description: "Das aktuell gewählte Modell"},
		{Name: "backend", Key: "promptvar.backend", Description: "Das aktuelle Backend (LM Studio, Ollama, …)"},
		{Name: "context_limit", Key: "promptvar.context_limit", Description: "Größe des Kontextfensters in Tokens"},
		{Name: "context_used", Key: "promptvar.context_used", Description: "Zuletzt gemessene Kontextbelegung"},
		{Name: "tool_list", Key: "promptvar.tool_list", Description: "Liste der verfügbaren Tools"},
	}
}

// PreviewSystemPrompt renders a prompt template with live values.
func (a *App) PreviewSystemPrompt(tmpl string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	settings := a.Settings()
	models := a.visibleModels(context.Background())
	model := ""
	if len(models) > 0 {
		model = models[0].ID
	}
	return chat.PreviewPrompt(tmpl, settings.Location, model), nil
}

// UnresolvedPromptVars reports template variables that do not exist.
func (a *App) UnresolvedPromptVars(tmpl string) []string {
	return chat.UnresolvedVars(tmpl)
}

// --- compaction ------------------------------------------------------------
