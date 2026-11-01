package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	ilang "kichat/internal/lang"
)

// NIMBaseURL is where build.nvidia.com serves its hosted image models. The route is
// `/v1/genai/<publisher>/<model>`, not the OpenAI-compatible `/v1/images/generations`.
//
// That distinction cost a real 404. NVIDIA documents an OpenAI-compatible image route, but
// its Getting Started page runs it on the *self-hosted* container
// (`http://localhost:8000/v1/images/generations`). The hosted catalogue answers on this host
// instead: measured on 22 October 2026 with a valid key,
// `POST https://integrate.api.nvidia.com/v1/images/generations` returns 400 "model field is
// required" for an empty body and a plain `404 page not found` for every documented model
// name, while `/v1/genai/black-forest-labs/flux.1-schnell` and `…/flux.1-dev` are routed
// (a bad key gets 401 there, and any other publisher path gets 404).
//
// The address is a constant rather than a settings field: it is the vendor's, never the
// user's, and a stored copy is one more value that can go stale.
const NIMBaseURL = "https://ai.api.nvidia.com"

// NIMKeyCheckURL proves a credential without drawing anything. The hosted model listing on
// the vendor's OpenAI-compatible host answers 200 for a valid key and 401 for a bad one, and
// it is cheap — which is what a health probe has to be when the alternative is a billed (or
// queued) picture.
const NIMKeyCheckURL = "https://integrate.api.nvidia.com/v1/models"

// The hosted image models, named by their path segment after `/v1/genai/`. They are the ones
// this route was measured to serve; another id is a 404 until the vendor registers it, which
// is why the panel does not offer a longer list of guessed names. The measurement that
// separates a registered path from an invented one is cheap: an unregistered path answers
// `404 page not found` before any auth, while a registered one answers 401/403 for a junk key.
//
// NIMModelSchnell is the shipped default: Apache 2.0, four steps, and the one model whose
// output this app may sit behind a commercial feature. FLUX.1 [dev] is selectable because a
// user's own account may use it, but its model licence is non-commercial for production.
//
// NIMModelKlein is the one measured to draw on a free-trial key: 5.1 s for a 1024×1024 JPEG on
// 22 October 2026, where both FLUX.1 routes had still not answered after 240 s. Its weights are
// under the FLUX Kommunity License — free commercial use below US$5 M of annual revenue — which
// is why it is offered beside schnell rather than shipped as the default.
const (
	NIMModelSchnell = "black-forest-labs/flux.1-schnell"
	NIMModelKlein   = "black-forest-labs/flux-3-image-klein"
	NIMModelDev     = "black-forest-labs/flux.1-dev"
)

// NIMModels are the image ids this client offers, in the order the panel shows them.
var NIMModels = []string{NIMModelSchnell, NIMModelKlein, NIMModelDev}

// nimModes names the `mode` each route insists on. It is per model, not per service: FLUX.3
// Klein rejects the FLUX.1 value outright — measured, `mode: "base"` on its route answers
// `422 {"detail":[{"type":"literal_error","loc":["body","mode"],"msg":"Input should be 't2i' or 'i2i'"}]}`
// in under a second — while the FLUX.1 routes take "base". A single constant would therefore
// make one of the two models unusable, so the mode travels with the id.
var nimModes = map[string]string{NIMModelKlein: "t2i"}

// nimMode is the mode a model's route takes, defaulting to the FLUX.1 spelling.
func nimMode(model string) string {
	if mode := nimModes[model]; mode != "" {
		return mode
	}
	return "base"
}

// NIM draws through build.nvidia.com's hosted inference: one HTTPS call per picture, one
// bearer token, and a base64 picture back under `artifacts[].base64`.
//
// It is a separate provider from SDWebUI rather than a fourth server kind behind the
// `/sdapi/v1/txt2img` route, because nothing about that route applies: no checkpoint to
// switch, no sampler list, no progress route, and one token instead of a "user:pass" pair.
// What it does share is the shape of the answer (base64 under a JSON key) and the bounds in
// limits.go.
//
// Three things the hosted route cannot do, and each is said rather than approximated: there
// is no second sampling pass (so Generate refuses a hires request instead of returning a
// base-size picture), no progress while the request runs, and no sampler to pick.
type NIM struct {
	id      string
	name    string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client

	// Lang answers the interface language for the sentences this client writes
	// (images.WithLang). Nil means English.
	Lang func() string
	// keyCheck is where Health asks whether the credential works. It is a field rather than
	// the constant alone so a test can answer it without reaching the vendor; nothing in the
	// app sets it.
	keyCheck string
}

