---
name: live-tests
description: Use when running or adding tests against real services (Brave, LM Studio, DeepSeek, Mistral, NVIDIA, Groq, a real llama.cpp model, stable-diffusion.cpp) or when deciding whether a change needs a live check.
---

# Live tests

Offline tests never touch the network. Live ones are gated by environment
variables and skip themselves when the variable is absent — **a skipped live test
is not a pass**, and `make check` is green without any of them.

```sh
# Brave Search: one credit per request — ~4 requests for the tests below
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/research/ -run TestLive -v

# LM Studio: needs a *loaded* chat model; skips itself if none is
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/chat/ -run TestLive -v
KICHAT_LIVE=1 go test ./internal/chat/ -run TestLiveEngineTurn -v   # needs KICHAT_LMSTUDIO

# DeepSeek: metered, needs a key; skips without KICHAT_DEEPSEEK_KEY
KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/ -run TestLiveDeepSeek -v
#   optional: KICHAT_DEEPSEEK_BASE, KICHAT_DEEPSEEK_MODEL

# Mistral: metered, needs a key; skips without KICHAT_MISTRAL_KEY
KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key> go test ./internal/llm/ -run TestLiveMistral -v
#   optional: KICHAT_MISTRAL_BASE, KICHAT_MISTRAL_MODEL,
#   KICHAT_MISTRAL_REASONING_MODEL (the thinking-chunk shape needs one the
#   account may call; a tier refusal is reported, not failed)
#   the unanchored pattern also picks up the one-request temperature probe,
#   TestLiveMistralTemperatureAboveThePublishedMaximum

# NVIDIA: metered, needs a key; skips without KICHAT_NVIDIA_KEY
KICHAT_LIVE=1 KICHAT_NVIDIA_KEY=<key> go test ./internal/llm/ -run TestLiveNvidia -v
#   optional: KICHAT_NVIDIA_BASE, KICHAT_NVIDIA_MODEL. The catalogue lists
#   models most keys cannot call, so this drives one model rather than walking
#   the listing; TestLiveNvidia is the one that asserts a window of 0.

# Groq: metered, needs a key; skips without KICHAT_GROQ_KEY
KICHAT_LIVE=1 KICHAT_GROQ_KEY=<key> go test ./internal/llm/ -run TestLiveGroq -v
#   optional: KICHAT_GROQ_BASE, KICHAT_GROQ_MODEL (defaults to
#   `openai/gpt-oss-20b`, which publishes tools and reasoning) and
#   KICHAT_GROQ_VISION_MODEL, which adds the image check. The account's
#   token-per-minute allowance is small and one 128x128 PNG costs ~1300 prompt
#   tokens, so the vision check is opt-in.
#   the unanchored pattern also picks up TestLiveGroqShippedEntryReachesTheAPI,
#   which drives the entry DefaultBackendConfigs ships through NewRegistry.

# The projector, both rows at once: a real multimodal model with a projector
# beside it, started through the app's own supervisor — once pairing that file,
# once from an empty folder holding a symlink to the same weights. Two model
# loads, seconds each.
KICHAT_LIVE=1 KICHAT_VISION_MODEL=~/…/Qwen3-VL-8B-Instruct-Q4_K_M.gguf \
  go test ./ -run TestLiveProjectorIsWhatMakesAVisionModelSee -v
#   optional: KICHAT_VISION_TAG pins the installed build to start,
#   KICHAT_RUNTIME_DIR the folder the builds are installed in

# The Real-ESRGAN download: 45–52 MB from GitHub, then a real unpack and
# verification. It is what keeps the pinned digests honest, since upstream
# publishes none. No key, no GPU needed — nothing is executed.
KICHAT_LIVE=1 go test ./ -run TestLiveUpscalerInstall -v

# PrismML's fork: one release lookup, a 17 MB download and a real model load
KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v
#   optional: KICHAT_PRISM_MODEL=<path.gguf>, otherwise the smallest model in
#   the LM Studio folders; skips if there is none. Loads a real model: minutes.

# Stable Diffusion WebUI: draws one 64x64 picture at one step. Local and
# unauthenticated, so there is no key — but it needs a WebUI actually running,
# and it skips when there is none (the normal state).
KICHAT_LIVE=1 go test ./internal/images/ -run TestLiveSDWebUI -v
#   optional: KICHAT_SD_URL (default 127.0.0.1:7860), KICHAT_SD_AUTH=user:pass

# A real sd-server: gigabytes and minutes — the folder is required, no default.
KICHAT_LIVE=1 KICHAT_SD_FOLDER=~/Downloads/sd go test ./internal/images/ -run TestLiveSDServer -v
#   optional: KICHAT_SD_DIFFUSION_MODEL, KICHAT_SD_MODEL, KICHAT_SD_VAE, KICHAT_SD_LLM,
#   KICHAT_SD_CLIP_L, KICHAT_SD_CLIP_G, KICHAT_SD_T5XXL — the model set to load

# A real FLUX 3 Klein draw: the whole set is required, no default, because the profile is
# read from the diffusion file itself. Three draws, ~30 s: one 512² at the profile's own
# 16 steps / CFG 1.0, then the same seed twice, with and without a negative prompt.
KICHAT_LIVE=1 KICHAT_SD_CLI=~/sdMaster/buildVulkan/sd-cli \
  KICHAT_SD_DIFFUSION_MODEL=~/sdMaster/models/flux3/flux-3-image-klein-UD-Q4_K_XL.gguf \
  KICHAT_SD_LLM=~/sdMaster/models/flux3/Qwen3VL-4B-Instruct-Q4_K_M.gguf \
  KICHAT_SD_VAE=~/sdMaster/models/flux2/flux2-vae.safetensors \
  go test ./internal/images/ -run TestLiveFlux3KleinProfile -v

# Real-ESRGAN: the portable program, its weights, and the Vulkan driver behind them.
# The path is required, no default: it is only ever installed by hand today. Two runs of
# a 64x64 source, seconds each — a probe and a real 4x — and the log line says how long.
KICHAT_LIVE=1 KICHAT_ESRGAN=~/Downloads/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan \
  go test ./internal/images/ -run TestLiveESRGAN -v
```

