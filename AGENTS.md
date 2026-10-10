# Working on KiChat

KiChat is a local-first desktop LLM research chat: `main.go` + `app.go` + `api.go` on
top of Go packages in `internal/`, and a React 19 / TypeScript UI in `frontend/`.
It is a **Wails v2** application (Go binary + webkit2gtk webview), talking to LM Studio
(`:1234`) and Ollama (`:11434`), with Brave/DuckDuckGo web search and SQLite storage.
`PLAN.md` describes the architecture and the reasoning behind it; `COMPACT.md` is the deep
dive on context compaction (strategy, arithmetic, failure modes, where the code is).

## Environment (verified, not assumed)

| Tool | Version here | Consequence |
| --- | --- | --- |
| Go | 1.26.0 (`go 1.26.0`) | fine |
| Wails CLI | 2.15.0 | fine |
| Node / npm | 22.23.3 / 10.9.9 in `~/.local/node/bin` | **not on `PATH`** — see Trap 2 |
| webkit2gtk | only 4.1 | **every build needs `-tags webkit2_41`** |

**Trap 1 — build tag.** Only `webkit2gtk-4.1` is installed, so
`wails build -tags webkit2_41` is the only build that works. Without the tag it fails
to link.

**Trap 2 — node and npm are not on `PATH`.** They are in `~/.local/node/bin`
(22.23.3 / 10.9.9) rather than in `/usr/bin`. Mint 22.3 (zena, noble base) offers
`nodejs` 18.19.1 and `npm` 9.2.0 in `noble/universe`, and neither is installed — nor
would it be enough, since npm 9 cannot install Vite 7's native binding (below) and
`vite dev` needs ≥ Node 20.19. The Makefile assumes `npm` is callable, and npm is a
script whose first line is `#!/usr/bin/env node`, so a *path* to npm is not enough:
with node off `PATH`, `make build` dies at its `npm install` step with
`/usr/bin/env: 'node': No such file or directory` (exit 127), and the bare
`npm run build` that `wails build` runs for itself (wails.json) cannot start at all.
Fix that in the shell, not in the Makefile — the Makefile is the same one machines
with a properly installed node use:

```bash
export PATH="$HOME/.local/node/bin:$PATH"   # put it in ~/.bashrc to make it stick
```

A system-wide Node (NodeSource is the maintained apt route on Mint:
`curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -`, then
`sudo apt install nodejs`, which brings its own npm) lands in `/usr/bin` and needs
none of the above. Do not `apt install npm` from Mint's own repo next to it: that is
npm 9, it conflicts, and it is the one this project cannot build with.

The npm 9 `libc` trap that used to live here is history: with npm 10 a clean
`npm install` *does* install `@tailwindcss/oxide-linux-x64-gnu` (verified in an empty
directory against the real lockfile), so the Makefile's extra `--no-save` install and the
directory-as-stamp are now only a guard for an older npm — declaring the binding in
`package.json` or regenerating the lockfile still does not work, tried, do not repeat it.
**`vite dev` needs ≥ Node 20.19 / ≥ 22.12** (below that Vite 7 dies with
`crypto.hash is not a function`); the Node installed here is new enough and
`npm run dev` serves `http://localhost:5173/` in under a second, but `vite build` is what
the targets and the checks run.

**Upgrading the toolchain changes nothing this repo depends on.** Measured against this exact
lockfile, Node 22.23.3 and 24.21.0 (Krypton, the newest LTS — 26 is Current) crossed with
npm 11.19.0 and 12.2.0: a clean `npm install` from a fresh copy, `tsc && vite build`, and,
with Node 24, `make check` and `make build` — all exit 0, and `package-lock.json` comes out
byte-identical (lockfileVersion 3, which npm 9+ all read). Nothing pins the versions: no
`.nvmrc`, no `engines`, no `packageManager`, and the only file naming them is this table.
Two behaviours to expect:

- npm ≥ 11 gates lifecycle scripts behind `allowScripts` and warns about
  `esbuild@0.28.2 (postinstall: node install.js)` on every install ("not yet covered" on 11,
  "blocked" on 12). It is noise, not breakage: the build is green with that script never run,
  because esbuild resolves its prebuilt binary from the platform package the lockfile already
  installs. `npm install-scripts approve esbuild` silences it. (npm 12 permits Node
  `^22.22.2 || ^24.15.0 || >=26`, so it can be installed beside the Node here.)
- the Makefile resolves `npm` from `PATH`, falls back to `~/.local/node/bin/npm`, and never
  resolves `node` at all — so node itself has to be on `PATH` for any of it to run, for the
  shebang reason in Trap 2. Exporting that directory, or installing Node system-wide, is what
  makes a machine work; the Makefile is unchanged for it.

## Checks that must pass

```bash
make check                                  # the next four, in one step
make build                                  # the last one, into build/bin/kichat

gofmt -l .                                  # must print nothing
go vet ./...
go test ./... -count=1
cd frontend && npx tsc --noEmit             # or: npm run build (tsc && vite build)
wails build -tags webkit2_41                # produces build/bin/kichat
```

