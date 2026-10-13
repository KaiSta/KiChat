package images

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSDBuildsAreWhatTheReleaseReallyPublishes pins the table against the release it
// was read from, and the assertion that matters most is the *absence*: CUDA is
// published for Windows and not for Linux, so a Linux row offering it would send a
// user to an archive that does not exist. That is a claim about the vendor, which is
// why it is pinned here rather than left to the panel.
func TestSDBuildsAreWhatTheReleaseReallyPublishes(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         []string
		why          string
	}{
		{"linux", "amd64", []string{"cpu", "vulkan", "rocm"},
			"the Linux release ships a CPU, a Vulkan and a ROCm archive"},
		{"windows", "amd64", []string{"cpu", "cuda", "vulkan", "rocm"},
			"the Windows release ships a CUDA archive too"},
		{"darwin", "arm64", []string{"metal"},
			"macOS gets one arm64 build and no accelerator to choose"},
		{"linux", "arm64", nil,
			"nothing is published for this architecture, and that is a fact to report"},
		{"darwin", "amd64", nil,
			"the macOS archive is arm64 only"},
	}
	for _, c := range cases {
		got := SDBuildIDs(c.goos, c.goarch)
		if len(got) != len(c.want) {
			t.Errorf("%s/%s: builds = %v, want %v (%s)", c.goos, c.goarch, got, c.want, c.why)
			continue
		}
		for i, id := range c.want {
			if got[i] != id {
				t.Errorf("%s/%s: builds = %v, want %v (%s)", c.goos, c.goarch, got, c.want, c.why)
				break
			}
		}
	}

	// The one row that must not be invented for Linux.
	if IsSDBuild("linux", "amd64", AcceleratorCUDA) {
		t.Error("a CUDA build is offered for Linux, but the project publishes none there")
	}
	if !IsSDBuild("windows", "amd64", AcceleratorCUDA) {
		t.Error("the Windows CUDA build is not recognised")
	}
	// The value space is also the normalisation: anything else is "not chosen".
	if IsSDBuild("linux", "amd64", "opencl") || IsSDBuild("linux", "amd64", "") {
		t.Error("a value the platform does not publish was accepted")
	}

	// Every platform answers with a slice, never nil: this feeds a JSON array and the
	// panel iterates whatever it gets.
	for _, goos := range []string{"linux", "windows", "darwin", "plan9"} {
		if SDBuilds(goos, "arm64") == nil {
			t.Errorf("%s: SDBuilds answered nil; a nil slice marshals to JSON null", goos)
		}
	}
}

// TestEveryPublishedBuildNamesAnArchiveToLookFor is the panel's whole value: a user
// has to be able to recognise the file. An empty asset, or one without the release
// token in it, would be a row that tells them nothing.
func TestEveryPublishedBuildNamesAnArchiveToLookFor(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "arm64"}} {
		for _, build := range SDBuilds(platform[0], platform[1]) {
			if !strings.Contains(build.Asset, "{tag}") {
				t.Errorf("%s: %s names no release token to look for", build.ID, build.Asset)
			}
			if !strings.HasPrefix(build.Asset, "sd-") {
				t.Errorf("%s: %s is not an archive name of this project", build.ID, build.Asset)
			}
			if build.Accelerator == "" {
				t.Errorf("%s: no accelerator, so a detected folder could never be matched to it", build.ID)
			}
			// The build's own accelerator is what its archive is named for, except
			// for the two where the project's own word differs from ours.
			if build.ID == "rocm" && !strings.Contains(build.Asset, "rocm") {
				t.Errorf("the ROCm row names %s", build.Asset)
			}
		}
	}
}

