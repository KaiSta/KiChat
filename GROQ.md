# GROQ.md

How the api.groq.com backend works in KiChat: the shapes the service answers with, the one
decision its listing forces, and what is verified against the live API.
`PLAN.md` names the backend in one line; this document is the detail behind it. It is a
sibling of `NVIDIA.md`, `MISTRAL.md` and `DEEPSEEK.md`, which do the same for the other hosted
services.

Everything here was read out of the code and, where marked *measured*, observed against the
live API with a real `gsk_` key on the account this was written for. Where a claim comes from
the vendor's documentation rather than from a request, it says so.

**Contents:** 1 what this adds · 2 turning it on · 3 the listing · 4 the stream · 5 reasoning
that cannot be switched off · 6 refusals and the rate-limit budget · 7 where the code is ·
8 how this is verified · 9 known gaps.

The code references are `file:line` for the revision this was written against. The symbol
names beside them (`newGroqProvider`, `fromGroqEntry`, `flavourGroq`, …) are the durable
anchor when the numbers drift.

---

## 1. What this adds

Groq serves an OpenAI-compatible API, so **nothing in the chat path is new**: streaming,
tools, `max_tokens`, `seed` and `stream_options.include_usage` all go through
`openAICompatProvider` unchanged. What is Groq's own is the address, the fact that it cannot
answer without a key, and a model listing that is the **richest** of the hosted backends here
— richer than the plain OpenAI shape and the opposite of NVIDIA's four fields. That listing is
what §3 is about.

