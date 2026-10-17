package images

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCLI writes an executable that behaves like `sd-cli` in the parts this client
// depends on: it records its arguments, copies a picture to the path `-o` names, and logs
// the seed it used. A real binary would be tens of gigabytes of weights and minutes of
// model loading, and the project already tests the llama.cpp runtime this way — a
// `#!/bin/sh` stub in a temp directory — rather than against a real install.
//
// The picture is copied from the directory the script lives in, so the stub needs no
// base64 or printf escape games to produce bytes the sniffer recognises.
func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sd-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake sd-cli: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "picture.png"), pngBytes, 0o644); err != nil {
		t.Fatalf("write fake picture: %v", err)
	}
	return path
}

// cliRecorder is the argument-recording body: it keeps the command line, copies the
// picture where asked, and logs the seed it was given.
const cliRecorder = `
printf '%s\n' "$@" > "$(dirname "$0")/args.txt"
out=""; seed=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -s) seed="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp "$(dirname "$0")/picture.png" "$out"
echo "model loaded"
echo "generating image: 1/1 - seed ${seed:-0}"
exit 0
`

// argsOf reads back the command line the fake recorded.
func argsOf(t *testing.T, binary string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "args.txt"))
	if err != nil {
		t.Fatalf("the binary's command line was not recorded: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// TestCLIArgsCarryTheModelSetAndTheRecipe pins the whole command line, in order, because
// the order is a requirement rather than an accident: the user's own arguments come first so
// that a value the recipe records — the step count, the seed — is the one that wins when
// both set it. A panel that showed 20 steps while the binary drew 8 would record a recipe
// for a picture that does not exist.
//
// The flag spellings are asserted because they are easy to get wrong from prose about the
// project: `--clip_l` keeps its underscores, the scheduler flag is `--scheduler`, and a
// dashed spelling is not a flag the binary knows at all.
func TestCLIArgsCarryTheModelSetAndTheRecipe(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{
		DiffusionModel: "/models/qwen-image-Q5_K_M.gguf",
		VAE:            "/models/vae_bf16.safetensors",
		LLM:            "/models/Qwen3-VL-8B-Q5_K_M.gguf",
	}, Options{Extra: []string{"--diffusion-fa", "--cfg-scale", "1.0", "--sigmas", "1.0,0.5,0.0"}})

	seed := int64(4242)
	result, err := client.Generate(t.Context(), Request{
		Prompt:         "a cartoon sloth mascot waving",
		NegativePrompt: "blurry, text",
		Width:          1024,
		Height:         1024,
		Steps:          8,
		Sampler:        "euler",
		CfgScale:       7.5,
		Seed:           &seed,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	args := argsOf(t, binary)
	wantPrefix := []string{"--diffusion-fa", "--cfg-scale", "1.0", "--sigmas", "1.0,0.5,0.0"}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("argument %d = %q, want the user's own %q first:\n%s", i, args[i], want, strings.Join(args, " "))
		}
	}
	rest := strings.Join(args[len(wantPrefix):], " ")
	for _, want := range []string{
		// A standalone pipeline names its parts; -m must not appear at all.
		"--diffusion-model /models/qwen-image-Q5_K_M.gguf",
		"--vae /models/vae_bf16.safetensors",
		"--llm /models/Qwen3-VL-8B-Q5_K_M.gguf",
		"-p a cartoon sloth mascot waving",
		"-n blurry, text",
		"--steps 8",
		"-W 1024",
		"-H 1024",
		"--sampling-method euler",
		// Last, and therefore the winner over the `--cfg-scale 1.0` the user's own extra
		// arguments carry at the front of this same command line: the flag moved out of
		// Extra when the model was given a say in it, and the recipe has to describe the
		// scale that was really drawn with.
		"--cfg-scale 7.5",
		// Always sent, never omitted: the flag's own default is 42.
		"-s 4242",
		"-o ",
	} {
		if !strings.Contains(rest, want) {
			t.Errorf("the command line is missing %q:\n%s", want, rest)
		}
	}
	if strings.Contains(rest, " -m ") {
		t.Errorf("-m was passed alongside a standalone diffusion model:\n%s", rest)
	}
	if result.Seed != 4242 {
		t.Errorf("seed = %d, want the requested 4242", result.Seed)
	}
	if len(result.Images) != 1 || result.Images[0].Mime != MimePNG {
		t.Fatalf("images = %+v, want one PNG", result.Images)
	}
	if result.Model != "qwen-image-Q5_K_M.gguf" {
		t.Errorf("model = %q, want the diffusion model's own name", result.Model)
	}
}

// TestCLIArgsOmitWhatWasNotAskedFor is the panel's "empty means leave it to the binary"
// rule, and it is checked on the command line because that is where the mistake would be
// invisible: an empty `-n ""` replaces nothing, but an empty `--steps 0` is a real request
// for zero steps.
func TestCLIArgsOmitWhatWasNotAskedFor(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/v1-5-pruned-emaonly.safetensors"}, Options{})

	result, err := client.Generate(t.Context(), Request{Prompt: "a lovely cat"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	args := argsOf(t, binary)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-m /models/v1-5-pruned-emaonly.safetensors") {
		t.Errorf("a single-file checkpoint must be passed as -m:\n%s", joined)
	}
	for _, unwanted := range []string{"-n", "--steps", "-W", "-H", "--sampling-method", "--diffusion-model", "--vae", "--llm"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("%q was passed for a field that was left empty:\n%s", unwanted, joined)
		}
	}
	// The seed is the one field that is never omitted, even when nothing asked for one.
	idx := indexOf(args, "-s")
	if idx < 0 {
		t.Fatalf("no -s in the command line:\n%s", joined)
	}
	sent, err := strconv.ParseInt(args[idx+1], 10, 64)
	if err != nil || sent <= 0 {
		t.Fatalf("-s %q, want a positive seed", args[idx+1])
	}
	if result.Seed != sent {
		t.Errorf("result seed = %d, but -s said %d", result.Seed, sent)
	}
}

// TestCLIArgsTakeAModelFileAndItsParts is the invocation this backend was asked for: a
// *single* quantized diffusion model passed as `-m`, which still needs a VAE and a text
// encoder beside it. `-m` and `--diffusion-model` name the same thing and are not passed
// together, but `-m` with `--vae` and `--llm` is an ordinary newer pipeline rather than a
// contradiction — a fact that came from a real command line, and one the panel's hint now
// states so a user with that command is not told to empty the `-m` field.
func TestCLIArgsTakeAModelFileAndItsParts(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{
		Model: "../models/qwen-image-2.1-Q5_K_M.gguf",
		VAE:   "../models/qwen_image_2.1_vae_bf16.safetensors",
		LLM:   "../models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf",
	}, Options{})

	if _, err := client.Generate(t.Context(), Request{Prompt: "a lovely cat"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	joined := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{
		"-m ../models/qwen-image-2.1-Q5_K_M.gguf",
		"--vae ../models/qwen_image_2.1_vae_bf16.safetensors",
		"--llm ../models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf",
		"-p a lovely cat",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the command line is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--diffusion-model") {
		t.Errorf("-m and --diffusion-model are one model and must not both be passed:\n%s", joined)
	}
}

// TestCLIOffloadToCPUIsPassedBare is the one mode this app models itself, and the shape on
// the command line matters as much as its presence: a working Flux invocation passes
// `--offload-to-cpu` with no value, so sending one would be this app inventing syntax for a
// flag whose build decides what it accepts. The switch is checked *off* in the same test,
// because a checkbox that leaked the flag unconditionally would look identical on a machine
// where it happened to be set.
func TestCLIOffloadToCPUIsPassedBare(t *testing.T) {
	flux := ModelFiles{Model: "/models/flux-2-klein-4b-Q6_K.gguf"}

	t.Run("on", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		client := NewCLI("images", "sd-cli", binary, flux, Options{OffloadToCPU: true})

		if _, err := client.Generate(t.Context(), Request{Prompt: "a psyduck using waterguns"}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		args := argsOf(t, binary)
		if indexOf(args, "--offload-to-cpu") < 0 {
			t.Fatalf("the switch did not reach the command line:\n%s", strings.Join(args, " "))
		}
		joined := strings.Join(args, " ")
		for _, invented := range []string{"--offload-to-cpu 1", "--offload-to-cpu true", "--offload-to-cpu yes"} {
			if strings.Contains(joined, invented) {
				t.Errorf("a bare flag was given a value this app invented: %q\n%s", invented, joined)
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		client := NewCLI("images", "sd-cli", binary, flux, Options{})

		if _, err := client.Generate(t.Context(), Request{Prompt: "a psyduck using waterguns"}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if joined := strings.Join(argsOf(t, binary), " "); strings.Contains(joined, "--offload-to-cpu") {
			t.Errorf("the flag was passed with the switch off:\n%s", joined)
		}
	})
}

func indexOf(args []string, flag string) int {
	for i, arg := range args {
		if arg == flag {
			return i
		}
	}
	return -1
}

// TestCLIBelievesTheSeedItsLogReports covers the one case where the log disagrees with the
// command line, and it is not hypothetical: a build may ignore `--seed` and randomise
// anyway. The number that reproduces the picture is the one the log names, so that one is
// recorded — recording the number KiChat *asked* for would produce a different picture on
// every redraw while looking like it worked.
func TestCLIBelievesTheSeedItsLogReports(t *testing.T) {
	binary := fakeCLI(t, `
out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "$@" > "$(dirname "$0")/args.txt"
cp "$(dirname "$0")/picture.png" "$out"
echo "generating image: 1/1 - seed 999"
exit 0
`)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/one.gguf"}, Options{})

	asked := int64(4242)
	result, err := client.Generate(t.Context(), Request{Prompt: "a fox", Seed: &asked})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Seed != 999 {
		t.Errorf("seed = %d, want the 999 the binary says it drew with", result.Seed)
	}
}

// TestCLIReportsAFailedRunWithItsOwnWords is the whole diagnosis a user gets: this backend
// has no health route and no HTTP status, so the binary's last lines are all there is. A
// bare "exit status 1" would say nothing about the wrong path or the missing VAE.
func TestCLIReportsAFailedRunWithItsOwnWords(t *testing.T) {
	binary := fakeCLI(t, `
echo "loading model from /models/fox.gguf"
echo "error: failed to load vae: no such file" >&2
exit 1
`)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	_, err := client.Generate(t.Context(), Request{Prompt: "a fox"})
	if err == nil {
		t.Fatal("a failed run was reported as a success")
	}
	if !strings.Contains(err.Error(), "failed to load vae") {
		t.Errorf("the binary's own error was dropped: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("the exit status was dropped: %v", err)
	}
}

// TestCLIReportsARunThatWroteNothing is the shape a success looks like when it is not one:
// a wrapper script that exits 0 without producing a file. Reporting an empty result instead
// would file a zero-byte picture.
func TestCLIReportsARunThatWroteNothing(t *testing.T) {
	binary := fakeCLI(t, `
echo "generating image: 1/1 - seed 4242"
exit 0
`)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	_, err := client.Generate(t.Context(), Request{Prompt: "a fox"})
	if err == nil {
		t.Fatal("a run that wrote no picture was reported as a success")
	}
	if !strings.Contains(err.Error(), "kein Bild") {
		t.Errorf("err = %v, want a message about the missing picture", err)
	}
}

// TestCLIStopKillsTheProcess is this backend's Stop, and it is the one place where the CLI
// client is *simpler* than the HTTP one: there is no server to ask and no global interrupt to
// aim, so cancelling is killing.
//
// The assertion is on the time as much as on the error: a draw that has to run to completion
// would take the script's own minute, which is the behaviour this feature exists to avoid.
func TestCLIStopKillsTheProcess(t *testing.T) {
	// The fake is a *wrapper script* on purpose: a shell that starts the real drawing
	// process without `exec`, which is an ordinary thing to configure and the case where a
	// stop that only kills the shell looks like it worked — the wait ends and the machine
	// keeps drawing. The background loop stands in for sd-cli itself, and it beats a file so
	// that "the work stopped" is observable rather than assumed.
	binary := fakeCLI(t, `
dir="$(dirname "$0")"
i=0
while [ $i -lt 400 ]; do
  echo tick >> "$dir/beats.txt"
  i=$((i + 1))
  sleep 0.05
done &
echo "loading model, this takes a while"
sleep 60
`)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	_, err := client.Generate(ctx, Request{Prompt: "a fox"})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a cancelled draw was reported as a success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation itself — the draw path tells a user's Stop from a failure by it", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the draw took %s to stop, so the process kept running", elapsed)
	}

	// The wait ending is not the claim; the work ending is. Two readings a second apart settle
	// it, because the loop keeps beating while it runs whatever the shell is doing.
	first := countBeats(t, binary)
	time.Sleep(time.Second)
	if second := countBeats(t, binary); second != first {
		t.Errorf("the work is still running after Stop: %d beats, then %d a second later", first, second)
	}
	if first >= 400 {
		t.Error("the stand-in work had already finished, so this test proved nothing")
	}
}

// countBeats is how many times the fake's background loop has run so far. A missing file is
// zero beats rather than an error: the loop may not have started yet.
func countBeats(t *testing.T, binary string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "beats.txt"))
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

// TestCLIHealthNamesWhatIsMissing checks the setup failures one at a time, because each has
// a different fix and a shared "unavailable" would leave a user guessing which path is wrong.
func TestCLIHealthNamesWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "sd-cli.txt")
	if err := os.WriteFile(notExecutable, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("no binary configured", func(t *testing.T) {
		err := NewCLI("images", "sd-cli", "", ModelFiles{Model: "/models/fox.gguf"}, Options{}).Health(t.Context())
		if !errors.Is(err, ErrNotConfigured) {
			t.Errorf("err = %v, want ErrNotConfigured", err)
		}
	})

	t.Run("the binary is missing", func(t *testing.T) {
		err := NewCLI("images", "sd-cli", filepath.Join(dir, "nope"), ModelFiles{Model: "/models/fox.gguf"}, Options{}).
			Health(t.Context())
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v, want ErrUnavailable", err)
		}
	})

	t.Run("the binary cannot be executed", func(t *testing.T) {
		err := NewCLI("images", "sd-cli", notExecutable, ModelFiles{Model: "/models/fox.gguf"}, Options{}).
			Health(t.Context())
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "nicht ausführbar") {
			t.Errorf("err = %v, want a message about the missing execute bit", err)
		}
	})

	t.Run("the model file is missing", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		err := NewCLI("images", "sd-cli", binary, ModelFiles{VAE: filepath.Join(dir, "no-vae.safetensors")}, Options{}).
			Health(t.Context())
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no-vae.safetensors") {
			t.Errorf("err = %v, want the missing VAE path named", err)
		}
	})

	t.Run("no model at all", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		if err := NewCLI("images", "sd-cli", binary, ModelFiles{}, Options{}).Health(t.Context()); err == nil {
			t.Error("a binary with no model was reported healthy; a draw would fail later")
		}
	})

	t.Run("a working setup", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		model := filepath.Join(dir, "fox.gguf")
		if err := os.WriteFile(model, []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := NewCLI("images", "sd-cli", binary, ModelFiles{Model: model}, Options{}).Health(t.Context()); err != nil {
			t.Errorf("Health: %v", err)
		}
	})

	t.Run("both model slots filled", func(t *testing.T) {
		binary := fakeCLI(t, cliRecorder)
		model := filepath.Join(dir, "flux.gguf")
		if err := os.WriteFile(model, []byte("weights"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The file exists, so nothing is wrong with the paths — the *request* is. `-m` and
		// `--diffusion-model` are one slot, and a command line carrying both draws with
		// whichever the parser reads last while the panel names the other.
		err := NewCLI("images", "sd-cli", binary,
			ModelFiles{Model: model, DiffusionModel: model}, Options{}).Health(t.Context())
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "nur eines") {
			t.Errorf("err = %v, want a message naming the two model slots", err)
		}
	})
}

