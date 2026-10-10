package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Process states, as reported to the UI.
const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateFailed   = "failed"
)

// The two flags that describe a start in terms of memory rather than size, plus
// the sentinel that means "leave the server's own default alone". They are
// spelled here as well as in the settings package so neither has to import the
// other; the values are llama.cpp's own and are pinned by a test.
const (
	// FlashAttentionOn and FlashAttentionOff are the explicit modes; "auto" and
	// an empty value both mean the server decides and are not passed.
	FlashAttentionOn  = "on"
	FlashAttentionOff = "off"
	// CacheTypeF16 is llama.cpp's default KV cache type. Passing it is a no-op, so
	// it is omitted rather than sent.
	CacheTypeF16 = "f16"
)

// Options describe one server to start.
type Options struct {
	// Binary is the llama-server launcher, absolute.
	Binary string
	// Model is the GGUF file to load.
	Model string
	// Ctx is the context window to load it with; 0 lets the server choose.
	Ctx int
	// FlashAttention is `--flash-attn`: "on", "off", or empty/"auto" for the
	// server's own default, which is not passed as a flag.
	FlashAttention string
	// CacheTypeK and CacheTypeV are `--cache-type-k`/`-v`. Empty and "f16" are
	// the server's own default and are not passed.
	CacheTypeK string
	CacheTypeV string
	// Offload is how much of the model goes on the GPU: OffloadAll or OffloadOff
	// for the two explicit modes, empty for the server's own default — as many
	// layers as fit in the device memory. See gpuLayers.
	Offload string
	// Trace asks for the server's log at llama.cpp's trace level, which is where it
	// names the device it chose and how many layers it placed there; the reading
	// this app reports comes from those two lines and nowhere else. See
	// TraceVerbosity. It is placed before Extra, so a verbosity the user typed
	// themselves still wins.
	Trace bool
	// Extra is the rest of the user's command line, already split.
	Extra []string
	// Host defaults to 127.0.0.1. A server is only ever started on the loopback
	// interface: it is a local model, and binding it to every interface would
	// expose an unauthenticated endpoint to the network.
	Host string
	// Port 0 picks a free one.
	Port int
}

// Status is a snapshot for the UI.
type Status struct {
	State string   `json:"state"`
	Model string   `json:"model"`
	Port  int      `json:"port"`
	PID   int      `json:"pid"`
	URL   string   `json:"url"`
	Error string   `json:"error,omitempty"`
	Log   []string `json:"log,omitempty"`
	// Offload is what the server has said about the model's layers so far. It is
	// the only answer to "is this model actually running on the GPU?", and it is
	// empty until the server says something — a launcher started at the default
	// log level says nothing at all.
	Offload Offload `json:"offload"`
}

// Server supervises one llama-server process.
type Server struct {
	name string

	mu     sync.Mutex
	cmd    *exec.Cmd
	waited chan struct{}
	// binary is the launcher the current — or last — process was started from. A
	// running server keeps the build it started with, so this is what tells the
	// cleanup list which build must not be deleted while it answers.
	binary string
	// stopRequested separates "the user stopped it" from "it died", which is the
	// difference between a clean stop and a failure worth reporting.
	stopRequested bool
	status        Status
	log           *ring

	// probe reports whether a server at url is answering yet. It is a field so a
	// test can answer without a real server loading a real model.
	probe func(ctx context.Context, url string) error
	// readyTimeout bounds a start. Loading a large model off disk can take
	// minutes on a slow drive, so this is generous by default.
	readyTimeout time.Duration
	// grace is how long a shutdown may take before the process is killed.
	grace time.Duration
	// onState is called on every state change; the app turns it into an event.
	onState func(Status)
}

// NewServer builds a supervisor for one backend entry. The name is only used in
// messages, so a user with two managed servers can tell them apart.
func NewServer(name string) *Server {
	s := &Server{
		name:         name,
		log:          newRing(40),
		probe:        probeHealth,
		readyTimeout: 180 * time.Second,
		grace:        5 * time.Second,
		status:       Status{State: StateStopped},
	}
	// Installed here rather than per start, so the first line of the very first
	// model load is already seen. See Server.observeOffload.
	s.log.watch = s.observeOffload
	return s
}

// SetReadyTimeout overrides how long a start may take. Used by settings.
func (s *Server) SetReadyTimeout(d time.Duration) { s.readyTimeout = d }

// SetStateHandler registers the callback that reports state changes.
func (s *Server) SetStateHandler(fn func(Status)) { s.onState = fn }

