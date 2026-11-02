package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"kichat/internal/uimsg"
)

// MLX is the second program this package can supervise, and the first it has to
// install through something other than a GitHub archive.
//
// There is no MLX binary: Apple ships `mlx_lm.server` as part of the `mlx-lm`
// Python package, which is why an install here is a virtual environment and a pip
// run rather than an unpacked tarball. Everything after that — the free port, the
// readiness probe, the log ring, the tree kill — is the same work as llama.cpp's,
// which is why Options grew Args/ReadyPath/Label and this file only has to say
// what the command line is.
const (
	// MLXLabel names the program in messages and in the panel's log.
	MLXLabel = "mlx_lm.server"
	// MLXPackage is the distribution pip installs. It is not pinned: mlx-lm tracks
	// MLX and the wheels are built per Python and macOS version, so a pin is a
	// build that stops installing the day a new Python is released. The version
	// that actually landed is read back from the package and recorded.
	MLXPackage = "mlx-lm"
	// MLXPythonModule is the module `python -m` runs.
	MLXPythonModule = "mlx_lm.server"
)

// MLXSupported reports whether this machine can run MLX at all. It is not a
// preference: MLX is Apple's array framework for Apple silicon, so Intel Macs and
// every other platform are out.
func MLXSupported(goos, goarch string) bool {
	return goos == "darwin" && goarch == "arm64"
}

// MLXOptions is one managed MLX server, as a command line.
type MLXOptions struct {
	// ModelDir is the snapshot directory holding config.json and the weights. It
	// is what `--model` takes; a Hugging Face repo id is deliberately not accepted
	// here, because the server would download it on the first request.
	ModelDir string
	Host     string
	Port     int
	KVBits   int
	// PrefillStep is `--prefill-step-size`. The server's documentation pairs it
	// with a quantised KV cache: at a large step the attention score matrix can be
	// bigger than the memory the quantisation saved.
	PrefillStep int
	MaxTokens   int
	DraftModel  string
	// ChatTemplate is a template name or a path, `--chat-template`.
	ChatTemplate string
	// TrustRemoteCode passes `--trust-remote-code`, which some repositories need to
	// load their own modelling code.
	TrustRemoteCode bool
	// Extra is the user's own command line, already split. It goes last so it wins.
	Extra []string
}

