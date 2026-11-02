# MLX

Everything KiChat knows about running a model through Apple's MLX: the provider
that talks to `mlx_lm.server`, the environment it installs, and the one thing
about the server that is easy to promise and wrong to promise — that `running`
means the weights are loaded. It is a sibling of [`LLAMACPP.md`](LLAMACPP.md),
which is the other program this app installs and supervises itself.

Everything below was read out of `mlx_lm/server.py` and `mlx_lm/SERVER.md` on
main (2026-11) rather than guessed. Claims that are design intent say so, and the
things that have not been measured on a Mac yet are listed in §6.

**Contents:** 1 two shapes of the same server · 2 the provider · 3 the managed
environment · 4 the settings card · 5 where the code is · 6 how this is verified ·
7 known gaps.

---

## 1. Two shapes of the same server

`mlx` names two things, exactly as `llamacpp` does:

- **A server you point KiChat at.** `mlx_lm.server` speaks OpenAI's chat API, so
  the generic `custom` kind could reach it. It could not tell the truth about it:
  a listing that carries no window and no capabilities is not the same as one
  that carries them and has them ignored. The provider exists to map that shape
  honestly (§2).
- **A server KiChat installs and runs.** There is no MLX binary to download:
  Apple ships the server inside the `mlx-lm` Python package. KiChat creates a
  virtual environment under its own data directory, installs the package there
  and starts the server with a model folder the user picked (§3).

The two meet at the address. A managed start chooses a free port, and
`onManagedState` writes the URL it landed on into the `mlx` backend entry and
rebuilds the providers — the same mechanism llama.cpp uses.

MLX runs on **Apple silicon only**. The card is hidden elsewhere, the start is
refused with its own sentence, and the settings blob keeps the entry so a
configuration made on a Mac survives a save from another machine.

## 2. The provider

`internal/llm/mlx.go` returns the shared `openAICompatProvider`; only the
constructor differs. The shapes it depends on:

| Shape | What it is | What KiChat does with it |
| --- | --- | --- |
| `GET /v1/models` | `{id, object, created}` per entry; `id` is a Hugging Face repo id, or the path the server was started with | maps `id` and marks the row not-loaded. **No window, no capabilities** |
| `POST /v1/chat/completions` | OpenAI chat body; `stream`, `max_tokens`, `tools` accepted | the shared request builder, including `stream_options.include_usage` |
| stream delta | `content`, `reasoning`, `tool_calls` with `index` | `reasoning` is the field mlx-lm uses, not `reasoning_content`; the shared delta reader already accepts both |
| final chunk | usage, including `prompt_tokens_details.cached_tokens` | fills `Usage.CachedPromptTokens` |
| `GET /health` | 200 once the generation thread is alive, 503 otherwise | readiness, **not** a load indicator (§3.3) |
| a load failure | HTTP 404 with the Python exception's text | classified by body, never as "route missing" |

Because the listing publishes no window, `ContextLimit` is **0** and the app
falls back to its conservative default and the user's per-model override. That is
the same rule llama.cpp's `n_ctx_train` case is written around: an invented
window truncates a prompt from the front, losing the system prompt.

Tools are template-driven: the server only parses them when the tokenizer
reports a tool parser. Claiming fewer capabilities than a model has is the
cheaper mistake here — a model whose template does not parse tools answers in
prose, which is a visible failure rather than a silent truncation.

There is no `timings` object, so the speed badge is the app's estimate, not a
measurement by the server.

## 3. The managed environment

### 3.1 What is installed

- `python3 -m venv <data dir>/runtime/mlx` — a private environment, no system
  package touched, no administrator password.
- `pip install --upgrade pip`, then `pip install --upgrade mlx-lm`.
- The version that landed is read back with
  `python -c "import mlx_lm; print(mlx_lm.__version__)"` and stored, because pip
  resolves what pip resolves and a recorded pin is only a claim.

The package is deliberately not pinned: its wheels are built per Python and
macOS version, and a pin is the build that stops installing the day the machine's
Python moves. The trade is reproducibility for installability, and the version
line in the card is what makes the difference visible.

The interpreter is looked for among `python3`, `python3.13`, `python3.12`,
`python3.11`, `python`. One older than 3.11 is passed over rather than tried: pip
would spend a minute resolving wheels and then fail with an error whose cause is
the interpreter.

### 3.2 What is started

```
<venv>/bin/python -m mlx_lm.server \
  --model <snapshot directory> --host 127.0.0.1 --port <free port> \
  [--max-tokens N] [--kv-bits N] [--prefill-step-size N] \
  [--draft-model PATH] [--chat-template NAME] [--trust-remote-code] [user args…]
```

The supervisor is the same `internal/runtime.Server` that runs llama-server and
sd-server. It already had `Args`, `ReadyPath` and `Label`; MLX added one field,
`ModelDir`, because `--model` takes a folder and the supervisor's default check
rejects one.

The model is a **snapshot directory**: config.json and the weights. A Hugging
Face repo id is not accepted for a managed start, because the server would
download it on the first request — a multi-gigabyte side effect of pressing
Start. Downloading is a decision, not a side effect, and the folder picker is
the way in.

