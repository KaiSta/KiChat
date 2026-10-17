---
name: add-llm-backend
description: Use when adding a chat backend or changing how an existing one lists models, streams, classifies refusals, or reports limits and capabilities — a hosted API such as Groq, NVIDIA, Mistral or DeepSeek, or a change to llama.cpp/Ollama/LM Studio.
---

# Adding a chat backend

Read the closest existing backend first. They are not identical, and which one is
closest tells you the shape you need:

- `GROQ.md` — a rich listing (window, modalities, features), a free-tier budget,
  two entries that cannot stream. Start here for a hosted OpenAI-compatible API.
- `NVIDIA.md` — a four-field listing that publishes no window, and an availability
  check that hides what the account cannot call.
- `MISTRAL.md` — an alias-collapsed catalogue and typed content chunks.
- `DEEPSEEK.md` — `reasoning_content` and its refusals.

## Where a backend lives

| File | Role |
| --- | --- |
| `internal/llm/openai.go` | the shared `openAICompatProvider` and the `flavour` enum; every hook a vendor overrides |
| `internal/llm/<vendor>.go` | the provider: constructor, listing mapper, error mapper, and a doc comment holding the measured shapes |
| `internal/llm/registry.go` | `Reconfigure`'s kind switch, and `DefaultBackendConfigs` |
| `frontend/src/components/Settings.tsx` | the per-kind rows (API key field, placeholder, extra controls) |
| `<NAME>.md` | the subsystem document — every vendor fact that is not in the code comment |

## Steps

1. **Measure before writing.** The provider's doc comment is where the vendor's
   shapes live, and every line must be something you observed or a documented
   claim marked as such. Two facts are always needed: what the listing returns
   (window, capabilities, models that cannot chat) and how usage arrives
   (`stream_options.include_usage`). Without usage, the context meter silently
   falls back to an estimate.

2. **Write the provider.** If the API is OpenAI-compatible, return
   `*openAICompatProvider` and add a `flavour<Name>` constant in `openai.go`.
   Override only what differs — chat path, listing shape, usage frame, refusal
   text, headers. A backend that re-implements streaming is a backend that will
   drift from the others.

3. **Register it.** Add the `case "<kind>"` to `Registry.Reconfigure` and an entry
   to `DefaultBackendConfigs`. Hosted entries ship **disabled**, for two reasons
   worth repeating in the entry's comment: they are metered and need a key, and a
   ready hosted model would win the loaded-model race and quietly become the
   default for new chats.

4. **Keys need no code.** `BackendAPIKeyName`, `config.Load`, `SetBackendAPIKey`
   and `internal/secret` are already generic. Verify the round trip anyway — that
   is what `backend_key_test.go` is for.

5. **The frontend names kinds explicitly.** `Settings.tsx` lists every hosted kind
   where the API-key input is rendered and again where "required" versus
   "optional" is decided. A kind missing from those lists gets no key field at
   all, and the backend looks broken rather than unconfigured.

6. **Tests.** `<vendor>_test.go` pins the listing mapper (window, capabilities,
   rows dropped on the vendor's own evidence), stream reassembly, refusal
   classification and a missing key. `live_<vendor>_test.go` is gated on
   `KICHAT_LIVE` plus its own variable, skips cleanly without one, and asserts
   only what the live service can settle — see the `live-tests` skill.

7. **Docs.** A new `<NAME>.md` following `GROQ.md`'s structure (what it adds,
   turning it on, the listing, the stream, refusals, where the code is, how it is
   verified, known gaps), a row in the README's provider table, a line in
   `AGENTS.md`'s doc index.

8. **Checks.** `make check`, then the live test with a real key.

## Rules that decide correctness

- **A capability is the vendor's claim or it is absent.** Groq publishes
  `supported_features` per model, so its claims are the vendor's; NVIDIA publishes
  nothing, so its provider claims tools for every model. Never infer a capability
  from an id.
- **Never invent a context window.** A backend that publishes none gets `0` and a
  model override, not a plausible number. KiChat shows the difference.
- **Classify a refusal by its envelope, not its status.** A rejected key and a
  throttle can share an HTTP code; the vendor's `code` or its own sentence is what
  decides. Groq's oversized-request refusal and its rate limit are the same
  `rate_limit_exceeded` envelope, told apart by their words.
- **Drop a model only on evidence.** Groq's Whisper and TTS rows are dropped
  because the API says they do not support chat completions — that is a reading of
  the vendor's answer, not a rule about ids.
- **A per-request ceiling, if the vendor states one, belongs in the budget path**
  (`internal/llm/limits.go`, `ceiling`), not in the provider's head.
- **Do not add a vendor fact to `AGENTS.md`.** It belongs in the code comment and
  in the subsystem document.

## Done when

- `make check` is green and `gofmt -l .` prints nothing.
- The provider's doc comment explains every shape the mapper depends on.
- The listing maps to `ModelInfo` with honest window and capabilities, and the
  stream reassembles fragmented tool calls.
- The live test passes against a real account, or says in its output why it
  skipped.
- `DefaultBackendConfigs` still ships the entry disabled, and Settings renders a
  key field for it.
