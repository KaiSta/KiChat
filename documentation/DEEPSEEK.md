# DEEPSEEK.md

How the DeepSeek backend works in KiChat: the shapes the service answers with, the
decisions that follow from them, and what is verified against the live API.
`PLAN.md` names the backend in one line; this document is the detail behind it. It is a
sibling of `MISTRAL.md`, which does the same for the other hosted service.

Everything here was read out of the code and, where marked *measured*, observed against
the live API. Where a claim comes from the vendor's documentation rather than from a
request, it says so.

**Contents:** 1 what this adds · 2 turning it on · 3 the listing · 4 the stream ·
5 the two chat paths · 6 refusals · 7 where the code is · 8 how this is verified ·
9 known gaps.

The code references are `file:line` for the revision this was written against. The symbol
names beside them (`newDeepSeekProvider`, `fromDeepSeekEntry`, `flavourDeepSeek`, …) are
the durable anchor when the numbers drift.

---

## 1. What this adds

DeepSeek is a hosted, OpenAI-compatible service, so the chat path is the shared one —
streaming, tools, `max_tokens`, `stream_options.include_usage` all go through
`openAICompatProvider`. Three things are its own, and each is a fact the vendor's
documentation or its live listing settles:

| what | why it could not be shared |
| --- | --- |
| The model listing | It reports a context window, where the plain OpenAI listing reports none |
| The reasoning field | Thinking arrives in `reasoning_content`, a field of the delta |
| The base URL | `/v1` is an *alias* of the same API rather than a versioned path, so both routes are tried |

It was the first hosted backend, and it set the pattern the others follow: a provider of
its own in `internal/llm/deepseek.go`, a `flavourDeepSeek` branch in the shared listing
mapper, a shipped-but-disabled entry in `DefaultBackendConfigs`, and `ErrMissingAPIKey`
reported instead of attempting a call with no credential.

---

## 2. Turning it on

DeepSeek ships **disabled**, and that is a decision rather than a mechanic. It is a metered
remote service that needs a key, so enabling it by default would have two consequences the
app does not want:

- every health refresh would probe a paid API without being asked, and
- because a hosted model is ready the moment a key is accepted, its models would win the
  loaded-model race and quietly become the default for new chats.

A user who wants it enables it and pastes a key in **Settings → Backends**. The key goes to
the secret store through `llm.BackendAPIKeyName("deepseek")`
(`internal/secret`, `~/.config/kichat/`) and never into the settings blob, which is
asserted by `internal/config/config_test.go` (it sets the key in an isolated secret store,
saves the settings, and reads the key back out).

