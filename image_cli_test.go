package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
	"kichat/internal/images"
	"kichat/internal/store"
	"kichat/internal/tools"
)

// The CLI backend is the second kind of picture backend, and the one thing a test of the
// provider alone cannot show is that the app *selects* it: a provider that draws perfectly
// is invisible if rebuildServices still builds the HTTP client, and the only symptom would
// be a user whose configured binary is never started. So these tests go through the app:
// the settings, the kind switch, the tool, and the recipe that lands in the store.

// cliPicture is a stand-in for a PNG, the same bytes the stop test uses. It is not valid
// base64 itself, so a decode that returned its input would fail a round trip.
var cliPicture = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x7f, 0x3c}

// cliRunSeparator is the line the fake writes between two runs, and the line cliRuns
// splits on. It is a named constant because the two must agree: a helper that split on a
// different string than the script wrote reported every run but the first as absent, which
// is exactly how a redraw that did run looked like one that did not.
const cliRunSeparator = "---"

// cliScript writes an executable that behaves like `sd-cli` in the parts this app depends
// on: it records its command line, copies a picture to the path `-o` names, and logs the
// seed it drew with. A real binary is tens of gigabytes of weights and minutes of loading,
// and the project already tests the llama.cpp runtime against a stub this way.
//
// The command line is appended to rather than overwritten, so a test can read what each of
// several draws was asked for — which is the only way to see that a redraw pinned the seed
// the first draw reported.
//
// `seed` in the script is the seed the log names, and it is a parameter because the two
// behaviours it stands for are different tests: a build that honours `-s` echoes it back,
// and one that ignores it draws with its own.
func cliScript(seed string, body string) string {
	return `#!/bin/sh
# The usage probe is answered and then left, before anything is recorded: the app asks every
# build this question once (images.CLI.previewSupport) and a real sd-cli answers it without
# drawing, so a fake that fell through to the drawing body would both count as a draw in
# args.txt and claim a picture nobody asked for.
if [ "$1" = "-h" ]; then
  echo "usage: sd-cli [options]"
  echo "  -M, --model <path>        model path"
  echo "  -o, --output <path>       output path"
  exit 0
fi
dir="$(dirname "$0")"
printf '%s\n' "$@" >> "$dir/args.txt"
printf -- '` + cliRunSeparator + `\n' >> "$dir/args.txt"
out=""; asked=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -s) asked="$2"; shift 2 ;;
    *) shift ;;
  esac
done
` + body + `
cp "$dir/picture.png" "$out"
echo "loading model from the path it was given"
echo "generating image: 1/1 - seed ` + seed + `"
exit 0
`
}

// fakeSDCLI writes that executable and the picture it copies, and returns the binary's path.
func fakeSDCLI(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sd-cli")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake sd-cli: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "picture.png"), cliPicture, 0o644); err != nil {
		t.Fatalf("write the fake picture: %v", err)
	}
	return path
}

// cliRuns reads back every command line the fake was handed, one []string per run.
func cliRuns(t *testing.T, binary string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "args.txt"))
	if err != nil {
		t.Fatalf("the fake sd-cli was never started: %v", err)
	}
	var runs [][]string
	var current []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == cliRunSeparator {
			if len(current) > 0 {
				runs = append(runs, current)
			}
			current = nil
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	return runs
}

// cliValue returns the value of a flag in one recorded command line, empty when absent.
func cliValue(run []string, flag string) string {
	for i, arg := range run {
		if arg == flag && i+1 < len(run) {
			return run[i+1]
		}
	}
	return ""
}

// cliLastValue returns the *last* occurrence of a flag's value, which is the one that matters
// where a flag appears twice. `--cfg-scale` is the only one that does: the user's extra
// arguments carry it and the value KiChat sets follows, and the point of that order is that
// the later value is the one a duplicate-flag parser keeps.
func cliLastValue(run []string, flag string) string {
	value := ""
	for i, arg := range run {
		if arg == flag && i+1 < len(run) {
			value = run[i+1]
		}
	}
	return value
}

