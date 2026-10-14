# Working on KiChat

KiChat is a local-first desktop LLM research chat: `main.go` + `app.go` + `api.go` on
top of Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`.
It is a **Wails v2** application (Go binary + webkit2gtk on Linux, WebView2 on Windows),
talking to LM Studio (`:1234`) and Ollama (`:11434`), with Brave/DuckDuckGo web search and
SQLite storage.

This page is the index. It carries what it takes to build, check and change the repo, and
points at one document per subsystem for the measured detail behind it:

| document | what it holds |
| --- | --- |
| `PLAN.md` | the architecture, and the reasoning behind it |
| `COMPACT.md` | context compaction: strategy, arithmetic, failure modes, where the code is |
| `MISTRAL.md` | the Mistral backend: the measured listing and stream shapes, the alias collapse, the typed content chunks, the bugs review found |
| `DEEPSEEK.md` | the DeepSeek backend: the listing that reports a window, `reasoning_content`, the `/v1` alias, refusals |
| `NVIDIA.md` | the build.nvidia.com backend: the four-field listing, the catalogue that lists models the account may not call (why the provider filters nothing, and the one-token availability check that filters on evidence instead), the reasoning field that arrives twice, refusals |
| `LLAMACPP.md` | llama.cpp on both sides: the provider's `/props`-driven metadata (and why a missing claim is not `false`), and the managed runtime that installs and runs `llama-server` — two release sources including PrismML's fork, the update check, GPU offload, the ternary models |
| `RESEARCH.md` | web search and scraping: the Brave and DuckDuckGo providers, the metered LLM-context route that search-first uses, what classifies a refusal, the scraper's SSRF and robots guards, the token budgets, and the search-first seeded turn |
| `IMAGES.md` | image generation: three kinds of backend — the one `/sdapi/v1/txt2img` route that covers AUTOMATIC1111, Forge and stable-diffusion.cpp's `sd-server`, the local `sd-cli` run once per picture through the binary's own flags, and the *same* build run as a `sd-server` KiChat starts and stops itself (the port chosen free because 1234 is LM Studio's, `/sdapi/v1/sd-models` as the readiness route because sd-server serves no `/health`, start-with-app off by default, and a *reading* that asks for a restart instead of restarting on save) (the published per-accelerator archives, and the folder KiChat finds the program in, with several builds in one folder reported rather than picked) — how a generation parameter is decided (§3): the model chooses it per call through its `generate_image` arguments — including the guidance scale, the one that used to live in the extra command-line arguments — an empty field then falls through to the server's or the program's own default, and a value typed in Settings is *fixed* and overrides what the model asked for — the named profiles that save that configuration and switch a model in one pick (§3), the single place every request's size and step count are bounded, the `generate_image` tool, and how a picture becomes an ordinary image attachment — which is what lets a vision model see a *user's* attachment on the next turn, while the same thing for a picture KiChat drew is unverified (§9) |

**A vendor fact belongs in one of those, not here.** If you measure something new about a
backend, put it where a reader will look for it and leave this page a pointer — a fact that
lives in two places is a fact that will disagree with itself.

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

## Windows

A Windows build is supported, and almost all of it is the same code: neither KiChat nor
Wails v2 needs cgo, so the only platform-specific things are the webview (WebView2 there,
webkit2gtk here), the executable's name and version resource, and how a child process is
stopped. What a Windows machine needs:

- **bash and make** — Git for Windows or MSYS2. Every recipe here is POSIX sh, and the
  Makefile keeps `SHELL := /bin/bash` for that reason; from `cmd.exe` or PowerShell it is
  not the right entry point.
- **node on `PATH`** — the same requirement as Trap 2, without the trap: the Windows Node
  installer is on `PATH` already.
- **the WebView2 runtime** — present on Windows 11 and on 10 from 21H2 (Edge installs it),
  and the thing the app needs to open a window at all. The `-webview2` flag of the Wails CLI
  only decides what the *NSIS installer* does about a machine that lacks it.
- **`makensis` (NSIS) on `PATH`** — only for `make nsis`. It is in no platform's default
  toolchain and Wails neither ships nor fetches it.

**What is not measured here, and what is.** There is no Windows machine behind this section:
the three requirements above are the vendors' documented ones plus the direct consequence of
this Makefile being POSIX sh. Everything below *was* measured — on Linux — and it is all the
build, the executable and the compile gate; no runtime behaviour of Windows is claimed
anywhere in this repo, because none was observed. A report that a KiChat-built picture or
model misbehaves on Windows would be the first evidence of that kind.

`make build` builds for the machine it runs on: the webkit2gtk tag and the Debian hardening
flags are chosen by the target platform, not assumed (measured: `make -n windows` shows
`-platform windows/amd64` with neither `-tags webkit2_41` nor `-ldflags … -z,now`).

**`make windows` cross-builds from Linux or macOS, and needs no mingw.** Wails writes the
executable's version resource, its manifest and its icon in pure Go and this app has no cgo,
so `wails build -platform windows/amd64` links here: measured, it produces a 22 MB PE32+
**GUI** executable (subsystem 2 — no console window) into `build/bin/kichat.exe`. Ordering
matters and the target does it: the frontend is built first, because the Go build embeds
`frontend/dist`. The platform is `windows/amd64` whatever the host's architecture is, so the
artifact does not depend on whose laptop made it; `make windows TARGETARCH=arm64` overrides
it. WebView2 is the one place the two webviews meet in the build: the link reports it is
using Wails's Go WebView2Loader, which is the loader the exe carries — nothing here needs
`-tags native_webview2loader`.

**`make nsis` wraps that in a Setup.exe**, and its one new requirement is `makensis` on
`PATH`. Wails is not merely unable to find NSIS by itself: with no `makensis` it prints
`Cannot create installer: makensis not found` and exits **0**, so a release pipeline keyed
on the exit status ships a bare exe and never notices. The target checks for the tool up
front and fails with the package to install on each platform, before the 30 s cross-build
rather than after it. Measured here (makensis 3.09, taken out of unpacked `nsis` and
`nsis-common` debs with `NSISDIR` pointed at them — no root needed, the same trick works on
any Debian-ish host without the package): the run writes an **11.2 MB**
`build/bin/kichat-amd64-installer.exe` whose payload `makensis -V4` reports as
`File: "kichat.exe"->"kichat.exe" [compress] 9250948/22315008 bytes` plus
`File: "MicrosoftEdgeWebview2Setup.exe" [compress] 1694774/1793816 bytes`, and whose own
Properties read `ProductVersion 0.4.0`. That version is not a fourth place to type it in:
the script renders it from the same `wails.json` info block the exe's resource comes from,
which is also why `version-info` has to keep `productVersion` numeric — the script's
`VIProductVersion "${INFO_PRODUCTVERSION}.0"` will not compile a pre-release string. Two
knobs describe what Windows does rather than cosmetics: `WEBVIEW2=download|embed|browser|error`
is what the *app* does on a machine with no runtime (Wails passes the word through as a
build tag, so it is the app that acts; the installer always embeds the bootstrapper and runs
it silently), and `INSTALL_SCOPE=machine|user` is Program Files with elevation or
`%LOCALAPPDATA%\Programs` without it.

**The installer is not byte-reproducible, and the reason is worth knowing.** NSIS records
each embedded file's timestamp, and `wails build -nsis` rewrites `build/bin/kichat.exe` (and
the bootstrapper) immediately before it runs `makensis` — so the installer's bytes follow the
build clock even though the executable they carry does not. Measured: with the exe's bytes
*and* mtime held fixed, two `makensis` runs produce the same installer (`38a2e412…` both
times); touching only the mtime, bytes still hashing `e7bcfab7…`, changes it (`878cc8b9…`);
pinning the mtime makes it stable again (`9211497e…` twice). So every `make nsis` yields a
different installer for one commit, and the byte-identical rebuild this repo promises stays
where it was — the .deb's. Making the installer reproducible would mean calling `makensis`
ourselves after the build, which means maintaining the script rendering the `-nsis` path
does for us.

**`build/windows/installer/wails_tools.nsh` is generated, not source.** Wails rewrites it on
every `-nsis` run from the template inside the tool, resolving `{{.Info.*}}` against
`wails.json` in place — the tracked form was a template `makensis` could not use, and every
run left it dirty. It is gitignored now (verified: delete it, run `make nsis`, and it comes
back). `project.nsi` beside it *is* source — defines put there win over the generated ones —
and the bootstrapper Wails stages into `build/windows/installer/tmp/` is gitignored too.

`make check` includes a Windows compile of everything (`GOOS=windows go vet ./...`, 0.86 s),
which is where a `//go:build windows` file and every Windows branch of a test are compiled —
so that part of the port cannot rot on a machine with no Windows.

