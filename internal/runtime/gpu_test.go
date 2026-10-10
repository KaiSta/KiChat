package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two texts below are what a machine answers about its graphics stack. The
// driver line is the shape of /proc/driver/nvidia/version; the index is `ldconfig
// -p`, which is the loader's own answer to "is this library installed".
const (
	nvidiaProcText = "NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.95.05  Sun Sep 28 22:30:53 UTC 2025\n" +
		"GCC version:  gcc version 13.3.0 (Ubuntu 13.3.0-6ubuntu2~24.04)\n"
	loaderIndexText = "\tlibamdhip64.so.5 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libamdhip64.so.5\n" +
		"\tlibamdhip64.so (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libamdhip64.so\n" +
		"\tlibcudart.so.12 (libc6,x86-64) => /usr/local/cuda-12.8/lib64/libcudart.so.12\n" +
		"\tlibcublas.so.12 (libc6,x86-64) => /usr/local/cuda-12.8/lib64/libcublas.so.12\n" +
		"\tlibvulkan.so.1 (libc6,x86-64) => /lib/x86_64-linux-gnu/libvulkan.so.1\n"
)

func TestNvidiaDriverFromProc(t *testing.T) {
	major, ok := nvidiaDriverFromProc(nvidiaProcText)
	if !ok || major != 580 {
		t.Errorf("driver = %d, %v; want 580 from the kernel module line", major, ok)
	}
	// A machine with the NVIDIA module not loaded has no such file, and its
	// contents are then empty rather than a zero driver version.
	if _, ok := nvidiaDriverFromProc(""); ok {
		t.Error("an empty file was read as a driver version")
	}
	if _, ok := nvidiaDriverFromProc("Linux version 6.8.0-generic\n"); ok {
		t.Error("a kernel version was read as a driver version")
	}
}

