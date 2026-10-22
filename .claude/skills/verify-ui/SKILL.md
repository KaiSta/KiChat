---
name: verify-ui
description: Use when changing anything the window renders, adding or changing a bridge binding the UI calls, checking a UI state by eye, or when a screen renders empty, blank or stale.
---

# Verifying the UI

The real app is not reachable as a web page: its UI is not served over HTTP and
`window.go` is injected by the Go runtime, so opening `frontend/dist` in a browser
shows a bootstrap failure. There are three ways to look at it instead.

## The harness (preferred)

```sh
make frontend                 # or: cd frontend && npm run build
go run ./tools/uiharness      # http://127.0.0.1:8791/
```

A purple **MOCK BRIDGE** badge marks it, so a harness page is never mistaken for
the real app. The mock is `go:embed`ed: an edit to `mock-wails.js` needs a restart,
so use `go run` rather than a stale build.

The fixture chat deliberately contains every markdown feature — headings, a table,
a blockquote, inline and display math — a highlighted code block *beside an
unlabelled one* (they get different classes and a different colour), a `markdown`
fence for the one heading token the dark theme overrides, and a second chat with a
seeded search message and a compaction.

Query flags, each of which exists because the state is otherwise unreachable:

| Flag | Renders |
| --- | --- |
| `?throttle=1` | a stream paused like a rate limit: the phase and the seconds it names |
| `?pace=1` | a turn held back because the minute has no room — nothing was refused |
| `?spread=1` | a turn spaced one round per slot; the wait is paid before *each* round |
| `?crash=render` | an unrenderable message, to prove the error boundary still catches it |
| `?ceiling=8000` | the context meter measuring against a per-request ceiling instead of the window |
| `?ceiling=1200` | the projected request already over that ceiling (red row, danger badge) |
| `?ceilingsource=refusal` | the inspector's other ceiling row — from a refused request |
| `?images=sdserver&server=running\|starting\|failed\|restart` | the picture-server rows |
| `?images=nim` | the hosted picture kind: no folder, no process, the model select and the key borrowed from the Backends tab; add `&key=none` for the keyless state (red line, Test disabled) |
| `?images=nim&nimModel=<id>` | that same panel with a stored hosted id — the state where the select used to show schnell while the draw sent an id from an older build; `<id>` outside the offered list renders the extra, marked option and its warning line |
| `?upscale=esrgan` | the neural method switched on and found; add `&missing=1` for switched on but not installed |
| `?upscale=esrgan&upscalehold=1` | a neural enlargement still running: the phase row and the Stop that ends it |
| `?upscaler=install` | the Real-ESRGAN download row with its button and its size; pressing it runs a four‑step fake download and ends with the switch on (see `UPSCALING.md` §6) |
| `?upscaler=noarchive` | a platform with nothing published: the row says so where the button would be |
| `?startwithapp=1` | the start-with-app switch |
| `?missingmodel=1` | a chat pinned to a model the catalogue does not list: the notice above the composer and the switch it offers |

`?server=restart` is computed from the stored command line rather than stored,
which is what makes a save under a running server raise the badge. The runtime and
Backends fixtures have their own table in `LLAMACPP.md` §6, and the picture-server
rows in `IMAGES.md` §8 — update those when a flag changes.

**An unmodelled binding resolves and logs `unmodelled binding called: X`.** If the
console shows that, the mock needs X when you exercise that path — add it rather
than working around the warning.

**Render the empty case.** The harness always had a managed model configured, so
`GetRuntimeStatus` was only ever rendered with a populated `models` array — while a
real fresh install sends `null` (a nil Go slice marshals to `null`, not `[]`), and
Settings → Backends died on `.find` of null. Any new binding that returns a slice
needs its empty case in the harness, or its JSON pinned in a root-level Go test the
way `runtime_status_test.go` does.

What the harness **cannot** prove: backend behaviour. It proves rendering, state
flow and event wiring. Backend truth is the Go tests and the live tests.

## The real window (only when it is needed)

`xdotool`/`import` against a native Wails window works but is full of traps. What
was measured here:

- **Run detached**: `setsid nohup ./build/bin/kichat >/tmp/kichat.log 2>&1
  </dev/null &`. A plain background launch dies with `Ctrl+C detected. Shutting
  down...` when a tool call times out, because the timeout kills the process group
  — it looks exactly like a crash, so check the log before believing it.
- **Coordinates**: `xwininfo -id <id>`'s *Absolute upper-left* is the origin of the
  content (verified pixel-exact: cropping at `1440x900+1200+662` equalled an
  `import -window <id>` capture, RMSE 0). `xdotool getwindowgeometry` and
  `mousemove --window` are off by the frame extents (10 px, 40 px here), so aim
  with absolute coordinates.
- **Capture** with `import -window <id>`, wrapped in `timeout`: if the window is
  gone, `import` blocks forever. `import -window root` is the composited screen,
  useful for locating a window but cluttered.
- **Synthetic input partially works**: sidebar clicks landed, clicks in the main
  area did not, with the app confirmed as `_NET_ACTIVE_WINDOW`. Do not aim near
  "Dateien hinzufügen" — a miss opens a modal GTK dialog that swallows all input.
- Prefer the harness, or keyboard navigation, for anything but a final look.

## Pinning the streaming renderer

```sh
node tools/streamcheck/check.mjs      # Node 22.18+; needs `cd frontend && npm install`
```

It renders every prefix of four sample replies twice — finished and arriving — and
fails if the visible text differs, if the pieces `frontend/src/lib/stream.ts` cuts
would change as the text grows (the memo depends on their being append-only), or if
a line is lost in the split. It imports the real module, runs in under a second,
and is **not** part of `make check`.

Why it exists: re-parsing the whole reply on every delta cost 237 ms of parsing for
a 4 kB answer, 1.7 s for 12 kB and 6 s for 24 kB. Driving the harness in headless
Chromium, one streamed reply tore down and rebuilt 4511 elements and rewrote the
message 9576 times — the stutter. Parsing each settled piece once, throttling the
live piece and showing an open code fence as plain text brings those replies to 46,
137 and 266 ms.
