package images

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kichat/internal/proc"
)

// The weights the portable release ships. x4plus is trained on photographs and the
// anime variant on flat art; the program's own default is x4plus, and naming it here
// makes the choice visible in the recipe rather than implicit.
const (
	ESRGANDefaultModel = "realesrgan-x4plus"
	ESRGANAnimeModel   = "realesrgan-x4plus-anime"
)

// ESRGANModels is what the panel offers. Every one of them is a 4× model: the program's
// `-s` flag cannot invent an arbitrary factor, which is why this client always asks for
// the native scale and resamples the rest itself.
var ESRGANModels = []string{ESRGANDefaultModel, ESRGANAnimeModel}

// ESRGAN is one configured portable program.
type ESRGAN struct {
	Binary string
	Model  string
	Tile   int
}

// ModelName is the weights this run uses, defaulted rather than left empty so a recipe
// names what drew the picture.
func (e ESRGAN) ModelName() string {
	if model := strings.TrimSpace(e.Model); model != "" {
		return model
	}
	return ESRGANDefaultModel
}

// FindESRGAN resolves the configured path, or the program on PATH when nothing was
// configured. A user who already installed it system-wide does not have to type the
// path again; a user who downloaded the portable folder points at the executable once.
func FindESRGAN(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		found, err := exec.LookPath("realesrgan-ncnn-vulkan")
		if err != nil {
			return "", ErrUpscalerUnavailable
		}
		path = found
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrUpscalerUnavailable, path)
	}
	return path, nil
}

// Enlarge runs the program once and lands the answer on the exact target.
//
// The native 4× is the model's own scale; asking the binary for a different one would
// either be refused or silently produce a different network's arithmetic. The last step
// is the same Lanczos pass the classical tier uses, so a 512² source asked to reach 4K
// is 4× by the model and the remaining 1.875× by resampling — better than 7.5× Lanczos,
// and honest about where the detail came from.
func (e ESRGAN) Enlarge(ctx context.Context, src image.Image, target Target, lang string) (*image.RGBA, error) {
	program, err := FindESRGAN(e.Binary)
	if err != nil {
		return nil, err
	}
	modelDir, err := e.modelDir(program)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "kichat-upscale-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "in.png")
	output := filepath.Join(dir, "out.png")
	file, err := os.Create(input)
	if err != nil {
		return nil, err
	}
	if err := png.Encode(file, src); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}

	args := []string{
		"-i", input,
		"-o", output,
		"-n", e.ModelName(),
		"-s", "4",
		"-f", "png",
	}
	// `-m` is passed whenever the program came with its own models folder, which is the
	// shape every published archive has. The program's own default is the relative path
	// `models`, resolved against the *executable* (measured: a copy of the binary with no
	// models beside it reports `fopen …/alone/models/realesrgan-x4plus.param failed`), so the
	// default happens to be right for that layout — but only by accident, and naming the
	// folder is what makes it a promise instead.
	if modelDir != "" {
		args = append(args, "-m", modelDir)
	}
	if e.Tile > 0 {
		args = append(args, "-t", strconv.Itoa(e.Tile))
	}
	cmd := exec.CommandContext(ctx, program, args...)
	// The program is a single native binary, but the same reason sd-cli gets a group
	// applies: a user may configure a wrapper script, and Stop must reach the process
	// that is actually holding the GPU.
	proc.StartInOwnGroup(cmd)
	cmd.Cancel = func() error { return proc.KillTree(cmd) }
	cmd.WaitDelay = 5 * time.Second
	var log bytes.Buffer
	cmd.Stdout = &log
	cmd.Stderr = &log
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runErr != nil {
		return nil, fmt.Errorf("Real-ESRGAN: %w: %s", runErr, trimLog(log.String()))
	}
	// A run that loaded no weights is not a failed run — it is a successful one that
	// produced a black picture and exit code 0. Measured against the v0.2.0 archive, whose
	// Linux zip is 3.6 MB and contains no `models/` at all: the program prints
	// `fopen …realesrgan-x4plus.param failed` and `find_blob_index_by_name data failed`, then
	// writes an 880×880 fully black PNG. Exit code, output size and dimensions all look
	// right, so the only honest signal is the log line, and without this check the app would
	// file the black picture as the enlargement.
	if line := missingWeightsLine(log.String()); line != "" {
		return nil, fmt.Errorf("%w: %s", ErrUpscalerNoModels, line)
	}
	if _, err := os.Stat(output); err != nil {
		return nil, fmt.Errorf(text(lang,
			"Real-ESRGAN hat kein Bild geschrieben: %s",
			"Real-ESRGAN wrote no picture: %s"), trimLog(log.String()))
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	enlarged, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf(text(lang,
			"Real-ESRGAN-Ausgabe lesen: %w", "reading Real-ESRGAN's output: %w"), err)
	}
	return resampleTo(enlarged, target), nil
}

// modelDir is the models folder that belongs to this program, or empty when it has none.
//
// It is a folder beside the executable, which is how both published archives are laid out,
// and the reason it is resolved here rather than left to the program's default is that the
// default is silent when it fails: the model files that ship with the tiny v0.2.0 archive
// do not exist, and the run still succeeds. When a sibling `models` folder *is* there, the
// two files the chosen weights consist of have to be in it, and the error says which one is
// missing instead of leaving a black picture to be discovered later.
func (e ESRGAN) modelDir(program string) (string, error) {
	dir := filepath.Join(filepath.Dir(program), "models")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// No folder of its own: a user may have installed the weights elsewhere, and the
		// program's own search is then the only thing that knows. The log check in Enlarge is
		// what catches that case going wrong.
		return "", nil
	}
	model := e.ModelName()
	for _, ext := range []string{".param", ".bin"} {
		if _, err := os.Stat(filepath.Join(dir, model+ext)); err != nil {
			return "", fmt.Errorf("%w: %s", ErrUpscalerNoModels, filepath.Join(dir, model+ext))
		}
	}
	return dir, nil
}

// missingWeightsLine returns the program's own complaint when it could not load the weights,
// and "" when the log says nothing of the sort.
//
// It is a text match against a binary frozen in 2022, and that is a deliberate trade: the
// alternative is a picture that looks plausible to every check we have. Both strings are
// taken from a real run — see TestANeuralRunWithoutWeightsIsAFailure.
func missingWeightsLine(log string) string {
	for _, marker := range []string{"find_blob_index_by_name", "failed"} {
		if !strings.Contains(log, marker) {
			return ""
		}
	}
	for _, line := range strings.Split(log, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "find_blob_index_by_name") || strings.Contains(trimmed, "failed") {
			return trimmed
		}
	}
	return ""
}

// ProbeESRGAN runs one tiny picture through the program. It is the only honest answer
// to "is this set up": a path that exists proves nothing about a Vulkan driver, and the
// released build has no CPU fallback to quietly switch to.
func ProbeESRGAN(ctx context.Context, binary string, lang string) (string, error) {
	program, err := FindESRGAN(binary)
	if err != nil {
		return "", err
	}
	blank := image.NewRGBA(image.Rect(0, 0, 16, 16))
	probe := ESRGAN{Binary: program, Model: ESRGANDefaultModel}
	if _, err := probe.Enlarge(ctx, blank, Target{Width: 64, Height: 64}, lang); err != nil {
		return "", err
	}
	return program, nil
}

// trimLog keeps an error short enough to show in a window. The program prints a line
// per tile, and the useful part is almost always the last line.
func trimLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 600 {
		return s
	}
	return "…" + s[len(s)-600:]
}
