package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/images"
	"kichat/internal/ingest"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
)

// ImageStatus is the picture backend as the Settings panel reads it.
type ImageStatus struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"baseURL"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	// Models and Samplers are never nil. A nil Go slice marshals to JSON null,
	// and the panel iterates both — the empty case is exactly what a fresh
	// WebUI with no checkpoint installed answers with, so it is the one that
	// must not arrive as null.
	Models   []string `json:"models"`
	Samplers []string `json:"samplers"`
	KeySet   bool     `json:"keySet"`
	// Kind is which backend draws: "" for the HTTP server, "cli" for a local binary.
	// The panel needs it to render the right controls, and it is the *stored* kind
	// rather than anything derived, so a hand-edited blob shows what it says.
	Kind string `json:"kind"`
	// Binary is the CLI kind's executable, empty for the server kind. It is reported
	// separately from BaseURL rather than reusing one field: a path and a URL are
	// checked, opened and copied differently, and a single field meaning one in each
	// kind ends up in a log line as a broken link.
	//
	// It is the *resolved* program — the one a draw would run — not necessarily what
	// was typed: a configured program folder supplies it (images.ResolveProgram), and
	// the panel showing the file that will really be started is the point of resolving
	// it here rather than making the view re-do the search.
	Binary string `json:"binary"`
	// Folder is the CLI kind's program folder, empty when the program is named by path
	// instead. Reported so the panel can say which of the two is supplying the
	// executable, because they are not equivalent: a set folder wins, and one that
	// holds no program is a failure rather than a fallback.
	Folder string `json:"folder"`
	// Build is what the program folder *is*, read off its backend libraries:
	// "vulkan", "cuda", "rocm", "metal", or empty for the plain CPU build. It is a
	// reading rather than the stored CLIBuild, which is what the user meant to
	// download — and the two disagreeing is exactly what is worth showing.
	Build string `json:"build"`
	// Builds are the builds the project publishes for this machine (images.SDBuilds),
	// so the panel can offer only what can really be downloaded and name the archive to
	// look for. Never nil, because the panel iterates it.
	Builds []images.SDBuild `json:"builds"`
	// ReleasePage is where those archives live. It travels in the status rather than
	// being written into the interface a second time: it is the project's address, and
	// a copy of it in the frontend is a copy that goes stale on its own.
	ReleasePage string `json:"releasePage"`
	// Files are the local kinds' model paths, in the order they reach the binary. Both
	// this and Samplers exist so the panel can show what will really be loaded
	// without re-deriving it from the settings, and neither is ever nil.
	Files []string `json:"files"`
	// Program is the executable name the local kinds look for in the folder —
	// `sd-cli` or `sd-server` — so the panel can name the file it is reporting about
	// instead of hard-coding one of the two.
	Program string `json:"program"`
	// Pipeline is the *resolved* profile id and PipelineName its model family. Both are
	// reported rather than left to the view to derive, because the resolution is the one
	// place a renamed file changes what the app does: showing the result is what turns a
	// silent miss (no caption, wrong step count) into something the reader can see.
	Pipeline     string `json:"pipeline"`
	PipelineName string `json:"pipelineName"`
	// PipelineSource is where that profile came from — "chosen", "weights" for the
	// tensor layout inside the model file, or "name" for its file name, which is only
	// consulted when the weights cannot be read. Empty means nothing identified the model.
	// The panel prints it because the three are different claims: a name that lost the
	// `klein` marker still resolves from the weights, and saying which evidence was used
	// keeps a misidentification diagnosable.
	PipelineSource string `json:"pipelineSource"`
	// Captioning is the mode in force after the profile's default, so the panel can say
	// what will really happen when the user has left the switch on "follow the pipeline".
	Captioning string `json:"captioning"`
	// Server is the managed picture server's process row. It is always present and never
	// null, for the reason Models is: the panel reads its fields, and a null on a fresh
	// install would be a shape mistake in the one state every installation passes
	// through. Its State is one of internal/runtime's own — `stopped` when nothing runs,
	// which is what a kind with no process of its own has — and it is read for *every*
	// kind, because a server left running under another selection still has to be
	// stoppable.
	Server ImageServerStatus `json:"server"`
}

