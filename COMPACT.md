# COMPACT.md

How context compaction works in KiChat: the strategy, the arithmetic behind it, and the
failures each rule exists to prevent. `PLAN.md` §3 states the one-line intent ("Compaction
never deletes messages"); this document is the detail behind that sentence.

Everything here was read out of the code and, where marked *measured*, observed on a
running build. Claims that are design intent rather than verified behaviour say so.

**Contents:** 1 problem · 2 invariants · 3 data model · 4 entry points · 5 planning ·
6 summariser choice · 7 budgeting the summarisation call · 8 the transcript ·
9 the prompts · 10 reasoning models · 11 storage, events, UI · 12 settings ·
13 failure modes · 14 how this is verified · 15 known gaps · 16 where the code is.

The code references are `file:line` for the revision this was written against; the symbol
names next to them (`PlanCompaction`, `summariser`, `expressibleOnNative`, …) are the durable
anchor when the numbers drift.

---

## 1. The problem compaction solves

A research chat grows without bound: questions, answers, search payloads, tool results,
inlined documents. Eventually the assembled request no longer fits the model's context
window, and the local backend answers with an error or a silent truncation.

The three obvious solutions are all wrong for this app:

| approach | why it is rejected |
| --- | --- |
| Delete old messages | Destroying the user's own text to save context is unacceptable in a research tool |
| Truncate to the last N tokens | The newest tokens are not the most relevant ones — the research question from an hour ago usually is |
| Send everything and let the backend truncate | The truncation point is invisible; the user is told nothing about what the model can no longer see |

So compaction **folds** a prefix of the conversation into a written memory document and
leaves everything else alone. The folded messages stay in the database; the memory is a
*view* over them, not a replacement for them.

---

## 2. The invariants

Every other decision in this document follows from these six.

1. **Never destructive.** Messages are never deleted or rewritten. `store.Compaction`
   records a `FromSeq`/`ToSeq` range; the rows in `messages` are untouched, and the
   summary is an added reading instruction. Undo is therefore complete, not best-effort.
2. **One cumulative memory, merged — not re-summarised.** A fold always starts *after* the
   end of the newest existing compaction and is asked to merge the existing memory into
   one document. The active memory is always exactly one merged document, no matter how
   many times the chat has been compacted. This is what keeps compaction cost **linear**
   in the number of messages instead of quadratic (`context.go:117`, `compact.go:544`).
3. **Cut only at user-turn boundaries.** A cut between a tool call and its result produces
   a request the backend rejects, and a prompt that begins with an assistant message is
   malformed. So the planner's cut points are user messages only (`compact.go:154`).
4. **The memory is system content, never a fake user turn.** It is folded into the one
   leading system message, delimited by `<conversation_memory>…</conversation_memory>`
   (`context.go:217`). Faking a turn would put words in the user's mouth in the transcript
   they read back. It shares the prompt's message rather than travelling beside it because a
   strict template — the Qwen3 one the ternary models use — reads `messages[0]` as the
   system prompt and raises *"System message must be at the beginning."* for any other
   system message. A second leading one made every turn after a compact fail; joining them
   keeps the memory a system message and the request renderable.
5. **A request that does not fit the window is the one failure that must not happen.**
   This applies twice: to the chat request (that is what compaction is for) and to the
   summarisation request itself, which is priced so that prompt + transcript + worst-case
   output provably fit (`maintenance.go:198–232`).
6. **No silent no-op, no silent fallback.** "Nothing to compact" is a reported outcome
   (`CompactResult.Created`), not an error and not a success. A configured summary model
   that cannot be honoured is an error, not a fallback to the chat's model. A summary
   failure names its cause and the knob that fixes it.

### The design tension this accepts

Invariant 5 (always fit) is ranked above memory completeness. On a small window — an 8k
model with the default output cap — the reserve that guarantees the request fits also
squeezes the transcript allowance down to roughly its 1024-token floor, so the memory is
written from heavily trimmed history. That is the deliberate trade: a shorter, honest
memory rather than an overrun request. See §7.

---

## 3. Data model

One row per fold, in `compactions` (`internal/store/models.go:127`):

| field | meaning | written by |
| --- | --- | --- |
| `ID`, `ChatID` | identity, cascade-deleted with the chat | `compactWith` |
| `Trigger` | `auto` \| `manual` — how the memory came to exist | `triggerName(opts.Force)` |
| `FromSeq`, `ToSeq` | the folded range; `ToSeq` is the fold boundary the transcript renders at | planner |
| `Summary` | the memory text itself | summariser |
| `Focus` | the user's optional preservation instruction, kept so the card can show it | `CompactOptions.Focus` |
| `TokensBefore` | **estimated** tokens of the folded messages (content + reasoning/2) | `plan.TokensBefore` |
| `TokensAfter` | **estimated** tokens of the stored summary | `EstimateTokens(summary)` |
| `Model`, `Backend` | **the model that actually wrote it** — not necessarily the chat's own | summariser resolution |
| `CreatedAt` | for the history list | store |

`Model`/`Backend` are recorded because a memory written by a different model than the
chat's own is a fact worth being able to see (`maintenance.go:271`).

Per-chat compaction settings live on the `chats` row (`models.go:32–34`):
`AutoCompact`, `CompactThreshold`, `KeepRecentTurns`.

---

## 4. The two entry points

Both go through **one function**, `Engine.Compact` (`maintenance.go:54`) → `compactWith`
(`maintenance.go:102`). Only `Force`, `Target` and `Focus` differ.

### Automatic

`maybeAutoCompact` (`maintenance.go:86`) runs in the send pipeline, after search seeding
and **before** the turn is generated (`PLAN.md:85`), never mid-stream. It is gated by
`settings.Compact.Enabled` **and** the per-chat `AutoCompact` (`maintenance.go:89`). It passes
an empty `CompactOptions`, so `Force` is false — the planner's threshold decides — and
`Target` is 0. The created compaction is discarded: an automatic run has nobody to report back to,
and "nothing to fold" is already its silent outcome.

**Silence extends to the window nobody reports.** A model whose context window is unknown cannot be
compacted against at all — the threshold is a ratio of that window — but that is a standing fact
about the model rather than an incident, and this function runs before *every* send. So it returns
before calling `compactWith` when the limit resolves to zero, rather than forwarding the refusal:
forwarding it put the same warning in front of the user once per message for as long as the model
stayed undescribed. Measured on the NVIDIA backend, whose listing reports no window for any model,
and therefore on every turn of every one of its chats. The condition stays visible where it belongs
— the context meter reads `unbekannt` and the inspector names the source (`limitSource: "unknown"`)
— and the manual path keeps the refusal with its reason, since there the user asked. Pinned by
`TestAutoCompactStaysQuietWhenTheWindowIsUnknown` and `TestManualCompactStillRefusesAnUnknownWindow`.

### Manual

`App.CompactNow` (`api.go:335`) is the bound RPC behind the UI buttons. It passes
`Force: true` and:

- a **15-minute** context timeout. Measured: five minutes was not enough — a large
  transcript on a CPU-hosted local model was still producing tokens when the deadline cut
  it off, reported as `context deadline exceeded`. The output is bounded elsewhere (§7),
  so this is a backstop against a stuck backend, not a budget the work has to fit in.
- a `Focus` string and a `Target` ratio (0 = "as much as possible", 0.6 from the
  "→ 60 %" and "Compact to fit" buttons, 0.4 from "→ 40 %").
- optional `FromSeq`/`ToSeq` to pin the range explicitly (`maintenance.go:139`).

It emits `events.ChatsChanged` only when `Created` is true.

### Refusals, before any token is spent

| condition | result |
| --- | --- |
| a turn is running for this chat | refused: *"für diesen Chat läuft noch eine Generierung — kompaktieren ist möglich, sobald sie fertig ist"*. The turn owns the newest messages and writes into them as tokens arrive, so compacting now would freeze half an answer into the memory. The automatic run is exempt because it is triggered by that turn |
| no model selected | `kein Modell ausgewählt` |
| the chat's backend is not in the registry | `Backend %q ist nicht konfiguriert` |
| the window is unknown | `Kontextfenster des Modells ist unbekannt` — **manual only**; the automatic run returns silently before reaching this, because it fires on every send and has nobody to report to (§4) |
| nothing foldable | `Created: false`, **no summariser call at all** |
| a configured summary model is on a missing backend | error naming model and backend (§6) |

The UI also disables the buttons while a chat is busy and shows `compact.busy` instead of
letting the click be the thing that discovers the refusal.

---

## 5. Planning: what gets folded, and where the cut lands

`PlanCompaction` (`compact.go:110`) is pure: messages + compactions + budget in, a
`CompactPlan` out. It decides the range, the cut point, the counts and the reason string.

### 5.1 The gate

Without `Force`, the plan is empty unless `Budget.ProjectedRatio ≥ Threshold`. The reason
string records the decision, e.g. `Kontext bei 83% (Schwelle 80%)`, or
`manuell ausgelöst` for a forced run. The threshold is measured against the **projected**
next request (which includes the response reserve), not the last one — it must fire
before the request that would overflow, not after it.

Effective values, most specific first (`maintenance.go:354`, `:364`):

| setting | per-chat field | global field | fallback |
| --- | --- | --- | --- |
| threshold | `chats.compact_threshold` (0 < v < 1) | `Compact.Threshold` | 0.80 |
| turns kept verbatim | `chats.keep_recent_turns` | `Compact.KeepRecentTurns` | 6 |

### 5.2 The range

```
fromSeq  = (largest existing compaction.ToSeq) + 1   → clamped to the first message
keepFrom = the user message that leaves keepTurns turns verbatim above it
toSeq    = keepFrom − 1
```

Two rules make this well-behaved:

- **Never before the first message** and **never re-fold an already-folded range.** This
  is what implements invariant 2: folding resumes where the last memory stopped, so its
  cost is proportional to *new* history rather than to total history.
- **From and to are cheap to explain** — the card shows `#from–#to`, and the active
  memory's budget segment says `Zusammenfassung der Nachrichten from–to`.

### 5.3 The forced-short-chat rule

When the cut lands before `fromSeq` (the chat holds fewer turns than `keepTurns`), three
outcomes exist — they fire in this order (`compact.go:167–183`, then `:223–232`):

- **automatic**: `Needed: false` with reason `nichts zu komprimieren` (or the existing reason
  plus `— aber kein Verlauf zum Komprimieren`). Nothing is worth doing.
- **manual, first fallback**: fold up to the **newest user turn** (`compact.go:177–183`) —
  the last turn that could contain a tool exchange that must not be split. The comment names
  the motive: *"A manual compaction is a promise that the context gets compacted, so it must
  never end as a silent no-op while foldable history exists."*
- **manual, last resort**: if even that produces no cut, extend the cut to the **end of the
  chat** (`compact.go:223–232`). This is the state right after a previous compaction plus one
  new turn: every older turn is already folded and only the newest is kept verbatim, so the
  button would otherwise say "nothing to compact" forever. *"the end of the chat is always a
  valid cut point — nothing follows it, so no tool call is separated from its result."*
  Pinned by `TestManualCompactAlwaysFoldsWhenOnlyTheNewestTurnIsLeft`,
  `TestManualCompactionOnShortChatFoldsWhatExists`,
  `TestManualCompactionOnSingleTurnFoldsIt`,
  `TestManualCompactionOnlyNoopsWhenEverythingIsAlreadyFolded`.

### 5.4 Manual targets: "→ 60 %" means 60 %

With `Target > 0` the cut is **walked outwards from the keep-turns boundary** — folding one
turn per step, from the oldest kept turn towards the newest — until the estimate fits
`Target × Limit` (`compact.go:191–221`). The walk has to price the cut it is about to
install: the loop this replaced counted `i` backwards from the newest turn while folding
forwards with the same `i`, so its figure and its cut described two different states. The
cut could then be missed by a whole window in either direction — a 30 % target landing at
60 %, a 50 % target folding *fewer* turns than the 60 % one, or a bottom target stopping at
seven kept turns instead of the newest one. Three details make the promise honest:

- **The reserve counts towards the target.** The meter's `projected` number includes it,
  and the auto threshold reads that projected ratio — so pricing the manual target without
  the reserve would make the same percentage mean two different window states.
- **Each candidate cut point is priced as a whole turn span** — the user message, its
  answer and the tool traffic up to the next user turn (`turnTokenSpans`, `compact.go:327`).
  Counting only the user message makes the estimate believe it has folded less than it
  has, and it then folds turns it did not need to.
- **Attachments count towards the target** (`in.Budget.Attachments`). Inlined file text and
  image payloads sit inside the region the cut keeps; leaving them out priced the kept
  history too cheaply, and a chat carrying a 12 000-token file landed 22 points over its
  60 % target.
  Pinned by `TestPlanCompactionTargetFoldsToTheCutItPrices` (the resulting ratio, and that
  no less folding would have fitted) and `TestPlanCompactionTargetCountsAttachments`, both
  of which are sharper than `TestPlanCompactionForcedTargetFoldsMore`'s monotonicity — the
  old walk was non-monotone, so asserting *more* folding below a higher target was not
  enough.

### 5.5 The streaming guard

A message with `Status == streaming` is being written right now: its content is half an
answer and it is replaced in place when the turn finishes. Folding it would freeze that
fragment into the memory permanently, so the cut is pulled back before the turn that owns
it — and if that turn has no user message in front of it, **no cut can be proven safe and
nothing is folded** rather than guessed (`compact.go:297`). This also keeps the tail of the
prompt from beginning with an assistant message.
Pinned by `TestPlanCompactionNeverFoldsAStreamingMessage`.

### 5.6 How the fold is priced

```
TokensBefore        = Σ over folded messages ( EstimateTokens(content) + EstimateTokens(reasoning)/2 )
TokensAfterEstimate = min(MaxSummaryTokens, TokensBefore / 2)   → clamped to ≥ 1
```

Reasoning is counted at **half weight** throughout the planner: it is context, but it is
not the conversation. The `TokensBefore / 2` clamp exists so a fold can never be reported
as saving nothing — *"a summary we instruct the model to keep terse should not be budgeted
as larger than what it replaces, or compaction would appear to save nothing"*
(`compact.go:272`).

`TokensBefore`/`TokensAfter` are estimates. The context meter's `measured` figure is
separate and authoritative (§11).

### 5.7 Tests

`TestPlanCompactionCutsOnlyAtUserBoundaries`, `TestPlanCompactionDoesNothingBelowThreshold`,
`TestPlanCompactionFoldsOldTurnsAndKeepsRecentOnes`,
`TestPlanCompactionResumesAfterPreviousCompaction`,
`TestPlanCompactionWithTooFewTurnsFoldsNothing`,
`TestPlanCompactionNeverFoldsAStreamingMessage`,
`TestPlanCompactionForcedTargetFoldsMore`.

---

## 6. Choosing the summariser

`summariser()` (`maintenance.go:310`) resolves *who writes the memory*, and — separately —
*which window the summarisation call has to fit in*.

| case | provider/model | `Limit` priced against | `NumCtx` |
| --- | --- | --- | --- |
| `Compact.SummaryModel` empty | the chat's own | the chat's window | the chat's override |
| configured, identical to the chat's own | the chat's own (nothing to resolve) | the chat's window | still applies |
| configured, a different model | that model, on its own backend | **its own** `ContextLimit` | **not** sent |
| configured, backend missing | error: *"das Zusammenfassungs-Modell %q gehört zum Backend %q, das nicht konfiguriert ist"* | — | — |
| configured, window unknown | that model | the chat's window + an `events.Warning` naming the model | not sent |

The per-chat `NumCtx` override belongs to the chat's own model and is not carried to a
different summariser. The warning in the last row exists because the alternative is a
summary silently written through a different budget than the one that was configured.

Why a separate summary model is offered at all: a reasoning model can spend the whole
summary budget thinking and write nothing, and not every backend lets thinking be switched
off. A small instruction model sidesteps the question. The default stays the chat's own
model because it is already loaded.
Pinned by `TestCompactRunsOnTheConfiguredSummaryModel`,
`TestCompactReportsAnUnconfiguredSummaryBackend`.

---

## 7. Budgeting the summarisation call

A compaction that overflows its own window is a compaction that fails, so the summary
request is priced with the same rigour as a chat turn (`maintenance.go:198–232`).

```
overhead    = EstimateMessagesTokens(system prompt + user wrapper)      ← transcript omitted
outputCap   = max(8192, Compact.MaxSummaryTokens)                       ← default 8192
              trimmed down to: limit − overhead − 1024                  ← summaryMinTranscriptTokens
margin      = limit / 16                                                ← estimator slack
reserve     = outputCap + margin
transcript  = PlanTranscriptBudget(limit, reserve, overhead)
            → Total = limit − reserve − overhead          (floored at 0)
            → Block = Total / 4                           (transcriptBlockDivisor)
maxTokens   = outputCap                                                  ← sent as max_tokens
```

Because `overhead + Total + reserve ≤ limit`, the request provably fits: prompt,
transcript **and** the worst-case output, plus a margin for the estimator undershooting the
backend's real tokenizer.

### Why the cap is 8192, and why it is a cap

`max_tokens` on a reasoning model bounds **thinking and answer together**. A cap sized to
the summary alone (the default `MaxSummaryTokens` is 1200) is spent entirely on the reasoning
and returns an empty summary — the failure the code comment records, and what
`TestSummarizeRetriesWhenReasoningAteTheBudget` and `TestSummarizeExplainsAReasoningOnlyAnswer`
pin. 8192 is the number Cline uses for its own summariser (`compact.go:381`).

It is a **cap, not a target** (`compact.go:379`): *"the model is told to be terse, so it
stops far short of it. The cap only stops a runaway generation from filling the window —
the failure that produced a 20k-token 'memory' for a 13k-token fold."* *

`Compact.MaxSummaryTokens` can only **raise** the default, never lower it. The window can
lower it, and on a window so small that even the minimum transcript does not fit, the cap
collapses towards 1 token: the request fitting is non-negotiable.

### Why the transcript is not a fraction of the window

`PlanTranscriptBudget` (`compact.go:616`) says explicitly what it replaced: a fixed 4000
character cap bounded the wrong thing — *"the same 4000 characters are a modest block for a
128k model and an eighth of the whole window for an 8k one, and being per block it bounded
nothing as a whole"*, so a long conversation could reach a small model as a request larger
than its window. A half-window ceiling was also rejected: it *"threw away half the history
a large model could have read"*. The allowance is simply what the window leaves, and the
reserve is what keeps the transcript clear of it.
Pinned by `TestPlanTranscriptBudgetScalesWithTheWindow`,
`TestPlanTranscriptBudgetUsesTheWholeWindow`,
`TestCompactKeepsTheSummariserRequestInsideItsWindow`,
`TestCompactSummariserCapMatchesCline`,
`TestCompactReservesRoomForReasoningInTheSummariser`.

### The honest consequence on a small window

On an 8k window the reserve is the full 8192 cap, so the transcript allowance is squeezed
to roughly its 1024-token floor even though the model usually spends a tiny fraction of
that cap — measured, 9 output tokens against an 8192 cap. The fold is then summarised from
heavily trimmed history. This is invariant 5 outranking completeness: **a shorter memory
rather than an overrun request.** Lowering the cap via `Compact.MaxSummaryTokens` is not
possible by design; the intended lever for small windows is fewer folded turns
(`KeepRecentTurns`, `Threshold`, or a manual 40 %/60 % target).

\* The 20k-for-13k figure is quoted from the code comment recording the failure that
motivated the cap; the unit tests pin the bound, the figure itself is not re-measured here.

---

## 8. The transcript: what the summariser actually reads

`RenderTranscript` (`compact.go:632`) flattens the folded messages into plain text, bounded
at **two levels**: every message is cut to `Block`, and the finished transcript is cut to
`Total` (both cut with the `…[gekürzt]` marker). Two levels because a per-message cap
bounds each message, not the transcript: *"enough messages still add up past the window of
the model that has to read them."*

| role | rendered as | rule |
| --- | --- | --- |
| `system` (seeded search) | `### Websuche` + one line per source | **distilled, not excerpted** — see below |
| `user` | `### Nutzer` + content, capped | empty messages skipped |
| `assistant` | `### Assistent` + content, plus `(Tool-Aufruf: …)` when a tool was called | reasoning-only turns become `(nur Reasoning)` so the turn is not erased from history |
| `tool` | `### Tool-Ergebnis` + result, capped | — |

**Why search payloads are distilled rather than excerpted.** A seeded search message is a
system message and is usually the largest thing a fold removes, but its page prose is not
what a memory needs to carry. `renderSearchSources` reduces it to number, title and URL per
source — the part a later answer has to be able to cite. It matches the heading with
`^\[(\d+)\]` anchored at column 0 precisely so that a snippet which happens to begin with
`[2]` is not mistaken for a new source, and it stays inside the block budget.
Pinned by `TestRenderSearchSourcesDistilsTheListingFormat`,
`TestRenderSearchSourcesKeepsOnlyRealSources`, `TestRenderSearchSourcesStaysBounded`,
`TestRenderTranscriptCarriesSearchSourcesIntoTheSummary`,
`TestCutToTokensKeepsItsBudgetAndItsRunes`.

---

## 9. The prompts

### 9.1 The memory contract (system prompt)

`summarySystemPrompt` (`compact.go:348`) is deliberately explicit about
preservation: *"a compaction that loses a URL or a number silently corrupts every later
answer."* Its rules, verbatim:

- Write in the language of the transcript.
- Carry names, numbers, dates, prices, legal references and URLs over **WORD FOR WORD** —
  do not shorten or paraphrase them.
- Invent nothing. Anything that stayed unclear goes under "Offene Punkte".
- Be terse: bullets, no prose, no repetition.

and it fixes the output structure, omitting empty sections:

```
## Festgestellte Fakten      ## Entscheidungen        ## Offene Punkte
## Quellen                   ## Dateien & Dokumente  ## Vorgaben & Präferenzen
```

This is a research-memory schema: what was established, what was decided, what is still
open, which sources back it, which files are in play, and the user's standing preferences.

### 9.2 The user message

`summarisePrompt` (`compact.go:538`) is split out of `Summarize` so that the transcript
budget can be measured against *exactly* the text that will be sent — the wrappers, the
existing memory and the focus line all share the window with the transcript and are charged
before its allowance is set. Its shape:

```
<existing_memory>
…previous summary…
</existing_memory>

Der folgende Verlauf schließt an das bestehende Gedächtnis an.
Führe beides zu EINEM Gedächtnis zusammen, ohne Informationen zu verlieren.

Besonderer Fokus des Nutzers: …            ← only when a focus was given

<conversation>
…rendered transcript…
</conversation>

/no_think
```

The merge instruction is the whole of invariant 2 in one sentence, and the `<conversation>`
wrapper keeps the transcript distinguishable from the instructions around it.

---

## 10. Reasoning models: preventing too much output

Summarising is not a reasoning task, and a thinking model can spend its entire output
budget on private deliberation and never write a summary. Five layers address that, ordered
from cheapest to last resort. This is the section the whole `ReasoningOff`/native-route
work was built for.

### 10.1 Do not let it think: three levers

`compactWith` sets `DisableThinking` (`maintenance.go:240`), `Summarize` turns it into
`req.Think = &false` (`compact.go:476`, inside the attempt loop), and the provider layer
fans that out:

1. **The backend's own control.** True when the model advertises `CapThink`, or when the
   provider implements `llm.ThinkingToggle` and reports `CanToggleThinking()`. The field
   that carries it is the backend's own: on Ollama that is `think: false`, on LM Studio
   `enable_thinking: false`, on llama.cpp a `chat_template_kwargs` entry, and on Mistral
   `reasoning_effort: "none"` (MISTRAL.md §5 — sent only for a model the listing reports
   `reasoning: true` for, and retired on a `400`/`422` rather than risked twice).
   Pinned by `TestOllamaHonoursDisableThinking`,
   `TestLMStudioSendsEnableThinkingFalse`,
   `TestCompactDisablesThinkingForAThinkingModel`,
   `TestCompactAsksAToggleBackendNotToThink`,
   `TestMistralAsksAReasoningModelNotToThinkWithReasoningEffort`.
2. **LM Studio's native `/api/v1/chat` route.** With `Think=false` the request may travel
   on the native route with a **`reasoning`** field instead. The value is *the least
   reasoning the model permits*: `"off"` when that option exists, otherwise `"low"` — the
   smallest grade — and **no field at all** when the model publishes no reasoning config,
   because measured, LM Studio answers that with HTTP 400
   `does not expose reasoning configuration`. The capability listing is read **once**, lazily,
   and cached (`openai.go:487`); a 404 retires the namespace permanently, a 400 naming
   `reasoning` retires just that model (`retireReasoning`, `openai.go:551`), and any other
   failure falls through to the OpenAI-compatible path. The shape guard `expressibleOnNative` requires no tools, no
   images, no seed, no assistant turns and at least one user message — a summarisation
   request (system + one user message) is exactly what qualifies, so it takes this route.
   Pinned by `TestNativeRouteSwitchesReasoningOffForASummariser`,
   `TestNativeRouteReportsAnExhaustedBudgetAsLength`,
   `TestNativeRouteIsSkippedWhenReasoningCannotBeSwitchedOff`,
   `TestNativeRouteIsSkippedForRequestsItCannotExpress`,
   `TestNativeRouteRejectionFallsBackAndIsNotRepeated`,
   `TestNativeRouteIsNotConsultedForAnOrdinaryTurn`.
3. **`/no_think` in the prompt, always.** The only lever that reaches a backend with no
   reasoning control at all. A model that does not know the tag treats it as an ignorable
   trailing note. Pinned by `TestSummarizeAsksTheModelNotToThink`.

### 10.2 Bound whatever thinking still happens

`max_tokens = outputCap` (§7), and the cap bounds thinking and answer **together**. This is
why the cap is 8192 rather than the 1200 the summary itself is asked to be, and why it is
a ceiling rather than a target: the model is told to be terse, the cap only stops a
runaway generation from filling the window.

### 10.3 Keep the thinking out of the memory

`stripThinking` (`compact.go:415`) runs **before the answer is judged**:

- `<think>`, `<thinking>`, `<analysis>`, `<reasoning>` blocks are matched case-insensitively
  and across lines;
- a **dangling open tag** — a generation cut off mid-thought — is removed as well;
- what was removed is returned as *thinking*, not as answer, so the stored memory is only
  the answer. A memory consisting of nothing but deliberation would otherwise be stored and
  re-read on every later turn.

Pinned by `TestCompactStripsInlineThinkingFromASummary`,
`TestCompactRetriesAnAnswerThatWasOnlyThinking`.

### 10.4 Retry rules that cannot loop

`summaryAttempts = 3` (`compact.go:399`), and each attempt must earn the next
(`compact.go:466–515`):

| condition | outcome |
| --- | --- |
| `summary != ""` | done — the memory is returned |
| thinking was found | counted as reasoning, so the retry can size against it |
| no reasoning **and** `stop_reason != "length"` | **break** — a model that said nothing will say nothing again |
| `grown <= budget` | **break** — no progress, so retrying is pointless (`compact.go:507`) |
| otherwise | `grown = EstimateTokens(reasoning) + answer` (`compact.go:501`), capped at `ContextLimit − promptTokens` (`:502–505`), adopted as the next `budget` (`:510`) |

The first attempt asks for `answer`, which is `opts.MaxTokens` — the cap sized in §7 — or
1024 when unset (`compact.go:460–464`); `promptTokens` is that request's own estimated prompt
size (`compact.go:458`), which is why the retry cap is expressed relative to it.

The extra budget goes to the **thinking, not to a larger memory**: `answer` (the memory's
own allowance) is unchanged, only `budget` grows.
Pinned by `TestSummarizeRetriesWhenReasoningAteTheBudget`,
`TestSummarizeDoesNotRetryAGenuineEmptyAnswer`.

### 10.5 Fail with the cause named

`emptySummaryError` (`compact.go:520`) distinguishes the two empty outcomes:

- reasoned or hit `length` → *"das Modell hat sein Ausgabebudget beim Reasoning aufgebraucht
  und keine Zusammenfassung geschrieben (stop_reason %q, %d Zeichen Reasoning) — erhöhe
  „Max. Zusammenfassung (Tokens)“ in den Einstellungen"* — which names both the cause and
  the fix.
- otherwise → *"das Modell hat eine leere Zusammenfassung geliefert"*.

Pinned by `TestSummarizeExplainsAReasoningOnlyAnswer`, `TestSummarizeRejectsAnEmptyAnswer`.

### Measured behaviour

On a local reasoning model (`qwen3.8-4b-distill`), one compaction run, thinking **on**:
**67 prompt / 64 output tokens, all 64 of them reasoning, `stop_reason: "length"`, empty
message** — the failure above. With the native route and `reasoning: "off"`: **69 prompt /
9 output, 0 reasoning tokens**, answer *"Lima ist die Hauptstadt von Peru."*

These two figures are quoted from that run rather than re-measured while writing this
document. The test that produces both halves side by side — the prompt with the setting and
without it — is `TestLiveNativeReasoningOffProducesASummary`
(`internal/llm/live_test.go:195`), which is opt-in and needs a loaded LM Studio model; only
the reasoning-off half is asserted, because whether a model thinks past a small cap is a
property of that model.

---

## 11. Storage, events, and what the user sees

### 11.1 The write

After a successful summary (`maintenance.go:258–281`):

1. `CreateCompaction` (`maintenance.go:274`) stores the row (§3) with `Trigger` from
   `opts.Force`, the focus which was used, and `Model`/`Backend` of the model that
   wrote it.
2. `InvalidateMeasurement(chatID)` (`maintenance.go:280`) — the prompt has changed shape,
   so the previous turn's measured `prompt_tokens` no longer describes it. Without this the
   context meter would keep showing a number that belongs to a prompt that no longer exists.
3. `events.Compaction` (`maintenance.go:281`) carries the whole compaction to the
   frontend, which appends the card without a refetch. A manual run additionally gets
   `events.ChatsChanged` from `api.go`.

Status events bracket the work: `emitStatus(chatId, "compacting")` with a deferred
`"idle"` (`maintenance.go:156`), which is what makes the chat header show
`compact.running` ("Compacting …") for the duration (`ModelSwitcher.tsx:62`).

### 11.2 The compaction card

Rendered in the transcript at the fold boundary (`ChatView.tsx` groups by `toSeq`,
`MessageParts.tsx:268`):

- badge `compact.auto` / `compact.manual` (`MessageParts.tsx:276`);
- the title `compact.title`, i.e. "Compaction" (`MessageParts.tsx:278`);
- `#fromSeq–#toSeq · tokensBefore → tokensAfter Tokens (saved gespart)`;
- the **Focus** line, when one was given;
- a **Summary** expander showing the memory text as-is (`<pre>`, monospace, scrollable);
- **Show original messages / Hide originals** (`MessageParts.tsx:285`) — the folded messages
  are still rendered, dimmed, and the folded ones carry a 🗜 badge (`ChatView.tsx:358`);
- **Undo compaction**, only on the newest compaction — the only one that can be removed
  without contradicting a newer summary (`ChatView.tsx:77`).

Every label is an i18n key (`compact.*` in `frontend/src/lib/i18n.ts`), present in German,
English and Chinese.

### 11.3 The next turn: how the memory is used

`Assemble` (`context.go:117–215`):

- sorts compactions by `ToSeq` and takes the **newest** (`context.go:117`) — it already
  merges earlier ones;
- sets `FirstVerbatimSeq = latest.ToSeq + 1`; messages before it are *not* sent verbatim;
- folds the summary into the **single leading system message**, wrapped in
  `<conversation_memory>…</conversation_memory>` (`context.go:217`). The prompt, the
  document-index notice and the memory share that one message because a strict chat
  template accepts a system message only at position 0 (principle 4 above, and the
  seeded-search note below);
- adds it to `budget.Memory` (`context.go:207`) and to the Context Inspector as a segment
  labelled **"Gedächtnis (Kompaktierung)"** with the detail `Zusammenfassung der Nachrichten
  from–to` (`context.go:213`);
- counts every attachment **once**. Inlined document text is charged to
  `budget.Attachments` (`context.go:255`) and the message it was inlined into contributes
  only its own content to `budget.History` (`context.go:233`, `:263`). Both are added up into
  `Estimated`, so counting the text on both sides made the meter read half again as large as
  the request it describes. The measured path needs the same care for the opposite reason: a
  file attached to a message *newer* than the measurement was never in the measured prompt,
  so it is added to the delta (`context.go:316–319`).

So the memory is visible in the context inspector, counted in the meter, and clearly
attributed to a range of message numbers.

**A seeded search travels as a user turn, not a second system message.** `searchfirst.go`
stores the pre-flight research as a `system` row, and it belongs next to the question it
was run for. A strict template allows a system message only at position 0, so `Assemble`
changes that one role on the wire (`context.go:248`): the payload and its untrusted-data
notice are rendered as a user turn that a template will accept, while the stored row and
the source card the user reads back are untouched. Sending it as a second system message
was the same failure as a second leading one — a 500 from the template, not from the
window.

### 11.4 Undo

`DeleteCompaction` (`api.go:376`) removes the compaction and restores the folded messages
to the live context. Because the originals were never deleted this is complete, not
approximate. Two things happen with it:

- `DeleteCompactionsFrom` (`store/queries.go:733`) also drops every compaction with a
  `to_seq >= ` the undone one's. Those were summarised *on top of* the undone memory, so
  keeping them would leave a memory that references facts no longer in context. This is
  the fix recorded in `PLAN.md:216` — previously the code claimed to do this while
  discarding the list it had loaded.
- the measurement is invalidated again, for the same reason as in §11.1.

---

## 12. Settings reference

`config.CompactSettings` (`internal/config/config.go:63`), defaults from `config.Default()`:

| setting | default | effect |
| --- | --- | --- |
| `Enabled` | `true` | master switch for automatic compaction (manual still works) |
| `Threshold` | `0.80` | projected ratio at which automatic compaction fires |
| `KeepRecentTurns` | `6` | turns kept verbatim; also the floor for the forced-short-chat rule |
| `ReserveForResponse` | `1024` | headroom the budget keeps for the answer |
| `SummaryTemperature` | `0.2` | near-deterministic extraction rather than creative rewriting |
| `MaxSummaryTokens` | `1200` | **only raises** the effective 8192 output cap; the escape hatch for "spent everything on reasoning" |
| `SummaryModel` | empty | empty = the chat's own model; otherwise a `llm.Ref{Backend, Model}` (§6) |

Per chat (`store.Chat`): `autoCompact`, `compactThreshold` (0 < v < 1), `keepRecentTurns`.
An absent field keeps its default — the settings blob is one JSON row decoded over a fully
populated default, so no migration is needed for these.

---

## 13. Failure modes, and what each looks like

| situation | what happens | where |
| --- | --- | --- |
| a turn is running | button disabled + `compact.busy` (`ModelSwitcher.tsx:216`, `:223`); the engine refuses too | `maintenance.go:55` |
| nothing to fold (manual) | `Created: false`, toast `compact.noNeed` (`ModelSwitcher.tsx:141`), **no model call** | `maintenance.go:134` |
| nothing to fold (auto) | silent, the turn proceeds | `maybeAutoCompact` |
| no model / backend unconfigured / unknown window, **manual** | explicit error before any tokens are spent | `maintenance.go:54–72`, `:110` |
| unknown window, **automatic** | silent, the turn proceeds; the meter already says the limit is unknown | `maybeAutoCompact` |
| summary model on a missing backend | error naming model *and* backend | `maintenance.go:310` |
| summary model's window unknown | `events.Warning`, chat window used, memory still written | `maintenance.go:186` |
| reasoning model eats the budget | up to 3 attempts with a growing budget, then the "spent its budget on reasoning" error naming `MaxSummaryTokens` | `compact.go:520` |
| model returns only `<think>` blocks | blocks stripped, treated as reasoning, retried | `compact.go:415` |
| model returns nothing and did not reason | one attempt, then "empty summary" | `compact.go:520` |
| backend slower than 15 minutes | `context deadline exceeded` (the API backstop) | `api.go:344` |
| LM Studio without `/api/v1/models` | native route retired, OpenAI-compatible path + `/no_think` | `retireReasoning` |
| backend rejects the `reasoning` field | that model is retired from the native route, not the namespace | `retireReasoning` |
| chat shorter than `keepTurns`, automatic | no compaction, reason `nichts zu komprimieren` | `compact.go:167` |
| chat shorter than `keepTurns`, manual | first fold up to the newest user turn, then (if that is still empty) to the end of the chat | `compact.go:177`, `:223` |

---

## 14. How this is verified

**Unit tests** (`go test ./... -count=1`) pin the behaviour:

- *planning*: cut points on user boundaries, below-threshold no-op, resume after a previous
  compaction, too-few-turns, never fold a streaming message, forced target folds more
  (`internal/chat/chat_test.go`);
- *manual semantics*: short chat folds what exists, single turn folds itself, only no-ops
  when everything is already folded, no history is not a failure (`chat_test.go`); always
  folds when only the newest turn is left, an older compaction is never reported as new
  (`maintenance_test.go`);
- *budgeting*: transcript scales with the window, uses the whole window, the summariser
  request stays inside its window, the cap matches Cline and exceeds the summary target
  (`chat_test.go:382`, `:416`, `maintenance_test.go:347`, `:397`);
- *reasoning*: reasoning-off is requested, a toggle backend is asked not to think, inline
  thinking is stripped, a thinking-only answer is retried, an exhausted budget is explained,
  an empty answer is rejected, a genuine empty answer is not retried — the last four are the
  `TestSummarize*` tests, and the `TestCompact*` halves are at
  `maintenance_test.go:552`, `:587`, `:672`, `:709`;
- *wire*: the native route switches reasoning off for a summariser, reports an exhausted
  budget as `length`, is skipped when reasoning cannot be switched off or the request cannot
  be expressed, falls back once after a rejection and is not repeated, and is not consulted
  for ordinary turns (`internal/llm/native_reasoning_test.go`); Ollama honours
  `think:false` and LM Studio sends `enable_thinking:false` (`internal/llm/stream_test.go`);
- *rendering*: search payloads distilled to real sources and kept bounded, transcripts carry
  them into the summary (`chat_test.go`);
- *assembly*: a compaction replaces old history, a measurement taken at the fold boundary or
  across a compaction is discarded (`chat_test.go`).

**Live tests** (opt-in, metered) relevant here:

```bash
KICHAT_LIVE=1 go test ./internal/llm/ -run TestLiveNativeReasoningOffProducesASummary -v   # needs a loaded LM Studio model
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/chat/ -run TestLiveSearchFirstTurn -v
```

`TestLiveNativeReasoningOffProducesASummary` is the one that confirms against the real
service what the unit tests can only assert about the request.

---

## 15. Known gaps

Recorded deliberately, in the spirit of `PLAN.md` — these are the parts a reader should not
assume are covered.

- **The stored summary is not validated.** Nothing rejects or shrinks a summary that ignores
  the structure instruction, omits sections or comes back as long prose. The only bounds are
  the output cap (§7) and the prompt. There is no "summary too long → re-summarise" pass.
- **Only four thinking tag names are stripped.** `<scratchpad>`, or untagged prose emitted
  before the markdown, is not detected and lands in the memory. The prompt is front-loaded
  so the memory is still present, but it is not clean.
- **`PlanInput.MeasuredTokens` is declared but never populated or read** (`compact.go:85`).
  Its comment calls it "the authoritative used-token figure", which today it is not: the
  stored `TokensBefore` is the estimated size of the folded span. Either wiring it up or
  deleting the field would remove the discrepancy.
- **`CompactOptions.FromSeq`/`ToSeq` are plumbed end-to-end but no UI sends them.** The
  bound RPC accepts a pinned range; every caller currently passes 0/0
  (`ModelSwitcher.tsx:136`, `:394`), so the planner always decides.
- **`Target` is ignored on the automatic path**, which is correct but easy to misread: auto
  compaction fires at the threshold and folds the keep-turns cut, without aiming at a ratio.
- **The automatic path cannot rescue a chat the keep-turns cut leaves over the window.** The
  cut is the floor, so a chat whose last `KeepRecentTurns` turns alone exceed the limit is
  compacted *and stays over it*: measured with 1 910-token turns in an 8 192-token window,
  auto folded to 179 % of the window and reported the chat as still over limit (576-token
  turns land at 82 %, which is the case the threshold was designed for). The walk of §5.4
  could be run with a target of 1.0 to fold past the floor, but that trades the keep-turns
  guarantee for the window — a policy decision, not an arithmetic one, so it is recorded
  rather than decided here.
- **The meter's attachment figure is the stored count, not a re-derivation.** A document's
  `TokenCount` is what ingestion measured, and it is charged to `budget.Attachments`
  (`context.go:255`) on the way to `Estimated`; the `<document …>` wrapper and the base64
  envelope around the text are therefore counted nowhere, leaving the meter a few tokens
  short per file (the same order as the per-message framing that `History` has never
  counted). Re-deriving the block would change what the Context Inspector reports as the
  file's own cost, which is why it is recorded rather than fixed.
- **The first crossing turn pays the latency.** Compaction runs in the send path, before the
  turn; there is no background or pre-emptive run, and a local CPU model takes minutes.
- **One document, flat.** The memory is a single merge-targeted Markdown document; there is
  no per-topic or hierarchical structure, so memory quality ultimately rests on the model's
  fidelity when merging.
- **Cross-chat memory is not implemented.** It is a roadmap item (`PLAN.md:390`), backed by
  the same FTS index, not a feature of compaction.
- **The small-window trade-off is real** and accepted (§7): guarantee the request fits
  ahead of a longer transcript.

---

## 16. Where the code is

| concern | location |
| --- | --- |
| RPC, 15-min timeout, history listing, undo | `api.go:335`, `:344`, `:363`, `:376` |
| one entry point, refusals, auto-trigger, orchestration, budget, summariser resolution | `internal/chat/maintenance.go:54`, `:86`, `:102`, `:198`, `:310` |
| planner, cut policy, target walk, turn spans, prompts, transcript, summariser call, errors | `internal/chat/compact.go:110`, `:191`, `:327`, `:348`, `:415`, `:439`, `:520`, `:538`, `:616`, `:632` |
| memory injection, `FirstVerbatimSeq`, budget segments | `internal/chat/context.go:117`, `:197`, `:207`, `:213` |
| row shape, per-chat overrides, streaming status | `internal/store/models.go:12`, `:19`, `:127` |
| cascade delete of later compactions | `internal/store/queries.go:733` |
| settings, defaults | `internal/config/config.go:63`, `:129` |
| capability check, reasoning control, cache, retirement | `internal/llm/openai.go:120`, `:447`, `:487`, `:534`, `:551`, `:607`, `:630` |
| compaction card, fold grouping, 🗜 badge | `frontend/src/components/MessageParts.tsx:268`, `ChatView.tsx:72`, `:358` |
| compact buttons, focus field, busy state, running badge | `frontend/src/components/ModelSwitcher.tsx:62`, `:141`, `:216`, `:233`, `:242` |
| labels (de/en/zh) | `frontend/src/lib/i18n.ts` (`compact.*`) |
