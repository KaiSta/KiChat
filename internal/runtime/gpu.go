package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The GPU side of a build is not in its archive, and that is where the two
// families KiChat can install differ — measured on b11538 rather than assumed:
//
//   - CUDA. The archive is 153 MB and holds no CUDA library at all: 62 entries,
//     the only CUDA-named one being its own libggml-cuda.so, whose own ldd then
//     asks for `libcudart.so.13`, `libcublas.so.13` and the host's `libcuda.so.1`.
//     The runtime ships as a *separate* asset (`cudart-llama-…`, 440 MB for 13.4)
//     that has to be unpacked beside the build, because the build looks for its
//     libraries through rpath `$ORIGIN`. A user who has the driver but not a CUDA
//     toolkit — the normal case, and what "install a GPU build" invites — otherwise
//     gets a backend that cannot load and a model that quietly runs on the CPU.
//   - ROCm. The archive is 245 MB, ships no runtime, and has no companion asset:
//     the system's ROCm is used, from a directory AMD documents as *not* being on
//     the loader path by default. Without adding it the process finds no device,
//     which is what serverEnv is for.
//
// Versions have to line up too. A CUDA 13 build needs a 580+ driver; a ROCm 10
// build needs libamdhip64.so.7 while Ubuntu 24.04 provides .5 (ROCm 5.7). Both
// mismatches are silent: llama.cpp reports no device and no error, and the model
// runs on the CPU. The checks below refuse such an install *before* the download
// instead, and name what does not match.

// cudaMinDriverMajor is the oldest NVIDIA driver branch each CUDA major version
// runs on, from NVIDIA's own compatibility tables: CUDA 11.x goes back to 450,
// 12.x to 525, and 13.x documents 580. A build of a major whose minimum this
// machine's driver does not meet cannot initialise at all, whatever the minor
// version is.
var cudaMinDriverMajor = map[int]int{11: 450, 12: 525, 13: 580}

// hipMajorFor reads which libamdhip64.so major version a ROCm suite needs.
//
// Measured at both ends: ROCm 5.7 on Ubuntu 24.04 provides libamdhip64.so.5, and
// the ROCm 10.0 build of b11538 asks for libamdhip64.so.7. The soname followed the
// ROCm major up to 6 and went to 7 with ROCm 7, where it has stayed since — which
// is why the rule is "the major, capped at 7" rather than a table that would have
// to be filled in from releases nobody has measured. A suite that does not parse is
// not judged at all.
func hipMajorFor(suite string) (int, bool) {
	major, ok := majorOf(suite)
	if !ok {
		return 0, false
	}
	if major < 1 {
		return 0, false
	}
	if major > 7 {
		return 7, true
	}
	return major, true
}

// SuiteUsable reports whether a build of one family and suite can initialise a GPU
// on this machine, and why not when it cannot.
//
// True with an empty reason is both "this works" and "this is not judged": a family
// whose prerequisites are not measured here is left alone, because refusing an
// install on a guess is worse than letting the user try and reading the panel.
func SuiteUsable(variant, suite string) (bool, string) {
	// Every probe behind this is Linux: the driver's version comes from a proc
	// file and the runtimes from the loader's cache. On another platform nothing
	// would be found, which would read as "broken" rather than as "not looked at" —
	// and refusing a Windows install because a Linux probe sees nothing is the one
	// failure mode a pre-flight check must not have.
	if runtime.GOOS != "linux" {
		return true, ""
	}
	switch variant {
	case AcceleratorCUDA:
		return cudaUsableFor(suite, readProcFile("/proc/driver/nvidia/version"), loaderCache())
	case AcceleratorROCm:
		return rocmUsableFor(suite, hipRuntimeMajors())
	}
	return true, ""
}

