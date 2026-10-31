package images

import (
	"fmt"
	"os"
	"path/filepath"

	"kichat/internal/uimsg"
)

// The one Real-ESRGAN build KiChat downloads.
//
// Upstream publishes no checksums for these archives: the v0.2.5.0 release answers
// `digest: null` for all three of them. So the sizes and digests below were measured
// here rather than copied from anywhere — each archive was fetched from that release on
// 2026-10-18 and hashed — and the sizes were then checked against the figures the
// release API reports, which is what says the files are the published ones. The digest
// is compared in full by the installer, so pinning it is what keeps a replaced or
// truncated download from ever being unpacked.
//
// Two traps are recorded in the shape of these rows:
//
//   - The archive says `20220424` and the release that carries it is tagged `v0.2.5.0`,
//     not `v0.2.0`. The v0.2.0 tag holds no such asset at all, and the similarly named
//     `RealESRGAN-ncnn-vulkan.zip` from v0.1.0 is a different, much older program.
//   - The zips contain no wrapper directory and carry mode 0 on every entry, because
//     they were written by Windows. Everything therefore lands at the top of the install
//     directory, and the executable bit has to be set afterwards — see
//     runtime.Installer.Install, which does that for the launcher an archive names.
const (
	// UpscalerVersion is the build this app installs: the date in the archive's own
	// name, which is also the directory the install lands in
	// (`<app data dir>/upscalers/realesrgan-ncnn-vulkan/20220424`).
	UpscalerVersion = "20220424"

	// UpscalerProgram is the executable inside every archive, without Windows' `.exe`.
	// It is a 2022 build of the RRDB network from 2020; there is no newer release that
	// ships weights, which is why the pinned archive is years old.
	UpscalerProgram = "realesrgan-ncnn-vulkan"

	// upscalerRelease is the release these archives come from. It is only used to build
	// the URLs, so that a row below carries the file name and not a URL assembled in
	// two places.
	upscalerRelease = "https://github.com/xinntao/Real-ESRGAN/releases/download/v0.2.5.0/"
)

// UpscalerBuild is one downloadable archive.
//
// It is deliberately not runtime.Asset, although the installer takes that: this package
// is a client of the pictures, not of the llama.cpp supervisor, and it does not import
// internal/runtime (see the note in live_sdserver_test.go). The app is what maps one to
// the other, which keeps the pinned facts next to the code that uses the program.
type UpscalerBuild struct {
	Version string
	Name    string
	URL     string
	Size    int64
	Digest  string // "sha256:<hex>"
	// Binary is the executable's name inside the archive, without the platform's
	// extension, so the installer knows which file to make runnable.
	Binary string
}

// upscalerBuilds is keyed by `goos/goarch`, because that is the pair the archive is
// published for and the pair Go reports. The three rows are not interchangeable: the
// Linux build is an ELF x86-64 binary, the Windows one is PE32+ x86-64 and needs
// `vcomp140.dll` from the same folder, and the macOS one is a universal binary with
// x86_64 and arm64 slices (measured with `file` on all three), which is why Apple
// silicon is served by the same archive and not by a second row.
var upscalerBuilds = map[string]UpscalerBuild{
	"linux/amd64": {
		Version: UpscalerVersion,
		Name:    "realesrgan-ncnn-vulkan-20220424-ubuntu.zip",
		URL:     upscalerRelease + "realesrgan-ncnn-vulkan-20220424-ubuntu.zip",
		Size:    46931474,
		Digest:  "sha256:e5aa6eb131234b87c0c51f82b89390f5e3e642b7b70f2b9bbe95b6a285a40c96",
		Binary:  UpscalerProgram,
	},
	"darwin/amd64": {
		Version: UpscalerVersion,
		Name:    "realesrgan-ncnn-vulkan-20220424-macos.zip",
		URL:     upscalerRelease + "realesrgan-ncnn-vulkan-20220424-macos.zip",
		Size:    51817124,
		Digest:  "sha256:e0ad05580abfeb25f8d8fb55aaf7bedf552c375b5b4d9bd3c8d59764d2cc333a",
		Binary:  UpscalerProgram,
	},
	"darwin/arm64": {
		Version: UpscalerVersion,
		Name:    "realesrgan-ncnn-vulkan-20220424-macos.zip",
		URL:     upscalerRelease + "realesrgan-ncnn-vulkan-20220424-macos.zip",
		Size:    51817124,
		Digest:  "sha256:e0ad05580abfeb25f8d8fb55aaf7bedf552c375b5b4d9bd3c8d59764d2cc333a",
		Binary:  UpscalerProgram,
	},
	"windows/amd64": {
		Version: UpscalerVersion,
		Name:    "realesrgan-ncnn-vulkan-20220424-windows.zip",
		URL:     upscalerRelease + "realesrgan-ncnn-vulkan-20220424-windows.zip",
		Size:    45474481,
		Digest:  "sha256:abc02804e17982a3be33675e4d471e91ea374e65b70167abc09e31acb412802d",
		Binary:  UpscalerProgram,
	},
}

