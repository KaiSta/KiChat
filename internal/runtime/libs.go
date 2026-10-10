package runtime

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// notFound is the marker `ldd` prints for a shared object it could not resolve,
// as in:
//
//	libhipblas.so.3 => not found
const notFound = "=> not found"

// BackendLibraries lists the accelerator backend libraries of an installed build:
// the `libggml-<accel>.so` files ggml loads at runtime, one per backend family.
//
// The CPU variants, the base library and the RPC library are left out on purpose —
// they are always there and always loadable, and naming one of them as the reason a
// device is missing would be noise on every machine.
func BackendLibraries(buildDir string) []string {
	entries, err := os.ReadDir(buildDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "libggml-") || !strings.Contains(name, ".so") {
			continue
		}
		if strings.HasPrefix(name, "libggml-cpu") ||
			strings.HasPrefix(name, "libggml-base") ||
			strings.HasPrefix(name, "libggml-rpc") {
			continue
		}
		out = append(out, filepath.Join(buildDir, name))
	}
	sort.Strings(out)
	return out
}

// MissingLibraries names the shared objects this machine cannot give a build's
// accelerator backend libraries.
//
// It exists because a backend that fails to load says nothing at all, and that
// silence is what a user meets. Measured on this machine: the ROCm build of b11538
// against ROCm 5.7 printed `Available devices:` / `  (none)` with an empty stderr,
// while `ldd` on its own libggml-hip.so answered
//
//	libhipblas.so.3 => not found
//	librocblas.so.5 => not found
//	libamdhip64.so.7 => not found
//
// — the archive is built for ROCm 10, the machine's libamdhip64 is 5.7. So "the
// picked GPU build found no device" is true but useless; the names of what is
// missing are the part a user can act on, and they are what the panel shows beside
// an empty device list.
//
// Only Linux has an implementation: `ldd` is the GNU loader's own answer here, and
// the equivalents elsewhere would have to be written and measured before they could
// be trusted. Saying nothing is the honest answer where that has not been done.
func MissingLibraries(ctx context.Context, buildDir string) []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	seen := map[string]bool{}
	var missing []string
	for _, library := range BackendLibraries(buildDir) {
		for _, name := range missingLibraryNames(lddOutput(ctx, library)) {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// lddOutput runs the loader's own dependency check and returns what it printed.
//
// The exit status is deliberately ignored: `ldd` fails precisely when a dependency
// is missing, which is the answer this is after. An output that does not describe a
// dynamic executable ("not a dynamic executable") simply carries no marker and
// yields no names.
func lddOutput(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, "ldd", path)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// `ldd` is the loader's own check and honours LD_LIBRARY_PATH, so it gets the
	// same environment a server is started with: without the ROCm library path it
	// would report a HIP build's runtime as missing on a machine that has it.
	cmd.Env = serverEnv(path)
	// The loader can hang on a library that in turn starts something; the same
	// guard the other probes use, for the same reason — killing ldd does not kill a
	// child of its own, and that child holds this pipe.
	cmd.WaitDelay = 2 * time.Second
	_ = cmd.Run()
	return out.String()
}

// missingLibraryNames reads the unresolved names out of `ldd` output.
func missingLibraryNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		cut := strings.Index(line, notFound)
		if cut < 0 {
			continue
		}
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line[:cut]), "=>"))
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