// cliSettings is a configured CLI backend: the Qwen-Image shape from the command line this
// feature was asked for, since that is the set a single-file checkpoint cannot express.
func cliSettings(binary string) config.Settings {
	settings := config.Default()
	settings.Images.Enabled = true
	settings.Images.Kind = config.ImageKindCLI
	settings.Images.CLIBinary = binary
	settings.Images.CLIDiffusionModel = "/models/qwen-image-2.1-Q5_K_M.gguf"
	settings.Images.CLIVAE = "/models/qwen_image_2.1_vae_bf16.safetensors"
	settings.Images.CLILLM = "/models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf"
	settings.Images.CLIExtraArgs = "--diffusion-fa --cfg-scale 1.0"
	// The one mode the panel has a control of its own for. It is set here so the draw test
	// can see it leave the settings and reach the binary, which is the only place that
	// wiring is visible — the provider's own test starts from Options directly.
	settings.Images.CLIOffloadToCPU = true
	settings.Images.Width, settings.Images.Height, settings.Images.Steps = 1024, 1024, 8
	settings.Images.Sampler = "euler"
	// A fixed guidance scale, which is the one shared generation field that is not a whole
	// number. It is set here because this row is the *other* place the scale can come from:
	// the extra arguments above also carry a `--cfg-scale`, and what this test can see is the
	// order the app puts them in.
	settings.Images.CfgScale = 2.5
	return settings
}

// TestTheCLIKindIsWhatTheAppDrawsThrough is the selection itself, at the one place where it
// can be seen from outside: the provider the tool service was handed.
func TestTheCLIKindIsWhatTheAppDrawsThrough(t *testing.T) {
	binary := fakeSDCLI(t, cliScript("4242", ""))
	settings := cliSettings(binary)

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	provider := a.currentImageProvider()
	if provider == nil {
		t.Fatal("no provider was built for an enabled CLI backend")
	}
	if provider.ID() != "images" {
		t.Errorf("provider id = %q, want the shared \"images\" key: the two kinds are one settings section", provider.ID())
	}
	if provider.Name() != "sd-cli" {
		t.Errorf("provider = %q, want the CLI one — the server was built instead", provider.Name())
	}
	if _, ok := provider.(*images.CLI); !ok {
		t.Fatalf("provider is %T, want *images.CLI", provider)
	}

	// And the other kind still builds the other provider, so this is a switch rather than a
	// replacement.
	server := config.Default()
	server.Images.Enabled = true
	server.Images.BaseURL = "http://127.0.0.1:7860"
	a.rebuildServices(server)
	if got := a.currentImageProvider().Name(); got != "Stable Diffusion WebUI" {
		t.Errorf("provider = %q, want the server for a settings row that never chose a kind", got)
	}

	// Off is still nil, which is what removes the tool from the model's list.
	off := config.Default()
	a.rebuildServices(off)
	if a.currentImageProvider() != nil {
		t.Error("a provider was built for a switched-off feature")
	}
}

