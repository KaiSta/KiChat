package images

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSDServer writes a folder holding a `sd-server` stub that answers `-h` the way a real
// build does — its usage lists the listener flag — because that is the one question
// ServerSpec asks of the program before it builds a command line. Nothing is started: this
// file is about the command line a start *would* use, which is the part a real model load
// takes minutes to prove.
func fakeSDServer(t *testing.T, usage string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + usage + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "sd-server"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sd-server: %v", err)
	}
	return dir
}

// sdUsage is the shape of a real build's help, trimmed to the flags this client looks for.
const sdUsage = `usage: sd-server [options]` + "\n" +
	`  --listen-ip <ip>             ip to listen on` + "\n" +
	`  --listen-port <port>         port to listen on` + "\n" +
	`  --diffusion-model <path>     path to the diffusion model` + "\n" +
	`  --offload-to-cpu             offload the model to cpu` + "\n"

// hasPair reports whether a flag was passed with exactly that value, next to each other.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestServerArgsAreTheModelSetPlusAListener is the whole of the third kind's command line.
//
// It pins two things at once, and the second is the reason this file exists: the model half is
// the *same* list the one-shot kind runs (so a folder, an offload switch and a set of files
// configured once describe both lifetimes), and nothing that describes a single picture is in
// it — a server takes the prompt, the step count and the seed in the request body, and a
// stray `-o` on a daemon's command line would be a file nothing ever reads.
func TestServerArgsAreTheModelSetPlusAListener(t *testing.T) {
	dir := fakeSDServer(t, sdUsage)
	client := NewCLI("images", "sd-cli", "", ModelFiles{
		DiffusionModel: "/models/qwen-image-2.1-Q5_K_M.gguf",
		VAE:            "/models/qwen_image_vae.safetensors",
		LLM:            "/models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf",
		ClipL:          "/models/clip_l.safetensors",
		ClipG:          "/models/clip_g.safetensors",
		T5XXL:          "/models/t5xxl.safetensors",
	}, Options{
		Folder:       dir,
		OffloadToCPU: true,
		Extra:        []string{"--diffusion-fa", "--cfg-scale", "1.0"},
	})

	spec, err := client.ServerSpec(context.Background(), 45999)
	if err != nil {
		t.Fatalf("ServerSpec: %v", err)
	}
	if want := filepath.Join(dir, "sd-server"); spec.Program != want {
		t.Errorf("program = %q, want %q (found inside the folder, not typed)", spec.Program, want)
	}
	args := spec.Args
	// The user's own arguments first, so a flag both set is won by the one this app would
	// record, and the model set after — the same rule the one-shot command line follows.
	if len(args) < 2 || args[0] != "--diffusion-fa" || !slices.Contains(args, "--offload-to-cpu") {
		t.Errorf("args = %v, want the user's own arguments first", args)
	}
	for _, pair := range [][2]string{
		{"--diffusion-model", "/models/qwen-image-2.1-Q5_K_M.gguf"},
		{"--vae", "/models/qwen_image_vae.safetensors"},
		{"--llm", "/models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf"},
		{"--clip_l", "/models/clip_l.safetensors"},
		{"--clip_g", "/models/clip_g.safetensors"},
		{"--t5xxl", "/models/t5xxl.safetensors"},
		{"--listen-ip", "127.0.0.1"},
		{"--listen-port", "45999"},
	} {
		if !hasPair(args, pair[0], pair[1]) {
			t.Errorf("args = %v, want %s %s", args, pair[0], pair[1])
		}
	}
	// The listener goes last, so the port it starts on is the one this app picked: a
	// user-supplied `--listen-port` earlier in the list must not outvote it, or the app
	// would probe an address nothing is listening on.
	if len(args) < 2 || args[len(args)-2] != "--listen-port" {
		t.Errorf("args = %v, want the listener last", args)
	}
	// Nothing about one picture.
	for _, oneShot := range []string{"-p", "-n", "-o", "-s", "--steps", "-W", "-H", "--sampling-method", "--preview"} {
		if slices.Contains(args, oneShot) {
			t.Errorf("args = %v, want no per-picture flag (%s)", args, oneShot)
		}
	}
}

// TestServerSpecNamesTheFileAStartLoads: the supervisor stats one file before it spawns a
// process, so the spec has to say which of the configured paths that is — the diffusion model
// when the pipeline names its parts, the single-file checkpoint otherwise, and never the VAE
// or an encoder, which are not something a start can load on its own.
func TestServerSpecNamesTheFileAStartLoads(t *testing.T) {
	dir := fakeSDServer(t, sdUsage)
	parts := NewCLI("images", "sd-cli", "", ModelFiles{
		DiffusionModel: "/models/qwen.gguf",
		VAE:            "/models/qwen-vae.safetensors",
	}, Options{Folder: dir})
	spec, err := parts.ServerSpec(context.Background(), 1234)
	if err != nil {
		t.Fatalf("ServerSpec: %v", err)
	}
	if spec.Model != "/models/qwen.gguf" {
		t.Errorf("model = %q, want the diffusion model", spec.Model)
	}
	if spec.Label != "qwen.gguf" {
		t.Errorf("label = %q, want the diffusion model's file name", spec.Label)
	}

	single := NewCLI("images", "sd-cli", "", ModelFiles{Model: "/models/sdxl.safetensors"}, Options{Folder: dir})
	spec, err = single.ServerSpec(context.Background(), 1234)
	if err != nil {
		t.Fatalf("ServerSpec: %v", err)
	}
	if spec.Model != "/models/sdxl.safetensors" || spec.Label != "sdxl.safetensors" {
		t.Errorf("spec = %+v, want the single-file checkpoint", spec)
	}
}

