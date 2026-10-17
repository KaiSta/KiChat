package images

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file is the third kind of image backend: the *same* local build the CLI kind runs,
// started as a server instead of once per picture.
//
// It exists because of one measurement and one cost. The measurement is the trade stated at
// the top of cli.go: a one-shot process pays a model load per picture — minutes on a CPU for
// a q5 diffusion model — while a server pays it once and then spends only the sampling. The
// cost is the other side of the same trade, and it is why this is not the default: a process
// that stays up holds the memory its model was loaded into, and the user decides when that
// starts and when it stops.
//
// Nothing about *what* is loaded is decided here. The command line is the model half of the
// CLI's own (cli.go, `modelArgs`) with a listener appended, because `sd-cli` and `sd-server`
// are one executable's two faces from one archive: same flags, same folder, same model set.
// Keeping those two command lines in one place is the whole of the reason `modelArgs` exists,
// and it is what makes switching between the two lifetimes a change of `kind` rather than a
// second configuration to fill in.
//
// Two differences are deliberate:
//
//   - **The listener is bound to the loopback interface**, the way the managed llama.cpp
//     server is. This is a local model; an unauthenticated endpoint on every interface is a
//     port on the network, not a convenience.
//   - **The port is chosen by the app, never defaulted.** `sd-server` documents 1234 as its
//     own default, and 1234 is LM Studio's default too — which is a backend this app is
//     already pointed at on many machines. A collision there would either fail the start or,
//     worse, leave KiChat talking to the wrong server.

// serverHost is the only address the managed server is ever started on.
const serverHost = "127.0.0.1"

// DefaultServerReadyTimeout bounds a start: how long sd-server may take to load its model
// before the wait is reported as a failure.
//
// Longer than the chat runtime's own 180 seconds on purpose, because the thing being loaded
// is not the same size or the same shape: a llama.cpp GGUF streams off disk, while a q5
// diffusion pipeline plus its VAE and text encoder is several gigabytes and takes minutes on
// a CPU — the very load DefaultCLITimeout gives half an hour to, since the one-shot kind
// pays it on every picture. A bound carried over unchanged would report a working setup as
// broken minutes before it became ready, and the failure would name the model.
const DefaultServerReadyTimeout = 15 * time.Minute

// ServerReadyPath is the route the readiness probe asks for.
//
// It is an API route rather than a health endpoint on purpose. `sd-server` documents no
// /health — that is llama.cpp's — and the A1111-shaped routes it does serve are the ones this
// app uses: `/sdapi/v1/sd-models` answers 200 once a model is loaded, which is exactly the
// question a start has to answer. A probe on any other route would certify a server that
// cannot yet draw.
const ServerReadyPath = "/sdapi/v1/sd-models"

// listenFlag is the flag that moves the listener off sd-server's own default.
//
// It is spelled here once because two things read it: the argument list below, and the check
// that asks the build whether it has a server mode at all (serverSupport).
const listenFlag = "--listen-port"

// ServerKey is what a running server's configuration is compared by: the program it would run
// and the half of its command line that describes the load, as one string.
//
// The listener is deliberately not part of it. A port is chosen per start, so including one
// would make every running server look out of date the moment it came up. The *program* is part
// of it, because it is the other half of what a start depends on: pointing the folder at a
// different unpack is a different executable, even when every flag is identical. So is the VAE
// decode mode, because tiling is also a property of the load rather than of the request. The
// two are compared as configured (the folder and the typed path) rather than as resolved,
// because resolving is a filesystem walk that can fail — and a comparison that has to fail
// before it can answer is a comparison the panel would have to render as an error.
func (c *CLI) ServerKey() string {
	key := make([]string, 0, len(c.modelArgs())+len(c.vaeArgsConfigured())+2)
	key = append(key, c.folder, c.binary)
	key = append(key, c.modelArgs()...)
	// Unprobed, deliberately: this runs on a path that must not spawn a process, and the
	// question is "does the running server match what is configured" rather than "what
	// would this build accept today". See vaeArgsConfigured.
	key = append(key, c.vaeArgsConfigured()...)
	return strings.Join(key, "\x00")
}

// ServerSpec is one start of the managed server: the program to run, the command line after
// it, and what a status row shows while it runs.
type ServerSpec struct {
	// Program is the resolved sd-server executable, absolute where it could be resolved.
	Program string
	// Args is `os.Args[1:]`: the configured model set, the user's own arguments, and the
	// listener.
	Args []string
	// Model is the primary model file the start loads. It is the diffusion model when the
	// pipeline names one and the single-file checkpoint otherwise, which is the same choice
	// ModelFiles.Label makes — and it is a *file*, so a start can check that it is there
	// before a process is spawned with an argument pointing at nothing.
	Model string
	// Label names the model set for the row ("qwen-image-2.1-Q5_K_M.gguf").
	Label string
}

// ServerSpec builds the command line that starts this configuration as a server on the given
// port.
//
// The refusals mirror the ones a draw makes, and for the same reason: a server is minutes of
// loading and gigabytes of memory, so a configuration that cannot draw must be refused before
// a process exists rather than after. Two of them are new here — no model at all, because a
// server with nothing to load is a process that answers every request with an error, and a
// build whose own help does not list the listener flag, because starting it anyway would run
// it on its documented default port.
func (c *CLI) ServerSpec(ctx context.Context, port int) (ServerSpec, error) {
	program, err := ResolveServerProgram(c.folder, c.binary)
	if err != nil {
		return ServerSpec{}, err
	}
	if c.files.Empty() {
		return ServerSpec{}, fmt.Errorf("%w: kein Modell angegeben", ErrUnavailable)
	}
	if err := c.modelContradiction(); err != nil {
		return ServerSpec{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	model := strings.TrimSpace(c.files.DiffusionModel)
	if model == "" {
		model = strings.TrimSpace(c.files.Model)
	}
	if model == "" {
		return ServerSpec{}, fmt.Errorf("%w: kein Diffusionsmodell angegeben", ErrUnavailable)
	}
	if err := c.serverSupport(ctx, program); err != nil {
		return ServerSpec{}, err
	}
	args := c.modelArgs()
	args = append(args, c.vaeArgs(ctx, program, c.opts.VaeTiling)...)
	args = append(args, "--listen-ip", serverHost, listenFlag, strconv.Itoa(port))
	return ServerSpec{Program: program, Args: args, Model: model, Label: c.files.Label()}, nil
}

// serverSupport reports whether this build's own help lists the listener flag.
//
// Asked rather than assumed, the way previewSupport asks about the preview options, and for
// the same class of reason: the project tells users to read `-h` because its options change
// between builds. The consequence here is a port rather than a flag — a build that ignored an
// unknown option and carried on would listen on 1234, which is LM Studio's address on many
// machines, and a picture would then be requested from whatever answered there.
//
// A build that cannot answer `-h` at all is *not* refused. There is no evidence against it,
// and a launcher that will not print usage but does start a server is a working setup; Health
// takes the same line about the one-shot build.
func (c *CLI) serverSupport(ctx context.Context, program string) error {
	help, err := c.help(ctx, program)
	if err != nil && help == "" {
		return nil
	}
	if !strings.Contains(help, listenFlag) {
		return fmt.Errorf("%w: %s kennt %s nicht — dieser Build hat keinen Servermodus",
			ErrUnavailable, program, listenFlag)
	}
	return nil
}
