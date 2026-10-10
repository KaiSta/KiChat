# KiChat

A local-first research chat desktop app. A Wails v2 Go backend with a React 19 /
TypeScript frontend, driving **LM Studio** (`:1234`) or **Ollama** (`:11434`) so long
research conversations run against local models instead of a hosted service.

Features: automatic and manual context compaction, a live context-window meter built
from *measured* prompt tokens, Brave Search with a DuckDuckGo fallback, page scraping
with an SSRF guard, file/PDF/office attachments with BM25 retrieval over the chunks,
an editable system prompt (German research default), per-message tokens/s, native tool
calling with a text-protocol fallback, and in-chat model switching with a Stop button.

## Requirements

- **Go** 1.26+ (see `go.mod`)
- **Wails CLI v2** — `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **A chat backend** — LM Studio on `:1234` or Ollama on `:11434`
- **Linux**: GTK3 and WebKit2GTK. Systems that ship only `webkit2gtk-4.1`
  (Ubuntu 24.04+, Linux Mint 22+) must pass the `webkit2_41` build tag.
- **Node 20.19+ or 22.12+** (so npm 10+). Node 18 can build this project, but only once
  the native Tailwind binding is already installed — see *Node and npm versions* below.

## Build

```sh
wails build -tags webkit2_41   # when only webkit2gtk-4.1 is available
wails build                    # when webkit2gtk-4.0 is available
```

`wails build` installs the frontend dependencies, compiles `frontend/dist`, and only
then compiles Go, because `main.go` embeds that directory. A bare `go build ./...` on a
fresh clone therefore fails with `pattern all:frontend/dist: no matching files found`
until the frontend has been built once — that is expected, not a packaging error.

## Development

```sh
wails dev -tags webkit2_41
```

Requires Node 20.19+/22.12+ (see below). Wails additionally serves the app on
<http://localhost:34115> so the bound Go methods can be called from a browser.

## Node and npm versions

On **Node 18 / npm 9**, a clean `npm install` leaves Vite unable to start:

```
Error: Cannot find native binding. npm has a bug related to optional
dependencies (https://github.com/npm/cli/issues/4828).
```

Two separate things combine to cause this:

1. `@tailwindcss/oxide@4.3.3` declares `engines.node: ">= 20"`, so npm 9 reports
   `EBADENGINE` and warns rather than refusing.
2. Its platform packages declare `libc: "glibc"`. **npm 9 cannot evaluate the `libc`
   field**, so it classifies the perfectly matching `@tailwindcss/oxide-linux-x64-gnu`
   as an `UNMET OPTIONAL DEPENDENCY` and silently skips the binary. `libc`-aware
   optional-dependency resolution only arrived in npm 10.

The correct fix is Node 20.19+/22.12+, which also unblocks `wails dev`. If you are
stuck on Node 18, install the binding explicitly — npm honours a package requested by
name even when it cannot match it as an optional dependency:

```sh
npm i --no-save @tailwindcss/oxide-linux-x64-gnu@4.3.3   # run inside frontend/
```

Declaring that package in `package.json` does **not** help on npm 9: the optional
filter runs before the declaration is consulted, which was verified by trying it. Note
also that `--no-save` keeps it out of `package-lock.json`, so the command has to be
repeated after any change to the lockfile, and a machine that only runs
`npm install` will not pick it up.

## ROCm (AMD GPUs)

The managed llama.cpp ROCm build (`ubuntu-rocm-*`) carries **no** runtime of its own:
its `libggml-hip.so` links `libamdhip64`, `libhipblas` and `librocblas`, and those have
to come from the system — there is no runtime bundle for this family, unlike CUDA (see
below). Pick *ROCm (AMD GPU)* as the **Build** in the model's settings, and install a
matching ROCm first — otherwise the backend cannot load, llama.cpp reports no device and
the model quietly runs on the CPU.

**The versions have to line up.** Ubuntu 24.04 (noble, and Mint 22 on top of it) ships
ROCm 5.7, which provides `libamdhip64.so.5`, while the published ROCm assets ask for
`libamdhip64.so.7`, `libhipblas.so.3` and `librocblas.so.5`. Those sonames come from
ROCm 7.x, and the HIP soname has stayed at `.7` since, so any 7.x or newer user-space
runs the `rocm-10.0` build. KiChat checks this before it downloads anything: with no
matching HIP runtime it refuses the 245 MB build and names both versions instead.

Measured on 2026-10-10 with ROCm 7.2.4 for noble on a Radeon RX 7900 XT: the three
sonames are exactly what `hip-runtime-amd`, `hipblas` and `rocblas` install.

```sh
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.radeon.com/rocm/rocm.gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/rocm.gpg
echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/rocm/apt/7.2.4 noble main" \
  | sudo tee /etc/apt/sources.list.d/rocm.list
sudo apt-get update
sudo apt-get install -y --no-install-recommends rocm-hip-runtime hipblas rocblas
```

That is 17 packages — measured on this machine at **3.1 GB to download and 7.5 GB
unpacked**, most of it `rocm-llvm`, `comgr` and `rocblas`. It is user-space only: the
same transaction upgraded nothing and removed nothing. The `amdgpu` module, `/dev/kfd`
and `/dev/dri/renderD128` are the kernel's and are already there. Your user still has to
be in the `render` group (and usually `video`) to open them.

**AMD's repository has to outrank Ubuntu's.** Ubuntu also packages `rocminfo`, at
`5.7.1` — a version that sorts *above* AMD's `1.0.0.70204` — so without a pin apt keeps
Ubuntu's copy and the two stacks mix. AMD's own installer writes the same pin:

```sh
printf 'Package: *\nPin: origin repo.radeon.com\nPin-Priority: 600\n' \
  | sudo tee /etc/apt/preferences.d/rocm-pin-600
```

**Then check it, on the machine and in the app:**

```sh
rocminfo | grep -E "Marketing Name|gfx11"   # -> AMD Radeon RX 7900 XT / gfx1100
```

The GPU has to be one that ROCm version supports (`gfx1100` is the 7900 XT). After
that, restart KiChat or press Save / the refresh arrow in Settings → Backends — a running
instance caches its device probe per binary path — and start the model. The panel then
reads `ROCm0: AMD Radeon RX 7900 XT (20464 MiB, 20364 MiB free)` instead of a CPU
fallback. Measured here: the same 9.8 GB Q8_0 model answered at **65 tok/s on the ROCm
build against 4.8 tok/s on the CPU build**, with `offloaded 34/34 layers to GPU` in the
log. Settings → Backends shows that same reading for a running managed model — `On
ROCm0: 34 of 34 layers on the GPU` — beside a GPU-layers row that chooses between
automatic placement, every layer, or CPU only.

**Where KiChat looks for the libraries.** AMD's packages unpack into
`/opt/rocm-<version>/lib` and register nothing with the loader — `ldconfig -p` has no
`libamdhip64.so.7` — so KiChat resolves the directory itself (`ROCM_PATH`, then
`/opt/rocm`, then the versioned `/opt/rocm-*` trees, newest first) and starts
`llama-server` with it on `LD_LIBRARY_PATH`. The device, library and version probes get
the same environment, so the panel describes the build's situation rather than the app's.

**If the panel still says it found no device**, it also names what the build's backend
cannot resolve (`libamdhip64.so.7, libhipblas.so.3, …`): those sonames did not arrive,
so the install above did not land or the pin was skipped. Otherwise use *Vulkan (GPU)*
instead — it needs no ROCm and runs on every card RADV supports.

## CUDA (NVIDIA GPUs)

The CUDA build has the same hole in it. Its `libggml-cuda.so` links `libcudart.so.13`,
`libcublas.so.13` and the driver's own `libcuda.so.1`, and the build archive carries
none of them: unpacked on its own, `ldd libggml-cuda.so` answers `libcudart.so.13 => not
found`, `libcublas.so.13 => not found` and `libcuda.so.1 => not found`. Measured on
b11538 and again on b11541: 152.8 MB, 62 entries, exactly one of them CUDA-named — its
own backend, whose runpath is `$ORIGIN`.

**What differs from ROCm: llama.cpp does publish the runtime for CUDA.** The same
release carries `cudart-llama-b11541-bin-ubuntu-cuda-13.4-x64.tar.gz`: 440.2 MB for
13.4 and 594.4 MB for 12.8, against 152.8 MB and 172.0 MB for the two builds themselves.
KiChat fetches it in the same step, under one progress bar over both archives. It is
unpacked *beside* the launcher rather than anywhere else, because that runpath is the
only place the build looks: the bundle's own wrapper directory is flattened, leaving
`libcudart.so.13`, `libcublas.so.13` and `libcublasLt.so.13` next to `llama-server`.
A CUDA install is therefore 152.8 + 440.2 = **593 MB fetched by the app itself** —
while ROCm fetches a 245 MB build and then needs 3.1 GB of AMD user-space by hand.

The bundle is skipped when the machine already has one: if the loader's own index names
both `libcudart.so.<major>` and `libcublas.so.<major>`, a CUDA toolkit is installed and
the 440 MB is left alone. (Windows takes the same bundle as
`cudart-llama-bin-win-cuda-13.4-x64.zip`, 423.5 MB, with no build tag in its name.)

**The driver is the part that is never bundled, and its version picks the build.**
`libcuda.so.1` comes from the NVIDIA driver and appears in no archive, and a build
cannot initialise on a driver older than its major's minimum — NVIDIA's compatibility
table:

| CUDA | oldest driver |
| --- | --- |
| 13.x | 580 |
| 12.x | 525 |
| 11.x | 450 |

A release publishes several suites at once — b11541 carries `cuda-12.8-x64` and
`cuda-13.4-x64` — and KiChat takes the newest suite this machine can run, reading the
driver version from `/proc/driver/nvidia/version`. A 570 driver therefore gets 12.8
rather than a 13.4 build that would find no device and run the model on the CPU; a
machine with no driver at all (`libcuda.so.1` missing from `ldconfig -p`) is refused
before the download starts, and told that the driver is what is missing.

**Check it the way the ROCm one is checked.** Restart KiChat or press Save / the
refresh arrow in Settings → Backends, then start the model: the panel reads
`CUDA0: NVIDIA GeForce …`. A build that reports no device while its libraries do
resolve is one whose driver is too old for it.

## Ternary models (PrismML's fork)

A ternary GGUF — `PQ2_0`, `PTQ1_0` — is a GGML type mainline llama.cpp refuses to load,
so it needs [PrismML's fork](https://github.com/PrismML-Eng/llama.cpp), which KiChat
offers as the `llama.cpp source` in Settings → Backends. Everything else works as with
the official releases: pick a build family, download it, pick a GGUF.

**The fork's kernels are compiled into its GPU backends**, so offloading a ternary model
genuinely accelerates it rather than just moving the weights. Measured here on
`Ternary-Bonsai-2-27B-PQ2_0.gguf` (6.71 GB) with everything offloaded, on an RX 7900 XT:

| Launcher | Reading in the panel | Ready | Generation | Free memory while loaded |
| --- | --- | --- | --- | --- |
| fork, Vulkan | 65 of 65 layers on `Vulkan0` | 4 s | 53.3 tok/s | 11,087 MiB |
| fork, ROCm 7.2 | 65 of 65 layers on `ROCm0` | 2 s | 57.0 tok/s | 12,580 MiB |
| fork, Vulkan with `--gpu-layers 0` | no layer on the GPU | 3 s | 4.6 tok/s | 18,349 MiB |

The GPU-layers row is what produces those readings: *Automatic* is llama.cpp's own
placement, *All layers* forces every one onto the card, and *CPU only* is the last row of
the table — useful as the control that shows what the offload is worth. The sentence next
to the state is the server's own answer, read from its log, and it only claims a GPU when
the server named a device.

## Where data lives

| What | Location |
| --- | --- |
| Chats, messages, documents, FTS index, settings | `~/.config/kichat/kichat.db` |
| API keys (e.g. Brave Search) | `~/.config/kichat/secrets` |

Keys are written outside the working tree and are never read from the repository.

## Tests

```sh
go test ./...                          # hermetic unit tests
KICHAT_LIVE=1 go test ./internal/...   # additionally hits a real LM Studio
```

The live tests assert that a streamed response reports a non-zero `promptTokens`. That
value is what the context meter displays as *measured*; if it ever regresses to zero the
meter silently falls back to an estimate, so the assertion is the guard for it.

## Notes

- Token accounting is backend-authoritative: LM Studio is asked for streamed usage via
  `stream_options.include_usage`. The heuristic estimator is only used to predict a
  request *before* it is sent, and the UI badges those numbers as `estimated`.
- Compaction never deletes messages: originals stay in the database and remain viewable.
- Research requires a Brave Search API key for the primary path; without one it falls
  back to DuckDuckGo, which needs no key.
