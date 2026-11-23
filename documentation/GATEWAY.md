# External AI API

KiChat can expose one selected backend/model through a loopback-only HTTP API.
An external application supplies its own instructions, history and function tools;
KiChat budgets that input, optionally summarises completed older turns, and calls
its existing model provider. Tools are returned to the client for execution.

## Connecting LOMAssistant

1. Start the model in KiChat's Backends settings (or start the external model server).
2. Open Settings → External API. Select that model, enable the local API and save.
3. Copy the base URL and load/copy the connection key.
4. In LOMAssistant select its custom OpenAI-compatible provider. Use the copied
   URL (normally `http://127.0.0.1:17841/v1`), the connection key as API key, and
   the model name shown in KiChat (default `kichat`). Clear a separately configured deep/companion model
   or set it to that same name too.
5. Keep KiChat and the upstream model running. Start with a simple question, then
   ask about the current choice to exercise the Mod's `player_state`/`script` tools.

The settings choose the actual backend/model. The public alias defaults to `kichat`
and can be edited separately. Save the setting and enter the same alias in each
client; it stays the same when the actual model changes. Other clients using this protocol can use it too;
a game still needs a Mod or integration that supplies its state and tools.

On a 16 GB Mac, the shipped 16,384-token budget and 2,048-token response reserve
are starting settings, not measured performance claims. A client asking for more
output reserves that larger amount. Ollama receives `num_ctx`; llama.cpp and MLX
must also be started with appropriate limits. The API budget does not enlarge the
model server's loaded window. A known catalogue limit, per-model override or managed
llama.cpp window can lower the configured budget.

## Protocol

- `GET /v1/models`: lists the single configured model alias (default `kichat`).
- `POST /v1/chat/completions`: text or text-part messages, system/user/assistant/tool
  roles, function tools and their results, optional streaming, temperature, top-p,
  seed and an output limit (`max_tokens` or `max_completion_tokens`).
- `tool_choice` supports `auto` and `none`. Forced tool selection, images, structured
  output and unknown request fields produce an error rather than being ignored.
- `reasoning_effort=none` requests the provider's thinking-off control; low/medium/high
  request thinking but do not map distinct effort levels. Provider support varies.
- Streaming is SSE with content/reasoning/tool-call deltas, a finish reason, optional
  reported usage, and `[DONE]`. An upstream failure after streaming starts is an
  error frame without a successful completion marker.
- Calls require `Authorization: Bearer <connection key>`. The service binds IPv4
  `127.0.0.1` only, starts disabled, does not enable browser CORS, and limits request
  bodies to 8 MiB. Requests have a five-minute deadline including queue time.

Each request is resolved against the current settings. Gateway requests share a
single generation slot; disconnecting cancels queued or running work, and stopping
KiChat closes the service and cancels its requests. KiChat's own chats retain their
existing scheduling and can run concurrently. The Gateway never executes client
tools, adds KiChat project tools, or changes game state itself.

## Compaction

The client remains the authority for history: no session is inferred from a persona,
IP address, model name or save file. Every request contains the history it wants to
use. KiChat never appends an absent previous conversation.

Budgeting counts messages, reasoning, tool arguments/definitions, a 25% estimation
margin, the larger of the configured reserve and requested output, and a 256-token
margin. These are estimates, not a tokenizer guarantee; the upstream can still
reject input. Reported answer-request usage is shown separately in settings.

When the input does not fit, leading system instructions and everything from the
latest user message onward remain verbatim. Earlier completed turns are summarised
with KiChat's existing summariser, on the same model with thinking disabled where
supported. The full source is processed in bounded batches; large messages are
split at rune boundaries rather than clipped. Summaries are lossy and can omit
facts. Current-turn evidence is deliberately not summarised: if that portion alone
is too large, or the summary does not free enough room, the API returns an error.
The first version consequently cannot fix an oversized single `script` result in
an active tool round; clients should page or narrow that query.

Up to sixteen summaries are cached in process memory. A key includes the exact
original prefix, system instructions, upstream configuration and window. This saves
repeated summary work during tool rounds. Edited histories, client-side summaries
and changed personas do not match. The API writes no conversation into KiChat's
chat database; cache entries disappear on process exit. The client and upstream
server may have their own logging/storage policies.

The connection key lives in `internal/secret`, is excluded from settings/status and
is included in diagnostic redaction. Status stores only the latest request's
estimates, timings, reported usage and failure. Summary-generation token usage is
not included in the answer-request usage figures.

## Verification and boundaries

Offline tests cover JSON/SSE responses, tool calls and results, schema/auth errors,
protected current evidence, changed-history cache invalidation, oversized current
turns, disconnect cancellation and stop cancellation. Root tests use isolated
secrets to check credential storage and settings. `make check` and `make build`
are the project gates. The mock UI harness's `?gateway=used` flag displays a request
with compaction; without it the empty state is rendered.

The user reported a successful LOMAssistant connection and a basic answer that
looked reasonable with Qwen3-4B-Q4_K_M. The failed first request had an estimated
8,334 input tokens against an 8,192-token loaded window; after restarting llama.cpp
with 16,384 tokens, `/props` and `/v1/models` both confirmed that loaded window.
Full gameplay, route advice, compaction quality and sustained latency remain
unverified. Offline protocol tests use scripted providers. Cross-game memory, semantic game-state interpretation,
LAN access and screenshots are outside this first protocol implementation.

## Code

- `internal/gateway`: request validation, HTTP/SSE server, budgeting and summary cache.
- `internal/chat/compact.go`: the shared summariser and its request-size estimate.
- `api_gateway.go`: settings/provider resolution, key and server lifecycle bindings.
- `internal/config/gateway.go`: defaults and range normalisation.
- `frontend/src/components/settings/GatewayTab.tsx`: setup and latest-request status.
