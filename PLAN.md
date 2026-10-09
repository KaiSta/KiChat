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
| Backends | LM Studio (`:1234`) and Ollama (`:11434`), plus DeepSeek and llama.cpp (`:8080`) shipped prepared-but-off, plus a generic OpenAI-compatible endpoint |
| Context | Automatic and manual compaction, live context-window fullness meter, per-segment inspector |
| Research | Brave Search (API key) with DuckDuckGo fallback, page scraping with SSRF guard, optional search-before-answer |
| Files | Attachments, PDF/office text extraction, chunked with BM25 retrieval over the chunks |
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
    httpclient.go    Shared client, retries, timeouts
    openai.go        LM Studio / OpenAI-compatible SSE streaming
    ollama.go        Ollama NDJSON streaming
    deepseek.go      DeepSeek over the OpenAI wire format, with its own error envelope
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
    context.go       Prompt assembly and ContextBudget with measured-vs-estimated accounting
    compact.go       Compaction (summary generation, never destructive)
    prompt.go        Templating including tool-list substitution
    preview.go       Token prediction before sending
    maintenance.go   Auto-title, background upkeep
    searchfirst.go   Search-before-answer seeding
  research/        search.go, chain.go (Brave -> DuckDuckGo), brave.go, duckduckgo.go,
                   scrape.go (SSRF + robots guards), util.go
  ingest/          ingest.go, pdf.go (pdftotext), office.go (libreoffice), chunk.go
  tools/           Tool definitions and dispatch
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
9. **Automatic model fallback** when a backend is down or the context does not fit.
10. **Cross-chat memory** backed by the same FTS index.
11. **SourcesPanel UI** for the existing `scrapeIntoChat` binding.
12. Vision support for the VLM models already listed by LM Studio.
13. Remaining exploratory ideas: document generation, model benchmarking, branching
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
