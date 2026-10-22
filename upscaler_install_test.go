package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	rt "kichat/internal/runtime"
	"kichat/internal/uimsg"
)

// upscalerArchive is a stand-in for the published 46 MB zip: the program, optionally the
// weights, and nothing else. The entries carry mode 0, like the real ones, because that is
// what makes the executable bit the install's own job.
func upscalerArchive(t *testing.T, withWeights bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{"realesrgan-ncnn-vulkan": "#!/bin/sh\ntrue\n"}
	if withWeights {
		entries["models/realesrgan-x4plus.param"] = "params"
		entries["models/realesrgan-x4plus.bin"] = "weights"
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newUpscalerTestApp is an App whose upscaler installer and pinned table point at the
// fixture archive the server serves. Nothing here talks to GitHub: the download path, the
// digest check, the unpack, the executable bit and the settings write are the real ones,
// and only the bytes are a fixture.
func newUpscalerTestApp(t *testing.T, archive []byte, srv *httptestServer) *App {
	t.Helper()
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	a.ctx = context.Background()
	a.logfFn = func(string, ...any) {}
	a.emitFn = func(string, any) {}
	a.upscalerInstaller = rt.NewInstaller(t.TempDir())
	a.upscalerInstaller.Client = srv.srv.Client()
	sum := sha256.Sum256(archive)
	a.upscalerBuild = func(goos, goarch string) (images.UpscalerBuild, error) {
		return images.UpscalerBuild{
			Version: images.UpscalerVersion,
			Name:    "realesrgan-ncnn-vulkan-20220424-ubuntu.zip",
			URL:     srv.srv.URL + "/archive.zip",
			Size:    int64(len(archive)),
			Digest:  "sha256:" + hex.EncodeToString(sum[:]),
			Binary:  images.UpscalerProgram,
		}, nil
	}
	return a
}

// waitForInstall polls the bound method the panel polls, so the test waits the way the UI
// does rather than by sleeping a guessed amount.
func waitForInstall(t *testing.T, a *App) UpscalerInstall {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := a.GetUpscalerInstall()
		if err != nil {
			t.Fatalf("GetUpscalerInstall: %v", err)
		}
		if !status.Busy {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the install never finished: %+v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInstallUpscalerDownloadsVerifiesAndTurnsTheSwitchOn walks the whole button: the
// pinned archive is fetched and checked against its digest, unpacked with the executable
// bit set, verified to hold the weights, and recorded in the settings together with the
// switch. The last part is the point of the feature — an install that left the switch off
// would leave the user in front of the row it exists to replace.
func TestInstallUpscalerDownloadsVerifiesAndTurnsTheSwitchOn(t *testing.T) {
	archive := upscalerArchive(t, true)
	srv := newFixtureServer(t, "/archive.zip", archive, nil)
	a := newUpscalerTestApp(t, archive, srv)

	// The switch starts off, which is the state a user downloading it is in.
	if a.Settings().Images.UpscalerEnabled {
		t.Fatal("the switch was on before the install")
	}
	if err := a.InstallUpscaler(); err != nil {
		t.Fatalf("InstallUpscaler: %v", err)
	}
	status := waitForInstall(t, a)
	if status.Error != "" {
		t.Fatalf("install failed: %s", status.Error)
	}
	// The progress arrives with the archive's own size, which is what the bar needs before
	// the first byte is read.
	if status.Total != int64(len(archive)) {
		t.Errorf("progress total = %d, want the archive's %d bytes", status.Total, len(archive))
	}

	settings := a.Settings()
	if !settings.Images.UpscalerEnabled {
		t.Error("the switch is still off after installing")
	}
	want := filepath.Join(a.upscalerInstaller.Dir, images.UpscalerVersion, "realesrgan-ncnn-vulkan")
	if settings.Images.ESRGANBinary != want {
		t.Fatalf("esrganBinary = %q, want %q", settings.Images.ESRGANBinary, want)
	}
	info, err := os.Stat(settings.Images.ESRGANBinary)
	if err != nil {
		t.Fatalf("the recorded program is not on disk: %v", err)
	}
	if os.PathSeparator != '\\' && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want an executable bit", info.Mode().Perm())
	}
	// The weights came with it, and the panel's own status agrees without a restart.
	if _, err := os.Stat(filepath.Join(filepath.Dir(settings.Images.ESRGANBinary), "models", "realesrgan-x4plus.bin")); err != nil {
		t.Errorf("the weights did not arrive: %v", err)
	}
	statusOut, err := a.GetUpscalerStatus()
	if err != nil {
		t.Fatalf("GetUpscalerStatus: %v", err)
	}
	if !statusOut.Available {
		t.Errorf("status = %+v, want the freshly installed program reported as available", statusOut)
	}

	// And it survives a restart: what the next start reads is the store, not the running
	// app's copy.
	stored, err := config.Load(a.store, a.secrets)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !stored.Images.UpscalerEnabled || stored.Images.ESRGANBinary != want {
		t.Errorf("stored settings = %+v, want the install recorded", stored.Images)
	}
}

// TestInstallUpscalerRefusesAnArchiveWithoutWeights: an archive that holds the program and
// no model files is a download that would produce a black picture with exit code 0, so it
// must not be recorded as installed — and the switch must not be turned on for it either.
func TestInstallUpscalerRefusesAnArchiveWithoutWeights(t *testing.T) {
	archive := upscalerArchive(t, false)
	srv := newFixtureServer(t, "/archive.zip", archive, nil)
	a := newUpscalerTestApp(t, archive, srv)

	if err := a.InstallUpscaler(); err != nil {
		t.Fatalf("InstallUpscaler: %v", err)
	}
	status := waitForInstall(t, a)
	if !strings.Contains(status.Error, images.ErrUpscalerNoModels.Error()) {
		t.Fatalf("error = %q, want the missing weights reported", status.Error)
	}
	settings := a.Settings()
	if settings.Images.UpscalerEnabled || settings.Images.ESRGANBinary != "" {
		t.Errorf("settings = %+v, want a failed install to record nothing", settings.Images)
	}
}

// TestInstallUpscalerRefusesADownloadThatDoesNotMatchItsDigest: the pinned digest is the
// only thing between the settings and whatever the network served. This is the failure the
// table exists for.
func TestInstallUpscalerRefusesADownloadThatDoesNotMatchItsDigest(t *testing.T) {
	archive := upscalerArchive(t, true)
	// The same bytes in length, one of them different: the size check passes, so only the
	// digest can catch this, which is what makes it a test of the digest and not of the
	// truncation check beside it.
	replaced := bytes.Clone(archive)
	if i := bytes.IndexByte(replaced, 'p'); i >= 0 {
		replaced[i] = 'q'
	} else {
		t.Fatal("the fixture has no byte to change")
	}
	srv := newFixtureServer(t, "/archive.zip", replaced, nil)
	a := newUpscalerTestApp(t, archive, srv)

	if err := a.InstallUpscaler(); err != nil {
		t.Fatalf("InstallUpscaler: %v", err)
	}
	status := waitForInstall(t, a)
	if message, ok := uimsg.Parse(status.Error); !ok || message.Code != "runtime.checksumMismatch" || len(message.Args) != 2 {
		t.Fatalf("error = %q, want the checksum refusal naming expected and got", status.Error)
	}
	if a.Settings().Images.ESRGANBinary != "" {
		t.Error("a download that failed its check was recorded as installed")
	}
}

// TestInstallUpscalerReportsAPlatformWithNothingToFetch: the button cannot work
// everywhere, and it has to say so rather than fail as a network error later. The answer is
// part of the polled state, because the panel shows it before the button is pressed.
func TestInstallUpscalerReportsAPlatformWithNothingToFetch(t *testing.T) {
	a := newSettingsTestApp(t, config.Default(), t.TempDir())
	a.ctx = context.Background()
	a.logfFn = func(string, ...any) {}
	// A wired installer, so the refusal that follows is the platform's and not the app's own
	// "not ready" guard — the archive is looked up before anything is downloaded.
	a.upscalerInstaller = rt.NewInstaller(t.TempDir())
	a.upscalerBuild = func(goos, goarch string) (images.UpscalerBuild, error) {
		return images.UpscalerBuild{}, images.ErrUpscalerNoArchive
	}

	status, err := a.GetUpscalerInstall()
	if err != nil {
		t.Fatalf("GetUpscalerInstall: %v", err)
	}
	if want := goruntime.GOOS + "/" + goruntime.GOARCH; status.Unsupported != want {
		t.Fatalf("Unsupported = %q, want %q", status.Unsupported, want)
	}
	if status.Size != 0 || status.Version != "" {
		t.Errorf("status = %+v, want no archive described", status)
	}
	if err := a.InstallUpscaler(); !errors.Is(err, images.ErrUpscalerNoArchive) {
		t.Errorf("InstallUpscaler error = %v, want ErrUpscalerNoArchive", err)
	}
}

// TestASecondInstallIsRefusedWhileTheFirstRuns: 46 MB takes a while, and two goroutines
// unpacking into the same staging directory is how half an install ends up installed. The
// second press is refused rather than queued, which is what the disabled button shows.
func TestASecondInstallIsRefusedWhileTheFirstRuns(t *testing.T) {
	archive := upscalerArchive(t, true)
	release := make(chan struct{})
	srv := newFixtureServer(t, "/archive.zip", archive, release)
	a := newUpscalerTestApp(t, archive, srv)

	if err := a.InstallUpscaler(); err != nil {
		t.Fatalf("InstallUpscaler: %v", err)
	}
	// The server is holding the download open, so the first install is certainly running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := a.GetUpscalerInstall()
		if err != nil {
			t.Fatalf("GetUpscalerInstall: %v", err)
		}
		if status.Busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first install never started")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := a.InstallUpscaler(); err == nil {
		t.Error("a second install was accepted while the first was running")
	}
	close(release)
	if status := waitForInstall(t, a); status.Error != "" {
		t.Fatalf("the first install failed: %s", status.Error)
	}
}

// newFixtureServer serves one archive, optionally holding the response open until the
// caller releases it.
type httptestServer struct {
	srv *httptest.Server
}

func newFixtureServer(t *testing.T, path string, body []byte, release <-chan struct{}) *httptestServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if release != nil {
			<-release
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &httptestServer{srv: srv}
}
