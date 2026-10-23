# MISTRAL.md

How Mistral works in KiChat: what the provider is, the real shapes it answers with,
the decisions that follow from them, and the bugs review found in the first cut.
`PLAN.md` names the backend in one line; this document is the detail behind it.

Everything here was read out of the code and, where marked *measured*, observed against
the live API on 2026-10-11. Claims that are documentation- or design-derived say so
explicitly — there is one such claim and it is the most important one in the file (§4).

**Contents:** 1 what this adds · 2 turning it on · 3 the listing · 4 the thinking-chunk
shape · 5 the request and the stream · 6 refusals · 7 where the code is · 8 the four
bugs review found · 9 how this is verified · 10 known gaps · 11 deliberately not done.

The code references are `file:line` for the revision this was written against. The
symbol names beside them (`mapMistralEntries`, `deltaContent`, `isBareHost`, …) are the
durable anchor when the numbers drift.

---

## 1. What this adds

Mistral is a hosted, OpenAI-compatible service, so **nothing in the chat path is new**.
Tool calls, streaming, `max_tokens`, `temperature`, `top_p` and
`stream_options.include_usage` all go through `openAICompatProvider` exactly as they do
for DeepSeek.

Four things are Mistral-specific, and they were not found the same way. Two came out of
measuring the live API (the listing and the per-model capabilities), one from the vendor's
documentation confirmed against a recorded live response (the delta `content` field — see
§4), and one from the documentation alone, afterwards checked by hand:

| what | why it could not be shared |
| --- | --- |
| The listing | 46 published entries, only **27** of them able to hold a conversation, and those 27 are only **8 models** |
| Per-model capabilities | The listing reports `function_calling`, `reasoning` and `vision` per model, so a blanket claim would be wrong in both directions |
| The delta `content` field | A reasoning model sends a **list of typed chunks** where every other backend sends a string |
| The base URL | The vendor documents a bare host while every route lives under `/v1` |

The work is one new file ([internal/llm/mistral.go](internal/llm/mistral.go)) plus a
change to the **shared** delta parser in [internal/llm/openai.go](internal/llm/openai.go) —
which is the part that affects every provider, and the reason §4 is the longest section
here.

---

## 2. Turning it on

Mistral ships **disabled**, like DeepSeek and for the same reasons: it is metered and
remote, so enabling it on a fresh install would probe a paid API on every health refresh
without being asked, and — because a hosted model is "ready" the moment a key is accepted
— its models would win the loaded-model race and silently become the default for new
chats.

