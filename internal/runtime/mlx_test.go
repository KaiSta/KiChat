package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
