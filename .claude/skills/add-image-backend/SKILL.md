---
name: add-image-backend
description: Use when adding a picture-generation backend, or changing the draw path — a new image server or hosted image API, the sd-cli/sd-server handling, generation parameters, hires, VAE tiling or progress reporting.
---

# Adding an image backend

Read `IMAGES.md` first — §2 (the three server kinds and the shared route), §3
(parameters: model-decided, server default, or fixed in Settings), §5 (progress and
stopping), §8 (how it is verified). The interface was written for this: it holds a
WebUI client, a one-shot CLI and a managed server at once.

## The interface

`internal/images/images.go`:

```go
type Provider interface {
    ID() string
    Name() string
    Health(ctx context.Context) error
    Models(ctx context.Context) ([]Model, error)
    Samplers(ctx context.Context) ([]string, error)
    Generate(ctx context.Context, req Request) (Result, error)
}
```

Two optional interfaces, and they are optional on purpose:

- `Interrupter` — `Interrupt(ctx)`. A1111/Forge have `POST /sdapi/v1/interrupt`;
  sd-server cancels by job id and has no such route. Not implementing it is a truer
  statement than a method that always fails.
- `Progressive` — step counts and previews. The app turns them into the progress
  event; a backend without it just shows less.

## Steps

1. **Implement the provider** in `internal/images/<name>.go`. Keep HTTP in the
   provider; the interface has none in it, which is what lets a CLI and a server
   share it.

2. **Decode defensively, because the failure mode is a wrong picture, not an
   error.** `Result` carries the MIME type and extension `SniffImage` read off the
   bytes (a server configured for JPEG must not produce a `.png`). Accept the
   `data:image/png;base64,` prefix an older build emitted. Read the seed and
   checkpoint out of `info`, which is a JSON *string* containing JSON.

3. **Bound every request** in `internal/images/limits.go` — size, steps, and the
   hires pass. A new backend must not become the one path that accepts a 16384 px
   request because the server would try.

4. **Wire it up** where the provider is constructed: `newImageProvider` /
   `imageCLI` in `app.go`, keyed by `config.ImageSettings.Kind`. A managed server
   that KiChat starts gets its address from the running process, never from the
   stored `BaseURL` — a stale port must not become a way to draw from someone
   else's server (1234 is both sd-server's default and LM Studio's).

5. **Settings and i18n.** Add the kind and its fields to `ImageSettings`, the
   Images tab in `Settings.tsx` (`isCLI` / `isSD` branches), and all three
   languages in `i18n.ts`.

6. **Tests.** Offline: request shape, response decoding, base64 prefix, MIME
   sniffing, error classification, limits. Live: `TestLiveSDWebUI`-style, gated on
   `KICHAT_LIVE` and its own variables, skipping cleanly when no server answers —
   see the `live-tests` skill.

## Rules that decide correctness

- **An empty parameter means "the model or the server decides", never a default
  this package chose.** A value typed in Settings is *fixed* and overrides what
  the model asked for.
- **A hires pass is verified from the returned picture's size**, not from the
  request. A server that silently ignores the keys answers 200 at base size.
- **One draw at a time.** The app owns the slot (`TryDraw` in `internal/tools`);
  a new backend does not get to run beside another.
- **Cancelling the request ends the wait; interrupting ends the work.** Do not
  make stopping depend on a route not every server has.
- **A draw that times out leaves the server drawing.** That is a known gap and a
  deliberate one — the interrupt is global to the WebUI process.

## Done when

- `make check` is green.
- A picture decodes to real bytes through `SniffImage`, with the right MIME type.
- The tool disappears when the backend is not configured or the managed server is
  stopped, rather than failing at call time.
- `IMAGES.md` names the new backend and what was measured.