// probeHealth reports whether a llama-server at url is ready to answer.
//
// /health answers 503 with "Loading model" while the weights are still coming in
// and 200 once it can serve, which is exactly the distinction needed: the process
// being alive says nothing about whether a chat would work.
func probeHealth(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Start launches the server and returns as soon as the process exists. Readiness
// is reported through Status: the caller gets a "starting" server immediately
// rather than a bound method that blocks for minutes.
func (s *Server) Start(opts Options) error {
	if opts.Binary == "" {
		return errors.New("kein llama-server installiert")
	}
	if opts.Model == "" {
		return errors.New("kein Modell ausgewählt")
	}
	// Checked here rather than left to exec, so the user is told which of the two
	// paths is wrong while the server is not yet involved.
	if _, err := os.Stat(opts.Binary); err != nil {
		return fmt.Errorf("llama-server nicht gefunden: %w", err)
	}
	model, err := os.Stat(opts.Model)
	if err != nil {
		return fmt.Errorf("Modell nicht gefunden: %w", err)
	}
	if model.IsDir() {
		return fmt.Errorf("Modellpfad ist ein Ordner: %s", opts.Model)
	}

	if err := s.Stop(); err != nil {
		return err
	}

	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.Port
	if port == 0 {
		port, err = freePort()
		if err != nil {
			return fmt.Errorf("freien Port finden: %w", err)
		}
	}
	opts.Port = port

	args := opts.args()
	cmd := exec.Command(opts.Binary, args...)
	// The build is started from its own directory and with the library path that
	// directory needs; see serverEnv, which is where the CUDA runtime bundle and
	// the ROCm libraries are made findable. The published archives carry rpath
	// `$ORIGIN`, so the build's own libraries are found wherever the process was
	// started from — the working directory is set for the relative paths in a
	// user's own arguments, not for the loader.
	cmd.Dir = dirOf(opts.Binary)
	cmd.Env = serverEnv(opts.Binary)
	cmd.Stdout = s.log.writer()
	cmd.Stderr = s.log.writer()
	startInOwnGroup(cmd)

	// A fresh Status rather than an update: the placement reading belongs to the
	// process that answered it, and a restart must not keep the previous build's
	// numbers, or the previous model's. It is cleared *before* the process exists,
	// because the watcher that fills the reading runs on the very first line the
	// launcher writes — a fast one can have its placement already in the status by
	// the time exec returns, and resetting afterwards would throw it away.
	s.mu.Lock()
	s.status = Status{State: StateStarting, Model: opts.Model, Port: port}
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		// The row was marked "starting" before the process existed, so the first line
		// of output could not be lost. A start that never happened has to undo that:
		// a status saying "starting" would have the panel poll for a model load that
		// no process is performing, forever.
		s.markStopped()
		return fmt.Errorf("llama-server starten: %w", err)
	}

	url := fmt.Sprintf("http://%s:%d", host, port)
	s.mu.Lock()
	s.cmd = cmd
	s.binary = opts.Binary
	s.waited = make(chan struct{})
	s.stopRequested = false
	s.status.PID, s.status.URL = cmd.Process.Pid, url
	waited := s.waited
	s.mu.Unlock()
	s.publish()

	s.log.addf("llama-server gestartet (PID %d) auf %s", cmd.Process.Pid, url)
	s.log.addf("Aufruf: %s %s", opts.Binary, strings.Join(args, " "))
	go s.monitor(cmd, url, waited)
	return nil
}

// monitor waits for the process to become ready, then for it to exit.
func (s *Server) monitor(cmd *exec.Cmd, url string, waited chan struct{}) {
	exit := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		close(waited)
		exit <- err
	}()

	// A start is not cancelled by whoever asked for it: the request that pressed
	// Start has long returned by the time a model finishes loading, so the probe
	// runs on its own context with a per-attempt timeout.
	deadline := time.Now().Add(s.readyTimeout)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exit:
			s.finished(err)
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				s.log.addf("Zeitüberschreitung: llama-server war nach %s nicht bereit", s.readyTimeout)
				s.fail(fmt.Sprintf("llama-server war nach %s nicht bereit", s.readyTimeout))
				_ = s.Stop()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := s.probe(ctx, url+"/health")
			cancel()
			if err == nil {
				s.setState(StateRunning)
				s.log.addf("Modell geladen, Server bereit")
				// Keep watching: a server that dies later has to be reported
				// rather than leaving the UI showing a green row.
				s.finished(<-exit)
				return
			}
		}
	}
}

