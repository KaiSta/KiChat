# PRECOMPUTE.md

Background precompute for compaction: starting a fold's summary ahead of the threshold and
swapping it in when the fold comes due. **Status: not implemented.** This is the plan, the
case for it, the case against it, and what would have to be measured before it is worth
building. The subsystem it would change is described in [`COMPACT.md`](COMPACT.md); that is
where the fold's arithmetic and failure modes live, and this document does not repeat them.

**Contents:** 1 where a fold runs today · 2 what would change · 3 the turn claim, which is
the crux · 4 validity at use · 5 the gates · 6 what it buys · 7 what it costs · 8 what to
measure first · 9 the decision.

---

## 1. Where a fold runs today

Every automatic fold happens inside somebody's turn, in the send path:

| caller | when | claim |
| --- | --- | --- |
| `autoFold` (`internal/chat/maintenance.go`) | before every send, and again between tool rounds when `autoFoldThreshold` says so | none of its own — the turn already holds the chat |
| `Compact` (`maintenance.go`) | pressing the button | claims the chat, so Stop reaches it and a message cannot land beside it |
| `DeleteCompaction` | undo | claims, for the same reason |

`autoFold` decides nothing itself: `PlanCompaction` reads the threshold and the backend's
published request ceiling as two bounds of one decision. When one fires, `compactWith` runs
the whole fold — `Assemble`, the plan, `Summarize`, `CreateCompaction`,
`InvalidateMeasurement` — and the turn waits for it.

That wait is what precompute is about. On a CPU-hosted llama.cpp server a fold is minutes;
on a hosted backend it is the round trip of a large request. Either way it is paid at the
worst moment: at the top of a turn or between two tool rounds, with the user watching.

## 2. What would change

A third state between "no fold" and "fold":

```
              projection >= ratio * threshold
   no fold ─────────────────────────────────────► holding
      ▲                                             │
      │  invalidated (model, undo, clear)           │ fold comes due
      └─────────────────────────────────────────────┤
                                                    ▼
                              swap in if still valid, else drop and fold normally
```

- **Start.** `autoFold` runs on every send. When it finds no fold is due but the projection
  is above a start ratio, it begins a summary in the background for the range a fold would
  cover — planned with `Force: true, Target: threshold, ProtectNewestTurn: true`, so the cut
  lands where the real fold will land and the newest turn is never folded.
- **Hold.** The text lives in memory. No `Compaction` row is written, because a row is
  itself an instruction to stop sending the messages it covers (`FirstVerbatimSeq`): storing
  early would fold the history early, which is a different feature and a worse one.
- **Swap in.** When a fold comes due, `compactWith` checks the held summary against the
  plan it just made. If it holds, the provider call is skipped entirely and the row is
  written with the held text. If it does not, it is dropped and the fold runs as it does
  today.

The swap-in is where the saving is: on the crossing turn the fold costs a store write
instead of a summarisation. When the held range is shorter than the due range — the usual
case, since messages arrived after it started — the messages after it stay verbatim, which
is what the kept tail is for. opencode allows that tail up to twice `keep` (at least
30 000 tokens) beside a swapped-in summary; §5 argues for starting stricter than that.

## 3. The turn claim, which is the crux

The chat's claim (`TurnState`, `turnHandle`) is what makes Stop reach a fold, keeps two
turns from interleaving, and keeps a message from landing beside one. A background run must
not hold it: the user's next message would be refused (`chat.compactionWhileRunning`) for a
fold they did not ask for. So the two designs are:

**A — a second claim kind.** `TurnState` grows a read-only "background" claim that a turn
may hold simultaneously but a background run may not. Correct, and it buys nothing the
validation in §4 does not already have: the failure it prevents is a fold summarising a
range that a concurrent turn is rewriting, and a turn only ever *appends* (and rewrites its
own streaming reply in place), both of which are newer than any range a precompute takes.

**B — no claim, a snapshot, validate on use.** Chosen. The run takes its plan and copies the
messages in that range up front, then renders and summarises from the copy. Appends during
the run cannot move that range, and anything that *can* invalidate it is caught at use.

Constraint B does not remove: **the background run outlives an engine rebuild.** The app
builds engines over one shared `TurnState` so Stop still finds a running turn after a
settings change. A held summary and its goroutine have to be in that shared state too (or be
cancelled on rebuild), or the app keeps a summary computed by a provider that no longer
exists. That is the single most likely bug in the whole feature, and it is why this is not a
small change.

## 4. Validity at use

The swap-in is only allowed when every row holds. Anything else drops the held text and
folds normally — a dropped summary costs the work already spent, never a wrong memory.

| condition | why it can be false |
| --- | --- |
| `held.FromSeq == plan.FromSeq` | a fold happened since (`DeleteCompaction`, another auto fold), so the range starts somewhere else |
| `held.ToSeq <= plan.ToSeq` | the due range can only have grown; if it shrank, the history was rewritten |
| `held.Backend/Model == the chat's summariser` | the model changed, and the held text is another model's answer |
| the messages after `held.ToSeq` fit `effectiveKeepTokens` | otherwise swapping in leaves a tail too large to be the fold we needed |
| no message in `[FromSeq..ToSeq]` was deleted | a delete does not change the count of later messages, only their seqs — check the range's first and last seq still exist |
| the chat was not cleared | `ClearChat` leaves no messages; the range cannot be named |

The first and last rows are cheap store lookups. The one that needs care is the tail: the
walk in `PlanCompaction` already prices the kept history, so the swap-in can reuse
`plan.KeepFromSeq` — if `held.ToSeq + 1 > plan.KeepFromSeq`, the held summary covers more
than the fold asked for and the tail would be larger than planned.

