package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pngBytes is a byte string that is not valid base64 itself, so a decode that
// silently returned the input would fail the comparison rather than pass it.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}

func TestSDWebUIGenerateDecodesImages(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sdapi/v1/txt2img" {
			t.Errorf("path = %q, want /sdapi/v1/txt2img", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// `info` is a JSON *string* on AUTOMATIC1111, which is the shape a naive
		// decoder gets wrong by expecting an object.
		fmt.Fprintf(w, `{"images":["%s"],"info":"{\"seed\": 4242, \"sd_model_checkpoint\": \"sdxl.safetensors\"}"}`,
			base64.StdEncoding.EncodeToString(pngBytes))
	}))
	defer server.Close()

	client := NewSDWebUI("sd", "Stable Diffusion", server.URL, "")
	result, err := client.Generate(context.Background(), Request{
		Prompt:         "a red fox in snow",
		NegativePrompt: "blurry",
		Width:          768,
		Height:         512,
		Steps:          25,
		Sampler:        "Euler a",
		Model:          "sdxl.safetensors",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(result.Images))
	}
	if string(result.Images[0].Data) != string(pngBytes) {
		t.Errorf("image bytes survived the base64 round trip incompletely: %v", result.Images[0].Data)
	}
	if result.Images[0].Mime != "image/png" {
		t.Errorf("mime = %q, want image/png", result.Images[0].Mime)
	}
	// The seed and the checkpoint are read out of the nested info string: without
	// them a user cannot reproduce a picture they liked.
	if result.Seed != 4242 {
		t.Errorf("seed = %d, want 4242", result.Seed)
	}
	if result.Model != "sdxl.safetensors" {
		t.Errorf("model = %q, want sdxl.safetensors", result.Model)
	}

	if gotBody["prompt"] != "a red fox in snow" {
		t.Errorf("prompt = %v", gotBody["prompt"])
	}
	if gotBody["negative_prompt"] != "blurry" {
		t.Errorf("negative_prompt = %v", gotBody["negative_prompt"])
	}
	if gotBody["sampler_name"] != "Euler a" {
		t.Errorf("sampler_name = %v", gotBody["sampler_name"])
	}
	// A per-request checkpoint goes in override_settings, not a top-level field —
	// sending it under the wrong key silently draws with whatever was already
	// loaded instead of failing.
	override, ok := gotBody["override_settings"].(map[string]any)
	if !ok {
		t.Fatalf("override_settings missing from the request: %v", gotBody)
	}
	if override["sd_model_checkpoint"] != "sdxl.safetensors" {
		t.Errorf("override_settings = %v", override)
	}
}

// An older WebUI prefixed the base64 with a data: URL, and the two shapes cannot
// both be decoded by the same assumption.
func TestSDWebUIGenerateAcceptsDataURLPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"images":["data:image/png;base64,%s"]}`,
			base64.StdEncoding.EncodeToString(pngBytes))
	}))
	defer server.Close()

	result, err := NewSDWebUI("sd", "SD", server.URL, "").
		Generate(context.Background(), Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(result.Images[0].Data) != string(pngBytes) {
		t.Errorf("prefixed payload decoded wrong: %v", result.Images[0].Data)
	}
}

// The format is the server's `samples_format`, not a constant: a WebUI configured
// to answer with JPEG — which a user picks to save disk space — would otherwise be
// filed as a PNG, producing a file whose type and name disagree with its bytes.
func TestSDWebUIGenerateReportsTheFormatItReceived(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	webp := []byte{'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 0x56, 0x50}

	for _, tc := range []struct {
		name     string
		payload  []byte
		wantMime string
		wantExt  string
	}{
		{"png", pngBytes, MimePNG, "png"},
		{"jpeg", jpeg, MimeJPEG, "jpg"},
		{"webp", webp, MimeWebP, "webp"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"images":["%s"]}`, base64.StdEncoding.EncodeToString(tc.payload))
		}))
		result, err := NewSDWebUI("sd", "SD", server.URL, "").
			Generate(context.Background(), Request{Prompt: "x"})
		server.Close()
		if err != nil {
			t.Fatalf("%s: Generate: %v", tc.name, err)
		}
		if result.Images[0].Mime != tc.wantMime || result.Images[0].Ext != tc.wantExt {
			t.Errorf("%s: reported %q/%q, want %q/%q", tc.name,
				result.Images[0].Mime, result.Images[0].Ext, tc.wantMime, tc.wantExt)
		}
	}
}

// No model loaded is a specific, actionable state, and the WebUI reports it as a
// 404 on a route that exists — indistinguishable from a wrong port by status
// alone.
func TestSDWebUIGenerateWithoutAModelReportsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"No model loaded."}`)
	}))
	defer server.Close()

	_, err := NewSDWebUI("sd", "SD", server.URL, "").
		Generate(context.Background(), Request{Prompt: "x"})
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
}

func TestSDWebUIGenerateWithNoImagesReportsNoModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"images":[],"info":""}`)
	}))
	defer server.Close()

	_, err := NewSDWebUI("sd", "SD", server.URL, "").
		Generate(context.Background(), Request{Prompt: "x"})
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
}