// TestCLIDrawRecordsTheRecipeAndTheFilesItLoaded is the whole path in one test: the tool
// asks for a picture, the binary is started with the configured model set, the picture is
// filed as an attachment, and the recipe records what a redraw needs.
//
// What it asserts about the command line is deliberately the *shape* — one model set, the
// extra arguments the panel passes through — because the flag-by-flag pinning lives with the
// provider. What only this test can show is that the settings reached it: a `--vae` that
// arrived empty would still draw.
func TestCLIDrawRecordsTheRecipeAndTheFilesItLoaded(t *testing.T) {
	binary := fakeSDCLI(t, cliScript("4242", ""))
	settings := cliSettings(binary)

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	chatID := "chat-cli"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx := tools.WithCallingMessage(t.Context(), "msg-1")
	// The model asks for a guidance scale of its own, which the fixed one in the settings row
	// above has to beat — the whole reason the pin exists is that a model cannot see which
	// weights are installed, and 2.5 here stands in for a `txt_cfg` only the model file knows.
	if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
		`{"prompt":"a cartoon sloth mascot waving","cfg_scale":9}`); err != nil {
		t.Fatalf("generate_image: %v", err)
	}

	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}
	attachment := attachments[0]
	if attachment.Generation == nil {
		t.Fatal("the picture carries no recipe, so nothing could ever be redrawn")
	}
	recipe := *attachment.Generation
	// The seed is the point of choosing it here: the binary's log names one, and the recipe
	// has to carry the number that reproduces the picture rather than the one that was asked
	// for.
	if recipe.Seed != 4242 {
		t.Errorf("recipe seed = %d, want the 4242 the binary reported", recipe.Seed)
	}
	// The model is the set's diffusion model, because this backend loads a set rather than a
	// checkpoint a request can switch to.
	if recipe.Model != "qwen-image-2.1-Q5_K_M.gguf" {
		t.Errorf("recipe model = %q, want the diffusion model's own name", recipe.Model)
	}
	if recipe.Prompt != "a cartoon sloth mascot waving" {
		t.Errorf("recipe prompt = %q", recipe.Prompt)
	}
	// The shared parameters the panel offers for both kinds, asserted on the recipe because
	// that is what a redraw reads back: a size or a step count the settings chose but the
	// picture did not record is a repeat that silently draws something else.
	if recipe.Width != 1024 || recipe.Height != 1024 || recipe.Steps != 8 || recipe.Sampler != "euler" {
		t.Errorf("recipe = %dx%d @%d steps %q, want the picked 1024x1024 @8 euler",
			recipe.Width, recipe.Height, recipe.Steps, recipe.Sampler)
	}
	if recipe.CfgScale != 2.5 {
		t.Errorf("recipe cfg scale = %v, want the fixed 2.5 rather than the model's 9", recipe.CfgScale)
	}

	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(recipeData(t, a, attachment.DocumentID)))
	if err != nil {
		t.Fatalf("decode the stored picture: %v", err)
	}
	if string(data) != string(cliPicture) {
		t.Errorf("the stored bytes are not the picture the binary wrote")
	}

	runs := cliRuns(t, binary)
	if len(runs) != 1 {
		t.Fatalf("the binary was started %d times, want 1", len(runs))
	}
	run := runs[0]
	for flag, want := range map[string]string{
		// The three paths the panel offers for a Flux-style pipeline.
		"--diffusion-model": "/models/qwen-image-2.1-Q5_K_M.gguf",
		"--vae":             "/models/qwen_image_2.1_vae_bf16.safetensors",
		"--llm":             "/models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf",
		// The parameters a user picks for the same pipeline, on the same command line.
		"-W":                "1024",
		"-H":                "1024",
		"--steps":           "8",
		"--sampling-method": "euler",
	} {
		if got := cliValue(run, flag); got != want {
			t.Errorf("%s = %q, want %q\nthe command line was:\n%s", flag, got, want, strings.Join(run, " "))
		}
	}
	if cliValue(run, "-m") != "" {
		t.Errorf("a single-file model was passed for a pipeline of parts:\n%s", strings.Join(run, " "))
	}
	// Offload to CPU is a settings field rather than a recipe field, and this is the only
	// place it can be seen to have travelled from the panel to the command line.
	if !strings.Contains(strings.Join(run, " "), "--offload-to-cpu") {
		t.Errorf("the offload switch did not reach the binary:\n%s", strings.Join(run, " "))
	}
	// The extra arguments come from the panel and reach the binary untouched — this is the
	// only route to `--sigmas`, `--diffusion-fa` and the rest of a moving CLI's surface.
	for _, want := range []string{"--diffusion-fa", "--cfg-scale", "1.0"} {
		if !strings.Contains(strings.Join(run, " "), want) {
			t.Errorf("the user's own %q was dropped:\n%s", want, strings.Join(run, " "))
		}
	}
	// And the guidance scale is the one flag that therefore appears twice: the user's
	// `--cfg-scale 1.0` is still there, and the value that was actually drawn with follows
	// it, which is the order that makes the recipe honest. A user who wrote `--cfg-scale 1.0`
	// for a Flux pipeline keeps it only while nobody asks for another — pin 1.0 in the field
	// above and the model cannot outvote it either.
	if got := cliValue(run, "--cfg-scale"); got != "1.0" {
		t.Errorf("the user's own --cfg-scale = %q, want their 1.0 kept as written", got)
	}
	if got := cliLastValue(run, "--cfg-scale"); got != "2.5" {
		t.Errorf("the last --cfg-scale = %q, want the fixed 2.5 last so that it wins", got)
	}
}