The work is one file ([internal/llm/groq.go](internal/llm/groq.go)) plus a `flavourGroq`
branch in the shared listing mapper ([openai.go:280](internal/llm/openai.go#L280)), a shipped
but disabled entry in `DefaultBackendConfigs` ([registry.go:427](internal/llm/registry.go#L427)),
and the address repair in `normalise()` ([config.go:934](internal/config/config.go#L934)). It
follows the shape DeepSeek established: `requiresKey`, `ErrMissingAPIKey` reported instead of
attempting a call with no credential, and `State: loaded` on every model because a hosted
model answers without a load step.

| what | why it could not be shared |
| --- | --- |
| The model listing | It publishes each model's window, modalities and supported features, and it lists models that are not chat models at all |
| The base URL | `https://api.groq.com/openai/v1` — the only backend here whose path is not a bare `/v1` |
| The key | Same rule as the other hosted backends; the entry exists so a key has somewhere to go |

---

## 2. Turning it on

Groq ships **disabled**, the rule every metered hosted backend here follows: enabling it by
default would probe a paid API on every health refresh without being asked, and — because a
hosted model is ready the moment a key is accepted — its models would win the loaded-model
race and quietly become the default for new chats.

To use it: **Settings → Backends → Groq**, paste the key, tick the box, Save. The key goes to
the secret store through `llm.BackendAPIKeyName("groq")` (`internal/secret`,
`~/.config/kichat/`) and never into the settings blob. The field is marked required, because
nothing answers without one — the row's own probe reports a missing key rather than sending a
request.

There is no migration. `normalise()` appends the shipped entry to a settings blob that
predates it and repairs an empty address from `llm.GroqDefaultBaseURL`
([groq.go:75](internal/llm/groq.go#L75)), which is also what the settings field shows, so it
displays the address the app would actually use.

**The address trap is this vendor's alone.** `isBareHost` is the rule Mistral established for
completing a typed base URL, and every other kind completes a bare host with `/v1`. This API
lives under `/openai/v1`, so doing that here would build `https://api.groq.com/v1` — a path
that does not exist — which is why the completion appends `/openai/v1`
([groq.go:79](internal/llm/groq.go#L79)). A base URL that already carries a path is the user's
own choice and is left exactly as typed; a gateway in front of the API is therefore an address
edit, not a different kind.

---

## 3. The listing

`GET /openai/v1/models` is the one route this provider reads, and it is **not** a chat
catalogue. Measured on the account this was written for, it answers **11 entries**, each with
these fields:

| field | measured value | what the provider does with it |
| --- | --- | --- |
| `id` | `openai/gpt-oss-20b`, `qwen/qwen3.8-27b` | the model id, addressed as the vendor writes it |
| `owned_by` | `OpenAI`, `Meta`, `Alibaba Cloud`, `SDAIA`, `Canopy Labs` | the picker's publisher column |
| `active` | `true` on all 11 | an entry the vendor has switched off is dropped |
| `context_window` | 131072 on `openai/gpt-oss-20b`, 4096 on `allam-2-7b` | `ContextLimit` and `MaxContext`, so no override is needed |
| `max_completion_tokens` | 65536 on `gpt-oss-20b`, 512 on a prompt guard | read and kept in the entry struct; the app caps its own answer budget |
| `input_modalities` | `["text"]`, `["text","image"]`, `["audio"]` | `image` means `Type: vlm` and the vision claim |
| `output_modalities` | `["text"]`, `["transcription"]`, `["speech"]` | the chat-catalogue filter below |
| `supported_features` | `["tools","json_mode","structured_outputs","reasoning"]`, `["json_mode"]` | the tools and thinking claims |

The remaining fields carry no decision: `object` is always `model`, `created` is a real date
the app has no use for, `context_length`/`max_output_length` repeat the two numbers above,
`name` and `hugging_face_id` are display strings, `public_apps` is null, `pricing` is
per-token cents the panel does not show, and `model_card` is a documentation URL. `GET
/openai/v1/models/<id>` answers the same fields for one model, so there is no richer route to
read either.

### The capability claims are the vendor's, not ours

This is the pleasant difference from `NVIDIA.md` §3. That catalogue publishes four fields and
nothing else, so its provider claims tools for every model and invents no window. Here each
model states what it supports, so:

- **tools** is claimed when `supported_features` names `tools`. `allam-2-7b`, which publishes
  `["json_mode"]` alone, is offered without the claim rather than being assumed to have it.
- **thinking** is claimed when `supported_features` names `reasoning`. The model does reason by
  default on this API; the claim is what badges it and what compaction reads.
- **vision** is claimed when `input_modalities` names `image`, which is also what makes the
  model a `vlm` rather than an `llm`.

Measured, the claim is honest in both directions: `openai/gpt-oss-20b` publishes `tools` and
`reasoning`, and both were exercised live — reasoning streamed, and a `get_weather` call
arrived with `finish_reason: "tool_calls"`.

### The four entries that cannot chat, and why dropping them is not a guess

The listing mixes four models that are not chat models into the eleven:

| entry | its modalities | what asking it to chat answers |
| --- | --- | --- |
| `whisper-large-v3`, `whisper-large-v3-turbo` | `audio` → `transcription` | HTTP 400, "The model \`whisper-large-v3\` does not support chat completions" |
| `canopylabs/orpheus-v1-english`, `canopylabs/orpheus-arabic-saudi` | `text` → `speech` | not a chat route at all |

They are dropped, and the rule is the vendor's own: **an entry is offered only when its output
modalities name `text`** (`fromGroqEntry`, [groq.go:148](internal/llm/groq.go#L148)). The
refusal above is the evidence, and it also says the reverse — a filter written from the *id*
would be a guess, which is why the id is never consulted.

Two consequences worth stating:

- **A missing field is not a `false` claim.** An entry the listing has stopped describing — no
  `output_modalities`, no `supported_features`, no `active` — is kept, with no capabilities
  claimed. The rule is the one `LLAMACPP.md` states for `--flash-attn`: only a present claim
  may take something away. `TestGroqListingKeepsWhatTheListingDoesNotDescribe` pins it.
- **Two classifiers survive the filter, and they can only fail in one direction.**
  `meta-llama/llama-prompt-guard-2-22m` and its 86m sibling are `text` → `text`, `active` and
  carry a real window of 512. Measured: a call *without* `stream` answers HTTP 200 with a
  classification score instead of an answer, and the **same call with `stream: true` is
  refused** with HTTP 400 `"text classification models do not support streaming"`. Since
  KiChat streams every turn, these two rows can only ever produce that refusal — and nothing
  in the listing says so, so hiding them would be a rule about their names, which is the kind
  of guess this mapper refuses to make. They are offered, the vendor's sentence is what the
  user sees, and `TestLiveGroq` asserts the refusal so the gap stays a measurement rather than
  a memory. **The app does have a way to act on it that is not a guess**, which is the same
  mechanism NVIDIA.md §3 describes: Settings' *check* (`App.StartModelCheck` → `ProbeModels`)
  asks every model once with the streaming call the chat makes, and a non-retryable HTTP
  refusal — this 400 is one — is the only verdict `applyModelProbes` will blacklist on. So a
  user who runs the check has the two rows taken out of the picker on the vendor's own answer,
  while a provider that dropped them by name would have decided the same thing without asking.
  The non-streaming route that would make them usable instead is `GROQ.md` §9.1.

Measured end to end: 11 entries in, **7 chat models** out, and the live test asserts the four
absent ids by name so a change in the vendor's `output_modalities` shows up as a failure
rather than as an empty picker.

---

## 4. The stream

Nothing here is a special case in the code — the shared SSE parser handles it — but the shapes
are what a change would have to keep working:

- **The answer** arrives as `delta.content` frames. A first frame carries
  `{"role":"assistant","content":""}` before anything else.
- **Reasoning** arrives as `delta.reasoning`, beside a marker field `channel: "analysis"`.
  That is the field the app reads (`reasoning` is also what NVIDIA sends), so a thinking model
  shows its thinking in the collapsible box rather than in the answer. Measured on a
  "Reply with exactly: OK" turn: 113 characters of reasoning, `completion_tokens: 41`, of
  which `completion_tokens_details.reasoning_tokens: 31`.
- **Usage** is honoured via `stream_options.include_usage`: the frame carrying
  `finish_reason` arrives with `usage.prompt_tokens`/`completion_tokens`/`total_tokens`, and an
  **identical frame with `choices: []` follows it**. So the context meter gets a measured
  number rather than an estimate, and the parser has to tolerate an empty `choices` array —
  which it does, since that is OpenAI's own shape.
- The vendor's timing fields (`queue_time`, `prompt_time`, `completion_time`, `total_time`)
  are ignored; the app times a stream itself and badges the result `estimated`.
- **Tool calls** arrive fragmented in the usual `tool_calls` shape, with
  `finish_reason: "tool_calls"`.
- **`seed`** is this vendor's own name for the pinned seed and is honoured: a request with
  `seed: 42` comes back with `"seed": 42` in the body and in an `x_groq` object beside it.
  It is the default field name, so no `seedField` override is needed.
- The deprecated `max_tokens` is **still accepted** (measured, HTTP 200), which is why the
  shared request body needs no `max_completion_tokens` field of its own.

---

## 5. Reasoning that cannot be switched off

`reasoning_effort` exists on this API but has **no off value**: measured,
`reasoning_effort: "none"` is refused with HTTP 400 and "`reasoning_effort` must be one of
`low`, `medium`, or `high`".

So the provider sets neither `reasoningEffortOff` nor a thinking toggle: compaction's attempt to
silence thinking cannot be expressed here, and the honest answer is to not claim it. What
compaction does instead is the same fallback it uses for every backend that refuses the field
— it adapts its own output budget, so a summary asks for the room its answer needs rather than
assuming the model will not think. The `thinking` capability is still claimed, because the
model does reason.

---

## 6. Refusals and the rate-limit budget

Refusals are a small JSON envelope with a **code**, and the status is 401 for a missing key as
much as for a wrong one:

```json
{"error":{"message":"Invalid API Key","type":"invalid_request_error","code":"invalid_api_key"}}
```

That is why a missing key is answered locally (`ErrMissingAPIKey`) instead of by a request: the
vendor's body is the same either way and says nothing about where a key belongs, so
`TestGroqWithoutAKeyIsReportedAsSuch` pins the local answer, and
`TestGroqRefusalCarriesTheVendorsOwnWords` pins that a real refusal keeps the vendor's `code`
in the message the user sees. A wrong key is a key rather than a throttle, which is the
distinction `RESEARCH.md` §5 makes for Brave's rejected token.

**The budget is the tight resource here, and it is per minute — and per model.** Every
response carries `x-ratelimit-limit-requests`, `-limit-tokens`, `-remaining-*` and
`-reset-*`; the account measured allows **1000 requests** but only **8000 tokens per minute**
across it, while a single model on the `on_demand` tier publishes a ceiling of its own. A real
turn against `qwen/qwen3.8-27b` was refused with this, which is the shape every fact below is
read from — the model's own limit **7000**, the 1911 already spent, the 6248 this request was
counted at, and the wait in prose:

```json
{"error":{"message":"Rate limit reached for model `qwen/qwen3.8-27b` in organization
`org_01kz7a9rsze9da1b4q1yv7w8bx` service tier `on_demand` on input tokens per minute (ITPM):
Limit 7000, Used 1911, Requested 6248. Please try again in 9.934285714s. Need more tokens?
Upgrade to Dev Tier today at https://console.groq.com/settings/billing","type":"tokens",
"code":"rate_limit_exceeded"}}
```

A 128×128 PNG costs about **1300 prompt tokens** on its own, so an image turn is a sixth of a
minute's budget of that size — which is why the live test keeps every request short and makes
the image check opt-in (`KICHAT_GROQ_VISION_MODEL`).

Two exceptions are worth knowing about because the app maps them:

- **HTTP 429** is a throttle, not a verdict: `HTTPError.Retryable` reports it, the availability
  check deliberately does **not** blacklist a model over it (see `internal/llm/probe.go`), and
  the wait it names is *obeyed* rather than guessed at. `retryHint`
  ([internal/llm/httpclient.go:94](internal/llm/httpclient.go#L94)) reads `Retry-After` and, when
  that is missing or unparseable, the refusal's own sentence — the envelope above is why, since
  the number the vendor commits to is the one in prose — and `retryWait`
  ([internal/llm/httpclient.go:133](internal/llm/httpclient.go#L133)) waits it out, up to one
  minute, which is the window these limits are defined over. The fixed 250 ms/500 ms backoff
  this replaced spent every attempt inside the minute that had just been refused, so a request
  the vendor had said to retry in ten seconds arrived at the user as a failure.
- **HTTP 400** from the listing's own claims — a model that cannot chat, an oversized image —
  is the model's or the request's fault, and it is surfaced as sent.

---

## 7. Where the code is

| file | what it holds |
| --- | --- |
| [internal/llm/groq.go:75](internal/llm/groq.go#L75) | `GroqDefaultBaseURL` — the documented address |
| [internal/llm/groq.go:79](internal/llm/groq.go#L79) | `newGroqProvider` — the address rules, the single chat path, `requiresKey` |
| [internal/llm/groq.go:118](internal/llm/groq.go#L118) | `groqModelEntry` — the listing's fields, with `active` a pointer so a missing field is not a `false` |
| [internal/llm/groq.go:148](internal/llm/groq.go#L148) | `fromGroqEntry` — the mapping, the two drop rules, the capability claims |
| [internal/llm/groq.go:205](internal/llm/groq.go#L205) | `mapGroqEntries` — entry by entry, keeping the vendor's order |
| [internal/llm/openai.go:280](internal/llm/openai.go#L280) | `flavourGroq` — the branch that picks the mapper |
| [internal/llm/httpclient.go:94](internal/llm/httpclient.go#L94) | `retryHint` — the `Retry-After` header, and the wait a refusal writes in prose |
| [internal/llm/httpclient.go:133](internal/llm/httpclient.go#L133) | `retryWait` — the hint obeyed up to one minute, the doubling backoff otherwise |
| [internal/llm/registry.go:117](internal/llm/registry.go#L117) | the `case "groq"` that builds the provider |
| [internal/llm/registry.go:427](internal/llm/registry.go#L427) | the shipped-but-disabled entry |
| [internal/config/config.go:934](internal/config/config.go#L934) | the address repair in `normalise()` |
| [frontend/src/components/Settings.tsx:1518](frontend/src/components/Settings.tsx#L1518) | the kind list that shows the key field, marked required |

---

## 8. How this is verified

### Offline — `internal/llm/groq_test.go`

Against a listing served by a test server, in the shape measured on the live API:

- `TestGroqListingOffersOnlyWhatCanAnswer` — the four non-chat entries and the inactive one are
  dropped, the four chat models are not; the window is the published `context_window` rather
  than 0 or an invention; `gpt-oss-20b` claims tools and thinking and not vision, `qwen3.8-27b`
  is a `vlm` with vision, `allam-2-7b` claims none of the three.
- `TestGroqListingKeepsWhatTheListingDoesNotDescribe` — a bare `{"id","owned_by"}` entry is
  still offered, with no capabilities claimed.
- `TestGroqBaseURLCompletionFollowsThisVendorsPath` — empty, bare host, bare host with a
  trailing slash, the documented address and a gateway path, each against its chat path.
- `TestGroqRequestIsTheSharedOpenAIOne` — the request goes to the documented base plus
  `/chat/completions` with the bearer token, and carries `max_tokens`, `seed`, `stream` and
  `include_usage`.
- `TestGroqWithoutAKeyIsReportedAsSuch` and `TestGroqRefusalCarriesTheVendorsOwnWords` — the
  missing credential, and the `invalid_api_key` envelope.
- `TestRegistryBuildsGroqFromConfig` — the kind builds the provider, an empty address falls
  back to the vendor host, a disabled entry contributes nothing, and the provider borrowed
  no other vendor's request names: no `seedField` (this API's own name is the default) and no
  `reasoningEffortOff`/`thinkingToggle`, the two that would turn compaction's attempt to
  silence thinking into a 400 instead of its own output-budget fallback.
- `TestShippedGroqBackendIsOffByDefault` — the shipped entry is off, named `Groq`, and
  carries `/openai/v1` rather than the bare host every other hosted entry here uses.

And one in the root package, [groq_model_check_test.go](groq_model_check_test.go), where the
app's own sweep is driven against a Groq-shaped fixture:

- `TestTheAvailabilityCheckHidesGroqsStreamingRefusal` — the two halves of §3's decision
  joined up. The classifier row is *offered* before any check (the provider guesses nothing),
  the sweep asks it with the streaming call the chat makes and the vendor's 400 is the verdict
  `applyModelProbes` acts on, so it leaves the picker on the API's own answer — and the model
  that answered stays, the speech entry is never even asked, and clearing the blacklist brings
  it back. A probe that started reading 400 as inconclusive would leave a user two rows that
  can only fail with no way to clear them, and that is what this test exists to catch.

### Live — `internal/llm/live_groq_test.go`

```bash
KICHAT_LIVE=1 KICHAT_GROQ_KEY=<key> go test ./internal/llm/ -run TestLiveGroq -v
#   optional: KICHAT_GROQ_BASE, KICHAT_GROQ_MODEL, KICHAT_GROQ_VISION_MODEL
```

It drives `openai/gpt-oss-20b` unless told otherwise and asserts what the offline tests can
only fake: the listing maps the way the provider says it does (**7 chat models** from 11
entries, with the four non-chat ids asserted absent), the chosen model is `loaded` with a
**positive** window and a publisher, `include_usage` is honoured so the meter gets a measured
`prompt_tokens`, a real tool call streams, and a fake key is refused with HTTP 401 carrying
`invalid_api_key` — a key rather than a throttle. The image check is opt-in and asserts a real
answer to a picture the test draws itself, since a 1×1 fixture is refused by this API for a
reason that has nothing to do with the provider.

Two more assertions were added once they had been measured: every prompt-guard entry in the
listing is driven with the streaming chat call the engine makes and the HTTP 400 `"do not
support streaming"` is asserted (a model that starts streaming turns this into a failure and
the documentation into a correction), and `TestLiveGroqShippedEntryReachesTheAPI` takes the
entry `DefaultBackendConfigs` ships — switched on and given the key, exactly as the panel
leaves it — builds it through `NewRegistry` and makes it list and answer. That one is the only
check that covers the *wiring* rather than the mapper: a `case "groq"` that had stopped
building a provider, or a shipped address that had drifted, fails here and nowhere else. It
also documents the picker's problem in passing — it drives the first row whose published
`supported_features` claim tools rather than `models[0]`, because on this account the listing
leads with a classifier that refuses streaming.

Measured on the run this was written from: 1.8 s for the main test, 0.5 s for the shipped-entry
one, and the numbers in §4.

---

## 9. Known gaps

1. **The two prompt-guard classifiers stay in the picker, and they can be called but not
   streamed.** A non-streaming call answers HTTP 200 with a score; the streaming one the app
   always makes is refused with HTTP 400. Nothing in the listing separates them from the chat
   models, so a filter would be a rule about their names — but the request shape the app uses
   is a choice, and a one-shot fallback (send without `stream`, deliver the single answer as a
   delta) would turn these rows from a guaranteed failure into a working one. Not built;
   recorded with the measurement that would justify it.
2. **No image-input shape beyond data URLs has been measured.** A public `image_url` is
   documented; only a base64 data URL was exercised, and only on `qwen/qwen3.8-27b`.
3. **The audio and speech models are simply absent.** This backend is a chat provider, so
   Whisper's transcription and the Orpheus voices are dropped rather than exposed as some other
   kind of model.
4. **The rate-limit headers are read as a fact, not enforced.** The app does not pace requests
   against the per-minute token budget, and it does not reserve budget for the turn being typed;
   a burst that exhausts it comes back as a 429, whose own retry hint the shared client waits out
   (§6) before it is reported.
5. **`reasoning_effort` is never sent.** The app sends the field only to switch thinking *off*,
   which this API does not support, so its `low`/`medium`/`high` levels are unused.