### 3.3 `/health` is not a load

This is the one behaviour that differs from llama.cpp in a way a user notices.
`mlx_lm.server` answers `/health` 200 as soon as its generation thread is alive,
**before any weights exist**; the model is loaded by the first request that names
it. So:

- a managed start reports `running` once the process answers;
- the first message pays the load, which can be minutes on a large model;
- the card says so under its buttons, rather than letting a slow first reply look
  like a fault.

The alternative — a warm-up completion inside the start — was considered and left
out: it would turn Start into a several-minute blocking action and load a model
before the user has asked anything. It is the obvious next improvement if the
wait turns out to be a complaint.

### 3.4 Quantised KV cache

`--kv-bits` accepts 2, 3, 4, 6 and 8. Anything else is repaired to 0 by
`config.NormaliseMLX` — an unquantised cache is the server default and the only
mode that batches. The server's own documentation warns that attention on a
quantised cache is not fused and keeps a score matrix of
`prefill_step_size × context_length`, which is why `--prefill-step-size` sits
beside it in the card instead of being buried in extra arguments.

## 4. The settings card

Settings → Backends gets one more card, not a separate runtime page: the screen
maps over the backend entries and this is one. The shell — enable, name, address,
state badge, per-backend digest — is shared. The managed section is its own
component (`MLXModelSection`) rather than a branch in `LocalModelSection`,
because there is no GGUF to scan, no projector, no flash attention and no
`--gpu-layers`.

On a machine that is not `darwin/arm64` the card is not rendered. The entry stays
in the blob and is never started. The card's "start this model with KiChat"
switch is the one `startEnabledModels` reads at launch, so an MLX model is loaded
on every app start only when it was asked to be.

Starting MLX is a Settings action for now. The chat's local-model picker still
lists GGUF files, which is the shape llama.cpp takes and not the shape MLX does;
offering an MLX entry there would propose files the start rejects (§7).

## 5. Where the code is

| File | Role |
| --- | --- |
| `internal/llm/mlx.go` | provider constructor, `ManagedKind` |
| `internal/llm/registry.go` | the `mlx` case and its disabled default entry |
| `internal/runtime/mlx.go` | argument list, environment, version, install |
| `internal/runtime/supervise.go` | `Options.ModelDir` |
| `internal/config/config.go` | `ManagedModel.MLX`, `MLXOptions`, `MLXRuntime`, repair, clone |
| `app.go` | `startManagedMLX`, `mlxLauncher` |
| `api_runtime.go` | status fields, `InstallMLXRuntime`, `PickMLXModelFolder` |
| `frontend/src/components/settings/BackendsTab.tsx` | `MLXModelSection` and the platform gate |
| `frontend/src/components/ModelSwitcher.tsx` | the local-start section is still llama.cpp-only (§7) |

## 6. How this is verified

- `internal/llm/mlx_test.go` pins the listing (no invented window or
  capability), a stream carrying `reasoning`, a split tool call and cached usage,
  and a 404 load failure whose sentence survives.
- `internal/runtime/mlx_test.go` pins the command line (only chosen flags, no
  unaccepted `--kv-bits`), the platform predicate and the interpreter lookup.
- `internal/config/clone_test.go` proves the `MLX` pointer is copied, not shared.
- `make check` covers gofmt, vet on linux/windows/darwin and both arm64 halves,
  the Go tests and `tsc`.

**Not yet measured on real hardware.** These are the first things to check on an
M-series Mac, and what each would change:

1. that `pip install mlx-lm` succeeds behind the user's proxy and that the
   recorded version matches `import mlx_lm`;
2. how long the first request takes for a 4-bit 8B model, and whether the card's
   note is enough or a warm-up is needed;
3. that a folder holding `config.json` and safetensors is accepted as `--model`
   exactly as it is;
4. what `--kv-bits` does to memory and tokens/second on one model, so the hint is
   advice rather than a quote from the server's documentation;
5. the error text of a failed load, so the classification in §2 rests on a
   measurement rather than on the source.

A live test (`live_mlx_test.go`) should be gated on `KICHAT_LIVE`, `darwin/arm64`
and a runnable `mlx_lm.server`, and skip with its reason on every other machine.

## 7. Known gaps

- **No warm-up**, so the first message of a session carries the load (§3.3).
- **Starting is Settings-only.** The chat's local-model section scans for GGUFs;
  it needs a folder picker and an MLX branch before it can offer this kind.
- **No model download.** Selecting a repo id is not offered; the user points at
  an existing snapshot. Adding it would need a download row with progress and a
  cancel, which the app has for archives but not for Hugging Face snapshots.
- **No removal.** The environment has no counterpart to the llama.cpp cleanup
  list yet; deleting `runtime/mlx` by hand is a complete uninstall.
- **No update check.** The version is read from the package, but nothing asks
  PyPI whether a newer one exists.
- **One resident model.** The server loads a model per request and drops the
  previous one; the app pins the request to the model that was started, so a
  chat cannot make the server swap underneath it.