`make install`, `make uninstall`, `make deb` and `make lint-deb` are Debian's and say so
instead of failing part-way through an `install -D` Windows does not have.

## Checks that must pass

```bash
make check                                  # the next five, in one step
make build                                  # the last one, into build/bin/kichat

gofmt -l .                                  # must print nothing
go vet ./...
GOOS=windows go vet ./...                   # the Windows half compiles
cd frontend && npx tsc --noEmit             # or: npm run build (tsc && vite build)
wails build -tags webkit2_41                # produces build/bin/kichat
go test ./... -count=1
```

Both targets add the Linux build tag and resolve the tools themselves, which is what makes
them work from a shell where neither `node` nor `wails` is on `PATH`. The raw commands are
the same gates, but `npx` and `npm` need Trap 2's `export PATH` first, and the raw `wails
build` needs `-tags webkit2_41` on a machine whose only webkit2gtk is 4.1 (Trap 1).

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

# Mistral: metered, needs a key; skips without KICHAT_MISTRAL_KEY
KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistral -v
#   optional: KICHAT_MISTRAL_BASE, KICHAT_MISTRAL_MODEL,
#   KICHAT_MISTRAL_REASONING_MODEL (the thinking-chunk shape needs one the
#   account may call; a tier refusal is reported, not failed)
#   the unanchored pattern also picks up the one-request temperature probe,
#   TestLiveMistralTemperatureAboveThePublishedMaximum

# NVIDIA: metered, needs a key; skips without KICHAT_NVIDIA_KEY
KICHAT_LIVE=1 KICHAT_NVIDIA_KEY=<key> go test ./internal/llm/ -run TestLiveNvidia -v
#   optional: KICHAT_NVIDIA_BASE, KICHAT_NVIDIA_MODEL. The catalogue lists
#   models most keys cannot call, so this drives one model rather than walking
#   the listing; TestLiveNvidia is the one that asserts a window of 0.

# PrismML's fork: one release lookup, a 17 MB download and a real model load
KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v
#   optional: KICHAT_PRISM_MODEL=<path.gguf>, otherwise the smallest model in
#   the LM Studio folders; skips if there is none. Loads a real model: minutes.

# Stable Diffusion WebUI: draws one 64x64 picture at one step. Local and
# unauthenticated, so there is no key — but it needs a WebUI actually running,
# and it skips when there is none (the normal state).
KICHAT_LIVE=1 go test ./internal/images/ -run TestLiveSDWebUI -v
#   optional: KICHAT_SD_URL (default 127.0.0.1:7860), KICHAT_SD_AUTH=user:pass
#   for a WebUI started with --api-auth

# A real sd-server: starts the program out of a stable-diffusion.cpp folder —
# gigabytes and minutes — so the folder is required and there is no default.
# Settles "is sd-server in the archive" and prints the load-once arithmetic.
KICHAT_LIVE=1 KICHAT_SD_FOLDER=~/Downloads/sd go test ./internal/images/ -run TestLiveSDServer -v
#   optional: KICHAT_SD_DIFFUSION_MODEL, KICHAT_SD_MODEL, KICHAT_SD_VAE, KICHAT_SD_LLM,
#   KICHAT_SD_CLIP_L, KICHAT_SD_CLIP_G, KICHAT_SD_T5XXL — the model set to load
```

