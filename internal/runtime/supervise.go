package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"kichat/internal/proc"
	"kichat/internal/uimsg"
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
	// CacheTypeF32 and CacheTypeBF16 are the other two KV cache types that are not
	// quantized, so they are the company a quantized V cache needs flash attention
	// without. Spelled here for the same reason as F16, and pinned by a test.
	CacheTypeF32  = "f32"
	CacheTypeBF16 = "bf16"
	// SpecTypeDraftMTP is llama.cpp's name for speculative decoding that uses the
	// model's own multi-token prediction heads (docs/speculative.md). The heads ship in
	// the model's own GGUF, so there is no second file to name.
	SpecTypeDraftMTP = "draft-mtp"
)

// Options describe one server to start.
type Options struct {
	// Binary is the launcher, absolute.
	Binary string
	// Model is the file to load. For llama.cpp it is the GGUF itself; for another
	// program it is the primary file of the set the command line names, because this
	// is what a start checks for and what the row shows.
	Model string
	// ModelDir says Model is a directory rather than a file, which is what
	// mlx_lm.server's `--model` takes: an MLX model is a snapshot folder holding
	// config.json and the weights, not a single archive. Without this the folder
	// check below would reject the only shape MLX has.
	ModelDir bool
	// MMProj is the multimodal projector to load beside the model, `--mmproj`, and
	// empty for a text-only server. A vision model is two files: without the
	// projector llama.cpp has no vision tower to hand an image to, so it answers the
	// turn with "image input is not supported" — a 500 the user reads as this app
	// being broken, on a model that looks like it should be able to see.
	MMProj string
	// Args, when set, is the whole command line after the binary, built by the caller.
	// It replaces the llama.cpp argument list this package builds, which is how a
	// second program is supervised by the same code: everything else a start does —
	// the free port, the readiness poll, the log ring, the shutdown — is the same work
	// whatever the flags are called. Empty means the llama.cpp list below.
	Args []string
	// ReadyPath is the route the readiness probe asks for, "/health" when empty. It is
	// a field because the answer is the program's own: llama-server serves /health and
	// says 503 while it loads, while an image server has no such route and has to be
	// asked the question the app will really ask it.
	ReadyPath string
	// Label names the program in messages and in the log the panel shows, llama-server
	// when empty. A row that said "llama-server wurde beendet" about sd-server would be
	// wrong in the one place a user looks for a reason.
	Label string
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
	// ExpertOffload keeps a mixture-of-experts model's expert tensors in system
	// RAM: ExpertOffloadCPU passes `--cpu-moe`, ExpertOffloadLayers passes
	// `--n-cpu-moe ExpertLayers`. Empty passes nothing, which is llama.cpp's own
	// placement and the right answer for a dense model.
	ExpertOffload string
	// ExpertLayers is the N of `--n-cpu-moe N`, read only with ExpertOffloadLayers.
	ExpertLayers int
	// MTPDraft is the N of `--spec-draft-n-max N`, sent with `--spec-type draft-mtp`:
	// the model's own heads draft that many tokens and the model verifies them in one
	// batch. Zero sends nothing, which is llama.cpp's default of no speculation, and is
	// also what a file without the heads gets — see the header check at the start.
	MTPDraft int
	// MTPConfidence is the P of `--spec-draft-p-min P`: the head stops drafting once it
	// is less sure than this, which is what lets a longer draft pay. Zero is
	// llama.cpp's own "no floor" and sends nothing; it is only read with MTPDraft.
	MTPConfidence float64
	// MTPKVOverride is one `--override-kv` pair for the metadata key a file is missing,
	// sent only alongside a draft count. The value this app builds is
	// GGUFInfo.MTPKVOverride(): the head count of a file whose conversion kept the nextn
	// tensors and dropped the key llama.cpp splits the block table by, which would
	// otherwise load with every head reported unused.
	MTPKVOverride string
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

	// Serialize starts and stops so a timeout from an old process cannot stop a replacement.
	lifecycle sync.Mutex
	mu        sync.Mutex
	cmd       *exec.Cmd
	waited    chan struct{}
	// binary is the launcher the current — or last — process was started from. A
	// running server keeps the build it started with, so this is what tells the
	// cleanup list which build must not be deleted while it answers.
	binary string
	// label is what the last start called the program, for the messages about it. It
	// is remembered per server because the process outlives the Options it was started
	// from, and "llama-server wurde beendet" about an image server is wrong in the one
	// place a user looks for a reason.
	label string
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
	// First, because it is a contradiction between two settings rather than anything
	// about the files: a start that got as far as the binary would report the wrong
	// reason for the same failure.
	if err := opts.cacheAttentionRefusal(); err != nil {
		return err
	}
	label := opts.label()
	if opts.Binary == "" {
		return uimsg.Errorf("runtime.binaryMissing", label)
	}
	if opts.Model == "" {
		return uimsg.Errorf("model.noneSelected")
	}
	// Checked here rather than left to exec, so the user is told which of the two
	// paths is wrong while the server is not yet involved.
	if _, err := os.Stat(opts.Binary); err != nil {
		return uimsg.Errorf("runtime.binaryNotFound", label, err)
	}
	model, err := os.Stat(opts.Model)
	if err != nil {
		return uimsg.Errorf("model.notFound", err)
	}
	if model.IsDir() && !opts.ModelDir {
		return uimsg.Errorf("runtime.modelIsFolder", opts.Model)
	}

	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if err := s.stop(); err != nil {
		return err
	}

	host := opts.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.Port
	if port == 0 {
		port, err = FreePort()
		if err != nil {
			return uimsg.Errorf("runtime.freePort", err)
		}
	}
	opts.Port = port

	args := opts.commandArgs()
	readyPath := opts.ReadyPath
	if readyPath == "" {
		readyPath = "/health"
	}
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
	proc.StartInOwnGroup(cmd)

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
		return uimsg.Errorf("runtime.startServer", label, err)
	}

	url := fmt.Sprintf("http://%s:%d", host, port)
	s.mu.Lock()
	s.cmd = cmd
	s.binary = opts.Binary
	s.label = label
	s.waited = make(chan struct{})
	s.stopRequested = false
	s.status.PID, s.status.URL = cmd.Process.Pid, url
	waited := s.waited
	s.mu.Unlock()
	s.publish()

	s.log.addf("%s gestartet (PID %d) auf %s", label, cmd.Process.Pid, url)
	s.log.addf("Aufruf: %s %s", opts.Binary, strings.Join(args, " "))
	go s.monitor(cmd, url, readyPath, waited)
	return nil
}