// nimClient bounds a hosted draw by the caller's context, like the WebUI client: a picture
// takes seconds to a minute, and on the free trial a queued one can take much longer, so a
// fixed total timeout here would fail the slow ones. Stop is what ends a wait that is merely
// long.
var nimClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 10 * time.Minute,
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
	},
}

// NewNIM builds a client for the hosted API. model empty is the schnell default, so a
// settings blob that predates the field still draws.
func NewNIM(id, name, baseURL, apiKey, model string) *NIM {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = NIMBaseURL
	}
	chosen := strings.TrimSpace(model)
	if chosen == "" {
		chosen = NIMModelSchnell
	}
	return &NIM{
		id:       id,
		name:     name,
		baseURL:  base,
		apiKey:   strings.TrimSpace(apiKey),
		model:    chosen,
		client:   nimClient,
		keyCheck: NIMKeyCheckURL,
	}
}

func (n *NIM) ID() string   { return n.id }
func (n *NIM) Name() string { return n.name }

// ErrKeyRejected marks a 401 or 403 from the hosted API: the key is wrong, missing or not
// entitled to the model, which is a different repair from "the service is down".
var ErrKeyRejected = errors.New("the NVIDIA API key was rejected")

// ErrModelNotServed marks a 404 from the model's own route: the id names a path the hosted
// catalogue does not publish. It is separate from Unavailable because the remedy is the model
// picker, not a key or a network.
var ErrModelNotServed = errors.New("NVIDIA does not serve that model at its hosted address")

// Health checks the credential against the vendor's model listing: an unauthenticated call
// answers 401 there, so a key typed into the wrong backend cannot pass as healthy.
func (n *NIM) Health(ctx context.Context) error {
	if n.apiKey == "" {
		return fmt.Errorf("%w: no key is stored", ErrNotConfigured)
	}
	check := n.keyCheck
	if check == "" {
		check = NIMKeyCheckURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, check, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.apiKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	httpErr := &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       n.redact(strings.TrimSpace(string(raw))),
		URL:        check,
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %v", ErrKeyRejected, httpErr)
	}
	return httpErr
}

// Models returns the catalogue this client offers, without asking the service: the hosted
// listing is the *chat* catalogue, and a picker built from it would offer a text model as a
// checkpoint. Health is what proves the account really serves them.
func (n *NIM) Models(context.Context) ([]Model, error) {
	out := make([]Model, 0, len(NIMModels))
	for _, id := range NIMModels {
		out = append(out, Model{ID: id, Name: id, Type: "hosted"})
	}
	return out, nil
}

// Samplers is empty by design: the hosted route has no sampler parameter, and inventing a
// list here would offer the panel values that are silently dropped from the request.
func (n *NIM) Samplers(context.Context) ([]string, error) { return nil, nil }

