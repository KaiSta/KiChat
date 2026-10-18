package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalisedBoundsEveryNumber pins the one function every caller and both providers
// run, and pins it as idempotent: it is applied twice on the ordinary path — once where the
// request is built, so the recipe records what will actually be drawn, and once at the
// backend, so a caller that forgets cannot put the number on the wire.
func TestNormalisedBoundsEveryNumber(t *testing.T) {
	cases := []struct {
		name                        string
		width, height, steps        int
		wantWidth, wantHeight, want int
		why                         string
	}{
		{"inside the bounds is untouched", 768, 512, 25, 768, 512, 25,
			"a size the user picked is not this function's business"},
		{"the maximum itself", MaxDimension, MaxDimension, MaxSteps, MaxDimension, MaxDimension, MaxSteps,
			"the bound is inclusive"},
		{"above the maximum", 20000, 20000, 100000, MaxDimension, MaxDimension, MaxSteps,
			"a local server attempts these numbers rather than refusing them"},
		{"just above the maximum", MaxDimension + 1, MaxDimension + 1, MaxSteps + 1, MaxDimension, MaxDimension, MaxSteps,
			"the clamp happens before the rounding, so it lands on the maximum"},
		{"below the minimum", 63, 1, 1, MinDimension, MinDimension, 1,
			"below the minimum there is no latent grid, and one step is a valid request"},
		{"not a multiple of the step", 1025, 1021, 30, 1024, 1024, 30,
			"the latent grid needs a multiple of 8"},
		{"rounds to the nearest multiple, not always up", 1025, 1027, 30, 1024, 1024, 30,
			"1027 is 3 above 1024 and 5 below 1032, so it lands on the lower one"},
		{"rounds up when that is the nearer multiple", 1029, 1030, 30, 1032, 1032, 30,
			"1029 is 5 above 1024 and 3 below 1032"},
		{"zero is the server's own", 0, 0, 0, 0, 0, 0,
			"an empty box in the editor means \"you decide\", and 0 is how that is spelled"},
		{"negative is the same statement", -1, -64, -7, 0, 0, 0,
			"a model passing -1 means the same thing as an empty box; a JSON omitempty does not drop a negative, so it must not reach the wire"},
	}
	for _, c := range cases {
		in := Request{Width: c.width, Height: c.height, Steps: c.steps}
		got := in.Normalised()
		if got.Width != c.wantWidth || got.Height != c.wantHeight || got.Steps != c.want {
			t.Errorf("%s: Normalised(%d,%d,%d) = (%d,%d,%d), want (%d,%d,%d) — %s",
				c.name, c.width, c.height, c.steps, got.Width, got.Height, got.Steps,
				c.wantWidth, c.wantHeight, c.want, c.why)
		}
		if again := got.Normalised(); again != got {
			t.Errorf("%s: normalising twice changed the request: %+v then %+v", c.name, got, again)
		}
	}
}

// TestNormalisedBoundsTheGuidanceScale pins the one field of a request that is not a whole
// number, and it is a table of its own rather than a column in the one above because the
// reading of "nobody chose one" has to survive a float: zero is the sentinel and must not be
// raised to the minimum, which would turn an empty box into a scale nobody asked for.
//
// Not-a-number is in the table because it is reachable rather than theoretical: the tool
// decodes arguments loosely, `strconv.ParseFloat` accepts "nan", and a NaN that reached a
// JSON payload would fail the *entire* draw at marshal time — `encoding/json` refuses to
// represent one. Reading it as "nobody chose one" is the only answer that keeps a picture
// drawable.
func TestNormalisedBoundsTheGuidanceScale(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
		why  string
	}{
		{"a scale in the range is untouched", 7.5, 7.5, "a fractional scale is a real request"},
		{"one is the floor's own value", MinCfgScale, MinCfgScale, "a distilled or flow-matching model is run at 1"},
		{"below the floor is raised", 0.2, MinCfgScale, "at 1 the guidance term is inert, not wrong; below it there is no such reading"},
		{"above the ceiling is capped", 250, MaxCfgScale, "past it a picture only gets harder, for the same GPU seconds"},
		{"the ceiling itself", MaxCfgScale, MaxCfgScale, "the bound is inclusive"},
		{"zero is the backend's own", 0, 0, "an empty field means the model decides, and this is how that is spelled"},
		{"a negative scale is the same statement", -7, 0, "a model passing -1 means an empty box, and omitempty would keep a negative on the wire"},
		{"not a number is the same statement", math.NaN(), 0, "json.Marshal refuses a NaN, so it would fail the whole request"},
		{"infinity lands on the ceiling", math.Inf(1), MaxCfgScale, "it is above the maximum like any other large number"},
	}
	for _, c := range cases {
		got := Request{CfgScale: c.in}.Normalised()
		if got.CfgScale != c.want {
			t.Errorf("%s: Normalised(%v).CfgScale = %v, want %v — %s", c.name, c.in, got.CfgScale, c.want, c.why)
		}
		if again := got.Normalised(); again != got {
			t.Errorf("%s: normalising twice changed the request: %+v then %+v", c.name, got, again)
		}
	}
}