// monitor waits for the process to become ready, then for it to exit.
func (s *Server) monitor(cmd *exec.Cmd, url, readyPath string, waited chan struct{}) {
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
			s.finished(cmd, err)
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				s.lifecycle.Lock()
				s.mu.Lock()
				current := s.cmd == cmd && !s.stopRequested
				s.mu.Unlock()
				if current {
					label := s.program()
					s.log.addf("Zeitüberschreitung: %s war nach %s nicht bereit", label, s.readyTimeout)
					s.fail(fmt.Sprintf("%s war nach %s nicht bereit", label, s.readyTimeout))
					_ = s.stop()
				}
				s.lifecycle.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := s.probe(ctx, url+readyPath)
			cancel()
			if err == nil {
				// A probe may finish after Stop or after a replacement has started.
				s.mu.Lock()
				current := s.cmd == cmd && !s.stopRequested && s.status.State == StateStarting
				if current {
					select {
					case <-waited:
						current = false
					default:
						s.status.State = StateRunning
						s.status.Error = ""
					}
				}
				s.mu.Unlock()
				if current {
					s.publish()
					s.log.addf("Modell geladen, Server bereit")
				}
				// Wait even if the process exited during the probe: its failure must
				// still be reported unless a stop or a replacement owns the row.
				s.finished(cmd, <-exit)
				return
			}
		}
	}
}

// program is what the last start called the program, llama-server when nothing was
// started yet. Messages about a process that has already exited are built from this
// rather than from the Options of the call that started it, which is long gone.
func (s *Server) program() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.label == "" {
		return "llama-server"
	}
	return s.label
}

