package runtime

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// zipFile writes a zip whose entries are all mode 0, which is how the published
// archives this code also unpacks are written: the Real-ESRGAN zips come from Windows and
// carry no permission bits at all (measured on all three 20220424 archives), so the
// executable bit has to come from the install.
func zipFile(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
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
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return readFile(t, path)
}

// TestInstallOfAnArchiveWithItsOwnLauncher: the installer is not llama.cpp's alone. An
// archive that names another program — the upscaler's `realesrgan-ncnn-vulkan` beside a
// `models/` folder, which is the shape internal/images installs — has to be unpacked,
// its own launcher made executable, and its path returned, because that path is what the
// caller writes into the settings.
func TestInstallOfAnArchiveWithItsOwnLauncher(t *testing.T) {
	archive := zipFile(t, map[string]string{
		"realesrgan-ncnn-vulkan":         "#!/bin/sh\ntrue\n",
		"models/realesrgan-x4plus.bin":   "weights",
		"models/realesrgan-x4plus.param": "params",
		"README_ubuntu.md":               "read me",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	inst := &Installer{Dir: dir, Client: srv.Client()}
	sum := sha256.Sum256(archive)
	asset := Asset{
		Tag: "20220424", Name: "realesrgan-ncnn-vulkan-20220424-ubuntu.zip",
		URL: srv.URL + "/a.zip", Size: int64(len(archive)),
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Binary: "realesrgan-ncnn-vulkan",
	}
	binary, err := inst.Install(context.Background(), asset)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	buildDir := filepath.Join(dir, "20220424")
	if want := filepath.Join(buildDir, "realesrgan-ncnn-vulkan"); binary != want {
		t.Fatalf("binary = %q, want %q", binary, want)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("the returned path does not exist: %v", err)
	}
	// The archive carried mode 0 on every entry, so an install that did not chmod the
	// launcher leaves a program nobody can run — and one whose path the caller would
	// still happily record as installed.
	if os.PathSeparator != '\\' && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want an executable bit", info.Mode().Perm())
	}
	// Beside it, at the top of the build directory: the archive has no wrapper directory,
	// so an install that looked for one would lose the weights.
	if _, err := os.Stat(filepath.Join(buildDir, "models", "realesrgan-x4plus.bin")); err != nil {
		t.Errorf("the models folder did not arrive beside the launcher: %v", err)
	}
	// A fixture row that is not the launcher is left alone: it is part of the archive, and
	// deciding which of its files are useful is not the unpacker's business.
	if _, err := os.Stat(filepath.Join(buildDir, "README_ubuntu.md")); err != nil {
		t.Errorf("the archive's other files were dropped: %v", err)
	}
	// No llama.cpp name was invented for this archive.
	if _, err := os.Stat(filepath.Join(buildDir, launcherName())); err == nil {
		t.Errorf("a %s was created for an archive that does not carry one", launcherName())
	}
}
