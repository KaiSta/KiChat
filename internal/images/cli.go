package images

import (
	"bufio"
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
	"sync"
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

	// helpOnce memoises the one `-h` probe. Two callers want it — Health proves the process
	// starts, previewSupport learns whether this build has the preview options — and
	// spawning the binary twice to answer one question would be silly. A failed probe is
	// memoised too, which is what keeps a binary that cannot answer from being spawned on
	// every draw.
	helpOnce sync.Once
	helpOut  string
	helpErr  error
}

// help runs the binary's own `-h` once and keeps what it printed.
//
// The option table is the only documentation this backend has: the project tells users to
// run `-h` rather than publishing a stable list, so the app asks the same way a person
// would. Measured on the build in the repository's notes: 293 lines, on stdout, exit 0, in
// well under a second — cheap enough to be worth the one call.
func (c *CLI) help(ctx context.Context, binary string) (string, error) {
	c.helpOnce.Do(func() {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probeCtx, binary, "-h").Output()
		c.helpOut, c.helpErr = string(out), err
	})
	return c.helpOut, c.helpErr
}

// previewSupport reports whether this build's own help lists the preview options.
//
// It is asked rather than assumed. Those flags are newer than the first `sd-cli` builds, and
// an unknown option fails the *whole run* — so a build without them would turn "no progress
// display" into "no picture". A build whose help does not answer at all is treated the same
// way, which falls back to exactly the behaviour this client had before previews.
func (c *CLI) previewSupport(ctx context.Context, binary string) bool {
	help, err := c.help(ctx, binary)
	if err != nil && help == "" {
		return false
	}
	return strings.Contains(help, "--preview")
}