// TestCLIRedrawPinsTheSeedItRecorded is the feature the recipe exists for, checked against
// the one backend where the seed is not the app's own choice: the binary reports one, the
// recipe stores it, and a repeat has to send that number back. A redraw that let the app
// pick a fresh seed would produce a different picture every time and look like a broken
// canvas rather than like a missing pin.
func TestCLIRedrawPinsTheSeedItRecorded(t *testing.T) {
	// The binary ignores what it is asked and draws with its own seed, which is the case that
	// tells the recording from the sending: a recipe carrying 4242 while the command line said
	// something else would be a lie about a reproducible picture.
	binary := fakeSDCLI(t, cliScript("$((asked + 1))", ""))
	settings := cliSettings(binary)

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	chatID := "chat-cli-redraw"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx := tools.WithCallingMessage(t.Context(), "msg-1")
	if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
		`{"prompt":"a red fox in deep snow"}`); err != nil {
		t.Fatalf("generate_image: %v", err)
	}
	attachments, err := a.store.ListAttachments(chatID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments = %d, err = %v", len(attachments), err)
	}
	original := attachments[0]
	if original.Generation == nil {
		t.Fatal("the setup picture carries no recipe")
	}
	first := cliRuns(t, binary)
	if len(first) != 1 {
		t.Fatalf("the binary was started %d times during setup, want 1", len(first))
	}
	reported, err := strconv.ParseInt(cliValue(first[0], "-s"), 10, 64)
	if err != nil {
		t.Fatalf("the fake sent %q for -s: %v", cliValue(first[0], "-s"), err)
	}
	// The binary drew with the seed after the one it was asked for, and the recipe has to
	// report *that* one.
	if got := original.Generation.Seed; got != reported+1 {
		t.Errorf("recipe seed = %d, want the %d the binary said it drew with", got, reported+1)
	}

	// Through the tool service rather than the bound RegenerateImage, which is the same
	// call: the bound method only wraps it in the app's lifetime context and an attachments
	// event, and that event needs the Wails runtime's own context, which a test does not
	// have. What is being checked here is the command line the second draw produced.
	if _, err := a.currentToolService().Regenerate(t.Context(), original.ID, *original.Generation); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	runs := cliRuns(t, binary)
	if len(runs) != 2 {
		t.Fatalf("the binary was started %d times, want 2 — the redraw did not run", len(runs))
	}
	if got := cliValue(runs[1], "-s"); got != strconv.FormatInt(original.Generation.Seed, 10) {
		t.Errorf("the redraw asked for seed %s, want the recorded %d", got, original.Generation.Seed)
	}
	// The guidance scale comes back from the recipe rather than from the settings, which is
	// the same rule the seed follows: a picture is reproduced from what it records. Here the
	// two agree; the point of reading it off the recipe is that a picture drawn before the
	// field was filled in still redraws at its own scale.
	if got := cliLastValue(runs[1], "--cfg-scale"); got != "2.5" {
		t.Errorf("the redraw asked for --cfg-scale %q, want the recorded 2.5", got)
	}

	// And the picture is beside the original: half the point of a repeat is being able to
	// compare it with what it repeated.
	after, err := a.store.ListAttachments(chatID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("attachments after the redraw = %d, want 2", len(after))
	}
}

// TestTheCLIDrawBoundIsTheBackendsOwn is the one number that differs between the two kinds
// and cannot be seen from a fixture: a server holds its model in memory, while the CLI pays
// a load before the first step. A bound carried over unchanged would cut a real draw off
// mid-load and report it as a failure.
func TestTheCLIDrawBoundIsTheBackendsOwn(t *testing.T) {
	cli := config.Default()
	cli.Images.Kind = config.ImageKindCLI
	if got := imageDrawTimeout(cli); got != images.DefaultCLITimeout {
		t.Errorf("CLI bound = %s, want the CLI's own %s", got, images.DefaultCLITimeout)
	}
	server := config.Default()
	if got := imageDrawTimeout(server); got != 10*time.Minute {
		t.Errorf("server bound = %s, want 10m", got)
	}
}