// TestCLIRefusesTwoDiffusionModels is the same guard on the path with no settings button
// behind it: the chat model's tool call. A draw that ran here would spend minutes loading a
// model the panel does not name, so the refusal has to arrive before the process starts.
func TestCLIRefusesTwoDiffusionModels(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary,
		ModelFiles{Model: "/models/flux.gguf", DiffusionModel: "/models/flux.gguf"}, Options{})

	_, err := client.Generate(t.Context(), Request{Prompt: "a psyduck using waterguns"})
	if err == nil {
		t.Fatal("a draw ran with two diffusion models on one command line")
	}
	if !strings.Contains(err.Error(), "nur eines") {
		t.Errorf("err = %v, want the contradiction named", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(binary), "args.txt")); statErr == nil {
		t.Error("the binary was started despite the contradiction")
	}
}

// TestCLIHealthToleratesABinaryThatRefusesDashH documents a deliberate limit: the probe
// proves the process *starts*, not that it answers `-h`. A build that has dropped or renamed
// that option can still draw, and refusing it over a usage flag would block a working setup
// — the failure mode of every over-eager "test connection" button.
func TestCLIHealthToleratesABinaryThatRefusesDashH(t *testing.T) {
	binary := fakeCLI(t, `
echo "unknown option" >&2
exit 2
`)
	model := filepath.Join(filepath.Dir(binary), "fox.gguf")
	if err := os.WriteFile(model, []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewCLI("images", "sd-cli", binary, ModelFiles{Model: model}, Options{}).Health(t.Context()); err != nil {
		t.Errorf("a binary that rejects -h must still count as usable: %v", err)
	}
}

// TestCLIEmptyPromptIsRefused keeps the rule the HTTP client also follows: an empty prompt is
// not a degenerate draw, it is a blank canvas the server charges for. It is checked here
// because the argv would otherwise contain `-p ""` and the binary would happily draw
// something arbitrary.
func TestCLIEmptyPromptIsRefused(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	if _, err := client.Generate(t.Context(), Request{Prompt: "   "}); err == nil {
		t.Fatal("a blank prompt was drawn")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "args.txt")); err == nil {
		t.Error("the binary was started for a blank prompt")
	}
}