## 5. The gates

| gate | opencode | proposed here | reasoning |
| --- | --- | --- | --- |
| start ratio | 0.85 of the threshold | 0.85 of the threshold | nothing better to copy; it is far enough ahead to finish, close enough to be current |
| minimum size | 64 000 measured prompt tokens | **none at first**, then tune | their gate exists because a summary below that is not worth a background slot. KiChat's local windows are 8k–32k, so their number would mean the feature never runs where folds are frequent |
| kept tail beside a swap-in | 2 × `keep`, ≥ 30 000, ≤ ¼ of the budget | `effectiveKeepTokens` unchanged | their 30 000 floor is a share of a 200k window; copying the constant would keep more history than the tiny window folds are trying to shed |

The minimum-size gate is the one that needs an answer from measurement rather than from
opencode: a fold that costs seconds (hosted) and a fold that costs minutes (CPU) want
different gates, and the same setting cannot serve both unless it is per-backend.

## 6. What it buys

1. **Latency at the crossing turn.** The obvious one, and the only one that is purely
   performance: the fold stops being inside the turn.
2. **Room for a better summary.** A fold fires at the threshold, so the request that
   describes the history has to fit in what is left below the window — that squeeze is what
   drives the transcript ladder, the block clipping, and the refusal to take the prefix path.
   Started at 0.85 of the threshold, the same fold has a much emptier window and can carry
   the prefix unchanged (`COMPACT.md` §8.1) instead of a distilled transcript. That is a
   quality gain, not a speed one, and it is the strongest argument for the feature.
3. **A warmer prompt cache.** The summary runs right after the turn that sent the same
   prefix, so the prefix is a hit rather than an eviction. It makes the summary itself
   cheaper — on a local server by not re-evaluating the history, on DeepSeek by billing more
   of it at the cache-hit rate.
4. **A decoupled threshold.** Once the fold is nearly free at the crossing, the user's
   threshold stops being a scheduling compromise and becomes a comfort setting. Today 0.8
   is partly "how much interruption is worth it".

It does **not** improve the memory's content: same prompt, same rules, same model, and a
staler view of the history by construction.

## 7. What it costs

- **Contention on a single local GPU.** Precompute moves work off the critical path only if
  the model is idle. On one llama.cpp server the user's next reply queues behind the summary.
  A feature that speeds up every fifth turn and slows down every other one is not obviously
  a win, and this is the argument for gating it to hosted backends first.
- **A stale tail.** The swap-in leaves more history verbatim than a same-moment fold would.
  Whether that is good (fidelity) or bad (a request nearer the limit) depends on the window.
- **A new invalidation surface.** §4 is six conditions and a store read. Every one of them
  is a way to write a memory that does not match the range it claims.
- **A background goroutine with a lifetime.** §3. It must be cancellable, and it must not
  paint "compacting" in the UI while the user is typing — `emitStatus` currently says exactly
  that, and a precompute is not a fold the user asked for.
- **Two more reasons for the fold to differ from the plan.** A held summary that is used
  makes the stored `TokensBefore/After` describe a different range than a fresh fold would;
  the card in the transcript has to stay honest about which messages it covers.

## 8. What to measure first

Nothing here should be built before the first two rows exist, because they decide whether
the feature is worth its risk at all:

| measurement | how | what would justify building |
| --- | --- | --- |
| time a fold adds to the crossing turn | a timed `Compact` on a real 8k/32k local model, and on DeepSeek | minutes locally, or more than a few seconds hosted |
| how often the prefix path is refused for room | log the `usePrefix` decision and the reason over a long session | "refused for room" is common, which is what gate 2 in §6 is worth |
| prompt cache hit share on a fold | the reply's *Cache hits* (`DEEPSEEK.md` §5) on a fold vs on an ordinary turn | a fold that is mostly misses today |
| whether precompute would fire at all | projection at every send, histogrammed per window size | it fires at least once per long session on the models people use |

Acceptance criteria if built: on a 32k local model, the crossing turn completes without a
provider call; a held summary is dropped (and the fold still succeeds) when the model is
switched, when a compaction is undone, and when the chat is cleared; and the kept tail after
a swap-in is within `effectiveKeepTokens` of the plan's.

## 9. The decision

**Not now.** The honest summary:

- The one unambiguous gain is latency, and on the local backend KiChat is built for, it
  trades that gain against slowing the *next* turn — the same GPU, the same server.
- The one gain that is not about latency at all (§6.2, more room for a lossless summary) is
  real, and it is also the one that can be had for much less: it is a question of *when* the
  threshold is crossed, and the same effect could be approximated by lowering the threshold
  on backends where a fold is cheap, with no new state at all.
- The cost is a background goroutine that has to survive an engine rebuild, a six-row
  validity table, and a UI state that is neither "compacting" nor idle.

If it is built anyway, the smallest version worth trying is: **hosted backends only, above a
per-backend minimum, one held summary per chat, no changes to `TurnState`.** That version
needs §3's shared-state decision and §4's table, and nothing else — and if it turns out the
local case is where the pain actually is, the answer there is a faster summariser, not more
machinery.

### Out of scope

- Compacting in the background while a turn runs (that is design A plus a queue, and it is
  a different feature: it changes what a turn is allowed to do, not when a fold starts).
- Pruning tool outputs in place between folds. opencode's V2 dropped it too: the token
  tail already covers the shape it was for.
- Provider-native compaction. No backend KiChat supports offers one.