What each one buys — every subsystem is documented in full in its own file, so this is the
short version:

- `TestLiveBraveContext`, `TestLiveBraveContextClampsLongQuery`,
  `TestLiveBraveChainUsesContext`, `TestLiveBraveBadKeyIsNotMisclassified` and
  `TestLiveSearchFirstTurn` — RESEARCH.md §9: the real page-chunk route, local clamping
  beating the API's 422, the chain picking that route rather than the fallback, a rejected
  key classified as a key rather than a throttle (it sends no working credential, so it
  cannot succeed and cannot consume credit), and the whole path through to LM Studio with
  the context payload asserted larger than the listing it replaces.
- `TestLiveDeepSeek` — DEEPSEEK.md §8: the listing reports a real context window,
  `stream_options.include_usage` is honoured so the context meter gets a measured
  `prompt_tokens`, and a fragmented `tool_calls` stream reassembles.
- `TestLiveMistral` — MISTRAL.md §3 and §9: the listing is not a chat catalogue (46
  published, 27 chat-capable, 8 models after the alias collapse), the window and the
  per-model capabilities come from the listing, `include_usage` is honoured, and a
  fragmented `tool_calls` stream reassembles.
- `TestLiveNvidia` — NVIDIA.md §3 and §7: the catalogue is 80 four-field entries, the
  chosen model is offered `loaded` with tools and a window of **0** (no route of this API
  publishes one, so a value there would be invented), `include_usage` is honoured, and a real
  tool call streams.