// cudaUsableFor judges one CUDA suite against a driver version and a loader index.
// Both are passed in so the judgement can be pinned without the hardware: they are
// what the machine would answer.
func cudaUsableFor(suite, driverText, cache string) (bool, string) {
	major, ok := majorOf(suite)
	if !ok {
		return true, ""
	}
	least, known := cudaMinDriverMajor[major]
	if !known {
		return true, ""
	}
	driver, have := nvidiaDriverFromProc(driverText)
	if !have {
		if !pointsAtCUDADriver(cache) {
			return false, "es ist kein NVIDIA-Treiber installiert (libcuda.so.1 fehlt) — ohne ihn kann der CUDA-Backend nicht laden"
		}
		// A driver is there but its version could not be read: no verdict.
		return true, ""
	}
	if driver < least {
		return false, fmt.Sprintf("der NVIDIA-Treiber ist %d, CUDA %d braucht mindestens %d", driver, major, least)
	}
	return true, ""
}

// rocmUsableFor judges one ROCm suite against the HIP runtimes a machine has.
func rocmUsableFor(suite string, have []int) (bool, string) {
	want, ok := hipMajorFor(suite)
	if !ok {
		return true, ""
	}
	if len(have) == 0 {
		return false, "es ist keine ROCm-Laufzeit installiert (libamdhip64.so.* fehlt; AMD liefert sie nicht im Archiv mit)"
	}
	for _, major := range have {
		if major == want {
			return true, ""
		}
	}
	return false, fmt.Sprintf("ROCm %s braucht libamdhip64.so.%d, vorhanden ist %s", suite, want, describeHIP(have))
}

// describeHIP names the HIP runtime versions found the way a user would recognise
// them: the soname major is the ROCm version's (libamdhip64.so.5 is ROCm 5.x).
func describeHIP(majors []int) string {
	parts := make([]string, 0, len(majors))
	for _, major := range majors {
		parts = append(parts, fmt.Sprintf("libamdhip64.so.%d (ROCm %d.x)", major, major))
	}
	return strings.Join(parts, ", ")
}

// readProcFile reads a file the kernel provides, answering "" when it is not there.
// A missing /proc/driver/nvidia/version is the ordinary state of a machine without
// the NVIDIA module loaded.
func readProcFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// nvidiaDriverFromProc reads the driver version out of
// /proc/driver/nvidia/version, whose own line reads
//
//	NVRM version: NVIDIA UNIX x86_64 Kernel Module  580.95.05  Sun Sep 28 2025
//
// The first version-shaped word after "Kernel Module" is the driver.
func nvidiaDriverFromProc(text string) (int, bool) {
	for _, line := range strings.Split(text, "\n") {
		cut := strings.Index(line, "Kernel Module")
		if cut < 0 {
			continue
		}
		for _, field := range strings.Fields(line[cut+len("Kernel Module"):]) {
			if major, ok := majorOf(field); ok {
				return major, true
			}
		}
	}
	return 0, false
}

