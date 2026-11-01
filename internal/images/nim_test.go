package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nimArtifact is the shape the hosted route answers with: base64 under artifacts[].
func nimArtifact() string {
	return `{"artifacts":[{"base64":"` + base64.StdEncoding.EncodeToString(pngBytes) + `","finishReason":"SUCCESS"}]}`
}

// TestNIMGenerateUsesTheHostedModelPath pins the route that took a real 404 to find: the
// picture hangs off `/v1/genai/<publisher>/<model>` on the vendor's hosted host, not off the
// OpenAI-compatible `/v1/images/generations` — that path exists but serves no image model, so
// a request there reads as a broken URL rather than as a wrong address in this client.
func TestNIMGenerateUsesTheHostedModelPath(t *testing.T) {
	var got map[string]any
	var path, method, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method, auth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nimArtifact())
	}))
	defer srv.Close()

	seed := int64(42)
	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", NIMModelSchnell)
	result, err := p.Generate(context.Background(), Request{
		Prompt: "a cat", Width: 1024, Height: 768, Steps: 4, Seed: &seed,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if path != "/v1/genai/black-forest-labs/flux.1-schnell" || method != http.MethodPost {
		t.Fatalf("route = %s %s, want POST /v1/genai/black-forest-labs/flux.1-schnell", method, path)
	}
	if auth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want the bearer token", auth)
	}
	for key, want := range map[string]any{
		"prompt": "a cat",
		// The vendor's own examples send mode "base" for the FLUX routes; a body without
		// it is a request the model may refuse.
		"mode":   "base",
		"width":  float64(1024),
		"height": float64(768),
		"steps":  float64(4),
		"seed":   float64(42),
	} {
		if got[key] != want {
			t.Errorf("%s = %#v, want %#v", key, got[key], want)
		}
	}
	// The OpenAI route's spellings have no place on this one.
	for _, absent := range []string{"size", "n", "response_format", "sampler_name", "negative_prompt"} {
		if _, ok := got[absent]; ok {
			t.Errorf("request carries %q, which the hosted route does not take", absent)
		}
	}

	if len(result.Images) != 1 {
		t.Fatalf("got %d pictures, want 1", len(result.Images))
	}
	if string(result.Images[0].Data) != string(pngBytes) {
		t.Errorf("picture bytes differ from the answer")
	}
	if result.Images[0].Mime != MimePNG || result.Images[0].Ext != "png" {
		t.Errorf("format = %s/%s, want png — the type is read off the bytes", result.Images[0].Mime, result.Images[0].Ext)
	}
	if result.Seed != 42 {
		t.Errorf("seed = %d, want the seed that was sent", result.Seed)
	}
	if result.Model != NIMModelSchnell {
		t.Errorf("model = %q, want the configured one", result.Model)
	}
}

// TestNIMDefaultsToSchnellAndChoosesASeed covers the two values a fresh settings blob does
// not carry: the model id and the seed. A picture whose seed was never recorded cannot be
// drawn again, which is why the client picks one rather than leaving the choice to a service
// that does not report it back.
func TestNIMDefaultsToSchnellAndChoosesASeed(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, nimArtifact())
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "")
	result, err := p.Generate(context.Background(), Request{Prompt: "a cat"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if path != "/v1/genai/"+NIMModelSchnell {
		t.Errorf("path = %q, want the default model's own route", path)
	}
	seed, ok := got["seed"].(float64)
	if !ok || seed <= 0 {
		t.Fatalf("seed = %#v, want a positive seed sent with the request", got["seed"])
	}
	if result.Seed != int64(seed) {
		t.Errorf("reported seed %d != sent seed %d", result.Seed, int64(seed))
	}
	// An unchosen number is omitted rather than invented: zero means "the model's own".
	for _, absent := range []string{"width", "height", "steps", "cfg_scale"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s was sent for a request that named none: %#v", absent, got[absent])
		}
	}
}