// The refusals. Every one of them is a setup that cannot draw, and each has to fail here
// rather than after a process has loaded several gigabytes: the point of a managed server is
// minutes of loading, so a start that cannot work must not spend them.
func TestServerSpecRefusesASetupThatCannotStart(t *testing.T) {
	dir := fakeSDServer(t, sdUsage)

	cases := []struct {
		name       string
		folder     string
		files      ModelFiles
		wantInText string
	}{
		{
			name:       "no model at all",
			folder:     dir,
			files:      ModelFiles{},
			wantInText: "kein Modell angegeben",
		},
		{
			// The same slot twice: the binary would load whichever the parser reads last
			// while the panel names the other one.
			name:       "both model slots filled",
			folder:     dir,
			files:      ModelFiles{Model: "/models/a.safetensors", DiffusionModel: "/models/b.gguf"},
			wantInText: "beide gesetzt",
		},
		{
			// Only a VAE: there is nothing to diffuse with, and a server that starts and
			// refuses every prompt is worse than a refusal here.
			name:       "only a part",
			folder:     dir,
			files:      ModelFiles{VAE: "/models/vae.safetensors"},
			wantInText: "kein Diffusionsmodell",
		},
		{
			name:       "no folder and no path",
			folder:     "",
			files:      ModelFiles{Model: "/models/a.gguf"},
			wantInText: ErrNotConfigured.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewCLI("images", "sd-cli", "", tc.files, Options{Folder: tc.folder})
			client.Lang = german
			_, err := client.ServerSpec(context.Background(), 1234)
			if err == nil {
				t.Fatal("the start was allowed")
			}
			if !strings.Contains(err.Error(), tc.wantInText) {
				t.Errorf("refusal = %q, want it to mention %q", err, tc.wantInText)
			}
		})
	}
}

// TestServerSpecRefusesABuildWithNoServerMode is the check that keeps a picture from being
// requested from the wrong process.
//
// The listener flag is what moves sd-server off its documented default port, and that default
// is 1234 — LM Studio's address, which this app talks to on many machines. A build that does
// not know the flag would be started on it, so the flag's absence is *evidence* and is
// refused; a build that cannot answer `-h` at all is not evidence and is allowed, the same line
// Health takes about the one-shot program.
func TestServerSpecRefusesABuildWithNoServerMode(t *testing.T) {
	withoutFlag := fakeSDServer(t, "usage: sd-something [options]\n  -m <path>  model\n")
	client := NewCLI("images", "sd-cli", "", ModelFiles{Model: "/models/a.gguf"}, Options{Folder: withoutFlag})
	client.Lang = german
	_, err := client.ServerSpec(context.Background(), 1234)
	if err == nil {
		t.Fatal("a build without --listen-port was allowed to start")
	}
	if !strings.Contains(err.Error(), "keinen Servermodus") {
		t.Errorf("refusal = %q, want it to name the missing server mode", err)
	}

	// A launcher that refuses `-h` and prints nothing is a working setup as far as this can
	// tell: it is not evidence of anything, and blocking it would refuse a program that
	// starts perfectly well.
	silent := t.TempDir()
	if err := os.WriteFile(filepath.Join(silent, "sd-server"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client = NewCLI("images", "sd-cli", "", ModelFiles{Model: "/models/a.gguf"}, Options{Folder: silent})
	if _, err := client.ServerSpec(context.Background(), 1234); err != nil {
		t.Errorf("a build that cannot answer -h was refused: %v", err)
	}
}

// TestServerKeyIgnoresThePortAndFollowsTheModel is what tells "the running server is still the
// configuration you saved" from "it is loading a model you have since replaced".
//
// The listener is out of it because a port is picked per start: including it would make every
// server look out of date the moment it came up. Everything a user can change about *what* is
// loaded is in it, which is the set of fields that reach the command line — so adding a flag
// to modelArgs without a field would show up here as a key that did not change.
func TestServerKeyIgnoresThePortAndFollowsEverythingElse(t *testing.T) {
	base := NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{
		Folder: "/sd", Extra: []string{"--cfg-scale", "1.0"},
	})
	key := base.ServerKey()
	if key != base.ServerKey() {
		t.Error("the same configuration produced two keys")
	}

	for _, change := range []struct {
		name  string
		build func() *CLI
	}{
		{"diffusion model", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/other.gguf"}, Options{Folder: "/sd", Extra: []string{"--cfg-scale", "1.0"}})
		}},
		{"vae", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf", VAE: "/models/v.safetensors"}, Options{Folder: "/sd", Extra: []string{"--cfg-scale", "1.0"}})
		}},
		{"extra arguments", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{Folder: "/sd", Extra: []string{"--cfg-scale", "2.0"}})
		}},
		{"offload switch", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{Folder: "/sd", OffloadToCPU: true, Extra: []string{"--cfg-scale", "1.0"}})
		}},
		// The VAE decode is part of the load, so turning tiling on — or changing the
		// tile's side under it — has to offer the restart the row exists for.
		{"vae tiling", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{Folder: "/sd", VaeTiling: true, Extra: []string{"--cfg-scale", "1.0"}})
		}},
		{"vae tile size", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{Folder: "/sd", VaeTiling: true, VaeTileSize: 512, Extra: []string{"--cfg-scale", "1.0"}})
		}},
		{"folder", func() *CLI {
			return NewCLI("images", "sd-cli", "", ModelFiles{DiffusionModel: "/models/qwen.gguf"}, Options{Folder: "/other", Extra: []string{"--cfg-scale", "1.0"}})
		}},
	} {
		if got := change.build().ServerKey(); got == key {
			t.Errorf("%s: the key did not change, so a restart would not be offered", change.name)
		}
	}
}
