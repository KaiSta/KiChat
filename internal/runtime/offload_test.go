package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The app's word for a fully CPU-side model is `off`, and llama.cpp's spelling of
// that is a count of zero. Anything else is not a mode this app knows, and it must
// become the server's own default rather than a flag the server rejects.
func TestGpuLayersSpellsTheModesTheWayLlamaCppDoes(t *testing.T) {
	cases := []struct{ offload, want string }{
		{OffloadAll, "all"},
		{OffloadOff, "0"},
		{"  " + OffloadOff + " ", "0"},
		// "auto" is the server's own default — as many layers as fit — and asking
		// for it in words is a flag llama.cpp may not read the same way.
		{"", ""},
		{"auto", ""},
		{"gpu", ""},
		{"24", ""},
	}
	for _, c := range cases {
		if got := gpuLayers(c.offload); got != c.want {
			t.Errorf("gpuLayers(%q) = %q, want %q", c.offload, got, c.want)
		}
	}
}

// The flags themselves, on the builder that makes the command line. The user's own
// arguments stay last, which is what lets `--n-gpu-layers 24` in *Further
// arguments* win over the mode the picker set.
func TestOffloadAndTraceReachTheCommandLine(t *testing.T) {
	argsOf := func(o Options) []string { return o.args() }
	// The flag has to carry the value, in that order and next to each other.
	has := func(args []string, flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}

	// A model with no offload setting gets the server's own default: no flag at
	// all, because "auto" is what llama.cpp already does.
	if auto := argsOf(Options{Model: "/m.gguf"}); strings.Contains(strings.Join(auto, " "), "gpu-layers") {
		t.Errorf("an auto offload sent a flag: %v", auto)
	}
	if all := argsOf(Options{Model: "/m.gguf", Offload: OffloadAll}); !has(all, "--gpu-layers", "all") {
		t.Errorf("offload all = %v, want --gpu-layers all", all)
	}
	if off := argsOf(Options{Model: "/m.gguf", Offload: OffloadOff}); !has(off, "--gpu-layers", "0") {
		t.Errorf("offload off = %v, want --gpu-layers 0", off)
	}
	if tr := argsOf(Options{Model: "/m.gguf", Trace: true}); !has(tr, "-lv", "4") {
		t.Errorf("a traced start = %v, want -lv 4", tr)
	}
	// The expert flags: nothing for "auto", llama.cpp's own two spellings for the
	// explicit modes, and the user's count still last.
	contains := func(args []string, want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	if expertAuto := argsOf(Options{Model: "/m.gguf"}); contains(expertAuto, "--cpu-moe") || contains(expertAuto, "--n-cpu-moe") {
		t.Errorf("an auto expert offload sent a flag: %v", expertAuto)
	}
	if allExperts := argsOf(Options{Model: "/m.gguf", ExpertOffload: ExpertOffloadCPU}); !contains(allExperts, "--cpu-moe") {
		t.Errorf("expert offload cpu = %v, want --cpu-moe", allExperts)
	}
	someExperts := argsOf(Options{Model: "/m.gguf", ExpertOffload: ExpertOffloadLayers, ExpertLayers: 13})
	if !has(someExperts, "--n-cpu-moe", "13") {
		t.Errorf("expert offload layers = %v, want --n-cpu-moe 13", someExperts)
	}
	// A count of zero is llama.cpp's own "none of them", not a reason to drop the flag.
	if zero := argsOf(Options{Model: "/m.gguf", ExpertOffload: ExpertOffloadLayers}); !has(zero, "--n-cpu-moe", "0") {
		t.Errorf("expert offload layers 0 = %v, want --n-cpu-moe 0", zero)
	}
	// Both new flags come before the user's own, so a verbosity or a layer count
	// typed into *Further arguments* is the one that counts.
	both := argsOf(Options{Model: "/m.gguf", Offload: OffloadAll, Trace: true, Extra: []string{"-ngl", "24"}})
	if len(both) < 2 || both[len(both)-2] != "-ngl" || both[len(both)-1] != "24" {
		t.Errorf("the user's arguments are not last: %v", both)
	}
	if !has(both, "--gpu-layers", "all") || !has(both, "-lv", "4") {
		t.Errorf("missing one of the app's own flags: %v", both)
	}
}

// The measured lines, on both release sources. The device is named before the layer
// count, so the reading is carried forward rather than rebuilt from one line.
func TestThePlacementIsReadFromTheServersOwnLines(t *testing.T) {
	device := "0.00.479.028 I llama_prepare_model_devices: using device Vulkan0 (AMD Radeon RX 7900 XT (RADV NAVI31)) (0000:0c:00.0) - 18714 MiB free"

	var off Offload
	if off.Reported() {
		t.Error("an empty reading claims to be reported")
	}
	off = parseOffloadLine("0.00.044.416 I cmn common_param: verbosity = 3", off)
	if off.Reported() || off.Device != "" {
		t.Errorf("an unrelated line changed the reading: %+v", off)
	}
	off = parseOffloadLine(device, off)
	if off.Device != "Vulkan0" {
		t.Errorf("device = %q, want Vulkan0", off.Device)
	}
	if off.Reported() {
		t.Error("a device alone is not a placement: nothing has been said about layers yet")
	}
	off = parseOffloadLine("0.00.878.017 I load_tensors: offloaded 34/34 layers to GPU", off)
	if off.Layers != 34 || off.Total != 34 || off.Device != "Vulkan0" {
		t.Errorf("reading = %+v, want 34/34 on Vulkan0", off)
	}
	if !off.Reported() || !off.OnDevice() {
		t.Error("34/34 layers is a reported placement on a device")
	}

	// The CPU case is an answer, not a missing one: a `--gpu-layers 0` run still
	// names the device it would have used, which is what makes "off on a machine
	// that has a card" readable.
	var none Offload
	none = parseOffloadLine(device, none)
	none = parseOffloadLine("0.00.891.870 I load_tensors: offloaded 0/34 layers to GPU", none)
	if !none.Reported() || none.OnDevice() {
		t.Errorf("reading = %+v, want a reported placement with no layer on the device", none)
	}
	if none.Layers != 0 || none.Total != 34 || none.Device != "Vulkan0" {
		t.Errorf("reading = %+v, want 0/34 on Vulkan0", none)
	}

	// A partial placement: what a model too large for the card produces.
	var partial Offload
	partial = parseOffloadLine("load_tensors: offloading 24 repeating layers to GPU", partial)
	partial = parseOffloadLine("load_tensors: offloaded 26/42 layers to GPU", partial)
	if partial.Layers != 26 || partial.Total != 42 || partial.OnDevice() {
		t.Errorf("reading = %+v, want 26/42 and no device claimed", partial)
	}

	// The shape that makes the device line load-bearing: the fork's CPU-only build,
	// which lists nothing but the host CPU and still says every layer was offloaded.
	// Measured on prism-b10770-6684606's ubuntu-x64 archive at `-lv 4`; nothing in
	// its log says `using device`. Reading that as "34/34 on the GPU" would tell a
	// user their model is accelerated while it runs on the CPU — the silent
	// fallback this reading exists to expose.
	var cpuOnly Offload
	cpuOnly = parseOffloadLine("0.00.860.765 I cmn  common_param:   - CPU     : AMD Ryzen 7 5800X3D 8-Core Processor (31996 MiB, 31996 MiB free)", cpuOnly)
	cpuOnly = parseOffloadLine("0.00.860.768 I load_tensors: offloaded 34/34 layers to GPU", cpuOnly)
	if !cpuOnly.Reported() {
		t.Error("a CPU-only build reports a placement: 34/34 is what its log says")
	}
	if cpuOnly.Device != "" || cpuOnly.OnDevice() {
		t.Errorf("reading = %+v, want no device claimed for a build that named none", cpuOnly)
	}
}

// The reading has to survive the window being small, because it is printed early.
//
// Measured on a 9B model at trace level: `using device …` and `offloaded 34/34
// layers to GPU` sit about 80 lines before the end of a 250-line log, while the ring
// the panel shows holds 40 lines and the tail it returns is 12. Reading the tail
// afterwards would therefore report nothing for a model whose load went well, so the
// lines are consumed as they arrive.
func TestThePlacementIsKeptEvenWhenTheLogTailHasMovedPastIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	script := strings.Join([]string{
		"echo '0.00.479.028 I llama_prepare_model_devices: using device Vulkan0 (AMD Radeon RX 7900 XT (RADV NAVI31)) (0000:0c:00.0) - 18714 MiB free'",
		"echo '0.00.878.017 I load_tensors: offloaded 34/34 layers to GPU'",
		"i=0; while [ $i -lt 60 ]; do echo \"0.00.9$i.000 I load_tensors: graph $i built\"; i=$((i+1)); done",
		"sleep 30",
	}, "\n")
	binary, _ := fakeServer(t, script)

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return nil }
	if err := srv.Start(Options{Binary: binary, Model: writeModel(t), Offload: OffloadAll, Trace: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("server never became ready: %+v", srv.Status())
	}

	st := srv.Status()
	if !st.Offload.Reported() || st.Offload.Layers != 34 || st.Offload.Total != 34 || st.Offload.Device != "Vulkan0" {
		t.Errorf("offload = %+v, want 34/34 on Vulkan0", st.Offload)
	}
	// The same lines are gone from the tail, which is the point: the reading cannot
	// have come from there.
	for _, line := range st.Log {
		if strings.Contains(line, "offloaded 34/34") {
			t.Fatalf("the placement is still in the log tail, so this test proves nothing: %v", st.Log)
		}
	}
	if len(st.Log) == 0 {
		t.Fatal("no log tail at all, which means the ring is not being read")
	}

	// Stopped, the reading goes with the process it described: a row that says the
	// model is on a device it is no longer loaded on is worse than no reading.
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := srv.Status(); st.Offload.Reported() || st.Offload.Device != "" {
		t.Errorf("a stopped server still reports its placement: %+v", st.Offload)
	}
}

