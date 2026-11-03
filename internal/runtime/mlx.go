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

// mlxPythonVersions is the interpreter versions worth trying, newest first. They
// name both the `python3.x` files and the python.org framework directories.
//
// A version newer than the newest entry is not lost: `python3` is tried in every
// directory below, and that is the name Homebrew, python.org and pyenv all link.
// The list is for the machine where `python3` resolves to Apple's 3.9 while a
// newer interpreter sits beside it under its own name.
var mlxPythonVersions = []string{"3.15", "3.14", "3.13", "3.12", "3.11"}

// mlxPythonNames is the file names an interpreter may have in one directory,
// in the order one of them is preferred.
func mlxPythonNames() []string {
	names := []string{"python3"}
	for _, version := range mlxPythonVersions {
		names = append(names, "python"+version)
	}
	return append(names, "python")
}

// MLXPythonSearch is every place an interpreter may be, in the order it is tried:
// first the bare names, which are resolved the way the shell would resolve them,
// and then the absolute locations the shell would find through a PATH the app
// does not have.
//
// That second half is not belt and braces. A macOS app opened from the Finder or
// the Dock inherits launchd's environment, whose PATH is
// /usr/bin:/bin:/usr/sbin:/sbin: none of .zprofile, Homebrew, pyenv or a
// python.org install is in it, and the `python3` that is there is Apple's Command
// Line Tools 3.9. So a machine whose Terminal has 3.15 answered "no Python 3.11
// or newer" until these directories were named directly.
//
// home may be empty, which only drops the interpreters that live under it.
func MLXPythonSearch(home string) []string {
	search := mlxPythonNames()
	dirs := []string{
		"/opt/homebrew/bin", // Homebrew on Apple silicon
		"/usr/local/bin",    // Homebrew on Intel, and python.org's own links
	}
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".pyenv", "shims"),
			filepath.Join(home, ".local", "share", "mise", "shims"),
			filepath.Join(home, ".asdf", "shims"),
		)
	}
	// python.org's installer links into /usr/local/bin only when that directory
	// was its own idea, so the framework versions are named as well.
	for _, version := range mlxPythonVersions {
		dirs = append(dirs, filepath.Join("/Library/Frameworks/Python.framework/Versions", version, "bin"))
	}
	for _, dir := range dirs {
		for _, name := range mlxPythonNames() {
			search = append(search, filepath.Join(dir, name))
		}
	}
	return search
}

// MLXFindPython walks the search and returns the first interpreter that reports a
// version mlx-lm can be installed into, with the version it reported.
//
// A bare name is resolved with the same lookup a shell would do; an entry that
// names a place is taken as that path, on every platform — `IsLocal` is what tells
// the two apart, because a rooted path is not absolute to Windows, and a name typed
// here must never be stat'ed as if it were one.
//
// One that cannot be started, or that is older than the package supports, is passed
// over rather than tried: pip would spend a minute resolving wheels and then fail
// with a version error whose cause is the interpreter, not the package.
func MLXFindPython(ctx context.Context, search []string) (string, int, int, bool) {
	for _, entry := range search {
		path := entry
		if filepath.IsLocal(entry) {
			found, err := exec.LookPath(entry)
			if err != nil {
				continue
			}
			path = found
		} else if info, err := os.Stat(entry); err != nil || info.IsDir() {
			continue
		}
		major, minor, err := MLXPythonVersion(ctx, path)
		if err != nil {
			continue
		}
		if major > 3 || (major == 3 && minor >= 11) {
			return path, major, minor, true
		}
	}
	return "", 0, 0, false
}

// MLXSnapshot is one folder mlx_lm.server can load: a Hugging Face snapshot, which
// is a config.json and the weights beside it.
type MLXSnapshot struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Complete is false for a folder that has its config.json but no weights yet —
	// a download still running, or a snapshot someone tidied up. It is reported
	// rather than hidden: a card that silently drops the folder the user picked is
	// worse than one that says what is missing from it.
	Complete bool `json:"complete"`
}

