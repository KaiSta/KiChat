package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The text below is the real `ldd` output of the installed ROCm build's
// libggml-hip.so on this machine — the case that prompted this probe. The archive
// is built for ROCm 10 (`libamdhip64.so.7`), the machine has ROCm 5.7
// (`libamdhip64.so.5`), so ggml's HIP backend never loads, `--list-devices` answers
// "(none)" with an empty stderr, and the model quietly runs on the CPU. Kept
// verbatim: trimming it to the interesting lines would let the parser drift from
// what the loader actually prints.
const rocmLdd = "\tlinux-vdso.so.1 (0x00007ffd2bd5e000)\n" +
	"\tlibggml-base.so.0 => /home/kai/.config/kichat/runtime/b11538-rocm-10.0/libggml-base.so.0 (0x00007f0a83e73000)\n" +
	"\tlibhipblas.so.3 => not found\n" +
	"\tlibrocblas.so.5 => not found\n" +
	"\tlibamdhip64.so.7 => not found\n" +
	"\tlibstdc++.so.6 => /lib/x86_64-linux-gnu/libstdc++.so.6 (0x00007f0a83c00000)\n" +
	"\tlibc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f0a83a00000)\n"

func TestMissingLibraryNamesReadsOnlyTheUnresolvedOnes(t *testing.T) {
	names := missingLibraryNames(rocmLdd)
	want := []string{"libhipblas.so.3", "librocblas.so.5", "libamdhip64.so.7"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("names[%d] = %q, want %q", i, names[i], w)
		}
	}
}

// A library whose dependencies are all there must produce nothing: this is the
// Vulkan build on a machine with libvulkan, and a false name here would tell a
// working install that it is broken.
func TestMissingLibraryNamesStaysQuietWhenEverythingResolves(t *testing.T) {
	out := "\tlibvulkan.so.1 => /lib/x86_64-linux-gnu/libvulkan.so.1 (0x00007f0a83a00000)\n" +
		"\tlibdl.so.2 => /lib/x86_64-linux-gnu/libdl.so.2 (0x00007f0a83900000)\n"
	if names := missingLibraryNames(out); len(names) != 0 {
		t.Errorf("names = %v, want none for output with no unresolved dependencies", names)
	}
	// The loader says this for a file it cannot use at all. It is not a missing
	// dependency and must not be reported as one.
	if names := missingLibraryNames("\tnot a dynamic executable\n"); len(names) != 0 {
		t.Errorf("names = %v, want none for a file that is not a dynamic executable", names)
	}
}

// Only the accelerator backends are named: the CPU variants and the base library
// are in every build and load everywhere, so listing them would put a scary line
// under every working install.
func TestBackendLibrariesPicksAcceleratorBackendsOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"libggml-base.so", "libggml-base.so.0", "libggml-cpu-zen4.so", "libggml-cpu-x64.so",
		"libggml-rpc.so", "libggml.so", "libggml.so.0", "libggml-hip.so", "libggml-vulkan.so",
		"llama-server", "libllama.so",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	libs := BackendLibraries(dir)
	if len(libs) != 2 {
		t.Fatalf("libraries = %v, want the two accelerator backends", libs)
	}
	if filepath.Base(libs[0]) != "libggml-hip.so" || filepath.Base(libs[1]) != "libggml-vulkan.so" {
		t.Errorf("libraries = %v, want them sorted by name", libs)
	}
}

// The end-to-end shape the app relies on: on this machine, against the real
// installed ROCm build, the probe names the three ROCm 10 libraries. It is skipped
// where that build is not present, because a test must not depend on a user's
// downloads.
func TestMissingLibrariesOnTheInstalledBuild(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir := filepath.Join(home, ".config", "kichat", "runtime", "b11538-rocm-10.0")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("the ROCm build of b11538 is not installed here")
	}
	missing := MissingLibraries(context.Background(), dir)
	if len(missing) == 0 {
		t.Skip("the ROCm runtime this build needs is installed, so there is nothing to name")
	}
	for _, want := range []string{"libamdhip64.so.7", "libhipblas.so.3", "librocblas.so.5"} {
		found := false
		for _, name := range missing {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing = %v, want %s among them", missing, want)
		}
	}
}

// A directory that is not there is no names, not a crash: the panel asks about the
// folder of a launcher it may have just been told about.
func TestMissingLibrariesOnAMissingFolder(t *testing.T) {
	if got := MissingLibraries(context.Background(), filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Errorf("missing = %v, want none for a folder that is not there", got)
	}
}
