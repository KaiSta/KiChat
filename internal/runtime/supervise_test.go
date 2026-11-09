package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The supervisor was written for llama-server, and the picture server is the second thing
// that uses it. Two questions are answered by the caller rather than by llama.cpp's flag
// table: what the command line is, and which route says the program is ready. Getting either
// wrong is silent — a server started with llama.cpp's flags does not listen where the app
// then probes, and a probe on /health against a program that does not serve it never turns
// the row green.

// TestTheCallersCommandLineAndLabelWin is the pure half: the options decide, and an empty
// caller is still llama.cpp.
func TestTheCallersCommandLineAndLabelWin(t *testing.T) {
	// The image server's command line, which the CLI's own flag table already built: the
	// supervisor has to run it as given rather than wrap it in `-m`/`--host`/`--port`.
	args := []string{"--diffusion-model", "/models/q.gguf", "--listen-ip", "127.0.0.1", "--listen-port", "45999"}
	got := Options{Binary: "/sd/sd-server", Model: "/models/q.gguf", Args: args}.commandArgs()
	if len(got) != len(args) {
		t.Fatalf("commandArgs = %v, want the caller's own %v", got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("commandArgs = %v, want %v", got, args)
		}
	}
	if joined := strings.Join(got, " "); strings.Contains(joined, "--host") || strings.Contains(joined, "-c ") {
		t.Errorf("the llama.cpp flags were added to the caller's command line: %v", got)
	}

	// No caller and no label: llama.cpp's own list, and the name its messages use.
	built := Options{Model: "/m.gguf", Port: 1234}.commandArgs()
	if !hasFlag(built, "-m", "/m.gguf") || !hasFlag(built, "--port", "1234") {
		t.Errorf("the built command line = %v, want llama.cpp's own flags", built)
	}
	if name := (Options{}).label(); name != "llama-server" {
		t.Errorf("an unnamed program is %q, want llama-server", name)
	}
	if name := (Options{Label: "  sd-server "}).label(); name != "sd-server" {
		t.Errorf("label = %q, want the trimmed sd-server", name)
	}
}

// TestTheProjectorTravelsAsAFlag: `--mmproj` is the difference between a vision model
// answering an image and it answering 500 "image input is not supported" — measured on
// the user's own Qwen3-VL-8B, both ways. It has to reach the command line, it has to be
// able to be overridden by the user's own arguments, and a text-only model must not be
// handed the flag at all: llama-server reads an empty value as a path.
func TestTheProjectorTravelsAsAFlag(t *testing.T) {
	got := Options{Model: "/m.gguf", Port: 1234, MMProj: " /mm/mmproj-Model-F16.gguf "}.commandArgs()
	if !hasFlag(got, "--mmproj", "/mm/mmproj-Model-F16.gguf") {
		t.Errorf("arguments = %v, want the trimmed --mmproj path", got)
	}

	// The user's arguments come last, so a projector typed under "Further arguments"
	// is the one that wins over the one the app detected.
	both := Options{Model: "/m.gguf", Port: 1234, MMProj: "/auto.gguf",
		Extra: []string{"--mmproj", "/typed.gguf"}}.commandArgs()
	if seen := lastValueOf(both, "--mmproj"); seen != "/typed.gguf" {
		t.Errorf("the last --mmproj is %q, want the user's own /typed.gguf: %v", seen, both)
	}

	plain := Options{Model: "/m.gguf", Port: 1234}.commandArgs()
	if contains(plain, "--mmproj") {
		t.Errorf("a text-only model was given a projector flag: %v", plain)
	}
	blank := Options{Model: "/m.gguf", Port: 1234, MMProj: "   "}.commandArgs()
	if contains(blank, "--mmproj") {
		t.Errorf("a blank projector became a flag: %v", blank)
	}
}

// lastValueOf is the value of a flag's last occurrence, which is the one llama-server
// keeps when a flag is given twice.
func lastValueOf(args []string, flag string) string {
	value := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			value = args[i+1]
		}
	}
	return value
}