// TestCLIStatusNamesTheFilesAndNeverSendsNull is the panel's side of the CLI kind: it has no
// listing to show, so what it shows instead is the set it will load — and both lists travel
// as arrays, because a nil Go slice marshals to `null` and the panel iterates whatever it
// gets. That has been shipped twice in this codebase.
func TestCLIStatusNamesTheFilesAndNeverSendsNull(t *testing.T) {
	binary := fakeSDCLI(t, cliScript("4242", ""))
	settings := cliSettings(binary)
	// The files the set names do not have to exist for the status to report them; the health
	// probe is what checks that, and it is asserted separately below.
	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.Kind != config.ImageKindCLI {
		t.Errorf("kind = %q, want %q", status.Kind, config.ImageKindCLI)
	}
	if status.Binary != binary {
		t.Errorf("binary = %q, want %q", status.Binary, binary)
	}
	want := []string{
		"/models/qwen-image-2.1-Q5_K_M.gguf",
		"/models/qwen_image_2.1_vae_bf16.safetensors",
		"/models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf",
	}
	if len(status.Files) != len(want) {
		t.Fatalf("files = %v, want %v", status.Files, want)
	}
	for i, path := range want {
		if status.Files[i] != path {
			t.Errorf("files[%d] = %q, want %q", i, status.Files[i], path)
		}
	}
	// A switched-on backend whose model paths are not on this machine is reachable as a
	// binary and still cannot draw, which is the sentence the panel has to print.
	if status.OK || status.Error == "" {
		t.Errorf("a set naming files that do not exist reported ok=%v error=%q", status.OK, status.Error)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"models":[]`, `"samplers":[`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the JSON is missing %s, so the panel receives null and iterates it: %s", want, raw)
		}
	}
}

// sdProgramFolder lays out what a release archive unpacks into — one root directory
// holding the program, its backend library and the picture the stub writes — and returns
// the folder a user would point KiChat at.
func sdProgramFolder(t *testing.T, accelLibrary string) (folder, program string) {
	t.Helper()
	folder = t.TempDir()
	build := filepath.Join(folder, "sd-master-60423ab-bin-Linux-Ubuntu-24.04-x86_64-vulkan")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	program = filepath.Join(build, "sd-cli")
	if err := os.WriteFile(program, []byte(cliScript("4242", "")), 0o755); err != nil {
		t.Fatalf("write the fake sd-cli: %v", err)
	}
	if err := os.WriteFile(filepath.Join(build, "picture.png"), cliPicture, 0o644); err != nil {
		t.Fatalf("write the fake picture: %v", err)
	}
	if accelLibrary != "" {
		if err := os.WriteFile(filepath.Join(build, accelLibrary), []byte("x"), 0o644); err != nil {
			t.Fatalf("write the backend library: %v", err)
		}
	}
	return folder, program
}

// TestTheProgramFolderSuppliesTheDraw is request "point KiChat at the folder you
// downloaded into", played end to end: no program path is stored anywhere, the folder is
// set, and the picture is drawn by the program KiChat found inside it. The detected build
// travels with it, because the folder's own libraries are the only thing that answers
// which accelerator the user actually has once the archive's name is gone.
func TestTheProgramFolderSuppliesTheDraw(t *testing.T) {
	folder, program := sdProgramFolder(t, "libggml-vulkan.so")

	settings := cliSettings("")
	settings.Images.CLIFolder = folder

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.Binary != program {
		t.Errorf("resolved program = %q, want the sd-cli inside the folder (%q)", status.Binary, program)
	}
	if status.Build != images.AcceleratorVulkan {
		t.Errorf("detected build = %q, want vulkan from the folder's own backend library", status.Build)
	}
	if len(status.Builds) == 0 {
		t.Error("no published builds were reported, so the panel cannot say which archive to fetch")
	}

	chatID := "chat-folder"
	if err := a.store.CreateChat(store.Chat{ID: chatID, Title: "Bilder", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	ctx := tools.WithCallingMessage(t.Context(), "msg-1")
	if _, err := a.currentToolService().Execute(ctx, chatID, tools.ToolGenerateImage,
		`{"prompt":"a lovely cat"}`); err != nil {
		t.Fatalf("generate_image: %v", err)
	}
	if runs := cliRuns(t, program); len(runs) != 1 {
		t.Fatalf("the program in the folder was started %d times, want 1", len(runs))
	}
}

// TestAFolderWithoutAProgramIsNotFallenBackFrom pins the precedence rule between the two
// ways of naming the program. A folder is a statement about where it is, so a folder that
// holds no program is a reported failure — not a quiet fallback to an older typed path,
// which would draw with a program nobody chose and look like the folder being ignored.
func TestAFolderWithoutAProgramIsNotFallenBackFrom(t *testing.T) {
	typed := fakeSDCLI(t, cliScript("4242", ""))
	settings := cliSettings(typed)
	settings.Images.CLIFolder = t.TempDir() // it exists, and holds nothing

	a := newSettingsTestApp(t, settings, t.TempDir())
	a.ctx = t.Context()
	a.rebuildServices(settings)

	status, err := a.GetImageStatus()
	if err != nil {
		t.Fatalf("GetImageStatus: %v", err)
	}
	if status.OK || status.Error == "" {
		t.Errorf("a folder with no program reported ok=%v error=%q", status.OK, status.Error)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(typed), "args.txt")); statErr == nil {
		t.Error("the typed program was run even though a folder was set")
	}
}

// recipeData reads the document text of a stored picture, which is its base64 payload.
func recipeData(t *testing.T, a *App, documentID string) string {
	t.Helper()
	document, err := a.store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	return document.Text
}
