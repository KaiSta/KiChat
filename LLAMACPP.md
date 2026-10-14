# LLAMACPP.md

Everything KiChat knows about llama.cpp: the provider that reads its metadata, the runtime
that installs and runs `llama-server`, the two release sources including PrismML's fork,
and the ternary models that need it. `PLAN.md` names the feature in one line; this document
is the detail behind it. It is a sibling of `MISTRAL.md` and `DEEPSEEK.md`, which do the
same for the two hosted services.

Everything here was read out of the code and, where marked *measured*, observed on this
machine against real builds. Claims that are design intent rather than verified behaviour
say so.

**Contents:** 1 two halves under one name · 2 the provider · 3 the managed runtime ·
4 ternary models on the GPU · 5 where the code is · 6 how this is verified · 7 known gaps ·
8 deliberately not done.

The code references are `file:line` for the revision this was written against. The symbol
names beside them (`enrichLlamaCpp`, `Distribution`, `Installer.Builds`, `ring.watch`, …)
are the durable anchor when the numbers drift.

---

## 1. Two halves under one name

`llamacpp` names two different things, and the settings entry is where they meet:

- **A server you point KiChat at.** `llama-server` is an OpenAI-compatible HTTP server, so
  the generic `custom` kind could already reach it. It could not reach its *metadata*,
  and this provider exists for that (§2).
- **A server KiChat installs and runs.** `internal/runtime` downloads `llama-server` into
  `~/.config/kichat/runtime/<tag>/` and starts it with a GGUF the user picked (§3). It is
  the one package in the app that installs software.

The two are joined by the address: when a managed server comes up, `app.go` writes the URL
it landed on into the `llamacpp` backend entry, enables it, and rebuilds the providers —
which is why a row reading "connection refused" becomes reachable and its model turns up in
the switcher. Nothing runs at startup unless a managed entry is switched on, and **no check
downloads anything**: the package's tests build their own archives and serve them from an
`httptest` server.

---

## 2. The provider

### 2.1 Why a provider of its own

The generic OpenAI-compatible path is deliberately metadata-blind, and two things the app
does depend on metadata it cannot get there: how large the context window actually is
(which decides when compaction has to run) and whether the model can be handed tools and
images. llama.cpp publishes both in a namespace of its own.

Every shape below was read off llama.cpp's server documentation (`tools/server/README.md`)
rather than guessed; the field names are quoted.

### 2.2 `/props` is the centre of gravity

```
GET /props
  "default_generation_settings": { "n_ctx": 1024, ... }
  "chat_template": "...", "chat_template_caps": {...}
  "modalities": { "vision": false }, "model_path": "...", "build_info": "..."
```

`n_ctx` is the window the model is loaded **with** — the number a prompt has to fit in —
and `/v1/models` cannot report it: it reports `meta.n_ctx_train`, the window the model was
trained *for*. The difference is not academic. `-c, --ctx-size N` documents its default as
`0, 0 = loaded from model`, so the loaded window is neither known client-side nor derivable
from the GGUF. Only the server can say it.

Reading `n_ctx_train` and calling it the context limit would promise a window the server
refuses, and the backend would then truncate the prompt **from the front** — losing the
system prompt. So:

| source | fills | why |
| --- | --- | --- |
| `/v1/models` `meta.n_ctx_train` | `MaxContext` only | the ceiling, not the loaded window |
| `/props` `n_ctx` | `ContextLimit` | the window the server actually allocated |
| neither | the app's own conservative default | the safe way round |