Both targets add `-tags webkit2_41` and resolve the tools themselves, which is what makes
them work from a shell where neither `node` nor `wails` is on `PATH`. The raw commands are
the same gates, but `npx` and `npm` need Trap 2's `export PATH` first.

`wails build` regenerates `frontend/wailsjs/go/models.ts`, so after changing a Go
struct that crosses the bridge, build (or `wails generate module`) before type-checking
the frontend — otherwise `tsc` fails on the missing field, and the generated diff should
contain only the new fields.

## Live tests (opt-in, use real metered services)

Offline tests never touch the network. Live ones are gated by environment variables:

```bash
# Brave Search: one credit per request — ~4 requests for the tests below
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/research/ -run TestLive -v

# LM Studio: needs a *loaded* chat model; skips itself if none is
KICHAT_LIVE=1 KICHAT_BRAVE_KEY=<key> go test ./internal/chat/ -run TestLive -v
KICHAT_LIVE=1 go test ./internal/chat/ -run TestLiveEngineTurn -v   # needs KICHAT_LMSTUDIO

# DeepSeek: metered, needs a key; skips without KICHAT_DEEPSEEK_KEY
KICHAT_LIVE=1 KICHAT_DEEPSEEK_KEY=<key> go test ./internal/llm/ -run TestLiveDeepSeek -v
#   optional: KICHAT_DEEPSEEK_BASE, KICHAT_DEEPSEEK_MODEL

# PrismML's fork: one release lookup, a 17 MB download and a real model load
KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v
#   optional: KICHAT_PRISM_MODEL=<path.gguf>, otherwise the smallest model in
#   the LM Studio folders; skips if there is none. Loads a real model: minutes.
```

What each live test buys:

- `TestLiveBraveContext` — the real `/res/v1/llm/context` route returns page chunks.
- `TestLiveBraveContextClampsLongQuery` — local clamping beats the API's 422.
- `TestLiveBraveChainUsesContext` — the chain picks the context route, not the fallback.
- `TestLiveBraveBadKeyIsNotMisclassified` — a rejected key is not reported as a throttle.
  It sends no working credential, so it cannot succeed and cannot consume credit; it
  checks both wordings the API uses for one and the same rejection (below).
- `TestLiveSearchFirstTurn` — settings → chain → context → seeded prompt → LM Studio,
  and asserts the context payload is bigger than the headline listing it replaces.
- `TestLiveDeepSeek` — the three claims the provider makes that only the real service
  can confirm: the listing reports a context window (LM Studio's namespace does,
  the plain OpenAI one cannot), `stream_options.include_usage` is honoured so the
  context meter gets a measured `prompt_tokens`, and fragmented `tool_calls` stream
  as expected. DeepSeek reasons by default and spends budget on `reasoning_content`
  first, so a small `max_tokens` returns `finish_reason: "length"` with empty
  `content` — the test asks for 2048/4096 for that reason.
- `TestLivePrismForkRunsAModel` — PrismML's fork as a runtime rather than a table of
  names: its release list, its tag scheme (`TagOfDir` round-trip, and that the tag is not
  offered as an update over itself), its archive — whose `./` root entry aborted the
  install on the test's first run — its ternary kernels (`PQ2_0`/`PTQ1_0` in the shared
  objects), then a real GGUF loaded on its `llama-server` with `/props`, a real turn and
  a `system_fingerprint` naming the release. Measured: install 1 s, model ready in 2 s
  on this machine, `/props` `n_ctx=4096`. The same model that answers in 24 tokens of
  `reasoning_content` with an empty `content` is why the turn accepts either field — the
  same trap the DeepSeek test documents.

**Never put an API key in a tracked file.** Pass it in the environment; the app itself
stores it through `internal/secret` (`~/.config/kichat/`). Measured facts worth keeping:
a rejected key comes back as **HTTP 422 with `code: "SUBSCRIPTION_TOKEN_INVALID"`** (not
401), and its free-text `detail` varies with the shape of the value sent — a key-shaped
one draws "The provided API key is invalid.", anything else "The provided subscription
token is invalid." So the **code** is what classifies it; matching on the wording misses
half of the failures and reports them as plain server errors (which is what the real
`websearch` failure that prompted this fix looked like). A used-up balance vs a momentary
throttle can likewise only be told apart by the envelope's `code` (`QUOTA_LIMITED` /
`RATE_LIMITED`), because both can arrive as 429.

**DuckDuckGo's HTML endpoints can be refused outright, and the refusal looks like an
empty page.** Measured here: both `html.duckduckgo.com/html/` and
`lite.duckduckgo.com/lite/` answered **HTTP 202** — not an error status — with the
anti-bot interstitial ("Unfortunately, bots use DuckDuckGo too… Select all squares
containing a duck") and no result nodes, for GET and POST, with a Firefox or a Chrome
user agent, with referer and language headers set. Nothing in the request shape changed
it: the check is on the client, so pacing or a HEAD/GET variation is not a workaround.
That is why a block is detected by the interstitial's own markup (`anomaly.js`,
`challenge-form`, `anomaly-modal`) and reported as `ErrBlocked`, and why the lite layout
is not retried after it; "no results (layout may have changed)" is reserved for a page
that really has no hits. A working Brave key, a different provider, or a different
network is the way out.