// MLXArgs is the flag list a launcher that already is `mlx_lm.server` accepts.
func MLXArgs(o MLXOptions) []string {
	host := strings.TrimSpace(o.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	args := []string{"--model", o.ModelDir, "--host", host, "--port", strconv.Itoa(o.Port)}
	if o.MaxTokens > 0 {
		args = append(args, "--max-tokens", strconv.Itoa(o.MaxTokens))
	}
	// Only the widths the server accepts are passed; the settings layer repaired
	// everything else back to 0, which means "leave the cache unquantised".
	switch o.KVBits {
	case 2, 3, 4, 6, 8:
		args = append(args, "--kv-bits", strconv.Itoa(o.KVBits))
	}
	if o.PrefillStep > 0 {
		args = append(args, "--prefill-step-size", strconv.Itoa(o.PrefillStep))
	}
	if draft := strings.TrimSpace(o.DraftModel); draft != "" {
		args = append(args, "--draft-model", draft)
	}
	if template := strings.TrimSpace(o.ChatTemplate); template != "" {
		args = append(args, "--chat-template", template)
	}
	if o.TrustRemoteCode {
		args = append(args, "--trust-remote-code")
	}
	return append(args, o.Extra...)
}

// MLXModuleArgs is the whole command line after a Python interpreter: `-m
// mlx_lm.server` and then the flags.
func MLXModuleArgs(o MLXOptions) []string {
	return append([]string{"-m", MLXPythonModule}, MLXArgs(o)...)
}

// MLXVenvDir is where KiChat's own mlx-lm environment lives, beside the llama.cpp
// builds and for the same reason: one directory the app owns, workspace-relative
// to its data directory and removable without touching the user's Python.
func MLXVenvDir(dataDir string) string {
	return filepath.Join(dataDir, "runtime", "mlx")
}

// MLXVenvPython is the interpreter inside that environment, whichever name the
// platform's venv layout uses.
func MLXVenvPython(venvDir string) (string, bool) {
	for _, rel := range []string{
		filepath.Join("bin", "python3"),
		filepath.Join("bin", "python"),
		filepath.Join("Scripts", "python.exe"),
	} {
		path := filepath.Join(venvDir, rel)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// MLXVersion reads the installed package's version.
//
// It asks the environment rather than the settings, because pip resolves what pip
// resolves: a recorded version is only a claim, and the panel's version line has to
// be the one that will actually run.
func MLXVersion(ctx context.Context, python string) (string, error) {
	out, err := exec.CommandContext(ctx, python, "-c", "import mlx_lm; print(mlx_lm.__version__)").Output()
	if err != nil {
		return "", uimsg.Errorf("runtime.mlxVersion", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", uimsg.Errorf("runtime.mlxVersion", "no version printed")
	}
	return version, nil
}

// MLXPythonVersion asks an interpreter what it is, so an install can refuse a
// Python the package does not support before it spends minutes on pip.
func MLXPythonVersion(ctx context.Context, python string) (int, int, error) {
	out, err := exec.CommandContext(ctx, python, "-c",
		"import sys; print(f'{sys.version_info[0]}.{sys.version_info[1]}')").Output()
	if err != nil {
		return 0, 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), ".", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unreadable version %q", strings.TrimSpace(string(out)))
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// mlxFailure is the one line of a venv or pip failure worth showing.
//
// CombinedOutput can be megabytes — pip prints every wheel it considers — and the
// message crosses the bridge into a settings row. The last line is usually the
// error itself ("No matching distribution found…"), so it is preferred; the whole
// output is the fallback when there is only one line, bounded either way.
func mlxFailure(out []byte, err error) string {
	text := strings.TrimSpace(string(out))
	if text == "" {
		return err.Error()
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[i+1:])
	}
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	return text
}

// MLXPythonCandidates is what to look for when the user has not named an
// interpreter. `python3` first because a macOS with the developer tools has it,
// then the versioned names Homebrew and python.org install.
func MLXPythonCandidates() []string {
	return []string{"python3", "python3.13", "python3.12", "python3.11", "python"}
}

// MLXInstall builds the environment and installs the package. It returns the
// version that landed.
//
// Phases are reported rather than bytes: pip's progress is not a length it knows
// in advance, and a bar that guessed would be a lie. The caller turns the phase
// into the row's sentence, which is the same treatment a llama.cpp download gets.
//
// The environment is created once and reused: a second install upgrades the
// package in place rather than deleting an environment a running server may be
// using. Removing it is a separate, explicit act.
func MLXInstall(ctx context.Context, python, venvDir string, onPhase func(string)) (string, error) {
	if onPhase == nil {
		onPhase = func(string) {}
	}
	if err := os.MkdirAll(filepath.Dir(venvDir), 0o755); err != nil {
		return "", uimsg.Errorf("runtime.mlxInstall", err)
	}
	onPhase("venv")
	cmd := exec.CommandContext(ctx, python, "-m", "venv", venvDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", uimsg.Errorf("runtime.mlxInstall", mlxFailure(out, err))
	}
	py, ok := MLXVenvPython(venvDir)
	if !ok {
		return "", uimsg.Errorf("runtime.mlxPythonMissing", venvDir)
	}

	onPhase("pip")
	cmd = exec.CommandContext(ctx, py, "-m", "pip", "install", "--upgrade", "pip")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", uimsg.Errorf("runtime.mlxInstall", mlxFailure(out, err))
	}
	cmd = exec.CommandContext(ctx, py, "-m", "pip", "install", "--upgrade", MLXPackage)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", uimsg.Errorf("runtime.mlxInstall", mlxFailure(out, err))
	}

	onPhase("version")
	version, err := MLXVersion(ctx, py)
	if err != nil {
		return "", err
	}
	return version, nil
}
