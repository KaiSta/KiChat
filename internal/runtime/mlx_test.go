package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"kichat/internal/uimsg"
)

func TestMLXArgsCarryOnlyWhatWasChosen(t *testing.T) {
	full := MLXArgs(MLXOptions{
		ModelDir:        "/models/qwen3-4bit",
		Port:            9123,
		KVBits:          4,
		PrefillStep:     512,
		MaxTokens:       4096,
		DraftModel:      "/models/draft",
		ChatTemplate:    "qwen3",
		TrustRemoteCode: true,
		Extra:           []string{"--seed", "7"},
	})
	want := []string{
		"--model", "/models/qwen3-4bit",
		"--host", "127.0.0.1",
		"--port", "9123",
		"--max-tokens", "4096",
		"--kv-bits", "4",
		"--prefill-step-size", "512",
		"--draft-model", "/models/draft",
		"--chat-template", "qwen3",
		"--trust-remote-code",
		"--seed", "7",
	}
	if !reflect.DeepEqual(full, want) {
		t.Fatalf("MLXArgs =\n%v\nwant\n%v", full, want)
	}

	// Every optional flag is absent when nothing chose it: a flag the server does
	// not get is a server default, and those are the right answers.
	quiet := MLXArgs(MLXOptions{ModelDir: "/models/a", Port: 8080})
	wantQuiet := []string{"--model", "/models/a", "--host", "127.0.0.1", "--port", "8080"}
	if !reflect.DeepEqual(quiet, wantQuiet) {
		t.Fatalf("MLXArgs with defaults =\n%v\nwant\n%v", quiet, wantQuiet)
	}

	// An unsupported width never reaches the command line: the settings layer
	// repairs it to 0, and this is the second line of defence.
	if args := MLXArgs(MLXOptions{ModelDir: "/m", Port: 1, KVBits: 5}); hasArg(args, "--kv-bits") {
		t.Errorf("an unaccepted --kv-bits width was passed: %v", args)
	}

	// The module form is the one a Python interpreter takes.
	module := MLXModuleArgs(MLXOptions{ModelDir: "/m", Port: 1})
	if len(module) < 2 || module[0] != "-m" || module[1] != MLXPythonModule {
		t.Fatalf("MLXModuleArgs = %v, want it to start with -m %s", module, MLXPythonModule)
	}
}

func hasArg(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestMLXIsAppleSiliconOnly(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         bool
	}{
		{"darwin", "arm64", true},
		{"darwin", "amd64", false},
		{"linux", "amd64", false},
		{"windows", "arm64", false},
	}
	for _, c := range cases {
		if got := MLXSupported(c.goos, c.goarch); got != c.want {
			t.Errorf("MLXSupported(%q, %q) = %v, want %v", c.goos, c.goarch, got, c.want)
		}
	}
}

