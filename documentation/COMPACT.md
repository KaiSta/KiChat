# COMPACT.md

How context compaction works in KiChat: the strategy, the arithmetic behind it, and the
failures each rule exists to prevent. `PLAN.md` §3 states the one-line intent ("Compaction
never deletes messages"); this document is the detail behind that sentence.

Everything here was read out of the code and, where marked *measured*, observed on a
running build. Claims that are design intent rather than verified behaviour say so.

**Contents:** 1 problem · 2 invariants · 3 data model · 4 entry points · 5 planning ·
6 summariser choice · 7 budgeting the summarisation call · 8 the transcript ·
9 the prompts · 10 reasoning models · 11 storage, events, UI · 12 settings ·
13 failure modes · 14 how this is verified · 15 known gaps · 16 where the code is ·
17 condensing a tool result · 18 ageing a reply's older results.

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
| `Trigger` | `auto` \| `manual` \| `too_large` — how the memory came to exist | `triggerName(opts.Force)`, or `CompactOptions.Trigger` |
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

Both go through **one function**, `Engine.Compact` (`maintenance.go:71`) → `compactWith`
(`maintenance.go:183`). Only `Force`, `Target`, the two automatic bounds and `Focus` differ.

### Automatic

`autoFold` (`maintenance.go:127`) runs in the send pipeline, after search seeding and **before**
the turn is generated (`PLAN.md:85`), never mid-stream. It resolves the **two bounds** an automatic
fold can be decided by and hands both to the planner in one call
(`CompactOptions{Threshold, Ceiling}`), because they are one decision rather than two:

- the **user's threshold**, read from `settings.Compact.Enabled` **and** the per-chat
  `AutoCompact`, and passed as **0 when either of them is off** — 0 meaning *no bound at all*
  rather than *a default*, which is why nothing defaults it on the way in. `Force` stays false,
  so `Target` is 0 unless the ceiling below supplies one — or the chat is a **project**, whose
  automatic run always aims at its own threshold (§5.3).
- the **backend's per-request ceiling** (`llm.RequestCeiling` — Groq, GROQ.md §6). It is a number
  the vendor published rather than a preference, so it folds whether or not the user asked for
  automatic compaction; it is 0 for a backend that publishes none, which is why this function
  takes the provider as well as the settings.

With both 0 there is nothing to decide, and the function returns before anything is assembled or
summarised. The created compaction is discarded: an automatic run has nobody to report back to,
and "nothing to fold" is already its silent outcome.

The two bounds used to be **two calls** — `maybeAutoCompact` for the threshold and
`foldForCeiling` for the ceiling — and the cost was not only duplicated code: a chat that had
crossed both folded twice in the same turn, the threshold run stopping at its keep-turns boundary
and the ceiling run then folding further for its target, so it paid for two summaries to arrive
where one fold reaches. Pinned at that seam by `TestBothBoundsCrossedCostsOneFold` (2 compactions
and 3 backend calls before the merge, 1 and 2 after).

**Silence extends to the window nobody reports.** A model whose context window is unknown cannot be
compacted against at all — both bounds are ratios of it — but that is a standing fact
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
- optional `FromSeq`/`ToSeq` to pin the range explicitly. The start must follow the active memory, and the end must be the end of a complete turn (or the end of the chat).

It emits `events.ChatsChanged` only when `Created` is true.

### Size — a request the backend would refuse

Two more runs exist, both in `internal/chat/oversize.go`, and both because folding *is* the
answer to a backend saying the request is too big (the vendor's own shapes are in GROQ.md §6):

- **`foldForSize`** runs *after* a refusal and is the recovery. The refused turn was never
  answered, so its empty reply row is dropped, the history is folded (`Force`, `Trigger:
  too_large`, `ProtectNewestTurn`) and the turn is sent once more. It reports a `foldOutcome`
  rather than a bool, because the two ways a fold does not happen read differently to the user: a
  fold that had nothing older to fold produces the identical body and the identical refusal, so
  nothing is sent and the user gets the limit and the fix rather than the vendor's envelope,
  while a fold whose summariser *broke* is reported as the failure it is — the sentence the turn
  ends with does not blame an empty history (GROQ.md §6.2). Its aim is the ceiling the refusal
  named, **corrected by the vendor's own count of the refused body** (`sizeTargetWithCount`): the
  app's estimate of that body against the refusal's `Requested` is how far the estimator was low,
  the aim shrinks by that ratio (only when the vendor counted more, capped at 4×), and the same
  corrected ceiling budgets the summarisation (`CompactOptions.SummaryCeiling`). Without it the
  retry lands where the *estimate* says the ceiling is, and a backend whose tokenizer counts more
  refuses it a second time — which ends the turn, because a refusal is folded once.
- **the ceiling bound inside the planner** is the preventive fold, and it is not a second entry
  point any more: whether the next request would run into a published ceiling is the planner's own
  question, compared in one place with the user's threshold (`automaticBound`, §5.1) and answered
  with a `Target` when the ceiling is what binds (`ceilingTarget`). Only a backend that publishes a
  ceiling has it at all (`llm.RequestCeiling` — Groq, whose per-minute token allowance is also the
  largest single request the account may send), and its gate is `0.9 × ceiling` against the
  *projected* prompt. Its card says `auto`, which is exactly what a threshold nobody pressed means.

  The gate is asked again **between tool rounds** (`generate` calls `autoFold` with
  `midTurn` set, before every round after the first): a round appends a model reply and a tool
  result that the check before the turn could not price, and a refusal there costs the whole
  answer — `foldForSize` answers it on the turn's error and restarts the turn. The **ceiling** is
  always asked mid-turn. The **user's threshold** is asked only when `Compact.MidTurnCheck` is on
  (`autoFoldThreshold`, one function so the two calls cannot disagree about what "switched on"
  means): folding on a window rather than on a published ceiling spends a summarisation inside the
  answer the user is reading, so it is their decision and it ships off. That switch is also the
  only mid-turn check a backend that publishes **no** ceiling can get at all — every local model
  — and without it such a turn is only ever priced before it starts. An automatic fold there also
  *aims* at the threshold (`Target`), and that is what lets it run at all on a history shorter
  than `KeepRecentTurns`: the planner refuses a targetless fold whose cut would keep every turn,
  and a turn that grows through tool calls is usually one of a few long turns rather than one of
  many short ones. The fold still stops before the newest user turn, so the question being answered
  and the tool results it is working from stay verbatim; the check before a turn is untouched, and
  a short chat there is still left alone. Pinned by
  `TestABetweenRoundCheckFoldsAChatShorterThanKeepRecentTurns`.
  Pinned by `TestTheBetweenRoundFoldAsksTheThresholdOnlyWhenTheUserSaidSo` and
  `TestTheMidTurnCheckFoldsALocalToolTurnWhenTheUserAsks` (which asserts the pre-turn check stays
  quiet, so the fold it sees is the mid-turn one). The mid-turn switch is **subordinate**: a chat
  whose automatic compaction is off does not regain it between rounds.

  A **project** is the one exception, and `autoFoldThreshold` is where it is written down: it
  asks its threshold between rounds whatever the switch says. The switch is a choice a chat can
  make because a chat has an alternative — end the answer, fold before the next message — while
  a project's reply *is* the piece of work, and the round it is in the middle of is the answer,
  so a request that outgrows the window costs that work instead of delaying it. Nothing else
  folds it either: `Assemble` ([internal/chat/context.go](../internal/chat/context.go)) measures
  an over-full request and never trims one. The master switches are not overridden, so
  compaction switched off is still no fold, and
  `TestAProjectChecksBetweenRoundsWithoutTheChatSwitch` pins both halves — the fold with the
  chat switch where it ships, and none with the feature off.

  How far that fold goes is `ceilingTarget`, and it used to be ignored: the walk priced the aim
  against the caller's own `Target`, which an automatic run leaves at **0** — where 0 means "fold
  as far as the cut allows". Every ceiling fold therefore went all the way to the newest user
  turn, discarding the memory to save turns a request 2 % of the window did not need. The aim is
  now `target`, so the cut stops as soon as the kept turns plus the prompt, the reserve and the
  summary fit the ceiling (`TestAnAutomaticCeilingFoldAimsAtTheCeiling`).

  And a fold cannot make a turn sendable when one *tool result* is the growth: the result
  belongs to the turn being answered, so the cut may not touch it. That half is not
  compaction's either — a result is bounded where the tool returns it (32 KiB) and otherwise
  delivered whole, so on a backend with a per-request ceiling below its window such a turn
  ends with the refusal (GROQ.md §6.2 item 4, §17.6).

  Where that ceiling comes from is what decides which turn it can save. It is the vendor's own
  number, read from **the budget header of any response** — `x-ratelimit-limit-tokens`, which
  Groq sends with the answer as much as with the refusal, handed to the provider by the shared
  client before the body is read — and from **the sentence of a refusal**, which is the fallback
  for a session whose very first request is already oversized. So the second turn of a session
  is planned against the real limit while the first one still goes out against the window: a
  response is what carries the number, so the ceiling is one request behind by construction
  (GROQ.md §6, §9.6). Two rules keep that safe — the header is read for Groq alone (another
  vendor's rate-limit header is not evidence about the largest request it accepts), and a newly
  read number may only *tighten* what a refusal already proved.

  A third source exists for the turn neither of those can save: a ceiling the user typed into
  that model's override after reading their organization's Limits page
  (`llm.ModelOverride.RequestCeiling`, GROQ.md §6.2). The backend publishes no route that answers
  it before a response, so this is the only number available to the first request of a session —
  and `ceilingFor` merges it with the learned one under the same rule, keeping the smaller, so it
  cannot loosen what the vendor stated. The field is offered for Groq's models only, because it is
  the one backend here whose per-request budget is published nowhere a client can ask.

  What a *turn* is paced against is the minute, and the vendor measures it two ways: the
  **remainder** of the same header family for tokens, and for **requests** a limit learned from a
  refusal's own sentence against a count of this app's own responses — because no header of this
  API carries a per-minute request rate at all (GROQ.md §6.4). Either one being spent holds the
  request back rather than letting it be refused, so the wait taken is the longer of the two. A
  turn that needs more requests than the minute allows is spread against that rate *before* its
  first request instead, so its rounds go out one per slot rather than in a burst that then sits at
  the wall (GROQ.md §6.4, `internal/chat/pace.go`). A
  summarisation call is deliberately not paced, because it carries no watcher and a wait that
  nothing can show is not taken — so a compaction that runs into a spent minute is still answered
  with the vendor's own 429 and waited out by the shared client.

The ceiling is deliberately **not** gated by `settings.Compact.Enabled` or a chat's `AutoCompact`.
Those decide whether compaction happens *quietly*; a request at a published ceiling is about to be
refused, which puts that bound beside the Compact button rather than beside the silent threshold.
It therefore reaches the planner through the automatic entry point with the user's own bound
switched off (`Threshold: 0`), the state `TestAKnownCeilingFoldsTheHistoryBeforeTheRequestGoesOut`
and `TestBothBoundsCrossedCostsOneFold` both drive.

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

`PlanCompaction` (`compact.go:270`) is pure: messages + compactions + budget in, a
`CompactPlan` out. It decides the range, the cut point, the counts and the reason string.

### 5.1 The gate

Without `Force`, the plan is empty unless `Budget.ProjectedRatio` has reached **the tighter of two
bounds**, and that comparison is one function (`automaticBound`) because the two are one decision
rather than two that have to be reasoned about together:

| bound | where it comes from | expressed as | folds at |
| --- | --- | --- | --- |
| `Threshold` | the user's setting (per chat, else global, else 0.80) | already a ratio | its own value |
| `Ceiling` | the backend's published per-request limit, in tokens | `0.9 × ceiling / effective limit` | 0.9 of the ceiling |

Both are measured against the **projected** next request (which includes the response reserve), not
the last one — the fold has to fire before the request that would overflow, not after it. The reason
string records which bound fired: `Kontext bei 83% (Schwelle 80%)`, `Anfrage bei 3800 von 4000 Token
des Anfrage-Limits`, or `manuell ausgelöst` for a forced run.

The two differ in what crossing them *means*, and that is the whole of the rest of the difference. A
threshold reached folds as far as its cut allows — the context is fuller than the user wants — while
a request that has reached a ceiling's fold point is one the backend would **refuse**, so it folds
further: to the ceiling itself, through the same target walk a manual "→ 60 %" uses (§5.4). The aim
is decided by the ceiling's own fold point rather than by which bound fired, because a fold
triggered by a *lower* threshold on a chat that is already at the ceiling has to reach it as well.

There is one exception, and it is the floor under the threshold: when the **projected request is
already over the effective limit**, keeping the recent turns is not a saving. The fold aims at the
bound that fired, through the same walk, because a cut at the keep-turns boundary can leave a
request that still does not fit. That is the shape a **project** has — a few long work sessions,
often fewer turns than `KeepRecentTurns` — and without the aim the fold either refused (nothing
older to fold) and the oversized request went out, or trimmed one turn and left the kept turns
several windows large. Measured: a two-turn project priced at 23 031 estimated tokens against a
16 384-token window folded nothing before the turn and only folded after the first tool round;
the same fixture now folds before the request and every request it sends fits. Pinned by
`TestPlanCompactionFoldsPastTheKeptTurnsWhenTheRequestWouldNotFit` and
`TestAProjectFoldsAnOverfullRequestBeforeItGoesOut`.

A ceiling that cannot bind cannot fold early either: it is expressed against the limit the planner
measures with, so a ceiling above the window becomes a bound above the window's own (1.1 windows is
0.99 of it), and one more than `1/0.9` windows away is no bound at all — at any projection that
reached it the window is already over its limit, and the user's threshold is the earlier of the two.
Pinned by `TestTheTwoAutomaticBoundsAreOneDecision`, `TestACeilingAboveTheWindowIsNotABound` and
`TestAChatWithNoBoundAtAllFoldsNothing`.

**The ratio is taken against the limit the next request has to fit, which is not always the
window.** `ContextBudget.EffectiveLimit` is the smaller of the model's window and the backend's
own per-request ceiling (Groq's per-minute token allowance, GROQ.md §6), and `Percent`,
`ProjectedRatio` and `OverLimit` are all computed from it. So on a backend that publishes such a
ceiling, both automatic bounds are ratios of the same limit, and the percentage the user reads in
the meter is the percentage the planner acts on — a chat at 7 % of a 131k window but 108 % of an 8k
ceiling is already over the line, and reads that way.

Effective values, most specific first (`maintenance.go:463`, `:473`):

| setting | per-chat field | global field | fallback |
| --- | --- | --- | --- |
| threshold | `chats.compact_threshold` (0 < v < 1) | `Compact.Threshold` | 0.80 |
| turns kept verbatim | `chats.keep_recent_turns` | `Compact.KeepRecentTurns` | 6 |
| size kept verbatim | — | `Compact.KeepRecentTokens` | 0 → a quarter of the effective limit, clamped to 2000–15000 |

### 5.2 The range

```
fromSeq  = (largest existing compaction.ToSeq) + 1   → clamped to the first message
keepFrom = the user message that leaves keepTurns turns verbatim above it
         → then walked forward while the kept span exceeds KeepRecentTokens
toSeq    = keepFrom − 1
```

**The kept tail is a size as well as a count.** `KeepRecentTurns` says how much of the
conversation stays verbatim; it cannot say how much of the window that costs, because a
turn is a unit of conversation and not of size. Six turns are a few hundred tokens of short
questions and several windows of tool traffic. So the cut is walked forward one user turn
at a time — oldest first — until the messages above it fit `KeepRecentTokens`, and it stops
before the newest user turn, which is the exchange being answered.

Measured before this existed: 1 910-token turns in an 8 192-token window, auto fold at the
80 % threshold, and the chat came out at **179 % of the window** still over it, because the
six kept turns alone were in the kept set. That is the shape the size bound is for. It is
an upper bound and not a target — the smaller of the two keep rules wins — and a turn is
never split to reach it (invariant 3), so one turn larger than the budget leaves the tail
over it. That last case is the one the walk cannot fix, and it is recorded in §15 rather
than papered over.

The default is derived rather than fixed (a quarter of the effective limit, clamped to
2000–15000) because the right figure is a share of what the model can hold: 15 000 tokens
is most of an 8k window and a tenth of a 131k one. The clamp's ends are opencode's own
(`keep.tokens` 15000, clamped 2000–15000) — the only evidence available, since this app has
no measurement of its own for either end.

Two rules make this well-behaved:

- **Never before the first message** and **never re-fold an already-folded range.** This
  is what implements invariant 2: folding resumes where the last memory stopped, so its
  cost is proportional to *new* history rather than to total history.
- **From and to are cheap to explain** — the card shows `#from–#to`, and the active
  memory's budget segment says `Zusammenfassung der Nachrichten from–to`.

### 5.3 The forced-short-chat rule

When the cut lands before `fromSeq` (the chat holds fewer turns than `keepTurns`), three outcomes
exist — they fire in this order (the `toSeq < fromSeq` branch, then the escape hatch):

- **automatic, no target**: `Needed: false` with reason `nichts zu komprimieren` (or the existing
  reason plus ` — aber kein Verlauf zum Komprimieren`). Nothing is worth doing, and nothing may be:
  an automatic fold has nobody to promise anything to.
- **automatic, on a project, with a target**: a project's automatic run always aims at its own
  threshold (`autoFold`, `Target`), before a turn as much as between its rounds, so it takes the
  same cut a manual run would: everything before the newest user turn. A project is the shape this
  branch used to swallow — its history is a few long work sessions, so a cut that keeps
  `KeepRecentTurns` turns either does not exist (fewer turns than that) or leaves more context than
  the threshold allows. A chat without an aim keeps the rule above, because it has a fold point
  between two messages; a project's reply is the work, and its next round may never come.
  Pinned by `TestAProjectFoldsAtItsThresholdBeforeTheTurn`, against
  `TestABetweenRoundCheckFoldsAChatShorterThanKeepRecentTurns` for the chat's half.
- **any run with a target, and every manual one**: fold up to the **newest user turn** — the last
  turn that could contain a tool exchange that must not be split. For a manual run the comment
  names the motive: *"A manual compaction is a promise that the context gets compacted, so it must
  never end as a silent no-op while foldable history exists."* For the ceiling bound it is the
  point of the run: refusing here would leave exactly the oversized request the fold exists to
  prevent, and it is what makes that bound work on a chat with fewer turns than `keepTurns`.
- **manual, last resort**: if even that produces no cut, extend the cut to the **end of the
  chat** (`compact.go:223–232`). This is the state right after a previous compaction plus one
  new turn: every older turn is already folded and only the newest is kept verbatim, so the
  button would otherwise say "nothing to compact" forever. *"the end of the chat is always a
  valid cut point — nothing follows it, so no tool call is separated from its result."*
  Pinned by `TestManualCompactAlwaysFoldsWhenOnlyTheNewestTurnIsLeft`,
  `TestManualCompactionOnShortChatFoldsWhatExists`,
  `TestManualCompactionOnSingleTurnFoldsIt`,
  `TestManualCompactionOnlyNoopsWhenEverythingIsAlreadyFolded`.

The last resort above is a **forced** run's privilege: an automatic fold never takes it, because an
automatic run has nobody to promise anything to and folding the message it is about would leave the
request exactly as large. `PlanInput.ProtectNewestTurn` (`CompactOptions.ProtectNewestTurn`) turns
it off even for a forced run, and the size fold sets it: the message the user just sent is what the
next request has to carry, and folding it would answer the question with a summary of itself while
leaving the request exactly as large — the summary is written from the same text. With the escape
off, a chat whose only foldable content is the current turn folds nothing and says so, which is the
honest answer a size fold needs: `foldForSize` then skips its retry, and the user is told the limit
rather than handed a body that would be refused identically.### 5.4 Targets: "→ 60 %" and a ceiling mean what they say

With `Target > 0` the cut is **walked outwards from the keep-turns boundary** — folding one
turn per step, from the oldest kept turn towards the newest — until the estimate fits
`Target × Limit`. One walk serves every caller that has a target: a manual "→ 60 %", the fold that
answers a refusal (`foldForSize`) and the automatic fold aimed at a published ceiling
(`ceilingTarget`, which is `ceiling / window` because the walk prices against the window).
 The walk has to price the cut it is about to
install: the loop this replaced counted `i` backwards from the newest turn while folding
forwards with the same `i`, so its figure and its cut described two different states. The
cut could then be missed by a whole window in either direction — a 30 % target landing at
60 %, a 50 % target folding *fewer* turns than the 60 % one, or a bottom target stopping at
seven kept turns instead of the newest one. Three details make the promise honest:

- **The reserve counts towards the target.** The meter's `projected` number includes it,
  and the auto threshold reads that projected ratio — so pricing the manual target without
  the reserve would make the same percentage mean two different window states.
- **Each candidate cut point is priced as a whole turn span** — the user message, its
  answer and the tool traffic up to the next user turn (`turnTokenSpans`, `compact.go:511`).
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

**`limit` is not always the provider's published ceiling.** When the summarisation is the second
half of a size fold, `foldForSize` passes `CompactOptions.SummaryCeiling` — the published ceiling
divided by the ratio between the refusal's own `Requested` and the app's estimate of the body it
sent (GROQ.md §6.2) — and the budget above is computed against that. It only applies to a
summariser that is the chat's own model, since the ratio was measured for that model's tokenizer.
Measured: a 30 000-token ceiling over a 60-turn history gives an uncorrected summary request
estimated at 19 936 tokens, which a vendor counting twice the estimate refuses; the corrected
ceiling of 6 000 brings it under that ceiling
(`TestTheSummariserFollowsTheCorrectedCeiling`). The `reserve` above is what hides this in the
ordinary case — the output cap and the margin already cover a modest underestimate — which is why
the correction only bites when the transcript fills its budget.

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

Before rendering, inline text documents in the folded range are loaded into the transcript. A fold is refused if a document cannot be read or does not fit in full; inline images are also refused because a text-only summary cannot describe them faithfully. An empty transcript is refused rather than stored as a memory of unread messages. Ordinary conversation text remains subject to the deliberate trimming described below.

`RenderTranscript` (`compact.go`) flattens the folded messages into plain text, bounded at
**two levels**: every message is cut to `Block`, and the finished transcript to `Total`. Two
levels because a per-message cap bounds each message, not the transcript: *"enough messages
still add up past the window of the model that has to read them."*

The second level is a **ladder**, and it replaced one cut of the finished text. That cut kept
the beginning of the transcript, so overflowing it removed its tail — the newest folded
messages, which are exactly the ones that join the memory to the turns kept verbatim. Now
messages are shortened oldest first to a quarter of their block, every message reaches that
rung before any is left out, and whole messages are dropped oldest first with a note
(`[7 ältere Nachrichten ausgelassen]`) saying how many. **The newest message is shortened
but never left out**, and the note's cost is taken off the budget before what to keep is
chosen, so the sentence that reports a cut cannot be the thing that overshoots it.

The tail of a *block* is kept too: `clipToTokens` shares the allowance between the beginning
and the end of every message, because a build log's failure line and a test run's summary
are at the end of the text. `cutToTokens` still cuts at the end only, and that is a second
function rather than the same one because its callers say *"the first N tokens were read,
and the rest was not"* — a document condensed for the summariser (§17) and a tool result cut
to the room a request has left. A cut that carried the end would make that sentence false.

### 8.1 The prefix path: re-sending the request instead of a transcript

`summarisationPrefix` offers the summarisation the chat's own request up to the fold
boundary — the system message, the memory in force, and the folded messages exactly as they
were sent — and `compactWith` takes it when the request it makes still fits beside the
output cap and the estimator margin (§7).

Why: prompt caching. The user's turn has just sent that prefix, so a summary request that
opens with the same bytes is a hit on any server that caches a prefix; on a CPU-hosted
llama.cpp server that is the difference between reading the history and evaluating it again,
which is most of the minutes a fold takes there. It is also lossless in a way a rendering is
not — no search-payload distillation, no block clipping, no `(nur Reasoning)` substitution.

The transcript path stays the fallback, and it is what is sent when any of these holds:

| condition | why the transcript instead |
| --- | --- |
| prefix + the output cap + margin does not fit the window | fitting is the one thing the transcript path guarantees unconditionally |
| the summariser is not the chat's own model (§6) | another model never processed those bytes, so the prefix is a full evaluation of a larger request |
| the boundary cannot be named | `Assembly.MessageSeqs` is nil once a rewrite changed the request's shape (`sanitiseToolHistory`, `ageToolResults`, a continuation), and a guessed boundary would cut the history in the wrong place |
| the prefix would be the system message alone | there is no history in it to summarise |
| the prefix carries an image | a summarisation is a text request; the pictures belong to the conversation |

**The memory is carried once.** On this path the memory in force is already the system
message's `<conversation_memory>`, so the instruction refers to it — *"the memory in the
system prompt above already covers the older messages; merge it with the following history"*
— instead of quoting it again as `<existing_memory>`. The transcript path keeps the quoted
block, because its request has no system message of its own carrying the memory. That is a
second wording of the merge sentence in each language, and it is worth paying for: the
sentence is what stops a fold from silently dropping the old memory, since a model told only
"compact the following history" writes a memory of the new messages.

Pinned by `TestTheSummaryResendsTheRequestPrefixWhenItFits`,
`TestTheSummaryFallsBackToATranscriptWhenThePrefixDoesNotFit`,
`TestThePrefixIsOnlyOfferedWhenItCanBeNamed`, `TestThePrefixRequestCarriesTheMemoryOnce` and
`TestThePrefixTailRefersToTheMemoryAboveInsteadOfRepeatingIt`.

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

`summarySystemPrompt` is deliberately explicit about
preservation: *"a compaction that loses a URL or a number silently corrupts every later
answer."* It is written in the app's language (Settings → Language), names that
language explicitly, and translates its headings with it: the contract being German is
what made a German memory out of an English transcript, and the memory travels in the
system message of every later request. Its rules, for the English contract:

- Write in English.
- Carry names, numbers, dates, prices, legal references and URLs over **WORD FOR WORD** —
  do not shorten or paraphrase them.
- Invent nothing. Anything that stayed unclear goes under "Open questions".
- Carry findings — what was examined, what came of it, the file, the line and what is wrong
  there — as one bullet each, with identifiers and paths word for word, and keep the brevity
  rule off them.
- Be brief: bullets, no prose, no repetition.
- Record the state of the work as far as the transcript shows it: what is done, what is still open, and what the next step is.
- Leave out what the next request carries anyway: the system instructions, the project's AGENTS.md files, and the tool descriptions. What the user asked for themselves belongs in the memory.
- Do not mention the compaction or that context was compacted.

and it fixes the output structure, omitting empty sections:

```
## Festgestellte Fakten      ## Entscheidungen        ## Befunde & offene Arbeit
## Offene Punkte             ## Quellen              ## Dateien & Dokumente
## Vorgaben & Präferenzen
```

The German contract is shown above; the English and Chinese contracts carry the same
sections in their own language. This is a memory schema: what was established, what was
decided, what is still open, which sources back it, which files are in play, and the
user's standing preferences.

**Findings are carried as findings, not folded into "facts".** The section exists because of
a reported failure: a review produced a list of defects with file and line, the fold that
followed folded the answer away, and the next request — *"fix what you found"* — was
answered by a model that no longer had them. The contract's own brevity rule is what
finished them off: "Stichpunkte, keine Prosa" over a 1 200-token memory turns ten findings
into one vague sentence. So the rule that carries names and numbers verbatim now names
findings too, and says the brevity rule does not apply to them — a finding that cannot be
acted on from the memory alone has been lost, the same standard the contract already sets
for a URL. Pinned by `TestTheMemoryKeepsFindingsActionable`.

### 9.2 The user message

`summarisePrompt` is split out of `Summarize` so that the transcript
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
wrapper keeps the transcript distinguishable from the instructions around it. On the prefix
path (§8.1) the `<existing_memory>` block is absent and the same sentence becomes *"the
memory in the system prompt above already covers the older messages"* — the memory is in the
request already, and quoting it twice charged the fold for it twice.

### 9.3 One restructure when the shape was ignored

A memory that comes back as flat prose is not an error — it still knows things — but every
later turn has to search it instead of scanning it. So a summary that carries **none** of the
section headings the contract asks for is sent once more, and what travels on that second
request is the answer, not the conversation (`summaryRestructure`, per language):

```
Recast the memory below into the structure the contract above requires:

<draft>
…the first answer…
</draft>

Answer with only the memory: the ## headings the contract defines, no preamble.
Leave no fact out and add nothing.
```

The shape check fires only when the answer carried no heading at all, so the sections it is
missing **are** the structure, and the material for them is what the model already wrote.
That is why the retry is cheap. It used to re-send the transcript or the prefix with one
sentence appended — up to 80 % of the window, minutes of prompt on a CPU-hosted model, and a
request no prefix cache can hit because of the appended sentence — to ask for headings that
could be added to the draft. Now the draft is bounded by the summariser's output cap instead
of by the window, so the second call is a fraction of the first rather than a second full
pass over the history.

Two things limit what this can cost further. The restructure is only sent when the shapeless
answer is at least `summaryStructureMinTokens` (400) tokens: a model that answered a
page-long instruction with three words is fixed by choosing another model, not by asking
again. And nothing is rejected: a retry that also comes back shapeless leaves the first
answer in place, because a memory with the wrong shape still carries more than no memory at
all.

The headings are matched across all three contract languages, because a model may answer in
the transcript's language instead of the interface's, and that memory followed the structure
even though it is not the one that was requested. Pinned by
`TestAShapelessMemoryIsAskedOnceMore` (which also asserts the second request carries the
draft and not `### Nutzer`), `TestAShortShapelessMemoryIsNotAskedAgain` and
`TestARestructureAsksForTheShapeWithoutTheHistory`.

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
| `summary != ""` and stop reason is `length` | refuse the incomplete memory |
| `summary != ""` otherwise | done — the memory is returned |
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

`DeleteCompaction` claims the chat like a turn before it removes the compaction and restores the folded messages
to the live context. Because the originals were never deleted this is complete, not
approximate. Two things happen with it:

- `DeleteCompactionsFrom` (`store/queries.go:733`) also drops every compaction with a
  `to_seq >= ` the undone one's. Those were summarised *on top of* the undone memory, so
  keeping them would leave a memory that references facts no longer in context. This is
  the fix recorded in `PLAN.md:216` — previously the code claimed to do this while
  discarding the list it had loaded.
- the measurement is invalidated again, for the same reason as in §11.1.

### 11.5 The measured size after a restart

The count the meter treats as authoritative is the backend's own `prompt_tokens`, and it lives
in `chat.TurnState` — which outlives an Engine but not the process. After a restart the chat was
therefore priced by `llm.EstimateTokens`, and the estimator is deliberately priced in the safe
direction and counts half of every reply's reasoning whether or not the request can carry it
(§5.6). On a project's history that combination can cross the window the chat had been running
inside: the meter reports an over-limit context for a request that fits, and the model picker
will not start a local model on that reading (`ModelSwitcher.commit` refuses a start whose budget
is over the limit), so the session cannot be resumed.

Every measuring request wrote its `prompt_tokens` onto the reply row it produced
(`generate.go`, `row.PromptTokens`), so the newest such row *is* the count the chat last had.
`measurementFromHistory` (`engine.go`) adopts it when the in-memory count is gone, and drops a
row that cannot describe the prompt about to be sent: another backend/model's count (a different
tokenizer), a reply written before the newest fold (its request still carried the messages that
fold replaced — the same rule as `InvalidateMeasurement` in §11.1), and a zero `prompt_tokens`
(no measurement rather than a size). `Seq` is the message before that reply, since a reply cannot
be inside the prompt it answers, so the ordinary delta accounting of `Assemble` applies
unchanged: the reply itself and everything after it are priced as the delta.

---

## 12. Settings reference

`config.CompactSettings` (`internal/config/config.go:63`), defaults from `config.Default()`:

| setting | default | effect |
| --- | --- | --- |
| `Enabled` | `true` | master switch for the automatic **threshold** (manual still works, and a backend's published ceiling folds regardless — §4) |
| `MidTurnCheck` | `false` | ask the threshold again between the tool rounds of one answer. Off because it costs a summary inside an answer somebody waits for; it is the only mid-turn check a backend with no published ceiling gets, and a **project** asks it regardless — its reply has no fold point between two messages (§4) |
| `Threshold` | `0.80` | projected ratio at which the threshold bound fires |
| `KeepRecentTurns` | `6` | turns kept verbatim; also the floor for the forced-short-chat rule |
| `KeepRecentTokens` | `0` (derived) | size budget for the kept tail; 0 means a quarter of the effective limit, clamped to 2000–15000. The smaller of the two keep rules wins (§5.2) |
| `ReserveForResponse` | `1024` | headroom the budget keeps for the answer |
| `SummaryTemperature` | `0.2` | near-deterministic extraction rather than creative rewriting |
| `MaxSummaryTokens` | `1200` | **only raises** the effective 8192 output cap; the escape hatch for "spent everything on reasoning" |
| `SummaryModel` | empty | empty = the chat's own model; otherwise a `llm.Ref{Backend, Model}` (§6) |

Per chat (`store.Chat`): `autoCompact`, `compactThreshold` (0 < v < 1), `keepRecentTurns`.
An absent field keeps its default — the settings blob is one JSON row decoded over a fully
populated default, so no migration is needed for these.

The condensation of tool results has its own group, `config.DigestSettings` (§17), because
it is a different job with a different model: a fold rewrites a history the model has
already read, a condensation decides what a tool result becomes on its way in.

---

## 13. Failure modes, and what each looks like

| situation | what happens | where |
| --- | --- | --- |
| a turn is running | button disabled + `compact.busy` (`ModelSwitcher.tsx:216`, `:223`); the engine refuses too | `maintenance.go:55` |
| nothing to fold (manual) | `Created: false`, toast `compact.noNeed` (`ModelSwitcher.tsx:141`), **no model call** | `maintenance.go:134` |
| nothing to fold (auto) | silent, the turn proceeds | `autoFold` |
| no model / backend unconfigured / unknown window, **manual** | explicit error before any tokens are spent | `maintenance.go:54–72`, `:110` |
| unknown window, **automatic** | silent, the turn proceeds; the meter already says the limit is unknown | `autoFold` |
| summary model on a missing backend | error naming model *and* backend | `maintenance.go:310` |
| summary model's window unknown | `events.Warning`, chat window used, memory still written | `maintenance.go:186` |
| reasoning model eats the budget | up to 3 attempts with a growing budget, then the "spent its budget on reasoning" error naming `MaxSummaryTokens` | `compact.go:520` |
| model returns only `<think>` blocks | blocks stripped, treated as reasoning, retried | `compact.go:415` |
| model returns nothing and did not reason | one attempt, then "empty summary" | `compact.go:520` |
| backend slower than 15 minutes | `context deadline exceeded` (the API backstop) | `api.go:344` |
| LM Studio without `/api/v1/models` | native route retired, OpenAI-compatible path + `/no_think` | `retireReasoning` |
| backend rejects the `reasoning` field | that model is retired from the native route, not the namespace | `retireReasoning` |
| chat shorter than `keepTurns`, automatic with no bound crossed | no compaction, reason `nichts zu komprimieren` | `compact.go:340` |
| chat shorter than `keepTurns`, a target (manual, or the ceiling bound) | folds up to the newest user turn | `compact.go:340` |
| manual with nothing older at all | extends the cut to the end of the chat | `compact.go:414` |
| both automatic bounds crossed in one turn | **one** fold and one summary — the planner holds both bounds | `autoFold`, `automaticBound` |

---

## 14. How this is verified

**Unit tests** (`go test ./... -count=1`) pin the behaviour:

- *planning*: cut points on user boundaries, below-threshold no-op, resume after a previous
  compaction, too-few-turns, never fold a streaming message, forced target folds more, the two
  automatic bounds decided together (the ceiling folding with the user's switch off and aiming at
  itself, a ceiling above the window not folding early, no bound at all never folding)
  (`internal/chat/chat_test.go`);
- *the seam*: both bounds crossed in one turn costs one summary and two backend calls rather than
  two folds and three (`oversize_test.go`, `TestBothBoundsCrossedCostsOneFold` — it was written
  against the pre-merge code, where it fails with exactly those numbers);
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

- **The stored summary is validated for its shape only, and only once.** One restructure asks again
  when a long answer carried no section at all (§9.3); nothing rejects or shrinks a summary
  that omits sections, invents headings or comes back as long prose under the 400-token gate.
  There is no "summary too long → re-summarise" pass, and none of this inspects the content.
- **Only four thinking tag names are stripped.** `<scratchpad>`, or untagged prose emitted
  before the markdown, is not detected and lands in the memory. The prompt is front-loaded
  so the memory is still present, but it is not clean.
- **`PlanInput.MeasuredTokens` is gone.** It was declared and never read; the authoritative
  figure reaches the planner through `ContextBudget`, which `Assemble` builds from the last
  response's `prompt_tokens` plus an estimate of what came after it (§11.3) — so the trigger
  is anchored on a measurement already, and `TokensBefore` remains the estimate of the folded
  span because that is what a fold reports.
- **`CompactOptions.FromSeq`/`ToSeq` are plumbed end-to-end but no UI sends them.** The
  bound RPC accepts a pinned range; every caller currently passes 0/0
  (`ModelSwitcher.tsx:136`, `:394`), so the planner always decides.
- **A target on the automatic path comes from the ceiling, and only from there.** A threshold
  crossing folds the keep-turns cut without aiming at a ratio; `Target > 0` on an automatic run
  means the ceiling bound decided it (§5.1, §5.4). Easy to misread from the two callers that were
  separate before the merge, one of which always passed a target.
- **A single turn larger than the size budget still leaves the tail over it.** The walk of §5.2
  shrinks the kept history by folding whole turns, so the shape that defeated the keep-turns
  boundary — six 1 910-token turns against an 8 192-token window, folded to 179 % of the window
  — is now bounded by tokens instead of by count. What it cannot do is split a turn (invariant
  3), so a chat whose *newest* turn alone exceeds `KeepRecentTokens` keeps that whole turn and
  the fold cannot bring it under. Saying so is the honest end of it: the alternative is a prompt
  that begins on an assistant message, or a target of 1.0 that trades the keep-turns guarantee
  for the window.
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
| size fold: the recovery, the preventive fold, the target arithmetic, the message shown | `internal/chat/oversize.go` |
| the refusal that says the request is too big, and the ceiling it names | `internal/llm/limits.go`, `internal/llm/openai.go` (`noteRequestCeiling`) |
| the per-request budget read from a response's headers, and the watcher that carries them | `internal/llm/limits.go` (`ceilingFromHeaders`), `internal/llm/httpclient.go` (`responseWatcher`), `internal/llm/openai.go` (`requestBudget`) |
| settings, defaults | `internal/config/config.go:63`, `:129` |
| capability check, reasoning control, cache, retirement | `internal/llm/openai.go:120`, `:447`, `:487`, `:534`, `:551`, `:607`, `:630` |
| compaction card, fold grouping, 🗜 badge | `frontend/src/components/MessageParts.tsx:268`, `ChatView.tsx:72`, `:358` |
| compact buttons, focus field, busy state, running badge | `frontend/src/components/ModelSwitcher.tsx:62`, `:141`, `:216`, `:233`, `:242` |
| labels (de/en/zh) | `frontend/src/lib/i18n.ts` (`compact.*`) |

---

## 17. Condensing a tool result (its sibling, not a fold)

A fold is one answer to "this request is too large". The other one is to stop the large
thing entering the context at all, and that is now a feature of its own for the two places a
tool result can be long — a scraped page and a file read out of the project folder
(`internal/chat/digest.go`, §17.6 for the second one).

### 17.1 What it does

After a tool returns and before its text is stored as the message the model reads, a second
model condenses the text. The chat's model reads the condensation; the tool-call card
keeps what the tool actually returned. The message that is stored — and therefore
replayed on every later turn — holds the condensation, which is the whole point: a page
the user read once would otherwise cost every subsequent request in that chat.

**The condensation is written in the app's language.** A German page read in an English
window is summarised in English: the original's names, numbers and quotations stay
verbatim, but the prose around them follows the app, so a German source cannot pull the
next answer into German. The language (beside the target and the ceiling) is part of the
digest cache key, so a window that changes language does not get the other one's summary.

Inside a `scrape_url` result, only the page is condensed: the text between
`<scraped_content>` and `</scraped_content>` is replaced by the summary, and everything
the tool wrote around it — title, source line, page count, and, for a document too large
to inline, the document id and the chunk range that reads the rest — travels unchanged.
The two halves have opposite jobs: the page is what the second model may throw away, the
framing is how the chat reaches the parts it kept.

**What is condensed is the document, not the excerpt.** A result over the inline
threshold carries only the beginning of a page that was indexed whole (RESEARCH.md §6),
and a summary of that beginning answers questions about page 1 and none about page 50.
So `condenseToolOutput` looks up the exact document ID returned by that scrape, and
summarises its text. Concurrent scrapes of the same URL cannot substitute another version,
and an unindexed page cannot accidentally reuse an older document. If the indexed document
cannot be loaded, the excerpt is summarised and labelled partial.

**A document larger than the summariser reads is said to be partial.** The text is cut to
what the window leaves beside the summary and prompt reserve. A large output cap is reduced
when necessary to leave space for input, and the notice opens with
`Partial summary of … — the rest was not summarised` (`Teilzusammenfassung von … — der Rest
wurde nicht zusammengefasst` in a German window — the notice follows the interface language,
§17.6) instead of claiming a full summary. The rest is still reachable through the chunk range in the framing. A
56-page manual is ~98k tokens against a 32k window, so this is the normal case for a
manual, not an edge case.

| setting | where | default |
| --- | --- | --- |
| `Digest.Model` | Settings → Tool results | empty = **off** |
| `Digest.Threshold` | Settings → Tool results | `2000` tokens (0 means the default) |
| `Digest.MaxTokens` | Settings → Tool results | `600` tokens (0 means the default; range 100–8000) |
| `Backend.Digest` | Settings → Backends, per backend | `false` |

`Digest.MaxTokens` is the **target length of the summary**, not the size at which
condensing starts and not the request's hard stop. It is stated to the model in the prompt
("Target: about N tokens"; "Ziel: etwa N Tokens" in a German window), and the request is given whatever room the window leaves beside
the text — up to `MaxDigestMaxTokens = 8000` — so a summary that needs a few more tokens
than it was aiming at finishes instead of being cut mid-sentence. Only the window or that
8000-token line can still cut one, and a summary cut there is used as it is
(§17.2). The target is a setting because how short a summary can be before it stops
answering the question depends on the model and the page, and 600 was a starting point
rather than a measurement. Two rules resolve it
(`DigestSettings.DigestMaxTokens`): 0 means the default, and the range is bounded
(100–8000).

The first version also capped it at `Threshold` — a summary should never be longer than
what it replaces — and that was wrong twice over. The threshold is the *smallest* page
worth a request, not the size of the page in hand, so a 40k-token page above a 2k
threshold can want far more than 2k tokens out. And `normalise` stored the capped value,
so a chosen 3000 came back as 2000, was gone from the blob, and raising the threshold
later did not restore it. Both are pinned by `TestTheDigestCapIsResolvedNotTrusted`. The
bound that remains is the tool service's own byte cap on the result itself (§17.6): a
condensation is delivered whole, and no longer cut to the room the request has left.

### 17.2 The rules that make it safe to switch on

- **Off unless a model is chosen.** Empty is the only "off": the one model this must
  never use is the chat's own, because that is the model whose request the condensation
  exists to shrink. On a backend whose allowance is a shared per-minute bucket, asking it
  to read the page first spends the budget the answer needs (GROQ.md §6.1).
- **Per backend.** What it buys differs per backend and so does the price, so the switch
  lives on the backend row: one where a page would otherwise be refused, or one with a
  large window that loses only the latency.
- **Only a page.** `scrape_url` is condensed. A document is already chunked and read by
  chunk range (`read_pages`), and a condensation there would throw away the precision
  retrieval was built for. A scraped document is both at once — `scrape_url` indexes a
  result too large to inline (RESEARCH.md §6) — so the rule is applied to the page text
  rather than to the result: the framing around it stays verbatim, which is what keeps an
  indexed document reachable from a condensed read
  (`TestACondensedDocumentKeepsThePointer`).
- **Never the reason a result is missing.** Every failure — no provider, an unreachable
  model, an empty answer — returns the result unchanged and emits a warning. The feature
  is an optimisation of what the model reads; the behaviour without it is always the
  fallback.
- **The target is an aim, not a wall.** The prompt asks the model to fit `MaxTokens`,
  and the request's `max_tokens` is the room the window leaves beside the text (capped at
  `MaxDigestMaxTokens`), so 600 asked for and 750 written is a complete 750-token summary
  rather than 600 tokens cut mid-sentence. A summary that still stops at that ceiling is
  kept: refusing it would hand the chat the whole page, exactly the cost this exists to
  avoid, and the tool card holds the page while the second read returns it. The
  rejection lives in `extractWithBudget` (`TruncatedError`) and only a fold sets it — a
  memory replaces history, so a sentence cut short there is a fact nothing can recover
  (`TestCompactionRejectsMaxTokensStop`, against
  `TestACondensationCutAtItsCeilingIsStillUsed`).
- **The way back is the same call.** The second read of a page in one chat hands over the
  full text (a per-process ledger keys it), so a model that needs an exact quotation can
  ask for it. The ledger is marked only when the full summary reaches the model; a tool
  budget that withholds or truncates it must not consume the escape hatch.
- **Cached by content.** The key is the model *reference* (backend and id, the same
  identity `llm.Ref` gives everything else), the prompt version, the cap the summary was
  written under, the file path for file summaries, and a hash of the *text that was read* — the document when one was found,
  the page text otherwise, and never the whole tool result. The framing is deliberately
  out of the key: it carries a document id that is new on every scrape, so a key over the
  whole result would miss on every re-read of the same document
  (`TestTheCondensationCacheIsKeyedByThePageNotItsFraming`). Hashing the text that was
  read, rather than the document, is what stops a summariser with a narrower window from
  serving a wider summary out of the cache. The target and the ceiling are in the key
  because one is a setting stated in the prompt and the other bounds the same answer, and
  the same ceiling can serve two targets: a summary asked to fit in 600 tokens is not the
  one the same model gives at 2000, so a changed target must miss the cache instead of
  serving the old length for up to 30 days. A page read twice in one
  chat or across chats costs one condensation; the entry expires after 30 days and the
  start-up prune removes it.
- **No ratio is stated to the model.** It is told that the text is a summary and that the
  full text can be had again; it is not told how much was cut. A ratio it cannot verify
  invites claims of completeness it cannot have.

### 17.3 Where it plugs in

`runTool` ([generate.go:341](../internal/chat/generate.go#L341)) hands the result through
`condenseToolOutput` ([digest.go:199](../internal/chat/digest.go#L199)) immediately before
`budget.take`, so the budget is charged for what the model actually receives. The call
itself reuses `extractWithBudget` ([compact.go:673](../internal/chat/compact.go#L673)) — the
retry that survives a reasoning model spending its cap on thinking is the same failure
here — with its own prompt (which states the target), the answer's ceiling and thinking
switched off where the backend allows it.

The allowance is also held under the summariser's window on the *first* attempt, not only
on the retry: a backend validates the prompt and the output allowance together, so 8000
output tokens beside a page that fills a small window is an oversized request, refused
before a token is generated. That was theoretical while the setting was a constant of
600; with the target as a setting and the room the window leaves as the ceiling it is
what a small local model sees first, so `extractWithBudget` shrinks the allowance to the
room that is left (pinned by `TestTheCapIsHeldUnderTheWindow`).

Because it is the same helper, every reasoning-model measure a fold takes is taken here
too, for free: the backend's thinking toggle, the `/no_think` soft switch, inline
`<think>`/`<analysis>` blocks stripped from the answer before it is judged (and counted
as reasoning when nothing else came back), and the retry that grows the budget by the
reasoning measured — capped by `ContextLimit − prompt`, so the condensation never asks
for more than the summariser can hold. The one thing that is *not* shared is the
budget: a fold's output cap is deliberately generous (`summaryOutputTokens = 8192`,
because `max_tokens` bounds thinking and answer together), while this one is small by
purpose and now configurable — which is why the retry matters more here than there.

### 17.4 Tests

`internal/chat/digest_test.go`, one per rule above: the page replaced by the
condensation end to end (through a real scrape against a local page), a backend without
the switch, no model configured, below the threshold, a failed condensation delivering
the page plus a warning, the second read returning the full text, a condensation that
stops at its ceiling still replacing the page, a cache hit across chats, a document chunk
left alone, the configured target reaching the model in the prompt with room above it, a
changed target missing the cache, inline thinking stripped from the answer,
and a thinking-only answer retried with a larger budget. `TestACondensationReplacesThePageTheModelReads`
fails by design if the call in `runTool` is removed. The cap's resolution (0 = default,
bounds, threshold) is pinned in `internal/config/config_test.go`. The file half is §17.6:
`TestALargeReadIsCondensedBeforeTheModelSeesIt`, `TestATargetedReadIsNeverCondensed`,
`TestAReadBelowTheThresholdIsHandedOverWhole`,
`TestARepeatedReadIsCondensedAgainFromTheCache`, `TestAReadsFramingIsNotCondensed`,
`TestAFileThatQuotesTheMarkersIsNotCutAtThem`, `TestABoundedReadIsRecognised` and
`TestTheFoldersRulesSurviveACondensation`.

### 17.5 What the card says: the two sizes

Until now the transcript showed the condensation where the model read it and the full
output on the tool card, and nothing said *how much* was cut. The card now carries both
sizes, and they are the backend's own measurements rather than a client-side estimate:
`ToolCallRecord.SourceTokens` is `llm.EstimateTokens` of what the tool returned, and
`ContextTokens` is the same estimate of the exact string handed to `appendToolResult` — so
the second number includes the digest notice and, where the tool service's own byte cap
cut the text, that cut as well.

Two outcomes produce those numbers and only one of them is a setting the user switched on,
so the record keeps the cause rather than leaving it to be inferred from the sizes:

| outcome | `Condensed` | card |
| --- | --- | --- |
| whole | false | `4,371 tokens` |
| condensed by the second model | true | `12,345 → 640 tokens` |

The `Withheld` column and the `4,374 tokens discarded` card are historical: they recorded a
result the app deliberately did not deliver because the request had no room left (§17.6), and
no row writes the flag any more. Rows written before that change still read as what they were,
which is why the field and the card's wording stay.

The numbers live in `tool_calls` (migrations 8 and 9, appended for the usual reason),
because the card is rebuilt from the row after a reload and a number recomputed in the
window would be a second tokenizer disagreeing with the one the budget charged. A row
written before those migrations carries zeros and renders nothing, which is the honest
reading of a call nobody measured.

A call is re-emitted once the condensation has run: the card was drawn while the tool
was still executing, and a count that appeared only after reopening the chat would be a
number the reader has to go looking for. `TestTheToolCardIsToldHowMuchWasCut` pins the
cut and uncut shapes on the emitted event *and* on the row read back, over the same page
under a threshold it clears and one it cannot reach.

The harness fixture for the research chat carries all four shapes, including the two that
no code writes any more, because they are only distinguishable by the words after the number.

### 17.6 Reading a file out of the folder

The same condensation covers `read_file` (`digestJobFile`), which is where it saves the
most: a coding chat reads source, and a 2000-line read is ~20k tokens against a 15k–25k
answer budget. Four things are different from a page, and each is a rule rather than a
detail.

**A file has its own prompt, because what may be lost is different.** `readFileSystemPrompt`
keeps paths, line numbers, identifiers and signatures *verbatim*; the page prompt's
"Stichpunkte, lass Navigation weg" would invite a summary to rename a function. A rename in
a summary is not a lost detail — `edit_file` takes `old_string`, `read_file` continues from
an `offset`, and a summary that invented either makes the next call wrong.
Pinned by `TestALargeReadIsCondensedBeforeTheModelSeesIt` (which asserts the file prompt and
`<file path="…">` reached the digester, not the page prompt).

**The notice speaks the interface language.** The one sentence that says a file read is a
summary, and names `read_file` with `offset`/`limit` as the way to the text, is the sentence
the model has to obey — and it was German in every window, so an English chat with an English
system prompt and English tool descriptions was handed `den genauen Text liefert read_file mit
offset/limit`. It now follows `Settings.Language` like the shipped prompts do
(`config.PromptBody`), with English as the fallback for anything else, the same fallback those
presets use. Not "always English": a German window whose model is told in English that the text
above is a summary has the same mismatch in the other direction, and the summary itself is
written in the language of the text, not of the interface. The summariser's own prompt stays
German for a different reason — its output language is dictated by the page or file it is given,
so translating it would change nothing a reader sees while retiring every cached condensation
through `digestPromptVersion`. Pinned by `TestTheNoticeSpeaksTheInterfaceLanguage`, which reads
the same file in a German, an English and a Chinese window and asserts the third falls back to
English. The notice is one of many such sentences, so the rule itself — and what stays a format
rather than a sentence — is stated once, in [`CODING.md`](CODING.md) §2.

**A bounded read is never condensed.** A call with `offset` or `limit` is one the model has
already decided about — it is what a coding chat does immediately before `edit_file` — so
`targetedRead` refuses those. It is the same rule Spotify's shunt hook applies, for the same
reason: the delegation is for *understanding*, and the edit needs the text.
Pinned by `TestATargetedReadIsNeverCondensed`.

**An outline is exempt too.** `"outline": true` asks for the declaration list rather than the
text (`workspace.Root.Outline`), and `outlineRequested` refuses it: the list is this app's own
deterministic answer, so a second model asked to summarise it could only lose the line numbers
the next read cites. Its own size does not decide this — a 300-declaration outline is well over
the threshold, and `TestAnOutlineIsNeverCondensed` asserts the fixture is above the threshold so
the test fails for the right reason. Note the classifier is stricter about `false` than
`targetedRead` is about an emptied field: a misread there leaves a read uncondensed, while one
here would answer a request for the text with a map.

**The framing is carried, not summarised.** `splitReadBody` cuts the result at the header
and at either of the two trailers, and both trailers travel verbatim: the
`Die Datei geht weiter … offset=N` line is the only way the model knows where a truncated
file resumes, and `<project_agents_md …>` is the folder's rules — a summary of rules is not
the rules. The markers live in `internal/tools` (`ContinueNotice`/`ContinueNotices`,
`ReadGuidanceTag`) and
are used by the tool that writes them, so the splitter cannot drift from the format. The
continuation sentence is the one marker that is a sentence as well as a format, so it has a
wording per language and `ContinueNotices` returns all of them: a stored result outlives the
window's language setting, and a chat switched to English still has to recognise the German
notice in its own transcript.

Neither trailer is found by its **first** occurrence, and that was a bug: this app's own
`tools/project.go` writes both of those sentences, so reading it cut the body at the quote and
handed the last 157 lines over uncondensed. What tells framing from content is the numbering —
every line of the body starts with `%6d| `, so only real framing starts a line — and that is
what `guidanceStart`/`continuationStart` check, from the end. The first version also matched
`<project_agents_md>` complete, which never appears because the tag carries `path=`; that one
was caught by `TestTheFoldersRulesSurviveACondensation`, and the quoting bug by
`TestAFileThatQuotesTheMarkersIsNotCutAtThem`.

**A file's escape hatch is a bounded read, so the ledger does not apply.** `digest`'s contract
for a page is that asking again returns the full text (the `condensationLedger`), which works
because the second scrape is a new result. For a file that rule is wrong in both directions:
the way to the text is `read_file` with an `offset`/`limit` — never condensed — and exempting
files after their first condensation would mean a *routine* re-read hands over 20k tokens, once
per turn, for the rest of the chat. So `rereadIsEscape` is true only for `digestJobPage`, and a
repeated read is condensed again from the cache: same job, model, cap and text is one key, so
the repeat costs a lookup and no request. Pinned by
`TestARepeatedReadIsCondensedAgainFromTheCache` and, for the page half, by the unchanged
`TestTheSecondReadIsTheEscapeHatch`.

The cache key carries the job as well as the model, the prompt version, the cap and the text
(`digestKey`): the same words can be a page and a file, and the answers are not
interchangeable. Adding it changed every key once, so the first read of a page or a file
after that change missed the cache — a miss costs one request, while a shared entry would
answer a question about a source file with a summary of a web page.

**The other half of shunt is not here.** Spotify's plugin also routes boilerplate
*generation* to the cheap model, writing the file straight to disk with the chat model never
seeing it. That trade is not made here: the text a change consists of is what `edit_file`
carries and what the person reviewing it reads, and a file nobody read is more expensive than
the tokens it saved.

Nothing new is configurable: `Digest.Model` empty is still **off**, `Digest.Threshold` still
decides what is worth a second request, and `Backend.Digest` still gates it per backend. On a
single-GPU setup the condense model competes with the chat model for VRAM, which is the
reason the feature stays off until somebody picks a model, and the settings hints say so.

---

## 18. Ageing a reply's older tool results

The third answer to "this request is too large", beside the fold (§5) and condensing (§17):
stop *re-sending* what the reply has already read. It lives in `ageToolResults`
(`internal/chat/toolhistory.go`), is called by `Assemble`, and is not a setting.

**Why it exists.** A tool result is the only part of a coding reply that grows without bound:
each round adds a file window, a build log, a search, and every *later* round re-sends all of
them. The fold does not help, because the growth happens *inside* one answer, between folds —
with a 1M-token window and the 0.80 default, a reply may reach 400 000 tokens and re-send that
on each of up to sixty rounds. The rule is the difference between "the newest windows" and
"everything ever read". This is arithmetic from the two constants, not a measurement: the app
still has no per-round token ledger, so what a session of this shape really cost is read from
the backend's own accounting rather than from here.

**The rule.** Within the reply being answered — the messages after the newest user message —
tool results are kept verbatim newest-first up to `toolsKeptVerbatimTokens` (32 000, roughly the
last half-dozen file windows), and anything older than that budget and larger than
`toolStubMinTokens` (600) is replaced by one sentence:

> `[Tool-Ergebnis von read_file: 4 812 Tokens, aus Platzgründen aus dieser Anfrage entfernt.
> Der Text bleibt im Verlauf gespeichert; rufe das Tool bei Bedarf erneut auf.]`

A short result is never dropped: an error line or a test summary is cheap to keep and is often
the thing that matters, and a notice in its place would cost more explanation than it saves.
Earlier turns are untouched — what an earlier answer was based on is compaction's to summarise,
not this reply's to rewrite.

**A re-read replaces the read before it.** Ageing alone would keep five copies of one file for
as long as the reply stays inside the 32 000 budget, which is exactly the state a reply reaches
when it checks its own work: read, edit, read again, edit again. `supersedeReads` therefore runs
before the budget is counted, and leaves out every read whose lines a later read of the same
file covered as well:

> `[Tool-Ergebnis von read_file (internal/chat/engine.go, Zeilen 1–500): 4 812 Tokens. Eine
> spätere Lesung in dieser Antwort hat denselben Bereich abgedeckt; weiter unten steht der
> aktuelle Text, hier wurde der ältere Stand weggelassen. Brauchst du ihn doch, lies den Bereich
> erneut.]`

The rule is **coverage of lines, not the path**: a 2 000-line file is read in windows, and the
window at line 1 001 is not a replacement for the window at line 1. The window is taken from the
call's arguments — with the read's own resolution of a missing `offset`/`limit`, which is why
`workspace.DefaultReadLines` is exported — and never parsed out of the result, whose header a
window cut short by `MaxReadBytes` or by the end of the file does not carry. An `outline` is a
window in neither direction: a declaration list must not replace the text it exists to locate,
and a window must not replace it either. A result under `toolStubMinTokens` is left in place,
like an aged one.

**A correction, not only a saving.** The older copy is not merely redundant, it is wrong: it is
the file as it was before the edit the model just made, standing beside the copy that is the file
as it is. A model quoting the wrong one re-fixes what it has already fixed, which is the shape
of failure that costs a whole round of edits to undo.

**An edit moves past the read it changed.** The rule above only helps a model that re-reads. The
shape described in the last paragraph is the other one: read a window, edit a passage in it, edit
another below, never look again. `markStaleReads` therefore walks the reply in order and, when an
`edit_file` or `write_file` result names a file already read in that reply, marks the window —
differently per tool, because the two lose different amounts:

- `write_file` replaces every line, so the read is stubbed like a superseded one. Nothing is lost
  by that, because the new text is already in the request as the call's own arguments, which is
  how the model sees its own write; the file's token cost is simply not paid twice.
- `edit_file` replaces a passage and leaves the window around it correct. Evicting it would take
  away the lines the *next* edit is copied from and force a re-read round — more tokens than the
  sentence costs, spent to fetch text the model just had. So the read stays, with one line in
  front of it naming the file that changed and pointing at the changed passage in the edit's
  result below. In front and not behind, because a model copies the passage it is about to change
  while it reads, and a warning after five hundred lines arrives after the mistake.
- None for a `run_command`, although one can rewrite anything (`gofmt -w .`): it is also the tool
  that usually changes nothing — a test run, a listing, a search — and a warning on most windows
  of a reply is noise. Its read memory is still dropped (`Service.forgetAllReads`), so a model
  that does re-read gets the disk.

A result that did not run marks nothing and counts as no read: a card the user **denied**
(`ToolDenied`) and an edit that could not find its passage (`ToolError`) changed no file, and a
model told otherwise either re-reads text it still has or believes a change landed that never
did. `Assemble` hands the pass the store's status for exactly that — a third index-aligned array
beside `currentReply` — and only `ToolOK` counts.

A marked read is not a lost one: the text is still in the request, so it is not in `AgedReads`
and its shortcut stands. A stubbed rewrite is reported like any other dropped read. In both cases
a re-read returns the file, because the write tools drop the turn's read memory themselves
(`Service.forgetReads`). The sentence is charged to the history segment rather than reported as a
saving, so the meter keeps describing what is sent.

**A result is not rationed against the request.** Ageing bounds what a reply *re-sends*; the
other bound used to be the room the request that reads the result had left, measured every
round against the effective limit, the reserve and the reply just written. It was removed, for
the reason the cards showed: it withheld the results the turn had just asked for — a page, a
read, even a 24-token `Replace text` acknowledgement — and what it protected against is a
refusal the turn recovers from anyway. A result is now bounded by two things only: the tool
service's own byte cap (`tools.MaxToolResultBytes`, 32 KiB, both ends kept) and ageing.

**The refusal is the recovery.** A request the backend refuses for its own size is folded and
sent once more (`foldForSize`, `oversize.go`), which is the shape opencode calls "compact and
retry once". The cost of that trade is real and is pinned by
`TestAToolResultIsNotRationedAgainstTheRequest`: on a backend whose per-request ceiling is far
below its window — Groq's per-minute allowance is the one that case was written for — a page
larger than the ceiling now makes the next request oversized instead of arriving as nothing.
The turn then either folds (if there is older history to fold) or ends with the refusal, which
names the limit. That was chosen deliberately over silent withholding: a run that cannot
verify its own edits is worse than one that stops and says why.

The earlier shape is worth remembering because it looked reasonable at the time. A 65 536-token
window, the default 1 024-token reserve, a 5 419-token reply, and a card reading
`933 → 777 tokens (context)`; then `3 975 tokens discarded` for the `app.go` read that followed,
by which point ageing had brought the request down to 44 694 where 14 399 tokens were free. The
high-water mark that caused that was replaced by a per-round measurement, and the measurement
itself is gone now: the room was computed correctly and the round still had to be told "no".

**The one number that survives.** The tool service's own byte bound, because it is a fact
about a result rather than about a window: a full 24 kB `read_file` prices at about 7 800
tokens and a 30 kB command result at about 10 800 (the app's own 64 kB command measurement,
halved), and no single result may exceed `tools.MaxToolResultBytes` (32 KiB, measured at
10 347 tokens of source), applied once where the result leaves the tool service. That bound
is pinned by the tool service's own test (`TestOneResultCannotExceedTheCap`), because it is a
property of a result and not of a window.

**The coupling that makes it safe.** Dropping a `read_file` result would otherwise be a trap:
the app remembers which file each turn has read, and a model asking again for a range it already
read is answered "unchanged — use a different offset". That is true about the transcript and
useless about the request, because the request no longer carries the text. `Engine.buildAssembly`
therefore forgets the read for exactly the files that are in **no copy at all** — stubbed by
ageing, replaced by a later read, or both (`Service.ForgetReads`, `internal/tools/reads.go`) — so
the ordinary way to refresh a window works again. A file whose newer read is still in the request
is deliberately not forgotten: there the shortcut's answer is true about this request, and a
re-read would spend a round fetching text the model was just sent. Every other read keeps the
shortcut as well.

**What does not change.** The stored rows and the tool cards keep the full record; the rewrite
is on the request only. The saving is taken off the history segment, so the meter keeps
describing what is sent.

**Pinned by** `TestOldToolResultsAreAgedOutOfTheRequest` (oldest dropped, newest kept, calls
still pair), `TestAToolResultFromAnEarlierTurnIsNotAged` (scope), `TestAShortToolResultIsNeverAged`
(the floor), `TestARepeatedReadReplacesTheOneBeforeIt` (five reads of one window cost one),
`TestOnlyACoveringReadReplacesAnEarlierOne` (coverage, not path; an outline is untouched),
`TestAFileIsForgottenOnlyWhenNoCopyOfItIsSent` (one `ForgetReads` per path, and only then),
`TestAnEditMarksTheReadItMovedPast` (marked, not evicted, and only once),
`TestAWholeFileRewriteReplacesTheRead` (the case where evicting loses nothing),
`TestAReadAfterTheEditIsCurrent` (direction), `TestADeniedEditMarksNothing` and
`TestAssembleMarksAReadAnEditMovedPast` (the status, and that the index-aligned flag survives
assembly) and
`TestAssembleAgesTheReplyItIsAssembling` (the boundary survives the assembly
loop, which skips messages and inserts the system message at position zero).
