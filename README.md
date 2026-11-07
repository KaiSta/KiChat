<p align="center">
  <img src="build/appicon.png" width="96" alt="KiChat app icon: a blue speech bubble with a spark">
</p>

# KiChat

**A local-first AI workspace for chat, research, image generation and code — run models on
your own machine, or make the most of the subscription you already pay for.**

KiChat is a desktop app for **Linux, macOS and Windows** that puts your whole AI toolkit in one
window. It runs open-weight models (GGUF) on your own hardware through **LM Studio**, **Ollama**,
or a **llama.cpp** server it installs and supervises itself, and reaches the hosted providers you
already have: **Groq**, **NVIDIA build.nvidia.com**, **Mistral** and **DeepSeek**. Chats, sources,
documents, pictures and code changes stay in one searchable, offline history on your disk —
encrypted, with the key in your OS keyring and an optional passphrase-protected copy beside it.

No KiChat account, no service of its own, no telemetry. Nothing leaves the machine unless you
picked a hosted model or asked for a web search.

<p>
  <img alt="Go 1.27" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
  <img alt="Wails v2" src="https://img.shields.io/badge/Wails-v2-E83524">
  <img alt="React 19" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=white">
  <img alt="Linux, macOS and Windows" src="https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20Windows-4c8dff">
  <img alt="Interface in English, German and Chinese" src="https://img.shields.io/badge/UI-English%20%C2%B7%20Deutsch%20%C2%B7%20%E4%B8%AD%E6%96%87-2f5fb0">
  <img alt="Licensed under the Apache License 2.0" src="https://img.shields.io/badge/licence-Apache%202.0-4c8dff">
</p>

## Download

