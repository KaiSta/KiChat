# Working on KiChat

KiChat is a local-first AI desktop workspace: `main.go` + `app.go` + `api*.go` over
Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`. It is a
**Wails v2** application (Go binary + webkit2gtk on Linux, WebView2 on Windows),
talking to LM Studio (`:1234`), Ollama (`:11434`), a llama.cpp server it can install
itself, and hosted providers (Groq, NVIDIA, Mistral, DeepSeek), with Brave/
DuckDuckGo search, picture generation, and SQLite storage.

This file is the **index and the rules**, not the manual. It is loaded into every
session, so it stays short and points at three places instead of repeating them:
[`CODE_MAP.md`](documentation/CODE_MAP.md) for where anything lives (including the
storage schema and the request lifecycle), [`.claude/skills/`](.claude/skills) for the
procedures — one per recurring task, loaded only when used — and the subsystem
documents at the bottom for the measured facts behind each backend. An agent that does
not load Agent Skills should open the `SKILL.md` named below directly; the files are
plain Markdown by design.

## Start here

```sh
make check        # gofmt, vet (linux, windows, darwin, both arm64 halves), tests, tsc
make build        # build/bin/kichat
make run          # build and launch
```

**Every Linux build needs `-tags webkit2_41`**, because only webkit2gtk-4.1 is
installed. `make` adds it; a raw `wails build` does not. What this machine actually
has, and why each version is or is not a problem, is in
[`build-platforms`](.claude/skills/build-platforms/SKILL.md).

## Where things live

Go logic is one package per subsystem under `internal/`; the window is
`frontend/src`, where `lib/i18n.ts` holds every user-facing string. The places
opened most:

| Path | What |
| --- | --- |
| `app.go`, `api*.go` | the bridge: every method the window can call, split by domain (`api.go` holds settings, keys and the shared types) |
| `internal/chat` | context assembly, the model/tool loop, compaction, pacing |
| `internal/llm` | the provider interface and every chat backend |
| `internal/tools`, `internal/workspace` | what the model can call, and the folder boundary every file tool resolves against |
| `internal/images`, `internal/runtime` | picture generation and enlarging; llama.cpp install and supervision |
| `internal/store`, `internal/config`, `internal/secret` | state (encrypted at rest), settings, API keys |

[`CODE_MAP.md`](documentation/CODE_MAP.md) has the rest — every bridge file, every
package with its entry points and tests, the frontend components, and the table
mapping "to add X, touch Y" to the right skill.

## Skills

| Skill | Use when |
| --- | --- |
| [`add-llm-backend`](.claude/skills/add-llm-backend/SKILL.md) | a chat backend, or a listing, stream, refusal or capability |
| [`add-image-backend`](.claude/skills/add-image-backend/SKILL.md) | a picture backend, or parameters, hires, tiling, progress |
| [`change-settings`](.claude/skills/change-settings/SKILL.md) | a setting, a default or a prompt preset |
| [`build-platforms`](.claude/skills/build-platforms/SKILL.md) | an artifact, the Makefile, or a build reporting the wrong version |
| [`commit`](.claude/skills/commit/SKILL.md) | committing: subject, body, staging, the release commit |
| [`cut-release`](.claude/skills/cut-release/SKILL.md) | "cut x.y.z", bumping `VERSION`, tagging |
| [`live-tests`](.claude/skills/live-tests/SKILL.md) | a test against a real service |
| [`verify-ui`](.claude/skills/verify-ui/SKILL.md) | anything the window renders, or a bridge binding |

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

## Checks that must pass

`make check` is gofmt, `go vet` on linux/windows/darwin plus `make vet-arm64`, the
Go tests and `tsc`; `make build` writes `build/bin/kichat`. Both must exit 0.

CI runs those targets rather than a second copy of the commands, so a change to the
build belongs in the Makefile, and no job uploads an artifact: **a pushed tag is
what ships**. Which job builds what is in
[`build-platforms`](.claude/skills/build-platforms/SKILL.md).

Two traps a green run can hide:

- **`frontend/wailsjs/go/models.ts` is generated.** After changing a Go struct that
  crosses the bridge, build (or `wails generate module`) **before** type-checking
  the frontend, or `tsc` fails on a field that is not there yet.
- **A nil Go slice marshals to `null`, not `[]`.** A binding that returns a slice
  needs its empty case rendered in the harness or pinned in a root-level Go test —
  `runtime_status_test.go` is the example, and Settings → Backends once died on
  `.find` of null.

## Versioning

The number is authored once, in `VERSION`; `make build` links it into
`internal/version` (Settings → About, `kichat --version`) and rewrites `wails.json`'s
`info` block, which is where the Windows resource and the macOS bundle version come
from. The traps that make a build report the wrong number are in
[`build-platforms`](.claude/skills/build-platforms/SKILL.md); the procedure — bump,
commit, tag, push — is [`cut-release`](.claude/skills/cut-release/SKILL.md).

The release commit is written for users: a short bullet list in plain language of what
the new version brings, with no attribution lines, because its body is published as the
release page.

## Commits

The subject is `area: description` — lowercase, imperative, the behaviour change and
not the files — and subject and body are both short, plain language. No attribution
lines: no `Co-Authored-By`, no "generated with" trailer. The areas, the body's shape,
one change per commit and the `build: release x.y.z` exception are in
[`commit`](.claude/skills/commit/SKILL.md); `git` here is always `--no-pager`.

## Live tests

Offline tests never touch the network. Live ones are gated by `KICHAT_LIVE=1` plus a
key or a real model, skip when the variable is absent, and **a skip is not a pass**.
Never put an API key in a tracked file: pass it in the environment, and the app stores
keys through `internal/secret`. The matrix — which variable gates which test, what
each one buys, what it costs — is
[`live-tests`](.claude/skills/live-tests/SKILL.md).

## The window

```sh
make frontend && go run ./tools/uiharness    # http://127.0.0.1:8791/
```

The mock-bridge harness renders the built frontend with query flags for the states
that are otherwise unreachable. It proves rendering, state flow and event wiring —
not backend behaviour — and an `unmodelled binding called: X` in the console means
the mock needs X. The flag table, the native-window traps and the streaming-renderer
check are [`verify-ui`](.claude/skills/verify-ui/SKILL.md).

## Documentation

| Document | What it holds |
| --- | --- |
| [`PLAN.md`](documentation/PLAN.md) | the architecture, the reasoning behind it, and the build manual (§6: prerequisites, artifacts, traps) |
| [`COMPACT.md`](documentation/COMPACT.md) | compaction: strategy, arithmetic, failure modes |
| [`CODING.md`](documentation/CODING.md) | coding mode: folder boundary, confirmation gate, the terminal's shell, caps and timeout |
| [`RESEARCH.md`](documentation/RESEARCH.md) | search and scraping: providers, refusals, guards, budgets |
| [`IMAGES.md`](documentation/IMAGES.md) | picture generation: three server kinds, parameters, profiles |
| [`IMAGES-CLOUD.md`](documentation/IMAGES-CLOUD.md) | hosted picture backends: the free tiers and their cost |
| [`UPSCALING.md`](documentation/UPSCALING.md) | enlarging: the resampler, Real-ESRGAN, the Enlarge panel |
| [`LLAMACPP.md`](documentation/LLAMACPP.md) | the llama.cpp provider and runtime, the CUDA/ROCm prerequisites, projectors, ternary models, multi-token prediction |
| [`MLX.md`](documentation/MLX.md) | the MLX provider and the mlx-lm environment KiChat installs on Apple silicon |
| [`GROQ.md`](documentation/GROQ.md), [`NVIDIA.md`](documentation/NVIDIA.md), [`MISTRAL.md`](documentation/MISTRAL.md), [`DEEPSEEK.md`](documentation/DEEPSEEK.md) | a hosted backend each: listing, window, ceiling, pacing, refusals |
| [`ENCRYPTION.md`](documentation/ENCRYPTION.md) | the encrypted database: page encryption, the key and its recovery copy |
| [`CODE_MAP.md`](documentation/CODE_MAP.md) | where the code is, and the extension points |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | pull requests, the CLA, commit style |

Each one distinguishes what was observed from what is expected. When you measure
something new about a backend, put it where a reader will look for it and leave this
page a pointer.

## Keeping this file useful

- A section that grows past a screen is a procedure: move it to `.claude/skills/` and
  leave a row in the table.
- A fact about a vendor, or a measurement, belongs in the subsystem document or the
  provider's doc comment.
- If a skill's `description` stops matching the task it covers, fix the description
  first — that is what decides when the skill is loaded.
