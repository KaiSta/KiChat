# NVIDIA.md

How the build.nvidia.com backend works in KiChat: the shapes the service answers with, the
decision they force, and what is verified against the live catalogue.
`PLAN.md` names the backend in one line; this document is the detail behind it. It is a
sibling of `MISTRAL.md` and `DEEPSEEK.md`, which do the same for the other hosted services.

Everything here was read out of the code and, where marked *measured*, observed against the
live API with a real `nvapi-` key. Where a claim comes from the vendor's documentation
rather than from a request, it says so.

**Contents:** 1 what this adds · 2 turning it on · 3 the listing and the catalogue problem ·
4 the stream · 5 refusals · 6 where the code is · 7 how this is verified · 8 known gaps.

The code references are `file:line` for the revision this was written against. The symbol
names beside them (`newNvidiaProvider`, `fromNvidiaEntry`, `flavourNvidia`, …) are the
durable anchor when the numbers drift.

---

## 1. What this adds

NVIDIA serves the build.nvidia.com model catalogue through a NIM endpoint that is
OpenAI-compatible, so **nothing in the chat path is new**: streaming, tools, `max_tokens`,
`reasoning`/`reasoning_content` and `stream_options.include_usage` all go through
`openAICompatProvider` unchanged. What is NVIDIA's own is the address, the fact that it
cannot answer without a key, and a model listing that is sparse in a way no other backend's
is — which is the whole of §3.

The work is one file ([internal/llm/nvidia.go](../internal/llm/nvidia.go)) plus a
`flavourNvidia` branch in the shared listing mapper, a shipped-but-disabled entry in
`DefaultBackendConfigs`, and the settings wiring. It follows the shape DeepSeek established:
`requiresKey`, `ErrMissingAPIKey` reported instead of attempting a call with no credential,
and `State: loaded` on every model because a hosted model answers without a load step.

| what | why it could not be shared |
| --- | --- |
| The model listing | It carries four fields and no capability or window information at all |
| The base URL | `https://integrate.api.nvidia.com/v1` — the `/v1` is part of the address |
| The key | Same rule as DeepSeek and Mistral; the entry exists so a key has somewhere to go |

---

## 2. Turning it on

NVIDIA ships **disabled**, the rule every metered hosted backend here follows: enabling it by
default would probe a paid API on every health refresh without being asked, and — because a
hosted model is ready the moment a key is accepted — its models would win the loaded-model
race and quietly become the default for new chats.

To use it: **Settings → Backends → NVIDIA**, paste the key, tick the box, Save. The key goes
to the secret store through `llm.BackendAPIKeyName("nvidia")` (`internal/secret`,
`~/.config/kichat/`) and never into the settings blob. Keys are free to create at
`build.nvidia.com`, and the field is marked required because nothing works without one.

