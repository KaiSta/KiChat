# Working on KiChat

KiChat is a local-first desktop LLM research chat: `main.go` + `app.go` + `api.go` on
top of Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`.
It is a **Wails v2** application (Go binary + webkit2gtk webview), talking to LM Studio
(`:1234`) and Ollama (`:11434`), with Brave/DuckDuckGo web search and SQLite storage.
`PLAN.md` describes the architecture and the reasoning behind it.

## Environment (verified, not assumed)

| Tool | Version here | Consequence |
| --- | --- | --- |
| Go | 1.26.0 (`go 1.26.0`) | fine |
| Wails CLI | 2.15.0 | fine |
| Node / npm | 18.19.1 / 9.2.0 | **two traps, see below** |
| webkit2gtk | only 4.1 | **every build needs `-tags webkit2_41`** |

**Trap 1 — build tag.** Only `webkit2gtk-4.1` is installed, so
`wails build -tags webkit2_41` is the only build that works. Without the tag it fails
to link.

**Trap 2 — npm 9 cannot read the `libc` field.** On this Node/npm a clean
`npm install` skips `@tailwindcss/oxide-linux-x64-gnu`, and Vite then dies with
`Cannot find native binding`. npm only learned libc-aware optional resolution in v10.
The working route on Node 18 is:

```bash
cd frontend && npm i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3
```

Declaring it in `package.json` and regenerating the lockfile were both tried and both
fail; do not repeat that. **`vite dev` is impossible on Node 18** (Vite 7 needs
≥ 20.19 / ≥ 22.12 and dies with `crypto.hash is not a function`) — `vite build` works.

## Checks that must pass

```bash
gofmt -l .                                  # must print nothing
go vet ./...
go test ./... -count=1
cd frontend && npx tsc --noEmit             # or: npm run build (tsc && vite build)
wails build -tags webkit2_41                # produces build/bin/kichat
```

`wails build` regenerates `frontend/wailsjs/go/models.ts`, so after changing a Go
struct that crosses the bridge, build (or `wails generate module`) before type-checking
the frontend — otherwise `tsc` fails on the missing field, and the generated diff should
contain only the new fields.

## Live tests (opt-in, use real metered services)

Offline tests never touch the network. Live ones are gated by environment variables:

```bash
# Brave Search: one credit per request — ~4 requests for the tests below
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/research/ -run TestLive -v

# LM Studio: needs a *loaded* chat model; skips itself if none is
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/chat/ -run TestLive -v
KICHAT_LIVE=1 go test ./internal/chat/ -run TestLiveEngineTurn -v   # needs KICHAT_LMSTUDIO
```

What each live test buys:

- `TestLiveBraveContext` — the real `/res/v1/llm/context` route returns page chunks.
- `TestLiveBraveContextClampsLongQuery` — local clamping beats the API's 422.
- `TestLiveBraveChainUsesContext` — the chain picks the context route, not the fallback.
- `TestLiveBraveBadKeyIsNotMisclassified` — a rejected key is not reported as a throttle.
- `TestLiveSearchFirstTurn` — settings → chain → context → seeded prompt → LM Studio,
  and asserts the context payload is bigger than the headline listing it replaces.

**Never put an API key in a tracked file.** Pass it in the environment; the app itself
stores it through `internal/secret` (`~/.config/kichat/`). Measured facts worth keeping:
a rejected key comes back as **HTTP 422 "The provided API key is invalid."** (not 401),
and a used-up balance vs a momentary throttle can only be told apart by the error
envelope's `code` (`QUOTA_LIMITED` / `RATE_LIMITED`), because both can arrive as 429.

## Verifying the UI

### Preferred: the mock-bridge harness

The real app is not reachable as a web page — its UI is not served over HTTP, and
`window.go` is injected by the Go runtime into the webview, so opening `frontend/dist`
in a browser shows a bootstrap failure. `tools/uiharness` fixes that by serving the
built frontend with a mocked bridge:

```bash
cd frontend && npm run build          # the harness serves dist/
go run ./tools/uiharness             # http://127.0.0.1:8791/
```

Then open that loopback URL in the browser panel and drive it like any website: click,
type, read the accessibility tree, take screenshots, read the console. The page carries
a purple **MOCK BRIDGE** badge, so a harness page is never mistaken for the real app.

- `tools/uiharness/mock-wails.js` holds the fixtures: a chat whose assistant reply uses
  every markdown feature (headings, table, blockquote, highlighted code, inline and
  display math), a second chat with a seeded search message and a compaction, and
  streaming replies driven by real `chat:*` events over a simulated 40 ms cadence. The
  mock is `go:embed`ed, so a running harness picks up an edit to it only after a restart
  — use `go run`, or rebuild before restarting.
- Unmodelled bindings resolve **and log a warning** — if the console shows
  `unmodelled binding called: X`, the mock needs X when you exercise that path.
- `?crash=render` injects an unrenderable message to prove the error boundary still
  catches a render fault instead of leaving a blank window.
- It is a mock: it proves rendering, state flow and event wiring, **not** backend
  behaviour. Backend truth lives in the Go tests and the live tests above.

### When the real window is needed

`xdotool`/`import` against a native Wails window is possible but full of traps. What
was actually measured here:

- **Run the app detached**: `setsid nohup ./build/bin/kichat >/tmp/kichat.log 2>&1 </dev/null &`.
  A plain background launch dies with `Ctrl+C detected. Shutting down...` whenever a
  tool call times out, because the timeout kills the whole process group. That looks
  exactly like a crash — check the log before believing it.
- **Window coordinates**: use `xwininfo -id <id>`'s *Absolute upper-left* as the origin of
  the content (verified by a pixel-exact comparison: cropping the screen at
  `1440x900+1200+662` equals an `import -window <id>` capture, RMSE 0). `xdotool
  getwindowgeometry` and `mousemove --window` are off by the frame extents (10 px, 40 px)
  here, so aim with absolute coordinates: `xdotool mousemove $((1200+x)) $((662+y))`.
  Window decoration may not be where X claims it is — verify a click by its effect.
- **Capture** with `import -window <id>` for an occlusion-free image of the app. Wrap it
  in `timeout`: if the window is gone, `import` **blocks forever** waiting for it. The
  root capture (`import -window root`) is the composited screen, useful for locating a
  window but cluttered by other applications.
- **Synthetic input partially works**: sidebar clicks landed, clicks in the main area did
  not, with the app confirmed as `_NET_ACTIVE_WINDOW`. Also `xdotool windowactivate --sync`
  can block under focus-stealing prevention. Do not aim near "Dateien hinzufügen": a
  miss opens a modal GTK file dialog that swallows all further input.
- Prefer the harness, or keyboard navigation, for anything but a final look.

## Conventions

- Comments explain **why**, not what; user-facing strings go through
  `frontend/src/lib/i18n.ts` (de and en) — a hardcoded label is a bug.
- No compile suppressions, no unused dependencies, no swallowed errors. If a test needs
  weakening, say so out loud instead of doing it quietly.
- New settings fields need no migration: the settings blob is one JSON row decoded over a
  fully populated default, so absent fields keep their default (pinned by
  `internal/config/config_test.go`). Do not change an existing field's type.
- Tests pin behaviour (what the user or the model sees), not implementation details.
- `.freebuff/` is gitignored scratch space; `frontend/dist`, `build/bin` and `node_modules`
  are build output.