`propsFor` ([internal/llm/llamacpp.go:346](internal/llm/llamacpp.go#L346)) caches the
reading, and treats the two failure shapes differently: a **404 is remembered**, because a
build without `/props` will not grow one, while **any other failure is not**, so a server
that was merely still loading its model is asked again instead of being written off for the
rest of the session.

### 2.3 Which row a reading belongs to

This is the part that is easy to get wrong, and it is **not the same question** for the two
shapes of server this kind reaches. `enrichLlamaCpp`
([internal/llm/llamacpp.go:237](internal/llm/llamacpp.go#L237)) splits them:

- **A direct server** (`llama-server -m model.gguf`) serves exactly one model, does not
  answer before its weights are resident, and loads it with the window `/props` reports.
  That single row is loaded and gets the window.
- **A router** (`--models-dir`) serves every model it discovered and loads them on demand,
  up to `--models-max` at once. Several rows therefore do **not** mean all of them are
  loaded, and `/props` describes only the instance it names in `model_path`. Handing that
  one window to every row is exactly how a model gets promised a window the server will
  refuse, so with more than one row only the row `/props` names is marked loaded and only
  that row gets the window; the rest fall back to the conservative default.

`llamaCppLoadedIndex` ([internal/llm/llamacpp.go:310](internal/llm/llamacpp.go#L310)) names
that row: the id is compared first and the file name second, because a row's id *is* the
model path unless `--alias` replaced it. An alias therefore matches neither and leaves the
reading unused — a conservative default rather than one belonging to a different model.
`modelFileBase` splits on both separators, because a server on Windows reports
`C:\models\x.gguf` while the same model over a network share arrives as `/models/x.gguf`.

Failing to read `/props` never fails the listing: the models are what the caller asked for
and everything here is an enrichment.

### 2.4 Tools: absent is not false

`chat_template_caps` is decoded as a **map**, and that is the whole point.
llama.cpp's documentation shows the field as `{}` for a template it could not analyse;
decoding that into a struct would turn "the server did not say" into Go's `false` and
silently withdraw tool support from a server that never denied it.

So only an explicit `false` withholds tools:

- an absent key means the server did not report on the template, and llama.cpp's own
  default for the field is true;
- the app answers a missing tool claim by never using the native protocol, so guessing
  **no** would cost a working model its tools silently, while guessing **yes** costs
  nothing when it is wrong — such a model simply answers in prose.

One assumption worth recording, because it is the opposite of the obvious one: tools do
**not** need `--jinja`. The flag reads `--jinja, --no-jinja (default: enabled)`, so a
current build parses the model's chat template by default, and `chat_template_caps` is how
it reports what that template can do.

Vision is read from **both** sources — `architecture.input_modalities` in the listing and
`modalities.vision` in `/props` — because either may be the one the build in front of us
fills in. `withCap` appends without duplicating, since two sources reporting one capability
would otherwise show it twice in the UI.

### 2.5 Thinking goes through the chat template

llama.cpp has no `enable_thinking` field of its own; it hands the parameter to the chat
template via `chat_template_kwargs`, which is why the toggle is nested rather than
top-level (`llamaCppThinkingOff`). And it is **guarded**: llama.cpp validates the parameters
a request hands to a template, so offering one the template never reads is a rejected
request rather than an ignored extra — a hard-coded toggle would break every model whose
template lacks it. `llamaCppTemplateTogglesThinking`
([internal/llm/llamacpp.go:390](internal/llm/llamacpp.go#L390)) reads the template source and
looks for the parameter's own name, which a template that honours it has to mention.

A server that cannot be asked answers **false**, so the request goes out without the
parameter rather than with one the template may reject: an unreachable `/props` costs the
"do not think" optimisation, not the whole turn. Compaction adapts its output budget for
exactly this case.

### 2.6 The address: 9931

`LlamaCppDefaultBaseURL` ([internal/llm/llamacpp.go:63](internal/llm/llamacpp.go#L63)) is
`http://127.0.0.1:9931`, because `--host` defaults to `127.0.0.1` and `--port` to 9931.

The port was 8080 for years and moved in the b10240 build, to a number whose neighbours on a
developer machine are far less busy. A build from before that still answers on 8080, and the
Docker instructions still map 8080 — but the address the entry offers is the one a current
server actually listens on, because a stale default is what a user sees first: enable the
entry, ask for the model list, and get a connection error from a port nothing was ever going
to answer on. Correcting the address is the user's call, not a hurdle to get past before the
entry can work at all.

The chat paths are built rather than appended, which is a bug this file records: a base URL
that already names `/v1` would otherwise produce `/v1/v1/chat/completions` as the **first**
attempt — a request that is certain to 404, sent on the first message of every session, with
the fallback quietly hiding it afterwards. `/props` and `/v1/models` hang off the server
root, never off a `/v1` base, so the prefix is stripped for those.

### 2.7 Timings: the badge names the backend

The final frame of a chat completion carries a `timings` object, measured by the server, so
the speed badge can say **llama.cpp** instead of admitting to an estimate. `prompt_tokens`
already counts the cached part of the prompt, which is what `timings.cache_n` describes, so
it needs no mapping of its own into the usage object. `SourceLlamaCpp` is what the UI reads.

---

## 3. The managed runtime

### 3.1 What it installs

`internal/runtime` downloads `llama-server` from a build source into
`~/.config/kichat/runtime/<tag>/` and starts it for a model the user picked. Since the
ternary models arrived there are **two sources**, upstream `ggml-org/llama.cpp` and
PrismML's fork, selected in Settings → Backends (§3.3).

Measured while verifying it, in case a number is ever needed again:

- The Linux CPU asset is around **17.8 MB** whatever the build is — measured on
  `llama-b11534-bin-ubuntu-x64.tar.gz` (17,799,838 bytes, `sha256:d065dfb7…`) and again on
  `b11535` a day later, which is the point: the tag moves, the size does not. It unpacks to
  **50 files and 10 symlinks** and its launcher answers
  `version: 0.6.0-dev (build 11534, commit 50e3e3e48)`. The largest desktop builds are
  ~590 MB, which is why the download has a progress row rather than a blocker.
- **GitHub's `latest` release carries no binaries.** Since llama.cpp moved to versioned
  tags, `latest` points at a semver release whose only asset is a `nightly-tag.txt`; the
  archives live on the per-build releases (`b11534`). The listing is therefore scanned for
  the first release that actually publishes the asset. The lookup is unauthenticated and
  therefore limited to **60 requests an hour per address**: a 403 with
  `X-RateLimit-Remaining: 0` is reported as exactly that, not as "llama.cpp is down".
- **Every archive wraps its contents in its own directory** (`llama-b11528/`), and
  `llama-server` is a launcher whose code is in a sibling `libllama-server-impl.so`. The
  install flattens the wrapper and **keeps the siblings** — a build that lists models but
  cannot start is what the alternative produces. `launcherUnder`
  ([internal/runtime/install.go:463](internal/runtime/install.go#L463)) is how the launcher
  is found, and it is also what the cleanup list and `RemoveBuild` use to decide a directory
  is a build at all.

### 3.2 The families, and why the picker is not a hard-coded list

**One release carries seven families, and the names are not a pattern you can guess.** Read
off `b11535` and confirmed on `b11537` two days later, which is the point — the families do
not change, the tag does:

```
ubuntu-x64 / ubuntu-arm64        win-cpu-x64
ubuntu-vulkan-*                  win-vulkan-*
ubuntu-rocm-*-x64 (10.0 today)   win-rocm-*-x64
ubuntu-cuda-*-x64 (12.8 & 13.4)  win-cuda-*-x64
ubuntu-sycl-fp16|fp32-x64        win-sycl-x64
ubuntu-openvino-*-x64            win-openvino-*-x64
                                 win-opencl-adreno-arm64
macos-arm64 / macos-x64 (no accelerator)
cudart-llama-… (a different asset, with the CUDA runtime folded in)
```

ROCm, SYCL and OpenVINO are **x64 only**, so the picker is filled from
`Distribution.PublishedAccelerators(goos, goarch)`
([internal/runtime/distribution.go:147](internal/runtime/distribution.go#L147)) — the
*source's* own answer, since the two do not publish the same families — rather than from a
list of its own. A family's suite is matched with a `*`, and the newest match wins.

Every one of them was resolved against the live listing (linux/amd64): **CPU 17.8 MB,
Vulkan 31.8, ROCm 244.9, CUDA 152.8, SYCL 55.5, OpenVINO 110.7**, with the arm64 and macOS
rows correctly offering fewer.

**Adding a family or a source means touching lists in step, and a test holds them
together.** For a family: `config.Accelerators()`, `runtime.Accelerators()` and the
`settings.managed.accel*` labels in all three dictionaries. For a source:
`config.Distributions()`, `runtime.Distributions()` and the `settings.managed.dist*`
labels. Both are checked by `accelerator_test.go` in the root package — a value that exists
in one and not another is a build nobody can ask for, an option that renders as a raw id, or
a stored setting that silently repairs itself to the default.

`runtime.Accelerators()` stays the **union** of both sources' families, because it is the
value space a settings blob may store; which of them a given source actually has is
`PublishedAccelerators`.

### 3.3 The two distributions: upstream, and PrismML's fork

A source is not a flag. Its releases live at a different repository, its build tags have a
different shape, and its archives have different names and contents — so *which source* is
part of every question `internal/runtime` answers: what is the newest build, what is
installed here, is there an update. All of it is one value, `Distribution`
([internal/runtime/distribution.go:71](internal/runtime/distribution.go#L71)), and the two
instances are the whole difference (plus a per-source table of families).

- **The fork is what runs a ternary model, and a mainline build cannot.**
  [PrismML-Eng/llama.cpp](https://github.com/PrismML-Eng/llama.cpp) is a fork of
  `ggml-org/llama.cpp` (not of `ik_llama.cpp`) tracking it closely, and the ternary kernels
  are the additions: its build carries the **fork-private GGML types `PQ2_0` (142) and
  `PTQ1_0` (143)**, which mainline refuses to load, so a ternary GGUF such as
  [prism-ml/Ternary-Bonsai-2-27B-gguf](https://huggingface.co/prism-ml/Ternary-Bonsai-2-27B-gguf)
  (`PTQ1_0` 5.95 GB, `PQ2_0` 7.21 GB, plus `mmproj` projectors) needs this build and not a
  flag on the other one. Measured on the installed CPU archive: `PQ2_0` appears in
  `libllama.so` **and** in every `libggml-cpu-*.so`, `PTQ1_0` only in `libllama.so` — which
  is what `forkKernels` in the live test looks for.
- **The families differ, and so do the archive names.** Read off `prism-b10770-6684606`
  (26 assets, 2026-10-09): Linux CUDA is `linux-cuda-*-x64` where upstream says
  `ubuntu-cuda-*`; ROCm on Windows is `win-hip-radeon-x64` with no suite to match; there is
  **no OpenVINO build, no Adreno build, no Linux CUDA for arm64 and no Vulkan or SYCL for
  arm64 Windows**; macOS has an extra `macos-arm64-kleidiai`, which the exact `macos-arm64`
  row deliberately does not pick up. Sizes on linux/amd64: CPU 17.0 MB, Vulkan 34.2,
  ROCm 148.3, CUDA 175.3 (13.3) / 198.3 (12.8) / 311.3 (12.4), SYCL 38.5.
- **On Linux the fork publishes no CUDA runtime bundle, and its build needs one.** The
  archive is the build only: `ldd` on its extracted `libggml-cuda.so` answers
  `libcudart.so.13 => not found`, `libcublas.so.13 => not found`, `libcuda.so.1 => not
  found` — the same three upstream's CUDA build asks for. The fork's only `cudart-llama-…`
  assets are the **Windows** zips, so its Linux CUDA row has no companion to attach and a
  machine with the driver but no toolkit ends up with a backend that cannot load; the panel
  names exactly that, because `MissingLibraries`
  ([internal/runtime/libs.go:69](internal/runtime/libs.go#L69)) reads the loader's answer for
  the build directory. Attaching upstream's Linux bundle would cross both sources and
  suites, so it is documented rather than done — a size reading of 175–311 MB is *not*
  evidence that the runtime is inside, which is the mistake this bullet was written from.
  Windows keeps a companion (`win-cuda-13.3-x64` 170.7 MB beside
  `cudart-llama-bin-win-cuda-13.3-x64.zip` 372.9 MB).
- **The fork's archive opens with a `./` entry, and that used to abort the install.** Its
  first tar entry is the extraction root itself (`.`), which `entryPath`
  ([internal/runtime/install.go:1128](internal/runtime/install.go#L1128)) refused with
  "Archiv-Eintrag ohne Namen" — upstream's archives carry no such entry, so nothing had
  found it. The root entry now resolves to the extraction root and is skipped, while an
  entry that resolves there and is *not* a directory is still refused. Found by the live
  test on its first run, which is the argument for having one.

### 3.4 Tags, and the update check

**The tag scheme is the fork's own, and that is what broke the update row.** Its tags are
`prism-b10770-6684606` — the prefix, the build number *and* a short commit — while
upstream's are `b11541`. A helper that read a directory name as "everything before the first
`-`" therefore reduced `prism-b10770-6684606-cpu` to `prism`; compared against the published
`prism-b10770-6684606` that is "different, therefore newer", so the installed build was
offered as an update over itself on every poll.

`Distribution.TagOfDir` ([internal/runtime/distribution.go:250](internal/runtime/distribution.go#L250))
therefore strips the family and suite by looking for the source's *whole* tag shape from the
right (`prism-b10770-6684606-cpu` → `prism-b10770-6684606`, `b11541-vulkan-10.0` →
`b11541`), and a name of the other source reduces to no tag at all — which is what keeps a
fork build out of an upstream comparison and the reverse. Pinned by
`TestAForkBuildIsNotOfferedItsOwnBuildAsAnUpdate` and, through the real bound method,
`TestCheckRuntimeUpdateReadsTheSelectedSource`.

**An update check is a request, and a release tag is per release, not per family.**
`CheckLlamaRuntimeUpdate` ([api.go:473](api.go#L473)) asks the release list for the newest
build of the family the app installs and caches the reading; `GetRuntimeStatus`
([api.go:820](api.go#L820)) only reports the cached copy, because that status is polled every
**1.2 s** while a model loads and the API allows 60 unauthenticated requests an hour per
address.

The comparison is between **build tags** (`b11535` against `b11541`), not between
installation directories: every family of one release shares its tag, so a tag comparison
cannot mistake a family switch for an update, while a directory comparison would flag
`b11541-cpu` against `b11541-vulkan` on every check. It is **numeric**, because `b1000`
sorts below `b999` as text. And a lookup that failed reports the API's reason rather than
"current" — "up to date" is a claim this app is not entitled to make when it could not ask.

### 3.5 Starting a model

- **A load is minutes, and readiness is the server's own answer.** `/health` is 503 while
  the weights come in and 200 once the server can serve.
- **The release tag is not the version, and asking the binary costs a process.** The panel
  shows what `llama-server --version` answers (`0.6.0-dev (build 11535, commit f39148a95)`
  for `b11535`), read once per path and again after a save, because the status it belongs to
  is polled every 1.2 s while a model loads. `BinaryVersion`
  ([internal/runtime/version.go:26](internal/runtime/version.go#L26)) carries a deadline
  because a launcher that hangs must not hold a status call open, and a build that prints its
  version and exits non-zero still counts: what was asked for arrived. `parseVersionOutput`
  **searches** rather than reading the first line, because llama.cpp writes other things
  first (a build warning, the device list, a note about the missing API key), and it keeps
  the whole line rather than just the number, because the build and commit in it are exactly
  what makes two installations of "0.6.0-dev" distinguishable.
- **Probing one that hangs needs `exec.Cmd.WaitDelay`**: cancelling the context kills the
  launcher but not a child it forked, and that child inherits the pipe the output is
  collected through, so `Wait` would sit there until it exits. Measured with a launcher that
  sleeps: without it the probe returned after **30 s**.
- The probe runs with the same environment a server is started with (`serverEnv`,
  [internal/runtime/gpu.go:407](internal/runtime/gpu.go#L407)), because a HIP build cannot
  load its backend without the ROCm library path and a probe that answered "no version" would
  be reading its own environment rather than the build's. `serverEnv` adds `/opt/rocm/lib`
  for a build carrying `libggml-hip`.

### 3.6 Placement, and why the trace level is asked for

**The placement lines exist only at trace level, and only a named device makes them mean a
GPU.** Measured on `b11541` and on `prism-b10770-6684606`: at the default level (3, info) a
load of a 9B model writes 24–28 lines and **not one about a device**, while `-lv 4` (trace)
writes

```
llama_prepare_model_devices: using device Vulkan0 (…) (0000:0c:00.0) - 18678 MiB free
load_tensors: offloaded 34/34 layers to GPU
```

and a real managed start against both sources reports `34/34 on Vulkan0` through
`GetRuntimeStatus`. So a managed start asks for the trace level — but **only for a launcher
this app installed**, because `-lv` is a flag an older binary a user points at may not know
and an unknown argument stops the server rather than a report
(`TestACustomLauncherIsNotAskedForTheTraceLevel`). A build this app downloaded always has
it: the oldest upstream release GitHub still serves (`b11272`, `0.5.0-dev`) documents `-lv`,
and nothing older than that could have been installed through this app.

**Two traps in reading it.** A CPU-only build prints `offloaded 65/65 layers to GPU` too —
measured on the fork's `ubuntu-x64` archive, whose only device is the host CPU and whose log
never says `using device`, and on a GPU build started with `--device none`, which is the same
shape. So the app requires a **named** device before it says a layer is on the GPU, and
reports the CPU otherwise (`Offload.OnDevice`). The decision is made where the reading is
parsed, not in the panel: `GetRuntimeStatus` sends the device id only when layers are really
on it, and the panel reads "a name means the GPU, no name means the CPU". A
`--gpu-layers 0` run is the case that needs both halves: it *does* name `Vulkan0` while
reporting `0/65` layers, so the id is dropped.

**The lines are written while the weights load**, about 80 lines before the end of a 250-line
log, while the ring holds 40 and the status returns the last 12 — so the supervisor consumes
them as they arrive (the ring's `watch` hook, wired to `Server.observeOffload`
([internal/runtime/supervise.go:455](internal/runtime/supervise.go#L455))) instead of
reading the tail afterwards, and only from a
server that is starting or running, because output can arrive after exec has reaped the
process and a reading on a stopped row is a claim about a model nothing is loading. The
status is cleared **before** the process is started, so a fast launcher cannot have its first
lines wiped by the reset, and a start whose exec fails puts the row back to stopped rather
than leaving a "loading" row the panel would poll forever
(`TestLateOutputFromAStoppedServerIsIgnored`, `TestAFailedExecLeavesNoLoadingRow`).

### 3.7 GPU offload

`-ngl, --gpu-layers, --n-gpu-layers` documents "either an exact number, 'auto', or 'all'
(default: auto)", with `--fit on` and a 1024 MiB target margin, so a GPU build offloads as
much as fits without KiChat sending anything — the same for the fork as for upstream.

A managed model carries a mode (`ManagedModel.Offload`):

| mode | what is sent |
| --- | --- |
| `auto` | nothing at all — the server's own default is the right answer |
| `all` | `-ngl all` |
| `off` | `-ngl 0`, because llama.cpp has no word for CPU only |

An exact count stays a *further argument* (`-ngl 24`), which is where a number only one
model needs belongs, and it wins over the mode because the user's arguments go last on the
command line. `-dev`, `-ts` and `-sm` live there too, and the device row, the free-memory
warning and the ROCm library path are source-agnostic.

The mode list is spelled in both `config` and `runtime` (the settings package owns the
stored value, the runtime package the flag value) and held together by
`TestOffloadModesAgreeEverywhere` in the root package, which also requires a label for each
mode in all three dictionaries.

### 3.8 Cleanup

**Nothing removes the build an update replaces, so the cleanup list has to.**
`Installer.Builds` ([internal/runtime/builds.go:47](internal/runtime/builds.go#L47)) walks
the runtime directory, measures each build (a symlink is counted as the link it is, or the
versioned library beside it counts twice) and reports what an interrupted install left behind
— the `<dir>.installing` staging directory, which no other code will ever clean up.

`RemoveBuild` ([internal/runtime/builds.go:118](internal/runtime/builds.go#L118)) refuses a
name that is not a single directory name and a directory with no launcher in it, and the App
refuses the build in use and the build a *running* server was started from — the latter is
why `Server` records the launcher it started
([internal/runtime/supervise.go:471](internal/runtime/supervise.go#L471)), since the status
itself carries no path. A launcher the user supplied protects nothing: it lives outside the
runtime directory, and `buildDirOf` says so rather than matching on a substring of the path.

### 3.9 Which binary runs

**Switching the source changes which launcher runs, and the app says so rather than
substituting.** A managed start resolves the recorded build *only if it belongs to the
selected source*, then falls back to the newest build of that source, and otherwise reports
"llama-server ist noch nicht installiert" — it never runs the other llama.cpp.

`TestTheSelectedSourceDecidesWhichBuildKiChatStarts` pins both directions: with the fork
selected and only an upstream build on disk, `installed` is false and a start is refused;
with a fork build on disk, that build is the one named and started (the version line proves
it) even while the recorded tag still names upstream's. The two sources' builds sit side by
side in one runtime folder and the cleanup list treats each on its own, so switching back and
forth does not have to re-download.

### 3.10 Where the GGUF comes from

**LM Studio's models folder is not where you think it is.** `~/.lmstudio/models` exists on
this machine and is **empty**, while `~/.lmstudio/settings.json` has `downloadsFolder`
pointing at `/mnt/<uuid>/lmstudio`, which holds six chat models —
`publisher/model/file.gguf`, two or three levels deep, each model's `mmproj-*.gguf`
projectors beside it.

So the folder to offer first is the one LM Studio's own setting names, not the first
candidate that exists. `LMStudioFolders`
([internal/runtime/discover.go:80](internal/runtime/discover.go#L80)) returns them in order
and `ScanModelFolder` ([internal/runtime/discover.go:155](internal/runtime/discover.go#L155))
reads one; the whole thing is unit-tested against temporary homes.

**And one remembered folder is not enough to make the choice cheap.** A user with LM Studio's
downloads on one drive and a library of their own on another — the ternary models the fork
exists for, say — had to walk the folder dialog again to get back to the first one, so the row
keeps the folders it was pointed at instead: `modelDirs` in the settings blob, most recently
picked first, drawn as one chip per folder, with the folder in use always one of them (it is
added at the head when a list arrives without it, which is what a blob written before the list
existed carries). `NormaliseModelDirs`
([internal/config/config.go:641](internal/config/config.go#L641)) trims, drops duplicates and
bounds the list at eight, and is run both on load and in `SaveSettings` — a path typed straight
into the text box never went through the picker, and the store the row reads back has to
carry it. `ModelFolders` ([api.go:1140](api.go#L1140)) then answers with those folders first
and LM Studio's after, one entry per path, and a fourth place models can come from is now
reachable in one click rather than a dialog.

Two smaller consequences of the same shape:

- **An empty field falls back to the remembered library**, not to LM Studio's directory
  (`defaultModelDir`, [api.go:1210](api.go#L1210)): the panel's placeholder names that same
  folder, so scanning somewhere else would list one library under another one's path.
- **A folder can be forgotten**, and forgetting means the *list*, not the disk. The chips are
  drawn from the draft's list rather than from the backend's answer — a chip re-derived from
  the store's copy came straight back the moment it was clicked away, which reads as the
  button not working — while the place it came from, LM Studio's own folders, is detected
  rather than stored and so has no chip to remove.

**And the listing marks the file that is *in use*, not merely the one that is on disk.** A row
carries an `in use` badge — `loading` while the model is still coming in — when the running
`llama-server` was started with that file. The path is the one the supervisor keeps in
`rt.Status.Model`, and it crosses the bridge as `ManagedStatus.LoadedModel`
([api.go:302](api.go#L302)), filled only while that server is running or starting.

It is deliberately **not** `ModelPath`, and the difference is the reason the field exists: a
save does not restart anything — the rule `PLAN.md` states as *Saving a model does not start
it* — so picking another model moves the stored path while the process keeps serving the file
it loaded, and the two differ until a start. A listing that marked the selection as loaded would
claim a switch that has not happened, which is why the row names the model the server *still*
serves once the two part ways.
`TestTheRowNamesTheFileTheRunningServerLoaded` ([loaded_model_test.go](loaded_model_test.go))
drives a real managed start to pin all three states: nothing while idle, the started file while
running, and that file still after a re-pick — then stops the server, because the supervisor
keeps the model in its status once the process is gone and the state is what the field is
gated on.

A server nobody here started is not covered: an entry pointing at a `llama-server` the user
runs themselves has no supervisor, so no loaded model is reported for it. The provider's own
`/props` reading (§2) is what describes that case, and it is a different question — it is about
the backend's listing, not about a file in the folder row.

### 3.11 Starting a model from the chat

**A stopped managed server answers with nothing.** That is not a fault — `/v1/models` of a
process that is not running is empty — but it had a consequence the chat had no answer for:
the model picker lists what the backends report, so with no server up the row is simply
absent, and the only way to a local model was Settings → Backends, naming the file there,
waiting out the load, and coming back to pick the model. Three screens for one intention, in
the panel whose whole job is picking a model.

The picker carries that intention itself now, as a collapsed section at the head of its list
(`LocalModelStart`, [frontend/src/components/ModelSwitcher.tsx:908](frontend/src/components/ModelSwitcher.tsx#L908)):
the entry KiChat runs (`kind == "llamacpp"`, the one kind whose server this app starts and
stops), the folder it would read — `ScanModelFolder("")`, so the same folder §3.10 describes,
loaded when the section is opened rather than when the panel is — and, on a click, the GGUFs
in it with their sizes. The state rides in the header while a load runs, because a load is
minutes long and a header that said nothing would read as a click that did nothing.

**The two clicks are not the same click.** A file that is not loaded goes through
`StartManagedModelWith` ([api.go:1303](api.go#L1303)): the stored entry with another
`modelPath` and never a fresh entry, so a context size, a KV cache type or an offload choice
made in Settings survives a pick in the chat. The chat is then re-pointed at it **after** the
server reports `running` — a switch made while the weights are still coming in fails the next
request — and through the same path a click on a model in the list takes, including the
"next message only" checkbox, which is a choice the user already made about how a switch is
to be applied. A file that *is* already loaded is a selection, not a start: `Server.Start`
calls `Stop()` first (§3.5), so starting the file in memory would be minutes of loading for
the file the server is already serving. Measured in the harness: clicking the loaded row
called no start binding at all and the port stayed what it was.

**The start writes the entry, and that is the point.** Starting a file without storing it
would leave the chat on a model the panel does not name and the next launch on another one,
so a pick is a selection with a side effect: the entry KiChat would start at launch becomes
the file that was picked. `enabled` is left as it is — which is why a first pick from the
chat, an entry that does not exist yet, sends the panel's own defaults with `enabled: false`
(`defaultManagedModel`, [frontend/src/lib/api.ts:151](frontend/src/lib/api.ts#L151)) and so
starts the model *now* without promising to start it on every launch. That switch stays a
Settings decision, and the entry's row there shows it.

The write is the reason a start from the chat re-reads the settings rather than only the
model list (`refreshSettings`, once when the file is stored and once when the server comes
up): the Settings panel renders the store's copy of the blob, and its Save writes that copy
back, so a stale one would put the previous file — and the previous server address, which
`onManagedState` rewrites on every start — back over what the pick had stored. Measured in
the harness: after starting Qwen3.8 from the picker, the panel's **Selected model** row names
that file, with `in use` on its row in the folder listing.

**The window is checked exactly as a click checks it** — the same `GetBudget` comparison the
picker makes — with one difference: a start that fits commits at once, because the user has
already named the file, while one that does not stops at the `Switch and compact` panel with
the numbers in front of it rather than failing on the next request.

A refusal is reported where it happened: the section keeps the backend's own message under
the list, and the rows come back enabled, so the file is one click away. Nothing is stored
by a start that failed, because a stored path that is not there fails every later start
too — including the automatic one at launch. `TestStartingFromThePickerNamesTheFileWithoutEnablingIt`,
`TestASecondPickRestartsOntoTheOtherFile` and `TestOnlyTheEntryKiChatRunsCanBeStartedThisWay`
([start_with_model_test.go](start_with_model_test.go)) pin the three
that matter: the file is stored and `enabled` is not touched, a second pick restarts onto the
other file rather than storing a path the process is not serving, and an entry KiChat only
talks to — LM Studio — is refused with its name in the message rather than silently started.

**And the same section gives the memory back.** A managed server holds the whole file in
memory, and the panel whose click put it there is where releasing that belongs: the state
line at the top of the expanded section carries the state word, the file the process is
serving, and a **Stop** — `StopManagedModel` ([api.go:1358](api.go#L1358)), the binding
Settings' own Stop button calls, so there is no second way to end a process. It is offered
whenever the state is not `stopped`, which is what makes it the way out of a load begun by
mistake as well: an abort is the same stop, and a 27B file takes minutes to come in.

**A stop writes nothing.** The entry keeps its file and its `enabled` flag, which is what
makes a stopped model one click from running again; `enabled` stays a Settings decision
about the next launch, and stopping a model is not that decision. What changes is the state
line's own reading: the mark, the port and the address go with the process, and the
"in use" badge in the file list goes with them. `TestStoppingFromThePickerKeepsTheEntryStartable`
([stop_model_test.go](stop_model_test.go)) drives a real picker start and then a stop on the
stand-in launcher: the entry still names the file with `enabled` untouched, the loaded model
and the port are gone, a second stop answers nil — the stale reading a button can be offered
on — and the panel's own start works on what survived.
`TestStoppingSomethingThatIsNotRunningIsNotAnError` covers the never-started entry and an
unknown backend id, both of which have to answer without an error and without touching
anything.

The chat does not move. The conversation keeps naming the model it was using and its dot
reads as *not loaded*, exactly as after a stop in Settings — the stop is about the server's
memory, not about which model the chat is set to.

**The section polls while anything is loading, not only while its own click is.** The state
line is drawn from `GetRuntimeStatus`, and a load started in Settings — or one whose panel
was closed and opened again — reaches `running` in the picker rather than freezing at the
reading it opened with. Measured in the harness: a click on a file puts `loading …` on the
row and in the header within 300 ms, **Stop** beside them; pressing it mid-load leaves the
fixture `stopped` with every row enabled again, which is the pending ready timer being
cancelled the way a killed process cancels it.
### 3.12 A vision model is two files, and the app pairs them

**A multimodal GGUF is only half a model.** The vision tower lives in a separate `mmproj`
file, and `llama-server` started without it has nothing to hand an image to. It does not
answer text-only — it **refuses the turn**:

```
HTTP 500 {"error":{"code":500,"message":"image input is not supported - hint: if this is
unexpected, you may need to provide the mmproj","type":"server_error"}}
```

Measured on this machine's own Qwen3-VL-8B through the app's supervisor
(`prism-b10777-e3de715-rocm-7.2`, the `Q4_K_M` weights with the `F16` projector beside them),
same build, same 64×64 PNG, one flag apart:

| start | `/props` `modalities` | image request |
| --- | --- | --- |
| without `--mmproj` | `{"vision": false, "video": false, "audio": false}` | **500**, the body above |
| with `--mmproj …F16.gguf` | `{"vision": true, "video": true, "audio": false}` | **200** |

So the app resolves it itself, at every start, and passes `--mmproj` just before the user's own
arguments. `ManagedModel.MMProj` is the override: empty means "whatever sits beside the model",
which is where every download puts the pair, and a path pins it to that file instead — the two
halves are not always downloaded together. A named file that is not there is refused **before**
the model loads, because llama.cpp would otherwise read five gigabytes of weights first and only
then fail to open the projector. A detected file is never written back into the settings: an
empty field has to keep meaning "the one beside the model" or a replaced quant would need the
field cleared by hand.

The folder is read, not guessed, and `MatchProjector`
([internal/runtime/discover.go:309](internal/runtime/discover.go#L309)) answers `""` rather than
picking:

| folder | answer, and why |
| --- | --- |
| one projector, any name (`mmproj.gguf`) | it — position alone is unambiguous |
| several, one named for this model | that one: the model's own file name tokens decide, which is how a 4B sibling's projector in the same folder is passed over |
| two quants of one projector (`F16` and `BF16`) | **none** — a tie, and two quants of one file are the user's choice |
| a projector named for another model | **none**: a name that is not contained in the model's own name is that model's, and a vision tower that does not fit what it is loaded with is worse than none |
| nothing | **none**, which is what the panel warns about: this model cannot read an image |

`mmproj`, `f16`, `bf16`, `q4`/`iq4`-style tokens are dropped before two names are compared, so
the `Q4_K_M` weights and the `F16` projector of one model still look like the same model.

**The consequence upstream of this: the model reports itself as text-only, and the app believed
it.** `/props` `modalities.vision` is read by the provider
([internal/llm/llamacpp.go:294](internal/llm/llamacpp.go#L294)), so a projector-less start is
listed as an `llm` with no `vision` capability — while the chat context attaches an image to a
turn regardless of capability
([internal/chat/context.go:255](internal/chat/context.go#L255)), with only the ingest warning
as notice. That combination is how the 500 above reaches the transcript as the server's own
words instead of a sentence from the app.

### 3.13 A limitation, not a bug

**A managed server has no API key and allows all origins**, which its own log points out. It
binds to `127.0.0.1` only, so this is a local-origin exposure rather than a network one; it is
recorded as a limitation in `PLAN.md` §8.

---

## 4. Ternary models on the GPU (measured)

**The ternary kernels are in the fork's GPU backends, not only its CPU one.** That is what
makes offload worth anything for a ternary model — a backend without them would move the
layers but not the maths. Measured on the extracted `prism-b10770-6684606` archives:

| backend | kernel evidence |
| --- | --- |
| Vulkan (`libggml-vulkan.so`, 58 MB) | `dequant_pq2_0`, `matmul_pq2_0_f32_fp32_data`, `block_pq2_0_packed16`, `mul_mat_vec_id_ptq1_0_q8_1_f32_subgroup`, `get_rows_ptq1_0` — shader and kernel entry points, i.e. compiled kernels rather than a type name in a registry |
| CUDA 13.3 (`libggml-cuda.so`) | 100 × `pq2_0`, 259 × `ptq1_0`, 2 × `PQ2_0` |
| ROCm 7.2 (`libggml-hip.so`) | 1718 × `pq2_0`, 7178 × `ptq1_0` |

Note the **case**: the *type names* are uppercase (`PQ2_0`) and appear in the CPU objects and
in CUDA, while the Vulkan shaders and the HIP kernels name themselves lowercase — a grep for
the uppercase spelling alone would report the Vulkan build as missing them.

**And a ternary model on the GPU was measured, where the CPU comparison is the point.**
`Ternary-Bonsai-2-27B-PQ2_0.gguf` (6.71 GB, in the LM Studio folder's `prism-ml/` directory,
whose `Ternary-Bonsai-2-27B-mmproj-BF16.gguf` the scanner skips) loaded through the app's own
supervisor on the fork's `prism-b10770-6684606` builds, with `Offload: all` and the trace
level the app sets:

| build | placement | ready | speed | free memory |
| --- | --- | --- | --- | --- |
| Vulkan | 65/65 layers on `Vulkan0` | 4 s | **53.3 tok/s** | 11,087 MiB |
| ROCm 7.2 | 65/65 on `ROCm0` | 2 s | **57.0 tok/s** | 12,580 MiB |
| Vulkan, `--gpu-layers 0` (the app's `off` mode) | `0/65 on Vulkan0` | — | **4.6 tok/s** | — |

`/props` answered `n_ctx=4096` with an 8,952-character template, the answer was coherent and
the server reported `system_fingerprint: b10770-6684606ac`. That gap is what proves the
ternary kernels run *on the device*: a backend that had them only for the CPU would move the
layers and still produce the 4.6 tok/s number. The reading arrives the way a managed start
reports it in the panel — `On Vulkan0: 65 of 65 layers on the GPU`.

---

## 5. Where the code is

| file | symbol | what it does |
| --- | --- | --- |
| [internal/llm/llamacpp.go:63](internal/llm/llamacpp.go#L63) | `LlamaCppDefaultBaseURL` | `http://127.0.0.1:9931`, and why not 8080 |
| [internal/llm/llamacpp.go:67](internal/llm/llamacpp.go#L67) | `newLlamaCppProvider` | the two chat paths, `/v1/models`, `/props`, `flavourLlamaCpp` |
| [internal/llm/llamacpp.go:121](internal/llm/llamacpp.go#L121) | `llamaCppProps` | the part of `/props` the app acts on, caps as a map |
| [internal/llm/llamacpp.go:182](internal/llm/llamacpp.go#L182) | `fromLlamaCppEntry` | entry → `ModelInfo`, with `n_ctx_train` as the ceiling only |
| [internal/llm/llamacpp.go:237](internal/llm/llamacpp.go#L237) | `enrichLlamaCpp` | folds `/props` in; direct server vs router |
| [internal/llm/llamacpp.go:310](internal/llm/llamacpp.go#L310) | `llamaCppLoadedIndex` | which row a `/props` reading belongs to |
| [internal/llm/llamacpp.go:346](internal/llm/llamacpp.go#L346) | `propsFor` | caches the reading; 404 remembered, other failures not |
| [internal/llm/llamacpp.go:390](internal/llm/llamacpp.go#L390) | `llamaCppTemplateTogglesThinking` | the guard on sending `chat_template_kwargs` |
| [internal/runtime/distribution.go:71](internal/runtime/distribution.go#L71) | `Distributions` | the two sources |
| [internal/runtime/distribution.go:147](internal/runtime/distribution.go#L147) | `PublishedAccelerators` | the source's own answer per platform |
| [internal/runtime/distribution.go:167](internal/runtime/distribution.go#L167) | `AssetName` | tag + platform + family → the archive |
| [internal/runtime/distribution.go:250](internal/runtime/distribution.go#L250) | `TagOfDir` | the whole tag shape, from the right |
| [internal/runtime/install.go:150](internal/runtime/install.go#L150) | `NewInstaller` | owns the runtime directory |
| [internal/runtime/install.go:642](internal/runtime/install.go#L642) | `Installer.Install` | download, verify, extract, flatten, move into place |
| [internal/runtime/install.go:1128](internal/runtime/install.go#L1128) | `entryPath` | tar entries; the fork's `./` root entry |
| [internal/runtime/builds.go:47](internal/runtime/builds.go#L47) | `Installer.Builds` | the cleanup list, with sizes |
| [internal/runtime/builds.go:118](internal/runtime/builds.go#L118) | `RemoveBuild` | refuses a non-build |
| [internal/runtime/supervise.go:149](internal/runtime/supervise.go#L149) | `NewServer` | one supervisor per managed model |
| [internal/runtime/supervise.go:195](internal/runtime/supervise.go#L195) | `Server.Start` | free port, `-lv 4`, readiness |
| [internal/runtime/supervise.go:375](internal/runtime/supervise.go#L375) | `Server.Stop` | |
| [internal/runtime/supervise.go:471](internal/runtime/supervise.go#L471) | `Server.Binary` | which launcher started it |
| [internal/runtime/gpu.go:407](internal/runtime/gpu.go#L407) | `serverEnv` | the ROCm library path |
| [internal/runtime/libs.go:69](internal/runtime/libs.go#L69) | `MissingLibraries` | the loader's answer for a build directory |
| [internal/runtime/discover.go:80](internal/runtime/discover.go#L80) | `LMStudioFolders` | which folder to offer first |
| [internal/runtime/discover.go:155](internal/runtime/discover.go#L155) | `ScanModelFolder` | the `.gguf` files in one |
| [internal/config/config.go:641](internal/config/config.go#L641) | `NormaliseModelDirs` | the folders the row remembers: trimmed, deduped, bounded, and holding the folder in use |
| [api.go:1140](api.go#L1140) | `ModelFolders` | the remembered folders first, then LM Studio's, one entry per path |
| [api.go:302](api.go#L302) | `ManagedStatus.LoadedModel` | the file the running server was started with; empty unless one is running |
| [api.go:1210](api.go#L1210) | `defaultModelDir` | which folder an empty field means |
| [api.go:1303](api.go#L1303) | `StartManagedModelWith` | the chat picker's start: the stored entry with another file, checked, then started |
| [internal/runtime/discover.go:270](internal/runtime/discover.go#L270) | `ProjectorsBeside` | the projector GGUFs sitting in the model's own folder |
| [internal/runtime/discover.go:309](internal/runtime/discover.go#L309) | `MatchProjector` | which of them belongs to that model, or none when the folder does not answer |
| [app.go:350](app.go#L350) | `managedProjector` | the typed projector, else the detected one; a typed file that is gone refuses the start |
| [internal/runtime/version.go:26](internal/runtime/version.go#L26) | `BinaryVersion` | `--version`, with a deadline and `WaitDelay` |
| [api.go:473](api.go#L473) | `CheckLlamaRuntimeUpdate` | one request, cached |
| [api.go:820](api.go#L820) | `GetRuntimeStatus` | reports the cached copy; clears before start |

---

## 6. How this is verified

### Offline

The package's tests build their own archives and serve them from an `httptest` server, so
**no check downloads anything**: `install.go`, `builds.go`, `distribution.go`,
`offload.go`, `gpu.go`, `libs.go`, `discover.go` and `version.go` each have a test beside
them, and the root package's `accelerator_test.go` holds the family and source lists in
step across the settings package, the runtime package and the three dictionaries.

### Live

```
KICHAT_LIVE=1 go test ./internal/runtime/ -run TestLivePrism -v
#   optional: KICHAT_PRISM_MODEL=<path.gguf>, otherwise the smallest model in
#   the LM Studio folders; skips if there is none. Loads a real model: minutes.
```

`TestLivePrismForkRunsAModel` exercises the fork as a runtime rather than a table of names:
its release list, its tag scheme (`TagOfDir` round-trip, and that the tag is not offered as
an update over itself), its archive — whose `./` root entry aborted the install on the
test's first run — its ternary kernels (`PQ2_0`/`PTQ1_0` in the shared objects), then a real
GGUF loaded on its `llama-server` with `/props`, a real turn and a `system_fingerprint`
naming the release.

Measured: install **1 s**, model ready in **2 s** on this machine, `/props` `n_ctx=4096`, a
7,828-character chat template, and `--version` answering
`0.2.0-dev (build 10770, commit 6684606ac)` — the running server naming the release it was
built from. The same model that answers in 24 tokens of `reasoning_content` with an empty
`content` is why the turn accepts either field, the same trap DEEPSEEK.md §4 documents.
Everything it downloads is 17 MB and loadable in seconds, which is why this is cheap enough
to keep.

**A managed chat was measured, not assumed.** `LFM2.5-1.2B-Instruct-Q8_0.gguf` (1.25 GB,
from the folder above) started under `b11535`, `/health` went 200, the provider listed it as
`loaded` with a 4,096-token window, and a real turn came back in **5.4 s** with
`prompt=33 completion=26` and llama.cpp's own timings (**6.2 tok/s**, ttft 1366 ms,
`source llamacpp`). Models llama.cpp cannot load at all are a different matter: the bundled
`nomic-embed-text-v1.5` starts and answers `/health` but refuses `/v1/chat/completions` with
"the current context does not support logits computation".

### A test can start a server without llama.cpp installed

The root package's `TestMain` re-executes the test binary as a stand-in launcher when
`KICHAT_TEST_FAKE_LLAMA_SERVER=1` is in its environment: the script the tests write exports
that variable plus the build number, and the child then answers `/health` 200, `--version`
(`0.6.0-dev (build 11535, commit deadbeef0)`, the build its directory is named after) and
`--list-devices`, and exits by itself after a minute so a test that dies without stopping it
cannot leave a listener behind.

That is what makes the guard that refuses to delete the build a *running* server was started
from testable: the process and the readiness poll are real, and no python, netcat or
downloaded archive is needed. Nothing takes that path unless the variable is set, and a test
that skips on Windows (the launcher is a shell script) is the only user of it.

The same stand-in is what lets the picker's start
([start_with_model_test.go](start_with_model_test.go)) and the loaded mark
([loaded_model_test.go](loaded_model_test.go)) be asserted on a server that is really running
rather than on a stub: the file is written to a temporary directory, the start goes through
`Server.Start`, and what is checked is the *status* — the stored path, `enabled` untouched, the
`LoadedModel` of a running entry, the second pick that replaces it, and the refusal of an entry
KiChat only talks to.

### The harness fixtures

The mock-bridge harness (`AGENTS.md` explains what it is) carries the runtime's states as
query flags. **All of them install a launcher first, so the rows that depend on one are
actually there:**

| flag | what it renders |
| --- | --- |
| `?update=available` | a newer build published than the one on disk |
| `?update=error` | a lookup that could not be made — a rate limit |
| `?update=stale` | a reading taken for another family, which the panel must not show as an answer about the installed one |
| `?devices=` / `?fit=` with no flag | an installed build that is current |
| `?dist=prism` | the fork as the installed source: the build on disk is named `prism-b10770-6684606-cpu`, the tag column prints that whole tag rather than `prism`, the family list drops OpenVINO (the fork publishes five of upstream's six), and the update row is about `prism-…` releases. Without the flag the fixture is upstream. |
| `?offload=cpu` | a `--gpu-layers 0` start: the same placement sentence, about the CPU |
| `?devices=none` | a CPU-only build — every layer reported offloaded with **no device named**, which the panel must not present as a GPU |
| `?mmproj=auto` (the `?fit=` default) | the projector LM Studio downloaded beside the model, paired without being asked: the field is empty and its placeholder plus the row under it name `mmproj-Ministral-3-3B-Instruct-2512-F16` as what makes the model able to see |
| `?mmproj=typed` | a projector the user pointed at somewhere else: the path is in the field and the row says it is loaded rather than found |
| `?mmproj=none` | a multimodal GGUF with no projector beside it — the only state where the row warns, since this model answers every image with the 500 above |
| `?builds=old` | the cleanup list: a build an update replaced (31.8 MB) beside the staging directory of an interrupted download (612 MB, marked incomplete) and the build in use, which carries no remove button. Pressing `Update now` under `?update=available` produces the same shape on its own, because the mock's install leaves the replaced build in the list the way the real runtime directory does. |

The three placement shapes need the model started first: a stopped server reports no
placement, the way the real row clears it.

The model-folder row has no flag of its own, because it has no second state worth a flag:
the fixture is pointed at LM Studio's downloads **and remembers one library of its own**, so
the row opens with two custom chips beside LM Studio's two, and the folder picker answers with
a third path (`/media/usb/models`) that holds models — a pick that landed on a folder already
in the row would neither add a chip nor scan anything. `ModelFolders` in the mock is computed
from `settings.runtime.modelDirs` the way the backend computes it from the blob, so picking,
forgetting and saving all move the chips rather than only the draft.

The loaded mark needs one flag and one click: `?fit=fits` installs a build and selects the
Ministral file, and pressing **Start** then loads it — so the listing carries the mark while
the row is `starting` and again once it is `running`. Clicking another file in the listing
produces the state the mark exists for, since a pick does not restart anything: the checkmark
and the header move to the new file, the mark stays on the loaded one, and the line under the
state badge names it.

The chat's picker is a second screen over the same two states, and `?fit=fits` is the flag for
it as well: the local section at the head of the picker lists the three GGUFs of the folder in
use, a click puts `loading …` on the row and the state in the section header, and once the mock
finishes its 800 ms start the file carries `in use` — while the panel closes, the chat's own
button moves to that file, and the Settings row's **Selected model** names it (the app re-reads
the settings the start wrote, so a Save there cannot put the old file back). Reopening the
panel shows the state in the collapsed header (`running`) and, expanded, the state line with
its **Stop**: pressing it clears the badge, the state line and the `in use` mark, leaves the
entry naming the file, and drops the catalogue row the running server had — while the chat's
own button keeps naming the model, without a window number. Pressing **Stop** while the
fixture's 800 ms load is still running is the abort path: the mock cancels the pending ready
timer the way a killed process does (`cancelManagedStart`), because a fixture that let it fire
would report a server coming up a second after it was stopped, which is a state the backend
cannot produce. The mock also writes the backend's address only on the way up, matching
`onManagedState`, which returns early for anything but a running server — so a stopped row keeps
the last address and reads as refused rather than blank. With no flag at all the launcher is missing and the section says so
(`No llama-server yet — download one, or name a path.`) instead of offering files, which is the
state a fresh install is in. The mock's `StartManagedModelWith` stores the entry and then calls
the same `startManagedServer` the Settings row's start calls, so a picker pick cannot land in a
state the backend would not produce. One caveat when reading that page: the catalogue's
`LLAMACPP` group carries a permanently `loaded` `Qwen3-8B-Q4_K_M` row, a label fixture for
"an id is a path", so it is not the managed server's state and does not move with it.

---

## 7. Known gaps

1. **No GPU family beyond the measured ones has been downloaded and run.** The
   platform → family mapping was resolved against the live release listing for every family,
   and Vulkan and ROCm were run on a real ternary model, but CUDA, SYCL and OpenVINO were
   never installed here — those rows are verified as *offered correctly*, not as *working*.
2. **The fork's Linux CUDA row has no runtime bundle to attach** (§3.3), so that combination
   is documented as broken-by-omission rather than fixed.
3. **A load takes minutes and no check performs one voluntarily.** The live Prism test loads
   a small GGUF; the ternary model sizes (6.71 GB) are measured by hand.
4. **`GET /props` has no authentication path here.** `authHeaders` is sent, so a keyed
   server works, but `--api-key` on a *managed* server is not configured by the app.

---

## 8. Deliberately not done

- **Attaching upstream's CUDA bundle to the fork's Linux CUDA build.** It would work often
  enough to look right and cross both sources and suites to do it; the size reading
  (175–311 MB) is not evidence that a runtime is inside, which is the mistake that produced
  the note in §3.3.
- **A directory comparison for the update check.** It would flag `b11541-cpu` against
  `b11541-vulkan` on every poll; tags are compared instead (§3.4).
- **Reading `n_ctx_train` as `ContextLimit`.** It is the trained-for window, not the loaded
  one, and using it promises a window the server refuses and silently truncates the prompt
  from the front (§2.2).
- **Sending `-lv 4` to a launcher the user supplied.** An older binary may not know the flag,
  and an unknown argument stops the server rather than producing a report (§3.6).
