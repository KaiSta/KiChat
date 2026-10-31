package images

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ilang "kichat/internal/lang"
)

// TestLiveSDServer starts a real `sd-server` out of a real stable-diffusion.cpp folder and
// measures the thing this whole kind exists for: the model is loaded *once*, and every
// picture after that costs only its sampling.
//
// It is skipped twice over, because both halves are deliberate. `KICHAT_LIVE=1` is what says
// that real work may happen, and `KICHAT_SD_FOLDER` is what names the folder — there is no
// default, because this test does not merely talk to a process, it *starts* one that will
// load gigabytes and hold them until the test kills it. A test that guessed a folder would do
// that on a machine whose owner never asked for it.
//
//	KICHAT_LIVE=1 KICHAT_SD_FOLDER=~/Downloads/sd go test ./internal/images/ -run TestLiveSDServer -v
//	  optional: KICHAT_SD_DIFFUSION_MODEL, KICHAT_SD_MODEL, KICHAT_SD_VAE, KICHAT_SD_LLM,
//	  KICHAT_SD_CLIP_L, KICHAT_SD_CLIP_G, KICHAT_SD_T5XXL — the model set the server is
//	  started with, which has to be filled in from the folder's own weights.
//
// What it settles that no offline test can:
//
//   - **the folder really holds a server.** Whether the published archives carry `sd-server`
//     beside `sd-cli` was the open question that made this kind a plan rather than a feature;
//     FindServerProgram answers it against the archive the user actually unpacked.
//   - **the build accepts the listener flag**, which is the check the app makes before it
//     starts anything — a build without it would listen on its documented 1234, the address
//     LM Studio answers on.
//   - **the readiness route is the right one.** The app probes `/sdapi/v1/sd-models`; that a
//     loaded server answers it is what the whole start state hangs on.
//   - **the load is paid once.** The numbers printed here are the argument for the kind: the
//     time to ready, then the wall time of the first and of the second picture. A server that
//     reloaded per draw would show a second picture the size of the first plus the load.
//
// The arithmetic is reported rather than asserted. Whether a draw is cheaper than a load
// depends on the model, the accelerator and the step count — on a fast GPU with twenty steps
// a *draw* can outlast a small model's load — so a threshold here would fail for a reason
// that has nothing to do with the client. What is asserted is what the client depends on: the
// program is there, it starts, it becomes ready on the route the app probes, and two pictures
// come back as real PNGs with a seed.
func TestLiveSDServer(t *testing.T) {
	if os.Getenv("KICHAT_LIVE") != "1" {
		t.Skip("set KICHAT_LIVE=1 to start a real sd-server")
	}
	folder := strings.TrimSpace(os.Getenv("KICHAT_SD_FOLDER"))
	if folder == "" {
		t.Skip("set KICHAT_SD_FOLDER to the folder a stable-diffusion.cpp archive unpacked into")
	}

	files := ModelFiles{
		Model:          os.Getenv("KICHAT_SD_MODEL"),
		DiffusionModel: os.Getenv("KICHAT_SD_DIFFUSION_MODEL"),
		VAE:            os.Getenv("KICHAT_SD_VAE"),
		LLM:            os.Getenv("KICHAT_SD_LLM"),
		ClipL:          os.Getenv("KICHAT_SD_CLIP_L"),
		ClipG:          os.Getenv("KICHAT_SD_CLIP_G"),
		T5XXL:          os.Getenv("KICHAT_SD_T5XXL"),
	}
	client := NewCLI("images", "sd-cli", "", files, Options{Folder: folder})

	// The folder question first, before any process: this is the file the app resolves and
	// the one a user cannot see in a listing.
	program, err := ResolveServerProgram(folder, "", ilang.German)
	if err != nil {
		t.Fatalf("the folder holds no %s: %v", ServerProgramName(runtime.GOOS), err)
	}
	t.Logf("server program: %s", program)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spec, err := client.ServerSpec(ctx, 0)
	if err != nil {
		t.Skipf("this configuration cannot start a server yet: %v", err)
	}
	if spec.Model == "" {
		t.Fatal("no model was named, so a start would load nothing")
	}

	// A port the OS says is free, asked for the way the app asks: a fixed one would collide
	// with whatever else is on the machine, including sd-server's own default and LM Studio.
	port, err := freeTestPort()
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	spec, err = client.ServerSpec(ctx, port)
	if err != nil {
		t.Fatalf("ServerSpec: %v", err)
	}
	t.Logf("command line: %s %s", spec.Program, strings.Join(spec.Args, " "))

	// The process is started here rather than through internal/runtime: this test is about
	// the command line and the routes, and a supervisor would only wrap them.
	proc := exec.Command(spec.Program, spec.Args...)
	proc.Dir = filepath.Dir(spec.Program)
	proc.Stdout, proc.Stderr = os.Stdout, os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	// The model's memory is released by killing the process and nowhere else, so this has to
	// happen even when an assertion below fails.
	defer func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	web := NewSDWebUI("images", "sd-server", baseURL, "")
	loadTime, err := waitForLiveServer(ctx, web)
	if err != nil {
		t.Fatalf("the server never answered %s: %v", ServerReadyPath, err)
	}
	t.Logf("load: %s until %s answered", loadTime.Round(time.Millisecond), ServerReadyPath)

	// Two pictures of the smallest size that still round-trips: what is being measured is
	// the lifetime, not the art.
	draws := make([]time.Duration, 0, 2)
	var seeds []int64
	for i := range 2 {
		start := time.Now()
		// The guidance scale travels with the draw because this is the one build whose
		// tolerance for it is unverified: `cfg_scale` is a key the A1111 route documents
		// and this server parses (`j.value("cfg_scale", …txt_cfg)`), but this app sends a
		// *smaller* payload than a WebUI does and a build that rejected an unexpected key
		// would fail every draw rather than this one. The echoed value below is what tells
		// "parsed" apart from "ignored".
		result, err := web.Generate(ctx, Request{
			Prompt: "a plain grey square", Width: 64, Height: 64, Steps: 1, CfgScale: liveCfgScale,
		})
		drawn := time.Since(start)
		if err != nil {
			t.Fatalf("draw %d: %v", i+1, err)
		}
		if len(result.Images) == 0 {
			t.Fatalf("draw %d returned no picture", i+1)
		}
		data := result.Images[0].Data
		if len(data) < 8 || string(data[1:4]) != "PNG" {
			t.Fatalf("draw %d is not a PNG (first bytes: % x)", i+1, data[:min(16, len(data))])
		}
		if result.Seed == 0 {
			t.Errorf("draw %d reported no seed, so the picture cannot be asked for again", i+1)
		}
		if !strings.Contains(result.Info, fmt.Sprintf("%v", liveCfgScale)) {
			t.Errorf("draw %d: the guidance scale %v is not echoed in info, so this build either "+
				"ignored or refused the key: %s", i+1, liveCfgScale, truncate(result.Info, 400))
		}
		draws = append(draws, drawn)
		seeds = append(seeds, result.Seed)
		t.Logf("draw %d: %s, %d bytes, seed %d, model %q", i+1, drawn.Round(time.Millisecond), len(data), result.Seed, result.Model)
	}
	t.Logf("load %s → draw 1 %s, draw 2 %s — the second picture paid no load again",
		loadTime.Round(time.Millisecond), draws[0].Round(time.Millisecond), draws[1].Round(time.Millisecond))
	t.Logf("seeds: %v", seeds)
}

// waitForLiveServer polls the route the app probes until the server answers it, and reports
// how long that took — which is the load, and the first half of the measurement.
func waitForLiveServer(ctx context.Context, web *SDWebUI) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(DefaultServerReadyTimeout)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := web.Health(ctx); err == nil {
			return time.Since(start), nil
		} else {
			last = err
		}
		// Slow on purpose: every poll is a request against a process that is loading
		// gigabytes, and the app itself asks once every 300 ms.
		time.Sleep(time.Second)
	}
	return 0, fmt.Errorf("still not ready after %s: %w", DefaultServerReadyTimeout, last)
}

// freeTestPort asks the OS for a port that is free right now. The app has its own helper for
// this in internal/runtime; this package does not import it, because a client package that
// depends on the supervisor would make the supervisor's tests depend on a client in turn.
func freeTestPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}