// TestStepFromLogReadsTheSamplingBarAndNotTheLoadBar pins the reading that live progress
// depends on, and it is pinned because the obvious pattern is wrong: the program draws the
// *same* bar while it loads its tensors, so matching `n/m` alone reported the loader's 6 of
// 265 as the step counter of a two-step draw. That is measured rather than imagined, and the
// rate is what tells the two apart — a sampling bar names a per-step time, a load bar names a
// throughput.
func TestStepFromLogReadsTheSamplingBarAndNotTheLoadBar(t *testing.T) {
	cases := []struct {
		line  string
		step  int
		steps int
		want  bool
	}{
		{"  |=====>              | 3/12 - 2.20s/it", 3, 12, true},
		// The loader's own bar, which is the whole reason the pattern carries a rate.
		{"  |#####                | 6/265 - 4.18GB/s", 0, 0, false},
		{"  |==========>          | 2/12 - 0.53it/s", 2, 12, true},
		// A total of zero is not a fraction, and dividing by it must not reach the panel.
		{"  |=====>              | 3/0 - 2.20s/it", 0, 0, false},
		{"loading tensors completed, taking 2.11s", 0, 0, false},
		{"generating image: 1/1 - seed 4242", 0, 0, false},
	}
	for _, tc := range cases {
		step, steps, ok := stepFromLog(tc.line)
		if ok != tc.want {
			t.Errorf("stepFromLog(%q) matched = %v, want %v", tc.line, ok, tc.want)
			continue
		}
		if !ok {
			continue
		}
		if step != tc.step || steps != tc.steps {
			t.Errorf("stepFromLog(%q) = %d/%d, want %d/%d", tc.line, step, steps, tc.step, tc.steps)
		}
	}
}