// nimRequest is the hosted generation payload. `mode` is not optional for the FLUX routes and
// its legal values differ per model (see nimModes), and width/height are separate fields here
// rather than the OpenAI route's `"1024x1024"` string, because this is the vendor's native
// shape and not the compatibility shim.
//
// A field nobody chose is omitted rather than sent as a default: the model's own value is the
// right answer for a number this app did not decide, the same rule every other backend here
// follows.
type nimRequest struct {
	Prompt   string  `json:"prompt"`
	Mode     string  `json:"mode"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Steps    int     `json:"steps,omitempty"`
	CfgScale float64 `json:"cfg_scale,omitempty"`
	// Seed is omitted when nobody chose one. The client picks one before sending (see
	// Generate), because a picture whose seed is unknown cannot be drawn again.
	Seed *int64 `json:"seed,omitempty"`
}

// nimResponse is the answer: base64 under `artifacts[].base64`. The `data[]` half is accepted
// as well, because a model behind the same host can answer in the OpenAI shape — and the
// bytes, not the key they arrived under, are what the user asked for.
type nimResponse struct {
	Artifacts []struct {
		Base64       string `json:"base64"`
		Seed         int64  `json:"seed"`
		FinishReason string `json:"finishReason"`
	} `json:"artifacts"`
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	// Detail carries the vendor's refusal text for a request it accepted but would not run.
	Detail string `json:"detail"`
}

// Generate draws one picture. A second sampling pass is refused rather than ignored: the
// hosted route has no img2img pass, and a 200 at base size would look like a successful
// enlargement to anything that only read the status.
// lang is the resolved model language for the sentences this client writes.
func (n *NIM) lang() string { return ilang.Of(n.Lang) }

func (n *NIM) Generate(ctx context.Context, req Request) (Result, error) {
	if n.apiKey == "" {
		return Result{}, fmt.Errorf("%w: no key is stored", ErrNotConfigured)
	}
	req = req.Normalised()
	if req.Hires.Enabled {
		return Result{}, ErrHiresUnsupported
	}
	prompt := req.EffectivePrompt()
	if prompt == "" {
		return Result{}, errors.New(text(req.Lang, "der Bild-Prompt ist leer", "the image prompt is empty"))
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = n.model
	}
	seed := req.Seed
	if seed == nil {
		chosen := RandomSeed()
		seed = &chosen
	}
	payload := nimRequest{
		Prompt:   prompt,
		Mode:     nimMode(model),
		Width:    req.Width,
		Height:   req.Height,
		Steps:    req.Steps,
		CfgScale: req.CfgScale,
		Seed:     seed,
	}

	started := time.Now()
	var decoded nimResponse
	if err := n.do(ctx, http.MethodPost, n.modelPath(model), payload, &decoded); err != nil {
		return Result{}, n.explain(err)
	}

	result := Result{Seed: *seed, Model: model, DurationMs: time.Since(started).Milliseconds()}
	// filtered is the vendor's own word for a picture its safety pass removed: the route answers
	// 200 with an empty artifact and `finishReason: "CONTENT_FILTERED"`. Measured on a benign
	// prompt ("a red bicycle leaning on a brick wall"), so it is a state a user can reach
	// without trying — and "answered without a picture" would read as a bug in this client.
	filtered := ""
	for _, artifact := range decoded.Artifacts {
		if strings.TrimSpace(artifact.Base64) == "" {
			if reason := strings.TrimSpace(artifact.FinishReason); reason != "" && reason != "SUCCESS" {
				filtered = reason
			}
			continue
		}
		data, err := n.picture(ctx, artifact.Base64, "")
		if err != nil {
			return Result{}, err
		}
		mime, ext := SniffImage(data, MimePNG, "png")
		if len(result.Images) == 0 && artifact.Seed > 0 {
			result.Seed = artifact.Seed
		}
		result.Images = append(result.Images, Image{Data: data, Mime: mime, Ext: ext})
	}
	for _, item := range decoded.Data {
		data, err := n.picture(ctx, item.B64JSON, item.URL)
		if err != nil {
			return Result{}, err
		}
		mime, ext := SniffImage(data, MimePNG, "png")
		result.Images = append(result.Images, Image{Data: data, Mime: mime, Ext: ext})
	}
	if len(result.Images) == 0 {
		if filtered != "" {
			return Result{}, fmt.Errorf(text(n.lang(),
				"NVIDIA hat das Bild verworfen (%s) — der Prompt hat die Prüfung des Dienstes "+
					"nicht passiert; ein anderer Prompt hilft",
				"NVIDIA rejected the picture (%s) — the prompt did not pass the service's "+
					"filter; a different prompt helps"), filtered)
		}
		if detail := strings.TrimSpace(decoded.Detail); detail != "" {
			return Result{}, fmt.Errorf(text(n.lang(),
				"NVIDIA hat kein Bild geliefert: %s", "NVIDIA delivered no picture: %s"), detail)
		}
		return Result{}, errors.New("NVIDIA answered without a picture")
	}
	return result, nil
}

// explain names the one failure this route produced in practice: a request the vendor accepts
// and then does not answer. The free trial queues its models, and a bare "context deadline
// exceeded" reads as a bug in KiChat rather than as a wait that ended — so the sentence says
// which wait it was and that Stop is the way out of it.
func (n *NIM) explain(err error) error {
	// A 404 names the id that was sent — which the picker may not be showing, if the stored
	// value predates the list — so the sentence also names what this build offers. Without it,
	// "this model is not served" leaves a user whose panel says flux.1 looking at a URL that
	// says something else.
	if errors.Is(err, ErrModelNotServed) {
		return fmt.Errorf("%w — this build offers %s; pick one in Settings → Image generation", err, strings.Join(NIMModels, " and "))
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Cancelled is the Stop button, which the caller already reports as such.
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf(text(n.lang(),
			"NVIDIA hat nicht geantwortet (Zeitüberschreitung) — die gehostete Warteschlange kann "+
				"lange sein; der Stop-Knopf beendet das Warten: %w",
			"NVIDIA did not answer (timeout) — the hosted queue can be long; the Stop button ends "+
				"the wait: %w"), err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf(text(n.lang(),
			"NVIDIA war nicht erreichbar (Zeitüberschreitung beim Verbinden): %w",
			"NVIDIA could not be reached (timeout while connecting): %w"), err)
	}
	return err
}

// modelPath is the route one model id hangs off. A bare name with no publisher is still
// routed as a path segment, so a user who types one gets the vendor's own 404 rather than a
// request this app refused to send.
func (n *NIM) modelPath(model string) string {
	return "/v1/genai/" + strings.TrimPrefix(strings.TrimSpace(model), "/")
}

// picture turns one answer entry into bytes: base64 when it carries any, otherwise the URL
// the model stored the file at.
func (n *NIM) picture(ctx context.Context, b64, url string) ([]byte, error) {
	if strings.TrimSpace(b64) != "" {
		data, err := decodeImage(b64)
		if err != nil {
			return nil, fmt.Errorf(text(n.lang(),
				"das Bild von NVIDIA war nicht dekodierbar: %w",
				"NVIDIA's picture could not be decoded: %w"), err)
		}
		return data, nil
	}
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("NVIDIA answered without picture bytes")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: "", URL: url}
	}
	// A finished image can be much larger than a preview. Read one extra byte so an
	// oversized response is rejected instead of silently saving a truncated picture.
	const maxPictureBytes = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPictureBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPictureBytes {
		return nil, errors.New(text(n.lang(), "das Bild von NVIDIA ist größer als 32 MB", "NVIDIA's picture exceeds 32 MiB"))
	}
	if len(data) == 0 {
		return nil, errors.New(text(n.lang(), "NVIDIA hat ein leeres Bild geliefert", "NVIDIA returned an empty picture"))
	}
	return data, nil
}

// do sends one request with the bearer token and classifies its failures.
func (n *NIM) do(ctx context.Context, method, path string, body any, out any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, n.once(ctx, method, path, body, out)
	})
	return err
}

func (n *NIM) once(ctx context.Context, method, path string, body any, out any) error {
	url := n.baseURL + path
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.apiKey)
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		httpErr := &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       n.redact(strings.TrimSpace(string(raw))),
			URL:        url,
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %v", ErrKeyRejected, httpErr)
		case http.StatusNotFound:
			// This route answers a plain "404 page not found" for a model it does not
			// publish, which reads as a broken URL rather than as a model id the picker
			// should be showing instead.
			return fmt.Errorf("%w: %v", ErrModelNotServed, httpErr)
		}
		return httpErr
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// redact removes the key from a body that echoed it back. A gateway in front of the API can
// repeat the Authorization header it refused, and that body travels to a toast.
func (n *NIM) redact(body string) string {
	if n.apiKey == "" {
		return body
	}
	return strings.ReplaceAll(body, n.apiKey, "[redacted]")
}