// TestHiresNormalisesItsNumbers pins the second pass's bounds and its one sentinel: a
// disabled pass is the zero value, and an enabled one with no numbers gets the defaults
// rather than the minimum. The distinction matters downstream because the recipe stores
// these fields only when the pass ran, so "configured but off" must not leave numbers behind.
func TestHiresNormalisesItsNumbers(t *testing.T) {
	cases := []struct {
		name string
		in   Hires
		want Hires
	}{
		{"off is the zero value", Hires{Enabled: false, Scale: 1.8, Denoise: 0.5}, Hires{}},
		{"an enabled pass with no numbers gets the defaults", Hires{Enabled: true}, Hires{Enabled: true, Scale: DefaultHiresScale, Denoise: DefaultHiresDenoise}},
		{"inside the range is untouched", Hires{Enabled: true, Scale: 1.4, Denoise: 0.35}, Hires{Enabled: true, Scale: 1.4, Denoise: 0.35}},
		{"a scale below the minimum is raised", Hires{Enabled: true, Scale: 1.01, Denoise: 0.4}, Hires{Enabled: true, Scale: MinHiresScale, Denoise: 0.4}},
		{"a scale above the maximum is capped", Hires{Enabled: true, Scale: 4, Denoise: 0.4}, Hires{Enabled: true, Scale: MaxHiresScale, Denoise: 0.4}},
		{"a denoise below the minimum is raised", Hires{Enabled: true, Scale: 1.5, Denoise: 0.05}, Hires{Enabled: true, Scale: 1.5, Denoise: MinHiresDenoise}},
		{"a denoise above the maximum is capped", Hires{Enabled: true, Scale: 1.5, Denoise: 0.99}, Hires{Enabled: true, Scale: 1.5, Denoise: MaxHiresDenoise}},
		{"negative numbers are the sentinel", Hires{Enabled: true, Scale: -1, Denoise: -1}, Hires{Enabled: true, Scale: DefaultHiresScale, Denoise: DefaultHiresDenoise}},
		{"not a number is the sentinel", Hires{Enabled: true, Scale: math.NaN(), Denoise: math.NaN()}, Hires{Enabled: true, Scale: DefaultHiresScale, Denoise: DefaultHiresDenoise}},
	}
	for _, c := range cases {
		if got := c.in.Normalised(); got != c.want {
			t.Errorf("%s: Normalised(%+v) = %+v, want %+v", c.name, c.in, got, c.want)
		}
		if again := c.in.Normalised().Normalised(); again != c.in.Normalised() {
			t.Errorf("%s: normalising twice changed the pass: %+v then %+v", c.name, c.in.Normalised(), again)
		}
	}
}

// TestOutputSizeIsTheProductOfBaseAndScale keeps the one arithmetic both the recipe and the
// verification of a second pass depend on in one place. The aspect ratio is scaled, not
// independently rounded, and a size nobody chose stays unknown rather than becoming a number
// this app made up.
func TestOutputSizeIsTheProductOfBaseAndScale(t *testing.T) {
	cases := []struct {
		name         string
		req          Request
		wantW, wantH int
	}{
		{"no second pass is the base size", Request{Width: 768, Height: 512}, 768, 512},
		{"one and a half times", Request{Width: 768, Height: 512, Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}}, 1152, 768},
		// The two 16:9 bases the Settings presets fill in (SIZE_PRESETS in
		// frontend/src/components/Settings.tsx). Both land exactly on the screen size
		// their label names, which is what lets a preset carry a fixed enlargement; a
		// change to MaxDimension or MaxHiresScale breaks this table before it breaks a
		// preset in silence.
		{"the FHD preset is exactly FHD", Request{Width: 1280, Height: 720, Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}}, 1920, 1080},
		{"the 4K preset is exactly 4K", Request{Width: 1920, Height: 1080, Hires: Hires{Enabled: true, Scale: 2, Denoise: 0.4}}, 3840, 2160},
		// The base is normalised first, because that is the size the first pass really
		// draws: 700x500 becomes 704x504 on the latent grid, and 704x504 × 1.5 is
		// 1056x756, which the second pass then rounds up to 1056x760.
		{"the base is rounded before it is scaled", Request{Width: 700, Height: 500, Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}}, 1056, 760},
		// 520 × 1.25 is 650, and the app's 8-grid prediction is 656. A1111 returns that
		// exact target; sd.cpp aligns it again to its own spatial multiple (704 for a UNet),
		// which is why verification treats the prediction as a floor rather than an
		// equality. Rounding to the nearest multiple would predict 648 and then refuse
		// A1111's real answer.
		{"a scaled edge is aligned up, not to the nearest multiple", Request{Width: 520, Height: 520, Hires: Hires{Enabled: true, Scale: 1.25, Denoise: 0.4}}, 656, 656},
		{"an unknown base stays unknown", Request{Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}}, 0, 0},
		{"an unknown height stays unknown", Request{Width: 768, Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4}}, 768, 0},
	}
	for _, c := range cases {
		gotW, gotH := c.req.OutputSize()
		if gotW != c.wantW || gotH != c.wantH {
			t.Errorf("%s: OutputSize() = %dx%d, want %dx%d", c.name, gotW, gotH, c.wantW, c.wantH)
		}
	}
}

