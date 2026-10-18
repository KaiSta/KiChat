package main

import (
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	rt "kichat/internal/runtime"
)

// TestLiveUpscalerInstallFetchesThePinnedArchive drives the bound method the button calls,
// against the real archive: it is fetched from GitHub, compared to the pinned size and
// sha256, unpacked, made executable, checked for its weights, and written into the settings
// with the switch on.
//
// It is what keeps the pinned table honest. Upstream publishes no checksums for these
// archives — the release API answers `digest: null` — so the digests in
// internal/images/upscaler_install.go were computed from the files themselves, and only a
// fresh fetch can catch a typo in one of them or an asset that was replaced under its
// version. Cost: 45 to 52 MB and a few seconds, nothing else; the program is never run.
//
//	KICHAT_LIVE=1 go test ./ -run TestLiveUpscalerInstall -v
//
// Nothing is written into the app's own settings or its upscalers directory: the store, the
// install and the app object are all temporary, so a run cannot replace what this machine
// already has.
func TestLiveUpscalerInstallFetchesThePinnedArchive(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to download 45–52 MB from GitHub")
	}
	build, err := images.UpscalerBuildFor(goruntime.GOOS, goruntime.GOARCH)
	if err != nil {
		t.Skipf("no archive is published for this platform: %v", err)
	}

	// The app's own wiring, with the real table and a real client — only the directories and
	// the database are temporary.
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	a.ctx = t.Context()
	a.logfFn = func(string, ...any) {}
	a.emitFn = func(string, any) {}
	a.upscalerInstaller = rt.NewInstaller(t.TempDir())
	// The default client's timeout is 30 minutes, sized for a 590 MB CUDA build; this
	// download is 46 MB, and a bound that says so fails a dead network quickly.
	a.upscalerInstaller.Client = &http.Client{Timeout: 10 * time.Minute}
	a.upscalerBuild = images.UpscalerBuildFor

	status, err := a.GetUpscalerInstall()
	if err != nil {
		t.Fatalf("GetUpscalerInstall: %v", err)
	}
	if status.Size != build.Size || status.Version != build.Version {
		t.Fatalf("status = %+v, want the pinned %s of %d bytes", status, build.Version, build.Size)
	}
	if status.Unsupported != "" {
		t.Fatalf("this platform was reported as having no archive: %s", status.Unsupported)
	}

	if err := a.InstallUpscaler(); err != nil {
		t.Fatalf("InstallUpscaler: %v", err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		status, err = a.GetUpscalerInstall()
		if err != nil {
			t.Fatalf("GetUpscalerInstall: %v", err)
		}
		if !status.Busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the install never finished: %+v", status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	// Reaching here means the digest and the byte count both matched: the archive really is
	// the one the table pins, which is the claim this test exists for.
	if status.Error != "" {
		t.Fatalf("the install failed: %s", status.Error)
	}
	if status.Phase != "done" {
		t.Errorf("phase = %q, want \"done\"", status.Phase)
	}

	settings := a.Settings()
	if !settings.Images.UpscalerEnabled {
		t.Error("the switch is still off after installing")
	}
	program := settings.Images.ESRGANBinary
	if program == "" {
		t.Fatal("no program was recorded")
	}
	if want := filepath.Join(a.upscalerInstaller.Dir, build.Version, filepath.Base(program)); program != want {
		t.Errorf("esrganBinary = %q, want %q", program, want)
	}
	// The executable bit is not in the archive: every entry of these zips carries mode 0,
	// because they were written by Windows. So a run that reaches here has to have set it.
	info, err := os.Stat(program)
	if err != nil {
		t.Fatalf("the recorded program is not on disk: %v", err)
	}
	if os.PathSeparator != '\\' && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want an executable bit", info.Mode().Perm())
	}
	// And what is on disk is a usable install rather than a program that would write a black
	// picture and exit 0, which is what the digest alone cannot tell us.
	if _, err := images.VerifyUpscalerInstall(filepath.Dir(program)); err != nil {
		t.Fatalf("the unpacked archive is not usable: %v", err)
	}
	// The panel's own status agrees, which is the rebuild after the save.
	reading, err := a.GetUpscalerStatus()
	if err != nil {
		t.Fatalf("GetUpscalerStatus: %v", err)
	}
	if !reading.Available || reading.Path != program {
		t.Errorf("status = %+v, want the installed program reported as available", reading)
	}
	t.Logf("%s (%d bytes) installed to %s", build.Name, build.Size, filepath.Dir(program))
}