func TestMajorOfReadsOnlyVersionShapedSegments(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  int
		valid bool
	}{
		{"13.4", 13, true},
		{"10.0", 10, true},
		{"580.95.05", 580, true},
		{"5", 5, true},
		{"fp16", 0, false},
		{"", 0, false},
		{"-1", 0, false},
	} {
		got, ok := majorOf(tc.in)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Errorf("majorOf(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

// TestCUDAUsableJudgesTheDriver is the check that keeps a CUDA install from
// downloading 153 MB of a backend this machine's driver cannot initialise: NVIDIA's
// own tables put CUDA 12.x at driver 525 and CUDA 13.x at 580.
func TestCUDAUsableJudgesTheDriver(t *testing.T) {
	cases := []struct {
		name    string
		suite   string
		driver  string
		cache   string
		usable  bool
		wantsIn string
	}{
		{
			name: "a 580 driver runs either suite", suite: "13.4", driver: nvidiaProcText,
			cache: loaderIndexText, usable: true,
		},
		{
			name: "a 570 driver is too old for CUDA 13", suite: "13.4",
			driver: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.86.16\n",
			cache:  loaderIndexText, usable: false, wantsIn: "570",
		},
		{
			name: "the same 570 driver still runs CUDA 12.8", suite: "12.8",
			driver: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.86.16\n",
			cache:  loaderIndexText, usable: true,
		},
		{
			name: "a driver below the 12.x floor runs neither", suite: "12.8",
			driver: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  470.256.02\n",
			cache:  loaderIndexText, usable: false, wantsIn: "470",
		},
		{
			// No driver module loaded and no driver library: there is no CUDA on
			// this machine at all, which is worth saying before a download.
			name: "no driver at all", suite: "13.4", driver: "",
			cache:  "\tlibvulkan.so.1 (libc6,x86-64) => /lib/x86_64-linux-gnu/libvulkan.so.1\n",
			usable: false, wantsIn: "libcuda.so.1",
		},
		{
			// The module's proc file is unreadable but the driver library is
			// installed: the version is unknown, so nothing is refused.
			name: "driver library without a readable version", suite: "13.4", driver: "",
			cache:  "\tlibcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1\n",
			usable: true,
		},
		{
			// A CUDA major this table has never seen is not judged: refusing on a
			// guess would block a machine that works.
			name: "an unknown CUDA major", suite: "14.0", driver: "",
			cache: "", usable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usable, why := cudaUsableFor(tc.suite, tc.driver, tc.cache)
			if usable != tc.usable {
				t.Fatalf("usable = %v (%s), want %v", usable, why, tc.usable)
			}
			if tc.wantsIn != "" && !strings.Contains(why, tc.wantsIn) {
				t.Errorf("reason = %q, want it to name %q", why, tc.wantsIn)
			}
			if tc.usable && why != "" {
				t.Errorf("reason = %q for a usable build, want nothing", why)
			}
		})
	}
}

// TestHIPMajorForFollowsTheSoname: measured at both ends — ROCm 5.7 provides
// libamdhip64.so.5, the ROCm 10.0 build of b11538 asks for .so.7, and the soname has
// not moved again since ROCm 7.
func TestHIPMajorForFollowsTheSoname(t *testing.T) {
	cases := map[string]int{"5.7": 5, "6.4": 6, "7.0": 7, "10.0": 7}
	for suite, want := range cases {
		got, ok := hipMajorFor(suite)
		if !ok || got != want {
			t.Errorf("hipMajorFor(%q) = %d, %v; want %d", suite, got, ok, want)
		}
	}
	if _, ok := hipMajorFor("fp16"); ok {
		t.Error("a non-version suite was read as a HIP major")
	}
}

// TestROCmUsableNamesTheVersionMismatch is exactly the state this machine was in: a
// ROCm 10 build against a system with ROCm 5.7. The refusal has to name both
// versions, because "no device found" is what the user otherwise has to go on.
func TestROCmUsableNamesTheVersionMismatch(t *testing.T) {
	usable, why := rocmUsableFor("10.0", []int{5})
	if usable {
		t.Fatal("a ROCm 10 build was called usable on a ROCm 5 system")
	}
	for _, want := range []string{"libamdhip64.so.7", "libamdhip64.so.5", "ROCm 10.0"} {
		if !strings.Contains(why, want) {
			t.Errorf("reason = %q, want it to name %q", why, want)
		}
	}
	if usable, why := rocmUsableFor("10.0", []int{7}); !usable {
		t.Errorf("a ROCm 10 build on ROCm 10 was refused: %s", why)
	}
	// Nothing installed at all is its own sentence: there is no version to
	// compare, and the archive does not bring the runtime with it.
	usable, why = rocmUsableFor("10.0", nil)
	if usable || !strings.Contains(why, "libamdhip64.so") {
		t.Errorf("usable = %v, reason = %q; want a refusal naming the missing runtime", usable, why)
	}
	// A suite past ROCm 7 still asks for libamdhip64.so.7, which is where the
	// soname has stayed, so an 11.0 build on a ROCm 7 machine is allowed.
	if usable, why := rocmUsableFor("11.0", []int{7}); !usable || why != "" {
		t.Errorf("usable = %v, reason = %q; want a ROCm 11 build on ROCm 7 to pass", usable, why)
	}
	// A suite that is not a version at all is not judged: a guess about those would
	// block a machine that works.
	if usable, why := rocmUsableFor("fp16", nil); !usable || why != "" {
		t.Errorf("usable = %v, reason = %q; want an unjudged suite to pass", usable, why)
	}
}

// The real machine is asked as well, but only for agreement: what the machine has
// is a fact about the machine, not about this code, and a test that pinned one
// verdict would fail the day someone installs ROCm 10.
func TestROCmUsableAgreesWithTheInstalledRuntime(t *testing.T) {
	have := hipRuntimeMajors()
	usable, why := SuiteUsable(AcceleratorROCm, "10.0")
	wanted := false
	for _, major := range have {
		if major == 7 {
			wanted = true
		}
	}
	if usable != wanted {
		t.Errorf("SuiteUsable(rocm, 10.0) = %v (%s) while the machine has %v", usable, why, have)
	}
}

// TestFamiliesWithoutAJudgementPass: a family whose prerequisites this build does
// not interrogate must never be refused, or a working Vulkan install would be
// blocked by a check that has nothing to say about it.
func TestFamiliesWithoutAJudgementPass(t *testing.T) {
	for _, variant := range []string{AcceleratorCPU, AcceleratorVulkan, AcceleratorSYCL, AcceleratorOpenVINO, AcceleratorOpenCL} {
		if usable, why := SuiteUsable(variant, "9.9"); !usable {
			t.Errorf("%s was refused: %s", variant, why)
		}
	}
}

func TestLoaderLibraryMajorsReadsTheLoaderIndex(t *testing.T) {
	if got := loaderLibraryMajors(loaderIndexText, "libamdhip64.so"); len(got) != 1 || got[0] != 5 {
		t.Errorf("amdhip64 majors = %v, want just 5 — the unversioned soname is not a version", got)
	}
	if got := loaderLibraryMajors(loaderIndexText, "libcudart.so"); len(got) != 1 || got[0] != 12 {
		t.Errorf("cudart majors = %v, want just 12", got)
	}
	if got := loaderLibraryMajors(loaderIndexText, "libcublas.so"); len(got) != 1 || got[0] != 12 {
		t.Errorf("cublas majors = %v, want just 12", got)
	}
	if got := loaderLibraryMajors("", "libcudart.so"); got != nil {
		t.Errorf("majors = %v for an unreadable index, want none", got)
	}
}

// TestCUDARuntimePresentDecidesOnTheBundleSkip: the companion archive is 440 MB, so
// it is left alone when the machine's own CUDA runtime already answers for the
// sonames this build wants — and only then.
func TestCUDARuntimePresentDecidesOnTheBundleSkip(t *testing.T) {
	if !cudaRuntimePresentIn("12.8", loaderIndexText) {
		t.Error("a machine with libcudart.so.12 and libcublas.so.12 was not recognised")
	}
	if cudaRuntimePresentIn("13.4", loaderIndexText) {
		t.Error("CUDA 13 was called present on a machine that has CUDA 12")
	}
	if cudaRuntimePresentIn("12.8", "") {
		t.Error("an unreadable loader index was read as a machine that has CUDA")
	}
	onlyCudart := "\tlibcudart.so.13 (libc6,x86-64) => /usr/local/cuda/lib64/libcudart.so.13\n"
	if cudaRuntimePresentIn("13.4", onlyCudart) {
		t.Error("libcudart alone was read as a complete CUDA runtime")
	}
}

// TestSortROCMRootsPicksTheNewestNumericVersion: AMD's apt packages install into
// /opt/rocm-<version>/lib, so which tree is used decides which ROCm the app talks
// to. A string sort puts 7.10.0 below 7.2.0, which would quietly use an older
// runtime than the machine has.
func TestSortROCMRootsPicksTheNewestNumericVersion(t *testing.T) {
	roots := []string{"/opt/rocm-7.2.4", "/opt/rocm-7.10.0", "/opt/rocm-6.4.4", "/opt/rocm-7.2.10"}
	sortROCMRoots(roots)
	want := []string{"/opt/rocm-7.10.0", "/opt/rocm-7.2.10", "/opt/rocm-7.2.4", "/opt/rocm-6.4.4"}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("roots = %v, want %v", roots, want)
		}
	}
}

