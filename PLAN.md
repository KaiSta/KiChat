# PLAN.md

The plan, the decisions behind it, and what has actually been proven. This is a record
of intent and evidence, not a wish list — every claim under *Verified* was checked
against a running build, and everything unproven is labelled as such.

---

## 1. Objective

Build **KiChat**, a local-first desktop research chat app, so long research
conversations can run against models on the user's own machine instead of a hosted
service. Hard requirement: the user's prompts, files and generated answers never leave
the machine except when the user explicitly invokes web search.

Target UX: a single-window chat client that behaves like a serious research tool —
context that survives long conversations, sources that can be inspected, and honest
numbers about what is being sent to the model.

### Feature scope

| Area | Delivered |
| --- | --- |
| Backends | LM Studio (`:1234`) and Ollama (`:11434`), plus DeepSeek, Mistral, NVIDIA (build.nvidia.com), Groq and llama.cpp (`:9931`) shipped prepared-but-off, plus a generic OpenAI-compatible endpoint |
| Local models | The llama.cpp entry can also be provided by KiChat itself: it fetches `llama-server` from a build source — upstream llama.cpp, or PrismML's fork when a ternary model is wanted — from the family that platform publishes (CPU, Vulkan, ROCm, CUDA, SYCL, OpenVINO or the Adreno build), starts a GGUF of the user's choosing on a free port, and stops it with the app, started again on every launch while the switch is on. The GGUF does not have to be downloaded again: the picker lists what is already in the folder LM Studio uses |
| Context | Automatic and manual compaction, live context-window fullness meter, per-segment inspector |
| Research | Brave Search (API key) with DuckDuckGo fallback, page scraping with SSRF guard, optional search-before-answer |
| Files | Attachments, PDF/office text extraction, chunked with BM25 retrieval over the chunks |
| Images | Text-to-image generation through software the user runs themselves, in one of two kinds: a Stable Diffusion WebUI reachable at `/sdapi/v1/txt2img` (AUTOMATIC1111, Forge or stable-diffusion.cpp's `sd-server`), or stable-diffusion.cpp's `sd-cli` run once per picture with no daemon at all. Asked for by a `generate_image` tool the chat model calls, or by the draw controls under a picture. The picture is stored as an ordinary image attachment, so it renders in the chat, can be saved to a location of the user's choosing, and can be handed to a vision model on a later turn. See IMAGES.md |
| Prompting | Editable system prompt (German research default), five shipped presets — including one tuned for small local models — template variables (`{{datetime}}`, `{{location}}`, `{{tool_list}}`, `{{context_used}}`, …) |
| Tools | Native tool calling with an automatic text-protocol fallback |
| Metrics | Per-message tokens/s, TTFT, generation duration, stop reason, token counts |
| UX | In-chat model switching with a pre-switch warning, Stop button, English default with German and Chinese also shipped, dark-first theme |

---

## 2. Architecture

Wails v2 desktop app. The Go side owns all state, token accounting and I/O; the
frontend renders. Nothing important is computed in JavaScript.

```
main.go            Wails bootstrap: window, theme, go:embed all:frontend/dist
app.go             App struct, service wiring, ChatState, chat CRUD
api.go             Remaining bound RPCs (settings, search status, secrets, compaction)
internal/
  events/          Event names shared by the Go emitters and the frontend's map
  llm/             Provider abstraction over the wire formats
    types.go         Message, ModelInfo, Usage, GenerationStats, StreamEvent, Provider
    tokens.go        Heuristic estimator (pre-send prediction only)
    httpclient.go    Shared client, retries, timeouts; every response's headers
                     handed to a watcher on the context
    limits.go        An over-size refusal told apart from a throttle, and the
                     per-request ceiling it states (GROQ.md §6)
    pace.go          The minute a turn is paced against, in both the ways it is
                     measured: what is left of a backend's tokens and when they
                     reset, and the requests-per-minute rate learned from a
                     refusal and counted from this app's own responses — and the
                     wait before a request either cannot pay for (GROQ.md §6.4)
    openai.go        LM Studio / OpenAI-compatible SSE streaming
    ollama.go        Ollama NDJSON streaming
    deepseek.go      DeepSeek over the OpenAI wire format, with its own error envelope
    mistral.go       Mistral: its own listing (chat-capable models only, the aliases
                     collapsed to one row per model, per-model capabilities) and the
                     typed content chunks its reasoning models stream, which
                     openai.go's deltaContent decodes
    nvidia.go        build.nvidia.com: the four-field listing, which carries no
                     capability or window and lists models the account may not
                     call — so the provider filters nothing and the window stays 0
    groq.go          Groq: the listing that publishes each model's window,
                     modalities and features — so the capabilities are the
                     vendor's own claims and the models that cannot chat
                     (Whisper's transcription, the TTS voices) are dropped
    probe.go         One token per model, to learn what no listing can say; the
                     verdicts become the picker's blacklist (see NVIDIA.md)
    llamacpp.go      llama.cpp /props: the loaded window, template caps, timings
    registry.go      BackendConfig, provider construction, model catalogue cache
  store/           SQLite persistence (modernc.org/sqlite, pure Go)
    models.go        Chat, Message, Attachment, Document, Chunk, Compaction
    queries.go       CRUD, FTS5 BM25 search, counters, cascade deletes
    store.go         Schema creation and migrations
  config/          Settings, defaults, normalisation, default system prompt, presets
  chat/            The engine
    engine.go        Send/Stop/turn serialisation, Measurement registry
    generate.go      One turn: model resolution, streaming, tool loop, stats
    context.go       Prompt assembly and ContextBudget with measured-vs-estimated
                     accounting, measured against the effective per-request
                     limit (window or a backend's own ceiling, GROQ.md §6)
    compact.go       Compaction (summary generation, never destructive)
    prompt.go        Templating including tool-list substitution
    preview.go       Token prediction before sending
    maintenance.go   Auto-title, background upkeep
    oversize.go      A request the backend refuses for its own size: fold the
                     history, retry once, remember the ceiling — which the
                     backend's own budget header states before anything is
                     refused, so the next turn is folded instead (GROQ.md §6)
    searchfirst.go   Search-before-answer seeding
  research/        search.go, chain.go (Brave -> DuckDuckGo), brave.go, duckduckgo.go,
                   scrape.go (SSRF + robots guards), util.go
  ingest/          ingest.go, pdf.go (pdftotext), office.go (libreoffice), chunk.go
  images/          sdwebui.go: the /sdapi/v1/txt2img client, one route for
                   AUTOMATIC1111, Forge and stable-diffusion.cpp (see IMAGES.md)
  tools/           Tool definitions and dispatch; image.go turns a generate_image
                   call into rows in the attachments table
  runtime/         The llama.cpp KiChat installs and runs itself
    install.go       release lookup per build family, verified download, extraction
    supervise.go     one server per managed model: free port, /health, log tail, stop
    version.go       what the launcher reports about itself (`--version`)
    discover.go      where LM Studio keeps its models, and a bounded scan of a folder
    proc_unix.go     process group and SIGTERM-then-SIGKILL (proc_windows.go is the pair)
  secret/          API key storage outside the repository
frontend/src/
  main.tsx         React root; imports style.css
  App.tsx          Shell: sidebar, toasts, chat/settings view switch
  store.ts         zustand store: single wiring point for events, send/stop/regenerate
  lib/api.ts       Typed binding layer (see the Plain<T> note below)
  lib/i18n.ts      en/de/zh strings; a missing translation is a compile error
  lib/stream.ts    arriving text: settled/live split, open-fence detection, throttle
  components/      ChatView, ModelSwitcher (meter + inspector), MessageParts,
                   Markdown (one parse per settled piece, live piece throttled),
                   Settings (5 tabs)
```

### Data flow of one turn

`Send` -> resolve model -> drop trailing reply if regenerating -> persist the user
message -> seed search if enabled -> maybe auto-compact -> `generate` -> stream events
to the UI over Wails events -> persist the assistant message with backend-reported
usage -> emit `chat:done` with `{chatId, messageId, lastMessageId, status, stats,
usage}`.

Turns are serialised per chat: a second `Send` while one is in flight is rejected
rather than interleaved.

### Events (`internal/events`)

`chat:start`, `chat:delta`, `chat:reasoning`, `chat:tool`, `chat:message`,
`chat:status`, `chat:done`, `chat:error`, `chat:warning`, `chat:budget`,
`chat:compaction`, `chat:title`, `chat:attachments`, `chats:changed` — named once in
`internal/events`, emitted only through those constants, and wired in exactly one place
in `store.ts`, so there is a single place to reason about event handling. Two tests in
that package read the frontend's map and scan the emit sites, because a wrong name does
not break the build — it just quietly leaves a listener that never fires.

Phases ride on `chat:status`, which is what the panel shows beside the composer:
`searching`, `compacting`, `tool`, `stopping`, and `throttled` — a retry sleeping out a
backend's own rate limit, which is the one phase whose payload carries more than its name
(`waitMs`, how long the backend asked for) because "waiting" alone does not say whether
that is a second or a minute. It was added because a silent wait is indistinguishable from
a hang: see `internal/chat/throttle.go` and `RetryWatcher` in `internal/llm`.

### Storage

SQLite at `~/.config/kichat/kichat.db`:
`chats`, `messages`, `compactions`, `tool_calls`, `attachments`, `documents`, `chunks`,
`chunks_fts` (+ shadow tables), `search_cache`, `scrape_cache`, `settings`,
`model_overrides`, `counters`.

API keys live in `~/.config/kichat/secrets`, deliberately outside both the database and
the working tree, so they cannot be committed by accident.

### Versions

Go 1.26.0 · Wails v2.15.0 · React ^19.1.0 · zustand ^5.0.15 · Vite ^7 · TypeScript ^5.6.3

---

## 3. Decisions and why

**Token accounting is backend-authoritative.** The backend's reported `prompt_tokens`
is the truth; the heuristic estimator (no tiktoken) is used only to predict a request
*before* it is sent, and the UI badges those numbers `estimated`. Mixing the two would
make the context meter quietly wrong, which is worse than showing nothing.

**`stream_options.include_usage` is required for LM Studio.** A streamed response omits
`usage` entirely unless the request asks for it. Without that flag the context meter
could only ever display an estimate. This is now one of the few things protected by a
test (see *Verified*).

**Compaction never deletes messages.** Originals stay in the database and remain
viewable, so a summary is always a view over history rather than a replacement for it.
Losing the user's own text to save context would be unacceptable in a research tool.

**Tool calling is negotiated, not assumed.** Native `tools` are used when
`ModelInfo.Capabilities` advertises tool support, otherwise a text protocol is used.
Local model support for native tools is inconsistent, and silently sending unsupported
`tools` produces worse behaviour than falling back.

**SQLite via `modernc.org/sqlite`.** Pure Go, so no cgo and no cross-compilation pain.
FTS5 with BM25 was verified working before committing to it.

**Scraped content is untrusted data, never instructions.** The default system prompt
carries an explicit anti-prompt-injection clause, and page fetching has SSRF and
robots.txt guards.

**KiChat runs the model itself when asked to.** A user whose only local model server is
one they started by hand has to start it before every session, and the app can neither
know which GGUF is loaded nor stop it afterwards. So the llama.cpp entry carries a
second half that the app provides rather than contacts: it fetches `llama-server` from
llama.cpp's own releases, starts it on a free port with a GGUF the user picked, and stops
it with the app. Four decisions follow from that.

*The archive is unpacked whole, not a single binary out of it.* `llama-server` is a
17 KB launcher whose code is in a `libllama-server-impl.so` beside it, next to
`libllama.so`, `libggml*.so` and the symlinks that name them; an install that kept only
the launcher would produce a build that lists models and cannot start. Every published
archive also wraps its contents in its own directory (`llama-b11528/`), which is
unwrapped once at install time so that every derived path — including the one the
launcher is chmodded at — stays one level deep. GitHub's *latest* release is not the one
to ask: since llama.cpp moved to versioned tags it points at a semver release with no
binaries at all, so the release list is scanned for the first entry that actually
publishes the asset. Which *family* to fetch is a choice: the releases carry CPU, Vulkan,
ROCm, CUDA, SYCL, OpenVINO and (for Windows on arm64) Adreno builds, and the table in
`variantsFor` is the measured list per platform and architecture — ROCm, SYCL and
OpenVINO are x64 only and macOS publishes one Metal-enabled build per architecture. A
family whose name carries a suite version (`cuda-13.4`, `rocm-10.0`, `sycl-fp16`) is
asked for with a wildcard and matched against what the release actually publishes, so
the day the suite moves on nothing here needs editing; when a release carries several
suites of one family the newest wins, and the `cudart-` bundles — the same build with the
CUDA runtime folded in — are skipped rather than mistaken for it. Each family installs
into its own directory (`b11535-rocm-10.0`), because folding them together would delete
the CPU build the moment a user tried the ROCm one and re-download 600 MB to go back. A
release tag is not a version either: `b11534` is a name this app
chose for a download, it says nothing about a launcher the user supplied instead, and it
is the *published* build rather than the one on disk after a rebuild — so the panel asks
the binary itself and shows what it answers (measured on `b11535`: `0.6.0-dev (build
11535, commit f39148a95)`), read once per path and again after a save, because a status call is polled
while a model loads and spawning a process per poll would be its own bug. The download is checked against the sha256 GitHub reports for it,
and extraction happens into `<tag>.installing` and is renamed into place only when it is
complete, so an interrupted install leaves nothing that looks installed — nothing a start
would run, either: the name is refused outright wherever a build is looked for, because a
crash late in an install leaves a launcher in the staging directory, and the tag inside
that name is real enough to pass any check on the text alone.

*The build comes from one of two sources, and the choice is part of every answer about
it.* Upstream's `ggml-org/llama.cpp` releases are the default. PrismML's fork of the same
project is the other, and it is not a preference: it adds the GGUF types a ternary model
is stored in (`PQ2_0`, `PTQ1_0`), which mainline llama.cpp refuses to load — so a ternary
model such as Bonsai 2 runs on that build and on nothing else. The two differ in ways that
cannot be smoothed over: different repositories, different tag schemes (`prism-b10770-6684606`
against `b11541`), different archive names (`linux-cuda-*` against `ubuntu-cuda-*`,
`win-hip-radeon-x64` for ROCm on Windows), a family list one shorter (no OpenVINO, no
Adreno, no arm64 CUDA on Linux) and, on Linux CUDA, no runtime bundle at all to attach —
the fork publishes the companion asset for Windows only, while its backend asks for
`libcudart.so.13`/`libcublas.so.13` just as upstream's does. So the source is one setting for the whole runtime — one
runtime directory, one installed launcher, one update lookup — and it is a value in
`internal/runtime` (`Distribution`) rather than a flag threaded through the installer.
What follows from it is one rule, and it is the one a user would notice being broken: a
managed start runs a launcher of the *selected* source or none at all. It never runs the
other llama.cpp, because a user who selects the fork to load a ternary model would
get a build that cannot load it, silently. The installs coexist in the runtime folder,
so switching back and forth does not re-download.

*Which family decides whether there is a GPU, and the app owns the rest of it.*
Offloading itself is llama.cpp's: `-ngl/--gpu-layers` documents "an exact number, 'auto',
or 'all' (default: auto)" with `--fit on`, so a GPU build places as many layers as fit
without the app passing anything. What KiChat adds is the half that was missing. A
managed model carries a mode — `auto` (nothing passed), `all`, or the app's own word
`off`, which becomes `-ngl 0` because llama.cpp has none — and the *answer* to "did it
really happen?" comes from the server itself: `using device Vulkan0 …` and
`load_tensors: offloaded 65/65 layers to GPU`, read out of its log as the lines arrive.
The device line is the load-bearing one, because the layer line alone means nothing: a
CPU-only build reports every layer offloaded and names no device, so the app says a model
is on the GPU only when the server named the card it went to.
Those lines exist only at llama.cpp's trace level, so a managed start asks for it — but
only for a launcher this app installed, because `-lv` is a flag an older binary a user
points at may not know and an unknown argument stops a server rather than a report. A
layer count, `-dev`, `-ts` and `-sm` stay where a flag for one model belongs: in its
further arguments, which are passed last and therefore win.

What the fork adds is that offloading a ternary model is worth doing at all: its ternary
kernels are compiled into the GPU backends, not only into the CPU ones — measured
`dequant_pq2_0`/`matmul_pq2_0_f32_fp32_data`/`block_pq2_0_packed16` and
`mul_mat_vec_id_ptq1_0_q8_1_f32_subgroup` in `libggml-vulkan.so`, and hundreds of
`pq2_0`/`ptq1_0` kernel names in `libggml-cuda.so` and `libggml-hip.so`. A backend that
lacked them would move the layers to the GPU and keep doing the maths on the CPU.

*A managed server's address cannot live in settings in advance.* The port is chosen at
start, so when a server reports ready the address it settled on is written into the
backend entry, the entry is enabled, the settings are saved and the services are rebuilt.
That is what makes the row in settings and the chat's registry name the server that is
actually running, and it is also why the address survives a restart of the app: it is
persisted, even though it will be replaced by the next start.

*Readiness is the server's own answer, not the process existing.* A model load takes
minutes, so `Start` returns as soon as the process exists and the state is reported as it
changes. `/health` answers 503 while the weights are coming in and 200 once the server can
serve, which is exactly the distinction needed — a running process says nothing about
whether a chat would work. The start is not tied to the request that asked for it either:
that caller is long gone by the time the model is up.

*Models a user already has are used, not downloaded again.* Asking someone with 40 GB of
GGUF files in LM Studio to fetch another copy to run in KiChat would be an absurd answer
to "run this locally", so the picker lists what is already on disk. The folder to offer
first is the one **LM Studio's own setting names** — read out of `~/.lmstudio/settings.json`
as a single field, since it is another application's private file and all that is wanted
from it is a hint — and only then `~/.lmstudio/models`, its pre-0.3.x location, and the
models it bundles. The measured reason for that order: on the machine this was written
on, `~/.lmstudio/models` exists and is **empty** while that setting points at a folder on
another drive holding six chat models, so "the first folder that exists" reports no
models on a machine full of them. The scan stops at a depth and a file count and says
when it did, because a folder can be a drive root or a network share; it descends into no
hidden directory (LM Studio's housekeeping lives in `.internal`), and it skips `mmproj-*`
files, which carry the same extension as a model and which llama-server refuses to load.

*A model that is on disk is one click from the conversation.* The chat's picker carries the
managed server's own files, because a stopped server answers with nothing and the catalogue it
lists from therefore has no row for it: without this, starting a local model meant Settings →
Backends, naming the file, waiting out the load, and coming back to pick the model. The click
loads that file and moves the chat onto it *after* the server is ready, and it stores the file
on the entry as well — the chat and the panel must not end up naming different models, and the
next launch has to start what was picked. What it does not do is flip the entry on: whether
KiChat starts a model by itself at launch stays a setting, so a pick from the chat starts it
now and promises nothing about the next launch. The same section gives the memory back: the
state line that says a server is up carries the button that ends it, because the panel whose
click took the memory is where releasing it belongs, and because a load begun by mistake — a
27B file is minutes of it — has to be callable off from where it was started. A stop writes
nothing, so the file the entry was given is one click from running again.

*Saving a model does not start it.* It used to, and that turned every edited field — the
context size, the extra arguments — into a restart of a model that may have taken minutes
to load. Starting and stopping are their own calls, and switching a model off stops it,
because a multi-gigabyte model left in memory with no row that offers to stop it is the
worse failure. The runtime lives in `Config/RuntimeDirectory` (`~/.config/kichat/runtime`),
not in the repository and not in the database, and the binaries are shared by every
managed entry while the model choice is per entry. The same release archives are used for
every platform, so the format follows the *asset's* name rather than the host's: Windows
ships `.zip` even when the app runs on Linux.

**The frontend type layer wraps the generated bindings.** Wails generates TS *classes*
with a `convertValues` helper on any type containing nested structs, which makes object
spreads and hand-built literals fail `tsc`. `lib/api.ts` defines a recursive `Plain<T>`
that strips it (with an explicit `readonly unknown[]` branch first, otherwise the mapped
type stops being homomorphic and destroys arrays), then exports namespaced aliases.
`Gen` must be imported as a value, not `import type`, or `Settings.createFrom` breaks.
`BackendConfig` was given camelCase JSON tags because the generated names
(`ID`/`BaseURL`) were unusable from TypeScript.

**Language:** English is the default, with German and Chinese shipped alongside it. The interface and the shipped system prompts move together: every built-in prompt exists in each shipped language, and `(*Settings).LocalisePrompts` rewrites one that is still byte-identical to a shipped body into the language the interface is set to. A prompt the user edited — or added themselves — is never touched, because rewriting it would throw their work away and a half-translated prompt is worse than an untranslated one. That runs both when settings are loaded and when they are saved, which is what makes switching the language in settings change the text in the textarea beside it instead of waiting for a restart. A stored value the build does not recognise resolves to English, so a hand-edited or older settings blob can never leave the UI without a dictionary, and a language with no prompt body falls back to the English one rather than to an empty textarea. All three dictionaries are keyed off one key set, so a key added to one language and forgotten in another fails `tsc` instead of falling back silently.

**The system-prompt presets are data, not code branches.** `config.DefaultPresets(lang)` returns five built-ins — research (the default), small-local, assistant, coding and blank — and the settings view offers them as a dropdown. The small-local one exists because a local setup often starts on a model that cannot hold a long instruction block: it is short, imperative, and bans the two failure modes that bite hardest at that size (inventing facts, and running off into unstructured prose). The list is stored whole in the settings blob, so a preset shipped later would never reach an existing installation — `normalise()` therefore appends any built-in that is missing while leaving an edited one untouched, the same way the shipped backends are re-added.

**The context window is re-read on demand.** The limit is resolved from the model's loaded context length, falling back to its maximum only when the model is not loaded — LM Studio reports both, and confusing them is how a model running at 8k gets displayed as 128K. Because a listing is cached (30 s in the registry, 10 minutes for Ollama's `/api/show`, and indefinitely in the frontend store), a model loaded or reconfigured after the app last looked keeps showing the old number. The meter therefore carries an explicit re-check that forces a fresh listing *and* clears the providers' own caches — `CacheInvalidator` is a separate optional interface, so a provider without such a cache does not have to implement an empty one — then recomputes the chat state, since the budget is derived from that same catalogue server-side.

**The transcript follows the newest content.** A reply arrives as *one* item whose text grows, and Virtuoso's `followOutput` reacts to appended items only — its own documentation says "keep the list aligned when new items are appended" — so the view stayed at the offset it had when that item was appended. Measured in the mock-bridge harness before the change: the streamed reply left the last ~740 px of the answer below the fold, and it was still there once the turn had ended, so every reply had to be fetched with a scroll of its own. The follow is therefore driven by the list's own height, which changes for an appended item and a growing one alike, and the decision to act on it is the direction of the user's scroll rather than the distance from the bottom: growth pushes the bottom away without moving the scroll position at all, so a position that has moved up is someone reading history and one that reaches the bottom again is someone done with it. Two details are load-bearing, both from that run. The upward test carries an 8 px tolerance, because the engine settled a follow of its own by moving the position back by *one* pixel, and a strict comparison read that as the user and stopped following for the rest of the turn. And Virtuoso's `atBottomStateChange` is deliberately not the flag, for the same reason in a different disguise: one frame of growth between a follow and that callback's next test says "not at the bottom". Sending re-enables the follow, because it is a deliberate move to the newest content, and opening a chat lands on the newest *message* — `initialTopMostItemIndex` with `align: 'end'`, since a bare index aligns the last item's top with the top of the viewport, which for a long reply is the middle of it.

**The build asks the toolchain which platform it is for, and the Windows release identity is derived rather than typed.** Nothing in the Go code needed a port — no cgo, no Unix-only call outside the two platform files that already split process handling — so the platform story is three specific things. The webkit2gtk tag and the Debian hardening flags are keyed on the *target* platform in the Makefile (they are Linux-only by construction: `-buildmode=pie` is Go's Linux flag and `-Wl,-z,now` is an ELF linker option), and the platform is passed explicitly because the Wails CLI's default is the host's. A Windows binary therefore comes from `make windows`, which cross-builds from Linux and macOS: Wails writes the executable's version resource, manifest and icon in pure Go, so no mingw is involved — measured, a 22 MB PE32+ GUI executable. And because those file properties are *rendered* from the `info` block of `wails.json` with no flag to override them, the version would otherwise be a second place to type a release: the first cross-built `kichat.exe` carried `ProductVersion 1.0.0` and `LegalCopyright "Copyright........."` (the defaults) while the binary's own `--version` said `0.4.0`. `make version-info` derives both fields from `VERSION` instead — the numeric part into `productVersion`, the full string into `comments`, since a Windows version field is at most four numbers and `0.2.0~beta1` has no numeric spelling of the marker — and `make check` compiles everything for Windows (0.86 s) so that half of the port cannot rot unnoticed on a machine that has no Windows.

The process half follows the same rule: Windows has no process group, so the "and everything it started" that `Setpgid` plus a negative pid gives on Unix is asked of `taskkill /T /F` at both call sites — the one-shot draw and the supervised server — because a survivor inherits the output pipes the stop path waits on, which is the failure the Unix group kill exists to prevent. What a job object would add (holding the tree from the moment the process starts) is not reachable from the two call sites' shape: one runs before the start and one after it, and the job has to be created before and carried between.

---

## 4. Verified

Everything below was observed on the real build against a live LM Studio instance
(`swift-1.5-qwen3.8-27b-gsq-rco`, 48,128-token loaded context) — not inferred.

**Unit tests** — `go test ./...` passes; `go vet ./...` and `gofmt` clean. Coverage
includes the SSE reader at *every* chunk boundary (a JSON frame straddling a network
read), tool-call assembly, compaction, Stop mid-generation, serialised turns, Brave and
DuckDuckGo parsing, SSRF/robots rejection, store round-trips with FTS and cascade
deletes, and a real synthesised PDF through the ingest path.

**Live tests** — skipped by default, run with `KICHAT_LIVE=1`:
`internal/llm/live_test.go` (`TestLiveLMStudio`) and `internal/chat/live_test.go`
(`TestLiveEngineTurn`). They assert a measured `promptTokens > 0` and that a
`Measurement` is recorded. That assertion is the guard for the `stream_options` fix: if
it ever regresses, the meter silently degrades to an estimate and the test fails.

**End-to-end through the GUI** — the app was driven for real (a message typed into the
composer and submitted), and the database confirmed the result:
`"Nenne die Hauptstadt von Italien in einem Satz."` -> assistant replied
`"Die Hauptstadt von Italien ist Rom."`, persisted `status=complete` with
`prompt_tokens=1713`, `completion_tokens=207`, `tokens_per_second=35.38`,
`ttft_ms=2194`, `stats_source=estimated`, `stop_reason=stop`. The UI rendered the stats
row and the context meter switched from the estimated `1.450` to the **measured**
`1.713 / 48.128 ctx`.

**The managed runtime, against the real thing.** `internal/runtime` was exercised end to
end on this machine, not against a fixture: the release lookup returned
`llama-b11534-bin-ubuntu-x64.tar.gz` (17,799,838 bytes, `sha256:d065dfb7…`), the download
matched that digest, the archive unpacked to 50 files and 10 symlinks and flattened to
one level, and `llama-server --version` answered `version 0.6.0-dev (build 11534, commit
50e3e3e48)`. Started against a real GGUF it reported `starting` and then `running` on
`http://127.0.0.1:46815` in under a second, `/health` answered `200 {"status":"ok"}`,
the app's own provider listed the model as `state=loaded`, and the chat path was refused
with `the current context does not support logits computation` — the right answer for the
embedding-only model that run used, and proof that the request reached the server. `Stop`
left no process behind (checked with `kill -0`), the status went back to `stopped` with no
PID, and a second start came up on a different port (`43567`) and stopped cleanly again.
A day later the release had moved to `b11535`; the same sequence ran green against it,
which is what the number in this paragraph is doing here — the tag moves, the behaviour
does not.

**A real chat on a model KiChat started itself.** Not a fixture and not an embedding
model: `llama-server` `b11535` was installed by the installer, started on
`LFM2.5-1.2B-Instruct-Q8_0.gguf` (1.25 GB, taken from the folder LM Studio's setting
names), and answered through the app's own provider — `starting` → `running` in seconds,
`/health` 200, the listing reporting `loaded` with a 4,096-token window, and a turn back
in 5.4 s with `prompt=33 completion=26` plus llama.cpp's own timings (`6.2 tok/s`, ttft
1366 ms, `source llamacpp`, `stop`): *"The Mariana Trench is the deepest sea, reaching
about 36,000 feet (10,994 meters) deep."*

**Context inspector** — opened from the UI, showing the segment breakdown:
System-Prompt 1,067 + Tool-Schema 835, response reserve 1,874, limit source `Backend`,
projection 15%.

Note on LM Studio: its streamed responses do not report tokens/s, so those figures come
from the client-side estimator and are labelled `geschätzt`. This is correct behaviour,
not a bug — the fallback exists precisely for this case.

### Guards added after the pass above

These were checked with `go test ./...`, not against a running app, and are recorded
separately so the next full build pass can confirm them in situ rather than reading this
section as evidence it did not gather.

- **The scraper re-guards the host on every redirect hop**, and again on the address it
  actually dials. Before this, the guard covered only the URL that was named, so a page
  could redirect the scraper into `127.0.0.1`, the local network or a cloud metadata
  endpoint and have the response handed to the model. `TestScrapeRedirectHopIsGuarded`
  and `TestPrivateDialControl` fail on the previous code.
- **Transient failures are retried** — 429, 5xx and transport errors get two further
  attempts with a doubling backoff before a turn fails. Only a request whose response
  has not yet been handed to a caller is retried, so a stream is never replayed. This is
  what `HTTPError.Retryable` was written for and never used.
- **Credentials are redacted from backend error bodies**, which some gateways echo back.
  Coverage is the LLM client's credential headers; the Brave client builds its own
  requests, so see the limitations below.
- **Compaction deletion is real.** Deleting a compaction now drops the later ones
  summarised on top of it, which the code previously claimed to do while discarding the
  list it had just loaded.
- **"Nothing to compact" is an outcome, not an error.** A manual compaction over a
  conversation with nothing to fold returned `keine Kompaktierung erstellt`, which the
  window showed as a failure. `Compact` now returns a `CompactResult` whose `Created`
  flag says what happened, so the UI can say "nothing to compact" — and the same flag
  stops a no-op run from handing back an *older* compaction as its own result, which a
  caller could not tell apart from having compacted.
- **No API slice is nil.** `chat.UnresolvedVars` returned a nil slice when every
  variable was known, which marshals to JSON `null` rather than `[]`; the prompt editor
  calls `.length` on it, so opening the system-prompt tab threw and tripped the error
  boundary. The remaining API-visible slices already returned empty slices, and this one
  now does too. Both faults are pinned by tests rather than by inspection.
- **…and it happened again, which is the point of the rule.** `GetRuntimeStatus` built
  its rows with `append` into a nil slice, so on a machine with no managed model
  configured — every fresh install — `status.models` arrived as `null`. The local-model
  section reads it with `.find`, so opening Settings → Backends threw once and took the
  whole view to the error boundary (`TypeError: Cannot read properties of null (reading
  'find')`), which is what the report showed. The rows now come from
  `runtimeStatusBase`, whose slices are never nil and whose JSON shape is pinned by
  `runtime_status_test.go`; `PublishedAccelerators` answers an empty list on a platform
  with no build for the same reason. The panel also reads the arrays defensively now, so
  a shape mistake costs one row instead of the window — but the fix is the backend, and
  the test is the guard. The harness did not catch this: its fixture always has a managed
  model configured, so the idle answer the crash needs was never rendered there.
- **The interface theme is stated, not implied.** The stylesheets declared no
  `color-scheme`, so WebKitGTK painted native form controls in its own default (light)
  scheme while the app's text stayed near-white — the settings dropdowns rendered as
  empty boxes. WebKitGTK ignores a `background` on a `<select>`, so no amount of control
  styling fixed this; `color-scheme` per theme does. Verified by rendering the built
  bundle in WebKitGTK 4.1 itself (`.freebuff/webkit_check.py`), which is also the only
  way to catch a fault that does not reproduce in the Chromium harness.

### Testing notes worth keeping

Reaching the native window from a script needs care on this X session: `wmctrl` reports
the KiChat window ~32px above its true client area, so a coordinate measured in a
screenshot must be clicked at `(x, y + 32)`. Synthetic clicks also need a `mousemove`
immediately before the button press, and typing only lands once the window is focused.
An early test run accidentally hit "Dateien hinzufügen", which opens a modal GTK file
dialog that silently swallows all further input — if automated input stops working, look
for that dialog before debugging anything else.

---

## 5. Postmortems

### 5.1 The blank window (the bug the user reported)

**Symptom:** pressing "Neuer Chat" left the entire window empty — sidebar included.

**Cause:** two React faults stacked in `ChatView.tsx`, each hiding the next.

1. `React #310`, *Rendered more hooks than during the previous render.*
   `const pending = useStore(pendingAttachments)` sat **after** two conditional returns
   (`if (!activeChatId) return …`, `if (chatLoading && !chatState) return …`). The render
   with no chat selected ran 11 hooks; the render after New Chat ran 12. React unmounts
   the whole tree when the hook count changes, which is why the sidebar vanished too.
2. `React #185`, infinite update loop — revealed only once #1 was fixed.
   `pendingAttachments` returns `.filter()`'s brand-new array on every call, so
   `useSyncExternalStore` saw a fresh snapshot each render and span forever.

**Fix:** hoist the selector above both early returns and stabilise it with
`useShallow`. Both constraints are recorded in a comment, because either one being
"simplified away" reintroduces the same blank window.

**How it was found:** window screenshots looked blank, which was first misread as a
stale compositor buffer. A temporary overlay in `index.html` that painted
`window.onerror` / `console.error` into the DOM produced `Minified React error #310`,
then `#185`. The overlay was removed afterwards; its absence is verified in both the
source and the shipped bundle.

**Lesson:** a render error blanks the whole app with no message. This is the single
strongest argument for the error boundary in the roadmap below.

### 5.2 Corrections made along the way

- `research.pdfExtractor` had to be a **type alias** (`= func(...)`) for
  `ingest.PDFExtractor` to remain assignable.
- `BackendConfig` had no JSON tags, generating unusable `ID`/`BaseURL` names; camelCase
  tags were added and the bindings regenerated.
- The German default prompt contained two English or Chinese fragments
  (`bei明确要求`, `liveen Daten`); both were corrected, and a hardcoded location was
  replaced with the `{{location}}` and `{{datetime}}` template variables.
- The Vite template's placeholder document title was replaced with `KiChat`, and the
  window was set to 1440x900 with a minimum of 900x600 and a `#0d1017` background
  matching the theme.
- The first attempt to fix the clone-build failure (declaring the Tailwind native binary
  as an optional dependency) **did not work**; it was reverted with the lockfile restored
  byte-for-byte, and the real cause documented instead. See §6.

---

## 6. Build and environment

```sh
wails build -tags webkit2_41   # when only webkit2gtk-4.1 is available
wails build                    # when webkit2gtk-4.0 is available
```

- `-tags webkit2_41` is **mandatory** on systems shipping only `webkit2gtk-4.1`
  (Ubuntu 24.04+, Linux Mint 22+). A bare `wails build` fails there.
- A bare `go build ./...` on a fresh clone fails with
  `pattern all:frontend/dist: no matching files found`, because `main.go` embeds that
  directory. The frontend must be built once first — expected, not a packaging bug.
- **Node 20.19+ or 22.12+ required** for a clean install and for `wails dev` (Vite 7
  calls `crypto.hash`, which Node 18 lacks).
- On Node 18 / npm 9 a clean `npm install` silently skips
  `@tailwindcss/oxide-linux-x64-gnu`, and Vite then fails with *Cannot find native
  binding*. Root cause: the package's registry metadata carries `libc: "glibc"`, npm 9
  cannot evaluate `libc`, and it reports a matching binary as an
  `UNMET OPTIONAL DEPENDENCY`; libc-aware optional resolution only arrived in npm 10.
  Neither declaring the package in `package.json` nor regenerating the lockfile helps
  (both tested). The working route on Node 18 is
  `npm i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3` inside `frontend/`.

---

## 7. Repository

- Remote: `sshd@192.168.178.27:/shares/enarkry/git_repos/kiChat.git`, branch `master`,
  tracking `origin/master` configured in repo-local git config only.
- 84 tracked files (85 once this document is committed, since it is not in the commit
  that number was measured against). `frontend/node_modules`, `frontend/dist`, `build/bin`,
  `frontend/package.json.md5`, `.freebuff/` and stray `*.db` are ignored — a clone was
  checked to confirm all sources arrive and no build artefacts do.
- No credentials are committed; keys live in `~/.config/kichat/secrets`.

---

## 8. Known limitations

- **Ollama is unverified at runtime.** No Ollama server was running, so that path is
  written against the documented contract and covered by unit tests only.
- **Brave Search needs a user key.** Without one the DuckDuckGo fallback is used; the
  Brave path itself has not been exercised live. Brave is metered (~1,000-1,250 searches
  per $5 of monthly credit), which is why fallback matters.
- **The DuckDuckGo fallback can be refused by an anti-bot check.** Both scraped HTML
  endpoints answer a suspected client with HTTP 202 and a challenge page instead of
  results, and nothing in the request shape changes that (measured against the live
  endpoints). The provider now reports this as a block rather than an empty search, but
  it means a user without a working Brave key can end up with no search at all until the
  check clears or another provider is configured.
- **OCR is out of scope.** `pdftotext`/`pdfinfo` are used; `tesseract` is absent, so
  scanned PDFs will not yield text.
- **`scrapeIntoChat` has no UI.** The Go binding exists but the planned SourcesPanel was
  never written, so scraped pages can only be reached through the tool loop.
- **Secret redaction is partial.** It covers the credential headers on the LLM client.
  The Brave client builds its own request, so a key echoed back by that endpoint would
  reach the UI unredacted; Brave's documented rejection body does not echo the key, so
  this is a gap in the guard rather than an observed leak.
- **The LM Studio folder is read out of an unofficial file.** Its location comes from
  `downloadsFolder` in `~/.lmstudio/settings.json`, which is that application's private
  state and can change shape between releases. Nothing breaks when it does — the hint is
  dropped and the platform defaults are offered instead — but a user who moved their
  models with a future LM Studio would have to point the picker at the folder once by
  hand.
- **A vision model's projector is paired from one folder, and a tie is left to the
  user.** The scanner still skips a model projector — any GGUF whose file name carries
  `mmproj` as a token, whether it is `mmproj-<model>-F16.gguf` as LM Studio names it or
  `<model>-mmproj-BF16.gguf` as it comes off Hugging Face — because `llama-server`
  cannot load one as a model (it comes back with "the current context does not support
  logits computation"); the token rule replaced a prefix test that offered the Hugging
  Face spelling, which is what the PrismML ternary model's own folder holds. The
  projector is now found and passed by the app instead (LLAMACPP.md §3.12), in the
  model's own folder only — and only for a path that names a folder at all, since a bare
  file name would be looked for in the directory KiChat happened to be started from. The
  two names are compared with the case folded away and the extension stripped, because the
  halves are routinely uploaded by different hands; both rules came out of a review of the
  matcher, not out of taste. So a projector kept somewhere else, with the two halves of
  the model downloaded apart, has to be named in the field that exists for it, and two
  projectors that fit one model equally well leave the row warning rather than picking
  one — the app will not guess at a vision tower, and a wrong one is worse than none.
  Both rows of the measurement behind this are a live test
  (`TestLiveProjectorIsWhatMakesAVisionModelSee`), so the table cannot drift from what a
  start really does.
- **A model that cannot see is still offered images.** `--mmproj` and the projector
  field decide whether the server has a vision tower, and the provider reads the answer
  its own way: `/props` `modalities.vision` types a projector-less start as text-only
  (LLAMACPP.md §3.12). Nothing holds an attachment back on that basis — the chat context
  attaches an image to the turn whoever the model is, with a warning from the ingest
  layer as the only notice. With a projector present this is invisible; without one the
  backend's own 500 is what the user reads.
- **The fork is a third party's build, and the app installs it whole.** Selecting PrismML
  as the source downloads binaries published by that project rather than by llama.cpp's
  own maintainers, from a fork that tracks upstream but can lag it, restructure its
  releases or stop. Nothing here checks those binaries beyond the sha256 GitHub reports
  for the asset, and the version and device probes only describe the launcher to itself.
  A user who does not want that can name their own `llama-server` instead, which overrides
  both sources.
- **A ternary model was measured, on the GPU, against the CPU.** `Ternary-Bonsai-2-27B-PQ2_0.gguf`
  (6.71 GB, from the LM Studio folder's `prism-ml/` directory, whose `mmproj-BF16` projector
  the scanner skips) loaded through the app's own supervisor on the fork's
  `prism-b10770-6684606` builds, with everything offloaded and the trace level the app asks
  for: **Vulkan** reported 65/65 layers on `Vulkan0`, ready in 4 s, `/props` `n_ctx=4096`, a
  real turn with a coherent answer and `system_fingerprint: b10770-6684606ac`, at **53.3
  tok/s** (11,087 MiB of the card free while loaded); **ROCm 7.2** reported 65/65 on `ROCm0`,
  ready in 2 s, **57.0 tok/s**, 12,580 MiB free. The same file on the same Vulkan launcher
  with `--gpu-layers 0` — what the app's `off` mode sends — answered at **4.6 tok/s** and
  reported `0/65 on Vulkan0`, so the offload is worth 11–12×, and a backend whose ternary
  kernels were missing would have produced that CPU number with the layers merely moved.
  What is still unmeasured is how a ternary model *should* be sampled: a managed model
  carries a context size, flash attention, a KV cache type and free-form extra arguments,
  but no per-model sampler, so the global settings apply.
- **The placement reading names one card, and the split case is unresolved.** A machine
  with several cards of one backend splits the layers across them, and llama.cpp prints a
  `using device` line per card while the layer line remains a single summary — so the
  panel's `On Vulkan1: 34 of 34 layers on the GPU` attributes a model-wide count to one
  device of several. The half that matters is still right (a named device means the
  layers are on GPUs, no name means they are on the CPU), and this machine has one card
  per backend, so the reading was not measured on a split. Carrying the device list
  across instead of one id is what would settle it.
- **A managed llama-server can outlive a hard kill.** A clean quit stops every server it
  started, and each runs in its own process group so the shutdown reaches the whole tree.
  A `SIGKILL` to the app, though, leaves the server running with its model in memory and
  no row left to stop it — nothing here reparents the child (a `Pdeathsig` would be the
  way on Linux, and Windows would need a job object). The servers are on the loopback
  interface only, so what is lost is memory, not exposure.
- **A managed server accepts requests from anything on the machine.** `llama-server`
  starts without an API key and, as its own log says, with CORS allowing all origins, so
  another local process — or a web page in the user's browser — can use the model while it
  is up. Chatting with a local model is what the feature is for, which is why this was not
  closed by generating a key the entry then has to carry; it would need `--api-key` plus
  the key stored like every other secret, and the readiness probe re-checked against a
  keyed `/health`.
- **A fresh clone does not build on Node 18 / npm 9** (see §6). This is a toolchain
  limitation rather than a defect in the repository, but it does mean the repository
  alone is not sufficient to build on the machine it was developed on.
- No CI, so none of the above is enforced automatically.

---

## 9. Roadmap

Ordered by value, not effort. Shipped items are deleted rather than left to rot: the
**error boundary** that was #1 here is in `App.tsx`, wrapping the view switch, and the
event-name constants now live in `internal/events` so a mismatch fails the build.

1. **CI on a clean clone** with Node 22 and `-tags webkit2_41`, running `go vet`,
   `go test`, `tsc --noEmit` and a full build. This converts the build recipe and the
   native-binding trap into a red build instead of a discovery. It is now the highest
   value item on this list by a wide margin: without it, the frontend cannot be checked
   automatically at all, and `go vet ./...` cannot run on a clone that has not been
   built yet.
2. **Move the development toolchain to Node 22.** Fixes the clone build, restores
   `wails dev`, and removes the recurring `--no-save` step.
3. **Give the binding layer tests.** `app.go` and `api.go` are ~1,000 lines with no test
   file, and they are the surface the UI actually calls. The event mismatch fixed above
   is exactly the class of defect a test there would have caught. Either commit a
   placeholder `frontend/dist` so `package main` compiles for testing, or move the
   bindings behind an `internal/app` package.
4. **Localise the backend's user-facing text.** All UI-facing errors and warnings are
   German literals emitted from Go, so the English and Chinese UI show German toasts. The fix is
   for Go to emit stable codes and let the frontend translate, keeping today's strings
   as the `de` entries. Tool output and seeded search text are prompt content for the
   model, not UI text, and should stay as they are.
5. **Semantic retrieval over ingested documents.** FTS5 BM25 exists and documents are
   already chunked; embeddings (the local `text-embedding-nomic-embed-text-v1.5` is
   already available) would enable "ask a question across the whole library".
6. **Citation-fidelity checker.** Nothing currently verifies that a claim appears in the
   cited source. A post-generation pass over the fetched text would make research mode
   trustworthy rather than merely plausible.
7. **MCP client**, so tools come from the ecosystem instead of being hardcoded.
8. **Multi-model arena** — one prompt across several local models, side by side.
9. **Model download inside the app.** The picker now lists the GGUF files a user already
   has — LM Studio's folder first — so nobody has to fetch a file twice, but a *fresh*
   machine still has nothing to list. A search over Hugging Face's GGUF repos, a
   digest-checked download into `~/.config/kichat/models`, and the same progress row the
   installer already has would close the last gap between "install KiChat" and "chat with
   a local model".
10. **Automatic model fallback** when a backend is down or the context does not fit.
11. **Cross-chat memory** backed by the same FTS index.
12. **SourcesPanel UI** for the existing `scrapeIntoChat` binding.
13. Vision support for the VLM models already listed by LM Studio. The half that
    accepts an image is already there — a picture is an image attachment, and the
    assembly turns it into an image part (IMAGES.md §4); what is missing is the UI
    that lets a user drop one in for a model that can read it.
14. More image backends behind the same `images.Provider`: ComfyUI (a workflow graph,
    then poll `/history` and fetch `/view`) and a hosted OpenAI-style
    `/v1/images/generations`. The interface has no HTTP in it, which is what makes
    this an addition rather than a rewrite.
15. Remaining exploratory ideas: document generation, model benchmarking, branching
    conversations, and scheduled watch tasks.

---

## 10. Verification checklist

Re-run after any change; the exit status must be checked, not just the output.

```sh
gofmt -l .                                # expect empty
go vet ./...                              # expect 0
go test ./...                             # expect all packages ok
KICHAT_LIVE=1 go test ./internal/...      # live backend; asserts measured promptTokens
cd frontend && npx tsc --noEmit           # expect 0
wails build -tags webkit2_41              # expect a binary in build/bin
```

Then open the app and confirm by hand: create a chat, send a message, watch the context
meter report a **measured** token count, and confirm the reply renders with its stats
row. The most recent full pass of this list, including the GUI step, is recorded in §4.
