package runtime

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
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