// majorOf reads the leading number of a version-like segment: "13.4" is 13, "5.7"
// is 5, "580.95.05" is 580. Anything that does not start with digits has no major,
// which the callers have to be able to tell apart from zero.
func majorOf(s string) (int, bool) {
	head, _, _ := strings.Cut(strings.TrimSpace(s), ".")
	if head == "" {
		return 0, false
	}
	for _, r := range head {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	major, err := strconv.Atoi(head)
	if err != nil {
		return 0, false
	}
	return major, true
}

// loaderCacheOnce holds the loader's index: it is a property of the machine that
// does not change while the app runs, and reading it costs a process.
var (
	loaderCacheOnce sync.Once
	loaderCacheText string
)

// loaderCache is the dynamic loader's own index of what it can find, `ldconfig -p`.
//
// It is the authority on "is this library installed": a soname in it resolves. A
// soname that is *not* in it may still resolve through a path this process does not
// see, so a miss is only ever a reason to keep looking — never to refuse on its own.
// An empty answer means the index could not be read, and every caller treats that
// as "unknown" rather than as "absent".
func loaderCache() string {
	loaderCacheOnce.Do(func() {
		if runtime.GOOS != "linux" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ldconfig", "-p")
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.WaitDelay = 2 * time.Second
		_ = cmd.Run()
		loaderCacheText = out.String()
	})
	return loaderCacheText
}

// pointsAtCUDADriver reports whether the loader's index names the driver's own
// library, which is what the CUDA backend loads on top of the runtime. A machine
// without it has no CUDA at all, whatever else is installed.
func pointsAtCUDADriver(cache string) bool {
	for _, line := range strings.Split(cache, "\n") {
		field := strings.TrimSpace(line)
		if cut := strings.IndexAny(field, " \t"); cut >= 0 {
			field = field[:cut]
		}
		if field == "libcuda.so.1" {
			return true
		}
	}
	return false
}

// loaderLibraryMajors reads the major versions of one soname family out of
// `ldconfig -p` output. Every line is `<soname> (<arch>) => <path>`, so the soname
// is the first field.
func loaderLibraryMajors(cache, prefix string) []int {
	seen := map[int]bool{}
	var majors []int
	for _, line := range strings.Split(cache, "\n") {
		field := strings.TrimSpace(line)
		if cut := strings.IndexAny(field, " \t"); cut >= 0 {
			field = field[:cut]
		}
		rest, ok := strings.CutPrefix(field, prefix+".")
		if !ok {
			continue
		}
		major, err := strconv.Atoi(rest)
		if err != nil || seen[major] {
			continue
		}
		seen[major] = true
		majors = append(majors, major)
	}
	sort.Ints(majors)
	return majors
}

// hipRuntimeMajors lists the HIP runtime versions this machine has, from the
// loader's index and from the ROCm directories a package or a tarball install puts
// them in — ROCm in /opt/rocm/lib is not always in the cache yet, and a machine
// whose only copy is there is a working machine.
func hipRuntimeMajors() []int {
	seen := map[int]bool{}
	var majors []int
	add := func(found []int) {
		for _, major := range found {
			if !seen[major] {
				seen[major] = true
				majors = append(majors, major)
			}
		}
	}
	add(loaderLibraryMajors(loaderCache(), "libamdhip64.so"))
	for _, dir := range append([]string{"/opt/rocm"}, rocmVersionedRoots()...) {
		add(majorsInDir(filepath.Join(dir, "lib"), "libamdhip64.so"))
	}
	sort.Ints(majors)
	return majors
}

// rocmVersionedRoots finds the ROCm trees that sit beside the default one, newest
// version first: /opt/rocm-7.2.4 and the like are what AMD's apt packages create,
// and a side-by-side upgrade leaves more than one of them behind.
func rocmVersionedRoots() []string {
	matches, err := filepath.Glob("/opt/rocm-*")
	if err != nil {
		return nil
	}
	sortROCMRoots(matches)
	return matches
}

// sortROCMRoots orders ROCm trees newest version first.
//
// The comparison is numeric per segment, not textual: `/opt/rocm-7.2.10` is newer
// than `/opt/rocm-7.2.4` and a string sort puts it first only by luck, while
// `7.10.0` against `7.2.0` would go the wrong way outright. A tree whose suffix is
// not a version sorts after the ones that are.
func sortROCMRoots(matches []string) {
	sort.Slice(matches, func(i, j int) bool {
		a, aok := dottedVersion(strings.TrimPrefix(filepath.Base(matches[i]), "rocm-"))
		b, bok := dottedVersion(strings.TrimPrefix(filepath.Base(matches[j]), "rocm-"))
		if !aok || !bok {
			// A directory that is not versioned is not comparable; keep it after
			// the ones that are, in a stable order.
			return aok && !bok
		}
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return len(a) > len(b)
	})
}

// majorsInDir reads the major versions of one soname family in a directory. A
// directory that is not there is no versions, not a failure.
func majorsInDir(dir, prefix string) []int {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"*"))
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var majors []int
	for _, match := range matches {
		rest, ok := strings.CutPrefix(filepath.Base(match), prefix+".")
		if !ok {
			continue
		}
		major, err := strconv.Atoi(rest)
		if err != nil || seen[major] {
			continue
		}
		seen[major] = true
		majors = append(majors, major)
	}
	sort.Ints(majors)
	return majors
}

// cudaRuntimePresent reports whether this machine already provides the CUDA runtime
// a build of one suite needs, so the 440 MB companion bundle can be left alone.
//
// Only a hit is trusted: `libcudart.so.13` and `libcublas.so.13` in the loader's
// own index are really there, while a miss only costs the download. A user with a
// CUDA toolkit installed this way keeps it, and everybody else gets the libraries
// llama.cpp published for exactly this build.
func cudaRuntimePresent(suite string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return cudaRuntimePresentIn(suite, loaderCache())
}

