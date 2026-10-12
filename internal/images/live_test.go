package images

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"kichat/internal/config"
)

// TestLiveSDWebUI exercises the client against a real WebUI on this machine. It is
// skipped unless KICHAT_LIVE=1, so the normal suite stays hermetic and never
// contacts (or, worse, starts drawing on) a server the user happens to be running
// for their own work.
//
// Unlike the metered backends there is no credential to pass: the whole point of
// this provider is that it is local and unauthenticated. The address is therefore
// the one input, and it defaults to AUTOMATIC1111's own port.
//
//	KICHAT_LIVE=1 go test ./internal/images/ -run TestLiveSDWebUI -v
//	KICHAT_LIVE=1 KICHAT_SD_URL=http://127.0.0.1:7861 go test ./internal/images/ -run TestLiveSDWebUI -v
//	KICHAT_LIVE=1 KICHAT_SD_AUTH=user:pass go test ./internal/images/ -run TestLiveSDWebUI -v
//
// Three claims are checked, each of which the offline tests can only fake:
//
//   - the server answers `/sdapi/v1/sd-models` and names its checkpoints. A WebUI
//     with nothing installed answers it too, which is why the test reports that
//     state rather than failing on it — "reachable but empty" is a real answer.
//   - `sampler_name` is echoed back in `info`, which is what proves the sampler
//     this client sends is one the server recognises rather than silently ignored.
//   - a picture actually decodes to PNG bytes with the right magic. A server that
//     answered with a data: URL, or with a URL to a file, would produce bytes that
//     are not a picture, and only a real round trip can tell the two apart.
//
// This test spends real GPU seconds. The picture is deliberately small — 64x64 at
// one step — because what is being verified is the wire shape, not the art.
func TestLiveSDWebUI(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to run against a live image server")
	}
	baseURL := os.Getenv("KICHAT_SD_URL")
	if baseURL == "" {
		baseURL = config.DefaultImageBaseURL
	}
	client := NewSDWebUI("images", "Stable Diffusion WebUI", baseURL, os.Getenv("KICHAT_SD_AUTH"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A closed port is the expected state on a machine that never set this up, so
	// it is reported as a skip rather than a failure: the test's job is to describe
	// a running server, not to insist that one is running.
	if err := client.Health(ctx); err != nil {
		t.Skipf("no image server at %s (this is the normal state): %v", baseURL, err)
	}

	models, err := client.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	t.Logf("checkpoints: %d", len(models))
	for _, m := range models {
		t.Logf("  %s", m.ID)
	}
	if len(models) == 0 {
		t.Skip("the server is reachable but has no checkpoint installed, so it cannot draw")
	}

	samplers, err := client.Samplers(ctx)
	if err != nil {
		t.Fatalf("Samplers: %v", err)
	}
	t.Logf("samplers: %d (first: %v)", len(samplers), firstN(samplers, 5))

	// The smallest thing that still round-trips: one step, 64x64.
	sampler := ""
	if len(samplers) > 0 {
		sampler = samplers[0]
	}
	result, err := client.Generate(ctx, Request{
		Prompt:  "a plain grey square",
		Width:   64,
		Height:  64,
		Steps:   1,
		Sampler: sampler,
	})
	if err != nil {
		if errors.Is(err, ErrNoModel) {
			t.Skipf("the server has no model loaded: %v", err)
		}
		t.Fatalf("Generate: %v", err)
	}
	if len(result.Images) == 0 {
		t.Fatal("a generation returned no picture")
	}
	data := result.Images[0].Data
	t.Logf("picture: %d bytes, mime %s, seed %d, model %q, info %s",
		len(data), result.Images[0].Mime, result.Seed, result.Model, truncate(result.Info, 200))

	if len(data) < 8 || string(data[1:4]) != "PNG" {
		t.Fatalf("the payload is not a PNG (first bytes: % x)", data[:min(16, len(data))])
	}
	// A seed the server chose and did not report cannot be reproduced, which is the
	// one number a user needs to ask for the same picture again.
	if result.Seed == 0 {
		t.Error("the server reported no seed; the picture cannot be reproduced")
	}
	if sampler != "" && result.Info != "" && !strings.Contains(result.Info, sampler) {
		t.Errorf("the sampler %q is not echoed in info, so the server may have ignored it: %s",
			sampler, truncate(result.Info, 400))
	}
}

func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n] + "…"
}