// TestSDWebUIAsksForTheSecondPassOnlyWhenItIsOn pins the payload the A1111 route reads.
// The keys are the server's own spelling, and the one that matters is enable_hr: a payload
// that carried the numbers but not the switch would draw an ordinary picture and the tool
// would have to reject it.
func TestSDWebUIAsksForTheSecondPassOnlyWhenItIsOn(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"images":["`+base64.StdEncoding.EncodeToString(pngBytes)+`"],"info":"{}"}`)
	}))
	defer server.Close()

	client := NewSDWebUI("images", "SD WebUI", server.URL, "")
	if _, err := client.Generate(context.Background(), Request{
		Prompt: "a fox", Width: 768, Height: 512,
		Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	}); err != nil {
		t.Fatalf("Generate(hires): %v", err)
	}
	if _, err := client.Generate(context.Background(), Request{Prompt: "a fox"}); err != nil {
		t.Fatalf("Generate(one pass): %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	// The target is the app's number, not a second description of the factor: 768×512 at
	// 1.5× is 1152×768, and both A1111 and sd-server honour `hr_resize_x`/`hr_resize_y`.
	for key, want := range map[string]any{
		"enable_hr": true, "hr_scale": 1.5, "denoising_strength": 0.4,
		"hr_resize_x": float64(1152), "hr_resize_y": float64(768),
	} {
		if got, ok := bodies[0][key]; !ok || got != want {
			t.Errorf("the second-pass payload lacks %s=%v (got %v)", key, want, got)
		}
	}
	if _, ok := bodies[1]["enable_hr"]; ok {
		t.Errorf("an ordinary draw carried enable_hr: %v", bodies[1])
	}
	for _, key := range []string{"hr_resize_x", "hr_resize_y"} {
		if _, ok := bodies[1][key]; ok {
			t.Errorf("an ordinary draw carried %s: %v", key, bodies[1])
		}
	}
}

// TestSDWebUIBoundsTheRequestItSends is the half the callers cannot give: the number has to
// be bounded by the provider as well, because a future caller is free to build a request
// and never normalise it. Everything asserted here is read off the bytes that reached the
// server rather than off the request struct.
func TestSDWebUIBoundsTheRequestItSends(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		fmt.Fprintf(w, `{"images":["%s"]}`, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	defer server.Close()
	client := NewSDWebUI("sd", "Stable Diffusion", server.URL, "")

	if _, err := client.Generate(context.Background(), Request{
		Prompt: "a red fox", Width: 20000, Height: 63, Steps: 100000, CfgScale: 250,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := bodies[0]
	if got["width"] != float64(MaxDimension) || got["height"] != float64(MinDimension) {
		t.Errorf("size on the wire = %vx%v, want %dx%d",
			got["width"], got["height"], MaxDimension, MinDimension)
	}
	if got["steps"] != float64(MaxSteps) {
		t.Errorf("steps on the wire = %v, want the bound %d", got["steps"], MaxSteps)
	}
	// The guidance scale travels under the name both server flavours read it by — A1111's
	// `cfg_scale` on the processing object and stable-diffusion.cpp's `sd-server`, which
	// falls back to the model's own `txt_cfg` when the key is absent.
	if got["cfg_scale"] != MaxCfgScale {
		t.Errorf("cfg_scale on the wire = %v, want the bound %v", got["cfg_scale"], MaxCfgScale)
	}

	// A negative step count is not clamped up to 1 and is not sent at all. Sending it was a
	// real request the server had to interpret — `omitempty` drops a zero and keeps a
	// negative — and it is the shape a model produces when it means "you choose".
	if _, err := client.Generate(context.Background(), Request{Prompt: "a red fox", Steps: -7}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if value, present := bodies[1]["steps"]; present {
		t.Errorf("steps = %v was sent for a negative request; an unset step count is omitted", value)
	}
	if value, present := bodies[1]["cfg_scale"]; present {
		t.Errorf("cfg_scale = %v was sent when nobody chose one; omitting it leaves the model's own in force", value)
	}
}

// TestCLIBoundsTheCommandLineItRuns is the same assertion for the other backend, and the one
// where the numbers matter most: this backend hands them to a program as its own flags, and a
// 20000-pixel size there is a latent of tens of gigabytes rather than a rejected request.
// Measured before the bound existed: `--steps 100000 -W 20000 -H 20000`.
func TestCLIBoundsTheCommandLineItRuns(t *testing.T) {
	binary := fakeCLI(t, cliRecorder)
	client := NewCLI("images", "sd-cli", binary, ModelFiles{Model: "/models/v1-5.safetensors"}, Options{})

	if _, err := client.Generate(context.Background(), Request{
		Prompt: "a red fox", Width: 20000, Height: 63, Steps: 100000, CfgScale: 250,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	line := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{
		fmt.Sprintf("-W %d", MaxDimension),
		fmt.Sprintf("-H %d", MinDimension),
		fmt.Sprintf("--steps %d", MaxSteps),
		fmt.Sprintf("--cfg-scale %v", MaxCfgScale),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the command line is missing %q:\n%s", want, line)
		}
	}
	for _, unwanted := range []string{"20000", "100000", "-W 63 "} {
		if strings.Contains(line, unwanted) {
			t.Errorf("the unbounded value %q reached the binary:\n%s", unwanted, line)
		}
	}

	// A negative step count is dropped rather than clamped to 1, which is what the flag's
	// absence means here: the binary has its own default.
	if _, err := client.Generate(context.Background(), Request{Prompt: "a red fox", Steps: -7}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if line := strings.Join(argsOf(t, binary), " "); strings.Contains(line, "--steps") {
		t.Errorf("a negative step count was passed to the binary:\n%s", line)
	}
	if line := strings.Join(argsOf(t, binary), " "); strings.Contains(line, "--cfg-scale") {
		t.Errorf("a guidance scale nobody chose was passed to the binary:\n%s", line)
	}
}

// Kept is the one place the batch bound is applied, and it has to be cheap and harmless for
// the ordinary answer: a server that returns one picture, or four, must be passed through
// untouched — the caller loops over the slice it just drew. Only a server that returns more
// than one message can show is trimmed, and the count of what was trimmed stays readable as
// len(Result.Images) - len(Result.Kept()), which is what the tool result reports.
func TestKeptBoundsOneAnswersPictures(t *testing.T) {
	pictures := func(n int) Result {
		imgs := make([]Image, n)
		for i := range imgs {
			imgs[i] = Image{Data: []byte{byte(i)}, Mime: MimePNG, Ext: "png"}
		}
		return Result{Images: imgs}
	}

	inside := pictures(MaxPicturesPerDraw)
	kept := inside.Kept()
	if len(kept) != MaxPicturesPerDraw {
		t.Fatalf("kept = %d, want all %d", len(kept), MaxPicturesPerDraw)
	}
	if &kept[0] != &inside.Images[0] {
		t.Error("an answer inside the bound was copied rather than passed through")
	}

	over := pictures(MaxPicturesPerDraw + 3)
	kept = over.Kept()
	if len(kept) != MaxPicturesPerDraw {
		t.Fatalf("kept = %d, want the bound of %d", len(kept), MaxPicturesPerDraw)
	}
	if dropped := len(over.Images) - len(kept); dropped != 3 {
		t.Errorf("dropped = %d, want the 3 the caller has to report", dropped)
	}
	// The kept ones are the first ones: the server drew them in order, and the first picture
	// of a batch is the one its prompt describes.
	for i := range kept {
		if kept[i].Data[0] != byte(i) {
			t.Errorf("kept[%d] is picture %d, want the first ones in order", i, kept[i].Data[0])
		}
	}

	var empty Result
	if got := empty.Kept(); len(got) != 0 {
		t.Errorf("an empty answer kept %d pictures", len(got))
	}
}
