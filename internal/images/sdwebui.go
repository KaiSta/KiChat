package images

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SDWebUI talks to a Stable Diffusion WebUI's HTTP API.
//
// One client covers three servers, which is why this is the first image backend
// rather than one of many: AUTOMATIC1111's WebUI, its Forge fork and
// stable-diffusion.cpp's `sd-server` all serve the same `/sdapi/v1/txt2img` route
// with the same JSON shape and answer with base64 in `images[]`. ComfyUI is not
// covered by it — it takes a workflow graph rather than a prompt, and needs a
// submit-then-poll-then-fetch dance — so it would be a second client, not a branch
// in this one.
//
// The route is spelled out from AUTOMATIC1111's own API wiki
// (https://github.com/AUTOMATIC1111/stable-diffusion-webui/wiki/API); Forge
// documents that its endpoints are the same, and stable-diffusion.cpp documents
// `/sdapi/v1/txt2img` as a compatibility route beside its native one.
type SDWebUI struct {
	id      string
	name    string
	baseURL string
	// basicAuth is the "user:pass" a WebUI started with `--api-auth` requires. It
	// is empty for the default setup, which has no authentication at all — that is
	// not an oversight in this client, it is what the server ships with.
	basicAuth string
	client    *http.Client
}

// sdwebuiClient has no total timeout. A 1024x1024 picture at 30 steps on a
// mid-range card takes tens of seconds and on the CPU takes minutes, so the only
// bound that makes sense is the caller's context — which is the turn's, and which
// Stop cancels.
var sdwebuiClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Minute,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	},
}

// NewSDWebUI builds a client for one WebUI-compatible server. basicAuth is the
// "user:pass" pair the server was started with, or empty.
func NewSDWebUI(id, name, baseURL, basicAuth string) *SDWebUI {
	return &SDWebUI{
		id:        id,
		name:      name,
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		basicAuth: strings.TrimSpace(basicAuth),
		client:    sdwebuiClient,
	}
}

func (s *SDWebUI) ID() string   { return s.id }
func (s *SDWebUI) Name() string { return s.name }

// Health asks for the model list, which is the cheapest route that proves the API
// is up. A WebUI with no checkpoint installed still answers it, and deliberately
// counts as healthy: the server is running, and "there is nothing to draw with"
// is a different sentence from "there is nothing listening".
func (s *SDWebUI) Health(ctx context.Context) error {
	if s.baseURL == "" {
		return ErrNotConfigured
	}
	var models []sdModel
	if err := s.get(ctx, "/sdapi/v1/sd-models", &models); err != nil {
		return err
	}
	return nil
}

// Interrupt asks the server to stop the generation it is currently running.
//
// The route is `POST /sdapi/v1/interrupt`, registered by AUTOMATIC1111
// (`self.add_api_route("/sdapi/v1/interrupt", self.interruptapi, methods=["POST"])`,
// whose handler calls `shared.state.interrupt()` and answers `{}`) and by the Forge
// fork with the same registration. stable-diffusion.cpp's server has no equivalent:
// its own API cancels a *job* by id, and this client does not submit jobs, so a stop
// against it ends the wait and nothing else. That is why this is best effort and why
// the caller treats its failure as information rather than as a failed stop.
//
// What it interrupts is the server's current generation, not this request. The state
// it flips is global to the process — a picture the user is drawing in their own WebUI
// tab stops too. KiChat draws one picture at a time against a server the user runs for
// themselves, so the window is narrow, but a caller pressing Stop should know that is
// what it means.
//
// Deliberately not retried, unlike every other route here: by the time this is sent the
// draw it refers to has already been abandoned by the caller's context, so a second
// attempt can only interrupt whatever the server happens to be doing by then — a later
// generation this click did not mean to touch. One attempt, one error, and no second
// picture's problem.
func (s *SDWebUI) Interrupt(ctx context.Context) error {
	if s.baseURL == "" {
		return ErrNotConfigured
	}
	return s.do(ctx, http.MethodPost, "/sdapi/v1/interrupt", nil, nil)
}

// sdModel is one entry of `/sdapi/v1/sd-models`. Only three of its fields are
// read: the API returns a `config` blob per model that is large, ignored, and
// (on some builds) not valid JSON in the first place, so it is left undecoded.
type sdModel struct {
	Title     string `json:"title"`
	ModelName string `json:"model_name"`
	Filename  string `json:"filename"`
}

// Models lists the installed checkpoints. `title` is the name the API accepts
// back in an override, so it is what both ID and Name carry: the filename is not
// necessarily the same string, and sending the wrong one silently draws with the
// previously loaded model instead of failing.
func (s *SDWebUI) Models(ctx context.Context) ([]Model, error) {
	if s.baseURL == "" {
		return nil, ErrNotConfigured
	}
	var raw []sdModel
	if err := s.get(ctx, "/sdapi/v1/sd-models", &raw); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(raw))
	for _, m := range raw {
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = strings.TrimSpace(m.ModelName)
		}
		if title == "" {
			title = strings.TrimSpace(m.Filename)
		}
		if title == "" {
			continue
		}
		out = append(out, Model{ID: title, Name: title, Type: "checkpoint"})
	}
	return out, nil
}