There is no migration. `normalise()`
([internal/config/config.go:491](../internal/config/config.go#L491)) appends any shipped
backend that is missing and repairs an empty address, which is how the entry reaches an
installation that predates it.

---

## 3. The listing

`GET /models` answers with a `data` array that carries more than the lowest-common-
denominator OpenAI listing does. One entry, trimmed:

```json
{"id":"deepseek-flash","object":"model","owned_by":"deepseek","name":"DeepSeek-V4.1-Flash",
 "context_window":1048576,"max_output_tokens":393216,
 "input_modalities":["text","image"],"output_modalities":["text"],
 "effort":{"supported_levels":["low","high","max"],"default_level":"high"}}
```

`fromDeepSeekEntry` ([internal/llm/deepseek.go:75](../internal/llm/deepseek.go#L75)) maps it:

| field | becomes | note |
| --- | --- | --- |
| `context_window` | `ContextLimit` **and** `MaxContext` | the reason this provider has a listing of its own |
| `input_modalities` containing `image` | `Type: "vlm"` + `CapVision` | the attachment path depends on it |
| `effort` present | `CapThink` | its presence is the fact; the level is not read |
| — | `CapTools` always | see below |
| — | `State: loaded` | a hosted model has no load step |

**The tool claim is a deliberate guess, and it is the one thing here the listing cannot
settle.** Unlike Mistral's, this listing says nothing about function calling, and the app
uses the native tool protocol only when a model claims it — a model left without the claim
would lose tools entirely rather than fall back to the text protocol quietly. So
`CapTools` is appended unconditionally, and the live test is what makes that honest: it
checks that a tool call really does stream back. Verified on both shipped models, which
stream fragmented `tool_calls`.

`State` is `loaded` for every model, for the same reason as Mistral: reporting "not
loaded" the way a local model waiting for VRAM is reported would grey the row out of the
picker and keep it out of the default-model choice for no reason.

An entry with a field that cannot be decoded is **skipped, not fatal** — one malformed
model must not hide the others, the same way one dead backend does not hide a live one.
`TestDeepSeekListingSkipsAnEntryItCannotRead` pins it with a `context_window` sent as a
string.

---

## 4. The stream

Reasoning models here reason **by default**, and the thinking arrives in a field of the
delta called `reasoning_content` — unlike Mistral, whose reasoning rides inside a list of
typed chunks ([MISTRAL.md](MISTRAL.md) §4). The shared delta parser accepts three spellings
of reasoning (`reasoning`, `reasoning_content`, `thinking`); this vendor answers with the
second, and `TestDeepSeekStreamParsesReasoningContent` pins that spelling so a change to
the parser cannot quietly empty the reasoning panel while the answer still arrives.

Two consequences of reasoning-by-default, both measured:

- A reasoning model spends its budget on `reasoning_content` **before** any answer token,
  so a small `max_tokens` returns `finish_reason: "length"` with an **empty `content`**.
  That is not a bug in the request; it is what the service does. The live test asks for
  2048 tokens for the plain turn and 4096 for the tool call for exactly this reason.
- `stream_options.include_usage` is honoured, and the final frame carries
  `prompt_tokens`/`completion_tokens`/`total_tokens`. Without it the context meter could
  only ever show an estimate, which is why the shared request asks for it explicitly.

`usage` is read from **any** frame, so it does not matter whether a vendor attaches it to
the finish frame or sends a usage-only frame after it.

---

## 5. The two chat paths

`DeepSeekDefaultBaseURL` is `https://api.deepseek.com`, and `newDeepSeekProvider`
([internal/llm/deepseek.go:32](../internal/llm/deepseek.go#L32)) configures **both** routes:

```
{base}/chat/completions        the documented path
{base}/v1/chat/completions     the OpenAI-SDK-shaped alias
```

`/v1` is an alias of the same API rather than a versioned path, so both answer today and
the second is configured as the fallback: a 404 on the first retires it permanently, so
the cost is one request in the lifetime of the process rather than one per turn.
`TestDeepSeekFallsBackToTheV1Path` serves only the second route and asserts the reply
still arrives.

The listing path has no such fallback to make — `modelsPath` and `openAIModelsPath` are the
same address, which is worth knowing: a 404 there retries the identical URL and, if it ever
answered, would go through the flavour-blind `listOpenAIModels` mapper. Unreachable as
configured, but it is the same shape MISTRAL.md §11 records for Mistral.

---

### Prompt caching, and the one layout rule it forces

Context caching is automatic here and needs nothing in the request: the vendor's guide
([api-docs.deepseek.com/guides/kv_cache](https://api-docs.deepseek.com/guides/kv_cache/)) says
the cache is matched **from the start of the request** in 64-token blocks, and that a hit is
billed far below a miss. Neither the feature nor the price is something KiChat controls; what it
controls is whether a prefix ever matches.

Two consequences, and the second is why a coding reply is laid out the way it is:

- **The hit count is read, stored and shown.** The stream fills `Usage.CachedPromptTokens` from
  `prompt_cache_hit_tokens` — the top level of `usage`, not the `prompt_tokens_details
  .cached_tokens` an OpenAI-shaped server sends — and the engine writes it onto the reply's row
  (`messages.cache_hit_tokens`, migration 15). The reply's **Details** shows it as *Cache hits*
  with its share of the prompt, so a round that re-used everything it could is distinguishable
  from one that did not. The miss count is not read: `prompt_tokens − hits` says the same thing,
  and two numbers that must agree are two numbers that can disagree. Zero means "nobody reported
  one", which is why the row is absent rather than zero on a backend that publishes nothing.
- **A block that changes between the rounds of one reply must not sit in front of the history it
  would invalidate.** A changed prefix cannot match at the point of change or anywhere after it,
  so a project's git block rides in the tail of the request rather than in the system message
  (`Assemble` in `internal/chat/context.go`, and [CODING.md](CODING.md) §8 for what the block
  says): the assistant edits a file on nearly every round, and that edit used to rewrite message
  zero — a full miss for the whole history behind it, every round. Living in the tail is not a
  licence to be large there either — the tail is never a hit — which is why the full list of
  pending paths travels only with the request that opens the turn and the rounds after it carry
  a 132-token summary instead.
  `TestAProjectsGitStateStaysOutOfTheCachedPrefix` pins the property, that every message but the
  last is byte-identical across an edit.

What the numbers still do not add up to is **money**: KiChat carries no price table, so a reply's
Details says how many of its input tokens came from the cache and not what the round cost. A
vendor that changes its prices would otherwise silently change what the app displays.

The clock is the second block that rides there, and it is the one that mattered most. The
shipped presets all end in `Aktuelles Datum / Zeit: {{datetime}}`, and `{{datetime}}` used to
render to the minute — so a reply whose rounds crossed a minute boundary re-priced its entire
history on every round after it, which on a 1M-token model is the cost of a whole coding session
without any work to show for it. `{{datetime}}` now renders the day and no clock
(`RenderPrompt` in `internal/chat/prompt.go`); the exact time travels in the tail as
`<turn_clock>2026-11-01 14:32 (CET)</turn_clock>`, beside the working tree, so message zero is
byte-identical for as long as the reply lasts. `TestTheClockStaysOutOfTheRenderedPrompt` renders
the template ninety-five minutes apart and fails if the two differ;
`TestTheClockRidesInTheTailToo` asserts the placement on a real assembly.

What this still cannot protect is the parts of the prompt that are the user's own text:
`{{context_used}}` and `{{time}}` change between requests by definition, so a template that asks
for either opts out of prefix caching for the whole conversation. The shipped presets use
neither, and both are left alone.

## 6. Refusals

An empty key is refused **before any request is made**. `requiresKey` is true, so
`keyCheck` returns `ErrMissingAPIKey` and the user is told what is missing instead of being
handed the vendor's 401 body, which is long and never mentions where a key belongs.
`Health` wraps it as `ErrUnavailable: ErrMissingAPIKey`, so the settings badge can say both
that the backend is unusable and why. `TestDeepSeekWithoutAKeyIsNotAttempted` fails the
test if the server is reached at all.

---

## 7. Where the code is

| file | symbol | what it does |
| --- | --- | --- |
| [internal/llm/deepseek.go:22](../internal/llm/deepseek.go#L22) | `DeepSeekDefaultBaseURL` | `https://api.deepseek.com` |
| [internal/llm/deepseek.go:28](../internal/llm/deepseek.go#L28) | `ErrMissingAPIKey` | the deliberate refusal, shared with Mistral |
| [internal/llm/deepseek.go:32](../internal/llm/deepseek.go#L32) | `newDeepSeekProvider` | both chat paths, the listing, `flavourDeepSeek`, `requiresKey` |
| [internal/llm/deepseek.go:62](../internal/llm/deepseek.go#L62) | `deepSeekModelEntry` | one listing entry, with the window and the modalities |
| [internal/llm/deepseek.go:75](../internal/llm/deepseek.go#L75) | `fromDeepSeekEntry` | entry → `ModelInfo`, including the unconditional tool claim |
| [internal/llm/openai.go:103](../internal/llm/openai.go#L103) | `flavourDeepSeek` | selects the mapper in `mapModelEntries` |
| [internal/llm/registry.go:106](../internal/llm/registry.go#L106) | `Reconfigure` | `case "deepseek"` builds the provider |
| [internal/llm/registry.go:375](../internal/llm/registry.go#L375) | `DefaultBackendConfigs` | the shipped row: `deepseek`, disabled |
| [internal/config/config.go:491](../internal/config/config.go#L491) | `normalise` | repairs an empty address for an existing install |

The frontend needs nothing of its own: the API-key field and the "required" placeholder are
driven by `backend.kind === 'deepseek'` in
[frontend/src/components/Settings.tsx:1371](../frontend/src/components/settings/BackendsTab.tsx), the
same branch Mistral joined.

---

## 8. How this is verified

### Offline — `internal/llm/deepseek_test.go`

| test | what it pins |
| --- | --- |
| `TestDeepSeekListingCarriesWhatThePlanningNeeds` | the window on `ContextLimit`/`MaxContext`, `vlm` from an image modality, `CapTools`, `CapThink` from `effort` absence/presence, `State: loaded` |
| `TestDeepSeekListingSkipsAnEntryItCannotRead` | a malformed entry is dropped, its neighbour survives |
| `TestDeepSeekStreamParsesReasoningContent` | the field name, the content that follows it, usage including the cache-hit split, the `Bearer` header, the chat path |
| `TestDeepSeekFallsBackToTheV1Path` | the second route still answers after a 404 on the first |
| `TestDeepSeekWithoutAKeyIsNotAttempted` | `ErrMissingAPIKey` from `ListModels`, `Health` and `Chat` |
| `TestRegistryBuildsDeepSeekFromConfig` | the kind selects the provider, the default address, `requiresKey`, the flavour, and that a disabled entry builds nothing |
| `TestShippedDeepSeekBackendIsOffByDefault` | the shipped row is off and carries the vendor address |

### Live — `internal/llm/live_deepseek_test.go`

Gated on `KICHAT_LIVE=1` **and** `KICHAT_DEEPSEEK_KEY`; nothing runs by accident.

```
KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/ -run TestLiveDeepSeek -v
#   optional: KICHAT_DEEPSEEK_BASE, KICHAT_DEEPSEEK_MODEL
```

It checks the three claims only the real service can settle, each of which the offline
tests can only fake:

1. the listing reports a real context window — LM Studio's namespace does, the plain
   OpenAI one cannot;
2. `stream_options.include_usage` is honoured, so the context meter gets a measured
   `prompt_tokens`;
3. a tool call streams, which is what the unconditional `CapTools` claim rests on.

It logs the chosen model's context window, type, capabilities and state, and asserts a
tool call arrives with `get_weather` as its name and the city in its arguments.

### The gate

`make check` (gofmt, `go vet ./...`, every package's tests, `tsc --noEmit`) and
`make build` → `build/bin/kichat`.

---

## 9. Known gaps

1. **`CapTools` is claimed for every model, on the strength of two.** The listing cannot
   settle it, so a future model that cannot call tools would be offered the native
   protocol and answer in prose instead of calling anything. The live test checks the
   models the account can reach, not the whole catalogue.
2. **`max_output_tokens` is read by nobody.** The request sends its own `max_tokens`, so
   the app can ask for more than a model will produce; the vendor then stops at its own
   ceiling.
3. **The error envelope is not classified.** Unlike Brave's, a DeepSeek refusal reaches
   the user as the vendor's body, so a used-up balance and a momentary throttle are not
   told apart (MISTRAL.md §11 records the same gap for Mistral, which answers 429 with
   `code: "1300"`).