// TestSeedFromLogNeedsTheWholeLine is a bug the loose version would have shipped: the prompt
// is echoed into the same log, so a draw of "a robin's egg, seed 999" recorded 999 as the seed
// that produced the picture — and the recipe then described an image nobody had drawn, which
// is exactly the failure the seed line exists to prevent.
func TestSeedFromLogNeedsTheWholeLine(t *testing.T) {
	if got := seedFromLog("generating image: 1/1 - seed 4242"); got != 4242 {
		t.Errorf("seed = %d, want 4242 from the pipeline's own line", got)
	}
	if got := seedFromLog("prompt: a robin's egg, seed 999"); got != 0 {
		t.Errorf("seed = %d, want no seed: the word appearing in a prompt is not a seed line", got)
	}
	// A batch prints one line per image, and the batch above is always one picture.
	if got := seedFromLog("generating image: 1/2 - seed 111\ngenerating image: 2/2 - seed 222"); got != 222 {
		t.Errorf("seed = %d, want the last line's 222", got)
	}
}

// cliWithPreview is a fake `sd-cli` that answers `-h` with a usage text naming the preview
// options — which is the only thing that makes this client pass them, so the stub has to have
// it for the feature to be exercised at all.
const cliWithPreview = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --preview <method>        preview decoding method (none, vae, tae)"
  echo "  --preview-path <path>     write the preview to this file"
  echo "  --preview-interval <n>    how often to update it"
  exit 0
fi
out=""; prev=""
printf '%s\n' "$@" > "$(dirname "$0")/args.txt"
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --preview-path) prev="$2"; shift 2 ;;
    --preview|--preview-interval) shift 2 ;;
    *) shift ;;
  esac
done
echo "  |=====>              | 1/2 - 2.20s/it"
cp "$(dirname "$0")/picture.png" "$prev"
sleep 1.5
echo "  |==========>          | 2/2 - 2.10s/it"
cp "$(dirname "$0")/picture.png" "$prev"
sleep 1.5
cp "$(dirname "$0")/picture.png" "$out"
echo "generating image: 1/1 - seed 4242"
exit 0
`

// TestCLIReportsStepsAndAPreviewWhileItDraws is the CLI half of live progress, and it is an
// end-to-end test on purpose: the two sources are the program's own — a bar it redraws into
// its log and a file it rewrites on every step — and only running a process demonstrates that
// either is read at the right moment rather than after the draw has finished.
//
// It also pins the flags. `--preview-path` is what makes the file exist at all, and it is
// passed only because this build's own help lists it.
func TestCLIReportsStepsAndAPreviewWhileItDraws(t *testing.T) {
	binary := fakeCLI(t, cliWithPreview)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	var reports []Progress
	result, err := client.GenerateWithProgress(t.Context(), Request{Prompt: "a red fox"}, func(p Progress) {
		reports = append(reports, p)
	})
	if err != nil {
		t.Fatalf("GenerateWithProgress: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("a watched draw reported nothing at all")
	}
	if len(reports) < 2 {
		t.Errorf("only %d report(s) arrived; the step counter is meant to move: %+v", len(reports), reports)
	}

	args := argsOf(t, binary)
	if !strings.Contains(strings.Join(args, " "), "--preview-path") {
		t.Fatalf("the preview path was never passed, so the file this client polls cannot exist:\n%s", strings.Join(args, " "))
	}

	first := reports[0]
	if first.Step != 1 || first.Steps != 2 {
		t.Errorf("first report = %d/%d, want 1/2 from the program's own bar", first.Step, first.Steps)
	}
	if len(first.Preview) == 0 || first.PreviewMime != MimePNG {
		t.Errorf("first report carries no usable preview: %d bytes, mime %q", len(first.Preview), first.PreviewMime)
	}
	last := reports[len(reports)-1]
	if last.Step != 2 {
		t.Errorf("last report = %d/%d, want the second step — a counter that never moves is not progress", last.Step, last.Steps)
	}
	// The draw itself is unaffected by having been watched, down to the seed the recipe
	// records: the log is read line by line now rather than collected at the end.
	if result.Seed != 4242 {
		t.Errorf("seed = %d, want the 4242 the log reports", result.Seed)
	}
	if len(result.Images) != 1 {
		t.Errorf("images = %d, want the one picture the run wrote", len(result.Images))
	}
	if !strings.Contains(result.Info, "generating image:") {
		t.Errorf("Result.Info = %q, want the report lines worth keeping", result.Info)
	}
}

// cliWithCarriageReturnBars is the shape the real program writes, and the shape that used to
// freeze the step counter: a step is a carriage return (it redraws the same bar in place),
// a pass ends with a newline, and the preview file is written once rather than every step.
//
// Both halves are measured on stable-diffusion.cpp `master-959-d637cfb`: a scanner split on
// `\n` alone delivered a whole pass as one line, `stepFromLog` read the *first* bar out of
// it, and the window showed "step 1 of 3" once the pass was already over.
const cliWithCarriageReturnBars = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --preview <method>        preview decoding method"
  echo "  --preview-path <path>     write the preview to this file"
  echo "  --preview-interval <n>    how often to update it"
  exit 0
fi
out=""; prev=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --preview-path) prev="$2"; shift 2 ;;
    --preview|--preview-interval) shift 2 ;;
    *) shift ;;
  esac
done
printf '  |=====>    | 1/3 - 2.20s/it\r'
sleep 1
printf '  |=======>  | 2/3 - 2.20s/it\r'
sleep 1
printf '  |========> | 3/3 - 2.20s/it\r'
cp "$(dirname "$0")/picture.png" "$prev"
sleep 1
printf '\n'
cp "$(dirname "$0")/picture.png" "$out"
echo "generating image: 1/1 - seed 4242"
exit 0
`