// TestFindProgramLooksWhereAnArchiveUnpacksTo covers the three shapes a user's folder
// can be in, because getting this wrong is the whole feature failing: the archive
// unpacks into one root directory, and "unpack here" leaves that root inside the
// folder the user picked.
func TestFindProgramLooksWhereAnArchiveUnpacksTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is not an .exe")
	}
	name := ProgramName(runtime.GOOS)

	writeProgram := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("beside the folder", func(t *testing.T) {
		dir := t.TempDir()
		want := writeProgram(t, dir)
		got, err := FindProgram(dir)
		if err != nil {
			t.Fatalf("FindProgram: %v", err)
		}
		if got != want {
			t.Errorf("found %q, want %q", got, want)
		}
	})

	t.Run("in the archive's own root", func(t *testing.T) {
		dir := t.TempDir()
		want := writeProgram(t, filepath.Join(dir, "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-vulkan"))
		got, err := FindProgram(dir)
		if err != nil {
			t.Fatalf("FindProgram: %v", err)
		}
		if got != want {
			t.Errorf("found %q, want %q", got, want)
		}
	})

	t.Run("under bin", func(t *testing.T) {
		dir := t.TempDir()
		want := writeProgram(t, filepath.Join(dir, "sd-master-60423ab-bin-win-vulkan-x64", "bin"))
		got, err := FindProgram(dir)
		if err != nil {
			t.Fatalf("FindProgram: %v", err)
		}
		if got != want {
			t.Errorf("found %q, want %q", got, want)
		}
	})

	// Two builds beside each other is the case the depth limit alone does not answer.
	// A user who downloaded the CPU build and the Vulkan one kept both, and answering
	// with whichever name sorts first ran the CPU build on a machine with a GPU — with
	// nothing on screen to say so, since the CPU build is the one this app reports as
	// "nothing detected". The refusal names both, and the fix is one click.
	t.Run("two builds are reported rather than chosen", func(t *testing.T) {
		dir := t.TempDir()
		cpu := writeProgram(t, filepath.Join(dir, "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-cpu"))
		vulkan := writeProgram(t, filepath.Join(dir, "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-vulkan"))
		got, err := FindProgram(dir)
		if err == nil {
			t.Fatalf("one of two builds was chosen silently: %q", got)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v, want ErrUnavailable", err)
		}
		for _, want := range []string{cpu, vulkan} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %s", err, want)
			}
		}
	})

	// The one thing that does outvote a build in a subdirectory: a program the user
	// pointed straight at. That is the layout of somebody who unpacked the archive and
	// then picked the folder it made, and the choice cannot be read as an accident.
	t.Run("a program in the picked folder wins over one below it", func(t *testing.T) {
		dir := t.TempDir()
		want := writeProgram(t, dir)
		writeProgram(t, filepath.Join(dir, "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-vulkan"))
		got, err := FindProgram(dir)
		if err != nil {
			t.Fatalf("FindProgram: %v", err)
		}
		if got != want {
			t.Errorf("found %q, want the program in the picked folder %q", got, want)
		}
	})

	// The depth limit, pinned as a behaviour rather than left to the implementation: a
	// folder of old builds must not be guessed from. Adopting whichever program a
	// recursive walk reached first would draw on a build the user never chose, and the
	// fix — point at the inner folder — is one click.
	t.Run("two levels down is refused rather than guessed", func(t *testing.T) {
		dir := t.TempDir()
		writeProgram(t, filepath.Join(dir, "old", "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64"))
		if _, err := FindProgram(dir); err == nil {
			t.Error("a program two directories down was adopted")
		}
	})

	t.Run("nothing to find", func(t *testing.T) {
		dir := t.TempDir()
		// A folder that exists and holds something, just not a program: the error has
		// to name what was looked for and where, or a user cannot fix it.
		if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := FindProgram(dir)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), dir) {
			t.Errorf("err = %v, want it to name %s and the folder it searched", err, name)
		}
	})

	t.Run("a path that is not a folder", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "somefile")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := FindProgram(file); !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v, want ErrUnavailable for a file", err)
		}
		if _, err := FindProgram(""); !errors.Is(err, ErrUnavailable) && err == nil {
			t.Error("an empty folder was accepted")
		}
	})
}

// TestAcceleratorOfReadsTheBackendLibrary is how the panel can say which build a user
// actually downloaded rather than which one they said they would. It reads the
// libraries, because that is the only thing in the folder that answers the question —
// the archive's name is gone by the time it is unpacked.
func TestAcceleratorOfReadsTheBackendLibrary(t *testing.T) {
	cases := []struct {
		files []string
		want  string
		why   string
	}{
		{[]string{"libggml-vulkan.so", "libggml-base.so"}, AcceleratorVulkan,
			"a Vulkan build ships the vulkan backend"},
		{[]string{"libggml-hip.so"}, AcceleratorROCm,
			"ROCm's backend library is spelled -hip, never -rocm"},
		{[]string{"ggml-cuda.dll"}, AcceleratorCUDA, "the Windows CUDA backend"},
		{[]string{"libggml-metal.dylib"}, AcceleratorMetal, "the macOS build"},
		{[]string{"libggml-base.so", "libggml-cpu.so"}, "",
			"a CPU build ships no accelerator backend, and empty is what that is"},
		{[]string{"libsd.so", "libggml-base.so"}, "",
			"a library that merely exists is not a backend"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for _, name := range c.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := AcceleratorOf(dir); got != c.want {
			t.Errorf("AcceleratorOf(%v) = %q, want %q (%s)", c.files, got, c.want, c.why)
		}
	}

	// A directory that is not there is not a failure: an unset folder is the ordinary
	// state of an untouched installation, and it must read as "nothing found".
	if got := AcceleratorOf(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("a missing folder reported %q", got)
	}
}

// TestProgramNameIsThePlatformsOwn keeps the lookup honest on Windows, where the
// program is an .exe and a folder full of them would otherwise be searched for a name
// that cannot exist there.
func TestProgramNameIsThePlatformsOwn(t *testing.T) {
	if got := ProgramName("windows"); got != "sd-cli.exe" {
		t.Errorf("windows = %q, want sd-cli.exe", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := ProgramName(goos); got != "sd-cli" {
			t.Errorf("%s = %q, want sd-cli", goos, got)
		}
	}
}