There is no migration. `normalise()`
([internal/config/config.go:491](../internal/config/config.go#L491)) appends the shipped entry
to a settings blob that predates it and repairs an empty address from
`llm.NvidiaDefaultBaseURL`, which is also what the settings field displays so it shows the
address the app would actually use.

The same entry serves a **self-hosted NIM container**: point the address at it (the vendor's
default is `http://localhost:8000/v1`). That is an address edit rather than a different kind,
so the base URL is only completed with `/v1` when what was typed is a bare host —
`isBareHost`, the rule Mistral established. A base URL that already carries a path is left
exactly as typed.

---

## 3. The listing and the catalogue problem

`GET /v1/models` answers **80 entries, all ids distinct** *(measured)*. Every entry carries
exactly four fields, and `created` is the same placeholder value on all 80:

```json
{"id":"openai/gpt-oss-20b","object":"model","created":735790403,"owned_by":"openai"}
```

`GET /v1/models/<id>` is **not** a richer route — it answers the same four fields
*(measured)* — so there is no per-model metadata to fetch either. `fromNvidiaEntry`
([internal/llm/nvidia.go:121](../internal/llm/nvidia.go#L121)) maps what there is:

| field | becomes | note |
| --- | --- | --- |
| `id` | `ModelInfo.ID` | the fully-qualified name, `publisher/model` |
| `owned_by` | `Publisher` | also the id's first half |
| — | `State: loaded` | hosted: it answers as soon as the request is authenticated |
| `max_model_len` | `ContextLimit` | a self-hosted NIM (vLLM) states the window it serves; the hosted route sends no such field, so a hosted model stays at 0 |
| — | `CapTools` | see below |
| `object`, `created` | *not decoded* | `created` is a constant, so neither carries information |

### The listing is the global catalogue, not your entitlement

This is the fact that shapes the provider. One request per model at `max_tokens: 1`
*(measured)*:

| outcome | count | what it is |
| --- | --- | --- |
| HTTP 200 | 16 | a working chat endpoint |
| HTTP 404, `application/problem+json` | 45 | listed for everyone, callable only by an entitled account |
| HTTP 404, bare text `404 page not found` | 9 | no chat route exists — the embed/retrieve models |
| no response within 90 s | 6 | hangs at this budget |
| HTTP 500 / 502 / 400 | 4 | a broken or non-text input model on the vendor's side |

The 45 are the important ones, and their body is the whole story:

```json
{"status":404,"title":"Not Found","detail":"Function '04174188-f742-4069-9e72-d77c2b77d3cb': Not found for account 'OCCn-…'"}
```

**"Not found for account"** — so the catalogue lists what exists, and whether a model answers
depends on the account, not on the model. A second key reaches a different subset.

The nine bare-404s are a separate group: no chat route exists for them. They look like the
embeddings (`nvidia/nv-embedqa-*`, `nvidia/embed-qa-4`, `snowflake/arctic-embed-l`,
`nvidia/nvclip`), but `bigcode/starcoder2-15b` is in the set too — a chat model — so the
group is *"the chat route is absent"*, not *"the embeddings"*, and cannot be described by a
rule about the id.

### The decision: the provider filters nothing, and the app asks

**The provider drops no model**, and that is deliberate — it is the one place it diverges
from Mistral. Mistral can drop what cannot chat because its listing publishes
`capabilities.completion_chat` per model ([MISTRAL.md](MISTRAL.md) §3). This listing
publishes no capability of any kind, and the models that fail are refused *per account*.

So a filter written into the provider could only be a guess about entitlement, and the guess
would be wrong in the direction that matters: it would hide a working model from the account
that owns it. The row is offered, and a model the account cannot call fails on send with the
vendor's own message — which `HTTPError` carries to the toast verbatim.
`TestNvidiaAccountRefusalKeepsTheVendorDetail`
([internal/llm/nvidia_test.go:268](../internal/llm/nvidia_test.go#L268)) pins that, because the
status alone would read as "the backend is broken" rather than "this model is not yours".

The cost of that decision is a picker longer than the list of models that will answer, and it
is why the availability check below exists: the same question, answered with evidence instead
of a guess, on the machine that owns the key, at the user's request.

### The availability check: the catalogue, measured per account

**Nothing but asking can find this out.** A metadata route cannot: `GET /v1/models/<id>`
answers HTTP 200 with the same four fields for *every* entry — including for
`nvidia/nemotron-4-340b-reward`, which 404s on `chat/completions` — and it does so **with no
API key at all** *(measured)*. It is a public catalogue lookup, blind to both capability and
entitlement, so no per-model call can predict whether a model will answer. No account, usage
or entitlement route exists either: a search of the vendor's documentation and developer
forum found none, and the forum has users asking this same question unanswered.

What works is the sweep §3's table was measured with: **one `max_tokens: 1` request per
model**. Measured cost of the full 80-model sweep: **~52 seconds at six workers, and roughly
16 tokens in total** — a model this account may not call is refused before it generates
anything, so the requests that fail are close to free. Six models hang rather than answer, so
a per-model timeout is part of the procedure and not an error case.

The verdicts are deliberately not symmetric, and this is the part that matters:

| what came back | verdict | hidden? |
| --- | --- | --- |
| a stream, however short | `ok` | no — and an entry already hidden is removed |
| HTTP 404 (both shapes: `problem+json` and bare text) | `refused` | **yes** |
| HTTP 403, other non-retryable 4xx | `refused` | **yes** |
| HTTP 429, 5xx, connection failure | `inconclusive` | no — the entry keeps the verdict it had |
| no answer within the timeout | `timeout` | no — same |

Only a non-retryable refusal is evidence about the *model*. A throttle and a server error are
things that happen to a working model, and treating either as a verdict would blacklist a good
model for as long as nobody noticed — the failure mode that makes an automatic filter worse
than no filter. `TestProbeSeparatesARefusalFromATemporaryFailure`
([internal/llm/probe_test.go:90](../internal/llm/probe_test.go#L90)) pins the whole table,
including that a refused model is asked **once** while a throttled one carries the provider's
own retries, and that a stopped sweep leaves the models it never reached with *no* verdict at
all rather than with "not usable".

One guard keeps the sweep from being destructive: **if every model it reached was refused,
nothing is applied.** A wrong address or a key without entitlement refuses each model exactly
the way an unentitled account does, so that reading would empty the picker over a
misconfiguration. It is reported as its own verdict (`ReasonAllRefused`) instead, and
`TestASweepThatRefusesEverythingHidesNothing` pins it.

The result is stored, not re-derived: `HiddenModels` in the settings blob, keyed
`backend/model` the way `ModelOverrides` is, because a check that costs 80 requests must
survive a restart. It is consulted in `App.visibleModels`
([app.go:761](../app.go#L761), the filter itself at [app.go:773](../app.go#L773)) rather than in
the provider — every listing path goes through
that one function, so one filter covers the picker, the budget preview and the default model
of a new chat, and the provider keeps reporting what the vendor really publishes. Hiding is a
statement about the picker, not a revocation: a chat already pinned to a hidden model still
resolves it, which `TestTheAvailabilityCheckHidesWhatTheAccountCannotCall` asserts on both
lists.

No control in Settings edits the blacklist, so no draft the panel sends carries one;
`SaveSettings` reads the recorded list instead ([api.go:84](../api.go#L84)), the same way it does
for the installed llama.cpp build. Without that, saving the theme would put every hidden model
back. `TestAnUnrelatedSaveKeepsTheHiddenModels` is the test.

In the picker it is one button per backend heading, because the answer is per backend and one
vendor's catalogue needs it while a local server's does not. It shows `done/total` while it
runs, the footer shows how many models are hidden with a one-click undo
(`ClearHiddenModels`), and the sweep's own verdict gets a sentence per language — a *reason*
travels beside the backend's raw text rather than instead of it, so a German sentence never
appears in an English or Chinese window. The harness renders all of it: the fixture starts
with two models hidden so the footer is on screen, `?check=clear` is the zero case and
`?check=error` is the refusal above.

### Tools are claimed, thinking and vision are not

Tools are appended unconditionally
([internal/llm/nvidia.go:121](../internal/llm/nvidia.go#L121)), the same claim DeepSeek makes
and for the same reason: `engine.go` only uses the native tool protocol for a model that
claims it, so leaving the claim off would silently cost every model here its tools. The live
test is what keeps the claim honest — it asserts a real tool call streams.

`CapThink` and `CapVision` are **not** claimed. The catalogue contains both vision models and
models that never reason, and there is no field to tell them apart; claiming either for all of
them would be wrong for most, and claiming vision would send images to a model that cannot
read them. A model that reasons still streams it — the delta parser reads the field
regardless of the claim, which only decides the badges and whether compaction may try to
switch thinking off.

---

## 4. The stream

Plain OpenAI-style SSE, verified against `openai/gpt-oss-20b` and `z-ai/glm-5.3-flash`
*(measured)*:

- `stream_options.include_usage` is honoured: a final frame with an **empty `choices` array**
  carries `prompt_tokens`/`completion_tokens`/`total_tokens`. That is what the context meter
  needs to show a measured value instead of an estimate.
- **Reasoning arrives twice in the same delta** — in `reasoning` *and* in `reasoning_content`,
  with identical text:

  ```json
  {"choices":[{"delta":{"reasoning":"The user says…","reasoning_content":"The user says…"}}]}
  ```

  The shared `openAIDelta.reasoningText()` prefers `reasoning` and returns one of them, so the
  thinking panel shows the words once. This is the trap on this API: a parser that
  concatenated the fields would double every reasoning model's thinking in the UI while the
  answer text — the thing a reader checks — still looked right.
  `TestNvidiaStreamsReasoningWithoutDoublingIt` pins it.
- A reasoning model can spend its **entire budget** on thinking and return a `null` `content`
  with `finish_reason: "length"`. The first probe here did exactly that inside a 40-token cap.
  This is the case `Usage.ReasoningTokens` exists to explain, and it is why the live test asks
  for 4096 tokens to get one word back.
- Tool calls arrive as `tool_calls` with `finish_reason: "tool_calls"`. Measured, this
  catalogue returned the whole call in a single frame rather than fragmented; the
  reassembly path is pinned offline against fragmentation anyway.
- No `stats` object and no llama.cpp-style `timings` block, so the app derives tokens/s and
  time-to-first-token itself and badges them `estimated`.

---

## 5. Refusals

| what | what happens |
| --- | --- |
| no key configured | `ErrMissingAPIKey`, and **no request is sent** — the vendor's 401 body says nothing about where a key belongs |
| a bad key | the vendor's HTTP 401 body, carried verbatim |
| a model the account is not entitled to | HTTP 404 with the problem+json body in §3 |
| a model with no chat route | HTTP 404, body `404 page not found` |
| a model that is not text-input | HTTP 400 with `{"object":"error","message":"Content cannot be a plain string. The model does not support text input.",…}` |

The last two are why a picker row here can fail on the first send. Nothing in the listing
distinguishes them (§3), so the message the vendor returns is the only signal, and it is
passed through unedited.

---

## 6. Where the code is

| file | symbol | what it does |
| --- | --- | --- |
| [internal/llm/nvidia.go:67](../internal/llm/nvidia.go#L67) | `NvidiaDefaultBaseURL` | `https://integrate.api.nvidia.com/v1` — the `/v1` is part of the address |
| [internal/llm/nvidia.go:71](../internal/llm/nvidia.go#L71) | `newNvidiaProvider` | the provider: one chat path, `/models`, `flavourNvidia`, `requiresKey`, and the bare-host completion |
| [internal/llm/nvidia.go:110](../internal/llm/nvidia.go#L110) | `nvidiaModelEntry` | two fields read of the four sent; `object` and `created` are deliberately not decoded |
| [internal/llm/nvidia.go:121](../internal/llm/nvidia.go#L121) | `fromNvidiaEntry` | one entry → `ModelInfo`. No second return value: there is nothing to drop |
| [internal/llm/nvidia.go:157](../internal/llm/nvidia.go#L157) | `mapNvidiaEntries` | entry by entry; an entry with no id is skipped rather than offered nameless |
| [internal/llm/openai.go:127](../internal/llm/openai.go#L127) | `flavourNvidia` | the listing flavour, dispatched at [openai.go:274](../internal/llm/openai.go#L274) |
| [internal/llm/registry.go:115](../internal/llm/registry.go#L115) | `Reconfigure` | `case "nvidia"` builds the provider |
| [internal/llm/registry.go:418](../internal/llm/registry.go#L418) | `DefaultBackendConfigs` | the shipped row: `nvidia`, disabled |
| [internal/config/config.go:491](../internal/config/config.go#L491) | `normalise` | restores the address for an entry saved without one |
| [frontend/src/components/Settings.tsx:1412](../frontend/src/components/settings/BackendsTab.tsx) | the backend row | `kind === 'nvidia'` gets the key field, with the field marked required |
| [internal/llm/probe.go:82](../internal/llm/probe.go#L82) | `ProbeModels` | the sweep: one token per model, bounded workers, the listing's order kept |
| [internal/llm/probe.go:159](../internal/llm/probe.go#L159) | `classifyProbeFailure` | §3's verdict table in one function — retryable first, then the refusal |
| [api.go:1274](../api.go#L1274) | `StartModelCheck` | starts the sweep and returns while it runs; `GetModelCheckStatus` ([api.go:1353](../api.go#L1353)) is the poll |
| [api.go:1384](../api.go#L1384) | `applyModelProbes` | the verdicts, the all-refused guard, and the write to the settings blob |
| [api.go:1461](../api.go#L1461) | `ClearHiddenModels` | the undo behind the footer's button |
| [app.go:773](../app.go#L773) | `visibleModels` | where the blacklist is applied — one chokepoint for every listing |
| [internal/config/config.go:50](../internal/config/config.go#L50) | `HiddenModels` | the stored set; `normalise` deduplicates it |
| [frontend/src/components/ModelSwitcher.tsx:402](../frontend/src/components/ModelSwitcher.tsx#L402) | `runCheck` | the button, the progress label and the footer, all in the picker |

`openAICompatProvider.modelsFlavour` gained `flavourNvidia`, and `mapModelEntries`
([internal/llm/openai.go:274](../internal/llm/openai.go#L274)) dispatches the whole list to
`mapNvidiaEntries` for that flavour rather than mapping entry by entry in place — the same
shape Mistral's branch uses, so the "whole list, or one entry" split has one home.

---

## 7. How this is verified

### Offline — `internal/llm/nvidia_test.go`

| test | what it pins |
| --- | --- |
| `TestNvidiaListingCarriesWhatThePlanningNeeds` | `Publisher` from `owned_by`, `State: loaded`, `CapTools`, no vision/thinking claim, and a window of **0** — the hosted listing reports none, so nothing may be invented |
| `TestNvidiaListingUsesTheWindowASelfHostedNIMPublishes` | a local NIM's `max_model_len` becomes `ContextLimit`; the hosted listing, which omits the field, is unaffected |
| `TestNvidiaLearnsTheWindowItsRefusalNames` | a refused request's window is remembered on the provider and appears in the next listing; a model that was never refused stays at 0 |
| `TestNvidiaListingKeepsModelsTheAccountMayNotReach` | the no-filter decision, pinned so it cannot be quietly reversed |
| `TestNvidiaListingSkipsEntriesItCannotRead` | one malformed entry does not hide the rest; an entry with no id is dropped |
| `TestNvidiaStreamsReasoningWithoutDoublingIt` | `reasoning` + `reasoning_content` produce the words **once**, plus content, usage and stop reason |
| `TestNvidiaStreamsAToolCall` | a fragmented `tool_calls` stream reassembles into one executable call |
| `TestNvidiaAccountRefusalKeepsTheVendorDetail` | a 404 problem+json reaches the user with "Not found for account" intact |
| `TestNvidiaBaseURLCompletionOnlyTouchesABareHost` | six base URLs: which are completed with `/v1`, which are left alone |
| `TestNvidiaWithoutAKeyIsNotAttempted` | `ErrMissingAPIKey` from `ListModels`, `Health` and `Chat`, with the server failing the test if reached |
| `TestRegistryBuildsNvidiaFromConfig` | the kind selects the provider, the default address, `requiresKey`, the flavour, that no other vendor's request names were borrowed, and that a disabled entry builds nothing |
| `TestDefaultBackendsShipNvidiaOff` | the shipped row exists, is disabled, and names the vendor address |

Two more files hold the check itself, and they are offline too — no key, no network:

| test | what it pins |
| --- | --- |
| `internal/llm/probe_test.go` §`TestProbeSeparatesARefusalFromATemporaryFailure` | §3's verdict table against a fixture shaped like the live service: the two 404 shapes are refused, a 429 and a 500 are *not*, a hanging model is a timeout — and only two of the six are `Refused()`. Also the cost contract: `max_tokens: 1` on every request, a refused model asked **once** (a 404 is not retryable), a throttled one carrying the provider's own retries |
| `internal/llm/probe_test.go` §`TestAStoppedProbeIsNotAVerdict` | a cancelled sweep sends nothing and leaves every model with an empty verdict, which `Refused()` reads as false |
| `model_check_test.go` §`TestTheAvailabilityCheckHidesWhatTheAccountCannotCall` | the whole path through the picker's own interface: the refused model disappears, the answered one does not, **the throttled one does not**, `hiddenTotal` is 1, the *registry* still reports the hidden model, and the undo brings it back |
| `model_check_test.go` §`TestASecondSweepOffersAModelThatAnswersNow` | a model that starts answering is taken off the list by the next sweep |
| `model_check_test.go` §`TestASweepThatRefusesEverythingHidesNothing` | the guard, including that the verdict is reported as `ReasonAllRefused` for the UI's sentence |
| `model_check_test.go` §`TestAnUnrelatedSaveKeepsTheHiddenModels` | the draft has `hiddenModels = nil` set by hand and the save still keeps the list |

### Live — `internal/llm/live_nvidia_test.go`

```bash
KICHAT_LIVE=1 KICHAT_NVIDIA_KEY=<key> go test ./internal/llm/ -run TestLiveNvidia -v
#   optional: KICHAT_NVIDIA_BASE, KICHAT_NVIDIA_MODEL
```

It asserts the catalogue is non-empty, that the chosen model is offered `loaded` with a
publisher and `CapTools`, that `stream_options.include_usage` produces a measured
`prompt_tokens`, that a real tool call streams — and, in the negative, that the reported
context window is **zero**. A number appearing there would mean the provider had started
inventing metadata for the planner.

Last run against the live catalogue: 80 entries, `openai/gpt-oss-20b` at `ctx=0`,
`caps=[tool_use]`, `state=loaded`, content `"OK"` with 116 characters of reasoning,
`prompt_tokens=70`, `stop=stop`, and `get_weather({"city":"Berlin"})`.

It deliberately does **not** walk the whole listing: that would spend 80 requests mostly
discovering refusals, and the subset a given key can reach is not a fact about the models.

---

## 8. Known gaps

1. **No context window from the hosted route, by design.** No route of the hosted API reports
   one, so every hosted model arrives with `ContextLimit: 0` and the per-model override in
   Settings is the first source — the same state a plain OpenAI-compatible server is in. The
   second is the deployment itself: when a request is too large, vLLM's refusal names the window
   it enforces (`This model's maximum context length is 131072 tokens`),
   `classifyRequestTooLarge` reads it into `RequestTooLargeError.Window`, and the provider keeps
   it so the rest of the session plans against it. That is what makes the recovery fold possible
   on a model that reported no window, and it costs only the request that was already refused.
   A self-hosted NIM needs neither: vLLM's listing states the window it is serving as
   `max_model_len`, which `fromNvidiaEntry` maps straight through. Seeding a table of known
   windows was rejected: it would be unmaintainable and would silently rot into wrong numbers
   presented as measured ones.

   What a zero window actually costs, precisely — none of it crashes, and none of it divides by
   zero (`context.go:349` and `compact.go:502` are both guarded), but it is not a no-op either:

   | what | with a zero window |
   | --- | --- |
   | the context meter | reads `unbekannt`; the inspector names the source `unknown`. No percentage and no over-limit warning |
   | search-first | the page-content budget is **not** capped to a quarter of the window, so the configured `LLMContextTokens` is used as it stands |
   | compaction, manual | **refused**: `Kontextfenster des Modells ist unbekannt` |
   | compaction, automatic | skipped silently — see below |
   | the model picker | the row simply shows no window next to the model name |

   The automatic row is the one that needed work, and finding it is what this backend
   contributed back to the app. `autoFold` runs before *every* send, and it used to
   forward the refusal it got from `compactWith`, which emits an `events.Warning` the UI turns
   into a toast — so a working NVIDIA chat showed *"Kontextfenster des Modells ist unbekannt"*
   on every single message, for as long as no override was set. The fix is in
   [internal/chat/maintenance.go:168](../internal/chat/maintenance.go#L168): an unknown window is a
   standing fact about the model, not an incident, so the automatic path returns before
   `compactWith` and leaves the condition to the meter and the inspector, while the manual path
   keeps the explicit refusal because there the user asked. `COMPACT.md` §4 and §13 record the
   same distinction. Before the fix, `TestAutoCompactStaysQuietWhenTheWindowIsUnknown` failed with
   exactly the warning quoted above.
2. **The picker is the catalogue until the check has been run.** Most entries will not answer
   for a given key (§3), and the availability check is what resolves that — but it is a button,
   not something the app does on its own, because it spends one request per model of the
   backend. A user who never presses it sees exactly the state described above. Two smaller
   consequences go with that: the check is per backend and per install, so a second key means
   pressing it again; and its verdict is a reading of one moment, so a model that is
   upgraded into reach still needs another sweep to appear. Both are covered by the undo and
   by re-running it, and neither is a reason to hide a model on a guess.
3. **`CapThink` is never claimed**, so compaction will not try to switch thinking off on a
   reasoning model here. The vendor publishes no "do not think" field for this API, so there
   is nothing to send; the cost is that a compaction against a reasoning model spends budget
   on thinking it cannot suppress.
4. **`CapVision` is never claimed**, so the vision models in the catalogue
   (`meta/llama-3.2-11b-vision-instruct`, `meta/llama-3.2-90b-vision-instruct`) are offered
   without the attachment path. The listing cannot say which models take images, and the
   per-model override can only correct a window and a capability list, not the type.
5. **`Usage.ReasoningTokens` stays 0.** This API sends no `completion_tokens_details`, so the
   split between thinking and answer is not reported even though reasoning is streamed.