// TestCLIReportsEveryStepEvenWithoutANewPreview is the bug a user reported as "the step
// progress no longer increases": the counter was only reported when the preview file
// changed, and a real build writes that file once per pass rather than once per step. A
// frozen picture therefore froze the whole row.
//
// Two assertions, and they are the two halves of the fix: the steps advance although the
// preview file stood still, and at least one report carries no preview at all — a report
// that is a step and nothing else.
func TestCLIReportsEveryStepEvenWithoutANewPreview(t *testing.T) {
	binary := fakeCLI(t, cliWithCarriageReturnBars)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	var reports []Progress
	if _, err := client.GenerateWithProgress(t.Context(), Request{Prompt: "a red fox"}, func(p Progress) {
		reports = append(reports, p)
	}); err != nil {
		t.Fatalf("GenerateWithProgress: %v", err)
	}
	if len(reports) < 2 {
		t.Fatalf("only %d report(s) arrived; the bar is redrawn with a carriage return and each "+
			"one is a step: %+v", len(reports), reports)
	}
	stepless := false
	for _, report := range reports {
		if report.Step > 0 && len(report.Preview) == 0 {
			stepless = true
		}
	}
	if !stepless {
		t.Errorf("no report carried a step without a preview; the counter is still waiting for the "+
			"picture file: %+v", reports)
	}
	last := reports[len(reports)-1]
	if last.Step != 3 || last.Steps != 3 {
		t.Errorf("last report = %d/%d, want 3/3 from the bars the program wrote with carriage "+
			"returns", last.Step, last.Steps)
	}
}

// TestTheRealStepBarIsReadThroughItsCarriageReturns pins the exact bytes the measurement
// produced, not a paraphrase of them: a bar redrawn with `\r`, an ANSI erase-in-line after
// each one, an interleaved info line, and the second bar's rate written as `it/s` rather
// than `s/it`. Read as one newline-terminated line it yields step 1 twice; read through
// splitLogLines it yields 1 then 2, which is what makes the counter move.
func TestTheRealStepBarIsReadThroughItsCarriageReturns(t *testing.T) {
	const log = "\r  |=========================>                        | 1/2 - 10.93s/it\x1b[K" +
		"[I] qwen_image_2_1 build cached graph cut plan done (taking 2 ms) --- ggml_graph_cut.cpp:1032" +
		"\r  |==================================================| 2/2 - 3.28it/s\x1b[K\n"
	scanner := bufio.NewScanner(strings.NewReader(log))
	scanner.Split(splitLogLines)
	var steps []int
	for scanner.Scan() {
		if step, _, ok := stepFromLog(scanner.Text()); ok {
			steps = append(steps, step)
		}
	}
	if len(steps) != 2 || steps[0] != 1 || steps[1] != 2 {
		t.Fatalf("steps read = %v, want [1 2] — one per carriage return", steps)
	}
}

// TestTheVAEDecodeBarIsNotASamplingStep pins the second half of the measured log shape. A
// tiled decode prints `|…| 24/54 - 27.03it/s` — the same bar the sampler prints — after the
// last sampling pass has already finished, so a shape-only parser moved the row from
// "step 5 of 5" to "step 24 of 54". The program says `decoding 1 latents` before it and
// `latent 1 decoded` after, and that is what separates the two.
func TestTheVAEDecodeBarIsNotASamplingStep(t *testing.T) {
	log := &cliLog{pass: 1, passes: 2}
	for _, line := range []string{
		"  |=====>    | 1/5 - 2.20s/it",
		"  |==========| 5/5 - 2.20s/it",
		"[I] sampling completed, taking 11.23s --- image.cpp:899",
		"[I] hires sampling 1/1 completed, taking 27.56s --- image.cpp:1021",
		"[I] decoding 1 latents --- image.cpp:554",
		"  |=========================>  | 24/54 - 27.03it/s",
		"  |============================| 54/54 - 27.03it/s",
		"[I] latent 1 decoded, taking 2.22s --- image.cpp:620",
	} {
		log.write(line)
	}
	step, steps, _, _ := log.progress()
	if step != 5 || steps != 5 {
		t.Fatalf("progress after the decode = %d/%d, want the last sampling step 5/5", step, steps)
	}
}

// cliWithHires is a fake `sd-cli` whose own help lists the second pass and the VAE tiling
// switches, which is the only thing that makes this client pass them.
const cliWithHires = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --hires                   enable a second sampling pass"
  echo "  --hires-scale <n>         how much larger the output is"
  echo "  --hires-width <n>         target width of the second pass"
  echo "  --hires-height <n>        target height of the second pass"
  echo "  --hires-denoising-strength <n>  denoise strength of the second pass"
  echo "  --vae-tiling              decode the latent in tiles"
  echo "  --vae-tile-size <n>       tile size for the VAE"
  exit 0
fi
` + cliRecorder

// cliWithHiresButNoSize is the fork shape upstream never ships: `--hires` is listed, and
// neither a scale nor a target beside it. `--hires` alone is a second pass at whatever the
// program's own `hires_scale` default is, which is 2 upstream and 1.5 in this app — so the
// picture is real, the recipe records a different one, and only a refusal up front can tell
// the two apart before minutes of sampling.
const cliWithHiresButNoSize = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --hires                   enable a second sampling pass"
  echo "  --hires-denoising-strength <n>  denoise strength of the second pass"
  exit 0
fi
` + cliRecorder