// finished records the exit of a process that is no longer running.
func (s *Server) finished(cmd *exec.Cmd, err error) {
	// Take the log tail before the state lock: the log watcher also takes it.
	tail := s.log.tail(6)
	s.mu.Lock()
	if s.cmd != cmd {
		s.mu.Unlock()
		return
	}
	label := s.label
	if label == "" {
		label = "llama-server"
	}
	requested := s.stopRequested
	state := s.status.State
	s.cmd = nil

	switch {
	case requested, state == StateFailed:
		// A stop the user asked for is not a failure, and a failure has already
		// been reported with a better message than the exit code gives. The
		// identity is cleared here as well as in Stop, because this runs first
		// when the process notices the signal before the caller does — and a
		// stopped server still showing a PID is a stopped server that looks
		// running.
		if state != StateFailed {
			s.status.State = StateStopped
		}
	case err != nil:
		s.status.State = StateFailed
		s.status.Error = fmt.Sprintf("%s wurde beendet: %v%s", label, err, formatTail(tail))
	default:
		s.status.State = StateFailed
		s.status.Error = fmt.Sprintf("%s hat sich beendet", label)
	}
	s.status.PID, s.status.Port, s.status.URL = 0, 0, ""
	s.status.Offload = Offload{}
	s.mu.Unlock()
	if !requested && state != StateFailed {
		if err != nil {
			s.log.addf("%s beendet: %v", label, err)
		} else {
			s.log.addf("%s beendet", label)
		}
	}
	s.publish()
}

// Stop shuts the server down and waits for it to be gone. It is safe to call when
// nothing is running.
func (s *Server) Stop() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.stop()
}