// finished records the exit of a process that is no longer running.
func (s *Server) finished(err error) {
	s.mu.Lock()
	requested := s.stopRequested
	s.cmd = nil
	state := s.status.State
	s.mu.Unlock()

	switch {
	case requested, state == StateFailed:
		// A stop the user asked for is not a failure, and a failure has already
		// been reported with a better message than the exit code gives. The
		// identity is cleared here as well as in Stop, because this runs first
		// when the process notices the signal before the caller does — and a
		// stopped server still showing a PID is a stopped server that looks
		// running.
		if state != StateFailed {
			s.markStopped()
		}
	case err != nil:
		tail := s.log.tail(6)
		s.log.addf("llama-server beendet: %v", err)
		s.fail(fmt.Sprintf("llama-server wurde beendet: %v%s", err, formatTail(tail)))
	default:
		s.log.addf("llama-server beendet")
		s.fail("llama-server hat sich beendet")
	}
}

// Stop shuts the server down and waits for it to be gone. It is safe to call when
// nothing is running.
func (s *Server) Stop() error {
	s.mu.Lock()
	cmd, waited := s.cmd, s.waited
	if cmd == nil || cmd.Process == nil {
		s.mu.Unlock()
		return nil
	}
	s.stopRequested = true
	s.mu.Unlock()

	// SIGTERM first: llama-server frees its context and exits cleanly, which is
	// what keeps a large model's memory from being reclaimed by the OS only
	// after the process is killed.
	if err := terminate(cmd, false); err != nil {
		s.log.addf("SIGTERM konnte nicht zugestellt werden: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(s.grace):
		s.log.addf("keine Reaktion auf SIGTERM, Prozess wird beendet")
		if err := terminate(cmd, true); err != nil {
			s.log.addf("erzwungenes Beenden fehlgeschlagen: %v", err)
		}
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			// The signal was delivered, so the process is gone; what is still
			// pending is exec's copy of its output, which happens when a child
			// the server forked inherited the pipe and outlived it. Blocking
			// the caller — and the window — on that would be a bug of its own,
			// and the group kill above is what normally prevents it.
			s.log.addf("Ausgabekanäle von PID %d sind noch offen; der Server gilt als beendet", cmd.Process.Pid)
		}
	}

	s.mu.Lock()
	s.cmd = nil
	s.waited = nil
	s.mu.Unlock()
	s.markStopped()
	return nil
}

// markStopped records that nothing is running any more.
func (s *Server) markStopped() {
	s.mu.Lock()
	s.status.State = StateStopped
	s.status.PID = 0
	s.status.Port = 0
	s.status.URL = ""
	// The reading described a process that is gone. Leaving it on the row would
	// report a model as placed on a device it is no longer loaded on.
	s.status.Offload = Offload{}
	s.mu.Unlock()
	s.publish()
}

// observeOffload reads one line of the server's log for where the model's layers
// went, and keeps the answer on the status.
//
// Captured as the lines arrive instead of read out of the ring afterwards: the
// placement is printed while the weights are loaded, and everything a model says
// after that — the context, the KV cache, the graph — pushes it out of the
// window. Measured on a 9B model at trace level, the two lines sit about 80 lines
// before the end of a 250-line log, while the ring the panel shows holds 40.
// Nothing is published from here: this runs for every line a server writes, and
// the panel is polling a status while a model loads anyway.
func (s *Server) observeOffload(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Only the process that is starting or running can say where its layers went.
	// Output from a process that has already stopped arrives late on occasion —
	// the pipe is closed after exec has reaped it — and a reading written into a
	// stopped row would be a claim about a model nothing is loading.
	if s.status.State != StateStarting && s.status.State != StateRunning {
		return
	}
	s.status.Offload = parseOffloadLine(line, s.status.Offload)
}

// Binary is the launcher the current or last process was started from, empty before
// the first start. It is what a caller needs to answer "which build is this server
// running", which the status itself does not carry.
func (s *Server) Binary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binary
}

// Status returns a snapshot for the UI.
func (s *Server) Status() Status {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	st.Log = s.log.tail(12)
	return st
}

func (s *Server) setState(state string) {
	s.mu.Lock()
	s.status.State = state
	s.status.Error = ""
	s.mu.Unlock()
	s.publish()
}

