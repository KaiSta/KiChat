# Code map

Where a thing lives, so nobody has to grep the tree to find it. Read this before
adding a package, a bridge method or a provider; update it when a file moves or
changes owner.

Two rules keep this useful:

- **A package owns one thing.** If a change needs a new responsibility, it
  probably belongs in a new package, not in the nearest one.
- **The map points, it does not explain.** *Why* a thing is the way it is lives
  in the system document for that subsystem (see the table at the bottom of
  [`AGENTS.md`](../AGENTS.md)) or in the package's doc comment.

## The shape of the app

    frontend/src (React 19)          ← the window
        │  window.go.* (Wails binds every exported App method)
    api*.go / app.go (package main)  ← the bridge: settings, chats, images, files
        │
    internal/*                       ← everything that can be tested without a window
        │
    SQLite (~/.config/kichat) + the backends you configured

`main.go` is only the Wails bootstrap: window options, the embedded
`frontend/dist`, and `--version`.

## Top level

| Path | What it is |
| --- | --- |
| `main.go` | Wails bootstrap, window/menu options, `go:embed all:frontend/dist`, `--version` |
| `app.go` | The `App` struct and the chat lifecycle: send, regenerate, stop, `ClearChat` (the `/clear` command), chats CRUD, `GetBudget`, and `OpenProjectTerminal` (the hand-off's *open terminal*, which starts the user's emulator through `internal/runtime`) |
| `api.go` | The bridge's own vocabulary: settings, backends and keys, app info, search status, `OpenExternal` — and the struct types the other bridge files return |
| `api_database.go` | The encrypted database: `GetDatabaseStatus`, the lock state and key resolution at launch, `UnlockDatabase`, `StartFreshDatabase`, and the passphrase/keyring actions Settings → Data offers |
| `api_runtime.go` | llama.cpp as the app installs and supervises it: builds, GPU families, model folders, start/stop, the image server's process |
| `api_models.go` | Listing models and probing them, overrides, prompt variables, the model-file pickers |
| `api_chat.go` | Compaction, attachments, chats' documents and chunk search, web search and scrape |
| `api_changes.go` | The app's half of a project tool's question: `confirmChange`, the `chat:approval` payload, `ResolveToolApproval` / `ResolveToolApprovalWith` (a command's line can be edited before it runs), the standing auto-approval check and the per-chat "always" |
| `api_project_files.go` | What a project chat changed: `ProjectFileChanges` (one row per file, with a clamped diff and whether undo is safe) and `RevertFileChange` (the undo, refused for a file edited since or a running turn) |
| `api_images.go` | Picture generation: status, redraw, progress, the one-draw slot, cache, saving and reading an attachment's image |
| `api_upscale.go` | The enlarging bridge: `UpscaleAttachment`, the upscaler status/test/dialog/install (`InstallUpscaler`, `GetUpscalerInstall`), the upscale bound |
| `internal/` | All logic, one package per subsystem (below) |
| `frontend/` | React 19 + TypeScript; `frontend/dist` is embedded by the Go build |
| `tools/` | `uiharness` (mock-bridge browser UI), `streamcheck` (renderer pin), `notices` (third-party notices) |
| `packaging/` | Debian control, copyright, man page, `.desktop`, lintian overrides |
| `build/` | Icon sources, generated Windows/macOS bundle files, build output |
| `Makefile` | Every artifact: `build`, `windows`, `nsis`, `darwin`, `dmg`, `deb`, `install`, `check` |
| `VERSION`, `wails.json` | The one author of the version, and the Windows/macOS resource it feeds |

## Backend packages

| Package | Owns | Start at | Tests |
| --- | --- | --- | --- |
| `internal/chat` | Context assembly, the model/tool loop, compaction, throttling and pacing, search-first turns | `engine.go` (`Engine.Send`), `context.go` (`Assemble`), `compact.go` (`PlanCompaction`) | package tests + root engine tests |
| `internal/llm` | The provider abstraction and every backend that speaks it | `types.go` (`Provider`, `ChatRequest`, `StreamEvent`), `registry.go` (`DefaultBackendConfigs`, `NewRegistry`), `openai.go` (shared compatible path) | one `<vendor>_test.go` per backend + `live_*_test.go` |
| `internal/config` | `Settings`, its defaults, normalisation, prompts and presets | `config.go` (`Default`, `Load`, `Save`, `normalise`, `Clone`), `prompts.go` | `config_test.go`, `clone_test.go` |
| `internal/store` | SQLite: schema and migrations, chats/messages/attachments/documents, FTS5 search, caches, and encryption at rest | `store.go` (schema, `Open`, `OpenKeyed`), `crypt.go` (the keyed VFS), `encrypt.go` (`Encrypt`, `IsEncrypted`, `RecoverMigration`), `models.go` (types), `queries.go` (every query) | `store` tests, including the `KICHAT_TEST_DBKEY=1` keyed run |
| `internal/dbkey` | The database key: generation, the OS keyring entry, and `kichat.key` wrapped under an Argon2id-derived key | `dbkey.go` (`New`, `KeyringKey`, `SaveRecovery`, `OpenRecovery`) | `dbkey_test.go` |
| `internal/images` | Picture generation: the `Provider` interface, the WebUI client, `sd-cli`, the managed `sd-server`, and the one hosted kind (NVIDIA NIM). Enlarging: the Lanczos kernel, EXIF orientation, Real-ESRGAN | `images.go` (`Provider`), `sdwebui.go`, `cli.go`, `sdserver.go`, `nim.go` (hosted), `locate.go`, `limits.go`, `upscale.go` (`Upscale`, `Fit`), `resample.go`, `exif.go`, `esrgan.go`, `upscaler_install.go` (the pinned archives) | `*_test.go` + `live_test.go`, `live_sdserver_test.go`, `live_esrgan_test.go` |
| `internal/runtime` | llama.cpp as a thing KiChat installs and supervises: releases, GPU families, model folders, projector pairing, offload reading — plus the user's terminal, started in a folder for the hand-off (`terminal.go` and one `terminal_<os>.go` per platform) | `install.go`, `builds.go`/`distribution.go`, `supervise.go`, `discover.go`, `libs.go`, `offload.go`, `terminal*.go` | `*_test.go` + `live_prism_test.go` |
| `internal/research` | Brave and DuckDuckGo, the search-first context route, the scraper and its guards | `search.go`, `chain.go`, `brave.go`, `brave_context.go`, `duckduckgo.go`, `scrape.go` | `*_test.go` + `live_test.go` |
| `internal/ingest` | Attachments to text: PDF, office, chunking | `ingest.go` (dispatch), `pdf.go`, `office.go`, `chunk.go` | `ingest_test.go` |
| `internal/tools` | What the model can call, native schema *and* text protocol, plus `generate_image`, the project reads (`read_file`, `list_dir`, `search_files`, `find_files`), the changes (`write_file`, `edit_file`) and the terminal (`run_command`, which can be stopped without ending the turn) — the last three stop for the user's confirmation | `tools.go` (`Result`/`Summary`, `argumentAliases` — the field names another convention uses for these tools), `image.go`, `project.go` (reads), `find.go` (the glob), `reads.go` (the per-turn read shortcut and its invalidation), `change.go` (writes, the guarded list and the question), `snapshot.go` (the undo record), `command.go` (the terminal: caps, timeout, result, `StopCommand`) with `command_unix.go`/`command_windows.go` (the shell) | `tools_test.go`, `arguments_test.go`, `image_test.go`, `project_test.go`, `find_test.go`, `reads_test.go`, `change_test.go`, `snapshot_test.go`, `command_test.go` |
| `internal/workspace` | The folder a coding project works in: resolving it, keeping every path inside it, the read/list/search it allows (including what `.gitignore` excludes), the project's own AGENTS.md at the root and near a nested file, and the two write operations — each computed twice, once to be shown and once to be performed | `workspace.go` (`Open`, `Root.Resolve`, `Read`, `Text`, `List`, `Search`, `AgentsFile`/`AgentsFileNear`, `Remove`, `Digest`), `edit.go` (`DescribeWrite`/`Write`, `DescribeEdit`/`Edit`, `Change`), `diff.go` (`CompareDiff`, the hunks a change is shown as), `find.go` (the glob), `ignore.go` (`.gitignore`) | `workspace_test.go`, `edit_test.go`, `diff_test.go`, `find_test.go`, `ignore_test.go` |
| `internal/proc` | Starting a process in its own group and ending its tree, shared by the terminal tool, the supervised servers and the image CLI instead of three copies | `proc.go`, `proc_unix.go`, `proc_windows.go` | — |
| `internal/terminal` | The user's own terminal, started in a folder (the hand-off's *open terminal*) | `terminal.go` (`Detect`, `Open`), one `terminal_<os>.go` per platform | — |
| `internal/secret` | API keys: OS keyring, encrypted-file fallback | `secret.go` (`New`, `NewIsolated`) | `secret_test.go` |
| `internal/backup` | The portable archive: a selection of chats as one passphrase-encrypted file (AES-256-GCM, Argon2id) | `backup.go` (`Encode`, `Decode`, `Manifest`) | `backup_test.go` |
| `internal/trace` | The content-free diagnostic log: a fixed event schema, error kinds instead of error text, rotation beside the encrypted database | `trace.go` (`Logger`, `Event`, `KindOf`) | `trace_test.go` (+ `internal/chat/trace_test.go` for a whole turn) |
| `internal/logfile` | The launch log: `kichat.log`, written by `main.go` before a window exists | `logfile.go` (`Open`) | — |
| `internal/events` | The names of every Wails event, so emitters and the frontend cannot drift | `events.go` | — |
| `internal/uimsg` | A user-facing failure from Go as a dictionary key and its arguments, since Wails marshals a bound method's error as text; `Detailf` adds the sentence the model reads | `uimsg.go` | `uimsg_test.go` (and `i18n_test.go` in the root, for the dictionary half) |
| `internal/version` | Build identity filled by `-ldflags -X` | `version.go` | `version_test.go` |

## Frontend

| Path | Owns |
| --- | --- |
| `frontend/src/App.tsx` | Shell: sidebar, chat list, settings modal, global key handling (Escape) |
| `frontend/src/components/ChatView.tsx` | The conversation: message list, composer (which answers `/clear` itself), attachments, context meter, stop, in-chat search (Ctrl+F), and a project's folder and permission note |
| `frontend/src/components/ChatSearch.tsx` | The find bar over the transcript: query, match counter, previous/next, close |
| `frontend/src/components/Confirmation.tsx` | The question a project tool asks, in its two shapes — a change's two sides with line counts, a command's line *as an editable field* with its folder, shell and limit — and the three answers. It is rendered at the composer because the turn behind it is blocked |
| `frontend/src/components/ProjectChanges.tsx` | A project's changed files: the header's count, the panel, each file's diff and its undo (or the reason undo is not offered) |
| `frontend/src/components/MessageParts.tsx` | How one message renders — the split between settled and streaming pieces |
| `frontend/src/components/Markdown.tsx` | The markdown pipeline: remark/rehype, KaTeX, highlight.js |
| `frontend/src/components/settings/` | The settings screen: `Settings.tsx` is the shell and tab strip, `shared.tsx` the `Section`/`Field` helpers, and one file per tab (`GeneralTab`, `BackendsTab`, `ImagesTab`, `PromptTab`, `CodingTab` for what a project's tools may do unasked, `ResearchTab`, `ContextTab`, `PayloadTab`, `DataTab` for database encryption, backup and import) |
| `frontend/src/components/UnlockScreen.tsx` | The window while the database is locked: enter the passphrase, or set one when no keyring exists |
| `frontend/src/components/ModelSwitcher.tsx` | Model picker with capability badges and window size |
| `frontend/src/lib/api.ts` | The typed wrapper over the generated `window.go` bindings |
| `frontend/src/lib/i18n.ts` | Every user-facing string, in `de`, `en` and `zh` |
| `frontend/src/lib/stream.ts` | Cutting a streaming reply into stable, append-only pieces (the memo depends on it) |
| `frontend/src/store.ts` | Zustand state shared across components |
| `frontend/wailsjs/` | **Generated** by `wails build`; never hand-edit |

## Storage

One SQLite file, encrypted page by page (see `ENCRYPTION.md`). Schema and migrations are
in `internal/store/store.go`; adding a table is a new migration, never an edit of an old
one. The key is in the OS keyring by default; `kichat.key` beside the database is the
passphrase-wrapped recovery copy, and `internal/dbkey` owns both.

`chats`, `messages`, `compactions`, `tool_calls`, `file_snapshots` (what a project's
files looked like before this chat changed them, for the undo), `attachments`,
`documents`, `chunks` (+ `chunks_fts`, FTS5/BM25), `search_cache`, `scrape_cache`,
`settings` (one JSON row — see `internal/config`), `model_overrides`, `counters`.

Export and import of a chat is `internal/store/bundle.go` (`LoadChatBundle`,
`SaveChatBundle`, one transaction per chat), wrapped in the archive format by
`internal/backup` and exposed by `api_backup.go`.

## The request lifecycle

    ChatView send
      → lib/api.ts
      → App.SendMessage (app.go)
      → chat.Engine.Send
          → chat.Assemble (system prompt + history + attachments + budgets)
          → llm.Provider.Chat (per-backend request)
          → chat.Complete / tool loop (tools.Service)
          → events: chat:start, chat:delta, chat:reasoning, chat:tool, chat:budget, chat:done
      → store writes
      → the window's listeners (ChatView) render the deltas

Anything that crosses the bridge is a method on `*App`; anything that emits
progress uses a name from `internal/events`.

## The extension points

| To add… | Touch | Skill |
| --- | --- | --- |
| A chat backend (hosted API) | `internal/llm/<name>.go`, `registry.go`, `DefaultBackendConfigs`, `settings/BackendsTab.tsx` kind lists, docs | [`add-llm-backend`](../.claude/skills/add-llm-backend/SKILL.md) |
| An image backend | `internal/images/<name>.go` implementing `Provider`, `images.Provider` construction in `app.go`, settings | [`add-image-backend`](../.claude/skills/add-image-backend/SKILL.md) |
| A setting | `internal/config/config.go` (+ `clone_test.go`), the settings UI, i18n | [`change-settings`](../.claude/skills/change-settings/SKILL.md) |
| An enlarging method | `internal/images/upscale.go` (`Method*`, the switch in `Upscale`), the Enlarge panel, i18n | [`UPSCALING.md`](UPSCALING.md) |
| A tool the model can call | `internal/tools/tools.go` (`Definitions` + execution), `internal/chat` if it needs a round | — |
| A tool that reads the project folder | `internal/workspace` for the path boundary, `internal/tools/project.go` for the schema and result, `chat.toolDefinitions`/`projectTool` for the mode gate | — |
| A tool that changes the project folder | `internal/workspace/edit.go` (the `Describe*`/apply split), `internal/tools/change.go` (schema, `approveChange`, `WriteToolNames`), `internal/tools/snapshot.go` (the undo record), `tools.NeedsApproval` in the engine's gate, `api_changes.go` (the event and the bound answer), `Confirmation.tsx`, `ProjectChanges.tsx` | [`CODING.md`](CODING.md) |
| A tool that runs something on this machine | `internal/tools/command.go` (schema, caps, timeout, result) plus `command_unix.go`/`command_windows.go` (the shell, the process group, the tree kill), `approveCommand`, the command half of `approvalPayload`, `Confirmation.tsx` | [`CODING.md`](CODING.md) |
| A new chat mode (a different tool set or prompt per conversation) | `store.Chat.Mode` + a migration, `internal/chat/engine.go`'s tool gate, the branch that creates the chat in `app.go`, the sidebar's `newChat`/`newProject` controls | — |
| A tool result condensed before the model reads it | `internal/chat/digest.go` (the gate, cache and escape hatch), `toolOutputBudget.take` in `generate.go`, `config.DigestSettings` | [`COMPACT.md`](COMPACT.md) §17 |
| A slash command the window answers instead of sending | `ChatView.tsx` (`submit`, where the command is matched) and the bridge method behind it | — |
| A Wails event | `internal/events/events.go`, the emitter, the frontend listener | — |
| A user-visible string | `frontend/src/lib/i18n.ts`, all three languages | — |

## Tests, by kind

| Kind | Where | What it proves |
| --- | --- | --- |
| Unit | `*_test.go` beside the package | behaviour of one package, no network |
| Bridge/JSON | `*_test.go` **at the repo root** | the shapes the frontend receives — `runtime_status_test.go` pins the null-vs-empty case |
| Live | `live_*_test.go`, gated by `KICHAT_LIVE=1` | the real service, metered; see [`live-tests`](../.claude/skills/live-tests/SKILL.md) |
| Renderer | `tools/streamcheck/check.mjs` | every prefix of a reply renders identically streaming and settled |
| UI | `tools/uiharness` | rendering, state flow and event wiring against a mocked bridge |

Secrets in tests: always `secret.NewIsolated(dir)`. The real keyring is a
per-user global resource, and `secret.New` probes it.