// ErrUpscalerNoArchive marks a platform this app has nothing to download for. It is a
// separate error from ErrUpscalerUnavailable because the answer differs: there is no
// archive to point the user at, so the only setup that can work is a hand install.
var ErrUpscalerNoArchive = uimsg.Detailf("images.noUpscalerArchive",
	"there is no Real-ESRGAN archive for this platform; install the program by hand and choose its path")

// UpscalerBuildFor answers the archive for one platform, or why there is none.
//
// The four supported platforms are the ones upstream publishes: Linux and Windows on
// x86-64, macOS on both (one universal archive). An arm64 Linux or Windows machine has
// no build at all, and guessing the x86-64 one there would download 45 MB that cannot
// run.
func UpscalerBuildFor(goos, goarch string) (UpscalerBuild, error) {
	build, ok := upscalerBuilds[goos+"/"+goarch]
	if !ok {
		return UpscalerBuild{}, fmt.Errorf("%w (%s/%s)", ErrUpscalerNoArchive, goos, goarch)
	}
	return build, nil
}

// VerifyUpscalerInstall checks that an unpacked archive really holds a working program,
// and answers the path to it.
//
// This is the check that an install is worth recording: the archive is 45 to 52 MB and
// the download is verified, but what arrives is unpacked by code that cannot tell a
// program from a README, and the one thing worse than a failed install is a settings
// entry pointing at a folder that cannot do the work. The weights are checked by the
// same helper a run uses (ESRGAN.modelDir), so \"installed\" and \"able to enlarge\" agree
// by construction rather than by two lists of file names.
func VerifyUpscalerInstall(dir string, lang string) (string, error) {
	program := filepath.Join(dir, upscalerProgramName())
	info, err := os.Stat(program)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrUpscalerUnavailable, program)
	}
	// The published zips carry mode 0, so an executable bit that is missing here means
	// the unpacking forgot it rather than that the archive asked for it. Windows has no
	// such bit, and os.Stat reports 0666 there, so the question is only asked where it
	// means something.
	if os.PathSeparator != '\\' && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s", ErrUpscalerUnavailable, fmt.Sprintf(text(lang,
			"%s ist nicht ausführbar", "%s is not executable"), program))
	}
	models, err := (ESRGAN{Binary: program}).modelDir(program)
	if err != nil {
		return "", err
	}
	if models == "" {
		// modelDir answers "no folder of its own" without an error, because a run may
		// legitimately have its weights somewhere else. An install cannot: what the
		// archive was supposed to bring is exactly those files.
		return "", fmt.Errorf("%w: %s", ErrUpscalerNoModels, filepath.Join(dir, "models"))
	}
	return program, nil
}

// upscalerProgramName is the executable's file name on this platform.
func upscalerProgramName() string {
	if os.PathSeparator == '\\' {
		return UpscalerProgram + ".exe"
	}
	return UpscalerProgram
}