// TestNIMAcceptsBothAnswerShapes: the hosted route answers under artifacts[], and a model
// behind the same host can answer in the OpenAI shape. Both carry bytes, and the bytes decide
// the format either way.
func TestNIMAcceptsBothAnswerShapes(t *testing.T) {
	cases := map[string]string{
		"artifacts": `{"artifacts":[{"base64":"` + base64.StdEncoding.EncodeToString(pngBytes) + `"}]}`,
		"data":      `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(pngBytes) + `"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "")
			result, err := p.Generate(context.Background(), Request{Prompt: "a cat"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(result.Images) != 1 || string(result.Images[0].Data) != string(pngBytes) {
				t.Fatalf("the picture was not read out of the %s envelope", name)
			}
			if result.Images[0].Mime != MimePNG {
				t.Errorf("format = %q, want the type sniffed from the bytes", result.Images[0].Mime)
			}
		})
	}
}

// TestNIMFetchesAURLAnswer covers the third shape the answer can take: a stored file instead
// of inline bytes.
func TestNIMFetchesAURLAnswer(t *testing.T) {
	file := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngBytes)
	}))
	defer file.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"url":"`+file.URL+`/picture.png"}]}`)
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "")
	result, err := p.Generate(context.Background(), Request{Prompt: "a cat"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Images) != 1 || string(result.Images[0].Data) != string(pngBytes) {
		t.Fatalf("the stored picture was not fetched")
	}
}

// TestNIMRefusesAHiresPass is the correctness rule for the second sampling pass: the hosted
// route has none, and a 200 at base size would read as a successful enlargement.
func TestNIMRefusesAHiresPass(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "")
	_, err := p.Generate(context.Background(), Request{
		Prompt: "a cat", Hires: Hires{Enabled: true, Scale: 1.5, Denoise: 0.4},
	})
	if !errors.Is(err, ErrHiresUnsupported) {
		t.Fatalf("err = %v, want ErrHiresUnsupported", err)
	}
	if called {
		t.Error("the request was sent even though the pass cannot run")
	}
}

// TestNIMWithoutAKeyDoesNotCallOut covers the state a user reaches by choosing the hosted
// kind before adding a key: an error that names the missing credential, and no request.
func TestNIMWithoutAKeyDoesNotCallOut(t *testing.T) {
	p := NewNIM("images", "NVIDIA NIM", "https://example.invalid", "", "")
	if err := p.Health(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Health err = %v, want ErrNotConfigured", err)
	}
	if _, err := p.Generate(context.Background(), Request{Prompt: "a cat"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Generate err = %v, want ErrNotConfigured", err)
	}
}

// TestNIMRejectedKeyIsNamedAndRedacted: a 401 is a different repair from an outage, and the
// body a gateway echoes can contain the key that is shown in a toast.
func TestNIMRejectedKeyIsNamedAndRedacted(t *testing.T) {
	const key = "nvapi-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid key `+key+`"}`)
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, key, "")
	p.keyCheck = srv.URL + "/v1/models"
	err := p.Health(context.Background())
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("err = %v, want ErrKeyRejected", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the key reached the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("echoed body was not marked: %v", err)
	}
}

// TestNIMTimeoutSaysWhichWaitEnded is the failure this route produced in practice: the vendor
// accepts the request and then does not answer. A bare "context deadline exceeded" reads as a
// bug in KiChat, so the sentence names the wait and the Stop button — while a user's own Stop
// stays the cancellation it is.
func TestNIMTimeoutSaysWhichWaitEnded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Longer than the caller's budget, and short enough that the test finishes.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "")
	p.Lang = german
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := p.Generate(ctx, Request{Prompt: "a cat"})
	if err == nil {
		t.Fatal("a request that never answered reported success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "Zeitüberschreitung") {
		t.Errorf("the error does not say what happened: %v", err)
	}

	// A cancelled context is the Stop button, not a timeout: it must stay that answer.
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := p.Generate(cctx, Request{Prompt: "a cat"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation left alone", err)
	}
}

// TestNIMUnknownModelIsNamedAsSuch is the error the 404 that found this route produced: it
// has to say the model is not served, not that the address is wrong, because the repair is the
// model picker — and it has to name the models this build offers, because the picker can be
// showing something else entirely (a stored id no `<option>` matches displays the first one).
func TestNIMUnknownModelIsNamedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "404 page not found")
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", "some/model-that-is-not-hosted")
	_, err := p.Generate(context.Background(), Request{Prompt: "a cat"})
	if !errors.Is(err, ErrModelNotServed) {
		t.Fatalf("err = %v, want ErrModelNotServed", err)
	}
	if !strings.Contains(err.Error(), "some/model-that-is-not-hosted") {
		t.Errorf("the error does not name the model: %v", err)
	}
	for _, id := range NIMModels {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the error does not name the offered model %q: %v", id, err)
		}
	}
}