func hasFlag(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestAServerRunsTheCallersCommandLineAndProbesItsOwnRoute runs a real process: the argument
// list it was handed, the route readiness was asked on, and the name in the messages.
func TestAServerRunsTheCallersCommandLineAndProbesItsOwnRoute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, argsFile := fakeServer(t, "echo 'loading model'\nsleep 30\n")

	port, err := FreePort()
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	args := []string{"--diffusion-model", "/models/q.gguf", "--listen-ip", "127.0.0.1",
		"--listen-port", strconv.Itoa(port)}

	// The probe is answered here rather than by a real model load, exactly as the
	// llama.cpp tests do — but the URL it is *asked* is part of what is being checked, so
	// it is read out of the call.
	probed := make(chan string, 4)
	srv := NewServer("images")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(_ context.Context, url string) error {
		select {
		case probed <- url:
		default:
		}
		return nil
	}

	if err := srv.Start(Options{
		Binary:    binary,
		Model:     writeModel(t),
		Args:      args,
		Label:     "sd-server",
		ReadyPath: "/sdapi/v1/sd-models",
		Port:      port,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("the server never became ready: %+v", srv.Status())
	}
	if st := srv.Status(); st.URL != "http://127.0.0.1:"+strconv.Itoa(port) {
		t.Errorf("url = %q, want the address the caller's own --listen-port named", st.URL)
	}

	// The process really received that command line, and nothing of llama.cpp's.
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the process recorded no arguments: %v", err)
	}
	recorded := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(recorded) != len(args) {
		t.Fatalf("recorded %v, want exactly the caller's %v", recorded, args)
	}
	for i := range args {
		if recorded[i] != args[i] {
			t.Fatalf("recorded %v, want %v", recorded, args)
		}
	}

	// The route asked is the caller's, not llama.cpp's /health — the whole reason the
	// path is an option.
	select {
	case url := <-probed:
		if !strings.HasSuffix(url, "/sdapi/v1/sd-models") {
			t.Errorf("readiness was probed at %q, want the server's own route", url)
		}
	case <-time.After(time.Second):
		t.Fatal("no readiness probe was made")
	}

	// The messages name the program that is running: a row that said "llama-server wurde
	// beendet" about sd-server is wrong in the one place a user looks for a reason.
	log := strings.Join(srv.Status().Log, "\n")
	for _, want := range []string{"sd-server gestartet", "Aufruf:"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %q:\n%s", want, log)
		}
	}
}

// TestADeadServerIsReportedByTheCallersName: the failure messages are built from the label
// of the start that made the process, which is what a row for a program that has already
// exited has to say.
func TestADeadServerIsReportedByTheCallersName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "echo 'error: unsupported quantisation'\nexit 3\n")

	srv := NewServer("images")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return context.DeadlineExceeded }

	if err := srv.Start(Options{
		Binary: binary, Model: writeModel(t),
		Args: []string{"--diffusion-model", "/models/q.gguf"}, Label: "sd-server",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("state = %q, want failed: %+v", srv.Status().State, srv.Status())
	}
	st := srv.Status()
	if !strings.Contains(st.Error, "sd-server") || !strings.Contains(st.Error, "unsupported quantisation") {
		t.Errorf("error = %q, want the program's name and its own last output", st.Error)
	}
}

// An old readiness probe may finish after a restart; it must not mark the old
// server ready or let its exit overwrite the replacement's status.
func TestRestartIgnoresTheOldReadinessProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "sleep 30\n")
	model := writeModel(t)
	srv := NewServer("test")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstFinished := make(chan struct{})
	var once sync.Once
	srv.probe = func(context.Context, string) error {
		first := false
		once.Do(func() { first = true; close(entered) })
		if first {
			<-release
			defer close(firstFinished)
		}
		return nil
	}
	if err := srv.Start(Options{Binary: binary, Model: model}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first probe did not start")
	}
	if err := srv.Start(Options{Binary: binary, Model: model}); err != nil {
		close(release)
		t.Fatal(err)
	}
	newPID := srv.Status().PID
	close(release)
	select {
	case <-firstFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("first probe did not finish")
	}
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("replacement never became ready: %+v", srv.Status())
	}
	// Let the old monitor consume its already-reaped process after the probe returns.
	time.Sleep(100 * time.Millisecond)
	if st := srv.Status(); st.State != StateRunning || st.PID != newPID || st.URL == "" {
		t.Fatalf("old monitor replaced the new server's status: %+v, want pid %d", st, newPID)
	}
}