// userPicksPreview reports whether the user's own arguments already decide the preview.
//
// Their choice of method or path stays theirs: this app has no control for either, and
// appending its own would silently overrule the command line the user wrote (our arguments
// come last, which is the rule everywhere else in this file). The draw then runs unwatched
// rather than wrongly watched.
func (c *CLI) userPicksPreview() bool {
	for _, arg := range c.opts.Extra {
		if arg == "--preview" || strings.HasPrefix(arg, "--preview=") ||
			arg == "--preview-path" || strings.HasPrefix(arg, "--preview-path=") {
			return true
		}
	}
	return false
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

	// The usage text itself is not a diagnosis, and it is kept for previewSupport rather
	// than thrown away: only "the process could not be started or did not exit" is.
	if _, err := c.help(ctx, binary); err != nil {
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
	return c.GenerateWithProgress(ctx, req, nil)
}

// GenerateWithProgress is Generate, with the draw reported while it runs.
//
// Two sources, both of them the program's own and neither invented here. It writes a preview
// image to the path `--preview-path` names — measured on the build in §2 of the notes: the
// file is rewritten on every denoiser step, so a poll of it is a live picture of the draw —
// and it prints the sampling step into its log as a bar (`  |=====>    | 3/12 - 2.20s/it`).
// Neither is required: a build without those flags, or a draw nobody is watching, takes the
// same path with no preview and no reports.
//
// The log is read line by line rather than collected and read at the end, because two of the
// three things in it are only useful while the draw is running: the step, which is the
// progress, and the seed, which makes a recipe record the number the program really used
// rather than the one it was asked for. The recent lines are kept in a ring, which is what
// a failure prints.
func (c *CLI) GenerateWithProgress(ctx context.Context, req Request, report func(Progress)) (Result, error) {
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
	previewPath := filepath.Join(dir, "preview.png")

	args := c.cliArgs(req, prompt, *seed, output)
	watch := report != nil && !c.userPicksPreview() && c.previewSupport(ctx, binary)
	if watch {
		args = append(args, "--preview", cliPreviewMethod,
			"--preview-interval", cliPreviewInterval, "--preview-path", previewPath)
	}

	drawCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(drawCtx, binary, args...)
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// A pipe this file owns rather than `cmd.StdoutPipe()`, and the difference is not academic:
	// a pipe the *command* owns is closed by `Wait`, so anything still in it is read by nobody,
	// while this one keeps EOF meaning "the program and every process it started are done
	// writing". That is what lets the drain below be bounded without throwing the tail away —
	// and what lets the reading finish before the wait does, rather than after it.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("%s: %v", binary, err)
	}
	defer stdout.Close()
	cmd.Stdout = stdoutW
	sdlog := &cliLog{}
	if err := cmd.Start(); err != nil {
		stdoutW.Close()
		return Result{}, fmt.Errorf("%s: %v", binary, err)
	}
	// The parent's copy of the write end goes at once: an fd left open here would be a writer
	// of our own, and the scanner would then wait for a line nobody is going to write.
	stdoutW.Close()
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(stdout)
		// Progress bars are long single lines, and a wide terminal's worth of them exceeds the
		// scanner's default limit; a log that cannot be read would silently cost the seed line.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			sdlog.write(scanner.Text())
		}
	}()
	stopWatch := make(chan struct{})
	if watch {
		go watchPreview(stopWatch, previewPath, sdlog, report)
	}
	// The draw is the wait, and the watching has to last exactly as long: stopping the preview
	// watcher before the process exits would leave every draw longer than the bound un-
	// reported after that point, which is the entire case progress exists for. Measured while
	// fixing this: with the drain placed before the wait and bounded by a flat two seconds, a
	// three-second draw reported exactly one step and then went quiet.
	runErr := cmd.Wait()
	close(stopWatch)
	// The drain runs after, so the lines the program wrote on its way out are in hand before
	// the result is built. Its bound is for the case WaitDelay was added for in the first
	// place — a wrapper script that started the program without `exec` and left a grandchild
	// holding the pipe open, where EOF never arrives — and it is a bound on the *drain* rather
	// than on the draw, which is why the tail is what a timeout costs and never the report.
	select {
	case <-scanned:
	case <-time.After(2 * time.Second):
	}

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
		return Result{}, fmt.Errorf("%s: %v%s", binary, runErr, logTail(stderr.String(), sdlog.tail()))
	}

	data, err := os.ReadFile(output)
	if err != nil {
		return Result{}, fmt.Errorf("das Programm hat kein Bild geschrieben%s", logTail(stderr.String(), sdlog.tail()))
	}
	mime, ext := SniffImage(data, MimePNG, "png")
	result := Result{
		Images: []Image{{Data: data, Mime: mime, Ext: ext}},
		Seed:   *seed,
		Model:  c.files.Label(),
		Info:   sdlog.summary(),
	}
	// A seed the log names wins, even when it differs from the one this app sent. That is
	// not a theoretical case: a build may ignore `--seed`, and a recipe that recorded the
	// number KiChat *asked* for would then describe a picture that was never drawn — a
	// redraw would produce a different image every time and look like a broken feature.
	if reported := sdlog.seed(); reported != 0 {
		result.Seed = reported
	} else if reported := seedFromLog(sdlog.tail()); reported != 0 {
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

// The knobs of the preview, and why each has the value it has.
const (
	// cliPreviewMethod asks for the cheap decoder: `tae` is a small network rather than the
	// full VAE, and the preview is a thumbnail of a draw in progress, not the picture.
	// Measured on a build with no taesd file in its folder: real PNG previews regardless.
	cliPreviewMethod = "tae"
	// cliPreviewInterval is passed rather than left to the flag's own default. How often the
	// picture on screen changes is a choice this app makes for the user, not one it inherits
	// from whichever build happens to be installed.
	cliPreviewInterval = "1"
	// cliPreviewPoll is how often the preview file is looked at: below the ~2-3 s a step
	// takes on a mid-range GPU, and far above the cost of a stat call. A poll that finds the
	// file untouched reports nothing.
	cliPreviewPoll = 700 * time.Millisecond
	// cliPreviewMaxBytes bounds one preview read. A preview is a full-resolution picture of
	// the latent, so a 2048² draw can produce megabytes of it per step; past this the step
	// counter is still reported and the picture is not, because pushing eight megabytes
	// through the bridge per step costs more than the preview is worth.
	cliPreviewMaxBytes = 4 << 20
	// cliLogLines is how many lines of the program's own log are kept, as the tail a failure
	// prints. A 150-step draw at 2048² writes megabytes of progress bars and nobody reads
	// them afterwards.
	cliLogLines = 400
)

// cliStepLine finds the sampling step in the progress bar the program redraws with carriage
// returns: `  |=====>    | 3/12 - 2.20s/it`.
//
// The rate is part of the pattern, and that is the whole trick. The *model load* draws the
// same bar — `|#####     | 6/265 - 4.18GB/s` — so a pattern matching `n/m` alone reports the
// load's tensors as the draw's steps: measured, "step 6 of 265" for a two-step picture. A
// sampling bar names a per-step time (`s/it`, `it/s`); a load bar names a throughput.
var cliStepLine = regexp.MustCompile(`\|[ \t]*([0-9]+)/([0-9]+)[ \t]*-[ \t]*[0-9.]+[ \t]*(?:s/it|it/s)`)

// stepFromLog reads one sampling step out of one log line.
func stepFromLog(line string) (int, int, bool) {
	m := cliStepLine.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	step, stepErr := strconv.Atoi(m[1])
	steps, stepsErr := strconv.Atoi(m[2])
	if stepErr != nil || stepsErr != nil || steps <= 0 {
		return 0, 0, false
	}
	return step, steps, true
}

// cliSeedLine finds the seed in the binary's own log.
//
// The line is `generating image: <i>/<n> - seed <n>` from the shared image pipeline
// (`LOG_INFO` in src/pipeline/image.cpp) — measured on the build in the notes — printed once
// per picture. The *last* match wins because a batch prints one per image and the batch above
// is always one picture; taking the first would report the first image's seed for a later one.
//
// It is matched whole rather than by the word `seed`, which is what it used to be, because a
// prompt is echoed into that same log: a draw of "a robin's egg, seed 999" would otherwise
// record 999 as the seed that produced it.
var cliSeedLine = regexp.MustCompile(`generating image:[ \t]*[0-9]+/[0-9]+[ \t]*-[ \t]*seed[ \t]+(-?[0-9]+)`)

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

// cliLog is what this client reads out of the program's own log while it runs.
//
// Three things, each for a different caller: the seed (so a recipe records the number that
// was really used), the sampling step (so a draw that takes minutes is visibly moving), and
// the recent lines (so a failure can say why). Recent lines only — see cliLogLines — and
// guarded by a mutex, because the scanner goroutine writes while the draw's own goroutine and
// the preview watcher read.
type cliLog struct {
	mu    sync.Mutex
	lines []string
	seedN int64
	step  int
	steps int
}

func (l *cliLog) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > cliLogLines {
		l.lines = l.lines[len(l.lines)-cliLogLines:]
	}
	if seed := seedFromLog(line); seed != 0 {
		l.seedN = seed
	}
	if step, steps, ok := stepFromLog(line); ok {
		l.step, l.steps = step, steps
	}
}

