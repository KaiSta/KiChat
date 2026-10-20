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
| `context_window` | 131072 on `openai/gpt-oss-20b`, 4096 on `allam-2-7b` | `ContextLimit` and `MaxContext`, so no override is needed — but the window is **not** the largest request the account may send: see §6 |
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
`-reset-*`; the vendor's own note on them is that the `requests` family always means the
**day** and the `tokens` family always the **minute**. The account measured allows **1000
requests a day** but only **8000 tokens a minute** across it, while a single model on the
`on_demand` tier publishes a ceiling of its own — so the minute is what a long chat spends,
and it is the one the app reads (§6.4) and the one the numbers below describe. A real
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

**Three numbers in that family are read**, and the first of them does two jobs.
`x-ratelimit-limit-tokens` is the tokens-per-minute allowance of the model that produced the
response, and — as §6.1 shows — a request larger than a whole minute of that allowance cannot
succeed however long anyone waits. It is therefore also the largest **one request** the account
may send, and it arrives with every response: the answer as much as the refusal. The app reads
it (`ceilingFromHeaders`, [internal/llm/limits.go:136](internal/llm/limits.go#L136)) and keeps it
per model as the ceiling the next request is planned under, which is what moves the fold of an
over-long chat from *after* a refusal to *before* the request (§6.2 item 2) — and, with the
remainder and the reset beside it, as the minute a turn is paced against (§6.4). Two boundaries
on the ceiling are deliberate:

- **Only for this backend.** The header is read on a request the Groq provider makes
  (`modelsFlavour == flavourGroq`, [internal/llm/openai.go:1009](internal/llm/openai.go#L1009)),
  never for another OpenAI-compatible service that happens to send one. What the number means
  here is a measurement of *this* vendor — a per-minute allowance that a single request may not
  exceed — and another service's rate-limit header is not evidence about the largest request it
  accepts. Folding a chat against a number nothing measured would compact conversations that fit
  (`TestTheBudgetHeaderIsReadForGroqOnly`).
- **It may only tighten.** A response header and a refusal both state one limit in two
  different situations, and the smaller of the two becomes the plan: fitting the tighter of two
  statements fits both, while keeping the looser can cost a refused turn, and the rule makes the
  two writers order-independent (`TestACeilingOnlyEverTightens`). The cost is real and one
  direction — an allowance the account *raises* is not picked up until the app is restarted,
  which is the price of never having a ceiling that is too generous.

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

**And the wait is shown, because a silent one is indistinguishable from a hang.** A ten-second
wait while nothing arrives on the wire looks exactly like a turn that has stalled, so the retry
reports itself: `waitBeforeRetry`
([internal/llm/httpclient.go:154](internal/llm/httpclient.go#L154)) tells a `RetryWatcher`
([internal/llm/httpclient.go:180](internal/llm/httpclient.go#L180)) carried on the request's
context how long it is about to sleep and what it is sleeping for. The engine puts one on the
turn's own request — `watchingRetries`
([internal/chat/throttle.go:22](internal/chat/throttle.go#L22)) — and it publishes a
`chat:status` event carrying `phase: "throttled"` with `waitMs`, then retracts it to `idle` when
the wait ends, including when a Stop cuts it short. The panel renders that plus the number of
seconds beside the composer, in all three languages. Only the turn's request is watched: the
search seeding and the compaction already have phases of their own (`searching`, `compacting`),
and a listing, a health probe or the availability check explains no turn to anyone. And only a
wait the backend **named** is shown (`namedWait`): a plain backoff is 250 ms and then 500 ms, so
reporting it would put a line of status text on screen for a quarter of a second — a glitch
rather than an explanation — while an explicit `Retry-After` is both longer and the vendor's own
number.
- **HTTP 400** from the listing's own claims — a model that cannot chat, an oversized image —
  is the model's or the request's fault, and it is surfaced as sent.

### 6.1 The refusal that looks like a throttle, and is not one

Two failures that want opposite reactions share one envelope. Measured, a request that is
**larger than the whole per-minute allowance** — not one that has merely spent it — comes back
with the same `"type":"tokens"` and the same `"code":"rate_limit_exceeded"` as the throttle,
and the only thing that separates them is the sentence:

| | the throttle | the oversized request |
| --- | --- | --- |
| status | 429 | 413 |
| message | *"Rate limit reached for model … on input tokens per minute (ITPM): Limit 7000, **Used** 1911, Requested 6248. Please try again in 9.934285714s."* | *"**Request too large** for model … on tokens per minute (TPM): Limit 10000, Requested 15088, **please reduce your message size** and try again."* |
| what it means | the allowance is spent; waiting is the fix | the request does not fit under the limit **by itself**; waiting changes nothing |
| what the app does | obeys the wait, shows it, retries (§6) | never retries it, whatever the status (`internal/llm/limits.go`) |

Waiting is the wrong answer to the second one, and it is the expensive wrong answer: three
retries fit inside the same minute, and every one of them is a request that the limit exceeds
before it is even counted. The third shape of the same verdict is Groq's context-window
refusal — **HTTP 400**, `code: "context_length_exceeded"`, *"Please reduce the length of the
messages or completion."* — which is the model's own window rather than the account's
allowance. The classifier reads both, because both ask for the same thing, and it reads the
phrases rather than the code, because the code is the one thing the two cases share.

### 6.2 What the app does about it

That refusal names the fix — *"please reduce your message size"* — and the app already knew
how to do it: fold history into a memory. So a turn refused this way is no longer a failed
turn. `internal/chat/oversize.go`:

1. **The refusal is folded and the turn is sent once more.** The empty reply row a refused
   request leaves behind is dropped rather than kept as a failure — the retry answers the same
   user message — the history is folded, and the request goes out again. A fold with nothing
   older to fold folds nothing (`ProtectNewestTurn`: the message the user just sent is what the
   next request has to carry) and the turn is not retried, because the body would be identical.
   It is explained in the transcript's own language, in two sentences and only one of them a
   promise: *"Groq hat die Anfrage als zu groß abgelehnt (höchstens 10000 Token pro Anfrage,
   angefragt waren 15088)."*, and — **after** the memory exists, never before it — *"Der Verlauf
   wurde komprimiert — die Anfrage wird erneut gesendet."* The order is the point: a promise of
   a retry that never comes would be the one thing in that turn which is not true, and a fold
   can still fail (see item 4).

   **The fold is aimed with the vendor's own count, not the app's estimate.** A refusal states
   two numbers for the same body: `Requested`, what the vendor counted, and — from the assembly
   that was sent — what the app estimated. The estimator is a heuristic (§6.4), and on a ceiling
   that is a fraction of the window its error is the whole difference between a retry that fits
   and a *second* refusal, which ends the turn because a refusal is folded once. So the fold's
   aim is `ceiling × estimated / Requested` (`sizeTargetWithCount`, [internal/chat/oversize.go](internal/chat/oversize.go)),
   applied only when the vendor counted more than the app did and capped at 4× so a wild number
   cannot fold a chat to nothing. The same corrected ceiling budgets the summarisation call
   (`CompactOptions.SummaryCeiling`): it is a request against the same limit, and a transcript
   budgeted to a ceiling the vendor counts at twice the estimate is refused for the size of the
   call that exists to make the chat smaller.

   Measured, against a fixture that counts each request at twice the app's estimate: without the
   correction the folded retry is refused again — 12 626 counted against the 10 000 limit — and
   the turn ends *"auch mit komprimiertem Verlauf passt sie nicht hinein"*; with it the retry
   fits and answers (`TestAFoldedRetryFitsWhenTheVendorCountsMoreThanTheApp`). The summariser
   half is pinned separately: with a 30 000 ceiling and a 60-turn history an uncorrected
   transcript is estimated at 19 936 tokens, refused by the same vendor, and the corrected
   ceiling brings it under 6 000 (`TestTheSummariserFollowsTheCorrectedCeiling`).
2. **The ceiling is remembered, per model — and read from an ordinary answer wherever one
   exists.** No route of this API publishes it: the listing reports windows, not budgets. Two
   of the vendor's own statements do, and both are read.
   - **The response header**, which is the better route and the reason the first oversized
     request of a session is now folded rather than sent: `x-ratelimit-limit-tokens` accompanies
     the **answer** as well as the refusal, so the first successful turn of a session already
     teaches the app the limit the second turn is planned under. `requestBudget`
     ([internal/llm/openai.go:190](internal/llm/openai.go#L190)) is handed every response's
     headers by the shared client, before its body is read — a refusal's headers as much as a
     success's.
   - **The refusal's own sentence**, which is what a session whose *very first* request is
     already oversized still pays once: `Limit 10000` in *"Request too large … Limit 10000,
     Requested 15088 …"*. It arrives after the fact, which is the whole difference between the
     two sources.

   `openAICompatProvider` keeps whichever is tighter under `noteRequestCeiling`, published as
   `llm.RequestCeiling` with the source it came from, and from then on **the next turn is folded
   before the request goes out** — as one of the two bounds of the planner's single automatic
   decision, compared with the user's own threshold in `PlanCompaction` (`automaticBound`), so a
   turn that would cross both folds once rather than twice (COMPACT.md §4, §5.1). The refusal is
   therefore paid once per model per session rather than once per turn — and, after the first
   answer *or* refusal, not at all.

   Both numbers are visible: the context meter measures against the effective per-request budget
   and the inspector names the ceiling beside where it came from (§6.3).

   **The check runs again between tool rounds.** A turn is not one request on this backend's
   budget: a tool-heavy answer is several, and each round appends a model reply and a tool result
   that the check before the turn never priced — which is why a long turn could still be refused
   mid-answer even though its first request fitted. `generate` now asks the same planner before
   every round after the first (`autoFold` with `midTurn` set), so the fold happens between the
   rounds rather than being paid for on the turn's error, where it restarts the turn and loses the
   work already done. Only the ceiling is asked there; the user's threshold belongs to the history
   the turn was started with (COMPACT.md §4). Measured: a 12-turn history under a ceiling
   placed just above it folds on the second round — the oldest turns gone, the new message kept,
   no refusal (`TestAToolRoundFoldsBeforeItsNextRequestCrossesTheCeiling`).

   **A number the user types in is the only source that speaks first.** Both routes above need
   a request to have been answered, so the very first oversized request of a session — a fresh
   process, a newly chosen model, or any settings save, which rebuilds the providers and drops
   what they had learned — is still refused once. Nothing can be fetched in advance for it: there
   is no limits route (`/openai/v1/models` returns model metadata only), the exact figure for an
   organization is on a signed-in Limits page, and the public tables cannot stand in for it —
   read on 2026-10-17, [the Rate Limits page](https://console.groq.com/docs/rate-limits) lists
   **8K TPM** for `openai/gpt-oss-120b`, `openai/gpt-oss-20b` and `qwen/qwen3.8-27b` while
   [the Models page](https://console.groq.com/docs/models) lists **250K TPM** for the same three,
   both labelled Developer plan. So the per-model override (Settings → Backends → override) has a
   request-ceiling field, offered for this backend's models only, and `Engine.ceilingFor` merges
   it with whatever the provider learned, keeping the smaller of the two: the typed number fills
   a silence and never overrules a measurement, so a stale entry costs an extra fold rather than
   a refused request. It is written by `SetModelOverride`, which sits outside the settings form, so
   `SaveSettings` re-reads the recorded overrides rather than the draft's — the same rule the
   hidden-model list follows, and the reason a Save of an unrelated setting cannot drop a ceiling
   that was typed a moment earlier (`TestAnUnrelatedSaveKeepsAModelOverride`).
   `TestTheUsersOwnCeilingFoldsBeforeTheRequestGoesOut` pins that it drives the
   same preventive fold (one summary, no refusal, before the first request),
   `TestATypedCeilingNeverLoosensWhatTheBackendStated` the direction of the merge, and
   `TestTheMeterShowsTheTypedCeiling` that the meter paints it with the source `manual`.
3. **The summariser is budgeted against it too.** The call that makes a chat fit is a request
   of its own, subject to the same allowance: a transcript budgeted against the summary model's
   131k window is one more oversized request, which is how a compaction that exists to fix this
   fails with the refusal it was folding away. `compactWith` clamps its budget to the ceiling
   the summariser publishes.
4. **A tool result is cut to the room the next request has left.** This is the one growth no
   fold can take back, and it is the shape the reported failure had: the model thinks, calls
   `search_web` or `scrape_url`, and the round that reads the result is refused because the
   result alone is bigger than the whole allowance. A fold cuts at user-turn boundaries, so the
   reply and the tool traffic of the turn being answered stay verbatim — which is exactly the
   material that has to shrink. The user's own tool budget is no help either: it defaults to
   **25000** tokens, three times an 8K allowance, and one `scrape_url` may ask for **12000** on
   its own. So `generate` budgets the result against what the next request can actually carry —
   `roomForToolResults` is the effective limit less what the assembly already priced, the
   reserve for the answer and the reply this round wrote — and `toolOutputBudget.capTo` only
   ever tightens, so a backend that publishes no ceiling keeps the behaviour it had. A result
   cut this way carries the shared `…[gekürzt]` marker, so the model can read a narrower page
   range instead of re-scraping the same URL. Measured: a 13 480-token document read back
   through `read_pages` under a 6000-token ceiling reaches the model cut, and the second request
   fits (`TestAToolResultIsCutToWhatTheRequestCanCarry`).
5. **A refusal that survives all that is explained rather than quoted.** The vendor's sentence
   names an organization, a service tier and a billing page; what the user is shown is which
   limit was hit, what the app did about it, and what reduces the request — *"Kürze die
   Nachricht, entferne Anhänge oder wähle ein Modell mit einem höheren Limit."* What it did is
   one of three outcomes (`foldOutcome`), because "the fold did not happen" is not one fact:
   *"auch mit komprimiertem Verlauf passt sie nicht hinein"* (a memory was written and the turn
   was refused anyway), *"der Verlauf ließ sich nicht komprimieren"* (a fold was attempted and
   broke — a summariser that was refused, had no backend, or ran out of room), and *"es gab
   keinen Verlauf, der sich noch hätte zusammenfassen lassen"* (there was nothing older to
   fold). Only the **middle** one is a failure of the app, and it is reported beside the
   warning that names the cause, so the two sentences describe one event instead of
   contradicting each other.

The fold is forced, which puts it beside the Compact button rather than beside the automatic
threshold: `settings.Compact.Enabled` and a chat's `AutoCompact` decide whether compaction
happens *quietly*, and a refusal has already happened. Its card says `too_large` rather than
`manual` / `auto` — a fold nobody asked for should not read as a button somebody pressed. The
fold that *precedes* an expected refusal keeps the `auto` name, because a threshold was
crossed and nobody pressed anything.

### 6.3 If a chat is already too big, and what the meter shows

Nothing has to be undone by hand: send the next message. The turn that would have aborted now
folds what it can and answers. If even that is refused, the error names the limit and says what
helps — shorten the message, remove an attachment, choose a model with a larger allowance. The
Compact button also lowers what the next request costs, and the context meter's `projected`
number is the figure to watch once a ceiling is known.

The meter is where the ceiling is visible, and it has to be, because the window it used to
measure against is not the limit the request hits. `ContextBudget` therefore carries the ceiling,
the source it came from, and `EffectiveLimit` — the smaller of the window and the ceiling — and
**`Percent`, `ProjectedRatio` and `OverLimit` are all computed against the effective limit**
([internal/chat/context.go:130](internal/chat/context.go#L130)). A chat at 9k tokens of a 131k
window under an 8k ceiling reads **108 %**, not 7 %: the old reading was room that did not
exist. The switcher's inline meter shows `used / effective`, and its tooltip says which of the two
is binding — *"One request may carry 8000 tokens; the model window is larger (131072)."* — while
the inspector lists the ceiling and its source as a row of its own:

| source | what the inspector says |
| --- | --- |
| `llm.CeilingFromHeader` | *from the backend's own response* — no request was lost to the number |
| `llm.CeilingFromRefusal` | *from a refused request* — one was already refused to learn it |

The source is not decoration: it is the difference between a limit the app knew before the
damage and one it paid for, and it is the honest thing to show when the two routes to one number
are not equally cheap. Backends that publish no ceiling (every one but Groq) carry `ceiling: 0`
and a meter that is unchanged, measured against the window as before.

### 6.4 Pacing the minute

The ceiling answers *"will this request fit at all"*. The other failure a long chat runs into is
the one the same allowance produces slowly: the minute's budget is spent, and the next turn is
refused with a wait of seconds — measured, `Limit 7000, Used 1911, Requested 6248. Please try
again in 9.934285714s.` The shared client already obeys that wait and shows it (§6), so the turn
survives. What it does not do is avoid asking for the refusal in the first place, and that is
what §6.4 is: the request is held back until the numbers the vendor itself published say there
is room for it.

**The minute is measured two ways, and each is read from the evidence that exists for it.**
One of them is the vendor's own statement and the other has to be reconstructed, which is the
whole shape of this section:

- **Tokens**, from two more headers of the same family, sent on every response:
  **`x-ratelimit-remaining-tokens`** — what is left of the minute — and
  **`x-ratelimit-reset-tokens`** — how long that remainder lasts, as a duration (`7.66s`,
  `2m59.56s`). Together with the limit they are read by `budgetFromHeaders`
  ([internal/llm/pace.go](internal/llm/pace.go)) into a per-model statement. A statement is not
  a counter: past its reset the whole allowance is back, which is what a rolling minute means,
  and the newest response is the freshest reading of a quantity that moves rather than a second
  opinion about a fixed one.
- **Requests**, from nothing in the headers at all. The `requests` family beside the token ones
  is the **day's** allowance (`x-ratelimit-limit-requests` is RPD, per the vendor's own note),
  and the minute's request rate — 30 for the models this was measured against — is published in
  the console's table and in no response. So the app learns the *number* where the vendor states
  it and measures the *spend* itself:

  - **The number** comes from a refusal that names one, and only from that. Measured on another
    client's report (a client making 30 autocomplete calls back to back, which is exactly the
    shape a tool-heavy turn has):

    ```json
    {"error":{"message":"Rate limit reached for model `llama-3.1-8b-instant` in organization
    `org_…` on requests per minute (RPM): Limit 30, Used 30, Requested 1. Please try again in
    1.969s. Visit https://console.groq.com/docs/rate-limits for more information.",
    "type":"requests","code":"rate_limit_exceeded"}}
    ```

    The sentence names `requests per minute (RPM)` where the token throttle names input or
    output tokens (ITPM/OTPM), and `type` says `requests` where the token one says `tokens`;
    both are read, because the `code` is `rate_limit_exceeded` for all of them
    (`requestsPerMinuteLimit`). The number may only ever **tighten**, for the reason the ceiling
    does: a plan that fits the smaller of two statements about one limit fits both. What it must
    not do is touch the count, which is a measurement rather than a statement.
  - **The spend** is the app's own: every response this provider receives for the model, counted
    in the trailing minute, including the refusals. That is the only measurement of a rate
    nothing publishes — and it is a *lower bound* on the account's spend, since a second client,
    or this app's own listings and probes, spend the same allowance without being counted. The
    error has a direction on purpose: the vendor's own 429 still covers what the count misses,
    while a count that over-estimated would park turns for room the account had all along.
    Counting at the *response* is what makes it a count of requests that reached the vendor
    rather than of requests the app intended: a retry the shared client performed is one more,
    and a connection that never got an answer is none.

  A rate of 30 with 30 counted is a full minute, so the next request waits for the oldest of
  those 30 to fall out of the window — up to a minute, and by construction never more. The count
  is per model, because the limit is: the same account has 30 for `openai/gpt-oss-20b` and 3 for
  a guard classifier.

The rule is one sentence: **a request waits when either allowance cannot pay for it, and
nothing is waited for that the vendor's numbers do not say.** Five things follow from it, and
each is a boundary rather than an implementation detail:

- **Both allowances must have room, so the wait taken is the longer of the two.** A minute with
  tokens to spare and no requests left is still a minute this request cannot be sent in. Each
  half reports "no wait" for its own reasons — an unknown number, a covered request, a token
  reset too far out to be this vendor's minute — and the two are independent, so neither can
  veto the other's wait.
- **The cost compared is an estimate; the allowance is not.** It is the same heuristic the
  meter and the compaction planner use, because the allowance is spent by tokens rather than by
  prompts: the prompt (`EstimateMessagesTokens` + `EstimateToolTokens` — the tool block priced as
  the JSON it is sent as, since the flat per-schema figure undercounted the five shipped tools,
  1 261 against the 1 385 a 128k tokenizer counted) plus the answer's room,
  which is the `max_tokens` the request asks for and nothing when it asks for the backend's own
  default. Two refinements keep that from doing harm. A *prompt* larger than the whole allowance
  is the case no wait can help, so that is what the "can never be sent" guard compares — the
  answer's room is a cap the user set rather than a promise, and a large `max_tokens` must not
  read as a request too big to send, which would switch the pacing off for exactly the chats
  that spend the allowance fastest. And the wanted cost is capped at one window: beyond it the
  answer is "wait for the whole minute", which is the honest reading of a request that needs
  most of one. The estimate is only ever used to decide whether to *wait* — an underestimate
  costs the refusal that would have happened anyway, and an overestimate costs seconds. Nothing
  about the vendor's own numbers is estimated.
- **The newest statement wins** — the opposite rule from the ceiling's. The ceiling is one fixed
  limit that a refusal and a header both describe, so the tighter of the two is kept; this is a
  reading of a moving quantity, and a later response is a fresher measurement of it rather than a
  second opinion about it. Keeping the older, smaller remainder would make the app wait for a
  minute that has already been refilled. The window also rolls over on its own: past the reset
  the whole allowance is back, whether or not anything was sent.
- **A wait that cannot be shown is not taken.** Only the turn's own request carries a
  `RetryWatcher` (§6), so only that one is paced. A listing, a health probe or an availability
  check has no phase to say anything with and is bounded by its own timeout, so parking one of
  those silently for a minute would look like a hung app and then fail anyway.
- **Nothing is waited for past one window.** These allowances are defined per minute, so a reset
  beyond a minute is not this vendor describing its own minute; the request goes out and the
  vendor's own 429, with its hint, is the answer. And a prompt larger than a whole minute's
  allowance is not waited for either: no wait can make it fit, so it is left to the size fold
  (§6.1–6.2) instead of delaying that fold by the minute it is trying to fit inside.

What the user sees is the wait, in the shape §6 already established — a phase carrying the
seconds — but with its own sentence, because the two waits are not the same event:

| phase | when | what it says |
| --- | --- | --- |
| `throttled` | a request was *sent*, refused, and is going out again | *"The backend is throttling this request — retrying in about 9 s."* |
| `paced` | a request has **not been sent**; one of the minute's two allowances has no room for it yet | *"This model's allowance for the minute is spent — the request goes out in about 9 s."* |
| `spread` | a **turn** needs more requests than the minute allows, so its rounds go out one per slot | *"This answer needs more requests than the minute allows — the next round goes out in about 2 s."* |

The distinction travels on the watcher itself: a paced wait is the only one with **no error
behind it**, which is exactly the difference between a request that was refused and one that has
not been made (`llm.RetryWatcher.Waiting`). Both phases were already needed for the same reason
(§6): a ten-second wait while nothing arrives on the wire is indistinguishable from a hang.

The sentence says *allowance* rather than naming tokens, and that is deliberate: the paced wait
can now come from either half of the minute, and one line that named the wrong one would be a
false statement about a turn that is waiting. Which of the two is binding is a detail the user
cannot act on differently — both are "wait a few seconds" — while the tooltip and the inspector
are where the numbers live.

What is deliberately **not** done: nothing is reserved for a turn that is merely being typed, and
nothing outside a turn is paced. The allowance is spent by requests, so that is where it is
measured — a budget held back for a message nobody has sent yet would make the meter understate
the room a chat has.

#### A turn that needs more requests than the minute allows

Everything above is per request, and a per-request guard is not a plan for a *turn*. Ten tool
rounds against a minute with two slots left send two requests at once and then wait at the wall for
each of the other eight — and the wall is exactly where this app's arithmetic is weakest, because
its count is a *lower bound* on the account's spend (a second client, or this app's own listings
and probes, are spent too and are not in it) while the vendor refuses on its own. So the tool loop
reads the allowance **before its first request** and spaces its rounds against it
(`internal/chat/pace.go`, `planTurnRequests`).

The reading is a value of its own, `llm.Allowance`, published by the provider through
`llm.RequestAllowance` — the same shape as `llm.RequestCeiling`, and gated the same way: this
vendor's numbers only. It carries the two halves the per-request guard already keeps, plus the one
thing a plan needs that a guard does not: **when each counted request frees its slot** (`Free`).
Without it a series spaced against the rate alone would put its next request where the window has
no room for it.

`Allowance.Spread(requests)` returns the wait before each of them, and the turn turns those into
the moments its rounds go out:

- **The series is spaced at the allowance's own rate**, one request per `window / limit` — two
  seconds for thirty a minute. The first request is not held back while the window can pay for it,
  and each one after it waits for its own place in the minute.
- **The arithmetic is a simulation, not a division**, because the window already holds requests
  when the series starts: those are in `Free`, and the requests the plan places join them as they
  are placed. A slot is available while fewer than `Limit` requests are inside the trailing minute
  *including* the one about to be sent — the vendor refuses the request that reaches the limit, not
  the one that passes it. Twenty-nine of a thirty-request minute that all leave in ten seconds is
  the case this exists for: two seconds later is not a slot, ten seconds is.
- **Nothing is spaced when the turn fits, or when no rate has been stated.** Inside the minute's
  room the earliest request is the best one — the window rolls continuously, so taking a slot now
  cannot cost a later one — and a turn that fits therefore behaves exactly as it did before.
- **It is a schedule, not a reservation.** The plan covers as many rounds as the iteration cap
  allows, which is the worst case; a turn that uses fewer leaves the later slots unspent, and a
  turn that never calls a tool pays nothing for their having been planned.

What it costs is small and stated: the turn's *last* request can land up to one round later than a
greedy schedule would have sent it, in exchange for the burst being gone. What it buys is that no
request is sent to sit at the limit — the state in which a vendor whose accounting is a little ahead
of ours refuses it — and that the waits are known before the turn starts instead of being discovered
by the request that hits the wall. A paced wait says the allowance is spent and a spread wait is
taken while it is *not* (the app is deliberately spreading the turn), which is why the third phase
exists rather than one sentence serving both.

Two things are deliberately not done. The reading happens **once**, at the start of the turn, so a
retry the shared client performs — or a second chat's turn — changes the count under the plan: the
per-request guard stays where it is, behind the plan, and remains the thing that decides whether a
request may go at all, which is why a turn that outran its plan meets the guard rather than the
vendor. And nothing is planned for a rate nobody has stated, so a session whose first request is
already over the rate still pays that one refusal before any of this can help (§9.6).

The between-rounds fold (§6.2) is one more request the plan never counted, and it is left that way
on purpose: the plan is drawn before the turn, and whether a fold is needed is known only after a
round has been priced. It is rare by construction — a round has to cross the ceiling for it to
happen at all — and it is covered twice, by the per-request guard behind the plan and by the
shared client waiting out a throttle if the fold lands on a spent minute.

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
| [internal/llm/limits.go](internal/llm/limits.go) | `ErrRequestTooLarge`, `classifyRequestTooLarge` — the phrases that tell an oversized request from a throttle, and the `Limit`/`Requested` read out of the refusal |
| [internal/llm/limits.go:118](internal/llm/limits.go#L118) | `rateLimitTokensHeader`, `ceilingFromHeaders` — the per-request budget read out of a response |
| [internal/llm/pace.go](internal/llm/pace.go) | `budgetFromHeaders`, `modelBudget`, `noteRequestBudget`, `paceRequest` — the minute, and the wait before a request it cannot pay for — and the turn's half of it: `Allowance`, `RequestAllowance`, `Allowance.Spread` |
| [internal/chat/pace.go](internal/chat/pace.go) | `planTurnRequests`, `turnPacing.waitFor`, `holdRound` — a turn's rounds planned against the minute before its first request, and the wait that spreads them |
| [internal/llm/httpclient.go:255](internal/llm/httpclient.go#L255) | `responseWatcher`, `withResponseWatcher`, `observeResponse` — every response's headers, before its body is read |
| [internal/llm/openai.go:159](internal/llm/openai.go#L159) | `noteRequestCeiling` / `RequestCeiling` — the per-model ceiling, tightened rather than replaced, with `requestBudget` as the header's reader |
| [internal/chat/compact.go](internal/chat/compact.go) | `automaticBound`, `atCeiling`, `ceilingRatio`, `ceilingTarget` — the ceiling as one of the two automatic bounds the planner compares, and the aim it folds to |
| [internal/chat/oversize.go](internal/chat/oversize.go) | `foldForSize` (the recovery), `sizeTarget`, `turnFailureText` |
| [internal/chat/context.go:37](internal/chat/context.go#L37) | `ContextBudget.Ceiling` / `CeilingSource` / `EffectiveLimit` — the limit the percentage is measured against |
| [frontend/src/components/ModelSwitcher.tsx:23](frontend/src/components/ModelSwitcher.tsx#L23) | `ContextMeter` and `ContextInspector` — the effective budget, the note, and the row naming the ceiling's source |
| [internal/chat/engine.go:330](internal/chat/engine.go#L330) | the `runTurn` wiring: the fold before the request, the one retry after a refusal, the message the user is shown |
| [internal/chat/generate.go:80](internal/chat/generate.go#L80) | the tool loop: the minute's request allowance is read before the first round, and each round waits for its own moment |
| [internal/chat/maintenance.go:270](internal/chat/maintenance.go#L270) | the summarisation budget clamped to the summariser's own ceiling |
| [internal/llm/httpclient.go:94](internal/llm/httpclient.go#L94) | `retryHint` — the `Retry-After` header, and the wait a refusal writes in prose |
| [internal/llm/httpclient.go:133](internal/llm/httpclient.go#L133) | `retryWait` — the hint obeyed up to one minute, the doubling backoff otherwise |
| [internal/llm/httpclient.go:180](internal/llm/httpclient.go#L180) | `RetryWatcher`, `WithRetryWatcher`/`RetryWatcherOf` — how a wait is reported to whoever is painting a window |
| [internal/chat/throttle.go:17](internal/chat/throttle.go#L17) | `throttledPhase`, `pacedPhase`, `spreadPhase`, `watchingRetries`, `emitWaiting` — the three waits as a `chat:status` phase, with their length |
| [frontend/src/components/ChatView.tsx:348](frontend/src/components/ChatView.tsx#L348) | the line that renders the phase and the seconds it named, and the three sentences a wait can carry |
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

### Offline — the retry itself, and what the panel is shown

The wait is arithmetic and one phase, so it is pinned in two packages rather than against the
live budget (`internal/llm/retry_test.go`, `internal/chat/throttle_test.go`):

- `TestARetryReportsItsWaitToTheWatcher` — a 429 with `Retry-After: 0.2` against a real test
  server reports **one** wait of about 200 ms, carrying the 429 that caused it, and then reports
  it over. The hint rather than the 250 ms backoff, which is the whole point of the report.
- `TestAWaitCutShortByAStopIsStillRetracted` — the same request with a 30 s hint, cancelled from
  inside the report: the call returns in well under a second and the wait is still retracted,
  which is what keeps a Stop from leaving "waiting for the rate limit" on screen afterwards.
- `TestAPlainBackoffIsNotReportedToTheWatcher` — a 503 with no hint is still retried after the
  client's own 250 ms and the caller is told **nothing**: no line, and no retraction either. The
  negative half of the policy, which is the half a flicker of text would come from.
- `TestAThrottledWaitIsShownAsAPhaseOnTheChat` — the engine's half, which is the half a UI bug
  would live in: the turn's request carries a watcher at all, the wait becomes a `chat:status`
  event on **that** chat with `waitMs` **9123** in it, the next status event is `idle`, the turn
  does not end in a phase, and the reply still arrives after the wait. The provider is a fake
  rather than a server because the client's half is the two tests above; what this one pins is
  the wiring between them.
- `internal/chat/pace_test.go` — the turn's plan and its wiring, which is the half a paced turn
  has that a paced request does not: `TestAToolHeavyTurnIsSpacedOverTheMinute` (no round follows
  the one before it by less than the allowance's period, the first is not held back while the
  minute has room, and nothing past the plan waits), `TestAToolHeavyTurnIsSpacedAndSaysSo` (the
  same turn driven through the engine: it still makes every round of its cap, it takes the time
  the plan asked for, and the `spread` phase arrives carrying the milliseconds the plan named),
  `TestATurnWithoutARateIsNotPlanned` (a backend that cannot state one, and the one that can but
  has stated nothing), and `TestATurnThatFitsIsNotHeldBack` — the regression a plan like this
  could introduce, where every answer would pay its latency rather than only the tool-heavy ones.

### Offline — the oversized request, in two packages

The refusal and the fold it triggers are pinned without the network, because neither is about
Groq's live state:

- `internal/llm/limits_test.go` — `TestAnOversizedRequestIsToldApartFromAThrottle` puts the two
  measured envelopes side by side and asserts which is which, with the negatives that keep it
  honest: a throttle is *not* classified (its retry and its wait would be lost), a 5xx is not
  (a phrase in a proxy's error page must not make a server error unretryable), and an invalid
  key is not. `TestAnOversizedRequestIsNeverRetriedEvenWhenTheStatusSuggestsIt` pins the
  reason the check is explicit rather than left to the status, and
  `TestGroqKeepsTheCeilingItsRefusalNamed` drives a real 413 through the provider: one request
  goes out, not three, and the 10000 the refusal stated is readable afterwards.
- `internal/llm/pace_test.go` — §6.4 against a server that publishes a minute: the three
  headers read (and every unreadable shape refused) in `TestTheMinuteIsReadFromTheResponsesOwnHeaders`,
  the window rolling back to whole on its own, and the newest statement winning over an older and
  smaller one. `TestASpentMinuteIsWaitedOutBeforeTheRequestGoesOut` sends one request that cannot
  be paced (nothing has stated a budget yet) and one that is, and asserts the wait it took was the
  reset header's ~400 ms, that the watcher was told **no error with it**, that the phase was
  retracted, and that the held-back request *was* sent afterwards. The negatives carry the rest:
  a covered request is not delayed, an unwatched one is never held (`TestAnUnwatchedRequestIsNeverPaced`),
  a reset beyond one window is sent instead of waited out, a request larger than the whole
  allowance is left to the size fold, another vendor's lookalike headers pace nothing, and a Stop
  during the wait ends the turn without the request ever reaching the wire. The turn's half is
  arithmetic and is pinned without a server at all: `TestASeriesIsSpacedAtTheAllowanceRate`
  (thirty a minute is one request every two seconds — forty requests, exactly that plan),
  `TestASpentMinuteIsWaitedOutAndThenSpaced` (the window gives nothing until its oldest request
  leaves it, and the rate applies from there), `TestASlotIsNeverTakenBeforeTheWindowHasRoom` (the
  simulation rather than the division: twenty-nine of thirty still inside a minute that frees up
  in ten seconds means ten seconds, not two), `TestNoRateAndAFittingSeriesAreNotSpaced` (the two
  cases in which no plan may exist at all), and `TestTheAllowanceIsWhatTheAppCountedForTheModel`
  with `TestTheAllowanceIsNotReadFromAnotherVendor` for the reading itself.
- `internal/chat/oversize_test.go` — `TestASpentMinuteWaitsBeforeTheTurnGoesOut` is the same
  thing through the engine and the registry's own `groq` kind, where what matters is the part a
  provider test cannot see: the turn waits a second and a half the server's reset header named,
  the **`paced`** phase with that number is published while nothing is on the wire, it is
  retracted to `idle`, and the turn still answers. `internal/chat/throttle_test.go` keeps the
  other phase honest — a wait that arrives with an error is still `throttled`.
- `internal/llm/groq_test.go` — `TestGroqLearnsTheCeilingFromTheResponsesOwnHeaders` is §6's
  second half: one server answers a 413 whose sentence says 10000 while its header says 8000,
  and then answers normally; the tighter number is what the provider keeps, the source is the
  header, and nothing was learned about a model nobody called.
  `TestACeilingOnlyEverTightens` pins the two-writers rule in both arrival orders and refuses an
  unattributed number, and `TestTheBudgetHeaderIsReadForGroqOnly` pins the boundary — a custom
  OpenAI-compatible server sending the same header teaches the app nothing.
- `internal/chat/oversize_test.go` — `TestGroqsOversizedRefusalFoldsTheChatAndAnswers` drives
  the recovery through the registry's own `groq` kind against a Groq-shaped server, and asserts
  what the change is for: the turn ends with an answer instead of an error, no row is left
  marked failed, the compaction is named `too_large`, the warning carries the limit the vendor
  named, the retry body is smaller than the refused one and no longer carries the folded turns,
  and the ceiling is readable on the provider. `TestGroqWithoutAnythingToFoldSaysWhatToDo` pins
  the other end: a first message that is already too large is not retried (the body would be
  identical) and the error names the limit and the fix. `TestAKnownCeilingFoldsTheHistory…`
  pins the preventive fold — a 131k window whose threshold would never fire, under a 2000-token
  ceiling — and `TestTheSummariserIsBudgetedAgainstTheCeiling` pins that the summary call itself
  fits. `TestASizeFoldIsNamedInTheTranscript` and `TestTheSizeTargetAimsTheFoldAtTheCeiling`
  cover the two smaller facts those rest on: the trigger name, and the arithmetic that hands a
  ceiling to a planner that prices against the window. `TestTheFoldAimsWithTheVendorsOwnCountOfTheRequest`
  pins the correction that follows a refusal — twice the estimate means half the aim, an
  over-count changes nothing, a tiny estimate is noise and a wild ratio is capped — and
  `TestAFoldedRetryFitsWhenTheVendorCountsMoreThanTheApp` drives the same thing through the
  engine against a fixture whose vendor counts twice the app's estimate, where the turn used to
  end in a second refusal after the summarisation had already been paid for.
  `TestTheSummariserFollowsTheCorrectedCeiling` pins the one call left in that flow.
  `TestGroqLearnsTheBudgetFromAnAnswerAndFoldsTheNextTurn` is the end-to-end case that no refusal
  is involved in at all: a server that only ever answers normally, with the budget header on the
  answer, and the *second* turn folded before it goes out — asserted through the registry's own
  `groq` kind, together with the `chat:budget` event the meter is painted from
  (`ceiling` 8000 / `ceilingSource` "header", `limit` 131072, `effectiveLimit` 8000).

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
`invalid_api_key` — a key rather than a throttle. It also asserts the header reading against
real headers: **an ordinary answer must leave a ceiling behind**
([internal/llm/live_groq_test.go:209](internal/llm/live_groq_test.go#L209)), with
`CeilingFromHeader` as its source and a number of its own rather than the window the listing
publishes — which is the one claim §6's header route rests on that no fixture can stand in for,
and the run logs the number it read. The image check is opt-in and asserts a real
answer to a picture the test draws itself, since a 1×1 fixture is refused by this API for a
reason that has nothing to do with the provider.

`TestLiveGroqOversizedRequestIsRecognised` is the measurement behind §6 that no offline test can
stand in for: the real envelope for a request the account will not accept. Everywhere else those
fixtures come from published reports of the 413 — *"Request too large … Limit 10000, Requested
15088, please reduce your message size"* — so a run of this one is what confirms the wording, the
`Limit`/`Requested` pair and the ceiling kept from it. It sends one ~38k-token request against a
free-tier allowance of 8k, and an account that accepts it skips itself rather than failing.

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
4. **Nothing is reserved for the turn being typed, and the meter never shows a reservation.** The
   pacing of §6.4 holds a request back when the allowance left cannot cover it, but the allowance
   is only ever spent by requests: a chat with 6 248 input tokens against a 7000-per-minute
   allowance reads as room in the meter, and the next turn is the one that discovers there was
   none. Reserving it would mean guessing at a message nobody has typed — the meter would
   understate what the chat can send — so the meter keeps describing the request that exists and
   the pacing pays the seconds when it does not fit. What pacing misses, the vendor's 429 and its
   hint still cover (§6), and what it cannot help with at all is a request larger than the whole
   allowance, which is the size fold's case (§6.1–6.2).
5. **`reasoning_effort` is never sent.** The app sends the field only to switch thinking *off*,
   which this API does not support, so its `low`/`medium`/`high` levels are unused.
6. **The first request of a session is still planned against the window alone.** The budget
   header is read from a *response*, so a model the app has never been answered by or refused
   for has no ceiling yet — the number arrives with the first turn, and a session whose very
   first turn is already oversized is still sent and refused once, then folded and retried. Every
   later turn on that model, in that session, is planned against the real limit. Nothing this API
   offers closes that hole: the ceiling is per model, and a listing request would be describing
   the account rather than the model being planned for. The ceiling is therefore one request
   behind by construction, not by omission. The pacing of §6.4 sits on the same footing: the
   minute is read from a response too, so the first turn of a session is not paced either — and
   it is the one turn that most often pays for the discovery. So is a *turn's* plan (§6.4): with
   no rate stated there is nothing to space against, so the one crossing is paid first and the
   spreading begins after it. The same shape costs more than a restart suggests: the learned
   numbers live in the provider instance, and `SaveSettings` rebuilds the registry
   (`App.rebuildServices`) — so any settings save, however unrelated, starts a session's ceiling,
   rate and estimate correction over again and the next oversized turn pays its refusal anew.
7. **A raised allowance is not noticed until the app restarts.** The ceiling only ever tightens
   (§6), so a key that moves to a higher tier keeps being planned against the smaller number it
   was measured at. Re-reading it would have to mean *loosening* what a refusal proved, and the
   safe direction is worth more than the rediscovery: the cost of the stale number is one extra
   fold, and the cost of a too-generous one is the refused turn this whole section exists to
   avoid. (The *remainder* is the opposite case and is refreshed by every response — §6.4 — so a
   tier change deforms the plan only until the next answer.)
8. **The day's request allowance is read by nothing, and the minute's only half measurable.** The
   `requests` headers are deliberately unread: the vendor's own note on the family is that they are
   always the **day's** (`x-ratelimit-limit-requests` is RPD), so they would pace a turn against a
   window nine hundred times too long. What §6.4 paces on instead is the minute's request *rate*,
   and two of its three parts are only partly available:
   - **The number comes from a refusal**, so a session's first crossing is still sent and refused —
     the same one-request-behind shape the ceiling has (§9.6). It is the vendor's own `Limit 30`
     when it arrives, and never an assumed rate; the rate is per model, like every other limit here.
   - **The spend is this app's own count of responses**, which makes it a *lower bound*: requests
     made outside a turn — listings, the availability check — are not counted, and neither is a
     second client of the same account. The vendor's 429 stays the backstop for what the count
     misses, and for the wait it names.
   - **The refusal's shape rests on a measured report from another client** rather than on a
     request made here: what this machine has seen is the token throttle. That is why
     `requestsPerMinuteLimit` reads two signals — the sentence's `requests per minute (RPM)` and
     the envelope's `type: "requests"` — so a reworded sentence still teaches the number, and why
     a `type` that is not seen again costs nothing worse than the learning going quiet.

   A summarisation call is a request of its own and is not paced either — it carries no watcher,
   so a wait for it could not be shown (see §6.4) — which means a compaction that runs into a
   spent minute is still answered with the vendor's own 429 and waited out.
9. **The day's *token* allowance is invisible, and nothing is inferred from it.** The console's
   table gives each model four numbers — RPM, RPD, TPM and TPD — and the response headers carry
   only the middle two, so a daily token ceiling exists for every model and no response states
   how much of it is left. Reading `x-ratelimit-remaining-tokens` as if it described a day would
   be the worst kind of invented number: it would pace a turn against room that a spent day had
   already taken. A session that reaches that ceiling therefore arrives as one more
   `rate_limit_exceeded` with a wait in prose — the throttle shape, told apart by nothing — and
   the app answers it the way it answers a throttle, by obeying the wait. That wait is honest and
   futile at the same time: no wait inside a minute refills a day. The shape of that refusal has
   not been measured here, and until it is, a spent day is a throttle that keeps coming back.