func (s *Server) fail(message string) {
	s.mu.Lock()
	s.status.State = StateFailed
	s.status.Error = message
	s.status.PID = 0
	// The reading described the process that just died, whether it died while
	// loading or hours later. A failed row that still says which device the layers
	// went to is a claim about a model that is not running.
	s.status.Offload = Offload{}
	s.mu.Unlock()
	s.publish()
}

func (s *Server) publish() {
	if s.onState != nil {
		s.onState(s.Status())
	}
}

// args builds the command line. Only flags llama-server documents are used, and
// the user's own arguments come last so they can override one of ours.
func (o Options) args() []string {
	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	args := []string{
		"-m", o.Model,
		"--host", host,
		"--port", strconv.Itoa(o.Port),
	}
	if o.Ctx > 0 {
		args = append(args, "-c", strconv.Itoa(o.Ctx))
	}
	// How much of the model goes on the GPU. Only the two explicit modes are
	// passed: an "auto" offload is llama.cpp's own default, and asking for it in
	// words would be a flag on a build that may not read it the same way.
	if n := gpuLayers(o.Offload); n != "" {
		args = append(args, "--gpu-layers", n)
	}
	// The level whose output carries the placement reading. Before Extra, so a
	// verbosity the user typed themselves is the one that counts.
	if o.Trace {
		args = append(args, "-lv", strconv.Itoa(TraceVerbosity))
	}
	// Only the explicit flash-attention modes are passed. The default is "auto",
	// and asking for "auto" should not turn into a flag the server might read
	// differently on a build that spells it another way.
	switch o.FlashAttention {
	case FlashAttentionOn, FlashAttentionOff:
		args = append(args, "--flash-attn", o.FlashAttention)
	}
	// F16 is a no-op that only makes the command line longer, so it is left out.
	// Whatever else the settings hold was validated before it got here.
	if k := strings.TrimSpace(o.CacheTypeK); k != "" && k != CacheTypeF16 {
		args = append(args, "--cache-type-k", k)
	}
	if v := strings.TrimSpace(o.CacheTypeV); v != "" && v != CacheTypeF16 {
		args = append(args, "--cache-type-v", v)
	}
	return append(args, o.Extra...)
}

// freePort asks the kernel for an unused port.
//
// It is released again before llama-server binds it, so another process could in
// theory take it first. The alternative — handing llama-server a socket that is
// already open — is not something it accepts, and the readiness probe turns the
// rare loss into a reported failure instead of a silent one.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unerwartete Adresse für den freien Port")
	}
	return addr.Port, nil
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
}

// SplitArgs splits a command-line fragment the way a shell would for simple
// quoting, so an argument containing spaces survives: `--lora "/home/me/my
// adapter.gguf"`. It is not a shell: no expansion, no substitution, and a
// character that would be special to one is passed through as itself.
func SplitArgs(s string) []string {
	var (
		args    []string
		current strings.Builder
		started bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			// An empty quoted argument is still an argument.
			started = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if started {
				args = append(args, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if started {
		args = append(args, current.String())
	}
	return args
}

func formatTail(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" — letzte Ausgabe:\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ring keeps the last n lines a process wrote.
//
// The tail is the whole point of capturing anything: "llama-server wurde beendet"
// explains nothing, while the last lines of a failed model load name the missing
// file or the unsupported quantisation. A fixed window keeps a server that logs
// for hours from growing the app's memory with it.
type ring struct {
	mu    sync.Mutex
	lines []string
	max   int
	// watch, when set, is handed every line as it arrives, outside the lock. It is
	// for the readings that have to survive the window being small — the placement
	// of a model's layers is printed early and the log after it is long. It must
	// not block and must not call back into the ring.
	watch func(string)
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) addf(format string, args ...any) {
	r.add(fmt.Sprintf(format, args...))
}

func (r *ring) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	watch := r.watch
	r.mu.Unlock()
	// Called with no lock held: a watcher reads the server's status, which is
	// guarded by its own mutex, and taking the two in the other order elsewhere
	// would be a deadlock waiting for a log line.
	if watch != nil {
		watch(line)
	}
}

func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.lines) {
		n = len(r.lines)
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}

type lineWriter struct {
	r    *ring
	mu   sync.Mutex
	part strings.Builder
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			w.r.add(w.part.String())
			w.part.Reset()
			continue
		}
		// A runaway line without a newline must not grow without bound either.
		if w.part.Len() < 4096 {
			w.part.WriteByte(b)
		}
	}
	return len(p), nil
}

// writer is the io.Writer both output streams share, so the order of a server's
// own messages is preserved in the tail.
func (r *ring) writer() *lineWriter { return &lineWriter{r: r} }
