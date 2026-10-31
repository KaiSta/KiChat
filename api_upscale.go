package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/config"
	"kichat/internal/events"
	"kichat/internal/images"
	rt "kichat/internal/runtime"
	"kichat/internal/store"
	"kichat/internal/tools"
	"kichat/internal/uimsg"
)

// UpscaleAttachment enlarges one picture and files the copy beside its source.
//
// It takes the same one-picture-at-a-time slot a draw does. A resample does not compete
// for the image server, but two encodes at 4K do compete for memory, and the window has
// one place to press Stop; one rule for every picture operation is simpler than a second
// slot with a different meaning.
func (a *App) UpscaleAttachment(attachmentID string, req images.UpscaleRequest) (store.Attachment, error) {
	if err := a.ensure(); err != nil {
		return store.Attachment{}, err
	}
	service := a.currentToolService()
	if service == nil || a.ctx == nil {
		return store.Attachment{}, uimsg.Errorf("app.notReady")
	}
	release, ok := a.tryDraw()
	if !ok {
		return store.Attachment{}, images.ErrDrawInProgress
	}
	defer release()

	ctx, cancel := context.WithTimeout(a.ctx, upscaleTimeout(req))
	defer cancel()
	// Registered so a Stop can end the wait. The classical pass is a second of local work
	// that nobody will press Stop for; the neural one loads a model and walks the picture in
	// tiles, which is minutes on a small card, and until this was registered the only exits
	// from it were the 15-minute bound and closing the app.
	unregister := a.registerUpscale(attachmentID, cancel)
	defer unregister()
	attachment, err := service.Upscale(ctx, attachmentID, req)
	if err != nil {
		// A stop is the parent context being cancelled while the app is still alive,
		// exactly as a stopped draw is reported. A deadline is not a stop and stays the
		// error it is.
		if errors.Is(ctx.Err(), context.Canceled) && a.ctx.Err() == nil {
			return store.Attachment{}, images.ErrCancelled
		}
		return store.Attachment{}, err
	}
	// The view holds the message's attachment list, so without this the new picture
	// exists and stays invisible until a reload.
	a.emit(events.Attachments, map[string]any{"chatId": attachment.ChatID})
	return attachment, nil
}

// upscaleTimeout is generous enough for the slow path and not a minute longer.
//
// The classical pass is seconds at 4K; the neural one loads a model and walks the
// picture in tiles, which on a small card can be minutes. The caller's Stop is still the
// control for a run that is merely slow.
func upscaleTimeout(req images.UpscaleRequest) time.Duration {
	if req.Method == images.MethodESRGAN {
		return 15 * time.Minute
	}
	return 3 * time.Minute
}

// GetUpscalerStatus reports whether the neural path is on and whether its program can
// be found. It does not start the program: that is TestUpscaler, which is a button.
func (a *App) GetUpscalerStatus() (tools.UpscalerStatus, error) {
	if err := a.ensure(); err != nil {
		return tools.UpscalerStatus{}, err
	}
	service := a.currentToolService()
	if service == nil {
		return tools.UpscalerStatus{}, uimsg.Errorf("app.notReady")
	}
	return service.UpscalerStatus(), nil
}

