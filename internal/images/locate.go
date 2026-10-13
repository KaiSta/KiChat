package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// This file is how the app is *given* stable-diffusion.cpp, and how it says which
// build of it the user has.
//
// KiChat does not download it. The project publishes its archives on GitHub
// (`leejet/stable-diffusion.cpp`) and every one of them carries both `sd-cli` and
// `sd-server`, so the one step a user performs is: download the archive for their
// machine and unpack it. What this app adds is not a downloader but the two things
// that step leaves undone — knowing *which* archive to take, and telling the app
// where it landed without making anybody type a path.
//
// The published names were read off the project's own releases (tag
// `master-961-60423ab`): `sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64.zip` and
// its `-vulkan` and `-rocm-10.1.0` siblings, `sd-master-60423ab-bin-win-cpu-x64.zip`
// with `-cuda12-`, `-vulkan-` and `-rocm-10.1.0-` beside it, and
// `sd-master-60423ab-bin-Darwin-macOS-<version>-arm64.zip`. **There is no Linux CUDA
// archive**, which is why the table below has no such row: offering a build the
// release does not publish would send a user to a 404 and read as this app being
// wrong about their machine.

// SDBuild is one build family the project publishes for one platform.
type SDBuild struct {
	// ID is the value the setting stores and the panel offers.
	ID string `json:"id"`
	// Accelerator is the backend the build carries: cpu, vulkan, cuda, rocm, metal.
	// It is what AcceleratorOf turns a folder's libraries into, so a stored ID and a
	// detected family are the same vocabulary.
	Accelerator string `json:"accelerator"`
	// Asset is the published archive's name with `{tag}` where the release's own
	// build token goes and `{suite}` where a version does. Neither can be written
	// down in advance — the tag carries the release number (`master-60423ab`) and
	// the ROCm and macOS names carry their own versions — so the panel shows this
	// as a pattern to recognise rather than as a filename to open.
	Asset string `json:"asset"`
	// NoteKey is the interface key of a sentence a user needs before choosing this
	// one, empty for the builds that need no explaining. It is a *key* rather than the
	// sentence, which is this codebase's rule for anything the backend hands the
	// interface: the string lives in the dictionaries, in every language, and
	// i18n_test.go fails if one is missing.
	NoteKey string `json:"noteKey,omitempty"`
}

// ReleasePage is where every one of these archives lives, and the address the panel
// sends a user to. It is the `latest` page rather than a pinned tag: the archives
// are republished per master build and pinned links rot.
const ReleasePage = "https://github.com/leejet/stable-diffusion.cpp/releases/latest"

// sdBuilds lists what the project publishes per platform and architecture. An
// architecture it does not build for answers empty rather than an error: "nothing is
// published for this machine" is a fact the panel has to be able to print.
func sdBuilds(goos, goarch string) []SDBuild {
	switch goos {
	case "linux":
		if goarch != "amd64" {
			return []SDBuild{}
		}
		return []SDBuild{
			{ID: "cpu", Accelerator: AcceleratorCPU, Asset: "sd-{tag}-bin-Linux-Ubuntu-24.04-x86_64.zip"},
			{ID: "vulkan", Accelerator: AcceleratorVulkan, Asset: "sd-{tag}-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip"},
			{ID: "rocm", Accelerator: AcceleratorROCm, Asset: "sd-{tag}-bin-Linux-Ubuntu-24.04-x86_64-rocm-{suite}.zip"},
		}
	case "windows":
		if goarch != "amd64" {
			return []SDBuild{}
		}
		return []SDBuild{
			{ID: "cpu", Accelerator: AcceleratorCPU, Asset: "sd-{tag}-bin-win-cpu-x64.zip"},
			{ID: "cuda", Accelerator: AcceleratorCUDA, Asset: "sd-{tag}-bin-win-cuda12-x64.zip", NoteKey: "settings.images.cliBuildNote.cuda"},
			{ID: "vulkan", Accelerator: AcceleratorVulkan, Asset: "sd-{tag}-bin-win-vulkan-x64.zip"},
			{ID: "rocm", Accelerator: AcceleratorROCm, Asset: "sd-{tag}-bin-win-rocm-{suite}.zip"},
		}
	case "darwin":
		if goarch != "arm64" {
			return []SDBuild{}
		}
		// One archive, no accelerator to choose: the macOS build is the Metal one and
		// the project publishes nothing else for it.
		return []SDBuild{
			{ID: "metal", Accelerator: AcceleratorMetal, Asset: "sd-{tag}-bin-Darwin-macOS-{suite}-arm64.zip"},
		}
	}
	return []SDBuild{}
}