## The managed runtime (opt-in, fetches llama.cpp)

`internal/runtime` is the one package that installs software: it downloads
`llama-server` from a build source into `~/.config/kichat/runtime/<tag>/` and starts it
for a model the user picked. Since the ternary models arrived there are **two sources**,
upstream `ggml-org/llama.cpp` and PrismML's fork, selected in Settings → Backends (see
the subsection below). Nothing runs at startup unless a managed entry is switched on, and
no check downloads anything — the package's tests build their own archives and serve them
from an `httptest` server.

Measured while verifying it, in case a number is ever needed again:

- The Linux CPU asset is around **17.8 MB** whatever the build is — measured on
  `llama-b11534-bin-ubuntu-x64.tar.gz` (17,799,838 bytes, `sha256:d065dfb7…`) and again
  on `b11535` a day later, which is the point: the tag moves, the size does not. It
  unpacks to 50 files and 10 symlinks and its launcher answers
  `version: 0.6.0-dev (build 11534, commit 50e3e3e48)`. The largest desktop builds are
  ~590 MB, which is why the download has a progress row rather than a blocker.
- **GitHub's `latest` release carries no binaries.** Since llama.cpp moved to versioned
  tags, `latest` points at a semver release whose only asset is a `nightly-tag.txt`; the
  archives live on the per-build releases (`b11534`). The listing is therefore scanned for
  the first release that actually publishes the asset. The lookup is unauthenticated and
  therefore limited to 60 requests an hour per address: a 403 with
  `X-RateLimit-Remaining: 0` is reported as exactly that, not as "llama.cpp is down".
- **One release carries seven families, and the names are not a pattern you can
guess.** Read off `b11535` and confirmed on `b11537` two days later, which is the point —
  the families do not change, the tag does: `ubuntu-x64` / `ubuntu-arm64`, `ubuntu-vulkan-*`,
  `ubuntu-rocm-*-x64` (10.0 today), `ubuntu-cuda-*-x64` (12.8 and 13.4 at once),
  `ubuntu-sycl-fp16|fp32-x64`, `ubuntu-openvino-*-x64`, `win-cpu-x64`, `win-vulkan-*`,
  `win-rocm-*-x64`, `win-cuda-*-x64`, `win-sycl-x64`, `win-openvino-*-x64`,
  `win-opencl-adreno-arm64`, plus `macos-arm64` / `macos-x64` with no accelerator at all
  and `cudart-llama-…` bundles that are a different asset with the CUDA runtime folded
  in. ROCm, SYCL and OpenVINO are x64 only, so the picker is filled from
  `Distribution.PublishedAccelerators(goos, goarch)` — the *source's* own answer, since
  the two do not publish the same families — rather than from a list of its own.
  A family's suite is matched with a `*`, and the newest match wins. Every one of them was
  resolved against the live listing: CPU 17.8 MB, Vulkan 31.8, ROCm 244.9, CUDA 152.8,
  SYCL 55.5, OpenVINO 110.7 on linux/amd64, with the arm64 and macOS rows correctly
  offering fewer.
- **Adding a family or a source means touching lists in step, and a test holds them
together.** For a family: `config.Accelerators()`, `runtime.Accelerators()` and the
  `settings.managed.accel*` labels in all three dictionaries. For a source:
  `config.Distributions()`, `runtime.Distributions()` and the `settings.managed.dist*`
  labels. Both are checked by `accelerator_test.go` in the root package — a value that
  exists in one and not another is a build nobody can ask for, an option that renders as
  a raw id, or a stored setting that silently repairs itself to the default.
  `runtime.Accelerators()` stays the *union* of both sources' families, because it is the
  value space a settings blob may store; which of them a given source actually has is
  `Distribution.PublishedAccelerators`.
- **Every archive wraps its contents in its own directory** (`llama-b11528/`), and
  `llama-server` is a launcher whose code is in a sibling `libllama-server-impl.so`. The
  install flattens the wrapper and keeps the siblings — a build that lists models but
  cannot start is what the alternative produces.
- **A load is minutes, and readiness is the server's own answer.** `/health` is 503 while
  the weights come in and 200 once the server can serve.
- **The release tag is not the version, and asking the binary costs a process.** The
  panel shows what `llama-server --version` answers (`0.6.0-dev (build 11535, commit
  f39148a95)` for `b11535`), read once per path and again after a save, because the status
  it belongs to is polled every 1.2 s while a model loads. Probing one that hangs needs
  `exec.Cmd.WaitDelay`: killing the launcher does not kill a child it forked, and that
  child holds the pipe the output is collected through — without it the probe took 30 s.
- **A managed server has no API key and allows all origins**, which its own log points
  out. It binds to `127.0.0.1` only, so this is a local-origin exposure rather than a
  network one; it is recorded as a limitation in `PLAN.md` §8.