func (l *cliLog) seed() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seedN
}

func (l *cliLog) progress() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.step, l.steps
}

// tail renders the kept lines as one text block, for logTail.
func (l *cliLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// cliSummaryMarkers are the log lines worth keeping about a draw that *worked*.
//
// The unmarked remainder is progress bars and backend chatter: what a person looking at a
// finished picture wants is what was loaded, which seed was drawn with, how long the
// sampling took, and where the file went. This is what Result.Info carries, in the program's
// own words rather than as numbers this app re-derived.
var cliSummaryMarkers = []string{
	"loading diffusion model from",
	"loading vae from",
	"loading llm from",
	"loading tensors completed",
	"generating image:",
	"sampling completed",
	"decode_first_stage completed",
	"generate_image completed",
	"save result image",
}

// summary renders those lines, trimmed of the source-file suffix the program appends to each
// one, and capped: a log line naming a path can be long, and this is a report rather than the
// log itself.
func (l *cliLog) summary() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		keep := false
		for _, marker := range cliSummaryMarkers {
			if strings.Contains(line, marker) {
				keep = true
				break
			}
		}
		if !keep {
			continue
		}
		if idx := strings.Index(line, " --- "); idx > 0 {
			line = line[:idx]
		}
		out = append(out, strings.TrimSpace(line))
		if len(out) >= 12 {
			break
		}
	}
	return strings.Join(out, "\n")
}

// watchPreview reports a draw while it runs, until stop is closed.
//
// One report carries both signals: the log's step is what makes a fraction ("3 von 12") and
// the preview file is what makes a picture, and they are read on the same tick so a report
// never pairs a step from one moment with a picture from another.
func watchPreview(stop <-chan struct{}, path string, log *cliLog, report func(Progress)) {
	ticker := time.NewTicker(cliPreviewPoll)
	defer ticker.Stop()
	var lastTime time.Time
	var lastSize int64
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			continue
		}
		if info.ModTime().Equal(lastTime) && info.Size() == lastSize {
			continue
		}
		lastTime, lastSize = info.ModTime(), info.Size()
		step, steps := log.progress()
		update := Progress{Step: step, Steps: steps}
		// A preview that cannot be read is not a failure: the draw is what matters, and the
		// step counter alone is still an answer to "is it moving".
		if data, err := readPreview(path); err == nil {
			update.PreviewMime, _ = SniffImage(data, MimePNG, "png")
			update.Preview = data
		}
		report(update)
	}
}

// readPreview reads a preview file, refusing one bigger than the bridge should carry.
func readPreview(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > cliPreviewMaxBytes {
		return nil, fmt.Errorf("Vorschau ist %d Bytes groß", info.Size())
	}
	return os.ReadFile(path)
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