There is also no migration to run. The settings blob is decoded over a fully populated
default and `normalise()` appends any shipped backend that is missing
([internal/config/config.go:491](internal/config/config.go#L491)), so a Mistral row
appears in an installation that predates it, switched off, with its address filled in.
That mechanism already existed for DeepSeek; this is its second user, and
`TestNormaliseAddsMissingBackendsWithoutClobberingEdits` now asserts all five shipped
backends arrive exactly once and that the user's own edits survive.

To use it: **Settings → Backends → Mistral**, paste the key, tick the box, Save. The key
goes to the secret store through `llm.BackendAPIKeyName("mistral")`
(`internal/secret`, `~/.config/kichat/`) — never into the settings blob.

---

## 3. The listing

`GET https://api.mistral.ai/v1/models` — **measured: HTTP 200, 46 entries.** One entry,
trimmed:

```json
{"id":"ministral-8b-latest","object":"model","owned_by":"mistralai",
 "capabilities":{"completion_chat":true,"function_calling":true,"vision":true,
                 "reasoning":false,"completion_fim":false,"fine_tuning":true,
                 "ocr":false,"classification":false,"moderation":false,"audio":false,
                 "audio_transcription":false,"audio_transcription_realtime":false,
                 "audio_speech":false,"unified_resources":false},
 "name":"ministral-8b-latest","max_context_length":262144,"aliases":["ministral-8b-2512"],
 "deprecation":null,"default_model_temperature":0.3,"type":"base"}
```

(`description`, `created`, `billing_model_name` and `deprecation_replacement_model` are
omitted here and unread by the app.)

The `capabilities` object carries 14 flags; **four** are read.

### 3.1 It is not a chat catalogue

The embedding, moderation, OCR, speech and transcription models share the array. The 19
non-chat entries were four `*-embed*` (`codestral-embed`, `codestral-embed-2505`,
`mistral-embed`, `mistral-embed-2312`), `mistral-moderation-2603`, seven `mistral-ocr-*`,
five `voxtral-mini-*` (`-2602`, `-latest`, `-transcribe-realtime-2602`, `-realtime-2602`,
`-realtime-latest`) and two `voxtral-mini-tts-*`.

The separating flag is `completion_chat`, **not** `function_calling`:

| model | completion_chat | function_calling | vision | ctx | verdict |
| --- | --- | --- | --- | --- | --- |
| `ministral-8b-latest` | true | true | true | 262144 | offered |
| `mistral-ocr-latest` | **false** | **true** | true | 16384 | refused |
| `mistral-embed-2312` | **false** | false | false | 8192 | refused |

`mistral-ocr-*` is the whole argument in one row: it advertises function calling while
being unable to hold a conversation. Filtering on the tool flag would have put OCR models
in the model picker, where they fail on send. `fromMistralEntry`
([internal/llm/mistral.go:189](internal/llm/mistral.go#L189)) returns `(ModelInfo, false)`
for those, and the caller leaves them out of the catalogue entirely rather than offering
a row that cannot work.

### 3.2 The ids are not models

**Measured: 27 chat-capable ids are 8 distinct models.** The listing publishes one entry
per id that resolves to the same model, so `mistral-medium-latest` alone appears nine
times:

| canonical `name` | ids | kept |
| --- | --- | --- |
| `mistral-medium-latest` | 9 — incl. `mistral-medium-3.5`, `mistral-medium-2604`, `mistral-vibe-cli-with-tools`, `magistral-medium-latest` | `mistral-medium-latest` |
| `codestral-2508` | 4 — incl. `codestral-latest`, `mistral-code-fim-latest` | `codestral-latest` |
| `mistral-small-2603` | 4 — incl. `mistral-vibe-cli-fast`, `magistral-small-latest` | `mistral-small-latest` |
| `ministral-3b-2512` / `-8b-` / `-14b-` | 2 each | `ministral-*-latest` |
| `voxtral-small-2507` | 2 | `voxtral-small-latest` |
| `labs-leanstral-1-5-1` | 2 | `labs-leanstral-1-5-1` (no rolling alias) |

Every one of the eight groups agreed on its `max_context_length` **and** on all 14
capability flags — that agreement is what makes the shared `name` field usable as the
model's identity rather than the id. A per-entry mapper would have put 27 rows in the
picker for 8 models.

So `mapMistralEntries`
([internal/llm/mistral.go:117](internal/llm/mistral.go#L117)) reads the listing as a
whole, groups by `name`, and `preferredMistralID`
([internal/llm/mistral.go:153](internal/llm/mistral.go#L153)) picks the survivor: the
rolling `-latest` alias, shortest when a family has several, else the first id the
listing named. A picker row should follow the vendor's release rather than pin a dated
snapshot, and the alias reaches the same model either way.

An entry with no `name` at all falls back to its own id as the grouping key, so it stands
alone rather than being merged with something else.

### 3.3 Capabilities are per model

Unlike DeepSeek — whose listing is silent, so every model is assumed to call tools — this
one answers the question per model, and the provider simply forwards the answer:
`vision` → `Type: "vlm"` + `CapVision`, `function_calling` → `CapTools`, `reasoning` →
`CapThink`. A field that is absent reads as `false`, which is the safe direction for all
three: claiming a capability a model lacks costs the request, and claiming none it has
costs only an optimisation.

`State` is reported `loaded` for every model. A hosted model has no load step; reporting
it "not loaded" the way a local model waiting for VRAM is reported would grey it out of
the picker and keep it out of the default-model choice for no reason.

---

## 4. The thinking-chunk shape

**This is the one claim in this document that is not measured on this account.** Every
reasoning model behind this key is refused (see §6), so the shape below comes from
Mistral's documentation — *"the output of reasoning models includes a reasoning chunk with
the model's thinking traces, followed by the final answer"* — and from a recorded live
response captured by someone else (`vercel/ai` issue #13703, whose reproduction recorded
`mistral-small-latest` returning exactly this shape in **both** the streaming and the
non-streaming response).

A reasoning model here does **not** put its thinking in a field of the delta the way
DeepSeek's `reasoning_content` does. `choices[].delta.content` becomes a **list of typed
chunks**:

```
"content": [{"type":"thinking","thinking":[{"type":"text","text":"…"}]}]
"content": [{"type":"text","text":"…"}]
```

### Why this nearly shipped broken

`openAIDelta.Content` was a `string`. `json.Unmarshal` fails on a list, and the stream
loop's tolerance for a malformed frame is deliberate and correct — *"a single malformed
frame should not abort a long generation"* — so it skips the frame. The consequence:

> every frame of such an answer is dropped, and the reply arrives **empty, with no error
> at all**.

That is the worst failure shape there is: no exception, no log, just a blank answer that
looks like the model had nothing to say. It is why this is a shared-code change and not a
Mistral-only one.

### What the code does now

`deltaContent` ([internal/llm/openai.go:355](internal/llm/openai.go#L355)) accepts:

| input | result |
| --- | --- |
| a string | `Text` — every other backend, unchanged |
| `null` | empty — the shape DeepSeek's first frame uses |
| a list of chunks | `"text"` → `Text`, `"thinking"` → `Reasoning`, unknown types ignored |
| anything else (object, number) | empty, no error |

Two details are load-bearing:

- **The parts are decoded one at a time**, as `[]json.RawMessage`, and a part that cannot
  be read is skipped on its own. Decoding the list as one typed slice means a single
  unknown chunk type — a citation whose own `content` is a list — fails the whole array
  and takes the text beside it down too. That was a bug in the first version of this very
  change; it is §8.1.
- **Order is preserved**: for a frame carrying both kinds, the thinking is emitted before
  the text, so the reasoning panel never ends up holding what should have been shown as
  the answer.

Plain-string content is what every other provider sends, and
`TestMistralStreamKeepsPlainStringContent` pins both the string path and the ordering
rule so the shared parser cannot regress for LM Studio, Ollama or llama.cpp.

---

## 5. The request and the stream

### What the app sends

`model`, `messages`, `stream: true`, `temperature`, `stream_options:
{"include_usage": true}`, and then `top_p` (when > 0 — **the shipped default is 0.9**),
`max_tokens` (when > 0), `tools` (when any), and `random_seed` (when set).

**Measured:** `top_p: 0.9` + `temperature: 0.7` + `stream_options` all answer HTTP 200, so
nothing the app sends by default is rejected.

Two of those names are this vendor's own rather than the OpenAI-compatible world's, and
both are set in `newMistralProvider` instead of in the shared body builder — the same
separation that already kept LM Studio's `enable_thinking` and llama.cpp's
`chat_template_kwargs` apart:

| the app's own model | what Mistral reads | why it cannot be shared |
| --- | --- | --- |
| `seed` | **`random_seed`** | the published body has **no `seed` property at all**, and it is declared `additionalProperties: false` |
| `Think: false` on LM Studio → `enable_thinking`, on llama.cpp → `chat_template_kwargs` | **`reasoning_effort: "none"`** | the schema's own spelling for it: `"none"` disables the reasoning effort (it deprecates `prompt_mode: "reasoning"` in favour of it) |

`random_seed` is inert today — nothing in `internal/chat` sets `ChatRequest.Seed` — but it
is wired to the name the API knows, so the day something does set a seed, the field is not
a stranger's. `TestMistralCallsTheSeedFieldItsVendorNames` pins both sides of it: Mistral
gets `random_seed`, and an OpenAI-compatible server still gets `seed`.

`reasoning_effort` is **live**, and it is how compaction's "do not think" reaches this
vendor at all. It is only ever sent for a model the caller asked to silence, and the
caller derives that from the model's own `capabilities.reasoning` (`chat/maintenance.go`
asks the question as `CapThink`), so a model with nothing to switch off never sees the
field. What cannot be measured from here is whether the deployed service honours it or
refuses it: every reasoning model this account can name sits behind the 429/403 gate of §6
(measured: `mistral-medium-latest` → 429), so the field is made *safe* rather than
*assumed right* — the first `400`/`422` retires it for the session and the request is sent
again without it (`retireReasoningEffort`, `TestMistralAsksAReasoningModelNotToThinkWithReasoningEffort`).
A refusal therefore costs one round trip, once, and never a turn.

**The published bounds are not the app's.** The schema bounds `temperature` to
**`[0, 1.5]`**, while Settings offers up to `2` (`<input type="number" max="2">`) and
nothing between the two clamps the value — `config.normalise` only replaces a
non-positive number with the 0.7 default. A user can therefore build a body the vendor's
own schema forbids. Whether the service enforces that bound is **not measured**; §10.4 has
the probe that answers it, and the value to act on.

**Never sent:** `enable_thinking` (`thinkingToggle` is false — that is LM Studio's) and
`chat_template_kwargs` (`chatTemplateKwargs` is false — that is llama.cpp's). Those two
branches of the switch in `Chat` stay unreachable here, which is what kept the "do not
think" request falling through to the in-prompt soft switch that `Summarize` always sends
until `reasoning_effort` was wired up.

### The stream

Ordinary OpenAI SSE — `data: {…}\n\n`, terminated by `data: [DONE]` — with one curiosity:
some frames carry an extra `"p"` field (a short opaque string such as
`"abcdefghijklmnopqrstuvw"`). It is not read and needs no handling.

**Measured** (`ministral-3b-latest`, "Reply with exactly: OK", `max_tokens: 64`):

```
frame 1: delta {role:"assistant", content:""}
frame 2: delta {content:"OK"}
frame 3: delta {content:""}, finish_reason:"stop",
         usage {prompt_tokens:8, completion_tokens:2, total_tokens:10,
                prompt_tokens_details:{cached_tokens:0}, service_tier:"standard"}
data: [DONE]
```

Two things to note. `usage` rides the **finish frame** rather than a separate usage-only
frame — the shared parser reads `chunk.Usage` from every frame, so it is picked up either
way. And the measured `prompt_tokens` is the entire reason the request asks for usage: the
context meter shows a measured value instead of an estimate only because
`stream_options.include_usage` is honoured.

**Measured** tool call (`ministral-3b-latest`, "What is the weather in Berlin? You must
call get_weather."), one frame:

```json
{"delta":{"tool_calls":[{"id":"T4yroDKcT","type":"function",
  "function":{"name":"get_weather","arguments":"{\"city\": \"Berlin\"}"},"index":0}]},
 "finish_reason":"tool_calls",
 "usage":{"prompt_tokens":79,"completion_tokens":12,"total_tokens":91}}
```

That call arrived whole. Fragmented assembly — the case where the name and arguments
arrive across frames — is exercised by `TestMistralStreamsAFragmentedToolCall` against a
fake server rather than by this measurement.

### One honest wart

Mistral sends no `stats` and no llama.cpp `timings` block, so the app derives tokens/s
from wall-clock. On an answer of a couple of tokens that number is dominated by rounding
rather than by anything the model did: two runs of the same two-token request measured
`7246 tok/s` and `70.7 tok/s`, with `TimeToFirstTokenMs` 0 in the first and 7 in the
second. The UI labels it **"estimated"** rather than "measured", which is the correct
label and the reason the wart is not a bug. The same is true of DeepSeek; nothing about
this provider made it worse.

---

## 6. Refusals

Refusals are classified by **status code and envelope**, never by message text. There are
three distinct ones:

| case | status | body (measured, verbatim shape) |
| --- | --- | --- |
| No/bad key | 401 | `{"detail":"Invalid API Key"}` |
| Tier gate | 429 | `{"object":"error","message":"Rate limit exceeded","type":"rate_limited","param":null,"code":"1300","raw_status_code":429}` |
| Labs model | 403 | `{"object":"error","message":"Model labs-leanstral-1-5 is a Labs model. To use Labs models, an admin must enable them in …"}` |

Note the 401 uses a **different envelope shape** (`detail`, no `object`) from the chat
errors — one more reason the code is what classifies and the wording is not.

One 400 is not a verdict on the account but on the history's shape (reported from a live
chat on 2026-10-23, not measured on this account):

```json
{"object":"error","message":"Assistant message must have either content or tool_calls, but not none.",
 "type":"invalid_request_assistant_message","param":null,"code":"3240","raw_status_code":400}
```

An assistant message with `"content":""` and no `tool_calls` is tolerated by every other
backend the app talks to and **validated here**. It is also what the app's own failure
paths leave behind — a request that died before a token, a Stop before the first one, a
reply that only thought (reasoning is not part of the wire format, §4) — so replaying it
refused the whole turn and wrote another empty row, and one failure made every later send
of that chat fail the same way. `Assemble` now leaves such a turn out of the request
(`TestAssembleDropsAnAssistantTurnThatProducedNothing`); the row stays in the transcript.

The 429 is **a per-model gate as much as a rate**. Measured on one free-tier key:

| model | result |
| --- | --- |
| `ministral-3b-latest`, `ministral-8b-latest`, `ministral-14b-latest` | HTTP 200 |
| `mistral-small-latest`, `magistral-small-latest`, `mistral-vibe-cli-fast` | HTTP 429, `code:"1300"` |
| `labs-leanstral-1-5` | HTTP 403, Labs gate |

The rate-limit headers on a 200 answer read `x-ratelimit-limit-req-minute: 750` and
`x-ratelimit-limit-tokens-minute: 1300000`, so the 429s are not a general throttle being
hit — they are the plan excluding those models. That is exactly why the live test
**reports** a refusal instead of failing on it (§9): treating it as a defect would invent
a bug in the provider.

An empty key is refused **before any request is made**: `requiresKey` is true, so
`keyCheck` returns `ErrMissingAPIKey` and the user is told what is missing instead of
being handed the vendor's 401 body. `TestMistralWithoutAKeyIsNotAttempted` fails the test
if the server is reached at all.

---

## 7. Where the code is

| file | symbol | what it does |
| --- | --- | --- |
| [internal/llm/mistral.go:60](internal/llm/mistral.go#L60) | `MistralDefaultBaseURL` | `https://api.mistral.ai/v1` — the `/v1` is part of the address |
| [internal/llm/mistral.go:64](internal/llm/mistral.go#L64) | `newMistralProvider` | the provider: one chat path, `/models`, `flavourMistral`, `requiresKey`, and the two vendor-specific names (`seedField: "random_seed"`, `reasoningEffortOff`) |
| [internal/llm/mistral.go:106](internal/llm/mistral.go#L106) | `isBareHost` | decides whether the base URL may be completed with `/v1` |
| [internal/llm/mistral.go:117](internal/llm/mistral.go#L117) | `mapMistralEntries` | whole-listing map: drop non-chat, collapse aliases |
| [internal/llm/mistral.go:153](internal/llm/mistral.go#L153) | `preferredMistralID` | which alias of one model survives |
| [internal/llm/mistral.go:189](internal/llm/mistral.go#L189) | `fromMistralEntry` | one entry → `ModelInfo`, or `false` if it cannot chat |
| [internal/llm/openai.go:149](internal/llm/openai.go#L149) | `seedKey` | the shared builder's one flavour-dependent field name: `seed`, or `random_seed` |
| [internal/llm/openai.go:629](internal/llm/openai.go#L629) | `retireReasoningEffort` | a 400/422 to `reasoning_effort` retires the field and the request is sent again without it |
| [internal/llm/openai.go:355](internal/llm/openai.go#L355) | `deltaContent` | `content` as string **or** typed-chunk list; the shared fix |
| [internal/llm/openai.go:436](internal/llm/openai.go#L436) | `openAIDelta.Content` | now `deltaContent` instead of `string` |
| [internal/llm/openai.go:1037](internal/llm/openai.go#L1037) | `consume` | emits `delta.Content.Text`; reasoning first via `reasoningText()` |
| [internal/llm/openai.go:1417](internal/llm/openai.go#L1417) | `mergeDeltas` | merges both halves of `deltaContent` (the empty-string guards became no-ops) |
| [internal/llm/registry.go:113](internal/llm/registry.go#L113) | `Reconfigure` | `case "mistral"` builds the provider |
| [internal/llm/registry.go:406](internal/llm/registry.go#L406) | `DefaultBackendConfigs` | the shipped row: `mistral`, disabled |
| [internal/config/config.go:491](internal/config/config.go#L491) | `normalise` | repairs an empty address for an existing install |
| [frontend/src/components/Settings.tsx:1373](frontend/src/components/Settings.tsx#L1373) | backend row | shows the key field and requires it |
| [frontend/src/lib/i18n.ts:182](frontend/src/lib/i18n.ts#L182) | `settings.backendsHint` | the hint, all three languages (182/559/932) |
| [tools/uiharness/mock-wails.js:287](tools/uiharness/mock-wails.js#L287) | harness fixture | the row the UI is verified against |

`openAICompatProvider.modelsFlavour` gained `flavourMistral`
([internal/llm/openai.go:126](internal/llm/openai.go#L126)), and `mapModelEntries`
dispatches the whole list to `mapMistralEntries` for that flavour rather than mapping
entry by entry.

---

## 8. The bugs review found

§8.1–§8.4 were all in the first cut of this change. They are recorded because each one was
invisible in the same way: a wrong shape does not raise, it just produces a worse answer.
§8.1 and §8.3 were introduced by me and caught in review; §8.2 is a defect I had noticed
and then failed to act on; §8.4 was in the test rather than in the product. §8.5 came out
of a later pass that read the whole provider against the vendor's published schema instead
of against the endpoints it happens to call, and it is the same species of fault: a name
nothing on the other side was reading.

### 8.1 The frame-killing decode, one level in

The first version decoded the content list as **one typed slice** with a `string` field
for `content`. Any chunk type whose own `content` is a list — a citation, say — fails that
single decode, and the code returned `nil`, dropping the **entire frame including a text
part sitting right next to it**. That is precisely the fault `deltaContent` exists to
prevent, reintroduced one level deeper.

Proved rather than reasoned about: a throwaway test decoded the frame into the old shape
and reported

```
json: cannot unmarshal array into Go struct field .content of type string
```

so the frame really was being discarded. Fixed by decoding the parts individually. The
test frame now carries a citation with an array `content` beside a text part, and its
exact-match assertion separates all three outcomes: correct → `"21!"`, frame dropped →
`"21"`, naive read that lets the citation leak through → text containing `"Quelle"`.

### 8.2 Twenty-seven picker rows for eight models

Noticed while reading the listing and then not acted on in the first cut. See §3.2 — this
is the difference between a usable picker and a list where six consecutive rows are the
same model.

### 8.3 A completed address that broke a correct one

The first version appended `/v1` to any base URL not already ending in it. That is right
for the bare host the vendor's docs show, and wrong for a gateway prefix: `/openai`
became `/openai/v1`, turning a working address into a broken one. Now only a bare host —
scheme and host, no path (`isBareHost`, via `net/url`) — is completed, and anything the
user wrote with a path is left exactly as it is.

### 8.4 A skip that could have hidden a failure

The live test skips when the account may not call the model it needs. But it skipped
*after* earlier assertions had already run, and a test that is both failed and skipped
reports as "not checked" — which would have buried a genuine defect as a gap. It now
skips only while nothing has failed, and otherwise logs and returns so the failure stands.

Cosmetic drift fixed in the same pass: the file header said "three of them matter" over
four bullets; `mistralModelEntry` decoded `type` and `aliases` without ever reading them
(and `aliases` could imply it drove the collapsing, which it does not — `name` does); and
one earlier edit had collapsed a comment onto a code line.

### 8.5 Two field names the vendor does not use

Reading the published request schema whole — 662 kB of it — rather than the endpoints the
provider happens to call turned up two properties that are not Mistral's:

- **`seed`.** The shared body builder sent `seed`, the name every other backend in this
  codebase reads. Mistral's chat body has no such property: it calls it **`random_seed`**
  (the only seed-shaped property anywhere in the specification), and the body is declared
  `additionalProperties: false`. Nothing in `internal/chat` sets `ChatRequest.Seed` today,
  so the fault was latent rather than live — a defect with a trigger attached, waiting for
  the first feature that pins a seed. The name now travels with the flavour
  (`seedField`/`seedKey`) instead of living in the builder.
- **"do not think".** The switch in `Chat` offered `enable_thinking` (LM Studio) and
  `chat_template_kwargs` (llama.cpp) and then nothing at all, so a compaction on this
  backend asked a reasoning model to skip its thinking and sent no field to say so, falling
  back to the in-prompt soft switch. The schema documents the vendor's own spelling —
  `reasoning_effort`, whose `"none"` disables the reasoning effort, with `prompt_mode`
  deprecated in its favour — and that is now sent, for a reasoning model only, because only
  a model the listing reports `reasoning: true` for can be asked.

The two got different treatments on purpose. The seed was a **name** to correct. The
reasoning field is a **claim** about the deployed service that this account cannot measure
(§10.4), so it is sent optimistically and retired on the first `400`/`422`
(`retireReasoningEffort`): the worst case is one wasted round trip, not a failed
summarisation. A wrong name is invisible; a refused field is a failed turn.

---

## 9. How this is verified

### Offline — `internal/llm/mistral_test.go`

| test | what it pins |
| --- | --- |
| `TestMistralListingOffersOnlyConversationModels` | the filter (embed/OCR refused), the alias collapse, per-model capabilities, the surviving id |
| `TestMistralStreamReadsThinkingChunksFromContent` | thinking chunks → `EventReasoning`, text → `EventDelta`, the citation frame, usage and stop reason |
| `TestMistralStreamKeepsPlainStringContent` | the string path still works and thinking precedes text in one frame |
| `TestMistralBaseURLCompletionOnlyTouchesABareHost` | five base URLs: which are completed, which are left alone |
| `TestMistralWithoutAKeyIsNotAttempted` | `ErrMissingAPIKey` from `ListModels`, `Health` and `Chat`, with the server failing the test if reached |
| `TestMistralStreamsAFragmentedToolCall` | fragmented `tool_calls` reassemble into one executable call |
| `TestRegistryBuildsMistralFromConfig` | the kind selects the provider, the default address, `requiresKey`, the flavour, and that a disabled entry builds nothing |
| `TestShippedMistralBackendIsOffByDefault` | the shipped row is off and carries the vendor address |
| `TestMistralCallsTheSeedFieldItsVendorNames` | `random_seed` for Mistral and `seed` for an OpenAI-compatible server, each asserted absent from the other's body |
| `TestMistralAsksAReasoningModelNotToThinkWithReasoningEffort` | `reasoning_effort: "none"` on a `Think: false` request, the 422 it may draw, the retry that succeeds without it, and the field staying retired for the next call |

The fake servers serve `/v1/models` and `/v1/chat/completions` — the real paths — so a
provider that failed to complete the base URL would ask for `/models` and get a 404.

### Live — `internal/llm/live_mistral_test.go`

Gated on `KICHAT_LIVE=1` **and** `KICHAT_MISTRAL_KEY`; nothing runs by accident. Four
claims, three of which are measured:

```
KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistral -v
```

Last run, verbatim — only the `stats` object in the third line is elided, because its
numbers vary from run to run (§5):

```
listed 8 chat models out of 46 published
chosen model=ministral-3b-latest ctx=131072 type=vlm caps=[vision tool_use]
content="OK" reasoning=0 chars usage=&{PromptTokens:8 CompletionTokens:2 TotalTokens:10 CachedPromptTokens:0 ReasoningTokens:0} stats=&{… Source:estimated}
tool call=get_weather arguments={"city": "Berlin"}
thinking chunks not checked: mistral-medium-latest is refused for this account (HTTP 429: {"object":"error","message":"Rate limit exceeded","type":"rate_limited","param":null,"code":"1300","raw_status_code":429})
--- PASS: TestLiveMistral (2.07s)
```

`listed 8 … out of 46` is the alias collapse and the chat filter confirmed against the
real service rather than a fixture, and it is asserted from both sides: every listed model
carries a context window and is `loaded`, and the embedding/moderation ids the API really
publishes are absent from what the provider offers.

The fourth claim — the thinking-chunk decode — **could not be checked on this account**,
and the test says so out loud rather than passing quietly. See §4 and §10.

### The one metered probe

The temperature bound of §5 has a probe of its own in the same file, kept separate because
it is one request and needs no model that can reason:

```
KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistralTemperature -v
```

It sends `temperature: 1.8` — above the schema's `[0, 1.5]`, inside what Settings offers —
with `max_tokens: 1` on the cheapest model the listing has, and **reports** which way the
service went instead of asserting one. Either answer is a fact worth having, and a test
that failed on the expected one would be guessing in a test's clothing. §10.4 says what to
do with each result.

### The gate

`make check` (gofmt, `go vet ./...`, every package's tests, `tsc --noEmit`) and
`make build` → `build/bin/kichat`, both green after the last edit.
`frontend/wailsjs/go/models.ts` is byte-unchanged: no Go struct that crosses the bridge
was altered. The UI was driven in the mock-bridge harness in **English and German** — the
row, the `/v1` address, the required-key field, and the
`llm: backend unavailable: no API key configured` badge — with no console errors.

Re-run after the §8.5 fix: `make check` and `make build` both exit 0 (34 s for the build),
and no generated file moved — `frontend/wailsjs/go/models.ts` is identical, because nothing
that crosses the bridge was touched. That harness walk above belongs to the original
change; this pass changed no UI code.

---

## 10. Known gaps

1. **The thinking-chunk decode is not measured on this hardware.** It rests on the
   vendor's documentation plus a recorded live response from a third party. The offline
   test uses that exact payload, so the decoding is pinned; what is unverified is that
   Mistral still sends it. Closing this needs a key whose plan can call a reasoning model
   — `KICHAT_MISTRAL_REASONING_MODEL` selects one explicitly for that purpose.
2. **27 of the 46 entries are only partly accounted for offline.** The fixture models the
   five kinds of entry that matter; the real listing's 14 capability flags and 19 non-chat
   ids are summarised here rather than asserted one by one.
3. **A pure-API-key rotation is untested.** The 401 body shape is recorded, but the
   in-app flow (paste → save → probe) was verified only through the harness, not against
   the live API from inside a running app.
4. **Whether the service enforces `reasoning_effort` and the `[0, 1.5]` temperature bound
   is unmeasured.** Both are read off the published schema, and both need a model this
   account may call — the reasoning models sit behind the 429/403 gate of §6. The two are
   handled differently because their failure modes are not alike: `reasoning_effort`
   retires itself on a `400`/`422`, so a wrong guess costs one round trip once, while the
   temperature bound is a **user-reachable 422** that nothing in the app prevents
   (`<input type="number" max="2">`, no clamp in `config.normalise`).
   `TestLiveMistralTemperatureAboveThePublishedMaximum` settles it with one metered token:
   - **rejected** → Settings' maximum has to come down to `1.5`, and the bound belongs next
     to the field rather than in a table here;
   - **accepted** → the mismatch is schema drift and stays a sentence, since a value the
     service silently clamps is not a value the app should be inventing a limit for.

---

## 11. Deliberately not done

**The duplicate listing-path fallback.** For Mistral, DeepSeek *and* llama.cpp the rich
listing path and the plain-OpenAI fallback path are the **same URL**
([internal/llm/mistral.go:92](internal/llm/mistral.go#L92),
[internal/llm/deepseek.go:53](internal/llm/deepseek.go#L53),
[internal/llm/llamacpp.go:109](internal/llm/llamacpp.go#L109)). So a 404 makes
`ListModels` retry the identical address, and — if that retry ever answered — it would go
through `listOpenAIModels`, the lowest-common-denominator mapper that knows nothing of the
flavour. For Mistral that would mean **no chat filter, no alias collapse, no context
window, no capabilities**. It is unreachable today, because an identical URL 404s
identically; changing shared behaviour for three providers on a hypothetical was not worth
the regression risk to the one of them whose 404 paths are already pinned by its own
tests (`TestLlamaCppAMissingPropsRouteIsRemembered`, and the chat-path retirement tests
that assert a 404 retires a route after exactly one attempt). Worth knowing it is there.

**Alias search.** After the collapse, a user who knows a model by one of its other ids
cannot find it by that name in the model picker. That is a deliberate simplification, not
an oversight — but it is the obvious next thing to fix if the collapse proves annoying.

**No per-model allow-list.** Nothing stops a user selecting a model their plan excludes;
they get the vendor's 429 or 403 on send. Classifying those into an actionable message
("your plan does not include this model" rather than raw JSON) is not done here.
