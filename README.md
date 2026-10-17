<p align="center">
  <img src="build/appicon.png" width="96" alt="KiChat app icon: a blue speech bubble with a spark">
</p>

# KiChat

**A local-first AI workspace for chat, research, image generation and code — run models on your own machine, or make the most of the subscription you already pay for.**

KiChat is a desktop app for **Linux, macOS and Windows** that puts your whole AI
toolkit in one window. It runs open-weight models (GGUF) on your own hardware through
**LM Studio**, **Ollama**, or a **llama.cpp** server it downloads, starts and stops for
you — and it reaches the hosted providers whose free tier or subscription you already
have: **Groq**, **NVIDIA build.nvidia.com**, **Mistral** and **DeepSeek**. Chats,
sources, documents, generated pictures and code stay in one searchable, fully offline
history on your own disk.

No KiChat account, no service of its own, no telemetry. Nothing leaves the machine unless
you picked a hosted model or asked for a web search.

<p>
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white">
  <img alt="Wails v2" src="https://img.shields.io/badge/Wails-v2-E83524">
  <img alt="React 19" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white">
  <img alt="Linux, macOS and Windows" src="https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20Windows-4c8dff">
  <img alt="Interface in English, German and Chinese" src="https://img.shields.io/badge/UI-English%20%C2%B7%20Deutsch%20%C2%B7%20%E4%B8%AD%E6%96%87-2f5fb0">
  <img alt="Licensed under the Apache License 2.0" src="https://img.shields.io/badge/licence-Apache%202.0-4c8dff">
</p>

## Contents