// TestNIMKleinAsksForItsOwnMode pins the per-model `mode`: FLUX.3 Klein refuses the FLUX.1
// spelling with a 422 (`Input should be 't2i' or 'i2i'`), so one constant for the service would
// make one of the two models unusable.
func TestNIMKleinAsksForItsOwnMode(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, nimArtifact())
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", NIMModelKlein)
	result, err := p.Generate(context.Background(), Request{Prompt: "a cat", Width: 1024, Height: 1024})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if path != "/v1/genai/"+NIMModelKlein {
		t.Errorf("path = %q, want the klein route", path)
	}
	if got["mode"] != "t2i" {
		t.Errorf("mode = %#v, want \"t2i\" — klein rejects \"base\"", got["mode"])
	}
	if result.Model != NIMModelKlein {
		t.Errorf("model = %q, want klein", result.Model)
	}
}

// TestNIMContentFilterIsNamed covers an answer that is 200 and still has no picture: the vendor
// reports `finishReason: "CONTENT_FILTERED"` with an empty artifact, which was measured on a
// harmless prompt. "Answered without a picture" would read as a bug here; the reason is what a
// user can act on.
func TestNIMContentFilterIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"artifacts":[{"base64":"","finishReason":"CONTENT_FILTERED","seed":903473513}]}`)
	}))
	defer srv.Close()

	p := NewNIM("images", "NVIDIA NIM", srv.URL, "sk-test", NIMModelKlein)
	_, err := p.Generate(context.Background(), Request{Prompt: "a red bicycle"})
	if err == nil {
		t.Fatal("an answer with no picture reported success")
	}
	if !strings.Contains(err.Error(), "CONTENT_FILTERED") {
		t.Errorf("the error does not carry the vendor's reason: %v", err)
	}
	if strings.Contains(err.Error(), "without a picture") {
		t.Errorf("the reason was replaced by the generic sentence: %v", err)
	}
}

// TestNIMModelsAreImageModelsOnly pins the catalogue the panel offers: the hosted listing is
// the chat catalogue, so the list here is the image subset, named by the path the route takes.
func TestNIMModelsAreImageModelsOnly(t *testing.T) {
	p := NewNIM("images", "NVIDIA NIM", "", "sk-test", "")
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
		if !strings.Contains(m.ID, "/") {
			t.Errorf("%q is not a publisher/model path, so the route would 404", m.ID)
		}
	}
	if !seen[NIMModelSchnell] {
		t.Errorf("the default model is missing from %v", seen)
	}
	if samplers, err := p.Samplers(context.Background()); err != nil || len(samplers) != 0 {
		t.Errorf("Samplers = %v/%v, want none: the hosted route has no sampler", samplers, err)
	}
}

func TestNIMRecordsReportedSeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"artifacts":[{"base64":%q,"seed":17}]}`, base64.StdEncoding.EncodeToString(pngBytes))
	}))
	defer srv.Close()
	seed := int64(42)
	result, err := NewNIM("images", "NVIDIA", srv.URL, "key", "").Generate(t.Context(), Request{Prompt: "cat", Seed: &seed})
	if err != nil || result.Seed != 17 {
		t.Fatalf("result seed = %d, err = %v; want the seed the server reported", result.Seed, err)
	}
}

func TestNIMFetchesFullSizedImagesAndRejectsOversizedOnes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      int
		wantError bool
	}{
		{"larger than a preview", 5 << 20, false},
		{"over the image limit", (32 << 20) + 1, true},
		{"empty", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.CopyN(w, zeroReader{}, int64(tc.size))
			}))
			defer file.Close()
			p := NewNIM("images", "NVIDIA", file.URL, "key", "")
			data, err := p.picture(t.Context(), "", file.URL)
			if tc.wantError {
				if err == nil {
					t.Fatal("invalid response accepted")
				}
			} else if err != nil || len(data) != tc.size {
				t.Fatalf("got %d bytes, %v; want %d bytes", len(data), err, tc.size)
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