// ImageServerStatus is the managed picture server as a row in Settings.
//
// It mirrors the managed chat model's row rather than reusing its type: those are keyed by
// backend and carry llama.cpp's placement reading, which an image server does not report — a
// shared type would have half its fields permanently empty on one side and a reader could
// not tell "nothing to say" from "forgot to fill it in".
type ImageServerStatus struct {
	// State is one of internal/runtime's states, empty when the selected kind has no
	// process of its own.
	State string `json:"state"`
	URL   string `json:"url"`
	Port  int    `json:"port"`
	PID   int    `json:"pid"`
	// Error is why the last start or the running process failed, in the program's own
	// words where it had any.
	Error string `json:"error,omitempty"`
	// Log is the tail of the server's own output, never nil.
	Log []string `json:"log"`
	// RestartNeeded reports that the running server was started from a different
	// command line than the settings now describe. It is a reading rather than an
	// action: the panel offers the restart, because saving settings must never reload a
	// model that may have taken minutes to load.
	RestartNeeded bool `json:"restartNeeded"`
	// StartWithApp is the stored switch, reported so the row can show it without the
	// panel having to find it in the draft.
	StartWithApp bool `json:"startWithApp"`
}

// imageStatusFiles lists the model paths the CLI backend will pass, in argument order,
// skipping the empty ones. An empty field is not listed: it is omitted from the command
// line, and a blank row in the panel would suggest the binary is being handed something.
func imageStatusFiles(s config.ImageSettings) []string {
	out := []string{}
	for _, path := range (images.ModelFiles{
		Model:          s.CLIModel,
		DiffusionModel: s.CLIDiffusionModel,
		VAE:            s.CLIVAE,
		LLM:            s.CLILLM,
		ClipL:          s.CLIClipL,
		ClipG:          s.CLIClipG,
		T5XXL:          s.CLIT5XXL,
	}).Paths() {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// GetImageStatus reports the image server's state: whether the feature is on,
// whether the address answers, and what it offers.
//
// A switched-off feature reports no error at all. Nothing was contacted, so
// nothing failed, and saying otherwise would put a red row in front of a user who
// never asked for image generation — which is the mistake the backends panel
// already made once with a disabled entry.
func (a *App) GetImageStatus() (ImageStatus, error) {
	if err := a.ensure(); err != nil {
		return ImageStatus{}, err
	}
	settings := a.Settings()
	status := ImageStatus{
		Enabled:  settings.Images.Enabled,
		BaseURL:  settings.Images.BaseURL,
		Models:   []string{},
		Samplers: []string{},
		Kind:     settings.Images.Kind,
		Binary:   settings.Images.CLIBinary,
		Folder:   settings.Images.CLIFolder,
		Files:    imageStatusFiles(settings.Images),
		Builds:   images.SDBuilds(goruntime.GOOS, goruntime.GOARCH),
		Server:   ImageServerStatus{State: rt.StateStopped, Log: []string{}},

		ReleasePage: images.ReleasePage,
	}
	// The profile is resolved here rather than in the window: it reads the diffusion model's
	// file name, and a second copy of that rule would be a second answer to one question.
	pipeline := images.PipelineFor(settings.Images.Pipeline, settings.Images.CLIDiffusionModel)
	status.Pipeline = pipeline.ID
	status.PipelineName = pipeline.Name
	status.PipelineSource = pipeline.Source
	status.Captioning = pipeline.CaptioningMode(settings.Images.Captioning)

	// Which of the archive's two programs this kind runs, and what the folder really
	// holds for it. A folder supplies one executable or the other: the one-shot kind
	// runs sd-cli, the managed kind runs sd-server, and the resolved path is the one a
	// start or a draw would really use rather than the one the folder is named after.
	// The hosted kind runs no program at all, so there is no folder to read and no
	// executable to name — reporting `sd-cli` for it would be a field about nothing.
	if !config.IsImageNIM(settings.Images.Kind) {
		resolve, program := images.ResolveProgram, images.ProgramName(goruntime.GOOS)
		if config.IsImageSD(settings.Images.Kind) {
			resolve, program = images.ResolveServerProgram, images.ServerProgramName(goruntime.GOOS)
		}
		status.Program = program
		lang := config.ModelLanguage(settings.Language)
		if binary, err := resolve(settings.Images.CLIFolder, settings.Images.CLIBinary, lang); err == nil {
			status.Binary = binary
			status.Build = images.AcceleratorOf(filepath.Dir(binary))
		}
	}
	// Reported whatever the selected kind is, and that is load-bearing rather than uniform:
	// switching the kind away from the managed server while one is running leaves the process
	// up — a save never reloads a model that may have taken minutes to load — and a row that
	// was only filled for `sdserver` would hide the one control that can stop it. A server
	// with no row is several gigabytes of VRAM with nothing left to release it.
	status.Server = a.imageServerStatus(settings)
	// The hosted kind has no address of the user's to report, and its credential is the NVIDIA
	// backend's rather than a field of this section. Both are resolved here so the panel shows
	// what a draw would really use instead of what this section happens to store.
	if config.IsImageNIM(settings.Images.Kind) {
		status.BaseURL = images.NIMBaseURL
	}
	status.KeySet = a.imageAPIKey(settings) != ""

	provider := a.currentImageProvider()
	if provider == nil {
		return status, nil
	}
	// The same bound as a backend health probe: a WebUI that is up answers in
	// milliseconds, and one that is not must not hold the settings view open.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := provider.Health(ctx); err != nil {
		status.Error = err.Error()
		return status, nil
	}
	status.OK = true
	if models, err := provider.Models(ctx); err == nil {
		for _, m := range models {
			status.Models = append(status.Models, m.ID)
		}
	}
	if samplers, err := provider.Samplers(ctx); err == nil {
		status.Samplers = append(status.Samplers, samplers...)
	}
	return status, nil
}

// imageTestDrawID is the id the settings panel's test draw is registered under, and the one
// its Stop button sends. It is a fixed string rather than a uuid because the draw has no
// attachment and no message to derive one from — the panel that starts it is the thing that
// names it, on both sides of the bridge.
const imageTestDrawID = "image-test"

// TestImageBackend runs one generation and discards the result, so a user can
// find out that their WebUI is reachable and has a model loaded without spending
// a chat turn on it.
//
// The bound is the backend's own, exactly as it is for a redraw: the CLI kind pays a model
// load before its first step, and a fixed five minutes would report a working setup as a
// failure on a machine that needed six.
func (a *App) TestImageBackend() (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	settings := a.Settings()
	provider := a.currentImageProvider()
	if provider == nil {
		// A managed server that is not running has no provider at all, and saying "no
		// backend is configured" about a filled-in, saved configuration would send the
		// user looking for a mistake that is not there.
		if config.IsImageSD(settings.Images.Kind) {
			return "", uimsg.Errorf("image.serverNotRunning")
		}
		if config.IsImageNIM(settings.Images.Kind) {
			return "", uimsg.Errorf("image.noNvidiaKey")
		}
		return "", images.ErrNotConfigured
	}
	if a.ctx == nil {
		return "", uimsg.Errorf("app.notReady")
	}
	// A test draws a real picture, so it queues behind every other draw rather than beside it:
	// on the CLI and managed kinds it pays a model load, and the picture the *user* asked for
	// must not wait behind a colour swatch. Refused rather than queued, because the panel's
	// answer — "already drawing" — is something a user can act on.
	releaseDraw, ok := a.tryDraw()
	if !ok {
		return "", images.ErrDrawInProgress
	}
	defer releaseDraw()
	// The size is the user's own when they chose one, and a small square otherwise:
	// a setup that only works at its trained size would otherwise fail a test that
	// real draws pass. Bounded by the same call every other request goes through.
	dimension := func(value int) int {
		if value > 0 {
			return value
		}
		return 512
	}
	request := images.Request{Width: dimension(settings.Images.Width), Height: dimension(settings.Images.Height)}.Normalised()
	ctx, cancel := context.WithTimeout(a.ctx, imageDrawTimeout(settings, request))
	defer cancel()
	// Registered so the button that started it can also stop it. The id is fixed rather than a
	// uuid because there is no attachment and no message behind this draw: the panel's Stop
	// sends the same string, which is the whole contract. No progress callback is wired to it,
	// so the row never renders a progress line for a draw nobody is watching.
	release := a.registerDraw(imageTestDrawID, "", cancel)
	defer release()
	result, err := provider.Generate(ctx, images.Request{
		Lang:           config.ModelLanguage(settings.Language),
		Prompt:         "a simple color test pattern",
		NegativePrompt: settings.Images.NegativePrompt,
		Width:          request.Width,
		Height:         request.Height,
		Steps:          settings.Images.Steps,
		Sampler:        settings.Images.Sampler,
		CfgScale:       settings.Images.CfgScale,
		Model:          settings.Images.GenerationModel(),
	}.Normalised())
	if err != nil {
		// The same split a redraw makes: this context is cancelled by the Stop button and by
		// nothing else, so a cancelled context is the user's own click — unless the app itself
		// is closing, which reads as what it is.
		if errors.Is(ctx.Err(), context.Canceled) && a.ctx.Err() == nil {
			return "", images.ErrCancelled
		}
		return "", err
	}
	report := fmt.Sprintf("%d Bild(er) erzeugt (%s)", len(result.Images), provider.Name())
	if result.Model != "" {
		report += ", Modell " + result.Model
	}
	return report, nil
}

// RegenerateImage draws a picture from the recipe it is handed and returns the new
// attachment.
//
// This is the only path into image generation that does not go through a chat model: a
// recipe is data, not a request in words, so nothing has to be asked of the model to use it.
// The panel sends the whole recipe every time, which is what gives it three controls out of
// one call — a repeat passes the stored recipe back untouched, a variation passes it with
// the seed cleared, and the editor passes the fields the user changed. A zero field means
// "the server's own", so an emptied box in the editor means what it looks like it means.
//
// The picture is filed onto the message it was redrawn from, which is what puts it beside
// the original, and the attachments event is emitted because the view holds that message's
// attachment list — without it the new picture would exist and stay invisible until a manual
// reload.
func (a *App) RegenerateImage(attachmentID string, recipe store.GenerationRecipe) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	service := a.currentToolService()
	if service == nil {
		return store.Attachment{}, images.ErrNotConfigured
	}
	if a.ctx == nil {
		return store.Attachment{}, uimsg.Errorf("app.notReady")
	}
	// One picture at a time, and taken here rather than inside the service so that a refused
	// draw never reaches registerDraw — registering would cancel the draw already running
	// under this attachment, which is exactly the picture the user is waiting for.
	releaseDraw, ok := a.tryDraw()
	if !ok {
		return store.Attachment{}, images.ErrDrawInProgress
	}
	defer releaseDraw()
	// Generous, because a local backend drawing a large picture takes minutes on a CPU, and
	// present at all so a wedged process cannot hold the click open indefinitely. How
	// generous is the backend's own answer: a server holds its model in memory and spends
	// the bound on sampling, while the CLI kind loads one from disk first. A user who will
	// not wait that long presses Stop, which cancels this context — that, rather than the
	// bound, is the control for a draw that is merely slow.
	// The size is the recipe's, because that is what decides how much work the bound has to
	// cover: the same redraw of the same recipe at 4K is an order of magnitude longer than at
	// the size it was drawn at, and a bound that could not tell them apart would either cut
	// the large one off or hold the small one open far too long.
	size := images.Request{
		Width:  recipe.Width,
		Height: recipe.Height,
		Hires: images.Hires{
			Enabled: recipe.Hires,
			Scale:   recipe.HiresScale,
			Denoise: recipe.HiresDenoise,
		},
	}.Normalised()
	ctx, cancel := context.WithTimeout(a.ctx, imageDrawTimeout(a.Settings(), size))
	defer cancel()
	// The chat is read here rather than inside the service so the progress events can say
	// which chat they belong to, the way every other event does; one indexed row read is a
	// smaller price than a window that has to guess.
	chatID := ""
	if source, err := a.store.GetAttachment(attachmentID); err == nil {
		chatID = source.ChatID
	}
	release := a.registerDraw(attachmentID, chatID, cancel)
	defer release()

	attachment, err := service.Regenerate(ctx, attachmentID, recipe)
	if err != nil {
		// A stopped draw is not a failure and must not travel as one. The context is the
		// only thing that can have cancelled this call, so a cancelled context is what
		// tells the user's own click from a server that broke — and the *parent* being
		// gone is the app closing rather than a stop, which is left to read as what it is.
		if errors.Is(ctx.Err(), context.Canceled) && a.ctx.Err() == nil {
			return store.Attachment{}, images.ErrCancelled
		}
		return store.Attachment{}, err
	}
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// StopDrawResult is what stopping an image draw achieved.
//
// Three facts rather than a bare bool, because they are different sentences and the UI
// has to be able to pick the right one: a draw was running and is now over (say so),
// nothing was running (say nothing — this is a second click, or a draw that finished
// between the render and the click), and whether the *server* was also told to stop.
type StopDrawResult struct {
	// WasDrawing is true when a draw was in flight and is now over.
	WasDrawing bool `json:"wasDrawing"`
	// Interrupted is true when the image server was asked to abandon the work behind it.
	// False is not a failure: stable-diffusion.cpp has no route for it.
	Interrupted bool `json:"interrupted"`
	// InterruptError says why the server could not be asked. It is reported rather than
	// dropped so a server that has the route can be told from one that does not — but it
	// is never a failure *of the stop*: the wait it was pressed to end is already over.
	InterruptError string `json:"interruptError,omitempty"`
}

// reportDrawProgress turns one report from a running draw into an event the window can
// paint.
//
// The preview travels as a data: URL for the same reason a stored picture does — the
// webview loads from an embedded assets handler, and a path would be blocked by the same
// origin rule — and because the bytes are already in hand: a second bridge round trip to
// fetch what this call has just read would be a request per step.
//
// Called from the drawing goroutine, so it only touches the emitter, which is safe from
// any goroutine.
func (a *App) reportDrawProgress(id, chatID string, progress images.Progress) {
	if id == "" {
		return
	}
	// Noted so the release knows there is a row on screen to clear. A report for an id
	// nothing is registered under is still emitted: the window keys its rows by draw id, so
	// the worst it can do is update one nothing else will touch.
	a.drawsMu.Lock()
	if draw, ok := a.draws[id]; ok {
		draw.reported = true
		a.draws[id] = draw
	}
	a.drawsMu.Unlock()
	a.emit(events.ImageProgress, drawProgressPayload(id, chatID, progress, false))
}

// drawProgressPayload decides the shape of one `image:progress` event, in one place because
// two calls emit it — a report and the end of a draw.
//
// The keys are the interface the window reads, and a renamed key is not a compile error on
// either side of the bridge: it is a progress row that silently stops appearing while the
// draw itself keeps working. That is what the test in image_progress_test.go pins.
//
// `done` is part of every payload rather than a second event name, so a listener that misses
// one cannot be left holding a progress row for a draw that is over — which is a Stop button
// for something that is not running.
func drawProgressPayload(id, chatID string, progress images.Progress, done bool) map[string]any {
	// An ordinary draw (or the zero report that announces one) leaves both at zero, and
	// the window's sentence is "one pass" rather than a blank — so the absent statement is
	// spelled here, once, instead of in every render.
	pass, passes := progress.Pass, progress.Passes
	if passes <= 0 {
		passes = 1
	}
	if pass <= 0 {
		pass = 1
	}
	payload := map[string]any{
		"drawId": id,
		"chatId": chatID,
		"step":   progress.Step,
		"steps":  progress.Steps,
		// Which sampling pass this step belongs to. A draw with a second pass restarts
		// its step counter at 1 halfway through, so without these the window would show
		// a counter that appears to go backwards.
		"pass":   pass,
		"passes": passes,
		"done":   done,
	}
	// A phase is a part of the draw that is not sampling. It is a key of its own rather
	// than a step with a magic number: the window has a sentence to show for captioning,
	// and "step 0 of 0" would be a progress bar pretending to have started.
	if progress.Phase != "" {
		payload["phase"] = progress.Phase
	}
	if len(progress.Preview) > 0 && progress.PreviewMime != "" {
		payload["preview"] = "data:" + progress.PreviewMime + ";base64," +
			base64.StdEncoding.EncodeToString(progress.Preview)
	}
	return payload
}

// StopImageDraw ends the wait on a picture being drawn and asks the server to abandon
// the work behind it.
//
// Both halves matter and neither replaces the other. Cancelling the request is what makes
// the click take effect at once — a 40-step draw on a CPU server is minutes, and the
// ten-minute bound on the call is not a promise the user should be held to. The interrupt
// is what stops the server spending those minutes on a picture nobody will ever see, and
// it exists on AUTOMATIC1111 and Forge and not on stable-diffusion.cpp, so it is attempted
// and never required.
//
// Nothing is stored either way: the picture only exists once the server answers with it,
// so a stopped draw leaves the transcript exactly as it was.
//
// drawID is whatever the view knows the draw by, and there are two of those: the attachment
// a redraw works on, and the assistant message a model-started draw belongs to (its tool
// call is rendered there). Passing an id nothing is registered under is not an error — it
// is the "nothing was running" answer a second click produces.
func (a *App) StopImageDraw(drawID string) StopDrawResult {
	result := StopDrawResult{}
	if a.ctx == nil {
		return result
	}
	a.drawsMu.Lock()
	draw, ok := a.draws[drawID]
	if !ok || draw.stopping != nil {
		a.drawsMu.Unlock()
		return result
	}
	draw.stopping = make(chan struct{})
	a.draws[drawID] = draw
	a.drawsMu.Unlock()
	defer close(draw.stopping)
	result.WasDrawing = true
	draw.cancel()
	// Cancelling is the half that always works, and for an upscale it is the whole of it:
	// nothing is registered with a service, because there is no server-side work to abandon
	// (see registerUpscale). A draw registered before the tool service existed — or by a path
	// that never filled the field — is in the same position: the wait it was pressed to end is
	// over either way, which is the answer a backend that cannot be interrupted gives too.
	if draw.service == nil {
		return result
	}
	// A fresh, short context on purpose: the draw's own context was cancelled by the line
	// above, and an interrupt sent on it would never leave the machine.
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	asked, err := draw.service.InterruptDraw(ctx)
	result.Interrupted = asked && err == nil
	if err != nil {
		result.InterruptError = err.Error()
	}
	return result
}

// tryDraw takes the app-wide draw slot, and reports false when another draw already holds
// it. The release is called exactly once.
//
// Every path that starts a picture goes through it — a model-started draw in
// tools.Service, a redraw and the settings test here — because "one at a time" is a fact
// about the machine (one GPU; the CLI kind loads a checkpoint per draw) rather than about
// any one of them. It is a TryLock and not a queue: a second request is answered "one is
// already running" at once, which a user can act on, instead of being parked for minutes.
func (a *App) tryDraw() (release func(), ok bool) {
	if !a.drawSlot.TryLock() {
		return nil, false
	}
	return a.drawSlot.Unlock, true
}

// registerDraw records a draw so Stop can find it, and returns the function that releases
// the registration. The body is registerPicture; this is the draw-shaped way in.
//
// It is called from three places, and that is the point of it: RegenerateImage registers the
// attachment a redraw works on, tools.Service registers the assistant message a
// model-started draw belongs to, and TestImageBackend registers itself under imageTestDrawID
// because a test has neither. The two uuid kinds cannot collide, and the fixed third one is a
// name no uuid takes; a single Stop path serves a picture that exists, one that does not yet,
// and one that will not.
//
// The release is the same token dance as before: it removes the entry only if it is still
// the one this call made, because a second draw of the same picture must keep its own entry
// — deleting it would leave that draw uncancellable while Stop reported nothing to stop.
// It also tells the window the draw is over, since there is no other event that means it: a
// progress row that outlives its draw is a Stop button for something that is not running.
//
// That last part is why the event is *conditional* on the draw having been reported on. A
// backend that cannot say anything while it works — or one whose only draw is a redraw the
// user is watching under the picture — leaves no row behind, and an event that fires for
// every silent draw is one the window has to handle for no reason at all.
func (a *App) registerDraw(id, chatID string, cancel context.CancelFunc) func() {
	return a.registerPicture(id, chatID, cancel, a.currentToolService())
}

// registerUpscale records an enlargement so that the same Stop finds it, and registers no
// service on purpose.
//
// Stop has two halves: cancelling the request, which is what ends the wait, and asking the
// backend to abandon the work behind it. An upscale has no backend to ask — the classical
// pass is local arithmetic and the neural one is a child process this app starts and kills
// through its own cancellation. Asking an image server to interrupt here would abandon
// whatever *it* is drawing, which is a different picture from the one the user stopped.
func (a *App) registerUpscale(id string, cancel context.CancelFunc) func() {
	return a.registerPicture(id, "", cancel, nil)
}

// registerPicture is the one token dance behind both registrations: it records a picture
// operation so Stop can find it, and returns the function that releases the registration.
func (a *App) registerPicture(id, chatID string, cancel context.CancelFunc, service *tools.Service) func() {
	if a.ctx == nil || id == "" {
		return func() {}
	}
	a.drawsMu.Lock()
	if a.draws == nil {
		a.draws = map[string]inflightDraw{}
	}
	// A draw already registered under this id is cancelled first. A *concurrent* one can no
	// longer reach this point: every path takes the draw slot before it registers, so what is
	// left is a registration whose release never ran — and the newest recipe is still the one
	// a user is waiting for.
	if previous, ok := a.draws[id]; ok {
		previous.cancel()
	}
	a.drawToken++
	token := a.drawToken
	a.draws[id] = inflightDraw{token: token, cancel: cancel, service: service}
	a.drawsMu.Unlock()

	return func() {
		a.drawsMu.Lock()
		wasReported := false
		var stopping <-chan struct{}
		if draw, ok := a.draws[id]; ok && draw.token == token {
			wasReported = draw.reported
			stopping = draw.stopping
			delete(a.draws, id)
		}
		a.drawsMu.Unlock()
		// The backend interrupt is global; do not release the draw slot until it
		// finishes, or a late interrupt could stop the next picture.
		if stopping != nil {
			<-stopping
		}
		if !wasReported {
			return
		}
		a.emit(events.ImageProgress, drawProgressPayload(id, chatID, images.Progress{}, true))
	}
}

// GetAttachmentImage returns an attachment's payload as a data: URL the webview
// can render.
//
// The bytes are already stored base64-encoded — that is the shape an image
// attachment's document has, and what the assembly turns back into an image part
// — so this re-encodes nothing. The data: URL rather than a file:// path is
// deliberate: the webview loads from an embedded assets handler, and a path would
// be blocked by the same origin rule that stops any other local file from being
// read.
func (a *App) GetAttachmentImage(attachmentID string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return "", err
	}
	if attachment.DocumentID == "" {
		return "", uimsg.Errorf("image.noStoredImage", attachment.Filename)
	}
	mime := strings.ToLower(attachment.Mime)
	if attachment.Kind != string(ingest.KindImage) && !strings.HasPrefix(mime, "image/") {
		return "", uimsg.Errorf("image.notAnImage", attachment.Filename)
	}
	document, err := a.store.GetDocument(attachment.DocumentID)
	if err != nil {
		return "", err
	}
	payload := strings.TrimSpace(document.Text)
	if payload == "" {
		return "", uimsg.Errorf("image.empty", attachment.Filename)
	}
	if attachment.Mime == "" {
		// The renderer needs the type to paint anything at all.
		mime = "image/png"
	} else {
		mime = attachment.Mime
	}
	return "data:" + mime + ";base64," + payload, nil
}