- `TestLiveMistralTemperatureAboveThePublishedMaximum` — MISTRAL.md §5 and §10.4: one
  request at `temperature: 1.8`, above the schema's published `[0, 1.5]` and inside what
  Settings offers, reporting whether the service enforces the bound. It asserts nothing:
  either answer is a fact, and one of them means the UI maximum has to come down.
- `TestLivePrismForkRunsAModel` — LLAMACPP.md §6: the fork as a runtime, not a table of
  names — its release list, its tag scheme, its archive, its ternary kernels, and a real
  GGUF loaded on its `llama-server`.
- `TestLiveSDWebUI` — IMAGES.md §8: the WebUI route answers, the sampler this client
  sends is one the server recognises (it comes back in `info`), the guidance scale is echoed
  there too — the only backend that can show one *honoured* rather than merely sent — and the
  picture decodes to real PNG bytes: a server answering with a URL, or with a `data:` prefix,
  produces bytes that are not a picture, and only a real round trip tells the two apart.
- `TestLiveSDServer` — IMAGES.md §8: the folder really holds `sd-server` beside `sd-cli`,
  the build's own `-h` lists the listener flag the app refuses it without, a loaded server
  answers the readiness route the start hangs on, the built-in `cfg_scale` key is parsed
  rather than refused, and the time to ready plus two draws are
  printed — the numbers behind "the load is paid once", which are *reported* rather than
  asserted because the ratio depends on the model and the accelerator. It starts a process
  that loads gigabytes, so `KICHAT_SD_FOLDER` is required and there is no default.

**Never put an API key in a tracked file.** Pass it in the environment; the app itself
stores it through `internal/secret` (`~/.config/kichat/`). What the two search providers
measured — why a rejected Brave key has to be classified by the envelope's `code` rather
than the status, and why a DuckDuckGo refusal is not an empty result — is RESEARCH.md §5.

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
  every markdown feature (headings, table, blockquote, inline and display math) — and a
  highlighted code block *next to an unlabelled one*, because the two get different classes
  and so a different colour: the unlabelled fence is the shape a shell command is usually
  written in, and the light theme's syntax palette must not reach it. A `markdown` fence
  follows, since that is the only shape in which a `hljs-section` heading token appears —
  the one dark-theme value KiChat overrides. A second chat has a seeded search message and a
  compaction, and
  streaming replies driven by real `chat:*` events over a simulated 40 ms cadence. The
  mock is `go:embed`ed, so a running harness picks up an edit to it only after a restart
  — use `go run`, or rebuild before restarting.
- Unmodelled bindings resolve **and log a warning** — if the console shows
  `unmodelled binding called: X`, the mock needs X when you exercise that path.
- `?crash=render` injects an unrenderable message to prove the error boundary still
  catches a render fault instead of leaving a blank window.
- The **runtime and Backends fixtures** — the update row, the source picker, the placement
  reading and the cleanup list — are query flags with their own table in LLAMACPP.md §6.
  Every one of them installs a launcher first, so the rows that depend on one are there.
- The **picture-server fixtures** — `?images=sdserver` with `?server=running|starting|failed|restart`
  and `?startwithapp=1` — are query flags with their own rows in IMAGES.md §8. The mock models
  `StartImageServer`/`StopImageServer` as the *transitions* they are, so a start really passes
  through `starting` before it settles, and the restart reading is computed from the stored
  command line rather than stored, which is what makes a save under a running server raise the
  badge.