// stop runs with lifecycle held.
func (s *Server) stop() error {
	s.mu.Lock()
	cmd, waited := s.cmd, s.waited
	if cmd == nil || cmd.Process == nil {
		s.mu.Unlock()
		return nil
	}
	s.stopRequested = true
	s.mu.Unlock()

	// The graceful request first: a server frees its context and exits cleanly, which is
	// what keeps a large model's memory from being reclaimed by the OS only after the
	// process is killed. What that request *is* differs per platform — a SIGTERM on Unix,
	// TerminateProcess on Windows, where there is no graceful half at all — so the two
	// lines below name it through stopRequestName rather than saying SIGTERM on a system
	// that has no such thing. See internal/proc.
	if err := proc.Terminate(cmd, false); err != nil {
		s.log.addf("%s konnte nicht zugestellt werden: %v", proc.StopRequestName(), err)
	}
	select {
	case <-waited:
	case <-time.After(s.grace):
		s.log.addf("keine Reaktion auf %s, Prozess wird beendet", proc.StopRequestName())
		if err := proc.Terminate(cmd, true); err != nil {
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

// Program is what the last start called the program, so a caller that stops several
// servers can say which one it was talking about rather than naming llama.cpp in a
// message about a picture server.
func (s *Server) Program() string { return s.program() }

// markStopped records that nothing is running any more.
//
// A row that already says "failed" keeps saying it. Stopping a server that failed
// to become ready — which the readiness timeout does, right after it reports the
// failure — must not downgrade that to "stopped": the reason would survive in
// Error while the state contradicted it, and a load that timed out is not the same
// thing as a server the user stopped.
func (s *Server) markStopped() {
	s.mu.Lock()
	if s.status.State != StateFailed {
		s.status.State = StateStopped
	}
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
	// The address described it too, and it is the more dangerous of the two claims:
	// a URL on a failed row is one a user can click, a browser can reach, or another
	// tool can be pointed at — and it answers nothing, because the process behind it
	// is gone. A row that is red and says "failed" must not also carry a way in.
	s.status.Port = 0
	s.status.URL = ""
	s.mu.Unlock()
	s.publish()
}

func (s *Server) publish() {
	if s.onState != nil {
		s.onState(s.Status())
	}
}

// cacheAttentionRefusal reports a V cache type the server cannot load with flash
// attention off.
//
// llama.cpp implements a quantized V cache through flash attention and refuses the model
// otherwise — "quantized V cache requires flash_attn to be enabled", measured on build
// 12189 — so the contradiction is caught here, where it can be a sentence about the two
// settings instead of a log line from a load that already failed. "auto" is not a
// contradiction: the server switches flash attention on by itself for such a cache
// (same build, a q8_0 cache starts with no flag and is refused only with `off`).
func (o Options) cacheAttentionRefusal() error {
	v := strings.TrimSpace(o.CacheTypeV)
	if v == "" || o.FlashAttention != FlashAttentionOff {
		return nil
	}
	switch v {
	case CacheTypeF16, CacheTypeF32, CacheTypeBF16:
		return nil
	}
	return uimsg.Errorf("runtime.quantizedVCacheNeedsFlashAttention", v)
}

// label is what the messages call this program.
func (o Options) label() string {
	if l := strings.TrimSpace(o.Label); l != "" {
		return l
	}
	return "llama-server"
}

// commandArgs is the command line to run: the caller's own when it built one, the
// llama.cpp list below otherwise.
func (o Options) commandArgs() []string {
	if len(o.Args) > 0 {
		return o.Args
	}
	return o.args()
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
	// A mixture-of-experts model reads only the few experts that fire per token, so
	// keeping the expert tensors in system RAM costs bandwidth rather than
	// correctness — which is what makes a 20.4 GB model answer on a 16 GB card. The
	// two flags are llama.cpp's own; "auto" passes neither, because the placement
	// that fits is what `--fit` already does.
	switch o.ExpertOffload {
	case ExpertOffloadCPU:
		args = append(args, "--cpu-moe")
	case ExpertOffloadLayers:
		args = append(args, "--n-cpu-moe", strconv.Itoa(o.ExpertLayers))
	}
	// Multi-token prediction, from the model's own heads. Both flags are needed: the
	// type is what turns the heads on, and the count is how far ahead they draft. Only
	// sent when there is a count, because llama.cpp's default is no speculation at all.
	if o.MTPDraft > 0 {
		// The key a conversion dropped comes first: llama.cpp reads metadata overrides in
		// get_key, so this is what makes the head count exist before the flags that use it.
		if kv := strings.TrimSpace(o.MTPKVOverride); kv != "" {
			// The user's own pair wins: llama.cpp applies the first matching
			// `--override-kv` it reads, so a duplicate appended after theirs would shadow
			// them despite coming later on the command line.
			key, _, _ := strings.Cut(kv, "=")
			if !o.userOverridesKV(key) {
				args = append(args, "--override-kv", kv)
			}
		}
		args = append(args, "--spec-type", SpecTypeDraftMTP, "--spec-draft-n-max", strconv.Itoa(o.MTPDraft))
		// The floor is only meaningful with a count to truncate, so it rides along with
		// it. FormatFloat with -1 prints the shortest form that round-trips, which is
		// "0.75" rather than "0.750000" — the flag takes a probability, not a shape.
		if o.MTPConfidence > 0 {
			args = append(args, "--spec-draft-p-min", strconv.FormatFloat(o.MTPConfidence, 'f', -1, 64))
		}
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
	// The projector is the other half of a vision model. Before Extra, like every
	// flag above: a user who names a different one themselves types it as an extra
	// argument, and theirs is the one that counts.
	if p := strings.TrimSpace(o.MMProj); p != "" {
		args = append(args, "--mmproj", p)
	}
	return append(args, o.Extra...)
}

// userOverridesKV reports whether the user's own arguments already set this metadata
// key, as `--override-kv KEY=TYPE:VALUE` with the pair possibly inside a comma list.
func (o Options) userOverridesKV(key string) bool {
	key = strings.TrimSpace(key)
	for i := 0; i < len(o.Extra); i++ {
		var value string
		switch {
		case o.Extra[i] == "--override-kv" && i+1 < len(o.Extra):
			value = o.Extra[i+1]
		case strings.HasPrefix(o.Extra[i], "--override-kv="):
			value = strings.TrimPrefix(o.Extra[i], "--override-kv=")
		default:
			continue
		}
		for _, pair := range strings.Split(value, ",") {
			if k, _, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(k) == key {
				return true
			}
		}
	}
	return false
}

// FreePort asks the kernel for an unused port.
//
// It is released again before the server binds it, so another process could in
// theory take it first. The alternative — handing the server a socket that is
// already open — is not something either of them accepts, and the readiness probe
// turns the rare loss into a reported failure instead of a silent one.
//
// It is exported for the callers that build their own command line and therefore
// have to put the port *in* it (sdserver.go): the supervisor's Start still allocates
// one when Port is 0, but a caller that needs the number before Start cannot wait for
// that.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, uimsg.Errorf("runtime.unexpectedPortAddress")
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