// SaveAttachmentAs writes an attachment to a file the user picks, and returns the
// path it was written to — empty when the user cancelled.
//
// This is the only place a generated picture leaves the app's own storage, and it
// goes through the native save dialog rather than a folder setting so the choice
// is made per picture. The folder it was saved into is remembered as the next
// dialog's starting point, because re-navigating to the same place every time is
// what makes an export button annoying enough not to use.
func (a *App) SaveAttachmentAs(attachmentID string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	if a.ctx == nil {
		return "", uimsg.Errorf("app.notReady")
	}
	attachment, err := a.store.GetAttachment(attachmentID)
	if err != nil {
		return "", err
	}
	document, err := a.store.GetDocument(attachment.DocumentID)
	if err != nil {
		return "", err
	}
	// Only an image is stored as base64; a text attachment's document holds its
	// extracted prose, and writing that out as if it were bytes would produce a
	// file of literal characters rather than the original document.
	if attachment.Kind != string(ingest.KindImage) {
		return "", uimsg.Errorf("image.notStored", attachment.Filename)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(document.Text))
	if err != nil {
		return "", uimsg.Errorf("image.decodeFailed", err)
	}

	filename := attachment.Filename
	if strings.TrimSpace(filename) == "" {
		filename = "bild.png"
	}
	// The filter follows the stored picture's own format, because that format is not
	// always PNG: a WebUI can be configured to answer with JPEG, and offering only
	// `*.png` for a JPEG makes the file look like it cannot be saved here.
	pattern, filterName := "*.png", "PNG-Bild"
	switch strings.ToLower(attachment.Mime) {
	case "image/jpeg":
		pattern, filterName = "*.jpg", "JPEG-Bild"
	case "image/webp":
		pattern, filterName = "*.webp", "WebP-Bild"
	case "image/gif":
		pattern, filterName = "*.gif", "GIF-Bild"
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:            "Bild speichern",
		DefaultFilename:  filename,
		DefaultDirectory: a.Settings().Images.SaveFolder,
		Filters: []runtime.FileFilter{
			{DisplayName: filterName, Pattern: pattern},
			{DisplayName: "Alle Dateien", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(path) == "" {
		return "", nil // The user closed the dialog; not an error.
	}
	// 0600, like the database: this is a copy of a picture the user drew or attached, and
	// a world-readable one would be the easiest way out of a private store.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", uimsg.Errorf("image.saveFailed", err)
	}
	// Recorded, because it is the whole difference between a picture its owner has a
	// file of and one that lives only in this database — and that is the question the
	// pictures panel asks (§10). A failure here is not a failed save: the file is on
	// disk and the note is a reading, so it is a warning rather than an error.
	if err := a.store.SetAttachmentSavedPath(attachment.ID, path); err != nil {
		a.emit(events.Warning, map[string]any{
			"warning": "Der Speicherort konnte nicht vermerkt werden: " + err.Error(),
		})
	}

	if dir := filepath.Dir(path); dir != "" && dir != a.Settings().Images.SaveFolder {
		a.settingsMu.Lock()
		a.settings.Images.SaveFolder = dir
		snapshot := a.settings.Clone()
		a.settingsMu.Unlock()
		if err := a.sanitize(snapshot).Save(a.store); err != nil {
			// Remembering the folder is a convenience, and the picture is already on
			// disk: failing to write it must not read as a failed save.
			a.emit(events.Warning, map[string]any{
				"warning": "Der Zielordner konnte nicht gemerkt werden: " + err.Error(),
			})
		}
	}
	return path, nil
}

// PickSaveFolder opens a folder dialog, for the default export location.
func (a *App) PickSaveFolder() (string, error) {
	if a.ctx == nil {
		return "", uimsg.Errorf("app.notReady")
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Ordner für gespeicherte Bilder",
		DefaultDirectory: a.Settings().Images.SaveFolder,
	})
}

// ImageCache is the app's own picture storage as the settings panel reads it.
//
// It answers the two questions a user asks in one breath — where did those pictures go,
// and what may I throw away — from a single walk of the attachment table, so the numbers
// cannot disagree with each other.
//
// **Only pictures KiChat drew count.** A photograph the user dropped into the composer is
// an image attachment too, and it is emphatically not a cache entry: the file it came from
// still exists, this app simply was never told where, and deleting it would take a picture
// out of a conversation to free nothing. The recipe is what tells the two apart — a drawn
// picture records how it was drawn, an attached one has nothing to record.
//
// Unsaved means no export was ever recorded for it, which is not quite the same as "there is
// no copy": a picture saved before that note existed reads exactly like one nobody ever
// saved. The panel says so rather than pretending otherwise, and nothing here deletes
// anything without the user pressing the button that does.
//
// No field is a nullable slice, but `UnsavedChats` is the count that makes the button's
// warning honest: it is how many conversations lose a picture.
type ImageCache struct {
	// Count and Bytes are every picture KiChat drew that its store still holds.
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
	// UnsavedCount and UnsavedBytes are the part of that no export records. They are the
	// only thing the clear button touches, and zero when every picture was saved.
	UnsavedCount int   `json:"unsavedCount"`
	UnsavedBytes int64 `json:"unsavedBytes"`
	// UnsavedChats is how many chats hold at least one of those pictures.
	UnsavedChats int `json:"unsavedChats"`
	// Folder is where the save dialog starts — the place a user already chose for their own
	// pictures, so the panel can point at it while explaining what "saved" means. It is the
	// stored setting, not a live reading: an empty one means the dialog has no default yet.
	Folder string `json:"folder"`
}

// encodedSize is how many bytes a picture's payload occupies in KiChat's own store.
//
// The document holds the payload base64-encoded, so the row is about a third larger
// than the picture itself. Reporting the decoded size would under-tell the space a
// clear actually frees, and "why is my KiChat data directory gigabytes?" is the one
// question this panel exists to answer — so it counts what is on disk, not what the
// file would be once decoded.
func encodedSize(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 2) / 3 * 4
}

