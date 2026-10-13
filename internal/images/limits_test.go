package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
		Prompt: "a red fox", Width: 20000, Height: 63, Steps: 100000,
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

	// A negative step count is not clamped up to 1 and is not sent at all. Sending it was a
	// real request the server had to interpret — `omitempty` drops a zero and keeps a
	// negative — and it is the shape a model produces when it means "you choose".
	if _, err := client.Generate(context.Background(), Request{Prompt: "a red fox", Steps: -7}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if value, present := bodies[1]["steps"]; present {
		t.Errorf("steps = %v was sent for a negative request; an unset step count is omitted", value)
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
		Prompt: "a red fox", Width: 20000, Height: 63, Steps: 100000,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	line := strings.Join(argsOf(t, binary), " ")
	for _, want := range []string{
		fmt.Sprintf("-W %d", MaxDimension),
		fmt.Sprintf("-H %d", MinDimension),
		fmt.Sprintf("--steps %d", MaxSteps),
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
}