// SDBuilds lists what this machine can take, in the order a picker offers them: the
// portable builds first, then the vendor-specific ones.
func SDBuilds(goos, goarch string) []SDBuild {
	out := sdBuilds(goos, goarch)
	if out == nil {
		return []SDBuild{}
	}
	return out
}

// SDBuildIDs is the value space of the stored build setting for one platform, which
// is also its normalisation: anything else becomes empty, meaning "not chosen".
func SDBuildIDs(goos, goarch string) []string {
	ids := make([]string, 0, 4)
	for _, b := range SDBuilds(goos, goarch) {
		ids = append(ids, b.ID)
	}
	return ids
}

// IsSDBuild reports whether a stored value names a build this platform publishes.
func IsSDBuild(goos, goarch, id string) bool {
	for _, have := range SDBuildIDs(goos, goarch) {
		if have == strings.TrimSpace(id) {
			return true
		}
	}
	return false
}

// The accelerator names this package uses. They are the same words the managed
// llama.cpp runtime uses for its own builds (internal/runtime), which is what lets
// the two panels read alike, and the same words AcceleratorOf answers with.
const (
	AcceleratorCPU    = "cpu"
	AcceleratorVulkan = "vulkan"
	AcceleratorCUDA   = "cuda"
	AcceleratorROCm   = "rocm"
	AcceleratorMetal  = "metal"
)

// backendMarker maps the backend library a build ships to the accelerator it stands
// for. The names are llama.cpp's own (`libggml-vulkan.so`, `ggml-cuda.dll`,
// `libggml-hip.so`), which stable-diffusion.cpp uses unchanged because it links the
// same ggml — so one table reads both.
//
// ROCm's backend library is `-hip`, not `-rocm`: that is the ggml backend's own name,
// and matching on "rocm" would find nothing on a machine running the ROCm build.
var backendMarker = []struct{ marker, accelerator string }{
	{"-vulkan", AcceleratorVulkan},
	{"-cuda", AcceleratorCUDA},
	{"-hip", AcceleratorROCm},
	{"-metal", AcceleratorMetal},
	{"-sycl", "sycl"},
	{"-opencl", "opencl"},
}

// AcceleratorOf reads which accelerator a build directory was compiled for, from the
// backend libraries it holds: a Vulkan build ships a `ggml-vulkan` library and needs
// no other proof, and two accelerators in one directory answer with the first in
// backendMarker's order rather than at random.
//
// Empty is the CPU build as much as it is "nothing found", and that is deliberate:
// the CPU build ships no backend library at all, and a directory this app cannot read
// must not be reported as a GPU build the user would then wait on. The panel treats
// empty as the plain CPU build, which is what a directory with no accelerator library
// in it is.
func AcceleratorOf(dir string) string {
	for _, entry := range libraryNames(dir) {
		lower := strings.ToLower(entry)
		if !strings.Contains(lower, "ggml") {
			continue
		}
		for _, marker := range backendMarker {
			if strings.Contains(lower, marker.marker) {
				return marker.accelerator
			}
		}
	}
	return ""
}

// libraryNames lists the shared libraries in a directory, by base name. A directory
// that is not there is no libraries rather than a failure: the caller is asking what
// a folder holds, and an empty folder is an answer.
func libraryNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".so"), strings.Contains(name, ".so."),
			strings.HasSuffix(name, ".dll"), strings.HasSuffix(name, ".dylib"):
			names = append(names, name)
		}
	}
	return names
}