// drawnPictures returns every picture KiChat drew, newest last, in the order the store
// holds them.
//
// It is the one place the store's rows are turned into "a picture this app drew", so the
// status and the clear cannot disagree about what a candidate is: a recipe is present (so
// it is drawn, not attached) and the row is an image attachment.
func (a *App) drawnPictures() ([]store.Attachment, error) {
	all, err := a.store.AllAttachments()
	if err != nil {
		return nil, err
	}
	out := []store.Attachment{}
	for _, attachment := range all {
		if attachment.Generation == nil {
			continue
		}
		if attachment.Kind != string(ingest.KindImage) && !strings.HasPrefix(strings.ToLower(attachment.Mime), "image/") {
			continue
		}
		out = append(out, attachment)
	}
	return out, nil
}

// GetImageCache reports how much of KiChat's own storage is pictures it drew, and how much
// of that only KiChat holds.
func (a *App) GetImageCache() (ImageCache, error) {
	if err := a.ensure(); err != nil {
		return ImageCache{}, err
	}
	cache := ImageCache{Folder: a.Settings().Images.SaveFolder}
	pictures, err := a.drawnPictures()
	if err != nil {
		return ImageCache{}, err
	}
	chats := map[string]bool{}
	for _, picture := range pictures {
		cache.Count++
		cache.Bytes += encodedSize(picture.SizeBytes)
		if picture.SavedPath != "" {
			continue
		}
		cache.UnsavedCount++
		cache.UnsavedBytes += encodedSize(picture.SizeBytes)
		chats[picture.ChatID] = true
	}
	cache.UnsavedChats = len(chats)
	return cache, nil
}