// TestTheHelpProbeMatchesAFlagAsAWholeName is the substring trap: `--hires` is inside
// `--hires-upscalers-dir`, which a build can list while lacking the second pass itself. A
// probe that answered yes from a substring would then pass `--hires` to a program that does
// not know it, and an unknown option fails the whole run on this backend. The punctuation
// cases are here because usage lines spell a flag as `--hires,` or `(--hires)`.
func TestTheHelpProbeMatchesAFlagAsAWholeName(t *testing.T) {
	cases := []struct {
		help, flag string
		want       bool
	}{
		{"  --hires-upscalers-dir <dir>  where upscalers live", "--hires", false},
		{"  --hires <n>  enable a second pass", "--hires", true},
		{"  --hires, --hires-scale <n>", "--hires", true},
		{"  --hires-scale=<n>  how much larger", "--hires-scale", true},
		{"  (requires --vae-tiling)", "--vae-tiling", true},
		{"  --preview-path <path>", "--preview", false},
		{"", "--hires", false},
	}
	for _, tc := range cases {
		if got := helpLists(tc.help, tc.flag); got != tc.want {
			t.Errorf("helpLists(%q, %q) = %v, want %v", tc.help, tc.flag, got, tc.want)
		}
	}
}

// TestTheLogReadsASecondBarAsTheSecondPass pins the one signal that tells a hires row the
// counter did not go backwards. The program prints one bar per sampling pass and numbers each
// from 1, so the pass is inferred from a step that is lower than the one before it — and only
// while the draw has another pass to enter. A one-pass draw that reprints a stale number must
// stay on pass 1 rather than invent a pass that will never run.
func TestTheLogReadsASecondBarAsTheSecondPass(t *testing.T) {
	hires := &cliLog{pass: 1, passes: 2}
	hires.write("  |=====>    | 4/4 - 2.20s/it")
	hires.write("  |=>        | 1/4 - 2.20s/it")
	if step, steps, pass, passes := hires.progress(); step != 1 || steps != 4 || pass != 2 || passes != 2 {
		t.Errorf("hires progress = %d/%d pass %d/%d, want 1/4 pass 2/2", step, steps, pass, passes)
	}

	one := &cliLog{pass: 1, passes: 1}
	one.write("  |=====>    | 4/4 - 2.20s/it")
	one.write("  |=>        | 1/4 - 2.20s/it")
	if _, _, pass, passes := one.progress(); pass != 1 || passes != 1 {
		t.Errorf("one-pass progress = pass %d/%d, want the only pass", pass, passes)
	}
}

// TestCLIAsksForTheSecondPassAndTheVaeSwitches pins the two groups of flags this client only
// passes when the build's own help lists them. A build without `--hires` is refused rather
// than drawn at base size — that is the next test — while a build without the VAE switches
// simply loses them, because tiling is a comfort for a machine rather than a property of the
// picture.
func TestCLIAsksForTheSecondPassAndTheVaeSwitches(t *testing.T) {
	binary := fakeCLI(t, cliWithHires)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"},
		Options{VaeTiling: true, VaeTileSize: 512})

	if _, err := client.Generate(t.Context(), Request{
		Prompt: "a red fox",
		Width:  768, Height: 512,
		Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	args := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{
		"--hires", "--hires-scale 1.5", "--hires-denoising-strength 0.4",
		"--hires-width 1152", "--hires-height 768",
		"--vae-tiling", "--vae-tile-size 512",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the command line lacks %q:\n%s", want, args)
		}
	}
}

// TestCLIAsksForTheAlignedOutputSize is the reason the target is passed at all: the base
// size times the scale is rarely already a latent multiple, and the program's own rounding is
// not the number the recipe records. 520 × 1.25 is 650, which `Request.OutputSize` aligns up
// to 656; a command line carrying only `--hires-scale 1.25` would leave the program to pick
// 648 or 656, and `verifyHiresOutput` would then disagree with a second pass that did happen.
func TestCLIAsksForTheAlignedOutputSize(t *testing.T) {
	binary := fakeCLI(t, cliWithHires)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	if _, err := client.Generate(t.Context(), Request{
		Prompt: "a red fox",
		Width:  520, Height: 520,
		Hires: Hires{Enabled: true, Scale: 1.25, Denoise: 0.4},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	args := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{
		"--hires-width 656", "--hires-height 656", "--hires-scale 1.25",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("the command line lacks %q:\n%s", want, args)
		}
	}
}

// TestCLIRefusesASecondPassItsBuildCannotDraw is the difference between an ignored flag and a
// wrong picture: a build whose help does not list `--hires` would draw at base size and exit
// 0, and the recipe would then claim an enlargement that never happened. The refusal comes
// before the drawing process starts, which is what the recorded command line shows.
func TestCLIRefusesASecondPassItsBuildCannotDraw(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	_, err := client.Generate(t.Context(), Request{
		Prompt: "a red fox",
		Hires:  Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	})
	if !errors.Is(err, ErrHiresUnsupported) {
		t.Fatalf("Generate = %v, want ErrHiresUnsupported", err)
	}
	if !strings.Contains(err.Error(), "--hires") {
		t.Errorf("the error does not name the option the build is missing: %v", err)
	}
	if args := argsOf(t, binary); len(args) != 1 || args[0] != "-h" {
		t.Errorf("a draw was started anyway: the last command line was %v", args)
	}
}

// TestCLIRefusesASecondPassItCannotSize is the other half of that refusal: a build that lists
// `--hires` but none of the four number options would draw a second pass of its own choosing,
// and `verifyHiresOutput` would then refuse the result after the sampling it took to produce
// it. Both failures are the same fact — this build cannot express the enlargement the recipe
// asks for — so the refusal happens before the draw, which is what the absent command line
// shows: the fake records one for every run except its own `-h`.
func TestCLIRefusesASecondPassItCannotSize(t *testing.T) {
	binary := fakeCLI(t, cliWithHiresButNoSize)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	_, err := client.Generate(t.Context(), Request{
		Prompt: "a red fox",
		Width:  768, Height: 512,
		Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	})
	if !errors.Is(err, ErrHiresUnsupported) {
		t.Fatalf("Generate = %v, want ErrHiresUnsupported", err)
	}
	for _, want := range []string{"--hires-scale", "--hires-width", "--hires-height"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "args.txt")); err == nil {
		t.Errorf("a draw was started anyway: %v", argsOf(t, binary))
	}
}

// cliWithHeldDraw is a fake `sd-cli` that draws until the test lets it finish: it reports its
// first bar and preview at once and then waits for a file to appear beside the script. A draw
// whose end the test chooses is what makes the window below a window rather than a
// coincidence — the same stub as cliWithPreview with its sleeps replaced by that wait.
const cliWithHeldDraw = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --preview <method>        preview decoding method (none, vae, tae)"
  echo "  --preview-path <path>     write the preview to this file"
  echo "  --preview-interval <n>    how often to update it"
  exit 0
