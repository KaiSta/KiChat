# Working on KiChat

KiChat is a local-first desktop LLM research chat: `main.go` + `app.go` + `api.go` on
top of Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`.
It is a **Wails v2** application (Go binary + webkit2gtk webview), talking to LM Studio
(`:1234`) and Ollama (`:11434`), with Brave/DuckDuckGo web search and SQLite storage.
`PLAN.md` describes the architecture and the reasoning behind it; `COMPACT.md` is the deep
dive on context compaction (strategy, arithmetic, failure modes, where the code is).

## Environment (verified, not assumed)

| Tool | Version here | Consequence |
| --- | --- | --- |
| Go | 1.26.0 (`go 1.26.0`) | fine |
| Wails CLI | 2.15.0 | fine |
| Node / npm | 22.23.3 / 10.9.9 in `~/.local/node/bin` | **not on `PATH`** — see Trap 2 |
| webkit2gtk | only 4.1 | **every build needs `-tags webkit2_41`** |

**Trap 1 — build tag.** Only `webkit2gtk-4.1` is installed, so
`wails build -tags webkit2_41` is the only build that works. Without the tag it fails
to link.

**Trap 2 — node and npm are not on `PATH`.** They are in `~/.local/node/bin`
(22.23.3 / 10.9.9) rather than in `/usr/bin`. Mint 22.3 (zena, noble base) offers
`nodejs` 18.19.1 and `npm` 9.2.0 in `noble/universe`, and neither is installed — nor
would it be enough, since npm 9 cannot install Vite 7's native binding (below) and
`vite dev` needs ≥ Node 20.19. The Makefile assumes `npm` is callable, and npm is a
script whose first line is `#!/usr/bin/env node`, so a *path* to npm is not enough:
with node off `PATH`, `make build` dies at its `npm install` step with
`/usr/bin/env: 'node': No such file or directory` (exit 127), and the bare
`npm run build` that `wails build` runs for itself (wails.json) cannot start at all.
Fix that in the shell, not in the Makefile — the Makefile is the same one machines
with a properly installed node use:

```bash
export PATH="$HOME/.local/node/bin:$PATH"   # put it in ~/.bashrc to make it stick
```

A system-wide Node (NodeSource is the maintained apt route on Mint:
`curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -`, then
`sudo apt install nodejs`, which brings its own npm) lands in `/usr/bin` and needs
none of the above. Do not `apt install npm` from Mint's own repo next to it: that is
npm 9, it conflicts, and it is the one this project cannot build with.

The npm 9 `libc` trap that used to live here is history: with npm 10 a clean
`npm install` *does* install `@tailwindcss/oxide-linux-x64-gnu` (verified in an empty
directory against the real lockfile), so the Makefile's extra `--no-save` install and the
directory-as-stamp are now only a guard for an older npm — declaring the binding in
`package.json` or regenerating the lockfile still does not work, tried, do not repeat it.
**`vite dev` needs ≥ Node 20.19 / ≥ 22.12** (below that Vite 7 dies with
`crypto.hash is not a function`); the Node installed here is new enough and
`npm run dev` serves `http://localhost:5173/` in under a second, but `vite build` is what
the targets and the checks run.

**Upgrading the toolchain changes nothing this repo depends on.** Measured against this exact
lockfile, Node 22.23.3 and 24.21.0 (Krypton, the newest LTS — 26 is Current) crossed with
npm 11.19.0 and 12.2.0: a clean `npm install` from a fresh copy, `tsc && vite build`, and,
with Node 24, `make check` and `make build` — all exit 0, and `package-lock.json` comes out
byte-identical (lockfileVersion 3, which npm 9+ all read). Nothing pins the versions: no
`.nvmrc`, no `engines`, no `packageManager`, and the only file naming them is this table.
Two behaviours to expect:

- npm ≥ 11 gates lifecycle scripts behind `allowScripts` and warns about
  `esbuild@0.28.2 (postinstall: node install.js)` on every install ("not yet covered" on 11,
  "blocked" on 12). It is noise, not breakage: the build is green with that script never run,
  because esbuild resolves its prebuilt binary from the platform package the lockfile already
  installs. `npm install-scripts approve esbuild` silences it. (npm 12 permits Node
  `^22.22.2 || ^24.15.0 || >=26`, so it can be installed beside the Node here.)
- the Makefile resolves `npm` from `PATH`, falls back to `~/.local/node/bin/npm`, and never
  resolves `node` at all — so node itself has to be on `PATH` for any of it to run, for the
  shebang reason in Trap 2. Exporting that directory, or installing Node system-wide, is what
  makes a machine work; the Makefile is unchanged for it.

## Checks that must pass

```bash
make check                                  # the next four, in one step
make build                                  # the last one, into build/bin/kichat

gofmt -l .                                  # must print nothing
go vet ./...
go test ./... -count=1
cd frontend && npx tsc --noEmit             # or: npm run build (tsc && vite build)
wails build -tags webkit2_41                # produces build/bin/kichat
```

Both targets add `-tags webkit2_41` and resolve the tools themselves, which is what makes
them work from a shell where neither `node` nor `wails` is on `PATH`. The raw commands are
the same gates, but `npx` and `npm` need Trap 2's `export PATH` first.

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

