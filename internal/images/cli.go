package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file is the second kind of image backend: a local binary run once per picture,
// as opposed to the HTTP server internal/images/sdwebui.go talks to.
//
// The two are not interchangeable and neither replaces the other. A server keeps the
// model loaded, so a second picture costs the sampling alone; a one-shot process pays a
// model load every time — minutes on a CPU for a q5 diffusion model — and in exchange
// needs no daemon, no port, and no setup beyond the path to the binary. It is also the
// only way to reach parts of stable-diffusion.cpp that its A1111 compatibility route does
// not expose per request, such as custom sigma schedules and the separate text-encoder
// and VAE files a newer pipeline needs.
//
// The flags written here come from stable-diffusion.cpp's own option table
// (examples/common/common.cpp). They are spelled exactly as that source declares them,
// including the two that prose about the project habitually gets wrong: the scheduler flag
// is `--scheduler`, not `--schedule`, and the CLIP encoders are `--clip_l`/`--clip_g` with
// underscores. Anything outside this verified set is the user's to pass through
// ExtraArgs, which is what keeps KiChat from chasing a CLI whose options its own README
// says change frequently.

// ModelFiles are the paths one model is made of.
//
// More than one file is the normal case for anything newer than the original SD1.x
// text-to-image pipeline: Qwen-Image, Flux and SD3 each split into a diffusion model, a
// VAE and a text encoder, and the flags that name them are the only way to tell the
// binary what to load.
type ModelFiles struct {
	// Model is a single-file checkpoint, passed as `-m`. It is what the SD1.x and SD2.x
	// GGUFs are, and it is mutually exclusive with the standalone parts below in
	// practice: a pipeline that needs those is a pipeline without a single checkpoint.
	Model string
	// DiffusionModel, VAE and LLM are the standalone parts, passed as
	// `--diffusion-model`, `--vae` and `--llm`.
	DiffusionModel string
	VAE            string
	LLM            string
	// ClipL, ClipG and T5XXL are the separate text encoders some pipelines ask for.
	ClipL string
	ClipG string
	T5XXL string
}

// Paths lists every file this set names, in the order a person would read them. It exists
// so Health and the settings panel can check them all without repeating the list.
func (m ModelFiles) Paths() []string {
	return []string{m.Model, m.DiffusionModel, m.VAE, m.LLM, m.ClipL, m.ClipG, m.T5XXL}
}

// Label names the model for a caption or a tool result: the diffusion model if there is
// one, the checkpoint otherwise, empty when neither is set.
func (m ModelFiles) Label() string {
	for _, path := range []string{m.DiffusionModel, m.Model} {
		if trimmed := strings.TrimSpace(path); trimmed != "" {
			return filepath.Base(trimmed)
		}
	}
	return ""
}

// Empty reports whether the set names no model at all.
func (m ModelFiles) Empty() bool {
	for _, path := range m.Paths() {
		if strings.TrimSpace(path) != "" {
			return false
		}
	}
	return true
}

// DefaultCLITimeout bounds one draw through the CLI backend.
//
// Deliberately longer than the server's, because this backend pays a model load per
// picture on top of the sampling: a 10 GB corpus on a spinning disk is tens of seconds
// before a single step runs. The bound is a guard against a wedged process, not a promise
// to the user — Stop is what ends a draw that is merely slow, and it works here by killing
// the process rather than by asking a server to interrupt.
const DefaultCLITimeout = 30 * time.Minute

// cliSamplerNames are the sampling methods stable-diffusion.cpp's option table lists,
// quoted from `examples/common/common.cpp`.
//
// They are **not** A1111's names — the binary has `euler_a` where a WebUI has `Euler a` —
// which is why the sampler field cannot be shared between the two backends: a name the
// other server accepted would be rejected here. It is a hint rather than a validation, too:
// the project says its options change often, so a name a newer build invented is passed
// through rather than refused.
var cliSamplerNames = []string{
	"euler", "euler_a", "heun", "dpm2", "dpm++2s_a", "dpm++2m", "dpm++2mv2",
	"ipndm", "ipndm_v", "lcm", "ddim_trailing", "tcd", "res_multistep", "res_2s",
	"er_sde", "euler_cfg_pp", "euler_a_cfg_pp", "euler_ge", "dpm++2m_sde",
	"dpm++2m_sde_bt", "lms",
}