fi
out=""; prev=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --preview-path) prev="$2"; shift 2 ;;
    --preview|--preview-interval) shift 2 ;;
    *) shift ;;
  esac
done
echo "  |=====>              | 1/2 - 2.20s/it"
cp "$(dirname "$0")/picture.png" "$prev"
while [ ! -f "$(dirname "$0")/finish" ]; do sleep 0.05; done
cp "$(dirname "$0")/picture.png" "$out"
echo "generating image: 1/1 - seed 4242"
exit 0
`

// TestCLIDoesNotReportAfterTheDrawIsOver is the promise the test above only made visible under
// `-race`: a progress report belongs to the call that was given the callback, so none may still
// be running — or arrive — once GenerateWithProgress has returned.
//
// The watcher used to be stopped with a bare `close(stopWatch)`, which asks it to stop without
// waiting for it, and the last report of a draw could therefore finish after the draw's own
// caller was gone. On this backend that is not only a test's problem: the callback the app
// passes emits `image:progress`, so a finished draw kept repainting its own progress row, and
// the shared slice in the test above — a plain append from another goroutine — was a data race.
//
// The report is held open *across the moment the process exits*, which is the window the race
// lived in: the fake draw ends when the test says so, the callback stays inside the report for a
// further two seconds, and a client that returns in between is the bug. The release cannot come
// from the test's own flow — a client that waits for the report it is running would then be
// waiting for a report that waits for it — so a watchdog does it, and both a fixed and a buggy
// client get to finish.
func TestCLIDoesNotReportAfterTheDrawIsOver(t *testing.T) {
	binary := fakeCLI(t, cliWithHeldDraw)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()

	started := make(chan struct{})
	var startedOnce sync.Once
	var running atomic.Int32

	go func() {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			// The draw is held open below, so a report that never came would otherwise be
			// a hang rather than a failure: the stub is let go either way.
			t.Errorf("the held draw reported nothing, so the window this test needs never opened")
		}
		// End the draw while the report below is still being delivered. The script leaves
		// within one poll of this file appearing, so the client's return and the end of the
		// report are far apart in time — which is what the margin below is for.
		if err := os.WriteFile(filepath.Join(filepath.Dir(binary), "finish"), []byte("done"), 0o644); err != nil {
			t.Errorf("let the fake draw finish: %v", err)
		}
		time.Sleep(2 * time.Second)
		letGo()
	}()

	if _, err := client.GenerateWithProgress(t.Context(), Request{Prompt: "a red fox"}, func(Progress) {
		running.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		running.Add(-1)
	}); err != nil {
		t.Fatalf("GenerateWithProgress: %v", err)
	}

	if n := running.Load(); n != 0 {
		t.Errorf("%d progress report(s) were still running when the draw returned; a report may not "+
			"outlive the call that asked for it", n)
	}
}

// TestCLIDoesNotPreviewForABuildThatHasNoSuchFlag is the reason the flags are asked about
// rather than assumed: an unknown option fails the *whole run* on this binary, so passing
// `--preview-path` to a build that predates it would turn "no preview" into "no picture".
// The command line is what the assertion is on, because that is where the mistake lives.
func TestCLIDoesNotPreviewForABuildThatHasNoSuchFlag(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	if _, err := client.GenerateWithProgress(t.Context(), Request{Prompt: "a fox"}, func(Progress) {}); err != nil {
		t.Fatalf("GenerateWithProgress: %v", err)
	}
	if joined := strings.Join(argsOf(t, binary), " "); strings.Contains(joined, "--preview") {
		t.Errorf("preview flags were passed to a build whose help does not list them:\n%s", joined)
	}
}

// cliWithMemoryFlags is a build that lists the two switches a large draw needs to stay on
// the GPU, which is what makes the client add them.
const cliWithMemoryFlags = `
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  --hires                   enable a second sampling pass"
  echo "  --hires-scale <n>         how much larger the output is"
  echo "  --hires-width <n>         target width of the second pass"
  echo "  --hires-height <n>        target height of the second pass"
  echo "  --hires-denoising-strength <n>  denoise strength of the second pass"
  echo "  --vae-tiling              decode the latent in tiles"
  echo "  --vae-tile-size <n>       tile size for the VAE"
  echo "  --diffusion-fa            flash attention in the diffusion model"
  exit 0