Every release is built by CI from its tag and attached to a
[GitHub Release](https://github.com/KaiSta/KiChat/releases) as five files: a `.deb` and a Windows
`Setup.exe` for `amd64` and for `arm64`, and one universal macOS `.dmg`.

| Platform | File | Install |
| --- | --- | --- |
| Debian, Ubuntu, Mint | `kichat_<version>_amd64.deb` (or `_arm64`) | `sudo apt install ./kichat_<version>_amd64.deb` — apt resolves webkit2gtk |
| Windows 10/11 | `kichat-amd64-installer.exe` (or `kichat-arm64-installer.exe`) | run it; WebView2 is already there on Windows 11 and on 10 from 21H2 |
| macOS | `kichat_<version>_universal.dmg` | drag it to Applications, then clear quarantine once — it is ad-hoc signed, not notarized |

```sh
xattr -dr com.apple.quarantine /Applications/KiChat.app   # macOS only, once
```

No package for your system? [Build from source](#build-from-source) — the `Makefile` produces the
same artifacts locally.

## Which local model fits your machine

A `Q4_K_M` GGUF is roughly 4.8 bits per weight — about **0.6 GB per billion parameters** — plus
the KV cache for your context and a gigabyte or two for the desktop. A vision model needs its
`mmproj` projector as a second file on top. These picks were checked on **7 November 2026**, and
each size is a whole download rather than an estimate.

| Memory for the GPU | Pick | Why this one |
| --- | --- | --- |
| 4–6 GB | [**Qwen3.5-4B**](https://huggingface.co/Qwen/Qwen3.5-4B) — 2.7 GB + 0.7 GB projector | vision, tool calling and a 262k window in a 4B: the only pick this size that reads an attached image |
| | [**Spark-X2.5-4B**](https://huggingface.co/XHToken/Spark-X2.5-4B) — 4.1B dense, text only | the stronger agent of the two when the conversation is text: 54.6 on MCP-Atlas and 44.4 on SWE-bench Pro against 40.8 and 29.4 |
| 8 GB | [**Ornith-1.5-9B**](https://huggingface.co/ornith-ai/Ornith-1.5-9B) at `Q4_K_M` — 5.6 GB + 0.9 GB projector | MIT, 262k context, thinking on by default, and its 70.6 SWE-bench Verified belongs to a size class up |
| 12 GB | [**Ornith-1.5-9B**](https://huggingface.co/ornith-ai/Ornith-1.5-9B) at `Q8_0` — 9.6 GB + 0.9 GB | the same model near-lossless; `Q6_K` at 7.7 GB trades the last of it for a longer context |
| 16 GB | [**Qwen3.8-27B GSQ-RCO**](https://huggingface.co/ISTA-DASLab/Qwen3.8-27B-GSQ-RCO-GGUF) at `IQ3_S` — 11.8 GB + 0.9 GB | a 27B that reproduces its BF16 parent on AIME25 and LiveCodeBench at a fifth of the size, vision included; dense, so slower than the row below |
| | [**gpt-oss-20b**](https://huggingface.co/openai/gpt-oss-20b) — 12 GB | the same memory spent on speed: 3.6B active parameters, adjustable reasoning, no vision |
| 24 GB | [**Qwen3.8-27B**](https://huggingface.co/Qwen/Qwen3.8-27B) at `Q4_K_M` — 17–19 GB | the best all-round answer: dense, vision, 262k context, Apache 2.0 |
| 48–64 GB | [**Qwen3.8-27B**](https://huggingface.co/Qwen/Qwen3.8-27B) at `Q8_0` — 29 GB | the memory goes into quantisation and context rather than a larger model |
| 96 GB+ | [**Qwen3.8-Flash-Next**](https://huggingface.co/Qwen/Qwen3.8-Flash-Next) at Q4 — 93–120 GB | 125B with 6B active, the strongest model a desktop realistically runs; 128 GB in practice, under the Qwen Community Licence rather than Apache |

The 4B comparison and Ornith's coding score are the makers' own measurements; the GSQ-RCO figures
are the quantiser's, measured against the BF16 parent. A mixture of experts is the other way to
spend 16 GB: [Qwen3.6-35B-A3B](https://huggingface.co/Qwen/Qwen3.6-35B-A3B) is 20.4 GB at
`Q4_K_M`, but only 3B parameters fire per token, so llama.cpp can hold the expert weights in
system RAM and keep attention and the KV cache on the card — reported at 28–35 tok/s on a
12–16 GB card with 32 GB of RAM. The model panel has the control — **MoE experts**, *all in system
RAM* (`--cpu-moe`) or *first N layers* (`--n-cpu-moe N`) — so it is a dropdown choice like the rest
of the table; every other row assumes the weights fit in VRAM.

KiChat shows the running model's actual placement (`34 of 34 layers on the GPU`, with the device
and its free memory), so you can see whether it is really on the card. On unified memory the limit
is bandwidth rather than capacity. CPU-only is a real option: a 4B at short context is usable on
any laptop.

KiChat does not download model weights yet — it lists the GGUFs already in your LM Studio folders,
or wherever you point it. A curated catalogue is [on the roadmap](#roadmap).

## First run

1. **Start a backend.** Run LM Studio or Ollama, or let KiChat fetch its own:
   **Settings → Backends → llama.cpp → Install runtime**, pick your accelerator (CUDA, ROCm,
   Vulkan or CPU), choose a GGUF — [Which local model fits your
   machine](#which-local-model-fits-your-machine) names one per memory tier — press **Start**.
   KiChat downloads the build, starts `llama-server` on a free port and stops it with the app.
2. **Pick a model and a prompt.** The dropdown lists everything each enabled backend offers; the
   preset row has *Research*, *Small local models*, *Code & engineering*, *Coding agent (project)*,
   *Neutral assistant* and *Empty*, all editable.
3. **Ask something.** Drop files on the window to attach them — they are extracted, chunked and
   indexed so the model can search them instead of you pasting chunks.
4. **Optional: a project.** *New project* asks for one folder; the model reads and searches inside
   it and proposes changes as diffs you approve — see [Code](#code).
5. **Optional: search and pictures.** A Brave key gives the metered search route; leaving it empty
   uses DuckDuckGo. Settings → Images takes an AUTOMATIC1111 or Forge server (`--api`), a
   stable-diffusion.cpp folder KiChat runs itself, or the hosted NVIDIA NIM.

## Why KiChat

- **Local first, not local only.** The same window drives a 4B model on a laptop and a frontier
  model over an API, and switching is a dropdown, not a second app.
- **Long conversations that stay coherent.** Compaction you can trigger or leave automatic, the
  originals never deleted, and a context meter built from the `prompt_tokens` the backend
  actually reported.
- **Honest numbers.** Prompt and completion tokens, tokens/s, first-token time and the effective
  per-request limit — with where that limit came from — are on screen. Estimates are badged as
  estimates.
- **One place for chat and pictures.** The model can call `generate_image`; the result is an
  ordinary attachment you can save, enlarge, or redraw from its stored recipe.
- **Code you can say no to.** Point a project at a folder: every edit arrives as a diff you
  approve, every command as the exact line it will run, and nothing outside that folder is
  reachable.
- **Nothing is locked in.** Four local runtimes, four hosted APIs, keys in the OS keychain, and a
  history that is an encrypted SQLite database with an encrypted export to move it.

## What it does

### Chat

Streaming Markdown with highlighted code, tables, math and a collapsible reasoning pane; per-chat
model switching; a Stop that cancels the stream and the request; per-message tokens/s and
first-token time; light and dark themes; an interface in **English, German and Chinese**.

- **Tool calling both ways:** native function calling where the model has it, a parsed text
  protocol where it does not.
- **Only the tools the request declares are offered**, so a model with no search provider or
  picture server is never promised one it cannot use.
- **Mangled and borrowed names are handled.** `scr scrape_url` is repaired when exactly one tool
  fits and refused rather than guessed at when none does, and another assistant's vocabulary
  (`bash`, `read`, `grep`, `edit`) is translated onto the real tool, arguments included.

### Research

- **Web search** with **Brave** (one key, metered) and a **DuckDuckGo** fallback that needs none,
  behind an SSRF guard and robots.txt handling — a model reading a URL cannot be talked into
  reading your router's admin page.
- **Citations that resolve.** Sources are numbered once per answer — the second search of a turn
  continues at `[11]`, not `[1]` — and the reply carries a **Sources** list with clickable
  markers.
- **Budgeted pages.** A per-page token budget stops one long article swallowing the window, and an
  oversized page is condensed once by a second model instead of replayed at full length on every
  later turn — the tool card keeps the whole page.
- **Every tool card says what it cost:** `12,345 → 640 tokens` when a result was cut and one
  number when it was not, measured by the app rather than estimated. A result the budget refused
  outright reads `4,374 tokens discarded`.

### Images

**Drawing.** AUTOMATIC1111, Forge and stable-diffusion.cpp's `sd-server` share the
`/sdapi/v1/txt2img` route KiChat speaks — a WebUI on `:7860` usually needs nothing but `--api`.
KiChat can also start and stop `sd-server` itself, on a port that avoids LM Studio's 1234, run
`sd-cli` once per picture, or draw on **NVIDIA NIM** with the key the Backends tab already holds
(FLUX.1 schnell, FLUX.1 dev, FLUX.3 Klein; a prototyping allowance, off until you pick it).

- **The model chooses the parameters** through `generate_image` — size, steps, sampler, seed,
  guidance — while an empty field means the server's default and a value typed in Settings wins.
- **Profiles** save a whole configuration and switch checkpoint in one pick, with an optional
  hires pass and VAE tiling so a large draw decodes on a small card. A caption-trained pipeline
  gets a caption written for it.
- **One draw at a time, a Stop button, live progress.** The picture lands as an ordinary image
  attachment and can be redrawn later from the recipe stored with it.

**Enlarging** works on any picture, including a photo you uploaded. *Enlarge…* asks for the target
(FHD at 1920 on the long side, 4K at 3840, or a custom side, aspect kept) and then the method:

| Method | Cost, measured here | Detail | Needs |
| --- | --- | --- | --- |
| **Resample (Lanczos3)** | 0.13 s for 512²→1920², 0.47 s for 512²→3840² | clean, slightly soft; invents nothing | nothing — pure Go |
| **Real-ESRGAN 4×** | 1.4–2.3 s on a Vulkan GPU (22.6 s on llvmpipe) | plausible texture; can hallucinate | an optional 47 MB download and a Vulkan GPU |
| **Redraw (hires)** | minutes | a new interpretation of the picture | the image server, and a picture KiChat drew |

The neural program is one pinned archive with its weights inside, checksum verified by the app,
off by default and in no installer: **Download Real-ESRGAN** in Settings → Images → Enlarging
fetches it. An enlarged copy is filed beside its source with the operation recorded, and a scale
past 8× warns first. Details: [`UPSCALING.md`](documentation/UPSCALING.md).

### Code

**New project** in the sidebar asks for one folder, and the chat that opens is a coding agent that
works inside it and nowhere else:

- **The folder is the boundary.** Every path resolves against that root; one that leaves it,
  including through a symlink, is refused with the reason. There is no fallback to your home
  directory.
- **It reads the folder instead of guessing.** `list_dir`, `find_files` (a glob, `**` crossing
  folders), `search_files` (literal, case-insensitive) and `read_file` with line numbers. A long
  file can be asked for as an **outline** — declarations and line numbers, no bodies — which
  measured **1,597 tokens for `app.go` against 24,429** for the whole file.
- **Every change is shown before it is made.** `edit_file` and `write_file` arrive as a card
  carrying the diff; nothing is written until you answer. The **Changes** panel lists every file
  the chat touched, with its diff and a **Revert** that refuses a file you changed yourself since.
  A card offers **always for this kind in this chat**, and **Settings → Coding agent → Run without
  asking** is the wider per-kind switch that survives a restart.
- **A terminal, one command at a time, behind the same gate.** `run_command` shows the exact line,
  editable on the card, and runs it with no stdin, a 120 s limit (600 s if the model asks), the
  process tree killed at the limit and output capped at 32 KiB per end. A line that would throw
  away uncommitted work (`git reset --hard`, `checkout --`, `restore`, `clean -f`) is refused
  before you are asked, and so is one wanting administrator rights.
- **It sees the working tree:** branch, ahead/behind and up to 40 pending paths are in the prompt
  every turn. A project also gets 60 tool rounds and 120,000 tokens of tool results against a
  chat's 24 and 60,000, and an oversized file read is condensed by a model you pick, with paths
  and line numbers kept verbatim. The folder's own `AGENTS.md` arrives as project knowledge —
  never as permission to read outside it.

[`CODING.md`](documentation/CODING.md) has the gate, the shell, the caps and the undo record.

### Documents and vision

Attach **PDF, DOCX/DOC, XLSX/XLS/ODS** and plain text: extracted (with poppler's `pdftotext` when
installed, a built-in parser otherwise), chunked, indexed in SQLite FTS5 and retrieved with BM25,
so the model calls `search_documents` rather than receiving the whole file. A local GGUF with a
**vision projector** (`mmproj`) can see attached images — KiChat pairs the files, passes
`--mmproj` to `llama-server`, and badges the model `vision` only when the server confirms it.

## Backends and providers

Every backend is a row in **Settings → Backends**. The local ones ship enabled; the hosted ones
ship **off**, so nothing metered is probed or billed until you turn it on and paste a key.

| Provider | Where it runs | Key | Shipped | Why pick it |
| --- | --- | --- | --- | --- |
| LM Studio | `127.0.0.1:1234` | none | enabled | the easiest way to run a GGUF locally |
| Ollama | `127.0.0.1:11434` | none | enabled | the models you already pulled |
| llama.cpp | local, or managed by KiChat | none | disabled | KiChat downloads `llama-server`, starts it on a free port, stops it with the app |
| MLX | local, managed on Apple silicon | none | disabled | KiChat installs `mlx-lm` in a private Python environment and serves a snapshot folder |
| Groq | `api.groq.com` | yes, free tier | disabled | very fast inference on a smaller free allowance |
| NVIDIA | `build.nvidia.com` | yes, free to create | disabled | one catalogue spanning several model vendors |
| Mistral | `api.mistral.ai` | yes | disabled | a European host whose small models are cheap enough to leave on |
| DeepSeek | `api.deepseek.com` | yes | disabled | cheap reasoning models |

Local, hosted and mixed models share one dropdown, each with the window its backend reports.
Where a backend publishes none (NVIDIA on every route), the number is corrected in the model
override rather than invented. **A key you are not sure about is a solved problem:** the
availability check asks each model one token and hides the ones your account cannot call, which
is the usual case on a catalogue like build.nvidia.com's.

## Making the most of a free tier

A free tier is a small budget, and KiChat is built around that rather than an unlimited wallet:

- **The per-request ceiling is read, not guessed.** Groq refuses any single request above its
  token-per-minute allowance — 8,000 tokens on a free account, well under the model's window.
  KiChat takes the ceiling from the budget header on an ordinary answer and from a refusal's own
  sentence, keeps it, and only ever tightens it.
- **The fold happens before the refusal.** A request that would exceed the ceiling is compacted to
  fit, and the same check runs between tool rounds, so a long turn folds before its next request
  rather than after it was rejected.
- **The minute is paced, and "too big" is told apart from a throttle.** A turn needing more rounds
  than the rate allows is spread over the minute before its first request, with the wait shown as
  its own phase; both refusals arrive as `rate_limit_exceeded`, and the words decide whether to
  fold once or to wait out the retry the provider names.
- **Results are cut against the number the backend stated**, not the assembly's estimate: 3,776
  tokens where the estimate alone allowed 4,527, on a prompt reported at 1,200 under a 6,000-token
  ceiling. A page or a file that is too large is condensed once by a second model instead of being
  paid for on every later turn — Spotify's
  [`shunt`](https://engineering.atspotify.com/2026/09/portal-by-spotify-cut-my-claude-code-token-usage-by-90/)
  idea, with KiChat printing its own saving on the card instead of promising one. Their measured
  figure on a 162k-line Java monorepo was 82–94 % per large read.

The same discipline applies offline: one draw at a time on one GPU, a step counter and a preview
instead of an indefinite spinner, and a Stop button that reaches every running draw or neural
enlargement.

## GPU acceleration

KiChat installs `llama-server` from the families llama.cpp publishes — CPU, Vulkan, CUDA, ROCm,
and the smaller SYCL, OpenVINO and Adreno trees.

| Your hardware | Pick | What it needs |
| --- | --- | --- |
| NVIDIA | `cuda-<major>` matching your driver | the driver; KiChat fetches the CUDA runtime bundle |
| AMD | `vulkan` (works everywhere RADV does) | nothing |
| AMD, fastest | `rocm-<version>` | a matching ROCm user-space, installed by hand |
| Anything else | `cpu` or `vulkan` | nothing |

The panel tells you which it got — `CUDA0: NVIDIA GeForce …`,
`ROCm0: AMD Radeon RX 7900 XT (20464 MiB, 20364 MiB free)` — or that it found no device, naming
the libraries the build could not resolve, which is almost always the real cause. **Measured
here:** the same 9.8 GB `Q8_0` model answered at **65 tok/s on the ROCm build against 4.8 tok/s on
the CPU build**, `offloaded 34/34 layers to GPU`. The GPU-layers row (*Automatic*, *All layers*,
*CPU only*) exists so that comparison is one click away.

The CUDA runtime bundle, the driver floors and the ROCm user-space install — the apt repo, and the
pin that keeps AMD's packages ahead of Ubuntu's — are in
[`LLAMACPP.md` §3.14](documentation/LLAMACPP.md#314-the-libraries-the-archive-does-not-carry).

## Ternary models and vision

**Ternary weights** — GGML types `PQ2_0`, `PTQ1_0` — are refused by mainline llama.cpp, so they
need [PrismML's fork](https://github.com/PrismML-Eng/llama.cpp), which KiChat offers as the
**llama.cpp source** in Settings → Backends. The fork compiles its ternary kernels into its GPU
backends, so offloading one genuinely accelerates it: measured on
`Ternary-Bonsai-2-27B-PQ2_0.gguf`, 65 of 65 layers on the GPU at 53–57 tok/s against 4.6 tok/s
with the layers left on the CPU. [`LLAMACPP.md` §4](documentation/LLAMACPP.md#4-ternary-models-on-the-gpu-measured)
has the readings.

**Vision** is one file away: a multimodal GGUF carries only the language half, so a server started
without its `mmproj` refuses an image turn. KiChat finds the projector beside the model at every
start; two quants of one projector, or a projector named for another model, is answered with
*none* rather than a guess, and a path you set yourself pins one file.

## Privacy and where data lives

No KiChat account, no telemetry, no analytics, no crash reporting. KiChat talks only to what you
pointed it at — your backends, your search provider, a picture server that is not on this machine
— and to GitHub when you install or update a llama.cpp build or download the Real-ESRGAN archive.

`kichat.db` is **encrypted**, not a plaintext SQLite file. The key lives in your OS keyring;
`kichat.key` beside the database keeps a copy protected by a passphrase you choose in
**Settings → Data → Database**, so a new machine or a reinstalled OS can still open it. An older,
unencrypted database is converted on first start, and if the keyring entry is gone KiChat asks for
that passphrase instead of opening your chats. What this protects is the file: copied, disk-imaged
or synced into a cloud folder, it is useless without the key. It does not protect you from someone
using the app on an unlocked machine, or from malware running as you —
[`ENCRYPTION.md`](documentation/ENCRYPTION.md) lists the limits.

| What | Location |
| --- | --- |
| Chats, messages, documents, attachments, settings | `~/.config/kichat/kichat.db` (`~/Library/Application Support/kichat/` on macOS, `%AppData%\kichat` on Windows) — encrypted |
| Your API keys | the OS keyring; an encrypted file beside the database when no keyring is available |
| The database key | the OS keyring; a passphrase-protected copy (`kichat.key`) beside the database, if you set one |
| Chat backups you export | one `.kichat` file wherever you save it — AES-256-GCM under a passphrase KiChat never stores |

Backups leave only when you ask: **Settings → Data** writes the chats you pick into one encrypted
file, and importing it offers the same list back, chat by chat. Neither side talks to a server, so
the file can live in any cloud folder you already trust; the newer copy of a chat wins, so an
archive can add to a machine but never roll it back. Two things leave, and both are visible when
they happen: **web search** (the query goes to Brave or DuckDuckGo) and **hosted models or
pictures** (the same prompt a local model would receive). A local model plus no search is a fully
offline setup. Compaction never deletes messages — the originals stay viewable.

## Build from source

KiChat is a Go + Wails v2 desktop app, and the `Makefile` produces every artifact the
releases use. The prerequisites (Go 1.27+, the Wails CLI, Node 20.19+), the `webkit2_41`
tag, the full `make` matrix and the Node 18 / npm 9 trap are in
[`PLAN.md` §6](documentation/PLAN.md#6-build-and-environment).

## Development

```sh
make run                        # build and launch
wails dev -tags webkit2_41      # hot reload; on macOS: wails dev
```

`wails dev` also serves the bound Go methods on <http://localhost:34115>. The UI can be driven in
a browser with no backend: `make frontend` then `go run ./tools/uiharness` serves the built
frontend against a mocked Go bridge on <http://127.0.0.1:8791>, with fixture chats, streaming
replies and simulated rate-limit phases. The page carries a purple **MOCK BRIDGE** badge, so it is
never mistaken for the real app.

`wails build` regenerates `frontend/wailsjs/go/models.ts`, so after changing a Go struct that
crosses the bridge, build before type-checking the frontend. [`AGENTS.md`](AGENTS.md) is the
contributor index: the rules, the checks, and a skill per recurring task.

## Tests and checks

```sh
make check            # gofmt, vet (linux/windows/darwin and both arm64 halves), tests, tsc
make build            # and a real build, into build/bin/kichat
```

Live tests are opt-in and never in the default set, because they touch metered services or
gigabytes of weights:

```sh
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/research/ -run TestLive -v
KICHAT_LIVE=1 KICHAT_GROQ_KEY=<key>  go test ./internal/llm/ -run TestLiveGroq -v
KICHAT_LIVE=1 go test ./internal/llm/ -run TestLiveEngineTurn -v   # needs LM Studio
```

[`.claude/skills/live-tests/SKILL.md`](.claude/skills/live-tests/SKILL.md) is the full matrix of
variables, what each buys and what it costs. Never put a key in a tracked file — pass it in the
environment. Two tests guard the numbers this app is honest about: the LM Studio test asserts a
streamed response reports a non-zero `promptTokens`, and the Groq tests assert the per-request
ceiling and the rate-limit remainder really are readable from a live response.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `Cannot find native binding` from Vite | Node older than 20.19 — see the npm 9 trap above |
| Link error about webkit2gtk | built without `-tags webkit2_41` on a 4.1-only system |
| `pattern all:frontend/dist: no matching files found` | `go build` before the frontend was built — run `make build` |
| "no backend reachable" on first start | LM Studio and Ollama both stopped: start one, or install the managed llama.cpp runtime |
| Answers are slow and no GPU is in the panel | the build cannot resolve its runtime (CUDA/ROCm) or the driver is too old — the panel names the missing libraries |
| A picture never appears | the WebUI was started without `--api`, or the checkpoint is missing; `sd-server` needs its `-m` model file |
| An enlarged picture is black | Real-ESRGAN ran without its `models/` folder — that build exits 0 and looks fine; use the app's download button |
| A file tool answers "this chat has no project folder" | it is an ordinary chat: a folder comes from *New project*, not *New chat* |
| The model wants to run a command and nothing runs | the confirmation card is waiting: the turn is paused until you answer. **Settings → Coding agent → Run without asking** skips it per kind |
| Revert in the Changes panel is greyed out | the file changed since the chat wrote it — by you or a command — or it is not a text file; a running turn blocks it too |
| KiChat opens on a passphrase prompt instead of your chats | the OS keyring no longer holds the key (new machine, reinstalled OS) — enter the passphrase that wraps `kichat.key`, or import an encrypted export |

## Roadmap

**Delivered:** chat, research, images (local and hosted), enlarging, coding — a project folder
with approval-gated edits, a terminal, a Changes panel with undo, and the working tree in the
prompt — the managed llama.cpp runtime with GPU offload, vision projectors, the free-tier budget
machinery, an encrypted database and encrypted chat exports.

- **A curated model catalogue.** Pick your machine, or let KiChat read it, and get a recommended
  GGUF with its window, GPU layers and sampler settings, downloaded and launched in one click.
- **Video generation.** Short clips from open video models, a sibling of the picture path with the
  same rules for parameters, progress and cancellation.
- **More picture backends and operations.** ComfyUI, more hosted image APIs, then `img2img`,
  inpainting and more than one picture per request.
- **Faster first draws.** The one-shot draw's model load is silent today; the managed server
  reports it as a bounded `starting` row.
- **A change that spans many files.** Today one file is one card and one question, and a deletion
  or move through the terminal has no diff and no undo.

## Documentation

The README is the front door; the measured detail behind each subsystem lives in its own document,
and each one distinguishes what was observed from what is expected.

| Document | What it covers |
| --- | --- |
| [`PLAN.md`](documentation/PLAN.md) | the architecture, the reasoning behind it, and the build manual with its prerequisites |
| [`COMPACT.md`](documentation/COMPACT.md) | compaction and tool-result condensation: strategy, arithmetic, failure modes |
| [`CODING.md`](documentation/CODING.md) | coding mode: the folder boundary, the confirmation gate, the terminal, the undo record |
| [`RESEARCH.md`](documentation/RESEARCH.md) | search and scraping: providers, refusals, SSRF and robots guards, budgets |
| [`IMAGES.md`](documentation/IMAGES.md) | picture generation: server kinds, parameters, profiles, verification |
| [`IMAGES-CLOUD.md`](documentation/IMAGES-CLOUD.md) | hosted picture backends: what each free tier really is |
| [`UPSCALING.md`](documentation/UPSCALING.md) | enlarging: the resampler, Real-ESRGAN, targets, the Enlarge panel |
| [`ENCRYPTION.md`](documentation/ENCRYPTION.md) | the encrypted database: page encryption, the key and its recovery copy, the limits |
| [`LLAMACPP.md`](documentation/LLAMACPP.md) | the llama.cpp provider and runtime, the CUDA/ROCm prerequisites, projectors, ternary models |
| [`MLX.md`](documentation/MLX.md) | the MLX provider and the `mlx-lm` environment KiChat installs on Apple silicon |
| [`GROQ.md`](documentation/GROQ.md) | the Groq backend: listings, refusals, ceilings, pacing |
| [`NVIDIA.md`](documentation/NVIDIA.md) | the build.nvidia.com backend and its availability check |
| [`MISTRAL.md`](documentation/MISTRAL.md) | the Mistral backend: alias collapse, typed content chunks |
| [`DEEPSEEK.md`](documentation/DEEPSEEK.md) | the DeepSeek backend: reasoning content, refusals |
| [`CODE_MAP.md`](documentation/CODE_MAP.md) | where every package, file and extension point lives |
| [`AGENTS.md`](AGENTS.md) | the index: the rules, the checks, and a skill per recurring task |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | pull requests, the CLA, commit style, the harness |

## Versioning

The version is authored once, in [`VERSION`](VERSION). `make build` links it into
`internal/version` (Settings → About, `kichat --version`) and keeps the Windows resource in
`wails.json` in step; `make deb` uses it for the package name, control file and changelog. A
pushed tag is what ships: CI builds the five packages from that commit and attaches them to a
GitHub Release whose notes are the tag's own annotation.

## License

KiChat is licensed under the **Apache License, Version 2.0**; the full text is in
[`LICENSE`](LICENSE) and the attribution in [`NOTICE`](NOTICE). In short: use, modify,
redistribute and sell it, keep the notices and mark the files you changed, no warranty — and the
name and logo are not part of the trademark grant.

The binary ships third-party components under their own licences, with notices in
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md) (regenerated with `go run ./tools/notices`).
Contributions are welcome under the [contributor licence agreement](CLA.md): you keep your
copyright, and what you contribute stays available to everyone under Apache-2.0 —
[`CONTRIBUTING.md`](CONTRIBUTING.md) has the process.