func TestSDWebUIGenerateRejectsAnEmptyPrompt(t *testing.T) {
	client := NewSDWebUI("sd", "SD", "http://127.0.0.1:1", "")
	if _, err := client.Generate(context.Background(), Request{Prompt: "   "}); err == nil {
		t.Fatal("an empty prompt was accepted; it would draw something arbitrary")
	}
}

func TestSDWebUIModelsUsesTheTitleTheAPIAccepts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sdapi/v1/sd-models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// The second entry carries no title, which is the case the fallback exists
		// for; the third carries nothing usable and must be dropped rather than
		// offered as a nameless row.
		fmt.Fprint(w, `[
			{"title":"sdxl.safetensors [a1b2]","model_name":"sdxl","filename":"/m/sdxl.safetensors"},
			{"model_name":"flux-dev","filename":"/m/flux-dev.safetensors"},
			{"filename":"/m/nameless.safetensors"}
		]`)
	}))
	defer server.Close()

	models, err := NewSDWebUI("sd", "SD", server.URL, "").Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	// The third entry has a filename, so it is offered under that; the point is
	// that every row has a name and the title wins where there is one.
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3: %v", len(models), models)
	}
	if models[0].ID != "sdxl.safetensors [a1b2]" {
		t.Errorf("first model id = %q", models[0].ID)
	}
	if models[1].ID != "flux-dev" {
		t.Errorf("title-less model fell back wrong: %q", models[1].ID)
	}
}

func TestSDWebUIHealthAndEmptySamplersAreNotFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sdapi/v1/sd-models":
			fmt.Fprint(w, `[]`)
		case "/sdapi/v1/samplers":
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewSDWebUI("sd", "SD", server.URL, "")
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("a reachable server with no checkpoints is not unhealthy: %v", err)
	}
	samplers, err := client.Samplers(context.Background())
	if err != nil {
		t.Fatalf("Samplers: %v", err)
	}
	if len(samplers) != 0 {
		t.Fatalf("samplers = %v, want empty", samplers)
	}
}

func TestSDWebUISendsBasicAuthWhenConfigured(t *testing.T) {
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	client := NewSDWebUI("sd", "SD", server.URL, "user:secretpass")
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secretpass"))
	if header != want {
		t.Errorf("Authorization = %q, want %q", header, want)
	}
}

// A WebUI started without --api-auth takes no credential, and sending one anyway
// is a request it may refuse.
func TestSDWebUISendsNoAuthByDefault(t *testing.T) {
	var header string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	if err := NewSDWebUI("sd", "SD", server.URL, "").Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if header != "" {
		t.Errorf("Authorization = %q, want empty", header)
	}
}

// A rejected credential must not travel to a toast because the server repeated it
// back — which is exactly what a reverse proxy in front of a WebUI does.
func TestSDWebUIRedactsTheCredentialItSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `rejected credentials: %s`, r.Header.Get("Authorization"))
	}))
	defer server.Close()

	err := NewSDWebUI("sd", "SD", server.URL, "user:secretpass").
		Health(context.Background())
	if err == nil {
		t.Fatal("a 401 was reported as healthy")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("user:secretpass"))
	if strings.Contains(err.Error(), encoded) || strings.Contains(err.Error(), "secretpass") {
		t.Fatalf("the credential survived into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("nothing was redacted at all: %v", err)
	}
}

func TestSDWebUIRetriesATransientFailure(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A server that is loading a checkpoint answers 503 for a while and then
		// starts working. Retrying is what turns that into a picture instead of a
		// failure the user has to notice and repeat.
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"detail":"loading"}`)
			return
		}
		fmt.Fprintf(w, `{"images":["%s"]}`, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := NewSDWebUI("sd", "SD", server.URL, "").
		Generate(ctx, Request{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(result.Images))
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

func TestSDWebUINotConfigured(t *testing.T) {
	client := NewSDWebUI("sd", "SD", "", "")
	if err := client.Health(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Health err = %v, want ErrNotConfigured", err)
	}
	if _, err := client.Generate(context.Background(), Request{Prompt: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Generate err = %v, want ErrNotConfigured", err)
	}
}

func TestNormaliseDimension(t *testing.T) {
	cases := []struct {
		in, want int
		why      string
	}{
		{0, 0, "omitted stays omitted, so the server's own default applies"},
		{-64, 0, "a negative size is nonsense and must not become a request"},
		{20, 64, "below the smallest usable latent grid"},
		{512, 512, "already a multiple of 8"},
		{513, 512, "rounded to the nearest multiple of 8"},
		{515, 512, "rounded down when that is the nearer multiple"},
		{517, 520, "and rounded up when that is the nearer one"},
		{99999, 2048, "capped at something a local machine can finish"},
	}
	for _, tc := range cases {
		if got := normaliseDimension(tc.in); got != tc.want {
			t.Errorf("normaliseDimension(%d) = %d, want %d (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

func TestParseSeed(t *testing.T) {
	if ParseSeed("") != nil {
		t.Error("an empty seed must mean 'let the server choose', not seed 0")
	}
	if ParseSeed("-1") != nil {
		t.Error("-1 is the WebUI's own spelling of a random seed")
	}
	if ParseSeed("abc") != nil {
		t.Error("a malformed seed must fall back to random, not to zero")
	}
	got := ParseSeed(" 42 ")
	if got == nil || *got != 42 {
		t.Errorf("ParseSeed(42) = %v, want 42", got)
	}
}