// MLXScan is what one folder holds, as the MLX card reads it.
type MLXScan struct {
	Dir string `json:"dir"`
	// Snapshots is every snapshot in Dir itself, in its immediate children, or in
	// its snapshots/ directory — the three layouts a model ends up in: the snapshot
	// somebody unpacked, a folder of snapshots, and the Hugging Face cache's
	// models--org--name/snapshots/<hash>. Never nil: a null would be a second empty
	// case for the card to render.
	Snapshots []MLXSnapshot `json:"snapshots"`
	// Ggufs counts the .gguf files in Dir itself. Such a folder is llama.cpp's
	// model and mlx_lm.server cannot open it at all, so the card has to say that
	// rather than let Start fail on a model the server never names.
	Ggufs int `json:"ggufs"`
}

// mlxSnapshotAt reports whether dir is a snapshot, and whether its weights are
// there. A config.json alone is enough to be one: that is the file the server
// reads, so a folder it would try is worth naming even while the weights are still
// coming in.
func mlxSnapshotAt(dir string) (snapshot, complete bool) {
	if !IsDir(dir) || !mlxIsFile(filepath.Join(dir, "config.json")) {
		return false, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true, false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".safetensors", ".npz":
			return true, true
		}
	}
	return true, false
}

func mlxIsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// mlxSubdirs lists the immediate children of dir that are directories.
func mlxSubdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(dir, entry.Name()))
		}
	}
	return dirs
}

func countMLXGgufs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".gguf") {
			count++
		}
	}
	return count
}

// MLXScanFolder reports what one folder holds: the snapshots mlx_lm.server could
// load from it, and whether it is a GGUF folder instead.
//
// It reads a path the user is typing, so it asks the filesystem and nothing else: a
// folder that is not there is an empty scan, not an error the card would have to
// render beside every keystroke.
func MLXScanFolder(dir string) MLXScan {
	scan := MLXScan{Dir: strings.TrimSpace(dir), Snapshots: []MLXSnapshot{}}
	if scan.Dir == "" {
		return scan
	}
	add := func(candidate string) {
		snapshot, complete := mlxSnapshotAt(candidate)
		if !snapshot {
			return
		}
		for _, have := range scan.Snapshots {
			if have.Path == candidate {
				return
			}
		}
		scan.Snapshots = append(scan.Snapshots, MLXSnapshot{
			Path:     candidate,
			Name:     filepath.Base(candidate),
			Complete: complete,
		})
	}
	add(scan.Dir)
	for _, child := range mlxSubdirs(scan.Dir) {
		add(child)
	}
	for _, child := range mlxSubdirs(filepath.Join(scan.Dir, "snapshots")) {
		add(child)
	}
	scan.Ggufs = countMLXGgufs(scan.Dir)
	return scan
}

// MLXModelRefusal is why mlx_lm.server cannot load this folder, and nil when it
// can.
//
// It runs before a start, so the answer is a sentence about the folder instead of a
// process that exits on a model it cannot open — which is how a GGUF picked here
// used to fail, minutes later and in Python's words.
func MLXModelRefusal(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return uimsg.Errorf("model.noneSelected")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return uimsg.Errorf("model.notFound", err)
	}
	if !info.IsDir() {
		return uimsg.Errorf("runtime.mlxNotSnapshot", dir)
	}
	snapshot, complete := mlxSnapshotAt(dir)
	if !snapshot {
		if ggufs := countMLXGgufs(dir); ggufs > 0 {
			return uimsg.Errorf("runtime.mlxGgufFolder", strconv.Itoa(ggufs))
		}
		return uimsg.Errorf("runtime.mlxNotSnapshot", dir)
	}
	if !complete {
		return uimsg.Errorf("runtime.mlxNoWeights", dir)
	}
	return nil
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