// TestUpscaler runs one tiny picture through Real-ESRGAN, which is the only check that
// covers the Vulkan driver as well as the executable.
func (a *App) TestUpscaler() (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	service := a.currentToolService()
	if service == nil || a.ctx == nil {
		return "", uimsg.Errorf("app.notReady")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	return service.TestUpscaler(ctx)
}

// UpscalerInstall is the state of the one download KiChat can do for the neural
// upscaler.
//
// It is read through a bound method rather than pushed as an event, for the reason the
// runtime download is: 45 to 52 MB outlives the call that asked for it, and a reading
// cannot be missed because a listener was not mounted yet. Unsupported and Size are part
// of it so the button can be explained before it is pressed — either there is no archive
// for this machine, or there is one of a known size.
//
// Every field carries a json tag, because this type crosses the bridge and Wails generates
// the window's type from those tags. An untagged field crosses as its Go name, which would
// make this the one status on the screen whose fields are `Busy` and `Phase`.
type UpscalerInstall struct {
	Busy  bool   `json:"busy"`
	Phase string `json:"phase"` // "downloading" | "extracting" | "verifying" | "done"
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Error string `json:"error"`
	// Unsupported is this machine's platform — `linux/arm64` — when there is no archive
	// for it, and empty when there is one. It is the fact rather than a sentence: the
	// sentence is the window's, because it is translated and this is not.
	Unsupported string `json:"unsupported"`
	// Size and Version describe the archive the button would fetch: how much traffic
	// it costs, and which build it is. Zero for a platform with nothing to fetch.
	Size    int64  `json:"size"`
	Version string `json:"version"`
}

// GetUpscalerInstall reports that state. It does not touch the network: the pinned
// archive for this platform is a table, and a download's progress is what the installer
// has already reported.
func (a *App) GetUpscalerInstall() (UpscalerInstall, error) {
	if err := a.ensure(); err != nil {
		return UpscalerInstall{}, err
	}
	status := UpscalerInstall{}
	build, err := a.upscalerAssetFor(goruntime.GOOS, goruntime.GOARCH)
	switch {
	case err == nil:
		status.Size, status.Version = build.Size, build.Version
	case errors.Is(err, images.ErrUpscalerNoArchive):
		status.Unsupported = goruntime.GOOS + "/" + goruntime.GOARCH
	default:
		status.Error = err.Error()
	}
	a.upscalerMu.Lock()
	status.Busy = a.upscalerBusy
	status.Phase = a.upscalerPhase
	status.Done, status.Total = a.upscalerDone, a.upscalerTotal
	status.Error = a.upscalerErr
	a.upscalerMu.Unlock()
	return status, nil
}

// InstallUpscaler fetches the pinned Real-ESRGAN archive, checks that what arrived is
// really a program with weights, and points the neural path at it.
//
// It returns as soon as the download is under way — the archive is 45 to 52 MB, and the
// bound method blocks the window that called it. GetUpscalerInstall reports the progress
// and the outcome.
//
// The switch is not read here. A user who needs the download is exactly a user whose
// switch is still off, so requiring it would hide the button behind the setting it
// exists to satisfy; the install turns it on when it is done.
func (a *App) InstallUpscaler() error {
	if err := a.ensure(); err != nil {
		return err
	}
	// Both guards are the same state — an app that was built without its own initialisation
	// — and they are checked here rather than in the goroutine, where a nil dereference
	// takes the whole process down instead of failing one download.
	if a.ctx == nil || a.upscalerInstaller == nil {
		return uimsg.Errorf("app.notReady")
	}
	build, err := a.upscalerAssetFor(goruntime.GOOS, goruntime.GOARCH)
	if err != nil {
		return err
	}
	a.upscalerMu.Lock()
	if a.upscalerBusy {
		a.upscalerMu.Unlock()
		return uimsg.Errorf("download.running")
	}
	a.upscalerBusy, a.upscalerPhase, a.upscalerErr = true, "", ""
	a.upscalerDone, a.upscalerTotal = 0, build.Size
	a.upscalerMu.Unlock()
	go a.installUpscaler(build)
	return nil
}

// upscalerAssetFor answers the pinned archive for one platform, or why there is none.
// The table is a field of the app so a test can serve a fixture, and a nil one means the
// app was built without its own initialisation — the same state the nil-context guard
// above covers, reported instead of dereferenced.
func (a *App) upscalerAssetFor(goos, goarch string) (images.UpscalerBuild, error) {
	if a.upscalerBuild == nil {
		return images.UpscalerBuild{}, uimsg.Errorf("app.notReady")
	}
	return a.upscalerBuild(goos, goarch)
}

// installUpscaler is the download itself, on its own goroutine.
func (a *App) installUpscaler(build images.UpscalerBuild) {
	asset := rt.Asset{
		Tag:    build.Version,
		Name:   build.Name,
		URL:    build.URL,
		Size:   build.Size,
		Digest: build.Digest,
		Binary: build.Binary,
	}
	binary, err := a.upscalerInstaller.Install(a.ctx, asset)
	if err == nil {
		// The unpacker cannot tell a program from a README, and a truncated or
		// replaced download is caught by the digest before it is unpacked but not after
		// it is: this is what says the folder really holds the executable and the
		// weights before any of it is written down as installed.
		a.setUpscalerPhase("verifying")
		binary, err = images.VerifyUpscalerInstall(filepath.Dir(binary),
			config.ModelLanguage(a.Settings().Language))
	}
	if err != nil {
		a.finishUpscaler(err)
		return
	}

	// The path and the switch are written together. Installing is what the switch was
	// waiting for — leaving it off would put the user back in front of the row this
	// button exists to replace, with the download already spent.
	a.settingsMu.Lock()
	a.settings.Images.ESRGANBinary = binary
	a.settings.Images.UpscalerEnabled = true
	// A clone, not the struct: the save marshals it, and the maps inside are written by
	// other goroutines under settingsMu.
	updated := a.settings.Clone()
	a.settingsMu.Unlock()
	if err := a.sanitize(updated).Save(a.store); err != nil {
		a.finishUpscaler(fmt.Errorf("Installation speichern: %w", err))
		return
	}
	// The tool service reads the configured program at construction, so without this
	// the Enlarge panel would keep reporting "not set up" until the next start.
	a.rebuildServices(updated)
	a.logInfo("Real-ESRGAN %s installiert: %s", build.Version, binary)
	a.finishUpscaler(nil)
}

// recordUpscalerProgress is the installer's progress callback.
func (a *App) recordUpscalerProgress(p rt.Progress) {
	a.upscalerMu.Lock()
	a.upscalerPhase = p.Phase
	a.upscalerDone, a.upscalerTotal = p.Done, p.Total
	a.upscalerMu.Unlock()
}

// setUpscalerPhase reports a step of the install the installer itself does not run.
func (a *App) setUpscalerPhase(phase string) {
	a.upscalerMu.Lock()
	a.upscalerPhase = phase
	a.upscalerMu.Unlock()
}

// finishUpscaler ends the download, successfully or not.
func (a *App) finishUpscaler(err error) {
	a.upscalerMu.Lock()
	a.upscalerBusy = false
	if err != nil {
		a.upscalerErr = err.Error()
		a.upscalerPhase = ""
	} else {
		a.upscalerErr = ""
		a.upscalerPhase = "done"
	}
	a.upscalerMu.Unlock()
}

// PickUpscalerBinary asks for the realesrgan-ncnn-vulkan executable.
//
// No filter: the program has no extension on Linux and `.exe` on Windows, and a filter
// that guesses wrong hides the very file the user was told to choose. The title comes
// from the UI so it follows the interface language.
func (a *App) PickUpscalerBinary(title string) (string, error) {
	if err := a.ensure(); err != nil {
		return "", err
	}
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}