// fakePython writes a stand-in interpreter: a shell script that answers the version
// probe MLXPythonVersion runs, or fails to start when version is empty.
func fakePython(t *testing.T, dir, name, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script is not an interpreter on Windows")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexit 1\n"
	if version != "" {
		script = "#!/bin/sh\nprintf '" + version + "\\n'\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The reported failure this search order is written around: in Terminal `python3`
// is 3.15, in the Finder-launched app it is Apple's 3.9. One that is too old, gone
// or unable to start must not stop the walk.
func TestMLXFindPythonPassesOverWhatItCannotUse(t *testing.T) {
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	fakePython(t, pathDir, "python3", "3.9")
	missing := filepath.Join(pathDir, "python3.15")
	broken := fakePython(t, t.TempDir(), "python3.12", "")
	good := fakePython(t, pathDir, "python3.11", "3.11")

	path, major, minor, ok := MLXFindPython(context.Background(),
		[]string{"python3", missing, broken, good})
	if !ok {
		t.Fatal("no interpreter was found")
	}
	if path != good || major != 3 || minor != 11 {
		t.Fatalf("MLXFindPython = %q, %d.%d; want %q, 3.11", path, major, minor, good)
	}

	if _, _, _, ok := MLXFindPython(context.Background(), []string{"python3", broken, missing}); ok {
		t.Error("an interpreter was reported when only unusable ones were in the search")
	}
}

func TestMLXPythonSearchReachesPastTheLaunchersPath(t *testing.T) {
	const home = "/Users/ada"
	search := MLXPythonSearch(home)
	at := func(entry string) int {
		for i, candidate := range search {
			if candidate == entry {
				return i
			}
		}
		return -1
	}

	// A bare name comes first: an app started from a shell still uses the
	// interpreter that shell meant, and that is the one the user is thinking of.
	if len(search) == 0 || search[0] != "python3" {
		t.Fatalf("the search does not start with the bare name: %v", search)
	}

	for _, want := range []string{
		"python3.15",
		"/opt/homebrew/bin/python3",
		"/usr/local/bin/python3",
		"/Library/Frameworks/Python.framework/Versions/3.15/bin/python3",
		filepath.Join(home, ".local", "bin", "python3"),
		filepath.Join(home, ".pyenv", "shims", "python3"),
	} {
		if at(want) < 0 {
			t.Errorf("%q is not in the search", want)
		}
	}
	// Homebrew before the Intel path: on Apple silicon that is the one with a
	// current interpreter behind it.
	if at("/opt/homebrew/bin/python3") > at("/usr/local/bin/python3") {
		t.Error("the Homebrew directory is tried after /usr/local/bin")
	}

	// An unreadable home drops only the interpreters that live under it.
	for _, entry := range MLXPythonSearch("") {
		for _, under := range []string{".pyenv", ".asdf", ".local/share/mise", "/Users/"} {
			if strings.Contains(entry, under) {
				t.Errorf("an entry from the home directory survived an empty home: %q", entry)
			}
		}
	}
}

// writeSnapshot builds the shapes a model folder comes in: a config.json and, when
// asked, one weight file beside it.
func writeSnapshot(t *testing.T, dir string, config, weights bool) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if config {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if weights {
		if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("w"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// refusalCode is the uimsg code a refusal carries, so a test pins what the user is
// told without pinning the sentence.
func refusalCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	msg, ok := uimsg.Parse(err.Error())
	if !ok {
		t.Fatalf("the refusal is not a user message: %v", err)
	}
	return msg.Code
}

// The reported failure: a folder holding a GGUF and its projector, picked as an MLX
// model. mlx_lm.server cannot open one, and a start that found that out by exiting
// said nothing about the folder.
func TestMLXStartRefusesAFolderTheServerCannotLoad(t *testing.T) {
	ggufDir := t.TempDir()
	for _, name := range []string{"model-Q4_K_M.gguf", "mmproj-model.gguf"} {
		if err := os.WriteFile(filepath.Join(ggufDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	halfway := writeSnapshot(t, t.TempDir(), true, false)
	whole := writeSnapshot(t, t.TempDir(), true, true)

	cases := []struct {
		name string
		dir  string
		want string
	}{
		{"nothing picked", "", "model.noneSelected"},
		{"a folder that is not there", filepath.Join(t.TempDir(), "nope"), "model.notFound"},
		{"an empty folder", t.TempDir(), "runtime.mlxNotSnapshot"},
		{"a GGUF folder", ggufDir, "runtime.mlxGgufFolder"},
		{"a snapshot without weights", halfway, "runtime.mlxNoWeights"},
		{"a whole snapshot", whole, ""},
	}
	for _, c := range cases {
		if got := refusalCode(t, MLXModelRefusal(c.dir)); got != c.want {
			t.Errorf("%s: refusal = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMLXScanFolderFindsTheSnapshotsAndTheGgufs(t *testing.T) {
	// An unpacked snapshot is one row, and the folder itself is the model.
	single := writeSnapshot(t, t.TempDir(), true, true)
	scan := MLXScanFolder(single)
	if len(scan.Snapshots) != 1 || scan.Snapshots[0].Path != single || !scan.Snapshots[0].Complete {
		t.Fatalf("a snapshot folder scanned as %+v", scan.Snapshots)
	}

	// A folder of snapshots: the rows are the exact model, because `--model` takes the
	// snapshot and not the folder above it.
	parent := t.TempDir()
	writeSnapshot(t, filepath.Join(parent, "qwen-8b-4bit"), true, true)
	writeSnapshot(t, filepath.Join(parent, "llama-3-8b"), true, false)
	if err := os.WriteFile(filepath.Join(parent, "extra.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	scan = MLXScanFolder(parent)
	if len(scan.Snapshots) != 2 {
		t.Fatalf("two snapshots scanned as %+v", scan.Snapshots)
	}
	if scan.Ggufs != 1 {
		t.Errorf("ggufs = %d, want the one file in the folder", scan.Ggufs)
	}
	for _, snapshot := range scan.Snapshots {
		if snapshot.Name == "llama-3-8b" && snapshot.Complete {
			t.Error("a snapshot without weights was reported complete")
		}
	}

	// The Hugging Face cache keeps the snapshot two levels down, and the repository
	// directory is what a person picks, so the snapshot has to be found from it.
	cache := t.TempDir()
	writeSnapshot(t, filepath.Join(cache, "models--org--name", "snapshots", "abc123"), true, true)
	scan = MLXScanFolder(filepath.Join(cache, "models--org--name"))
	if len(scan.Snapshots) != 1 || filepath.Base(scan.Snapshots[0].Path) != "abc123" {
		t.Fatalf("the cache layout scanned as %+v", scan.Snapshots)
	}

	// An empty answer is an empty list rather than null: a null is a second empty
	// case the card would have to render, and the one it has crashed on before.
	if empty := MLXScanFolder(t.TempDir()); empty.Snapshots == nil || len(empty.Snapshots) != 0 {
		t.Fatalf("an empty folder scanned as %+v, want an empty list", empty.Snapshots)
	}
}

func TestMLXVenvPythonFindsTheInterpreter(t *testing.T) {
	dir := t.TempDir()
	if _, ok := MLXVenvPython(dir); ok {
		t.Fatal("an empty directory reported an interpreter")
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(bin, "python3")
	if err := os.WriteFile(python, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := MLXVenvPython(dir)
	if !ok || got != python {
		t.Fatalf("MLXVenvPython = %q, %v; want %q, true", got, ok, python)
	}
}