- **LM Studio's models folder is not where you think it is.** `~/.lmstudio/models`
  exists on this machine and is **empty**, while `~/.lmstudio/settings.json` has
  `downloadsFolder` pointing at `/mnt/<uuid>/lmstudio`, which holds six chat models —
  `publisher/model/file.gguf`, two or three levels deep, each model's `mmproj-*.gguf`
  projectors beside it. So the folder to offer first is the one LM Studio's own setting
  names, not the first candidate that exists; the helpers are in
  `internal/runtime/discover.go` and the whole thing is unit-tested against temporary
  homes.
- **An update check is a request, and a release tag is per release, not per family.**
  `CheckLlamaRuntimeUpdate` asks the release list for the newest build of the family the
  app installs and caches the reading; `GetRuntimeStatus` only reports the cached copy,
  because that status is polled every 1.2 s while a model loads and the API allows 60
  unauthenticated requests an hour per address. The comparison is between **build tags**
  (`b11535` against `b11541`), not between installation directories: every family of one
  release shares its tag, so a tag comparison cannot mistake a family switch for an
  update, while a directory comparison would flag `b11541-cpu` against `b11541-vulkan` on
  every check. It is numeric, because `b1000` sorts below `b999` as text, and a lookup
  that failed reports the API's reason rather than "current" — "up to date" is a claim
  this app is not entitled to make when it could not ask.
- **Nothing removes the build an update replaces, so the cleanup list has to.**
  `Installer.Builds` walks the runtime directory, measures each build (a symlink is
  counted as the link it is, or the versioned library beside it counts twice) and
  reports what an interrupted install left behind — the `<dir>.installing` staging
  directory, which no other code will ever clean up. `RemoveBuild` refuses a name that
  is not a single directory name and a directory with no launcher in it, and the App
  refuses the build in use and the build a *running* server was started from — the
  latter is why `Server` records the launcher it started (`Binary()`), since the status
  itself carries no path. A launcher the user supplied protects nothing: it lives
  outside the runtime directory, and `buildDirOf` says so rather than matching on a
  substring of the path.
- **A managed chat was measured, not assumed.** `LFM2.5-1.2B-Instruct-Q8_0.gguf`
  (1.25 GB, from the folder above) started under `b11535`, `/health` went 200, the
  provider listed it as `loaded` with a 4,096-token window, and a real turn came back in
  5.4 s with `prompt=33 completion=26` and llama.cpp's own timings (`6.2 tok/s`, ttft
  1366 ms, `source llamacpp`). Models llama.cpp cannot load at all are a different
  matter: the bundled `nomic-embed-text-v1.5` starts and answers `/health` but refuses
  `/v1/chat/completions` with "the current context does not support logits computation".
### The two distributions: upstream, and PrismML's fork

A source is not a flag. Its releases live at a different repository, its build tags have a
different shape, and its archives have different names and contents — so *which source*
is part of every question `internal/runtime` answers: what is the newest build, what is
installed here, is there an update. All of it is one value, `Distribution`, and the two
instances are the whole difference (plus a per-source table of families).