// sdSampler is one entry of `/sdapi/v1/samplers`; only `name` is used.
type sdSampler struct {
	Name string `json:"name"`
}

// Samplers lists the sampler names the server accepts. An empty list is a normal
// answer from a server too old or too stripped down to publish one, and means
// "let the server pick" rather than a failure.
func (s *SDWebUI) Samplers(ctx context.Context) ([]string, error) {
	if s.baseURL == "" {
		return nil, ErrNotConfigured
	}
	var raw []sdSampler
	if err := s.get(ctx, "/sdapi/v1/samplers", &raw); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if name := strings.TrimSpace(item.Name); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// sdRequest is the `/sdapi/v1/txt2img` payload. Only the keys this app sets are
// declared; the API fills in everything else from the server's own defaults,
// which is what makes the omitted fields a real "leave it to the user's WebUI"
// rather than a re-implementation of its settings.
type sdRequest struct {
	Prompt         string         `json:"prompt"`
	NegativePrompt string         `json:"negative_prompt,omitempty"`
	Width          int            `json:"width,omitempty"`
	Height         int            `json:"height,omitempty"`
	Steps          int            `json:"steps,omitempty"`
	SamplerName    string         `json:"sampler_name,omitempty"`
	Seed           *int64         `json:"seed,omitempty"`
	Override       map[string]any `json:"override_settings,omitempty"`
}

// sdResponse is the txt2img answer. `images` holds base64 with no data: prefix —
// but an older build prefixed it anyway, so the decoder accepts both.
type sdResponse struct {
	Images []string        `json:"images"`
	Info   json.RawMessage `json:"info"`
}

// Generate draws the requested pictures and decodes them.
func (s *SDWebUI) Generate(ctx context.Context, req Request) (Result, error) {
	if s.baseURL == "" {
		return Result{}, ErrNotConfigured
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Result{}, errors.New("der Bild-Prompt ist leer")
	}
	// The numbers are bounded here as well as by the caller that built the request; see
	// limits.go. A caller that forgets cannot make this client the one that sends a
	// server a 20000-pixel request.
	req = req.Normalised()

	// The seed is chosen here rather than left to the server, and that is a
	// correctness choice rather than a preference. Only the seed that was *used*
	// makes a picture reproducible, and not every server sharing this route reports
	// it: A1111 repeats it in `info`, while sd-server's compatibility route documents
	// `info` as an empty string. A request that left the seed open would then produce
	// a picture whose seed KiChat can never learn — recorded as "none", which is the
	// same row as a picture that cannot be drawn again. Choosing it here makes the
	// effective seed known before the request is sent, and turns `info` into a
	// confirmation instead of the only source of the truth.
	seed := req.Seed
	if seed == nil {
		chosen := RandomSeed()
		seed = &chosen
	}

	payload := sdRequest{
		Prompt:         prompt,
		NegativePrompt: strings.TrimSpace(req.NegativePrompt),
		Width:          req.Width,
		Height:         req.Height,
		Steps:          req.Steps,
		SamplerName:    strings.TrimSpace(req.Sampler),
		Seed:           seed,
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		payload.Override = map[string]any{"sd_model_checkpoint": model}
	}

	var decoded sdResponse
	if err := s.post(ctx, "/sdapi/v1/txt2img", payload, &decoded); err != nil {
		if errors.Is(err, errNoModelLoaded) {
			return Result{}, fmt.Errorf("%w: %v", ErrNoModel, err)
		}
		return Result{}, err
	}
	if len(decoded.Images) == 0 {
		// The route answered with no picture and no error, which is what a WebUI
		// does when its model is missing or still loading. Saying so beats
		// returning an empty result the caller would have to interpret.
		return Result{}, ErrNoModel
	}

	result := Result{}
	for _, encoded := range decoded.Images {
		data, err := decodeImage(encoded)
		if err != nil {
			return Result{}, fmt.Errorf("das Bild des Servers war nicht dekodierbar: %w", err)
		}
		// The route returns PNG by default, but the format follows the server's own
		// `samples_format` — so it is read off the bytes instead of assumed. A
		// picture filed under the wrong type is a file that only opens in viewers
		// that sniff, and it is offered under the wrong filter when it is saved.
		mime, ext := SniffImage(data, MimePNG, "png")
		result.Images = append(result.Images, Image{Data: data, Mime: mime, Ext: ext})
	}
	info := decodeInfo(decoded.Info)
	// What the server says it used wins, because that is the number that reproduces
	// the picture; the seed this client sent is the fallback for a server that
	// reports nothing at all.
	result.Seed = info.seed
	if result.Seed == 0 {
		result.Seed = *seed
	}
	result.Model = info.model
	result.Info = info.text
	return result, nil
}

// RandomSeed picks the seed for a request that did not name one.
//
// Positive and non-zero on purpose: zero is this package's spelling of "no seed is
// known", so a picture drawn with seed 0 and a picture whose seed was never learned
// would be one row in the transcript — and only one of them can be drawn again. The
// ceiling keeps the value inside what every server in the table accepts, which is
// also the range stable-diffusion.cpp's own random path produces (`resolve_seed`
// returns `rand()`, a 31-bit value).
func RandomSeed() int64 {
	return 1 + rand.Int64N(math.MaxInt32)
}

// errNoModelLoaded is the WebUI's own answer when nothing is loaded. It is
// matched on the body text because the API reports it as a plain 404 or 500 on a
// route that exists, so the status alone does not distinguish it from a wrong
// address — and the two need different words in the UI.
var errNoModelLoaded = errors.New("no model loaded")

// sdInfo is the part of the `info` field this app reads. The field is a JSON
// *string* on AUTOMATIC1111 (a JSON document nested inside the response) and was
// an object on some forks, so both shapes are accepted.
type sdInfo struct {
	seed  int64
	model string
	text  string
}

func decodeInfo(raw json.RawMessage) sdInfo {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return sdInfo{}
	}
	text := string(trimmed)
	// A quoted string: strip the quotes and use its contents as the JSON object.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return sdInfo{text: text}
		}
		text = inner
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return sdInfo{text: text}
	}
	info := sdInfo{text: text}
	if n, ok := fields["seed"].(float64); ok {
		info.seed = int64(n)
	}
	for _, key := range []string{"sd_model_checkpoint", "model", "model_name"} {
		if value, ok := fields[key].(string); ok && strings.TrimSpace(value) != "" {
			info.model = value
			break
		}
	}
	return info
}