// Options are the switches this app models itself, as opposed to the free text in Extra.
//
// It mirrors runtime.Options, which answers the same question for llama-server: a flag
// earns a field here only when it is a *mode* a user flips per model or per machine — one
// they would otherwise hand-edit into a command line and get wrong — while every flag that
// carries a value stays in Extra, where a renamed option costs nothing.
type Options struct {
	// OffloadToCPU passes `--offload-to-cpu`, bare: the sampling runs on the host CPU
	// while the weights stay where they were loaded, which is how Flux is run on a card
	// too small to hold it. Its presence is the whole message, so no value is sent — a
	// build that wants one can be spelled that way in Extra instead.
	//
	// It is not part of a recipe: it changes where the work happens, not what the picture
	// is, so a redraw takes it from settings rather than from the stored picture.
	OffloadToCPU bool
	// Folder is the directory the program and its libraries live in — what a release
	// archive unpacked. When it is set the executable is *found* inside it rather than
	// named, so the only path a user has to get right is the folder they downloaded
	// into; see ResolveProgram. Empty means the explicit Binary is used.
	Folder string
	// Extra is the rest of the user's command line, already split by runtime.SplitArgs.
	// It is placed *before* the arguments this app builds, so a flag that appears in both
	// is won by the value the recipe records — otherwise the panel would show one step
	// count while the binary drew with another, and the recorded recipe would describe a
	// picture that does not exist.
	Extra []string
}

// CLI draws one picture per process through a local `sd-cli`.
type CLI struct {
	id     string
	name   string
	binary string
	folder string
	files  ModelFiles
	opts   Options
	// Timeout bounds one draw; see DefaultCLITimeout.
	Timeout time.Duration
}

// program is the executable one draw runs: the folder's own sd-cli when a folder is
// configured, else the typed path. Both entry points resolve through here, so a broken
// folder is one reported error rather than two behaviours.
func (c *CLI) program() (string, error) { return ResolveProgram(c.folder, c.binary) }

// NewCLI builds a client for one local binary. Options.Extra is the user's extra command
// line, already split by runtime.SplitArgs — a shell-free splitter, because this must
// never hand a string to a shell.
func NewCLI(id, name, binary string, files ModelFiles, opts Options) *CLI {
	return &CLI{
		id:      id,
		name:    name,
		binary:  strings.TrimSpace(binary),
		folder:  strings.TrimSpace(opts.Folder),
		files:   files,
		opts:    opts,
		Timeout: DefaultCLITimeout,
	}
}

func (c *CLI) ID() string   { return c.id }
func (c *CLI) Name() string { return c.name }

// Files exposes the configured set, for the settings panel and its "test" button.
func (c *CLI) Files() ModelFiles { return c.files }

