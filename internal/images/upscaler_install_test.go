package images

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ilang "kichat/internal/lang"
)

// TestThePinnedUpscalerArchivesAreComplete: the table is the whole of what a download
// knows — a URL that is not https, a digest that is not a sha256, a size of zero or a
// launcher name that is not the one inside the archive would each fail somewhere later,
// and the failures look different every time. The digest shape is checked here because
// the installer treats an unparsable one as "no digest reported" and would then accept a
// download nothing verified.
func TestThePinnedUpscalerArchivesAreComplete(t *testing.T) {
	platforms := []string{"linux/amd64", "darwin/amd64", "darwin/arm64", "windows/amd64"}
	seen := map[string]bool{}
	for _, platform := range platforms {
		parts := strings.SplitN(platform, "/", 2)
		build, err := UpscalerBuildFor(parts[0], parts[1])
		if err != nil {
			t.Errorf("UpscalerBuildFor(%s): %v", platform, err)
			continue
		}
		if !strings.HasPrefix(build.URL, "https://") {
			t.Errorf("%s: URL %q is not https", platform, build.URL)
		}
		if !strings.HasSuffix(build.URL, build.Name) {
			t.Errorf("%s: URL %q does not name the archive %q", platform, build.URL, build.Name)
		}
		if build.Size <= 0 {
			t.Errorf("%s: size %d, want the published byte count", platform, build.Size)
		}
		digest := strings.TrimPrefix(build.Digest, "sha256:")
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			t.Errorf("%s: digest %q is not a sha256", platform, build.Digest)
		}
		if build.Version != UpscalerVersion {
			t.Errorf("%s: version %q, want %q", platform, build.Version, UpscalerVersion)
		}
		if build.Binary != UpscalerProgram {
			t.Errorf("%s: binary %q, want %q", platform, build.Binary, UpscalerProgram)
		}
		seen[build.Name] = true
	}
	// Three archives for four platforms: macOS is one universal build (measured with
	// `file`: x86_64 and arm64 slices in the same executable), and checking that a second
	// macOS archive is not invented with a digest nobody has is the point of the count.
	if len(seen) != 3 {
		t.Errorf("distinct archives = %d, want 3", len(seen))
	}
}

// TestPlatformsWithoutAnArchiveAreRefused: upstream publishes x86-64 builds for Linux
// and Windows and a universal one for macOS, so an arm64 Linux or Windows machine has
// nothing to fetch. Falling back to the x86-64 archive there would download 45 MB of
// program that cannot run, and the failure — the kernel refusing to execute it — would
// look like a broken install rather than a wrong one.
func TestPlatformsWithoutAnArchiveAreRefused(t *testing.T) {
	for _, platform := range []string{"linux/arm64", "windows/arm64", "freebsd/amd64", "linux/386"} {
		parts := strings.SplitN(platform, "/", 2)
		build, err := UpscalerBuildFor(parts[0], parts[1])
		if !errors.Is(err, ErrUpscalerNoArchive) {
			t.Errorf("UpscalerBuildFor(%s) error = %v, want ErrUpscalerNoArchive", platform, err)
		}
		if build.URL != "" {
			t.Errorf("UpscalerBuildFor(%s) = %+v, want no archive", platform, build)
		}
		if err != nil && !strings.Contains(err.Error(), platform) {
			t.Errorf("the refusal does not name the platform: %v", err)
		}
	}
}

// writeInstall puts an unpacked archive on disk: a program, and optionally the weights.
func writeInstall(t *testing.T, withWeights bool, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, upscalerProgramName()), []byte("#!/bin/sh\ntrue\n"), mode); err != nil {
		t.Fatal(err)
	}
	if withWeights {
		models := filepath.Join(dir, "models")
		if err := os.MkdirAll(models, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, ext := range []string{".param", ".bin"} {
			if err := os.WriteFile(filepath.Join(models, ESRGANDefaultModel+ext), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// TestVerifyingAnUnpackedArchiveChecksTheProgramAndItsWeights: this is the check that
// decides whether a 46 MB download is written down as installed. Both halves matter: the
// v0.2.0-era archives carry the executable and no weights at all, and a run of one of
// those produces a black picture with exit code 0 rather than an error.
func TestVerifyingAnUnpackedArchiveChecksTheProgramAndItsWeights(t *testing.T) {
	t.Run("a complete archive", func(t *testing.T) {
		dir := writeInstall(t, true, 0o755)
		program, err := VerifyUpscalerInstall(dir, ilang.German)
		if err != nil {
			t.Fatalf("VerifyUpscalerInstall: %v", err)
		}
		if want := filepath.Join(dir, upscalerProgramName()); program != want {
			t.Errorf("program = %q, want %q", program, want)
		}
	})

	t.Run("without the weights", func(t *testing.T) {
		_, err := VerifyUpscalerInstall(writeInstall(t, false, 0o755), ilang.German)
		if !errors.Is(err, ErrUpscalerNoModels) {
			t.Errorf("error = %v, want ErrUpscalerNoModels", err)
		}
	})

	t.Run("without a program", func(t *testing.T) {
		_, err := VerifyUpscalerInstall(t.TempDir(), ilang.German)
		if !errors.Is(err, ErrUpscalerUnavailable) {
			t.Errorf("error = %v, want ErrUpscalerUnavailable", err)
		}
	})

	t.Run("with a program that was not made executable", func(t *testing.T) {
		// The published zips carry mode 0, so this is exactly what an install that forgot
		// the chmod leaves behind.
		_, err := VerifyUpscalerInstall(writeInstall(t, true, 0o644), ilang.German)
		if os.PathSeparator == '\\' {
			t.Skip("Windows has no executable bit")
		}
		if !errors.Is(err, ErrUpscalerUnavailable) {
			t.Errorf("error = %v, want ErrUpscalerUnavailable", err)
		}
	})

	t.Run("with a folder where the program should be", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, upscalerProgramName()), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyUpscalerInstall(dir, ilang.German); !errors.Is(err, ErrUpscalerUnavailable) {
			t.Errorf("error = %v, want ErrUpscalerUnavailable", err)
		}
	})
}