## What each one buys

| Test | Settles |
| --- | --- |
| `TestLiveBrave*`, `TestLiveSearchFirstTurn` | the real page-chunk route, local clamping beating the API's 422, the chain choosing that route, a rejected key classified as a key, and the whole path into LM Studio |
| `TestLiveDeepSeek` | the listing's real window, `include_usage` honoured, fragmented tool calls reassembled |
| `TestLiveMistral` | the listing is not a chat catalogue, the alias collapse, capabilities from the listing, usage, tool calls |
| `TestLiveNvidia` | a four-field catalogue, a window of **0**, usage, a real tool call |
| `TestLiveGroq` | window/modalities/features from the vendor's own listing, entries that cannot chat dropped, usage, a tool call, a fake key refused as a key, the per-request ceiling and the rate-limit remainder readable, and the two streaming-incompatible rows failing with the vendor's own 400 |
| `TestLiveGroqOversizedRequestIsRecognised` | an oversized request is a refusal with its limit read out of the vendor's sentence, not a throttle — one request, and whether a refused request is charged is undocumented, so treat it as a minute's allowance |
| `TestLiveProjectorIsWhatMakesAVisionModelSee` | `--mmproj` in argv, a vision modality in `/props`, an image turn answering 200 where the same weights without it are refused, and the app listing `vision` |
| `TestLivePrismForkRunsAModel` | the fork's release list, tag scheme, archive, ternary kernels and a real GGUF loaded |
| `TestLiveSDWebUI` | the sampler the client sends is one the server knows, the guidance scale echoed back, and real PNG bytes |
| `TestLiveSDServer` | `sd-server` really ships beside `sd-cli`, the listener flag exists, the readiness route answers, `cfg_scale` parses, and the time to ready plus two draws is printed |
| `TestLiveFlux3KleinProfile` | the profile is identified from the checkpoint's own tensors, `Health` names a missing VAE before a multi-gigabyte load, 16 steps at CFG 1.0 really draw, and the negative prompt the profile drops really is inert |
| `TestLiveESRGAN` | the released Vulkan build starts on this machine, its weights load, and a real 4× comes back — the three things a stand-in program cannot say, and the one archive trap (a build with no `models/` that exits 0 writing a black picture) reported as `ErrUpscalerNoModels` |

## Rules

- **Never put an API key in a tracked file.** Pass it in the environment; the app
  stores it through `internal/secret`.
- **A live test must skip, not fail, without its variable** — and must say why.
- **Do not weaken an assertion to make a metered test pass.** A tier refusal is
  reported, not failed, where the account's plan is the variable.
- **A live test never reaches the real keyring or `~/.config/kichat`.** Tests use
  `secret.NewIsolated(dir)`.
- **Measure, then write down what you measured** — in the subsystem document, not
  in `AGENTS.md`.