// ResolveProgram answers which executable to run, given the folder and the explicit
// path a user may have given.
//
// The folder wins when it is set, and it does not fall back to the typed path when the
// lookup fails. That is the point of pointing at a folder: it is a statement about a
// *place*, and quietly running an older typed path instead would draw with a program
// the user had stopped thinking about. An empty folder is the typed path, which is how
// every installation that predates the folder field keeps working.
func ResolveProgram(folder, binary string) (string, error) {
	if strings.TrimSpace(folder) == "" {
		if strings.TrimSpace(binary) == "" {
			return "", ErrNotConfigured
		}
		return strings.TrimSpace(binary), nil
	}
	return FindProgram(folder)
}

// ProgramName is the executable's name on a platform. It is a function rather than a
// constant so the extension is read from the platform being asked about, which is
// what lets the lookups below be pinned for Windows from a Linux test run.
func ProgramName(goos string) string {
	if goos == "windows" {
		return "sd-cli.exe"
	}
	return "sd-cli"
}

// FindProgram locates the sd-cli executable in (or under) a directory the user picked.
//
// A release archive unpacks into a single root directory named after the build
// (`sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-vulkan/`), so a user's "point at
// the folder" can land in any of three places: inside that root, in its `bin`
// subdirectory, or one level above it — which is exactly the difference between
// unpacking "here" and unpacking into a folder named after `sd`.
//
// One level is as deep as it looks, on purpose. Recursing would find the program in a
// directory of old builds and silently pick one of them, and the user can always pick
// the inner folder; refusing with the paths it tried is a better answer than a guess
// that draws on a build nobody chose.
//
// "One of them" is the word that matters, and it applies one level down as much as two:
// a folder holding both a CPU build and a Vulkan one belongs to a user who downloaded
// both, and answering with whichever name sorts first used to run the CPU build on a
// machine with a GPU while the panel showed nothing to explain it. So more than one
// candidate is reported, not resolved — the one exception being a program *directly* in
// the picked folder (or in its `bin`), which is authoritative and ends the search.
func FindProgram(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("kein Programmordner angegeben")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s liegt nicht im Dateisystem", ErrUnavailable, dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s ist ein Ordner mit Dateien darin, keine Datei", ErrUnavailable, dir)
	}

	name := ProgramName(runtime.GOOS)

	// The folder itself is asked first and is authoritative: a program directly in it, or in
	// its `bin`, is the program the user meant, and nothing found below it can outvote that.
	tried := make([]string, 0, 8)
	if direct, closer := findProgramIn(dir, name); direct != "" {
		return direct, nil
	} else {
		tried = append(tried, closer...)
	}

	// Then one level down, which is where an archive that laid down a single root directory
	// put it. Every candidate is collected before any is used, because more than one is a
	// question rather than a preference.
	subs := []string{}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				subs = append(subs, entry.Name())
			}
		}
	}
	// Sorted because ReadDir promises an order and this must not depend on it: two candidate
	// roots in one folder would otherwise change from run to run — and the message below
	// names them.
	sort.Strings(subs)
	found := make([]string, 0, len(subs))
	for _, sub := range subs {
		candidate, closer := findProgramIn(filepath.Join(dir, sub), name)
		tried = append(tried, closer...)
		if candidate != "" {
			found = append(found, candidate)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: kein %s gefunden — gesucht wurde in %s",
			ErrUnavailable, name, strings.Join(tried, ", "))
	case 1:
		return absolute(found[0]), nil
	default:
		return "", fmt.Errorf("%w: mehrere %s gefunden: %s — bitte den Ordner auswählen, in dem das Programm direkt liegt",
			ErrUnavailable, name, strings.Join(found, ", "))
	}
}

// findProgramIn looks for the program directly in one directory and in its `bin`
// subdirectory, returning the first hit and every path it looked at, so a failed search can
// name them.
func findProgramIn(root, name string) (string, []string) {
	candidates := []string{filepath.Join(root, name), filepath.Join(root, "bin", name)}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return absolute(candidate), candidates
		}
	}
	return "", candidates
}

// absolute is a path the caller can run, and the raw one when the working directory cannot
// be resolved at all — a relative path is still worth trying, and failing the lookup over
// it would refuse a program that exists.
func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}