fi
` + cliRecorder

// TestALargeDrawKeepsItsWorkOnTheGPU is the measurement the thresholds in memoryArgs come
// from. A 1600×1216 second pass on a 20 GB card allocated an attention buffer that did not
// fit, printed `ggml_vulkan: Failed to allocate pinned memory`, and sampled on eight CPU
// threads; the final decode wanted a 10 GB VAE buffer and spent 55.75 s on the CPU against
// 7.75 s tiled on the GPU. Both switches only decide *how* the same picture is computed.
func TestALargeDrawKeepsItsWorkOnTheGPU(t *testing.T) {
	binary := fakeCLI(t, cliWithMemoryFlags)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	if _, err := client.Generate(t.Context(), Request{
		Prompt: "a red fox",
		Width:  1024,
		Height: 1024,
		Hires:  Hires{Enabled: true, Scale: 2, Denoise: 0.4},
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	joined := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{"--vae-tiling", "--diffusion-fa"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a 2048×2048 draw was not given %s:\n%s", want, joined)
		}
	}
}

// TestASmallDrawIsLeftAlone is the other half of the rule: the flags are for a picture that
// does not fit, and a draw that fits keeps the build's own defaults.
func TestASmallDrawIsLeftAlone(t *testing.T) {
	binary := fakeCLI(t, cliWithMemoryFlags)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	if _, err := client.Generate(t.Context(), Request{Prompt: "a red fox", Width: 512, Height: 512}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	joined := strings.Join(argsOf(t, binary), " ")
	for _, unwanted := range []string{"--vae-tiling", "--diffusion-fa"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a 512×512 draw was given %s, which it does not need:\n%s", unwanted, joined)
		}
	}
}

// TestAUserFlashAttentionChoiceWins keeps the extra arguments authoritative: the app's own
// flags are appended after them, so an added `--diffusion-fa` would outvote a `--fa` the
// user wrote — the opposite of what that field is for.
func TestAUserFlashAttentionChoiceWins(t *testing.T) {
	binary := fakeCLI(t, cliWithMemoryFlags)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"},
		Options{Extra: []string{"--fa"}})

	if _, err := client.Generate(t.Context(), Request{Prompt: "a red fox", Width: 1024, Height: 1024}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if joined := strings.Join(argsOf(t, binary), " "); strings.Contains(joined, "--diffusion-fa") {
		t.Errorf("the client added --diffusion-fa beside the user's own --fa:\n%s", joined)
	}
}

// TestCLILeavesThePreviewAloneIfTheUserSetIt is the same rule as every other argument in this
// client: the user's own command line comes first and wins. There is no control for the
// preview method or its interval, so appending ours would silently overrule what they wrote —
// and an unwatched draw is a far smaller loss than a draw with the wrong method.
func TestCLILeavesThePreviewAloneIfTheUserSetIt(t *testing.T) {
	binary := fakeCLI(t, cliWithPreview)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"},
		Options{Extra: []string{"--preview", "none"}})

	if _, err := client.GenerateWithProgress(t.Context(), Request{Prompt: "a fox"}, func(Progress) {}); err != nil {
		t.Fatalf("GenerateWithProgress: %v", err)
	}
	if joined := strings.Join(argsOf(t, binary), " "); strings.Contains(joined, "--preview-path") {
		t.Errorf("the client overrode the user's own preview choice:\n%s", joined)
	}
}

// TestCLIListsAreNeverNull is the bug this codebase has shipped twice: a nil Go slice
// marshals to JSON `null`, not `[]`, and the settings panel iterates whatever it gets. This
// backend has no listing route and no sampler table to ask for, which makes it the most
// likely place for the empty case to be reached for real.
func TestCLIListsAreNeverNull(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})

	models, err := client.Models(t.Context())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	samplers, err := client.Samplers(t.Context())
	if err != nil {
		t.Fatalf("Samplers: %v", err)
	}

	raw, err := json.Marshal(struct {
		Models   []Model  `json:"models"`
		Samplers []string `json:"samplers"`
	}{models, samplers})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"models":[]`) {
		t.Errorf("the model list is not an empty array: %s", raw)
	}
	// Samplers are the binary's own names rather than nothing: they are a constant of the
	// program, and offering them is what makes the field usable without a manual.
	if len(samplers) == 0 || !strings.Contains(string(raw), `"samplers":["euler"`) {
		t.Errorf("the sampler list should name the binary's own sampling methods: %s", raw)
	}
	if strings.Contains(string(raw), "Euler a") {
		t.Errorf("an A1111 sampler name leaked into the CLI list, which the binary would reject: %s", raw)
	}
}

// The `-h` probe is memoised for the life of the client, so it must not be cancelled with the
// draw that happened to ask first. A draw is exactly what a user stops, and a probe cancelled
// by that stop would be remembered as "this build cannot answer" — and previewSupport reads a
// failed probe as "no preview options", which would quietly switch the progress display off
// for the whole session on a build that has it.
func TestTheHelpProbeSurvivesAStoppedDraw(t *testing.T) {
	binary := fakeCLI(t, `
if [ "$1" = "-h" ]; then
  echo "--preview --preview-path PATH"
  exit 0
fi
echo "not a draw"
`)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/fox.gguf"}, Options{})
	// A cancelled ancestor, which is what the draw's context looks like the moment Stop is
	// pressed while the first picture is being asked for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !client.previewSupport(ctx, binary) {
		t.Fatal("a stopped draw's cancelled context was recorded as a build without previews")
	}
	// And the memo really holds the answer, rather than the second question having succeeded
	// where the first failed.
	if help, err := client.help(context.Background(), binary); err != nil || !strings.Contains(help, "--preview") {
		t.Fatalf("help = %q, err = %v, want the build's own option table", help, err)
	}
}

// TestDrawTimeoutFollowsThePicture pins the bound that a CPU fallback needs. The flat
// DefaultCLITimeout was written for a draw that fits on the card; a picture large enough that
// the backend continues on the CPU is hours rather than minutes, and a bound that did not
// follow the size would kill it after the sampling had already been paid for. It is a guard
// against a wedged process, so it is also capped rather than unbounded.
func TestDrawTimeoutFollowsThePicture(t *testing.T) {
	base := DefaultCLITimeout

	// One megapixel is what the flat bound is written for, and anything under it keeps it.
	for _, req := range []Request{
		{},
		{Width: 512, Height: 512},
		{Width: 1024, Height: 1024},
	} {
		if got := DrawTimeout(base, req); got != base {
			t.Errorf("DrawTimeout(%dx%d) = %s, want the base %s", req.Width, req.Height, got, base)
		}
	}

	// The FHD/4K recipe: 1920x1080 at 2x. 8.35 megapixels is eight times the base within a
	// minute, and that is the cap — so this is as long as a bound may get.
	uhd := Request{Width: 1920, Height: 1088, Hires: Hires{Enabled: true, Scale: 2, Denoise: 0.4}}
	width, height := uhd.OutputSize()
	if got := DrawTimeout(base, uhd); got < MaxDrawTimeout-time.Minute || got > MaxDrawTimeout {
		t.Errorf("DrawTimeout(%dx%d) = %s, want about the cap %s", width, height, got, MaxDrawTimeout)
	}
	// The largest picture this app builds — 2048 a side, doubled — is past the cap and gets
	// exactly it, so the guard has a fixed ceiling rather than a number that keeps growing.
	largest := Request{
		Width:  MaxDimension,
		Height: MaxDimension,
		Hires:  Hires{Enabled: true, Scale: MaxHiresScale, Denoise: 0.4},
	}
	if got, want := DrawTimeout(base, largest), MaxDrawTimeout; got != want {
		t.Errorf("DrawTimeout(%dx%d) = %s, want the cap %s", MaxDimension, MaxDimension, got, want)
	}
	if MaxDrawTimeout <= 3*time.Hour {
		t.Errorf("MaxDrawTimeout = %s, which is not hours of CPU sampling", MaxDrawTimeout)
	}

	// Halfway up: a 2x pass from 1024x1024 is four megapixels, so four base bounds, and the
	// growth is by pixels rather than by long side.
	mid := Request{Width: 1024, Height: 1024, Hires: Hires{Enabled: true, Scale: 2, Denoise: 0.4}}
	if got, want := DrawTimeout(base, mid), 4*base; got != want {
		t.Errorf("DrawTimeout(2048x2048) = %s, want %s", got, want)
	}
}
