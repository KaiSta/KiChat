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

The work is one file ([internal/llm/nvidia.go](internal/llm/nvidia.go)) plus a
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
([internal/config/config.go:521](internal/config/config.go#L521)) appends the shipped entry
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
([internal/llm/nvidia.go:116](internal/llm/nvidia.go#L116)) maps what there is:

| field | becomes | note |
| --- | --- | --- |
| `id` | `ModelInfo.ID` | the fully-qualified name, `publisher/model` |
| `owned_by` | `Publisher` | also the id's first half |
| — | `State: loaded` | hosted: it answers as soon as the request is authenticated |
| — | `ContextLimit: 0` | no route of this API publishes a window |
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

### The decision: nothing is filtered out

**This provider drops no model**, and that is deliberate — it is the one place it diverges
from Mistral. Mistral can drop what cannot chat because its listing publishes
`capabilities.completion_chat` per model ([MISTRAL.md](MISTRAL.md) §3). This listing
publishes no capability of any kind, and the models that fail are refused *per account*.

So a filter written here could only be a guess about entitlement, and the guess would be
wrong in the direction that matters: it would hide a working model from the account that owns
it. The row is offered, and a model the account cannot call fails on send with the vendor's
own message — which `HTTPError` carries to the toast verbatim. `TestNvidiaAccountRefusalKeepsTheVendorDetail`
([internal/llm/nvidia_test.go:268](internal/llm/nvidia_test.go#L268)) pins that, because the
status alone would read as "the backend is broken" rather than "this model is not yours".

The cost is a picker longer than the list of models that will answer for a given account.
That is the trade, and it is visible in the UI rather than hidden in a filter.

### Tools are claimed, thinking and vision are not

Tools are appended unconditionally
([internal/llm/nvidia.go:116](internal/llm/nvidia.go#L116)), the same claim DeepSeek makes
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
| [internal/llm/nvidia.go:62](internal/llm/nvidia.go#L62) | `NvidiaDefaultBaseURL` | `https://integrate.api.nvidia.com/v1` — the `/v1` is part of the address |
| [internal/llm/nvidia.go:66](internal/llm/nvidia.go#L66) | `newNvidiaProvider` | the provider: one chat path, `/models`, `flavourNvidia`, `requiresKey`, and the bare-host completion |
| [internal/llm/nvidia.go:105](internal/llm/nvidia.go#L105) | `nvidiaModelEntry` | two fields read of the four sent; `object` and `created` are deliberately not decoded |
| [internal/llm/nvidia.go:116](internal/llm/nvidia.go#L116) | `fromNvidiaEntry` | one entry → `ModelInfo`. No second return value: there is nothing to drop |
| [internal/llm/nvidia.go:152](internal/llm/nvidia.go#L152) | `mapNvidiaEntries` | entry by entry; an entry with no id is skipped rather than offered nameless |
| [internal/llm/openai.go:127](internal/llm/openai.go#L127) | `flavourNvidia` | the listing flavour, dispatched at [openai.go:274](internal/llm/openai.go#L274) |
| [internal/llm/registry.go:115](internal/llm/registry.go#L115) | `Reconfigure` | `case "nvidia"` builds the provider |
| [internal/llm/registry.go:418](internal/llm/registry.go#L418) | `DefaultBackendConfigs` | the shipped row: `nvidia`, disabled |
| [internal/config/config.go:521](internal/config/config.go#L521) | `normalise` | restores the address for an entry saved without one |
| [frontend/src/components/Settings.tsx:1412](frontend/src/components/Settings.tsx#L1412) | the backend row | `kind === 'nvidia'` gets the key field, with the field marked required |

`openAICompatProvider.modelsFlavour` gained `flavourNvidia`, and `mapModelEntries`
([internal/llm/openai.go:274](internal/llm/openai.go#L274)) dispatches the whole list to
`mapNvidiaEntries` for that flavour rather than mapping entry by entry in place — the same
shape Mistral's branch uses, so the "whole list, or one entry" split has one home.

---

## 7. How this is verified

### Offline — `internal/llm/nvidia_test.go`

| test | what it pins |
| --- | --- |
| `TestNvidiaListingCarriesWhatThePlanningNeeds` | `Publisher` from `owned_by`, `State: loaded`, `CapTools`, no vision/thinking claim, and a window of **0** — the listing reports none, so nothing may be invented |
| `TestNvidiaListingKeepsModelsTheAccountMayNotReach` | the no-filter decision, pinned so it cannot be quietly reversed |
| `TestNvidiaListingSkipsEntriesItCannotRead` | one malformed entry does not hide the rest; an entry with no id is dropped |
| `TestNvidiaStreamsReasoningWithoutDoublingIt` | `reasoning` + `reasoning_content` produce the words **once**, plus content, usage and stop reason |
| `TestNvidiaStreamsAToolCall` | a fragmented `tool_calls` stream reassembles into one executable call |
| `TestNvidiaAccountRefusalKeepsTheVendorDetail` | a 404 problem+json reaches the user with "Not found for account" intact |
| `TestNvidiaBaseURLCompletionOnlyTouchesABareHost` | six base URLs: which are completed with `/v1`, which are left alone |
| `TestNvidiaWithoutAKeyIsNotAttempted` | `ErrMissingAPIKey` from `ListModels`, `Health` and `Chat`, with the server failing the test if reached |
| `TestRegistryBuildsNvidiaFromConfig` | the kind selects the provider, the default address, `requiresKey`, the flavour, that no other vendor's request names were borrowed, and that a disabled entry builds nothing |
| `TestDefaultBackendsShipNvidiaOff` | the shipped row exists, is disabled, and names the vendor address |

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

1. **No context window, by design.** No route of this API reports one, so every NVIDIA model
   arrives with `ContextLimit: 0` and has to be given a number through the per-model override
   in Settings — the same state a plain OpenAI-compatible server is in. Seeding a table of known
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
   contributed back to the app. `maybeAutoCompact` runs before *every* send, and it used to
   forward the refusal it got from `compactWith`, which emits an `events.Warning` the UI turns
   into a toast — so a working NVIDIA chat showed *"Kontextfenster des Modells ist unbekannt"*
   on every single message, for as long as no override was set. The fix is in
   [internal/chat/maintenance.go:124](internal/chat/maintenance.go#L124): an unknown window is a
   standing fact about the model, not an incident, so the automatic path returns before
   `compactWith` and leaves the condition to the meter and the inspector, while the manual path
   keeps the explicit refusal because there the user asked. `COMPACT.md` §4 and §13 record the
   same distinction. Before the fix, `TestAutoCompactStaysQuietWhenTheWindowIsUnknown` failed with
   exactly the warning quoted above.
2. **The picker is the catalogue.** Most entries will not answer for a given key (§3). This is
   the deliberate trade described there, not an oversight. If it proves to be the first thing
   every user trips over, the honest fix is an account-scoped probe with a cache, not a
   name-based filter.
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