// A stopped process cannot become ready when a pending probe succeeds.
func TestStopIgnoresTheOldReadinessProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "sleep 30\n")
	srv := NewServer("test")
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	srv.probe = func(context.Context, string) error {
		close(entered)
		<-release
		close(done)
		return nil
	}
	if err := srv.Start(Options{Binary: binary, Model: writeModel(t)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		_ = srv.Stop()
		t.Fatal("probe did not start")
	}
	if err := srv.Stop(); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	<-done
	// The stopped process is already reaped; let the monitor consume that exit.
	time.Sleep(100 * time.Millisecond)
	if st := srv.Status(); st.State != StateStopped || st.PID != 0 || st.URL != "" {
		t.Fatalf("stopped server became ready: %+v", st)
	}
}

// A crash is explained by the line before the backtrace, not by the frames after it.
// llama.cpp forks gdb on SIGABRT, so the process's last lines are `#16 0x… in
// __libc_start_call_main` and gdb's notes about sources it could not find — and the
// assert that says what went wrong is above all of them. The output below is the tail of
// a real aborted load, which the panel used to report in full and with no reason in it.
func TestACrashIsReportedByItsReasonAndNotItsBacktrace(t *testing.T) {
	lines := []string{
		"llama-server gestartet (PID 8315) auf http://127.0.0.1:9931",
		"ggml_cuda_host_malloc: failed to allocate 4096.00 MiB on the host",
		"/src/ggml-cuda/ggml-cuda.cu:96: GGML_ASSERT(ptr != nullptr) failed",
		"#0  0x00007f2641c4f1ca in ggml_abort (file=…) at ggml.c:1234",
		"#1  0x00007f2641c4f2c5 in ggml_cuda_host_malloc (size=4294967296) at ggml-cuda.cu:96",
		"#16 0x00007f2641a2a1ca in __libc_start_call_main (main=…, argc=21, argv=…) at ../sysdeps/nptl/libc_start_call_main.h:58",
		"warning: 58\t../sysdeps/nptl/libc_start_call_main.h: No such file or directory",
		"warning: 360\t../csu/libc-start.c: No such file or directory",
		"#18 0x000055ad805d12a5 in _start ()",
		"[Inferior 1 (process 8315) detached]",
	}
	got := formatTail(failureTail(lines))
	if strings.Contains(got, "#0  0x") || strings.Contains(got, "[Inferior") {
		t.Errorf("the report still carries the backtrace: %q", got)
	}
	if !strings.Contains(got, "GGML_ASSERT(ptr != nullptr) failed") {
		t.Errorf("the report has no reason in it: %q", got)
	}

	// A log with nothing to strip is reported as before: the last six lines.
	plain := []string{"one", "two", "three", "four", "five", "six", "seven"}
	if tail := failureTail(plain); strings.Join(tail, ",") != "two,three,four,five,six,seven" {
		t.Errorf("plain tail = %v, want the last six lines", tail)
	}

	// A message that merely starts with a hash, or a warning in prose, is the program's
	// own and has to survive: only gdb's numbered frame and numbered-source shapes are
	// dropped.
	own := []string{"# configuration loaded", "warning: no usable GPU found, using the CPU"}
	if kept := failureTail(own); len(kept) != 2 {
		t.Errorf("the program's own lines were dropped: %v", kept)
	}
}