// cudaRuntimePresentIn is the same question against a given loader index.
func cudaRuntimePresentIn(suite, cache string) bool {
	major, ok := majorOf(suite)
	if !ok || cache == "" {
		return false
	}
	return loaderMajorPresent(cache, "libcudart.so", major) && loaderMajorPresent(cache, "libcublas.so", major)
}

// loaderMajorPresent reports whether the loader's index names one major version of
// a soname family.
func loaderMajorPresent(cache, name string, major int) bool {
	for _, have := range loaderLibraryMajors(cache, name) {
		if have == major {
			return true
		}
	}
	return false
}

// serverEnv is the environment a managed llama-server is started with.
//
// Two things are added to the inherited environment, both because the GPU runtime
// lives on the host rather than in the archive:
//
//   - the build's own directory. The published archives carry rpath `$ORIGIN`, so
//     this is not needed for their own libraries — but a CUDA runtime bundle that
//     was unpacked beside the build is found the same way, and a build whose rpath
//     an older packaging lacked still works from its own directory.
//   - the ROCm library directory, for a build that carries a HIP backend. The ROCm
//     archive ships no runtime and there is no companion asset for it: its
//     libggml-hip.so links libamdhip64/libhipblas/librocblas, which on a normal
//     ROCm install live in /opt/rocm/lib — a directory AMD documents as not being
//     on the loader path by default. Without this, a correctly installed ROCm
//     finds no device and the model runs on the CPU.
//
// Windows needs none of it: DLLs are resolved from the executable's directory
// first, which is exactly where the companion bundle unpacks.
func serverEnv(binary string) []string {
	env := os.Environ()
	if runtime.GOOS != "linux" {
		return env
	}
	dir := dirOf(binary)
	if strings.TrimSpace(dir) == "" {
		return env
	}
	paths := []string{dir}
	if hasHIPBackend(dir) {
		if rocm := rocmLibraryPath(); rocm != "" {
			paths = append(paths, rocm)
		}
	}
	paths = append(paths, splitLibraryPath(os.Getenv("LD_LIBRARY_PATH"))...)
	return setEnv(env, "LD_LIBRARY_PATH", strings.Join(paths, ":"))
}

// rocmLibraryPath is where this machine keeps the ROCm libraries, empty when there
// is nothing to point at.
//
// ROCM_PATH comes first because that is what a tarball install sets. Then
// /opt/rocm, and then the versioned trees beside it — which is the layout AMD's own
// apt packages actually produce: measured on 7.2.4 for noble, hip-runtime-amd
// 7.2.53211 unpacks libamdhip64.so.7 into /opt/rocm-7.2.4/lib and registers nothing
// with the loader, so a plain `apt install` leaves the libraries findable only by a
// path like this one.
func rocmLibraryPath() string {
	if root := strings.TrimSpace(os.Getenv("ROCM_PATH")); root != "" {
		if lib := existingDir(filepath.Join(root, "lib")); lib != "" {
			return lib
		}
	}
	for _, root := range append([]string{"/opt/rocm"}, rocmVersionedRoots()...) {
		if lib := existingDir(filepath.Join(root, "lib")); lib != "" {
			return lib
		}
	}
	return ""
}

// existingDir is the path when it names a directory, empty otherwise.
func existingDir(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return ""
}

// hasHIPBackend reports whether a build directory holds a HIP backend library,
// which is what makes the ROCm library path relevant.
func hasHIPBackend(dir string) bool {
	for _, library := range BackendLibraries(dir) {
		if strings.Contains(filepath.Base(library), "-hip") {
			return true
		}
	}
	return false
}

// splitLibraryPath splits a loader path, dropping empty entries.
func splitLibraryPath(value string) []string {
	var out []string
	for _, part := range strings.Split(value, string(os.PathListSeparator)) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// setEnv replaces one variable in an environment, keeping the rest as it was and
// adding the variable when it was not there.
func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if replaced {
				continue
			}
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}