// Health reports what is wrong with the setup, which for this backend is a path rather
// than a connection.
//
// Three checks, and the third is the one that earns its keep: a binary that exists but
// cannot be executed is the failure a user actually makes (a download that lost its mode,
// a `.exe` unpacked from an archive), and it produces a bare "exec format error" from the
// real draw minutes later. Running `-h` proves the process starts at all; its exit code is
// deliberately ignored, because a build that does not answer that option is still a build
// that can draw, and refusing it would block a working setup over a flag.
//
// It does not prove the binary *is* sd-cli — nothing short of a real draw does — and it
// never loads a model, so it is cheap enough for a settings button.
func (c *CLI) Health(ctx context.Context) error {
	binary, err := c.program()
	if err != nil {
		return err
	}
	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %s ist ein Ordner, keine Datei", ErrUnavailable, binary)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%w: %s ist nicht ausführbar", ErrUnavailable, binary)
	}
	if c.files.Empty() {
		return fmt.Errorf("%w: kein Modell angegeben", ErrUnavailable)
	}
	if err := c.modelContradiction(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	for _, path := range c.files.Paths() {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if _, err := os.Stat(trimmed); err != nil {
			return fmt.Errorf("%w: %s", ErrUnavailable, err)
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Output is discarded: `-h` writes its usage to stdout, and a usage text is not a
	// diagnosis. Only "the process could not be started or did not exit" is.
	if err := exec.CommandContext(probeCtx, binary, "-h").Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return fmt.Errorf("%w: %s ließ sich nicht starten: %v", ErrUnavailable, binary, err)
	}
	return nil
}

// modelContradiction reports the one setup this client refuses: both model slots filled.
//
// `-m` and `--diffusion-model` are the *same* slot in the binary's own option table, so a
// command line carrying both asks it to load two diffusion models and leaves the choice to
// whichever the parser reads last — while the panel shows the other one. It is refused
// rather than silently resolved because which of the two the user meant is not this app's to
// guess: dropping one would draw a picture with weights the panel does not name.
//
// Both entry points consult it, and that is the point of sharing it: Health turns it into a
// red badge in Settings before anything is saved, while Generate — the path the chat model's
// tool takes, where nobody pressed a test button — refuses with the same sentence instead of
// spending minutes loading a model the user did not choose.
func (c *CLI) modelContradiction() error {
	if strings.TrimSpace(c.files.Model) != "" && strings.TrimSpace(c.files.DiffusionModel) != "" {
		return errors.New("-m und --diffusion-model sind beide gesetzt, aber es kann nur eines geladen werden")
	}
	return nil
}

// Models reports no listing, and that is the honest answer rather than an empty one: this
// backend has no route to ask, because the model files are named on the command line. The
// settings panel therefore offers a file picker for this kind instead of a checkpoint list.
//
// The slice is non-nil on purpose. A nil slice marshals to JSON `null` rather than `[]`, and
// the panel iterates whatever it gets; that bug has been shipped here twice already.
func (c *CLI) Models(context.Context) ([]Model, error) {
	return []Model{}, nil
}

// Samplers lists the names this binary's own option table offers; see cliSamplerNames.
func (c *CLI) Samplers(context.Context) ([]string, error) {
	out := make([]string, len(cliSamplerNames))
	copy(out, cliSamplerNames)
	return out, nil
}

// Generate runs the binary once and reads the picture it wrote.
//
// The picture arrives as a file rather than as output: `-o` takes a path and there is no
// documented way to ask for the bytes on stdout, so the draw happens in a temporary
// directory that is removed whatever the outcome. Nothing else about the run is
// interpreted — the process's exit code, and the tail of its log when that is non-zero, are
// the whole diagnosis.
func (c *CLI) Generate(ctx context.Context, req Request) (Result, error) {
	binary, err := c.program()
	if err != nil {
		return Result{}, err
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Result{}, errors.New("der Bild-Prompt ist leer")
	}
	if err := c.modelContradiction(); err != nil {
		return Result{}, err
	}
	// The numbers are bounded here as well as by the caller that built the request; see
	// limits.go. This backend is the one where it matters most, because the binary takes
	// the numbers as command-line flags and does exactly what they say: a 20000-pixel size
	// is a latent of tens of gigabytes, and an unbounded step count is hours of sampling.
	req = req.Normalised()

	// The seed is chosen here for the same reason the HTTP client chooses one: only the
	// seed that was used makes a picture reproducible, and this backend's log reports it
	// only as prose. Choosing it means the number is known whatever the log says.
	seed := req.Seed
	if seed == nil {
		chosen := RandomSeed()
		seed = &chosen
	}

	dir, err := os.MkdirTemp("", "kichat-sdcli-")
	if err != nil {
		return Result{}, fmt.Errorf("kein Arbeitsverzeichnis für das Bild: %w", err)
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "bild.png")

	drawCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(drawCtx, binary, c.cliArgs(req, prompt, *seed, output)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Stop is a kill on this backend, so the child is given a group of its own and the kill
	// is aimed at the group: a configured "binary" may be a wrapper script, and a script
	// that started sd-cli without `exec` would otherwise survive the stop and keep drawing.
	isolateProcess(cmd)
	cmd.Cancel = func() error { return killProcess(cmd) }
	// A bound on the wait itself, whatever the child did with its pipes. `Wait` returns only
	// once the output pipes are closed as well as the process reaped, and a grandchild holding
	// the write end open — what a killed wrapper script leaves behind — would hold the caller
	// for as long as that grandchild lives. That is precisely what a Stop that "did nothing"
	// looks like from the window, and it is how this bound was found.
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()

	// The caller's own cancellation is reported as such, so the draw path can tell a user's
	// Stop from a failure. Killing the process is what Stop *is* on this backend: there is
	// no server to ask, so this provider deliberately does not implement images.Interrupter.
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if runErr != nil {
		if drawCtx.Err() != nil {
			return Result{}, fmt.Errorf("das Zeichnen hat länger als %s gedauert", c.Timeout)
		}
		return Result{}, fmt.Errorf("%s: %v%s", binary, runErr, logTail(stderr.String(), stdout.String()))
	}

	data, err := os.ReadFile(output)
	if err != nil {
		return Result{}, fmt.Errorf("das Programm hat kein Bild geschrieben%s", logTail(stderr.String(), stdout.String()))
	}
	mime, ext := SniffImage(data, MimePNG, "png")
	result := Result{
		Images: []Image{{Data: data, Mime: mime, Ext: ext}},
		Seed:   *seed,
		Model:  c.files.Label(),
		Info:   strings.TrimSpace(stdout.String()),
	}
	// A seed the log names wins, even when it differs from the one this app sent. That is
	// not a theoretical case: a build may ignore `--seed`, and a recipe that recorded the
	// number KiChat *asked* for would then describe a picture that was never drawn — a
	// redraw would produce a different image every time and look like a broken feature.
	if reported := seedFromLog(stdout.String()); reported != 0 {
		result.Seed = reported
	}
	return result, nil
}

// cliArgs builds one draw's command line.
//
// The user's extra arguments come first so that this app's own values win any flag they both
// set: the recipe records this app's values, and a panel that showed one step count while
// the binary drew another would record a recipe describing a picture that does not exist.
// A flag this app never sets — `--sigmas`, `--diffusion-fa`, `--cfg-scale`, `-v` — is
// passed through untouched, which is the point of the extra list. The two the app does set
// itself, because they are modes a user flips per model, are a bare `--offload-to-cpu` and
// the `-s` seed below; both are placed after the user's own arguments, so the app's value is
// the one that survives a flag written twice.
//
// `req.Model` is deliberately not among these. The files this backend loads are a property
// of the configured set rather than of one request — a pipeline's VAE and text encoder
// cannot be switched per draw by a name — so a per-request checkpoint change is not offered
// here, and a recipe records the set's own label instead.
func (c *CLI) cliArgs(req Request, prompt string, seed int64, output string) []string {
	args := make([]string, 0, 25)
	args = append(args, c.opts.Extra...)
	// Bare, with no value: that is how a working command line spells it, and a value would
	// be this app inventing syntax for a flag whose own build decides what it accepts.
	if c.opts.OffloadToCPU {
		args = append(args, "--offload-to-cpu")
	}
	// One shape or the other: a single-file checkpoint is `-m`, and a pipeline split into
	// parts names each part. Passing both would hand the binary two models to load and no
	// statement about which one to draw with.
	if v := strings.TrimSpace(c.files.Model); v != "" {
		args = append(args, "-m", v)
	}
	// A slice of pairs rather than a map: the order of a command line is observable, both to
	// a test and to a person reading a bug report, and ranging over a map would reshuffle it
	// on every draw.
	for _, part := range []struct{ flag, value string }{
		{"--diffusion-model", c.files.DiffusionModel},
		{"--vae", c.files.VAE},
		{"--llm", c.files.LLM},
		{"--clip_l", c.files.ClipL},
		{"--clip_g", c.files.ClipG},
		{"--t5xxl", c.files.T5XXL},
	} {
		if v := strings.TrimSpace(part.value); v != "" {
			args = append(args, part.flag, v)
		}
	}
	args = append(args, "-p", prompt)
	// An empty negative prompt is omitted, not sent as an empty string: the panel's empty
	// field means "leave it to the model", and the app's rule for every askable field is the
	// same one the HTTP client follows.
	if v := strings.TrimSpace(req.NegativePrompt); v != "" {
		args = append(args, "-n", v)
	}
	if req.Steps > 0 {
		args = append(args, "--steps", strconv.Itoa(req.Steps))
	}
	if req.Width > 0 {
		args = append(args, "-W", strconv.Itoa(req.Width))
	}
	if req.Height > 0 {
		args = append(args, "-H", strconv.Itoa(req.Height))
	}
	if v := strings.TrimSpace(req.Sampler); v != "" {
		args = append(args, "--sampling-method", v)
	}
	// Always sent, never omitted, and never -1. The flag's own default is 42 — not a random
	// seed — so a draw that left it out would produce the same picture every time and read
	// as a broken prompt, while -1 would hand the choice back to a binary whose answer this
	// app can only learn by parsing prose.
	args = append(args, "-s", strconv.FormatInt(seed, 10))
	args = append(args, "-o", output)
	return args
}

// cliSeedLine finds the seed in the binary's own log.
//
// The line is `generating image: <i>/<n> - seed <n>` from the shared image pipeline
// (`LOG_INFO` in src/pipeline/image.cpp), printed once per picture. The *last* match wins
// because a batch prints one per image and the batch above is always one picture; taking the
// first would report the first image's seed for a later one.
var cliSeedLine = regexp.MustCompile(`(?m)seed[ \t]+(-?[0-9]+)`)

// seedFromLog returns the seed the log reports, 0 when it reports none. Zero is this
// package's "no seed is known", and the value is rejected rather than returned for that
// reason: a log that said `seed 0` would otherwise be indistinguishable from a log without
// a seed line.
func seedFromLog(log string) int64 {
	matches := cliSeedLine.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// logTail renders the last few lines of a failed run's output for an error message. The
// binary's own words are the only diagnosis there is — it exits non-zero and says why on
// stderr — and the whole log is often hundreds of progress lines long, so this keeps the
// end, where the reason is.
func logTail(stderr, stdout string) string {
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = strings.TrimSpace(stdout)
	}
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	const keep = 6
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return ": " + strings.Join(lines, " | ")
}