// The whole path, through a real process and the ring: twelve frames after the assert
// must not push it out of the report.
func TestAFailedServerQuotesTheAssertAndNotTheFrames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "echo 'ggml.c:1234: GGML_ASSERT(n_ctx > 0) failed'\n"+
		"i=0\nwhile [ $i -lt 12 ]; do echo \"#$i  0x00007f0000 frame\"; i=$((i+1)); done\n"+
		"echo '[Inferior 1 (process 8315) detached]'\nexit 134\n")

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return context.DeadlineExceeded }

	if err := srv.Start(Options{Binary: binary, Model: writeModel(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("state = %q, want failed: %+v", srv.Status().State, srv.Status())
	}
	st := srv.Status()
	if !strings.Contains(st.Error, "GGML_ASSERT(n_ctx > 0) failed") {
		t.Errorf("error = %q, want the assert in it", st.Error)
	}
	if strings.Contains(st.Error, "#11  0x") || strings.Contains(st.Error, "[Inferior") {
		t.Errorf("error = %q, want no backtrace in it", st.Error)
	}
}

// The row cannot hold a crash, the file can. Every line of the program's output — the
// frames the message drops included — is on disk, and the failed row names the file, so
// the thing a user has to attach to a report survives the row that produced it.
func TestAFailedServerNamesTheFileHoldingItsWholeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	binary, _ := fakeServer(t, "echo 'ggml.c:1234: GGML_ASSERT(n_ctx > 0) failed'\n"+
		"i=0\nwhile [ $i -lt 8 ]; do echo \"#$i  0x00007f0000 frame\"; i=$((i+1)); done\n"+
		"exit 134\n")
	path := filepath.Join(t.TempDir(), "llama-test.log")

	srv := NewServer("test")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return context.DeadlineExceeded }
	if err := srv.SetServerLog(path); err != nil {
		t.Fatalf("SetServerLog: %v", err)
	}
	if err := srv.Start(Options{Binary: binary, Model: writeModel(t)}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 10*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("state = %q, want failed: %+v", srv.Status().State, srv.Status())
	}
	st := srv.Status()
	if !strings.Contains(st.Error, path) {
		t.Errorf("error = %q, want it to name %s", st.Error, path)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the server log: %v", err)
	}
	for _, want := range []string{"GGML_ASSERT(n_ctx > 0) failed", "#7  0x00007f0000 frame", "Aufruf:"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the log has no %q:\n%s", want, written)
		}
	}
}

// A line the program was still writing when it died has no newline, so it is only in
// the writer's buffer — and on a crash that line is the assert itself.
func TestALastLineWithoutItsNewlineIsKept(t *testing.T) {
	r := newRing(10)
	w := r.writer()
	if _, err := w.Write([]byte("GGML_ASSERT(n_ctx > 0) failed")); err != nil {
		t.Fatal(err)
	}
	if got := r.tail(10); len(got) != 0 {
		t.Fatalf("the ring already holds %v, want nothing until the line ends", got)
	}
	w.flush()
	if got := r.tail(10); len(got) != 1 || got[0] != "GGML_ASSERT(n_ctx > 0) failed" {
		t.Errorf("after flush the ring holds %v, want the unfinished line", got)
	}
}

// A failed row names no address, and the case that made this necessary is a crash *after* the
// server was ready: the row turned red and kept the URL it had while it was running, so the
// panel offered a link to a listener that no longer existed. A URL on a failed row is one a
// user can click and another tool can be pointed at; both answer nothing.
func TestAFailedServerKeepsNoAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	// Long enough for readiness to be answered below, so the row really is running before it
	// dies — otherwise this would pass for a server that never had an address to lose.
	binary, _ := fakeServer(t, "echo 'Modell geladen'\nsleep 1\nexit 4\n")

	port, err := FreePort()
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	srv := NewServer("images")
	srv.readyTimeout = 10 * time.Second
	srv.probe = func(context.Context, string) error { return nil }

	if err := srv.Start(Options{
		Binary: binary, Model: writeModel(t),
		Args: []string{"--listen-port", strconv.Itoa(port)}, Label: "sd-server", Port: port,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return srv.Status().State == StateRunning }) {
		t.Fatalf("the server never became ready: %+v", srv.Status())
	}
	if st := srv.Status(); st.URL == "" {
		t.Fatal("the running row has no address, so there is nothing for the failure to clear")
	}

	if !waitFor(t, 10*time.Second, func() bool { return srv.Status().State == StateFailed }) {
		t.Fatalf("the crash was never reported: %+v", srv.Status())
	}
	st := srv.Status()
	if st.URL != "" || st.Port != 0 {
		t.Errorf("the failed row still names %q (port %d), and nothing is listening there", st.URL, st.Port)
	}
	if st.PID != 0 {
		t.Errorf("pid = %d on a failed row", st.PID)
	}
	// Stop, which is what a cleanup after a failed start does, must not bring either field
	// back — and must leave the failure's reason in place.
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if after := srv.Status(); after.URL != "" || after.Port != 0 || after.State != StateFailed {
		t.Errorf("after Stop: state = %q, url = %q, port = %d; want the failed row, minus its address",
			after.State, after.URL, after.Port)
	}
}