- [Why KiChat](#why-kichat)
- [What it does](#what-it-does)
  - [Chat](#chat) · [Research](#research) · [Images](#images) · [Code](#code) · [Documents and vision](#documents-and-vision)
- [Backends and providers](#backends-and-providers)
- [Making the most of a free tier](#making-the-most-of-a-free-tier)
- [Which local model fits your machine](#which-local-model-fits-your-machine)
- [Install](#install)
- [First run](#first-run)
- [GPU acceleration: CUDA, ROCm, Vulkan](#gpu-acceleration-cuda-rocm-vulkan)
- [Ternary models and vision](#ternary-models-and-vision)
- [Privacy and where data lives](#privacy-and-where-data-lives)
- [Build from source](#build-from-source)
- [Development](#development)
- [Tests and checks](#tests-and-checks)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Roadmap](#roadmap)
- [Documentation](#documentation)
- [Versioning](#versioning)
- [License](#license)

## Why KiChat

- **Local first, not local only.** The same chat window drives a 4B model on a laptop
  and a frontier model over an API, and switching between them is a dropdown, not a
  second app.
- **Free tiers treated as a budget, not a trap.** KiChat reads the per-request token
  ceiling a provider enforces — Groq's free tier caps a single request at 8,000
  tokens, well under the model's window — folds the conversation to fit *before* it is
  refused, and spaces a long tool-using turn's rounds across the minute instead of
  hammering the rate limit. See [Making the most of a free
  tier](#making-the-most-of-a-free-tier).
- **Long conversations that stay coherent.** Automatic and manual context compaction,
  with the originals never deleted, and a context meter built from the **measured**
  `prompt_tokens` the backend reports rather than a guess.
- **Honest numbers.** Everything the model costs — prompt tokens, completion tokens,
  tokens/s, first-token time, the effective per-request limit and where that limit
  came from — is on screen. Estimates are badged as estimates.
- **One place for chat and pictures.** The model can call `generate_image`; the result
  is an ordinary attachment you can save or redraw from its stored recipe.
- **Nothing is locked in.** Three local runtimes, four hosted APIs, keys in the OS
  keychain, chats in one SQLite file you can copy.

## What it does

### Chat

Streaming answers with Markdown, syntax-highlighted code, tables, math and a
collapsible reasoning pane. Per-chat model switching, a Stop button that actually stops
the stream (and the request), per-message tokens/s and first-token time, light and dark
themes, and an interface in **English, German and Chinese**.

Tool calling works both ways: native function calling where the model supports it, and
a parsed text protocol for the local models that do not — so a small model can still
search the web or read a page.

### Research

A research-first chat is what KiChat was built for:

- **Web search** with **Brave Search** (one key, metered) and a **DuckDuckGo**
  fallback that needs no key.
- **Page scraping** with an SSRF guard and robots.txt handling, so a model reading a
  URL cannot be talked into reading your router's admin page.
- **Search first** as a switch: search before every answer, seed the turn with sources,
  then let the model reason over them.
- **Token budgets per page**, so one long article cannot swallow the context window.

### Images

Picture generation is a first-class part of the chat, not a separate toy:

- **Three servers, one client**: AUTOMATIC1111, Forge, and stable-diffusion.cpp's
  `sd-server` all share the `/sdapi/v1/txt2img` route KiChat speaks. A WebUI on
  `:7860` usually needs nothing but `--api` in its start command.
- **A `sd-server` KiChat starts and stops itself**, finding the program in the folder
  you unpacked it into and choosing a port that avoids LM Studio's 1234. And a
  **one-shot `sd-cli`** mode that runs the binary with its own flags, once per picture.
- **The model chooses the parameters.** Through `generate_image` it can set size,
  steps, sampler, seed, guidance — and leaving a field empty means "the server's
  default", while a value typed in Settings is fixed and overrides what the model asked.
- **Profiles** that save a whole picture configuration and switch checkpoint in one
  pick, an optional **hires second pass** (1.25–2× per side) verified against the
  returned picture's own size, and **VAE tiling** so a large pass decodes on a small
  card.
- **One draw at a time, with a Stop button and live step/preview progress.** The
  picture becomes an ordinary image attachment, so you can save it to a folder or redraw
  it later from the recipe stored with it.

### Code

- A **Code & engineering** system prompt preset beside the research, small-local,
  neutral and empty ones — and you can edit or add your own.
- Syntax-highlighted diffs, shell snippets and whole files, with a copy button on
  every block.
- Files as context: drop in a source file, a PDF spec or a spreadsheet and the model
  can search it with the `search_documents` tool instead of you pasting chunks.
- Native tool calling for agent-shaped work, with the same tools exposed to local
  models through the text protocol.
- One model per chat: a planning chat on the cheap fast one and a review chat on the
  big one sit side by side, each with its own context.

### Documents and vision

Attach **PDF, DOCX/DOC, XLSX/XLS/ODS** and plain text. Documents are extracted (with
poppler's `pdftotext` when it is installed, a built-in parser otherwise), chunked,
indexed in SQLite FTS5 and retrieved with BM25 — the model calls `search_documents`
rather than receiving the whole file.

A local GGUF with a **vision projector** (`mmproj`) can see images attached to a turn;
KiChat pairs the files, passes `--mmproj` to `llama-server`, and badges the model
`vision` only when the server itself confirms it.

## Backends and providers

Every backend is a row in **Settings → Backends**. The two local ones ship enabled; the
hosted ones ship **off** so nothing metered is probed or billed until you turn it on and
paste a key.

| Provider | Where it runs | Key | Shipped | Why pick it |
| --- | --- | --- | --- | --- |
| LM Studio | `127.0.0.1:1234` | none | enabled | the easiest way to run a GGUF locally |
| Ollama | `127.0.0.1:11434` | none | enabled | the models you already pulled |
| llama.cpp | local, or managed by KiChat | none | disabled | KiChat downloads `llama-server`, starts it on a free port, stops it with the app |
| Groq | `api.groq.com` | yes, free tier | disabled | very fast inference on a smaller free allowance |
| NVIDIA | `build.nvidia.com` | yes, free to create | disabled | one catalogue spanning several model vendors |
| Mistral | `api.mistral.ai` | yes | disabled | a European host whose small models are cheap enough to leave on |
| DeepSeek | `api.deepseek.com` | yes | disabled | cheap reasoning models |

Local, hosted and mixed models all appear in the same model dropdown, with the context
window each backend actually reports. Where a backend publishes no window (NVIDIA
publishes none on any route), the number is corrected in the model override rather than
invented — and a wrong window is a setting, not a recompile.

**A key you are not sure about is a solved problem.** The model-availability check asks
each model of a backend one token, and hides the ones your account cannot call — the
usual case on a catalogue like build.nvidia.com's, which lists models most keys have no
access to. It costs one request per model, so it runs when you ask for it, and its
result is stored.

## Making the most of a free tier

A free tier is a small budget, and KiChat is built around that rather than around an
unlimited wallet. Each of these is measured against the real API or pinned by a test,
not assumed:

- **The per-request ceiling is read, not guessed.** Groq publishes a token-per-minute
  allowance and refuses any single request above it — 8,000 tokens on a free account,
  against a model window several times that. KiChat takes the ceiling from the budget
  header on an ordinary answer and from a refusal's own sentence, keeps it for the next
  turn, and *only ever tightens* it. The context meter then measures against the real
  budget, so a chat that looks like it has room is told it does not.
- **The fold happens before the refusal.** When the next request would exceed the
  ceiling, the conversation is compacted to fit, with the summarisation itself budgeted
  against the corrected ceiling — and the same check is asked again between tool rounds,
  so a long turn folds before its *next* request rather than after it was rejected.
- **The minute is paced, not discovered.** An allowance is requests per minute as well
  as tokens per minute. A turn that needs more rounds than the rate allows is spread
  over the minute before its first request instead of hitting the wall, and the wait is
  shown as its own phase — nothing was refused and nothing is spent.
- **A refusal that means "too big" is told apart from a throttle.** Both arrive as the
  same `rate_limit_exceeded` envelope, so the words of the refusal decide: an oversized
  *request* is folded and retried once, a *throttle* waits out the retry the provider
  names, and the wait is visible while it sleeps.
- **Unusable models are hidden, not offered.** The one-token availability check above
  keeps a catalogue from advertising models your key cannot call.

The same discipline applies offline: one draw at a time on one GPU, a step counter and
a preview rather than an indefinite spinner, and a Stop button that reaches every
running draw.

## Which local model fits your machine

The rule of thumb is arithmetic: a `Q4_K_M` GGUF is roughly 4.8 bits per weight, so
about **0.6 GB per billion parameters**, plus the KV cache for your context and a
gigabyte or two for the desktop. A vision model needs its `mmproj` projector as a
second file on top of that.

| Memory for the GPU | What fits at `Q4_K_M` | What to expect |
| --- | --- | --- |
| 4–6 GB | 3–4B | fast chat, summarising, simple tool calls |
| 8 GB | 7–8B | the usual "good enough" local assistant |
| 12 GB | 12–14B | better reasoning, decent code |
| 16 GB | 14B at `Q5`, or 20B at `Q4` | a local model you stop apologising for |
| 24 GB | 27–32B | close enough to a hosted mid-tier model to matter |
| 48 GB+ (or 64 GB unified) | 70B at `Q4`, at a shorter context | the largest open weights a desktop can hold |

Three caveats worth more than the table:

- **These are starting points, not promises.** Context length, quantisation and the
  specific model change the fit by gigabytes. KiChat shows the running model's actual
  placement (`34 of 34 layers on the GPU`, with the device and its free memory) so you
  can see whether it really is on the card or quietly half on the CPU.
- **Unified memory is not a GPU with that much VRAM.** An Apple Silicon or APU machine
  gives the GPU a slice of system RAM, and bandwidth — not capacity — becomes the limit.
- **CPU-only is a real option.** A 4B model at short context is usable on any laptop,
  and the CPU is what every GPU build degrades to when the driver or runtime is missing.

KiChat does not download model weights yet — it lists the GGUFs already in the folders
LM Studio uses, or wherever you point it. A curated catalogue with one-click download
and configuration is [on the roadmap](#roadmap).

## Install

KiChat is a native desktop app built with [Wails v2](https://wails.io): a Go backend,
a React 19 frontend, SQLite on disk. There is no hosted service and no app store — the
`Makefile` produces the package for each platform, and each target resolves its own
toolchain from `PATH` with fallbacks.

| Platform | Command | Artifact |
| --- | --- | --- |
| Linux (host platform) | `make build` | `build/bin/kichat` |
| Linux (system-wide) | `sudo make install` | binary, `.desktop` entry, man page, icon |
| Debian, Ubuntu, Mint | `make deb` | `build/deb/kichat_<version>_<arch>.deb` |
| Windows, from Linux or macOS | `make windows` | `build/bin/kichat.exe` |
| Windows installer | `make nsis` | `build/bin/kichat-<arch>-installer.exe` |
| macOS | `make darwin` | `build/bin/kichat.app` (universal) |
| macOS disk image | `make dmg` | `build/dmg/kichat_<version>_<arch>.dmg` |

`make help` lists the rest, including `make run`, `make clean` and `make check`.

**Linux requires the `webkit2_41` build tag** on any distribution shipping only
webkit2gtk-4.1 (Ubuntu 24.04+, Linux Mint 22+). `make build` adds it for you; a raw
`wails build` needs `wails build -tags webkit2_41`, or it fails to link.

**macOS has to be built on a Mac** — Wails packages `.app` bundles only for the host it
runs on, so `make darwin` and `make dmg` refuse on any other machine rather than
producing nothing. The bundle is ad-hoc signed, not notarized, so a downloaded copy
needs its quarantine flag cleared once:

```sh
xattr -dr com.apple.quarantine /Applications/KiChat.app
```

`make darwin DARWIN_ARCH=arm64` builds the one slice instead of the universal pair.

## First run

1. **Start a backend.** Run LM Studio or Ollama, or let KiChat fetch its own:
   **Settings → Backends → llama.cpp → Install runtime**, pick your accelerator (CUDA,
   ROCm, Vulkan or CPU), choose a GGUF, press **Start**. KiChat downloads the build,
   starts `llama-server` on a free port and stops it with the app.
2. **Pick a model and a prompt.** The composer's model dropdown lists everything each
   enabled backend offers. The prompt preset row offers *Research*, *Small local
   models*, *Code & engineering*, *Neutral assistant* and *Empty*, and every preset is
   editable.
3. **Ask something.** Attach files by dropping them on the window; they are extracted,
   chunked and indexed so the model can search them.
4. **Optional: web search.** Paste a Brave Search key for the primary route, or leave it
   empty for DuckDuckGo. Turn on **Search before every answer** for research-heavy work.
5. **Optional: pictures.** In **Settings → Images**, point KiChat at an AUTOMATIC1111 or
   Forge server (`--api`), or at the folder where you unpacked stable-diffusion.cpp and
   let KiChat run `sd-server` itself. The model then has a `generate_image` tool.

## GPU acceleration: CUDA, ROCm, Vulkan

KiChat installs `llama-server` from the build families llama.cpp publishes — CPU,
Vulkan, CUDA, ROCm, and the smaller SYCL, OpenVINO and Adreno trees — and checks for an
update on demand. Which one you want depends less on your card than on what its runtime
looks like on disk.

| Your hardware | Pick | What it needs |
| --- | --- | --- |
| NVIDIA | `cuda-<major>` matching your driver | the driver; KiChat fetches the CUDA runtime bundle |
| AMD | `vulkan` (works everywhere RADV does) | nothing |
| AMD, fastest | `rocm-<version>` | a matching ROCm user-space, installed by hand |
| Anything else | `cpu` or `vulkan` | nothing |

The panel tells you which it got: `CUDA0: NVIDIA GeForce …`,
`ROCm0: AMD Radeon RX 7900 XT (20464 MiB, 20364 MiB free)`, or no device at all — in
which case it also names the libraries the build could not resolve, which is almost
always the real cause.

**What the offload is worth, measured here:** the same 9.8 GB `Q8_0` model answered at
**65 tok/s on the ROCm build against 4.8 tok/s on the CPU build**, with
`offloaded 34/34 layers to GPU` in the log. Numbers like these are why the GPU-layers row
exists — *Automatic*, *All layers* and *CPU only* are one click apart, so the control is
the comparison.

<details>
<summary><strong>NVIDIA CUDA: the runtime bundle, the driver floor, and why the build is not self-contained</strong></summary>

The published CUDA build does **not** carry its runtime. Its `libggml-cuda.so` links
`libcudart.so.13`, `libcublas.so.13` and the driver's own `libcuda.so.1`; unpacked on its
own, `ldd libggml-cuda.so` answers `libcudart.so.13 => not found` on all of them
(measured on b11538 and b11541: 152.8 MB, 62 entries, exactly one of them CUDA-named).

llama.cpp *does* publish the runtime separately, and KiChat fetches it in the same step,
under one progress bar: 440.2 MB for CUDA 13.4, 594.4 MB for 12.8, against 152.8 MB and
172.0 MB for the builds themselves. It is unpacked **beside** the launcher, because that
`$ORIGIN` runpath is the only place the build looks. A CUDA install is therefore
**593 MB fetched by the app itself**, and it is skipped when the machine already has a
CUDA toolkit (`libcudart.so.<major>` and `libcublas.so.<major>` in the loader's index).

The driver is the part no archive carries, and its version picks the build:

| CUDA | oldest driver |
| --- | --- |
| 13.x | 580 |
| 12.x | 525 |
| 11.x | 450 |

A release publishes several suites at once, and KiChat takes the newest one this machine
can run, reading the driver version from `/proc/driver/nvidia/version`. A 570 driver
therefore gets 12.8 rather than a 13.4 build that would find no device and run the model
on the CPU. A machine with no driver at all is refused before the download starts, and
told that the driver is what is missing.

</details>

<details>
<summary><strong>AMD ROCm: the version has to line up, and AMD's repo has to outrank Ubuntu's</strong></summary>

The ROCm build has the same hole and no runtime bundle to fill it. Its `libggml-hip.so`
links `libamdhip64`, `libhipblas` and `librocblas`, and those come from the system.

**The versions have to line up.** Ubuntu 24.04 (noble, and Mint 22 on top of it) ships
ROCm 5.7 — `libamdhip64.so.5` — while the published assets ask for `libamdhip64.so.7`,
`libhipblas.so.3` and `librocblas.so.5`. Those come from ROCm 7.x, so any 7.x or newer
user-space runs the `rocm-10.0` build. KiChat checks before it downloads anything: with
no matching HIP runtime it refuses the 245 MB build and names both versions instead of
handing you a model that silently runs on the CPU.

```sh
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.radeon.com/rocm/rocm.gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/rocm.gpg
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/rocm/apt/7.2.4 noble main" \
  | sudo tee /etc/apt/sources.list.d/rocm.list
sudo apt-get update
sudo apt-get install -y --no-install-recommends rocm-hip-runtime hipblas rocblas
```

That is 17 packages — measured at **3.1 GB to download and 7.5 GB unpacked**, most of it
`rocm-llvm`, `comgr` and `rocblas`, and user-space only. The `amdgpu` module, `/dev/kfd`
and `/dev/dri/renderD128` are the kernel's and are already there; your user still has to
be in the `render` group (usually `video` too) to open them.

**AMD's repository has to outrank Ubuntu's.** Ubuntu also packages `rocminfo` at
`5.7.1`, a version that sorts *above* AMD's `1.0.0.70204`, so without a pin apt keeps
Ubuntu's copy and the two stacks mix. AMD's own installer writes the same pin:

```sh
printf 'Package: *\nPin: origin repo.radeon.com\nPin-Priority: 600\n' \
  | sudo tee /etc/apt/preferences.d/rocm-pin-600
```

Then check it, on the machine and in the app:

```sh
rocminfo | grep -E "Marketing Name|gfx11"   # -> AMD Radeon RX 7900 XT / gfx1100
```

The GPU has to be one that ROCm version supports (`gfx1100` is the 7900 XT). Restart
KiChat or press Save / the refresh arrow in Settings → Backends — a running instance
caches its device probe per binary path — then start the model.

AMD's packages unpack into `/opt/rocm-<version>/lib` and register nothing with the
loader, so KiChat resolves the directory itself (`ROCM_PATH`, then `/opt/rocm`, then the
versioned trees, newest first) and starts `llama-server` with it on `LD_LIBRARY_PATH`.
The device, library and version probes get the same environment, so the panel describes
the build's situation rather than the app's.

**If the panel still says it found no device**, it also names what the build cannot
resolve (`libamdhip64.so.7, libhipblas.so.3, …`): those sonames did not arrive, so the
install above did not land or the pin was skipped. Otherwise use **Vulkan** instead — it
needs no ROCm and runs on every card RADV supports.

</details>

## Ternary models and vision

**Ternary weights** — GGML types `PQ2_0`, `PTQ1_0` — are refused by mainline
llama.cpp, so they need [PrismML's fork](https://github.com/PrismML-Eng/llama.cpp),
which KiChat offers as the **llama.cpp source** in Settings → Backends. Everything else
works the same way: pick a build family, install, pick a GGUF. The fork compiles its
ternary kernels *into* its GPU backends, so offloading one genuinely accelerates it.
Measured here on `Ternary-Bonsai-2-27B-PQ2_0.gguf` (6.71 GB) with everything offloaded,
on an RX 7900 XT:

| Launcher | Reading in the panel | Ready | Generation | Free memory while loaded |
| --- | --- | --- | --- | --- |
| fork, Vulkan | 65 of 65 layers on `Vulkan0` | 4 s | 53.3 tok/s | 11,087 MiB |
| fork, ROCm 7.2 | 65 of 65 layers on `ROCm0` | 2 s | 57.0 tok/s | 12,580 MiB |
| fork, Vulkan with `--gpu-layers 0` | no layer on the GPU | 3 s | 4.6 tok/s | 18,349 MiB |

**Vision** is one file away. A multimodal GGUF carries only the language half; the
vision tower lives in a separate `mmproj` file, and a server started without it refuses
an image turn outright. KiChat finds the projector that sits beside the model at every
start, passes `--mmproj` to the server, and only badges the model `vision` when the
server reports a vision modality — so a projector-less start is listed as text-only
rather than promising something it cannot do. A folder with two quants of one projector,
or a projector named for a different model, is answered with *none* rather than a guess;
a path you set yourself pins one file instead.

## Privacy and where data lives

Your chats are yours: no KiChat account, no telemetry, no analytics, no crash reporting.
The only outbound requests KiChat makes are to the backends you configured, the search
provider you use, and GitHub when you ask it to check for a llama.cpp update.

| What | Location |
| --- | --- |
| Chats, messages, documents, FTS index, attachments, settings | `~/.config/kichat/kichat.db` (`~/Library/Application Support/kichat/kichat.db` on macOS, `%AppData%\kichat` on Windows) |
| API keys and the image server's credential | the OS keyring; an encrypted `secrets.enc` beside the database when no keyring is available |

The keyring is preferred because a key in a file is a key in a backup. Nothing is ever
read from the working tree, and nothing is written into it.

Two things leave the machine by design, and both are visible when they happen: **web
search** (the query goes to Brave or DuckDuckGo) and **hosted models** (the same prompt
and context a local model would receive). A local model plus no search is a fully
offline setup.

Compaction never deletes messages — the originals stay in the database and remain
viewable, and a compaction is a separate row you can inspect or delete.

## Build from source

### Requirements

- **Go** 1.26+ (see `go.mod`)
- **Wails CLI v2** — `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **Node 20.19+ or 22.12+** (so npm 10+)
- **Linux**: GTK3 and WebKit2GTK, plus the build tag described below
- **macOS**: the Xcode Command Line Tools, and a Mac to build on
- **Windows**: nothing beyond Go, Wails and Node — the cross-build from Linux needs no
  mingw (`make nsis` additionally wants `makensis` on `PATH`)

```sh
# Linux
wails build -tags webkit2_41   # when only webkit2gtk-4.1 is available
wails build                    # when webkit2gtk-4.0 is available

# macOS
wails build                    # produces build/bin/kichat.app

# Windows, from Linux or macOS
wails build -platform windows/amd64
```

`wails build` installs the frontend dependencies, compiles `frontend/dist`, and only
then compiles Go, because `main.go` embeds that directory. A bare `go build ./...` on a
fresh clone therefore fails with `pattern all:frontend/dist: no matching files found`
until the frontend has been built once — that is expected, not a packaging error.

Windows builds are cross-built in pure Go: Wails writes the executable's version
resource, its manifest and its icon itself, and nothing here needs cgo. The result is a
22 MB **GUI** executable (no console window) whose Properties come from the `info` block
in `wails.json` — which `make build` keeps in step with `VERSION`.

<details>
<summary><strong>Node 18 / npm 9: the native-binding trap</strong></summary>

On **Node 18 / npm 9**, a clean `npm install` leaves Vite unable to start:

```
Error: Cannot find native binding. npm has a bug related to optional
dependencies (https://github.com/npm/cli/issues/4828).
```

Two separate things combine to cause it:

1. `@tailwindcss/oxide@4.3.3` declares `engines.node: ">= 20"`, so npm 9 reports
   `EBADENGINE` and warns rather than refusing.
2. Its platform packages declare `libc: "glibc"`. **npm 9 cannot evaluate the `libc`
   field**, so it classifies the perfectly matching `@tailwindcss/oxide-linux-x64-gnu` as
   an `UNMET OPTIONAL DEPENDENCY` and silently skips the binary. `libc`-aware optional
   dependency resolution only arrived in npm 10.

The correct fix is Node 20.19+/22.12+, which also unblocks `wails dev`. If you are stuck
on Node 18, install the binding explicitly — npm honours a package requested by name
even when it cannot match it as an optional dependency:

```sh
npm i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3   # run inside frontend/
```

Declaring that package in `package.json` does **not** help on npm 9: the optional filter
runs before the declaration is consulted, verified by trying it. `--no-save` also keeps
it out of `package-lock.json`, so the command has to be repeated after any change to the
lockfile.

</details>

## Development

```sh
make run                        # build and launch
wails dev -tags webkit2_41      # hot reload; on macOS: wails dev
```

`wails dev` serves the bound Go methods on <http://localhost:34115> as well.

The UI can also be driven in a browser with no backend at all: `make frontend` then
`go run ./tools/uiharness` serves the built frontend with a mocked Go bridge on
<http://127.0.0.1:8791>, complete with fixture chats, streaming replies and simulated
rate-limit phases. The page carries a purple **MOCK BRIDGE** badge, so it is never
mistaken for the real app.

`wails build` regenerates `frontend/wailsjs/go/models.ts`, so after changing a Go struct
that crosses the bridge, build before type-checking the frontend.

## Tests and checks

```sh
make check            # gofmt, vet (linux/windows/darwin), tests, tsc — in one step
make build            # and a real build, into build/bin/kichat
go test ./... -count=1
```

Live tests are opt-in and never run in the default set, because they touch metered
services or gigabytes of weights:

```sh
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/research/ -run TestLive -v
KICHAT_LIVE=1 KICHAT_GROQ_KEY=<key>  go test ./internal/llm/     -run TestLiveGroq -v
KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/  -run TestLiveDeepSeek -v
KICHAT_LIVE=1 KICHAT_MISTRAL_KEY=<key>  go test ./internal/llm/  -run TestLiveMistral -v
KICHAT_LIVE=1 KICHAT_NVIDIA_KEY=<key>   go test ./internal/llm/  -run TestLiveNvidia -v
KICHAT_LIVE=1 go test ./internal/llm/ -run TestLiveEngineTurn -v   # needs LM Studio
```

[`AGENTS.md`](AGENTS.md) lists every live test, what each one buys, and the environment
variable that gates it. Never put a key in a tracked file — pass it in the environment.

Two tests are worth knowing about because they guard the numbers this app is honest
about: the live LM Studio test asserts a streamed response reports a non-zero
`promptTokens` (zero would make the context meter silently fall back to an estimate),
and the Groq tests assert the per-request ceiling and the rate-limit remainder really are
readable from a live response.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `Cannot find native binding` from Vite | Node older than 20.19 — see the npm 9 trap above |
| Link error about webkit2gtk | build without `-tags webkit2_41` on a 4.1-only system |
| `pattern all:frontend/dist: no matching files found` | `go build` before the frontend was ever built — run `make build` |
| "no backend reachable" on first start | LM Studio and Ollama both stopped: start one, or install the managed llama.cpp runtime |
| A model is missing from the dropdown | its backend is disabled, or the availability check hid it because your key cannot call it |
| Context meter shows a lower limit than the model's window | that is the backend's per-request ceiling, which is real — see the tooltip and the inspector |
| Model answers slowly with no GPU in the panel | the build cannot resolve its runtime (CUDA/ROCm), or the driver is too old — the panel names the missing libraries |
| A picture never appears | the WebUI was started without `--api`, or the checkpoint is not installed; `sd-server` needs its `-m` model file |
| First picture takes minutes | a one-shot `sd-cli` draw loads the model silently before its first step; the managed `sd-server` shows a `starting` row instead |

## FAQ

**Does KiChat need an internet connection?**
No. With a local model and web search off, it never makes a network request. Search is
the opt-in exception.

**Does it send my chats to a cloud service?**
Only to the model you chose. A local backend means nothing leaves the machine; a hosted
backend means your prompt goes to that provider, exactly as it would in their own client.
There is no KiChat server in between, and no telemetry.

**Do I need a graphics card?**
No. CPU builds are offered for every platform, and they are what a missing driver falls
back to. A GPU turns a 4.8 tok/s answer into 65 tok/s on the same 9.8 GB model; a 4B
model is usable on a laptop's CPU.

**Do I need LM Studio or Ollama?**
No. KiChat can download and run `llama-server` itself, and start and stop it with the
app. If you already use LM Studio or Ollama, point KiChat at them — they ship enabled.

**Do I need an API key?**
Only for hosted providers and Brave Search. Everything local, and DuckDuckGo search,
needs no key. Hosted backends ship switched off, so an unused key is never spent on a
health check.

**Can I use a free-tier key?**
Yes, and that case is designed for. Groq's free tier is a per-request ceiling and a
per-minute request allowance: the ceiling is read from the API's own budget header (or
from a refusal's sentence), the request rate from a refusal plus the app's own count of
what it sent, and both are respected rather than discovered the hard way. See [Making
the most of a free tier](#making-the-most-of-a-free-tier).

**Can it generate images without AUTOMATIC1111?**
Yes — stable-diffusion.cpp's `sd-server` speaks the same API, and KiChat can start and
stop it from the folder you unpacked. `sd-cli` works too, one picture per process, which
skips loading a server at all.

**Does it generate video?**
Not yet. Chat, research, images and coding are implemented; video generation is on the
[roadmap](#roadmap).

**Which model should I download?**
See [Which local model fits your machine](#which-local-model-fits-your-machine). Until
the curated catalogue lands, the GGUFs in your LM Studio folders are already offered.

**Can I run it on Windows?**
Yes, natively or cross-built from Linux. Windows needs the WebView2 runtime, which
Windows 11 and Windows 10 from 21H2 already have.

**Where are my chats stored?**
One SQLite file under your config directory — see [Privacy and where data lives](#privacy-and-where-data-lives). Copy it and you have moved your history.

## Roadmap

Delivered: chat, research, images, coding, the managed llama.cpp runtime with GPU
offload, vision projectors and the free-tier budget machinery.

Next, roughly in the order they matter:

- **A curated model catalogue.** Pick your machine — or let KiChat read it — and get a
  recommended GGUF with its context window, GPU layers and sampler settings, downloaded
  and launched in one click. The runtime installer is the template: the same one-click
  evidence (size, source, progress) applied to weights instead of builds.
- **Video generation.** Short clips from open video models, as a sibling of the picture
  path rather than a separate app, with the same rules for parameters, progress and
  cancellation.
- **More picture backends.** ComfyUI (a workflow graph rather than a flat prompt) and
  hosted image APIs behind the same interface the three current servers share.
- **More picture operations.** `img2img`, inpainting, upscaling, and more than one
  picture per request.
- **Faster first draws.** The one-shot draw's model load is silent today; the managed
  server reports it as a bounded `starting` row.
- **Repo-aware coding.** A workspace tool that can read, search and edit a project
  folder, on top of the tool-calling that already exists.

## Documentation

The README is the front door; the measured detail behind each subsystem lives in its own
document, and each one distinguishes what was observed from what is expected.

| Document | What it covers |
| --- | --- |
| [`PLAN.md`](PLAN.md) | the architecture and the reasoning behind it |
| [`COMPACT.md`](COMPACT.md) | context compaction: strategy, arithmetic, failure modes |
| [`RESEARCH.md`](RESEARCH.md) | web search and scraping: providers, refusals, SSRF and robots guards, budgets |
| [`IMAGES.md`](IMAGES.md) | picture generation: the three server kinds, parameters, profiles, verification |
| [`LLAMACPP.md`](LLAMACPP.md) | the llama.cpp provider, the managed runtime, projector pairing, ternary models |
| [`GROQ.md`](GROQ.md) | the Groq backend, its listings, refusals, ceilings and pacing |
| [`NVIDIA.md`](NVIDIA.md) | the build.nvidia.com backend and its availability check |
| [`MISTRAL.md`](MISTRAL.md) | the Mistral backend, alias collapse, typed content chunks |
| [`DEEPSEEK.md`](DEEPSEEK.md) | the DeepSeek backend, reasoning content, refusals |
| [`AGENTS.md`](AGENTS.md) | how to build, check and change this repo; the live-test matrix |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | pull requests, the CLA, commit style, the harness |

## Versioning

The version is authored once, in [`VERSION`](VERSION). `make build` links it into
`internal/version` (shown in Settings → About and by `kichat --version`) and keeps the
Windows version resource in `wails.json` in step; `make deb` uses it for the package
name, its control file and its changelog. A release is one line in that file — and
`VERSION=x.y.z` on the make command line overrides it for a one-off.

## License

KiChat is licensed under the **Apache License, Version 2.0**. The full text is in
[`LICENSE`](LICENSE) and the attribution in [`NOTICE`](NOTICE). In short: you may
use, modify, redistribute and sell it, provided you keep the notices and mark the
files you changed; it comes with no warranty, and the name and logo are not part
of the trademark grant.

The binary ships third-party components under their own licences — MIT, BSD,
ISC and Apache-2.0. Their copyright notices and licence texts are in
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md), regenerated from the module
graph with `go run ./tools/notices`.

Contributions are welcome under the [contributor licence
agreement](CLA.md): you keep your copyright, and everything you contribute stays
available to everyone under Apache-2.0. [CONTRIBUTING.md](CONTRIBUTING.md) has
the process.