// A server that dies takes its reading with it, whether it never became ready or
// answered requests for hours first. A failed row that still says which card the
// layers went to is a claim about a model that is not running.
func TestAFailedServerReportsNoPlacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a shell script")
	}
	// Prints a placement, then dies on its own — which is how a server whose model
	// was loaded and then crashed looks from the supervisor's side.
	binary, _ := fakeServer(t, strings.Join([]string{
		"echo 'llama_prepare_model_devices: using device Vulkan0 (A Card) - 18714 MiB free'",
		"echo 'load_tensors: offloaded 34/34 layers to GPU'",
		"sleep 2",
		"exit 4",
	}, "\n"))

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return nil }
	if err := srv.Start(Options{Binary: binary, Model: writeModel(t), Trace: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("server never became ready: %+v", srv.Status())
	}
	// While it is up the reading is there — otherwise the assertion below would
	// pass for the wrong reason.
	if st := srv.Status(); !st.Offload.Reported() {
		t.Fatalf("no reading while running: %+v", st)
	}
	if !waitFor(t, 10*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("the server did not report its death: %+v", srv.Status())
	}
	if st := srv.Status(); st.Offload.Reported() || st.Offload.Device != "" {
		t.Errorf("a dead server still reports a placement: %+v", st.Offload)
	}
}

