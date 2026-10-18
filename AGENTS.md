# Working on KiChat

KiChat is a local-first AI desktop workspace: `main.go` + `app.go` + `api.go` over
Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`. It is a
**Wails v2** application (Go binary + webkit2gtk on Linux, WebView2 on Windows),
talking to LM Studio (`:1234`), Ollama (`:11434`), a llama.cpp server it can install
itself, and hosted providers (Groq, NVIDIA, Mistral, DeepSeek), with Brave/
DuckDuckGo search, picture generation, and SQLite storage.

This file is the **index and the rules**, not the manual. It is loaded into every
session, so it stays short and points at three places instead of repeating them:

| Read | For |
| --- | --- |
| [`CODE_MAP.md`](CODE_MAP.md) | where any package, file or extension point lives, and the request lifecycle |
| [`.claude/skills/`](.claude/skills) | the procedures — one per recurring task, loaded only when used |
| the subsystem documents (bottom of this file) | the measured facts behind each backend or feature |

An agent that does not load Agent Skills should open the `SKILL.md` named in the
table below directly; the files are plain Markdown by design.

## Start here

```sh
make check        # gofmt, vet (linux/windows/darwin), tests, tsc
make build        # build/bin/kichat
make run          # build and launch
```

Two things bite on this machine: **node is not on `PATH`** (it lives in
`~/.local/node/bin`, and npm's shebang needs node, not a path to npm), and **every
Linux build needs `-tags webkit2_41`** because only webkit2gtk-4.1 is installed.
`make` handles both; a raw `wails build` needs the tag and the `PATH` export.

## Where things live

| Path | What |
| --- | --- |
| `main.go` | Wails bootstrap, embedded `frontend/dist`, `--version` |
| `app.go`, `api.go` | the bridge: every method the window can call |
| `internal/chat` | context assembly, the model/tool loop, compaction, pacing |
| `internal/llm` | the provider interface and every chat backend |
| `internal/images` | picture generation: WebUI, `sd-cli`, managed `sd-server`; and enlarging: Lanczos, optional Real-ESRGAN |
| `internal/runtime` | llama.cpp install and supervision, GPU placement, model folders |
| `internal/research`, `internal/tools`, `internal/ingest` | search/scrape, model tools, attachments |
| `internal/store`, `internal/config`, `internal/secret`, `internal/events` | state, settings, keys, event names |
| `frontend/src` | the window; `lib/i18n.ts` holds every user-facing string |

[`CODE_MAP.md`](CODE_MAP.md) has the per-file detail, the storage schema, and a
table mapping "to add X, touch Y" to the right skill.

## Skills

| Skill | Use when |
| --- | --- |
| [`add-llm-backend`](.claude/skills/add-llm-backend/SKILL.md) | adding a chat backend, or changing a listing, stream, refusal or capability |
| [`add-image-backend`](.claude/skills/add-image-backend/SKILL.md) | adding a picture backend, or changing parameters, hires, tiling or progress |
| [`change-settings`](.claude/skills/change-settings/SKILL.md) | adding or changing a setting, default or prompt preset |
| [`build-platforms`](.claude/skills/build-platforms/SKILL.md) | producing an artifact, or changing the Makefile's build/packaging recipes |
| [`cut-release`](.claude/skills/cut-release/SKILL.md) | "cut x.y.z", bumping `VERSION`, tagging |
| [`live-tests`](.claude/skills/live-tests/SKILL.md) | running or adding a test against a real service |
| [`verify-ui`](.claude/skills/verify-ui/SKILL.md) | changing anything the window renders, or a bridge binding |

## Rules that are not negotiable

- **User-facing strings go through [`frontend/src/lib/i18n.ts`](frontend/src/lib/i18n.ts)**
  in all three languages (`de`, `en`, `zh`). A hardcoded label is a bug.
- **A test must never reach the real keyring or `~/.config/kichat`.** The keyring is
  a per-user global resource and `secret.New` probes it: tests use
  `secret.NewIsolated(dir)`, or they write their fixtures into the keyring of
  whoever ran the suite.
- **No compile suppressions, no unused dependencies, no swallowed errors.** If a
  test needs weakening, say so out loud instead of doing it quietly.
- **Tests pin behaviour** — what the user or the model sees — not implementation
  details.
- **Settings need no migration.** The blob is one JSON row decoded over a fully
  populated default, so an absent field keeps its default (pinned by
  `internal/config/config_test.go`). Never change an existing field's type.
- **A vendor fact lives in one place**: the subsystem document (or the provider's
  doc comment, which is the same thing nearer the code). Not here, and not twice.
- **Comments explain why**, and a comment that states a measurement says where it
  came from.
- **The scratch directory at the repo root is gitignored**; `frontend/dist`,
  `build/bin` and `node_modules` are build output.

## Environment (verified, not assumed)

| Tool | Here | Consequence |
| --- | --- | --- |
| Go | 1.26.0 | fine |
| Wails CLI | 2.15.0 | fine |
| Node / npm | 22.23.3 / 10.9.9 in `~/.local/node/bin` | **not on `PATH`**; export the directory, or install Node system-wide |
| webkit2gtk | 4.1 only | **every Linux build needs `-tags webkit2_41`** |

`vite dev` needs Node ≥ 20.19 / ≥ 22.12 (below that, Vite 7 dies with
`crypto.hash is not a function`), and npm 9 cannot install Tailwind's native binding
at all (`libc`-aware optional dependencies arrived in npm 10). The full
history, including what upgrading the toolchain does and does not change, is in
[`README.md`](README.md); the fix is a current Node, not a Makefile change.

## Checks that must pass

```sh
make check                                  # the next six, in one step
make build                                  # the last one, into build/bin/kichat