- **The fork is what runs a ternary model, and a mainline build cannot.**
  [PrismML-Eng/llama.cpp](https://github.com/PrismML-Eng/llama.cpp) is a fork of
  `ggml-org/llama.cpp` (not of `ik_llama.cpp`) tracking it closely, and the ternary
  kernels are the additions: its build carries the **fork-private GGML types `PQ2_0` (142)
  and `PTQ1_0` (143)**, which mainline refuses to load, so a ternary GGUF such as
  [prism-ml/Ternary-Bonsai-2-27B-gguf](https://huggingface.co/prism-ml/Ternary-Bonsai-2-27B-gguf)
  (`PTQ1_0` 5.95 GB, `PQ2_0` 7.21 GB, plus `mmproj` projectors) needs this build and not
  a flag on the other one. Measured on the installed CPU archive: `PQ2_0` appears in
  `libllama.so` **and** in every `libggml-cpu-*.so`, `PTQ1_0` only in `libllama.so` —
  which is what `forkKernels` in the live test looks for.
- **The tag scheme is the fork's own, and that is what broke the update row.** Its tags
  are `prism-b10770-6684606` — the prefix, the build number *and* a short commit — while
  upstream's are `b11541`. A helper that read a directory name as "everything before the
  first `-`" therefore reduced `prism-b10770-6684606-cpu` to `prism`; compared against the
  published `prism-b10770-6684606` that is "different, therefore newer", so the installed
  build was offered as an update over itself on every poll. `Distribution.TagOfDir`
  therefore strips the family and suite by looking for the source's *whole* tag shape from
  the right (`prism-b10770-6684606-cpu` → `prism-b10770-6684606`, `b11541-vulkan-10.0` →
  `b11541`), and a name of the other source reduces to no tag at all — which is what keeps
  a fork build out of an upstream comparison and the reverse. Pinned by
  `TestAForkBuildIsNotOfferedItsOwnBuildAsAnUpdate` and, through the real bound method,
  `TestCheckRuntimeUpdateReadsTheSelectedSource`.
- **The families differ, and so do the archive names.** Read off `prism-b10770-6684606`
  (26 assets, 2026-10-09): Linux CUDA is `linux-cuda-*-x64` where upstream says
  `ubuntu-cuda-*`; ROCm on Windows is `win-hip-radeon-x64` with no suite to match; there is
  **no OpenVINO build, no Adreno build, no Linux CUDA for arm64 and no Vulkan or SYCL for
  arm64 Windows**; macOS has an extra `macos-arm64-kleidiai`, which the exact
  `macos-arm64` row deliberately does not pick up. Sizes on linux/amd64: CPU 17.0 MB,
  Vulkan 34.2, ROCm 148.3, CUDA 175.3 (13.3) / 198.3 (12.8) / 311.3 (12.4), SYCL 38.5.
- **On Linux the fork publishes no CUDA runtime bundle, and its build needs one.** The
  archive is the build only: `ldd` on its extracted `libggml-cuda.so` answers
  `libcudart.so.13 => not found`, `libcublas.so.13 => not found`, `libcuda.so.1 => not
  found` — the same three upstream's CUDA build asks for. The fork's only
  `cudart-llama-…` assets are the **Windows** zips, so its Linux CUDA row has no
  companion to attach and a machine with the driver but no toolkit ends up with a
  backend that cannot load; the panel names exactly that, because `MissingLibraries`
  reads the loader's answer for the build directory. Attaching upstream's Linux bundle
  would cross both sources and suites, so it is documented rather than done — a size
  reading of 175–311 MB is *not* evidence that the runtime is inside, which is the
  mistake this bullet was written from. Windows keeps a companion (`win-cuda-13.3-x64`
  170.7 MB beside `cudart-llama-bin-win-cuda-13.3-x64.zip` 372.9 MB).
- **The ternary kernels are in the fork's GPU backends, not only its CPU one.** That is
  what makes offload worth anything for a ternary model — a backend without them would
  move the layers but not the maths. Measured on the extracted `prism-b10770-6684606`
  archives: Vulkan (`libggml-vulkan.so`, 58 MB) carries `dequant_pq2_0`,
  `matmul_pq2_0_f32_fp32_data`, `block_pq2_0_packed16`, `mul_mat_vec_id_ptq1_0_q8_1_f32_subgroup`
  and `get_rows_ptq1_0` — shader and kernel entry points, i.e. compiled kernels rather
  than a type name in a registry; CUDA 13.3 (`libggml-cuda.so`) 100 × `pq2_0`,
  259 × `ptq1_0` and 2 × `PQ2_0`; ROCm 7.2 (`libggml-hip.so`) 1718 × `pq2_0` and
  7178 × `ptq1_0`. Note the case: the *type names* are uppercase (`PQ2_0`) and appear in
  the CPU objects and in CUDA, while the Vulkan shaders and the HIP kernels name
  themselves lowercase — a grep for the uppercase spelling alone would report the
  Vulkan build as missing them.
- **GPU offload is a field on the managed model, and the reading is the server's own.**
  `-ngl, --gpu-layers, --n-gpu-layers` documents "either an exact number, 'auto', or
  'all' (default: auto)", with `--fit on` and a 1024 MiB target margin, so a GPU build
  offloads as much as fits without KiChat sending anything — the same for the fork as for
  upstream. A managed model carries a mode (`ManagedModel.Offload`): `auto` is not passed
  at all, `all` becomes `-ngl all`, and this app's word `off` becomes `-ngl 0`, because
  llama.cpp has no word for CPU only. An exact count stays a *further argument* (`-ngl
  24`), which is where a number only one model needs belongs, and it wins over the mode
  because the user's arguments go last on the command line. `-dev`, `-ts` and `-sm` live
  there too, and the device row, the free-memory warning and the ROCm library path
  (`serverEnv` adds `/opt/rocm/lib` for a build carrying `libggml-hip`) are
  source-agnostic. The mode list is spelled in both `config` and `runtime` (the settings
  package owns the stored value, the runtime package the flag value) and held together by
  `TestOffloadModesAgreeEverywhere` in the root package, which also requires a label for
  each mode in all three dictionaries.
- **The placement lines exist only at trace level, and only a named device makes them
  mean a GPU.** Measured on b11541 and on prism-b10770-6684606: at the default level
  (3, info) a load of a 9B model writes 24–28 lines and not one about a device, while
  `-lv 4` (trace) writes `llama_prepare_model_devices: using device Vulkan0 (…) (0000:0c:00.0)
  - 18678 MiB free` and `load_tensors: offloaded 34/34 layers to GPU`, and a real managed
  start against both sources reports `34/34 on Vulkan0` through `GetRuntimeStatus`. So a
  managed start asks for the trace level — but only for a launcher this app installed,
  because `-lv` is a flag an older binary a user points at may not know and an unknown
  argument stops the server rather than a report (`TestACustomLauncherIsNotAskedForTheTraceLevel`).
  A build this app downloaded always has it: the oldest upstream release GitHub still
  serves (`b11272`, `0.5.0-dev`) documents `-lv`, and nothing older than that could have
  been installed through this app. Two traps in reading it: **a CPU-only build prints
  `offloaded 65/65 layers to GPU` too** — measured on the fork's `ubuntu-x64` archive,
  whose only device is the host CPU and whose log never says `using device`, and on a GPU
  build started with `--device none`, which is the same shape — so the app requires a
  *named* device before it says a layer is on the GPU, and reports the CPU otherwise
  (`Offload.OnDevice`). The decision is made where the reading is parsed, not in the panel:
  `GetRuntimeStatus` sends the device id only when layers are really on it, and the panel
  reads "a name means the GPU, no name means the CPU". A `--gpu-layers 0` run is the case
  that needs both halves: it *does* name `Vulkan0` while reporting `0/65` layers, so the
  id is dropped. And the
  lines are written while the weights load, about 80 lines before the end of a 250-line
  log, while the ring holds 40 and the status returns the last 12, so the supervisor
  consumes them as they arrive (`ring.watch`) instead of reading the tail afterwards —
  and only from a server that is starting or running, because output can arrive after
  exec has reaped the process and a reading on a stopped row is a claim about a model
  nothing is loading. The status is cleared *before* the process is started, so a fast
  launcher cannot have its first lines wiped by the reset, and a start whose exec fails
  puts the row back to stopped rather than leaving a "loading" row the panel would poll
  forever (`TestLateOutputFromAStoppedServerIsIgnored`, `TestAFailedExecLeavesNoLoadingRow`).
- **A ternary model on the GPU was measured, and the CPU comparison is the point.**
  `Ternary-Bonsai-2-27B-PQ2_0.gguf` (6.71 GB, in the LM Studio folder's `prism-ml/`
  directory, whose `Ternary-Bonsai-2-27B-mmproj-BF16.gguf` the scanner skips) loaded through
  the app's own supervisor on the fork's `prism-b10770-6684606` builds, with `Offload: all`
  and the trace level the app sets: **Vulkan** reported 65/65 layers on `Vulkan0`, ready in
  4 s, `/props` `n_ctx=4096` and an 8,952-character template, a coherent answer and
  `system_fingerprint: b10770-6684606ac`, at **53.3 tok/s** — 11,087 MiB free while loaded;
  **ROCm 7.2** reported 65/65 on `ROCm0`, ready in 2 s, **57.0 tok/s**, 12,580 MiB free. The
  same file and the same Vulkan launcher with `--gpu-layers 0` (what the app's `off` mode
  sends) answered at **4.6 tok/s** and reported `0/65 on Vulkan0`. That gap is what proves
  the ternary kernels run *on the device*: a backend that had them only for the CPU would
  move the layers and still produce the 4.6 tok/s number. The reading itself arrives the way
  a managed start reports it in the panel — `On Vulkan0: 65 of 65 layers on the GPU`.
- **The fork's archive opens with a `./` entry, and that used to abort the install.**
  Its first tar entry is the extraction root itself (`.`), which `entryPath` refused with
  "Archiv-Eintrag ohne Namen" — upstream's archives carry no such entry, so nothing had
  found it. The root entry now resolves to the extraction root and is skipped, while an
  entry that resolves there and is *not* a directory is still refused. Found by the live
  test below on its first run, which is the argument for having one.
- **Installing the fork is verified end to end, not assumed.**
  `KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v` reads the fork's own
  release list, installs its CPU build (17.0 MB, 1 s), checks the launcher's
  `--version` against the tag it came from (`0.2.0-dev (build 10770, commit 6684606ac)`),
  loads a real GGUF on it, and asserts `/props` (`n_ctx=4096`, a 7,828-character chat
  template), a real turn and `system_fingerprint: b10770-6684606ac` — the running server
  naming the release it was built from. Everything it downloads is 17 MB and loadable in
  seconds, which is why this is cheap enough to keep.
- **Switching the source changes which launcher runs, and the app says so rather than
  substituting.** A managed start resolves the recorded build *only if it belongs to the
  selected source*, then falls back to the newest build of that source, and otherwise
  reports "llama-server ist noch nicht installiert" — it never runs the other llama.cpp.
  `TestTheSelectedSourceDecidesWhichBuildKiChatStarts` pins both directions: with the fork
  selected and only an upstream build on disk, `installed` is false and a start is
  refused; with a fork build on disk, that build is the one named and started (the version
  line proves it) even while the recorded tag still names upstream's. The two sources'
  builds sit side by side in one runtime folder and the cleanup list treats each on its
  own, so switching back and forth does not have to re-download.
- **A test can start a managed server without llama.cpp being installed.** The root
  package's `TestMain` re-executes the test binary as a stand-in launcher when
  `KICHAT_TEST_FAKE_LLAMA_SERVER=1` is in its environment: the script the tests write
  exports that variable plus the build number, and the child then answers `/health` 200,
  `--version` (`0.6.0-dev (build 11535, commit deadbeef0)`, the build its directory is
  named after) and `--list-devices`, and exits by itself after a minute so a test that
  dies without stopping it cannot leave a listener behind. That is what makes the guard
  that refuses to delete the build a *running* server was started from testable: the
  process and the readiness poll are real, and no python, netcat or downloaded archive
  is needed. Nothing takes that path unless the variable is set, and a test that skips
  on Windows (the launcher is a shell script) is the only user of it.

## Verifying the UI

### Preferred: the mock-bridge harness

The real app is not reachable as a web page — its UI is not served over HTTP, and
`window.go` is injected by the Go runtime into the webview, so opening `frontend/dist`
in a browser shows a bootstrap failure. `tools/uiharness` fixes that by serving the
built frontend with a mocked bridge:

```bash
make frontend                        # or: cd frontend && npm run build (needs node on PATH)
go run ./tools/uiharness             # http://127.0.0.1:8791/
```

Then open that loopback URL in the browser panel and drive it like any website: click,
type, read the accessibility tree, take screenshots, read the console. The page carries
a purple **MOCK BRIDGE** badge, so a harness page is never mistaken for the real app.

- `tools/uiharness/mock-wails.js` holds the fixtures: a chat whose assistant reply uses
  every markdown feature (headings, table, blockquote, highlighted code, inline and
  display math), a second chat with a seeded search message and a compaction, and
  streaming replies driven by real `chat:*` events over a simulated 40 ms cadence. The
  mock is `go:embed`ed, so a running harness picks up an edit to it only after a restart
  — use `go run`, or rebuild before restarting.
- Unmodelled bindings resolve **and log a warning** — if the console shows
  `unmodelled binding called: X`, the mock needs X when you exercise that path.
- `?crash=render` injects an unrenderable message to prove the error boundary still
  catches a render fault instead of leaving a blank window.
- `?update=available` (a newer build published than the one on disk), `?update=error` (a
  lookup that could not be made — a rate limit), `?update=stale` (a reading taken for
  another family, which the panel must not show as an answer about the installed one) and
  `?devices=`/`?fit=` with no flag (an installed build that is current) are what the
  update row is exercised with. All of them install a launcher first, so the row the
  update is about is actually there.
- `?dist=prism` selects the fork as the installed source: the build on disk is named
  `prism-b10770-6684606-cpu`, the tag column prints that whole tag rather than `prism`,
  the family list drops OpenVINO (the fork publishes five of upstream's six), and the
  update row is about `prism-…` releases. All of it installs a launcher first, so the
  rows that depend on one are actually there. Without the flag the fixture is upstream.
- Three shapes of the placement reading, because the device line is what makes the layer
  line mean a GPU: a full placement (`On Vulkan0: 34 of 34 layers on the GPU`), a
  `--gpu-layers 0` start under `?offload=cpu` (the same sentence, about the CPU), and —
  under `?devices=none`, which is a CPU-only build — every layer reported offloaded with
  no device named, which the panel must not present as a GPU. All three need the model
  started first: a stopped server reports no placement, the way the real row clears it.
- `?builds=old` is the cleanup list: a build an update replaced (31.8 MB) beside the
  staging directory of an interrupted download (612 MB, marked incomplete) and the build
  in use, which carries no remove button. Pressing `Update now` under `?update=available`
  produces the same shape on its own, because the mock's install leaves the replaced
  build in the list the way the real runtime directory does.
- It is a mock: it proves rendering, state flow and event wiring, **not** backend
  behaviour. Backend truth lives in the Go tests and the live tests above. What it
  cannot see is the *shape of an idle answer*: its fixture always has a managed
  llama.cpp model configured, so `GetRuntimeStatus` was only ever rendered with a
  populated `models` array — while a real fresh install sent `null` (a nil Go slice
  marshals to `null`, not `[]`) and Settings → Backends died on `.find` of null. When
  adding a binding that returns a slice, render the empty case in the harness too, or
  pin the JSON in a Go test the way `runtime_status_test.go` does.

### When the real window is needed

`xdotool`/`import` against a native Wails window is possible but full of traps. What
was actually measured here:

- **Run the app detached**: `setsid nohup ./build/bin/kichat >/tmp/kichat.log 2>&1 </dev/null &`.
  A plain background launch dies with `Ctrl+C detected. Shutting down...` whenever a
  tool call times out, because the timeout kills the whole process group. That looks
  exactly like a crash — check the log before believing it.
- **Window coordinates**: use `xwininfo -id <id>`'s *Absolute upper-left* as the origin of
  the content (verified by a pixel-exact comparison: cropping the screen at
  `1440x900+1200+662` equals an `import -window <id>` capture, RMSE 0). `xdotool
  getwindowgeometry` and `mousemove --window` are off by the frame extents (10 px, 40 px)
  here, so aim with absolute coordinates: `xdotool mousemove $((1200+x)) $((662+y))`.
  Window decoration may not be where X claims it is — verify a click by its effect.
- **Capture** with `import -window <id>` for an occlusion-free image of the app. Wrap it
  in `timeout`: if the window is gone, `import` **blocks forever** waiting for it. The
  root capture (`import -window root`) is the composited screen, useful for locating a
  window but cluttered by other applications.
- **Synthetic input partially works**: sidebar clicks landed, clicks in the main area did
  not, with the app confirmed as `_NET_ACTIVE_WINDOW`. Also `xdotool windowactivate --sync`
  can block under focus-stealing prevention. Do not aim near "Dateien hinzufügen": a
  miss opens a modal GTK file dialog that swallows all further input.
- Prefer the harness, or keyboard navigation, for anything but a final look.

### Pinning the streaming renderer

`node tools/streamcheck/check.mjs` (Node 22.18+, for its TypeScript support; needs
`cd frontend && npm install` first) renders every prefix of four sample replies twice —
once the way a finished message is rendered, once the way an arriving one is — and fails
if the visible text differs, if the pieces `frontend/src/lib/stream.ts` cuts would change
as the text grows (the memo depends on their being append-only), or if a line is lost in
the split. It imports the real module directly, runs in under a second, and is **not**
part of `make check`.

The numbers behind that shape, from the parser and from the real UI. With this app's
pipeline, re-parsing the whole reply on every delta and on every throttle commit cost
237 ms of parsing for a 4 kB answer, 1.7 s for a 12 kB one and 6 s for a 24 kB one
(`node` + `react-dom/server` over the real plugins). Driving the mock-bridge harness in
headless Chromium over CDP, with the fixture reply enlarged to ~5 kB so the run is worth
measuring, one streamed reply tore down and rebuilt 4511 elements and rewrote the message
9576 times — that is the stutter and the flicker, while the rest of the window stays still.
Parsing each settled piece once, throttling the live piece and showing an open code fence
as plain text brings the same three replies down to 46, 137 and 266 ms, a 45 kB code-only
answer from 6.7 s of parsing to a single parse, and the harness reply to 256 remounts over
887 rewrites.

## Versioning

The version is authored once, in `VERSION` at the repo root, and every surface is
derived from it: `make build` links it into `internal/version` (read by Settings →
General → About and by `kichat --version`), and `make deb` uses it for the package
name, `control` and `changelog`. Bumping a release is one line in `VERSION`;
`VERSION=x.y.z` on the make command line overrides it for a one-off.

The commit and date linked in come from `git`, and the date is the *commit's* date,
never the build clock — a wall-clock timestamp would break the byte-identical
rebuilds `-trimpath` otherwise guarantees. Two traps:

- **The linker path is the contract.** `-X kichat/internal/version.Version=…` in the
  Makefile stops matching the moment that package or a variable is renamed, and every
  build then silently reports `dev`. Keep the two in step.
- **A pre-release needs Debian's `~`.** Write `VERSION` as `0.2.0~beta1`, not
  `0.2.0-beta.1`: `~` sorts *before* the release (`0.2.0~beta1 < 0.2.0`), while the
  dash form sorts *after* it, so `apt upgrade` would happily move a pre-release onto
  the older-looking final.

## Git commits

The subject is `area: description` — lowercase, imperative, and it describes
the behaviour change, not the files. The areas in use so far: `chat`, `llm`,
`ui`, `research`, `docs`, `store`, `build`, `config`, `api`, `settings`,
`secret`, `ingest`, `tools`, `icon` — a new area is fine when the change
belongs nowhere else. A second effect joins with "and":

```
chat: make compaction work with a reasoning model
research: report a refused search, and keep tests out of the keyring
icon: draw the mark once, derive every surface from it, and make the taskbar one show
```

The body is prose, not a changelog: the failure or limitation that motivated
the change, what was measured, what changed and why, and which checks stayed
green. Numbers go in where they exist (237 ms of parsing → 266 ms, HTTP 422
with `SUBSCRIPTION_TOKEN_INVALID`); a body that only lists the files is a
stub. One commit is one coherent change, and the files are staged explicitly,
not with `git add -A`.

**Always `--no-pager`.** `git log`, `git diff` and `git show` open a pager,
and in a non-interactive shell the pager blocks the command until someone
presses a key: it looks hung, and its output lands in a temp file instead of
the result. `git --no-pager log -1` and friends are the same commands without
the hang.

## Conventions

- Comments explain **why**, not what; user-facing strings go through
  `frontend/src/lib/i18n.ts` (de and en) — a hardcoded label is a bug.
- No compile suppressions, no unused dependencies, no swallowed errors. If a test needs
  weakening, say so out loud instead of doing it quietly.
- New settings fields need no migration: the settings blob is one JSON row decoded over a
  fully populated default, so absent fields keep their default (pinned by
  `internal/config/config_test.go`). Do not change an existing field's type.
- Tests pin behaviour (what the user or the model sees), not implementation details.
- **A test must never reach the real keyring or the real `~/.config/kichat` data.** The
  keyring is a per-user global resource and `secret.New` probes it, so a test store built
  with `New` wrote its fixtures into the keyring of whoever ran the suite: a stored Brave
  key became the string `brave-key`, the app read that back at startup and every search
  came back as a rejected key (422 `SUBSCRIPTION_TOKEN_INVALID`). Tests use
  `secret.NewIsolated(dir)`, which keeps every secret in the temp dir and never probes.
- `.freebuff/` is gitignored scratch space; `frontend/dist`, `build/bin` and `node_modules`
  are build output.