// ImageCacheClear is what clearing the picture cache achieved.
//
// Two numbers rather than a bare count, because they are what the confirmation promised:
// the panel says "14 Bilder, 12 MB" before the click and has to be able to say what really
// went. A picture that could not be deleted is not counted and not silently ignored — the
// error travels, and whatever was removed before it stays removed, because each deletion
// is its own transaction.
type ImageCacheClear struct {
	Removed int   `json:"removed"`
	Bytes   int64 `json:"bytes"`
}

// ClearImageCache deletes every picture KiChat drew that has no recorded export.
//
// This is the one destructive thing in the image panel, and it is deliberately narrow: it
// touches neither an attached file (§ the drawnPictures rule) nor a picture its owner has a
// copy of, and it is never automatic. What it buys is the answer to "why is my KiChat data
// directory gigabytes?" — a session that draws pictures leaves every one of them in the
// store, and a user who never saves one has no other way to say they are finished with it.
//
// Each chat that lost something is told once, at the end, rather than per picture: the event
// makes the view re-read that chat, and one re-read per chat is what it costs either way.
func (a *App) ClearImageCache() (ImageCacheClear, error) {
	if err := a.ensure(); err != nil {
		return ImageCacheClear{}, err
	}
	pictures, err := a.drawnPictures()
	if err != nil {
		return ImageCacheClear{}, err
	}
	result := ImageCacheClear{}
	affected := map[string]bool{}
	for _, picture := range pictures {
		if picture.SavedPath != "" {
			continue
		}
		if err := a.store.DeleteAttachment(picture.ID); err != nil {
			return result, uimsg.Errorf("image.removeFailed", picture.Filename, err)
		}
		result.Removed++
		result.Bytes += encodedSize(picture.SizeBytes)
		if picture.ChatID != "" {
			affected[picture.ChatID] = true
		}
	}
	for chatID := range affected {
		a.emit(events.Attachments, map[string]any{"chatId": chatID})
	}
	if a.ctx != nil && result.Removed > 0 {
		runtime.LogInfof(a.ctx, "%d nicht gespeicherte Bilder entfernt", result.Removed)
	}
	return result, nil
}