// A start that never happened must not leave a row saying it is loading.
//
// The status is marked "starting" before the process exists, which is what keeps the
// first line of its output from being discarded; if exec then fails, that has to be
// undone rather than left behind, because the panel polls a "starting" row until it
// turns into something else and nothing ever would.
func TestAFailedExecLeavesNoLoadingRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the file-permission case is a Unix one")
	}
	// A launcher that exists and is not executable: the checks in Start pass, and
	// exec is the thing that refuses.
	dir := t.TempDir()
	binary := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := NewServer("test")
	srv.probe = func(context.Context, string) error { return nil }
	if err := srv.Start(Options{Binary: binary, Model: writeModel(t)}); err == nil {
		t.Fatal("want an error for a launcher that cannot be executed")
	}
	if st := srv.Status(); st.State != StateStopped || st.PID != 0 {
		t.Errorf("after a refused start the status is %+v, want a stopped row with no pid", st)
	}
}

// A reading written by a process that has already stopped must not come back.
//
// Output can arrive after exec has reaped the process — the pipe is closed late — and
// a placement written into a stopped or failed row is a claim about a model that is not
// loading. The supervisor therefore only takes lines from a server that is starting or
// running.
func TestLateOutputFromAStoppedServerIsIgnored(t *testing.T) {
	srv := NewServer("test")
	srv.log.add("load_tensors: offloaded 34/34 layers to GPU")
	if st := srv.Status(); st.Offload.Reported() {
		t.Errorf("a placement was recorded on a stopped server: %+v", st.Offload)
	}

	srv.mu.Lock()
	srv.status.State, srv.status.Offload = StateRunning, Offload{}
	srv.mu.Unlock()
	srv.log.add("load_tensors: offloaded 34/34 layers to GPU")
	if st := srv.Status(); !st.Offload.Reported() {
		t.Errorf("a running server's own line was ignored: %+v", st.Offload)
	}

	srv.markStopped()
	srv.log.add("load_tensors: offloaded 34/34 layers to GPU")
	if st := srv.Status(); st.Offload.Reported() {
		t.Errorf("a line that arrived after the stop was taken: %+v", st.Offload)
	}
}

// A log the app itself writes must not look like a placement: the ring carries the
// app's own lines next to the server's, and a message that happens to contain the
// words is not a device.
func TestOnlyTheServersOwnWordingCounts(t *testing.T) {
	lines := []string{
		"llama-server gestartet (PID 42) auf http://127.0.0.1:9931",
		"Aufruf: /x/llama-server -m /m.gguf --gpu-layers all",
		"0.00.973.000 W resolve_fused_ops: layer 0 is assigned to device CPU but fused Gated Delta Net (chunked) is assigned to device Vulkan0 (usually due to missing support)",
		fmt.Sprintf("Zeitüberschreitung: llama-server war nach %s nicht bereit", 180*time.Second),
	}
	var off Offload
	for _, line := range lines {
		off = parseOffloadLine(line, off)
	}
	if off.Reported() || off.Device != "" {
		t.Errorf("reading = %+v, want nothing from lines that are not a placement", off)
	}
}