# DeepSeek: metered, needs a key; skips without KICHAT_DEEPSEEK_KEY
KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/ -run TestLiveDeepSeek -v
#   optional: KICHAT_DEEPSEEK_BASE, KICHAT_DEEPSEEK_MODEL
```

What each live test buys:

- `TestLiveBraveContext` — the real `/res/v1/llm/context` route returns page chunks.
- `TestLiveBraveContextClampsLongQuery` — local clamping beats the API's 422.
- `TestLiveBraveChainUsesContext` — the chain picks the context route, not the fallback.
- `TestLiveBraveBadKeyIsNotMisclassified` — a rejected key is not reported as a throttle.
  It sends no working credential, so it cannot succeed and cannot consume credit; it
  checks both wordings the API uses for one and the same rejection (below).
- `TestLiveSearchFirstTurn` — settings → chain → context → seeded prompt → LM Studio,
  and asserts the context payload is bigger than the headline listing it replaces.
- `TestLiveDeepSeek` — the three claims the provider makes that only the real service
  can confirm: the listing reports a context window (LM Studio's namespace does,
  the plain OpenAI one cannot), `stream_options.include_usage` is honoured so the
  context meter gets a measured `prompt_tokens`, and fragmented `tool_calls` stream
  as expected. DeepSeek reasons by default and spends budget on `reasoning_content`
  first, so a small `max_tokens` returns `finish_reason: "length"` with empty
  `content` — the test asks for 2048/4096 for that reason.

**Never put an API key in a tracked file.** Pass it in the environment; the app itself
stores it through `internal/secret` (`~/.config/kichat/`). Measured facts worth keeping:
a rejected key comes back as **HTTP 422 with `code: "SUBSCRIPTION_TOKEN_INVALID"`** (not
401), and its free-text `detail` varies with the shape of the value sent — a key-shaped
one draws "The provided API key is invalid.", anything else "The provided subscription
token is invalid." So the **code** is what classifies it; matching on the wording misses
half of the failures and reports them as plain server errors (which is what the real
`websearch` failure that prompted this fix looked like). A used-up balance vs a momentary
throttle can likewise only be told apart by the envelope's `code` (`QUOTA_LIMITED` /
`RATE_LIMITED`), because both can arrive as 429.

**DuckDuckGo's HTML endpoints can be refused outright, and the refusal looks like an
empty page.** Measured here: both `html.duckduckgo.com/html/` and
`lite.duckduckgo.com/lite/` answered **HTTP 202** — not an error status — with the
anti-bot interstitial ("Unfortunately, bots use DuckDuckGo too… Select all squares
containing a duck") and no result nodes, for GET and POST, with a Firefox or a Chrome
user agent, with referer and language headers set. Nothing in the request shape changed
it: the check is on the client, so pacing or a HEAD/GET variation is not a workaround.
That is why a block is detected by the interstitial's own markup (`anomaly.js`,
`challenge-form`, `anomaly-modal`) and reported as `ErrBlocked`, and why the lite layout
is not retried after it; "no results (layout may have changed)" is reserved for a page
that really has no hits. A working Brave key, a different provider, or a different
network is the way out.

## Verifying the UI

### Preferred: the mock-bridge harness

The real app is not reachable as a web page — its UI is not served over HTTP, and
`window.go` is injected by the Go runtime into the webview, so opening `frontend/dist`
in a browser shows a bootstrap failure. `tools/uiharness` fixes that by serving the
built frontend with a mocked bridge:

```bash
make frontend                        # or: cd frontend && npm run build (needs node on PATH)
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

### Pinning the streaming renderer

`node tools/streamcheck/check.mjs` (Node 22.18+, for its TypeScript support; needs
`cd frontend && npm install` first) renders every prefix of four sample replies twice —
once the way a finished message is rendered, once the way an arriving one is — and fails
if the visible text differs, if the pieces `frontend/src/lib/stream.ts` cuts would change
as the text grows (the memo depends on their being append-only), or if a line is lost in
the split. It imports the real module directly, runs in under a second, and is **not**
part of `make check`.

The numbers behind that shape, from the parser and from the real UI. With this app's
pipeline, re-parsing the whole reply on every delta and on every throttle commit cost
237 ms of parsing for a 4 kB answer, 1.7 s for a 12 kB one and 6 s for a 24 kB one
(`node` + `react-dom/server` over the real plugins). Driving the mock-bridge harness in
headless Chromium over CDP, with the fixture reply enlarged to ~5 kB so the run is worth
measuring, one streamed reply tore down and rebuilt 4511 elements and rewrote the message
9576 times — that is the stutter and the flicker, while the rest of the window stays still.
Parsing each settled piece once, throttling the live piece and showing an open code fence
as plain text brings the same three replies down to 46, 137 and 266 ms, a 45 kB code-only
answer from 6.7 s of parsing to a single parse, and the harness reply to 256 remounts over
887 rewrites.

## Conventions

- Comments explain **why**, not what; user-facing strings go through
  `frontend/src/lib/i18n.ts` (de and en) — a hardcoded label is a bug.
- No compile suppressions, no unused dependencies, no swallowed errors. If a test needs
  weakening, say so out loud instead of doing it quietly.
- New settings fields need no migration: the settings blob is one JSON row decoded over a
  fully populated default, so absent fields keep their default (pinned by
  `internal/config/config_test.go`). Do not change an existing field's type.
- Tests pin behaviour (what the user or the model sees), not implementation details.
- **A test must never reach the real keyring or the real `~/.config/kichat` data.** The
  keyring is a per-user global resource and `secret.New` probes it, so a test store built
  with `New` wrote its fixtures into the keyring of whoever ran the suite: a stored Brave
  key became the string `brave-key`, the app read that back at startup and every search
  came back as a rejected key (422 `SUBSCRIPTION_TOKEN_INVALID`). Tests use
  `secret.NewIsolated(dir)`, which keeps every secret in the temp dir and never probes.
- `.freebuff/` is gitignored scratch space; `frontend/dist`, `build/bin` and `node_modules`
  are build output.