- The **Backends fixture holds a key for an entry that is switched off**: the shipped
  Mistral row, which is how a user who pasted a key into it and pressed "Aktualisieren"
  ends up stored-but-never-asked. That state has its own row, its own badge and its own
  one-click repair, so the fixture keeps it reachable rather than only the happy path.
- It is a mock: it proves rendering, state flow and event wiring, **not** backend
  behaviour. Backend truth lives in the Go tests and the live tests above. What it
  cannot see is the *shape of an idle answer*: its fixture always has a managed
  llama.cpp model configured, so `GetRuntimeStatus` was only ever rendered with a
  populated `models` array — while a real fresh install sent `null` (a nil Go slice
  marshals to `null`, not `[]`) and Settings → Backends died on `.find` of null. When
  adding a binding that returns a slice, render the empty case in the harness too, or
  pin the JSON in a Go test the way `runtime_status_test.go` does.

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

## Versioning

The version is authored once, in `VERSION` at the repo root, and every surface is
derived from it: `make build` links it into `internal/version` (read by Settings →
General → About and by `kichat --version`), and `make deb` uses it for the package
name, `control` and `changelog`. Bumping a release is one line in `VERSION`;
`VERSION=x.y.z` on the make command line overrides it for a one-off.

The commit and date linked in come from `git`, and the date is the *commit's* date,
never the build clock — a wall-clock timestamp would break the byte-identical
rebuilds `-trimpath` otherwise guarantees. Three traps:

- **The linker path is the contract.** `-X kichat/internal/version.Version=…` in the
  Makefile stops matching the moment that package or a variable is renamed, and every
  build then silently reports `dev`. Keep the two in step.
- **A pre-release needs Debian's `~`.** Write `VERSION` as `0.2.0~beta1`, not
  `0.2.0-beta.1`: `~` sorts *before* the release (`0.2.0~beta1 < 0.2.0`), while the
  dash form sorts *after* it, so `apt upgrade` would happily move a pre-release onto
  the older-looking final.
- **The Windows version resource is rendered, not linked, and the Makefile is what keeps it
  in step.** A file's Properties on Windows come from the version resource Wails generates
  out of `build/windows/info.json`, whose values are the `info` block of `wails.json` — and
  there is **no flag to override it** (checked: `wails build --help` has none). So the
  release version is a second place to type it in, and a forgotten one is not a cosmetic
  bug: the first cross-built `kichat.exe` measured here carried `ProductVersion 1.0.0`,
  `LegalCopyright "Copyright........."` and a manifest `assemblyIdentity version="1.0.0.0"`
  — the defaults — while `kichat --version` said `0.4.0`. `make version-info` (a prerequisite
  of `build` and of `windows`) rewrites the two derived fields from `VERSION`: the numeric
  part goes to `productVersion`, because a Windows version field is numbers with at most four
  parts and `0.2.0~beta1` has no numeric spelling of the marker, and the full string goes to
  `comments`, which is free text. Measured after the step: `ProductVersion 0.4.0`,
  `CompanyName Kai Stadtmueller`, `LegalCopyright Copyright 2026 Kai Stadtmueller`, manifest
  `version="0.4.0.0"`. Bumping a release therefore still starts with one line in `VERSION`,
  and the build reports the sync it performed — commit the synced `wails.json` with it.

## Git commits

The subject is `area: description` — lowercase, imperative, and it describes
the behaviour change, not the files. The areas in use so far: `chat`, `llm`,
`ui`, `research`, `docs`, `store`, `build`, `config`, `api`, `settings`,
`secret`, `ingest`, `tools`, `icon` — a new area is fine when the change
belongs nowhere else. A second effect joins with "and":

```
chat: make compaction work with a reasoning model
research: report a refused search, and keep tests out of the keyring
icon: draw the mark once, derive every surface from it, and make the taskbar one show
```

The body is prose, not a changelog: the failure or limitation that motivated
the change, what was measured, what changed and why, and which checks stayed
green. Numbers go in where they exist (237 ms of parsing → 266 ms, or the
`code` out of an error envelope); a body that only lists the files is a stub. One commit is one coherent change, and the files are staged explicitly,
not with `git add -A`.

**Always `--no-pager`.** `git log`, `git diff` and `git show` open a pager,
and in a non-interactive shell the pager blocks the command until someone
presses a key: it looks hung, and its output lands in a temp file instead of
the result. `git --no-pager log -1` and friends are the same commands without
the hang.

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