gofmt -l .                                  # must print nothing
go vet ./...
GOOS=windows go vet ./...                   # the Windows half compiles
GOOS=darwin go vet ./...                    # the macOS half compiles
cd frontend && npx tsc --noEmit             # or: npm run build (tsc && vite build)
go test ./... -count=1
wails build -tags webkit2_41                # the last one, into build/bin/kichat
```

`wails build` regenerates `frontend/wailsjs/go/models.ts`: after changing a Go
struct that crosses the bridge, build (or `wails generate module`) **before**
type-checking the frontend, or `tsc` fails on the missing field.

A nil Go slice marshals to `null`, not `[]`. A binding that returns a slice needs
its empty case rendered in the harness or pinned in a root-level Go test — that is
what `runtime_status_test.go` is for, and how Settings → Backends once died on
`.find` of null.

## Live tests

Offline tests never touch the network. Live ones are gated by environment
variables: `KICHAT_LIVE=1` plus a key, or a real model/server. They skip when the
variable is absent, and a skip is not a pass.

**Never put an API key in a tracked file.** Pass it in the environment; the app
stores keys through `internal/secret`. The full matrix — which variable gates which
test, what each one buys, and what it costs — is in the
[`live-tests`](.claude/skills/live-tests/SKILL.md) skill.

## Platforms

A Windows build cross-builds from Linux or macOS with no mingw; a macOS `.app` can
only be built on a Mac, and Wails exits **0** having built nothing when asked
otherwise — which is why the Makefile checks the host. `make nsis` needs
`makensis` on `PATH` and fails loudly when it is missing, because Wails does not.
The installer is not byte-reproducible; the .deb is. Everything measured about
these targets, including the version resource and the DMG, is in the
[`build-platforms`](.claude/skills/build-platforms/SKILL.md) skill.

## Versioning

The version is authored once, in `VERSION`, and every surface is derived from it:
`make build` links it into `internal/version` (Settings → About, `kichat --version`)
and rewrites `wails.json`'s `info` block, which is where the Windows resource and
the macOS bundle version come from. Three traps:

- **The linker path is the contract.** `-X kichat/internal/version.Version=…` stops
  matching the moment that package or variable is renamed, and every build then
  silently reports `dev`.
- **A pre-release needs Debian's `~`**: `0.2.0~beta1`, not `0.2.0-beta.1`, or `apt
  upgrade` treats the pre-release as newer than the final.
- **The Windows version resource is rendered, not linked**, and `wails build` has
  no flag to override it — `make version-info` is what keeps it in step.
- **The build date is the commit's date, never the build clock.** A wall-clock
  timestamp would break the byte-identical rebuilds `-trimpath` otherwise
  guarantees.

The release procedure itself (the commit, the tag, the push rules, the repair of a
missing tag) is the [`cut-release`](.claude/skills/cut-release/SKILL.md) skill.

## Verifying the UI

Prefer the mock-bridge harness over driving the real window:

```sh
make frontend && go run ./tools/uiharness    # http://127.0.0.1:8791/
```

It renders the built frontend with a mocked Go bridge, has fixtures for every
markdown feature, and query flags for the states that are otherwise unreachable
(`?throttle=1`, `?pace=1`, `?spread=1`, `?ceiling=8000`, `?images=sdserver`, …). It
proves rendering, state flow and event wiring — not backend behaviour. An
`unmodelled binding called: X` in the console means the mock needs X. The flag
table, the native-window traps (`xdotool`, `import`) and the streaming-renderer
check are in the [`verify-ui`](.claude/skills/verify-ui/SKILL.md) skill.

## Git commits

The subject is `area: description` — lowercase, imperative, and it describes the
behaviour change, not the files. Areas in use: `chat`, `llm`, `ui`, `research`,
`docs`, `store`, `build`, `config`, `api`, `settings`, `secret`, `ingest`, `tools`,
`icon`; a new area is fine when the change belongs nowhere else. A second effect
joins with "and":

```
chat: make compaction work with a reasoning model
research: report a refused search, and keep tests out of the keyring
icon: draw the mark once, derive every surface from it, and make the taskbar one show
```

The body is prose, not a changelog: the failure or limitation that motivated the
change, what was measured, what changed and why, and which checks stayed green.
Numbers go in where they exist; a body that only lists the files is a stub. One
commit is one coherent change, and files are staged explicitly, never `git add -A`.

**Always `--no-pager`.** `git log`, `git diff` and `git show` open a pager that
blocks a non-interactive shell — it looks hung, and the output lands in a temp file
instead of the result.

## Documentation

| Document | What it holds |
| --- | --- |
| [`PLAN.md`](PLAN.md) | the architecture, and the reasoning behind it |
| [`COMPACT.md`](COMPACT.md) | context compaction: strategy, arithmetic, failure modes |
| [`RESEARCH.md`](RESEARCH.md) | search and scraping: providers, refusals, guards, budgets |
| [`IMAGES.md`](IMAGES.md) | picture generation: three server kinds, parameters, profiles, verification |
| [`UPSCALING.md`](UPSCALING.md) | enlarging a picture: the resampler, Real-ESRGAN, targets, the Enlarge panel |
| [`LLAMACPP.md`](LLAMACPP.md) | the llama.cpp provider and managed runtime, projectors, ternary models |
| [`GROQ.md`](GROQ.md) | the Groq backend: listing, ceiling, pacing, refusals |
| [`NVIDIA.md`](NVIDIA.md) | the build.nvidia.com backend and its availability check |
| [`MISTRAL.md`](MISTRAL.md) | the Mistral backend, alias collapse, typed chunks |
| [`DEEPSEEK.md`](DEEPSEEK.md) | the DeepSeek backend, reasoning content, refusals |
| [`CODE_MAP.md`](CODE_MAP.md) | where the code is, and the extension points |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | pull requests, the CLA, commit style |

Each one distinguishes what was observed from what is expected. When you measure
something new about a backend, put it where a reader will look for it and leave
this page a pointer.

## Keeping this file useful

- If a section here grows past a screen, it is a procedure: move it to
  `.claude/skills/` and leave a row in the table.
- If it is a fact about a vendor or a measurement, it belongs in the subsystem
  document or the provider's doc comment.
- If a skill's `description` stops matching the task it covers, fix the description
  first — that is what decides when the skill is loaded.