// TestServerEnvAddsTheROCMPathForAHipBuild is the fix for a correctly installed
// ROCm: the archive ships no runtime, AMD documents /opt/rocm/lib as not being on
// the loader path, and without it a HIP build finds no device and quietly runs on
// the CPU.
func TestServerEnvAddsTheROCMPathForAHipBuild(t *testing.T) {
	build := t.TempDir()
	writeLauncher(t, build)
	if err := os.WriteFile(filepath.Join(build, "libggml-hip.so"), []byte("hip"), 0o644); err != nil {
		t.Fatal(err)
	}
	rocm := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rocm, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROCM_PATH", rocm)
	t.Setenv("LD_LIBRARY_PATH", "/opt/other/lib")

	env := envValue(t, serverEnv(filepath.Join(build, "llama-server")), "LD_LIBRARY_PATH")
	parts := splitLibraryPath(env)
	want := []string{build, filepath.Join(rocm, "lib"), "/opt/other/lib"}
	if len(parts) != len(want) {
		t.Fatalf("LD_LIBRARY_PATH = %v, want %v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i, parts[i], want[i])
		}
	}
}

// A build without a HIP backend gets no ROCm path: the CPU and Vulkan builds do not
// need it, and adding it would suggest the library came from somewhere it did not.
func TestServerEnvLeavesANonHIPBuildAlone(t *testing.T) {
	build := t.TempDir()
	writeLauncher(t, build)
	if err := os.WriteFile(filepath.Join(build, "libggml-vulkan.so"), []byte("vk"), 0o644); err != nil {
		t.Fatal(err)
	}
	rocm := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rocm, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROCM_PATH", rocm)
	t.Setenv("LD_LIBRARY_PATH", "")

	parts := splitLibraryPath(envValue(t, serverEnv(filepath.Join(build, "llama-server")), "LD_LIBRARY_PATH"))
	if len(parts) != 1 || parts[0] != build {
		t.Errorf("LD_LIBRARY_PATH = %v, want only the build directory", parts)
	}
}

// writeLauncher puts a file where the launcher of a build would be, so serverEnv has
// a directory to talk about.
func writeLauncher(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, launcherName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// envValue reads one variable out of an environment slice.
func envValue(t *testing.T, env []string, name string) string {
	t.Helper()
	prefix := name + "="
	value := ""
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s appears %d times in the environment, want once", name, count)
	}
	return value
}