// decodeImage accepts base64 with or without a data: prefix. Older builds of the
// WebUI returned the prefixed form, and a caller that assumes one shape produces a
// corrupt file from the other rather than an error: a `data:` URL contains `:` and
// `,`, which are not in the base64 alphabet, so decoding one always fails — the
// prefix has to be stripped rather than tolerated.
func decodeImage(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if idx := strings.Index(encoded, "base64,"); idx >= 0 && strings.HasPrefix(encoded, "data:") {
		encoded = encoded[idx+len("base64,"):]
	}
	return base64.StdEncoding.DecodeString(encoded)
}

// --- HTTP ------------------------------------------------------------------

const (
	maxAttempts   = 3
	retryBaseWait = 400 * time.Millisecond
	retryMaxWait  = 2 * time.Second
)

// withRetry repeats a request that failed for a transient reason. Only requests
// whose response was never handed to a caller are retried, so a retry can only
// cost a second generation, never a duplicate picture in the transcript: the
// result is returned, not streamed.
func withRetry[T any](ctx context.Context, attempt func() (T, error)) (T, error) {
	var (
		lastErr error
		zero    T
	)
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, err
		}
		value, err := attempt()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable(err) || i == maxAttempts-1 {
			break
		}
		if !sleepBackoff(ctx, i) {
			break
		}
	}
	return zero, lastErr
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func sleepBackoff(ctx context.Context, attemptNo int) bool {
	wait := retryBaseWait << attemptNo
	if wait > retryMaxWait {
		wait = retryMaxWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *SDWebUI) get(ctx context.Context, path string, out any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, s.do(ctx, http.MethodGet, path, nil, out)
	})
	return err
}

func (s *SDWebUI) post(ctx context.Context, path string, body any, out any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		return struct{}{}, s.do(ctx, http.MethodPost, path, body, out)
	})
	return err
}

func (s *SDWebUI) do(ctx context.Context, method, path string, body any, out any) error {
	url := s.baseURL + path
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
	if s.basicAuth != "" {
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(s.basicAuth)))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		text := strings.TrimSpace(string(raw))
		httpErr := &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       s.redact(text),
			URL:        url,
		}
		if looksLikeMissingModel(resp.StatusCode, text) {
			return fmt.Errorf("%w: %s", errNoModelLoaded, httpErr.Error())
		}
		return httpErr
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// looksLikeMissingModel recognises the shapes a WebUI uses when no checkpoint is
// loaded. The routes exist, so the status is a 404 or a 500 either way; only the
// body distinguishes "you have not loaded a model" from "you are talking to
// something else that happens to be on this port".
func looksLikeMissingModel(status int, body string) bool {
	if status != http.StatusNotFound && status != http.StatusInternalServerError {
		return false
	}
	lower := strings.ToLower(body)
	for _, needle := range []string{
		"no model", "no checkpoint", "model not loaded", "unable to load",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// redact removes the credentials this client sent from a body that echoed them
// back. A proxied or misconfigured WebUI can repeat the request it refused, and
// the body travels to a toast.
func (s *SDWebUI) redact(body string) string {
	if s.basicAuth == "" || len(s.basicAuth) < 8 {
		return body
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(s.basicAuth))
	body = strings.ReplaceAll(body, encoded, "[redacted]")
	return strings.ReplaceAll(body, s.basicAuth, "[redacted]")
}

// ParseSeed is a small helper for a settings field that holds a seed as text,
// where an empty or malformed value means "let the server choose" rather than
// zero. It lives here because the alternative — parsing in the caller — is where
// an empty string silently becomes seed 0 and every picture comes out identical.
func ParseSeed(value string) *int64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "-1" {
		return nil
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}
