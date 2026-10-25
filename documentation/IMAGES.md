# IMAGES.md

Text-to-image generation: the tool the chat model calls, the server behind it, and
how a picture reaches the window and the user's disk. `AGENTS.md` points here; the
measured detail lives here and nowhere else. Making an existing picture larger is a
different operation with no server and no model in the default case:
[`UPSCALING.md`](UPSCALING.md).

---

## 1. What it is, and what it deliberately is not

The chat model gets one more tool, `generate_image`. When it calls the tool, KiChat
draws the picture through a **Stable Diffusion WebUI the user runs themselves**,
files it as an attachment of the reply that asked for it, and the window shows it.

It is the first part of KiChat that produces a file rather than text, and the design
follows from one rule: **the picture is an image attachment, exactly like one the
user uploaded.** Nothing downstream needed a new concept. The frontend already
renders a message's attachments, so it needed only a component that paints a picture
instead of a chip, and the recipe on the attachment is what makes the picture
drawable again. One part of the uploaded shape does not carry over: an image reaches
the model as a part of a **user** turn only, and a generated picture belongs to the
assistant turn that asked for it, so it is stored, shown and redrawn but never sent
back (§4, §10). §4 is that argument in full.

Two things it is **not**:

- It is not a hosted image service *by default*. Nothing leaves the machine unless the
  user picks the one hosted kind (§2, NVIDIA NIM), and that kind is off until it is
  chosen. The local WebUI, `sd-cli` and managed `sd-server` need no key, no meter and no
  network, which is why they are the backend the section starts on.
- It is not `img2img`, inpainting or a LoRA browser. Those are routes on the same
  server, and adding them is a request field each rather than a new design.

## 2. One client, three servers — and three more kinds besides

`internal/images/sdwebui.go` speaks **`/sdapi/v1/txt2img`**, and one client covers
three servers because they share the route:

| server | endpoint | response |
| --- | --- | --- |
| AUTOMATIC1111 | `POST /sdapi/v1/txt2img` | `{"images":["<base64>"],"info":"<json string>"}` |
| Forge (fork of the above) | same | same |
| stable-diffusion.cpp `sd-server` | same, as a compatibility route beside its native `/sdcpp/v1/img_gen` | same |

The API wiki documents `images` as a list of base64-encoded pictures
(<https://github.com/AUTOMATIC1111/stable-diffusion-webui/wiki/API>); sd-server
documents the same compatibility route and shape
(<https://github.com/leejet/stable-diffusion.cpp/blob/master/examples/server/api.md>).

A **hosted** provider is none of these kinds, and there is exactly one: **NVIDIA NIM**
(`ImageSettings.Kind = "nim"`, §2). It is the only backend here that costs nothing to
install and leaves the machine to draw. What the other free cloud image APIs offer, what
each allowance really is, and why only this one was built is surveyed in
[`IMAGES-CLOUD.md`](IMAGES-CLOUD.md).

Four details of that shape are handled rather than assumed, each because the wrong
assumption produces a *wrong picture* instead of an error:

- **`info` is a JSON string containing JSON**, not an object. The seed and the
  checkpoint are read out of it, and a decoder that expected an object would silently
  report neither — which is how a picture the user liked becomes unreproducible.
- **An older build prefixed the base64 with `data:image/png;base64,`.** Both spellings
  are accepted; decoding only one yields garbage from the other, depending on where
  the comma falls.
- **`title`, not `filename`, is the name the API accepts back.** `override_settings.sd_model_checkpoint`
  takes the title; sending the filename instead draws with whatever was already loaded
  and says nothing.
- **The format is the server's `samples_format`, not a constant.** `images.SniffImage`
  reads PNG/JPEG/WebP/GIF off the leading bytes and reports both the MIME type and the
  file extension, so a WebUI configured for JPEG to save disk space does not produce a
  JPEG named `.png`, offered under a `*.png` filter and stored as `image/png`. An
  unrecognised payload keeps the PNG fallback rather than being refused — the bytes are
  what the user asked for.

`internal/images/sdwebui_test.go` pins all four, plus basic-auth (`--api-auth`), the
retry on a 503, the credential redaction, and the dimension rounding.

### Getting a model to draw with

**KiChat never downloads an image model, and has no control for one.** A checkpoint is a
several-gigabyte file that belongs to the server the user already runs, and a second
downloader inside this app would only disagree with the one they set up. What KiChat
needs is a server that is up *with its API exposed* and a checkpoint installed, and both
halves are the user's to arrange:

| server | start it with | where the checkpoint goes | default port |
| --- | --- | --- | --- |
| AUTOMATIC1111 | `./webui.sh --api` (add `--listen` to answer on the network, `--ckpt-dir PATH` to keep checkpoints elsewhere) | `models/Stable-diffusion/` | 7860 |
| Forge | `./webui.sh --api` | `models/Stable-diffusion/` | 7860 |
| stable-diffusion.cpp | `sd-server -m /path/to/model.gguf --listen-port 1234` | anywhere — the path is on the command line | set by `--listen-port` |

**`--api` is the step that is easy to miss, and the address field cannot explain it.** The
WebUI serves its own interface perfectly well without the flag while the `/sdapi/v1/*`
routes are not exposed at all, so the symptom is a Settings row that reports a failure
about a server the user can plainly see running in a browser tab. Both forks document the
flag as how you start *with* the API
(<https://github.com/AUTOMATIC1111/stable-diffusion-webui/wiki/API>,
<https://github.com/lllyasviel/stable-diffusion-webui-forge/issues/2040>), which is why
the Settings hint names it rather than leaving the user to find it.

Two further differences matter for setup. A1111 and Forge both listen on **7860**, which
is the address `DefaultImageBaseURL` offers, so the field is usually already right. A
WebUI started with `--api-auth user:pass` additionally wants the credential in the
`user:pass` field, which is stored in the secret store like every other credential.
stable-diffusion.cpp's `sd-server` instead takes the model on its own command line with
`-m`/`--model` and picks its port with `--listen-port`, so it is the one server whose
address has to be edited — and its flags have moved between builds.

**This subsection is the one part of this document that is *not* a measurement.** No
image server runs on the machine this was written on, so the flags, folders and ports
above are reproduced from the vendors' own documentation rather than observed here.
`TestLiveSDWebUI` (documented in §8) is what would settle them; until it has run against
a real server, treat the `--api` requirement as the vendors' claim rather than a measured
fact.

**ComfyUI is not covered, on purpose.** It takes a workflow graph rather than a flat
prompt, and a picture comes back through a submit → poll `/history` → `GET /view`
dance with the bytes written to *its* disk first. That is a second client with a
workflow template to manage, not a branch in this one.

### The other kind: a local program, one run per picture

Not every installation wants a daemon. `internal/images/cli.go` is a second provider for
stable-diffusion.cpp's **`sd-cli`**, selected by `ImageSettings.Kind = "cli"`, which runs the
binary once per picture and reads the PNG it wrote. The two kinds are not interchangeable
and neither replaces the other.

The trade is the one the Settings hint states: a server stays loaded, so a second picture
costs the sampling alone; a one-shot process pays a model load every time — minutes on a CPU
for a q5 diffusion model — and in exchange needs no daemon, no port and no setup beyond a
path, and reaches flags the A1111 compatibility route does not expose per request (a custom
sigma schedule, `--diffusion-fa`, the separate VAE and text-encoder files a newer pipeline
needs).

Flags are spelled exactly as `examples/common/common.cpp` declares them, including the two
that prose about the project habitually gets wrong: the scheduler is `--scheduler`, not
`--schedule`, and the CLIP encoders are `--clip_l`/`--clip_g` with underscores. Anything
outside that verified set travels in `ExtraArgs`, which is how a turbo model's `--sigmas` and
`--diffusion-fa` stay reachable without a new KiChat release. `--cfg-scale` used to be in that
list and no longer is — it is a *generation* parameter the model is given a say in (§3), so it
needs a name this app can set and record, and one command line can be seen twice in the
wild. A working command line copies across field for field:

```bash
./sd-cli -m ../models/qwen-image-2.1-Q5_K_M.gguf \
  --vae ../models/qwen_image_2.1_vae_bf16.safetensors \
  --llm ../models/Qwen3-VL-8B-Instruct-Q5_K_M.gguf -p "a lovely cat"
```

`-m` is **Single-file checkpoint**, `--vae` is **VAE**, `--llm` is **Text encoder**, and the
prompt comes from the chat. `--diffusion-model` is a field of its own for a pipeline that
names its parts with no `-m` at all — the same model either way, so passing both would hand
the binary two models to load. A pipeline of parts is the normal shape for anything newer
than SD1.x, and it is also why a *single* quantized diffusion model still lists a VAE and a
text encoder: `-m` and those two are not alternatives. `-m` and `--diffusion-model` *are* the
same slot, so a row that fills both is **refused rather than resolved** (`modelContradiction`),
by the health probe and by the draw path alike: a command line carrying both loads whichever
the parser reads last while the panel names the other, and which of the two the user meant is
not this app's to guess.

The paths are only half of what the panel offers for this kind. A pipeline's Sampler, Steps,
Size and Negative prompt are the *shared* fields — the same controls the server kind uses —
and they reach `--sampling-method`, `--steps` and `-W`/`-H` on the same command line. They
are labelled in the selected backend's own words, which is not a nicety: an empty field means
"the backend's own default", and on a one-shot program that is the program's default, not a
server's. Under the size boxes sit five buttons — 512², 768², 1024², FHD 16:9 and 4K 16:9 — because
they are the square sizes diffusion models are trained at or near, and the alternative is
typing the same number into two fields. The two 16:9 entries are bases whose second pass lands
*exactly* on a screen size rather than near one: 1280×720 at 1.5× is 1920×1080, and
1920×1080 at 2× is 3840×2160. Both bases are inside `MaxDimension` and on the latent grid and
both outputs inside `MaxOutputDimension`, so `Request.OutputSize` predicts the numbers the
backend is asked for (pinned in `limits_test.go`). 4K is the reason the preset carries a scale
of its own: 3840/1.5 is 2560, past the base bound, so the default 1.5× cannot reach it from any
legal base. A preset stores the enlargement but does **not** turn the second pass on — the
switch stays the user's, and the tooltip says what the pass would make of the base. Anything
else is still typed, so the presets add shortcuts and take nothing away.

A second working line shows the other shape — a Flux pipeline with one part passed as
`--diffusion-model`, and the mode switch this section ends on:

```bash
./sd-cli --diffusion-model ../models/flux-2-klein-4b-Q6_K.gguf \
  --vae ../models/flux2-vae.safetensors \
  --llm ../models/Qwen3-4B-Q4_K_M.gguf \
  -p "a psyduck using waterguns" \
  --cfg-scale 1.0 --steps 4 -v --offload-to-cpu --diffusion-fa
```

Six of those flags have a home in the panel and three do not, and the split is the design
rather than an oversight: `--steps` is **Steps**, `--offload-to-cpu` is the switch below, and
`-v` and `--diffusion-fa` are the extra arguments, because each of them carries a spelling
this app has no reason to model, and `--cfg-scale` now has a field of its own (§3). The rule
that separates them is what a flag can change: `-v` and `--diffusion-fa` decide how the run is
*logged* and whether the attention is the fast kernel, and neither changes what the picture is,
while the guidance scale is the one parameter here that a model can reason about and a recipe
has to record.

Six details are decisions rather than plumbing:

- **The user's extra arguments come first**, so a flag KiChat also sets is won by the value
the recipe records. A panel showing one step count while the binary drew another would record
a recipe describing a picture that does not exist.
- **The seed is chosen here and sent as `-s`, always, and never `-1`.** The flag's own default
is 42 — a *fixed* seed — so a draw that omitted it would return the same picture every time
and read as a broken prompt, while `-1` would hand the choice back to a binary whose answer
this app can only learn by parsing prose.
- **The seed the log names wins.** A build may ignore `--seed`; the shared image pipeline logs
`generating image: 1/1 - seed <n>` (`LOG_INFO`, `src/pipeline/image.cpp`), and the recipe has
to carry the number that reproduces the picture rather than the one that was asked for.
`cliSeedLine` takes the **last** match, because a batch prints one per image.
- **Stop is a kill, not an interrupt.** There is no server to ask, so this provider
deliberately does not implement `images.Interrupter`. The child is put into a process group of
its own (`proc_unix.go`) and the kill is aimed at the group, because a configured "binary"
may be a wrapper script that started `sd-cli` without `exec` and would otherwise survive the
Stop and keep grinding. Windows has neither `Setpgid` nor a negative pid, so
`proc_windows.go` asks `taskkill /T /F` for the same guarantee: `/T` walks the parent chain at
the moment it runs and ends the children with the parent, and `/F` is not optional because
without it the request is a close message that a console program like `sd-cli` never sees. A
job object with `KILL_ON_JOB_CLOSE` is the mechanism that holds a tree from the start, and it
is not used here because it has to be created before the process does and carried beside it,
while this file's two entry points are one call before the start and one after. `cmd.WaitDelay`
bounds the wait regardless: `Wait` returns only once the output pipes close, and a killed
grandchild holding the write end open is exactly what a Stop that "did nothing" looks like
from the window. That bound was found by a test, not by reading the docs.
- **The two entry points are the platform seam** — `isolateProcess` before the start,
`killProcess` after it — and `proc_other.go` now holds only what is left: the platforms that
are neither Unix nor Windows, where killing exactly the process that was started is all that
can be promised without measuring how that system names a tree. Both of the platforms this app
is built for end what a wrapper started; Windows used to be in the weaker file, and the
comment there said so.
- **CPU offload is a switch, not text.** `--offload-to-cpu` gets a checkbox
(`ImageSettings.CLIOffloadToCPU`) because it is a *mode* a user flips per model — Flux on a
card too small to hold it is the case it was asked for — and it is passed **bare**, with no
value, spelled the way a working command line spells it. It is deliberately the *only* flag
with a control of its own: every flag that carries a value stays in the extra arguments,
where a renamed option costs nothing. It is also not part of a recipe — it changes where the
work happens, not what the picture is — so a redraw reads it from settings rather than from
the stored picture.- **The draw bound is this backend's own** (`images.DefaultCLITimeout`, 30 minutes), and it
  **grows with the picture** (`images.DrawTimeout`): one base bound per megapixel of output,
  capped at eight so a wedged process cannot hold the app's single draw slot for a working
  day. A bound carried over from the server unchanged would cut a real draw off mid-load and
  report it as a failure, and a flat one would do the same to a 4K picture that fell back to
  CPU sampling *after* the card had already spent minutes on it — the one outcome worse than
  waiting, because the wait has been paid and nothing arrives. The app's outer context
  (`imageDrawTimeout`) applies the same function to both kinds, so a server's 10-minute
  figure scales with the size too. Stop, not the bound, is still the control for a draw that
  is merely slow.

### The same program as a server, started and stopped by KiChat

`ImageSettings.Kind = "sdserver"` is the third kind, and it is the *same* build in its other
lifetime: `sd-server` out of the same folder, with the same model set, started by KiChat and
left running until the user stops it or the app exits. It exists because of one measurement
and one cost. The measurement is the trade the CLI kind states: a one-shot process pays a
model load per picture — minutes on a CPU for a q5 diffusion model — while a server pays it
once and then spends only the sampling. The cost is the other side of the same trade, and it
is why this is not the default: a process that stays up holds the memory its model was loaded
into, so *"KiChat can run this for me"* and *"KiChat starts this every time I open the
window"* are two statements, and only the second one spends the user's VRAM on a decision
they had not made yet.

Nothing about *what* is loaded is decided here. The command line is the model half of the
CLI's own (`cli.go`, `modelArgs`) with a listener appended, because `sd-cli` and `sd-server`
are one executable's two faces from one archive: same flags, same folder, same model set.
Keeping those two command lines in one place is the whole of the reason `modelArgs` exists,
and it is what makes switching between the two lifetimes a change of `kind` rather than a
second configuration to fill in. Three details are deliberate:

- **The listener is bound to the loopback interface** (`--listen-ip 127.0.0.1`), the way the
  managed llama.cpp server is. This is a local model with no authentication at all; an
  endpoint on every interface is a port on the network rather than a convenience.
- **The port is chosen by the app, never defaulted.** `sd-server` documents 1234 as its own
  default, and 1234 is LM Studio's default too — a backend this app is already pointed at on
  many machines. A collision there would either fail the start or, worse, leave KiChat talking
  to the wrong server. The start takes a free port before the process exists, which is why the
  args carry the final one and why `ServerKey` is built the way it is below.
- **The build is asked whether it has a server mode at all** (`serverSupport`).
  `--listen-port` is looked for in the program's own `-h` output, the way `previewSupport`
  looks for `--preview`, because the project tells users to read `-h` rather than publishing a
  stable option list. A build that ignored an unknown flag and carried on would listen on 1234
  — LM Studio's address — and a picture would then be requested from whatever answered there.
  A build that cannot answer `-h` at all is *not* refused: there is no evidence against it,
  which is the line `Health` already takes about the one-shot program.

The start is the chat runtime's supervisor with three things made callable rather than
assumed (`rt.Options.Args`, `.ReadyPath`, `.Label`): the command line, the route readiness is
asked on, and the name the messages use. Two consequences are worth naming.

- **The readiness route is `/sdapi/v1/sd-models`, not `/health`.** That is llama.cpp's route;
  `sd-server` documents no health endpoint, and the A1111-shaped routes it does serve are the
  ones this app already uses. `/sdapi/v1/sd-models` answers 200 once a model is loaded, which
  is exactly the question a start has to answer — a probe on any other route would certify a
  server that cannot yet draw. `label()` is what keeps the failure messages honest: a row that
  said *"llama-server wurde beendet"* about sd-server would be wrong in the one place a user
  looks for a reason.
- **The bound is 15 minutes** (`images.DefaultServerReadyTimeout`), not the chat runtime's
  180 seconds. The thing being loaded is not the same size or shape: a llama.cpp GGUF streams
  off disk, while a q5 diffusion pipeline plus its VAE and text encoder is several gigabytes
  and takes minutes on a CPU — the very load `DefaultCLITimeout` gives half an hour to, since
  the one-shot kind pays it on *every* picture.

**A row that failed names no address**, and the case that made this a rule is a crash *after*
the server was ready: the row turned red and kept the URL it had while it was running, so the
panel offered a link to a listener that no longer existed. `fail()` now clears `Port` and `URL`
beside the PID and the offload reading, for the reason those two are cleared: the port was
chosen before the process was spawned, so it survives in the *log* — `sd-server gestartet (PID
…) auf http://…` is a record of what was attempted — but a URL on the row itself is one a user
can click and another tool can be pointed at, and nothing answers it. The harness fixture for
`?server=failed` sends the same shape, so the two cannot disagree.

**Starting is also restarting**, and there is no second call for it. `sd-server` creates its
context once before it listens and documents no way to replace it, so one pipeline per process
is the whole of the model-switching story: a different model is a different process. The app
therefore never has to decide between "start" and "restart" — a start replaces whatever was
running, which is what a user who pressed it meant either way.

What it must *not* do is reload on its own. A save never restarts the server, by the same rule
the managed chat models follow: a model that may have taken minutes to load is not reloaded
because an unrelated field was edited. Instead the row is told, from a reading rather than
from a flag: `imageServerRestartNeeded` compares the command line the running server was
started from (`images.ServerKey`, recorded at the start) with the one the settings now
describe, and the panel shows **Restart needed** beside a button that is already labelled
*Restart*. `ServerKey` is the program plus the model half of the command line, deliberately
*without* the port — a port is chosen per start, so including one would make every server look
out of date the moment it came up — and it is compared as *configured* (the folder and the
typed path) rather than as resolved, because resolving is a filesystem walk that can fail, and
a comparison that has to fail before it can answer is a comparison the panel would have to
render as an error. A profile's Apply uses the same reading to decide between **Apply** and
**Apply & restart**, and it restarts *after* the save: the settings are what a start reads, so
restarting first would load the model the user just moved away from.

Two settings govern the lifetime, and neither is inferred:

- **`startWithApp` starts it at launch, and is off.** Choosing the kind says "KiChat may run
  this for me"; this says "KiChat runs it every time I open the window", and the second is the
  one that costs memory on a machine whose user is about to do something else.
- **Nothing stops it but the user**, and that is the design rather than an omission: the point
  of a resident model is that it is already loaded, so an idle timer would take the memory
  back exactly when the next picture was about to be asked for. The Stop button is in front of
  the user, the row says what stopping costs, and the app's own exit stops it — `stopAllManaged`
  walks the same map the chat servers live in, so a picture server cannot outlive the window
  with nothing left in the UI to stop it.

A few consequences of the kind living in the settings section it shares with the other two:

- **The kind survives a save and a reload.** `normalise` keeps `cli` and `sdserver` as
  themselves and folds everything else to the empty string that means the HTTP server, so a
  hand-edited blob degrades to a backend that works — but a *known* kind has to come back out
  of the database unchanged, or a user's own sd-server would be silently replaced on the next
  launch by whatever answers on the stored HTTP address. The address a draw uses is the
  running server's own, taken from its status, never the stored field: a stale one must not
  become a way to draw from somebody else's server. That URL is deliberately *not* adopted
  back into `ImageSettings.BaseURL` the way a managed chat model's address is written into its
  own backend entry. There the field belongs to the server it names; here it is the *other*
  picture backend's address — the WebUI the user typed in — and a randomly chosen port written
  into it would rewrite that setting as a side effect of running a local program. Nothing needs
  it either: the provider is built from the running status, and the row shows that URL out of
  the process's own status rather than out of the settings.
- **With the server down there is no provider at all**, so `generate_image` is not offered.
  That is the honest state rather than a technicality: a backend that could only answer
  "nothing is listening" would put a tool in front of the model that costs a wasted round every
  time it was called. Starting the server brings the tool back without a restart of the app,
  because the state change rebuilds the services — which is what `ServerKey`, the address and
  the rebuild are all for.
- **A running server keeps its row when the kind is switched away** (`isSD && running` is not
  the only case that renders it): a multi-gigabyte process with nothing left in the window that
  stops it is exactly the state the chat backends' own model switch refuses to leave behind,
  so the row says the server belongs to another backend choice and offers Stop. What makes that
  render is the backend rather than the panel: `GetImageStatus` reports the process row for
  *every* kind — `stopped` when nothing runs — because a row filled only for `sdserver` would
  hide precisely the process it exists to stop. The panel then decides, and it offers only Stop
  in that state: the command line a start builds comes from the settings the other kind just
  replaced, so starting from this form would load something nobody asked for.
- **Stop against a *draw* is best effort on this kind**: `sd-server` has no
  `/sdapi/v1/interrupt`, so the wait ends and the server is not told (§5). One pipeline per
  process is the same fact seen from the other side.

### Where the program comes from

`sd-cli` is the one piece of this feature KiChat does **not** fetch. The project publishes
prebuilt archives per platform and accelerator on its own GitHub releases
(<https://github.com/leejet/stable-diffusion.cpp/releases>), and measured on tag
`master-961-60423ab` the published set is:

| platform | archives |
| --- | --- |
| Linux x86_64 | `sd-…-bin-Linux-Ubuntu-24.04-x86_64.zip` (CPU), `…-vulkan.zip`, `…-rocm-10.1.0.zip` |
| Windows x64 | `sd-…-bin-win-cpu-x64.zip`, `…-cuda12-x64.zip`, `…-vulkan-x64.zip`, `…-rocm-10.1.0-x64.zip` |
| macOS arm64 | `sd-…-bin-Darwin-macOS-<version>-arm64.zip` — one build, no accelerator to choose |

Every one of them carries **both** `sd-cli` and `sd-server`. **No Linux CUDA archive is
published**, which is why the table in the backend has no such row: a picker entry for it would
send a user to a file that does not exist, and the panel would look wrong about their machine
rather than about the release. The table lives in `internal/images/locate.go` and not in the
interface, so there is exactly one place the vendor's list can be wrong, and each row names the
archive to look for — with `{tag}` where the release's own build token goes, because the
filename carries it and cannot be written down in advance.

So the user's step is: download the archive for their machine and unpack it. KiChat's part is
the two things that step leaves undone.

- **Which archive to take.** The panel lists this platform's published builds, shows the
  filename pattern of the chosen one, notes the CUDA runtime it wants beside it, and opens the
  release page. It cannot link a file directly: the archive names carry the release token.
- **Where the program is.** `CLIFolder` is a *folder* picker, not a file picker, and the
  program is found inside it (`FindProgram`) — in the folder itself, in the single root
  directory an archive lays down, or in that root's `bin/`. One level is as deep as it looks:
  recursing would adopt whichever build a walk reached first out of a folder of old ones, and
  "point at the inner folder" is one click. Two builds *beside each other* are the same problem
  one level down, and that one was live: a user who kept the CPU build next to their Vulkan one
  got whichever name sorted first — always `cpu`, because that is where alphabetical order puts
  it — with nothing on screen to explain it, since the CPU build is exactly what the panel
  reports as "nothing detected". More than one candidate is therefore **reported**, naming each
  of them; the one thing that outvotes them is a program *directly* in the picked folder (or in
  its `bin`), which cannot be an accident. A set folder **wins** over a typed path and does
  not fall back to it, because pointing at a place is a statement about where the program is;
  a folder holding no program is a reported failure rather than a quiet draw with the path
  from last year.

The panel has to *say* that, because two fields that both look required are what a user
reported back as "seems double": while `CLIFolder` is set, the Program field is drawn
**disabled** with the reason under it instead of as an equal, and the program the folder
resolved to is printed beneath it as `Gefunden: …/sd-cli`. That reading comes from
`ImageStatus.Binary` and is printed only while `ImageStatus.Folder` still equals the folder in
the draft — the status answers for the *saved* settings, so a path printed under a folder the
user has just finished typing would be a fact about some other folder. A typed path is kept
rather than cleared when a folder arrives: it is stored, it is what runs once the folder is
empty, and a field that silently disappears takes its value with it.

What is installed is a **reading, not a setting**. `AcceleratorOf` reads the backend library
(`libggml-vulkan.so`, `ggml-cuda.dll`, `libggml-hip.so`, `libggml-metal.dylib`) and the panel
prints it beside the archive its picker is pointing at. There is deliberately no stored "which
build I chose" field: it could only ever disagree with the folder's own answer — the archive's
name is gone by the time it is unpacked — and the panel would then have two things to say. A
folder with no accelerator library in it is the CPU build, which is exactly what a build with
no backend to load is. Note that ROCm's backend library is spelled `-hip`, never `-rocm`.

`Models` returns an empty list, and that is the honest answer rather than an empty one: the
files are named on the command line, so the panel offers a file picker per field instead of a
checkpoint list. `Health` checks paths — the binary is a file, is executable, and every named
model exists — and runs `-h` only to prove the process starts at all, ignoring its exit code
because a build that does not answer that option can still draw. The `Kind` is folded to the
four spellings it has by `config.NormaliseImageClamps`: `cli`, `sdserver` and `nim` are stored
as themselves, and everything else — `server` written out in full included — becomes the empty
string that already means the HTTP server, so a hand-edited blob degrades to the backend that
used to work rather than to none at all.

### The hosted kind: NVIDIA NIM, where nothing runs here

`internal/images/nim.go` is the fourth provider, selected by `ImageSettings.Kind = "nim"`.
It calls **build.nvidia.com** over HTTPS at
`POST https://ai.api.nvidia.com/v1/genai/<publisher>/<model>` with `Authorization: Bearer …`
and a body of `prompt`, `mode: "base"`, `width`, `height`, `steps` and `seed`; the picture
comes back base64 in `artifacts[].base64`. The `data[].b64_json` / `data[].url` shapes are
accepted too, because a model behind the same host can answer in the OpenAI form. Bytes
decide the format either way (`SniffImage`), because the answer is not guaranteed to be a PNG.

**The route is the hosted one, and that cost a real 404.** NVIDIA documents an
OpenAI-compatible `POST /v1/images/generations`, but that route is the *self-hosted*
container's (its Getting Started page runs it on `http://localhost:8000`). Measured on
22 October 2026 with a valid key:

| request | answer |
| --- | --- |
| `POST integrate.api.nvidia.com/v1/images/generations`, empty body | `400` `{"error":{"message":"model field is required"}}` |
| …same route, `"model":"flux.1-schnell"`, `flux.1-dev`, `black-forest-labs/…`, `stable-diffusion-3.5-large`, `qwen-image` | `404 page not found` for every one |
| `POST ai.api.nvidia.com/v1/genai/black-forest-labs/flux.1-schnell` (bad key) | `401` — the route is registered |
| …same route, `…/flux.1-dev` | no 404, so it is registered too |
| …same route, `…/flux-3-image-klein` (bad key) | `403` `{"status":403,"title":"Forbidden","detail":"Authorization failed"}` — registered |
| …same route, `…/flux-3-image-klein-base`, or the bare `flux-3-image-klein` | `404 page not found` — not published at that path |
| …same host, `black-forest-labs/flux_1-schnell`, `stabilityai/…`, `qwen/qwen-image`, `nvidia/sdxl-turbo` | `404` fast — not published at that path |

The two answers are distinguishable, which is what makes the route check cheap: a path the
vendor does not publish answers `404` before it looks at the credential, while a published one
answers `401`/`403` for a junk key. That is how the model list below was checked without
spending a draw on each name.

So the address is a constant and not a settings field: it is the vendor's, never the user's,
and a stored copy is one more value that can go stale. The model list is a constant too
(`NIMModels`): the hosted model listing is the *chat* catalogue, and a picker built from it
would offer a text model as a checkpoint. The three ids are the ones the route was measured to
serve; anything else answers 404, which is an `ErrModelNotServed` naming the model *and* the
ids this build offers rather than a broken-URL error. `black-forest-labs/flux.1-schnell` is
the default because it is Apache-2.0 and four steps; `…/flux-3-image-klein` is second because
it is the one measured to answer on a free-trial key (its weights are under the FLUX Kommunity
License — free commercial use below US$5 M of annual revenue); `…/flux.1-dev` is offered
because a user's own account may use it, but its licence is non-commercial for production. A
bare `flux.1-schnell` or `flux-3-image-klein` in the settings blob is completed to the
publisher path on load (`NormaliseImageClamps`), because an id without its publisher is a 404
rather than a different model.

**`mode` belongs to the model, not to the service.** FLUX.3 Klein rejects the FLUX.1 spelling
outright: `mode: "base"` on its route answered `422
{"detail":[{"type":"literal_error","loc":["body","mode"],"msg":"Input should be 't2i' or 'i2i'"}]}`
in 0.5 s, while the FLUX.1 routes take `"base"`. So the client sends `t2i` for klein and
`base` for everything else (`nimModes`), and a single constant — the obvious reading of "the
vendor's examples send base" — makes one of the two models unusable.

**A 200 is not always a picture.** Klein answered a harmless prompt (`a red bicycle leaning
on a brick wall`) with `200`, an empty `base64` and `finishReason: "CONTENT_FILTERED"` in
0.6 s. The client reports the vendor's reason, because "answered without a picture" would
read as a bug in KiChat rather than as a prompt the service declined.

**The key is the NVIDIA backend's key.** `imageAPIKey` walks `settings.Backends` for a row of
kind `nvidia` and reads `llm.BackendAPIKeyName(row.ID)` — by *kind*, not by the literal id
`nvidia`, because the id is the user's to change and a row they added themselves (a second
account, a self-hosted NIM container on another port) is the same vendor with the same token
format. So the Backends tab is the one place a token is pasted and the one place it is
rotated. The image section's own stored value is deliberately *not* a candidate: that field is
the WebUI's `user:pass`, and sending it as a bearer token would turn a configured-looking
setup into a 401. With no key stored the provider is `nil` — the tool disappears from the
model's list instead of existing only to fail, and the panel says which tab to open rather
than showing an empty field.

Three things the route cannot do, said rather than approximated:

- **No negative prompt and no sampler are sent.** The hosted body has no such fields, so both
  stay hidden for this kind rather than collecting values the client would drop. `steps` *is*
  sent when one was chosen — it is a documented field of this route — and omitted otherwise, so
  the model's own default applies (four for schnell).
- **No second sampling pass.** `Generate` returns `ErrHiresUnsupported` instead of drawing
  at base size and letting the caller believe it was enlarged; changing the kind also turns
  the stored hires switch off, so the failure is not reachable by accident.
- **No progress.** The provider does not implement `Progressive`, so the window shows an
  indefinite spinner for the seconds the picture takes. A fake percentage would be worse.

The client chooses and sends a seed when the request names none, because a picture whose
seed was never recorded cannot be drawn again. A 401 or 403 is wrapped in `ErrKeyRejected`
so the advice is "replace the key", not "the service is down", a 404 in `ErrModelNotServed`
so the advice is the model picker, and a body that echoes the credential is redacted before it
reaches a toast. `Health` checks the credential against the vendor's model listing (a GET that
answers 401 for a bad key) rather than by drawing a billed picture. `GetImageStatus` reports
the vendor's address, an empty program and folder, and counts *models* rather than
checkpoints.

**Measured, not assumed** — one free-trial key, 22 October 2026, same machine, same client
shape:

| draw | answer |
| --- | --- |
| `…/flux-3-image-klein`, `mode: "base"` | `422` in 0.5 s — `Input should be 't2i' or 'i2i'` |
| `…/flux-3-image-klein`, `mode: "t2i"`, `a red bicycle leaning on a brick wall` | `200` in 0.6 s, `CONTENT_FILTERED`, no bytes |
| `…/flux-3-image-klein`, `mode: "t2i"`, `a lighthouse on a cliff at dusk, painted` | `200` in **5.1 s** — 1024×1024 JPEG, 130 441 bytes, `finishReason: "SUCCESS"` |
| `…/flux.1-schnell`, `mode: "base"` | no response headers within 240 s (twice) |

So the route, the body, the envelope and the seed all work, and the only thing still unresolved
is FLUX.1 on this account: 401/404 answer instantly, so the path is right, and what is left is
that model's own queue on the free trial. That is why a hosted draw is bounded at three minutes
and names the wait rather than sitting on a spinner — klein finishes in seconds, schnell did not
finish at all. Whether the free allowance is ~1,000 requests is still community-reported: the
successful draws were four, and the vendor reported nothing about a remaining quota.

**A stored id the picker does not list used to be invisible.** An early build of this field
wrote the bare `flux-3-image-klein`; the panel's `<select>` offered only the FLUX.1 ids, so the
browser displayed the *first* option (`flux.1-schnell`) while the state kept klein — and the
draw went out for klein, answering `404` for the missing publisher segment. What the user saw
was "flux 1 is picked" and an error naming a model they had never chosen. Two changes close
it: the stored value is completed to its publisher path on load, and the select now renders
the stored id as its own option, marked as one this build does not list, so the control cannot
say one model while another is sent.

`internal/images/nim_test.go` pins the request body (including the keys that must be
absent), the default model, klein's own `mode`, the chosen seed, the URL answer, the refused
hires pass, the missing key, the redacted 401, the filtered answer and the 404 that names the
offered ids.

## 3. Empty means "the model decides" — and then "leave it to your server"

Every generation field in Settings may be left empty, and an empty one is **omitted
from the request** rather than sent as a default:

| field | empty means | why |
| --- | --- | --- |
| server address | `http://127.0.0.1:7860` | AUTOMATIC1111's own default, and what Forge and sd-server use |
| credentials | no `Authorization` header | a WebUI ships with no authentication; sending one anyway is a request it may refuse |
| checkpoint | no `override_settings` | the user's WebUI has a model loaded; overriding it with a guess ignores their setup |
| sampler, steps, size | omitted | same reason — their WebUI already has a choice |
| guidance scale (`cfg_scale`) | omitted | 7.0 is what A1111 and sd-cli are shipped with, but `sd-server` falls back to a `txt_cfg` living inside the model file, so the real default is a number only the model can report |
| negative prompt | omitted | an empty *sent* string would **replace** a negative prompt configured in their WebUI; omitting the field leaves it in force |

This is the same instinct as the model picker offering the GGUF files a user already
has: the app must not quietly substitute its own guess for a decision the user made
elsewhere. The Settings section says so under the fields.

### One request, one picture — the one field and one bound that are *not* left to the server

A request field and an answer bound go the other way, and both for the same reason: they
decide how much work one call costs, and the user of this app asked for one picture.

- **`n: 1` is always sent** (`sdRequest.N`, not `omitempty`, unlike every field above). A
  batch size is not a drawing preference someone tuned in their WebUI, it is a multiplier on
  the request: a session left on "batch count 4, batch size 4" answers one call with
  sixteen pictures, sixteen minutes of GPU time, and a chat message that has to explain
  which twelve it threw away. `n` is the key A1111 and stable-diffusion.cpp both parse for
  it (`n_iter`/`batch_size` are the same thing under A1111's own names; the client sends the
  one both routes agree on), so one picture is what is asked for rather than inherited.
- **`MaxPicturesPerDraw` bounds what comes back anyway** (`internal/images/limits.go`). `n`
  is a request, and a server that ignores it, a build with different defaults, or a proxy in
  front of either can still answer with more; every picture in one answer becomes an
  attachment with a base64 document behind it. Four is a batch a person could have configured
  on purpose, so four are filed — the first four, in the order the server drew them — and
  both loops that store a result (`generate_image`'s and `Regenerate`'s) iterate
  `Result.Kept()` rather than the raw slice, so the bound is one rule and not one per caller.
  What was not kept is **reported to the model** rather than dropped in silence
  (`summarizeGeneration`: "Der Server hat N Bild(er) mehr geliefert …"): a model told only
  about the kept ones would answer questions about a batch it cannot see.
  `TestOneAnswerFilesAtMostTheBound` and `TestKeptBoundsOneAnswersPictures` pin both halves,
  and the dropped count stays legible as `len(Result.Images) - len(Result.Kept())`.

### The model decides first, and a filled field is an instruction

For a picture the *model* asked for, the sampler, step count, size and negative prompt are
answered in two steps rather than one, and the order is the whole design:

1. **The model, per call.** All of it is in the `generate_image` schema — `prompt`,
   `negative_prompt`, `width`, `height`, `steps`, `seed`, `sampler`, `cfg_scale`, with their
   bounds spelled out — so a model that knows a sketch wants 8 steps and a portrait 40 says so,
   and one that knows nothing says nothing. Its answers are the thing this app defaults to; the
   fields below are not defaults at all.
2. **The backend, when nobody answered.** A value neither side set is omitted from the request,
   which is the reading the table above describes: the server's or the program's own default
   applies.

**A value typed into Settings is not a default — it is a fixed value, and it wins.** Per field,
and *against* the model: a pinned size stays that size whether or not the model asked for
something else (`internal/tools/image.go`, `pinInt`/`pinFloat`/`pinString`). The asymmetry is
deliberate.
A model choosing 8 steps for a sketch and 40 for a portrait is the feature working; a model
*overruling* the step count, size or sampler that someone typed into Settings is a standing
instruction being ignored by a component that cannot see the machine, the installed model or
the licence it was bought with. Only the user knows those, so a value they entered is the last
word and the model's is the fallback.

Three consequences worth stating, because each of them looks like a detail and is not:

- **It is per field.** Pinning the size leaves the step count, the sampler and the negative
  prompt to the model — which is what makes "I want 1024×1024, otherwise you choose" one click
  rather than a configuration with holes punched in it.
- **The panel says which it is.** The empty state of those five fields reads *The model
  decides* rather than a backend default, and the group carries one sentence naming what a typed
  value costs and buys (`settings.images.pinHint`). The checkpoint selector beside them keeps
  its *Server default*, because that one really is the server's own — an empty checkpoint means
  no `override_settings` at all.
- **The recipe still records what was drawn.** `recipeFrom` is built from the request after
  the pins and the clamps, so a picture remembers the size it really has — which is what keeps
  Redraw and the picture editor honest: those two read the *recipe*, not Settings, and a
  redraw of a picture made before a value was pinned still reproduces that picture rather than
  silently adopting the new pin.

The tool description and `ImageProtocolHint` say the same thing to the model in words, because
a capability it does not know about is one it will fight: the call decides, and a value the user
fixed cannot be changed by it. `summarizeGeneration` reports the size, the steps, **the sampler
and the guidance scale** that were used, so a model that has to explain the picture it just made
can name the values that were really drawn with rather than the ones it asked for.

#### The guidance scale, and why it is the one that left the extra arguments

`cfg_scale` is the fifth field, added after the first four for a reason that is about *where a
value comes from* rather than about the field itself. Of everything the CLI kind passes through
untouched, it is the only parameter that decides what the picture *is* and that the A1111-shaped
route also takes per request, under two spellings that mean the same thing:

| backend | spelling | its own default |
| --- | --- | --- |
| AUTOMATIC1111, Forge | `cfg_scale` on `/sdapi/v1/txt2img` | `7.0` (`modules/processing.py`, `cfg_scale: float = 7.0`) |
| stable-diffusion.cpp `sd-server` | `cfg_scale`, same route | the model's own `txt_cfg` — `j.value("cfg_scale", …guidance.txt_cfg)` |
| stable-diffusion.cpp `sd-cli` | `--cfg-scale`, a float option | `7.0` (`examples/common/common.cpp`) |

The middle row is why "unset" had to stay unset rather than become a number: for a pipeline
whose guidance lives in the model file, the right value is whatever that file says, and no
configuration on this machine can know it. The scale is also the one parameter a model has a
reason to choose for itself — 1 for a distilled or flow-matching model (Flux, Qwen-Image,
SDXL-Turbo), where the guidance term is inert and a larger value does not sharpen the prompt so
much as wreck the picture, and 4–8 for the ordinary checkpoints where a low scale is looser and a
high one holds harder to the prompt. That is exactly the shape of the feature: a model deciding
1 for a Flux request and 7 for an SDXL one is the point, and a value the user typed is an
instruction that a component unable to see the installed weights does not get to overrule.

Two consequences are deliberate and worth naming:

- **It is clamped by the app, not by a vendor.** The range is 1–30
  (`images.MinCfgScale`/`MaxCfgScale`), chosen to bracket what the three backends document —
  7.0 is the shipped default of two of them and 1.0 is where a flow model is run — rather than
  quoted from a schema. Above 30 a picture stops getting better and gets *harder*, for the same
  GPU seconds; a model that invents 250 is believed about neither. A NaN (reachable: the tool
  decodes "nan" through `strconv.ParseFloat`) folds to "unset" rather than failing the draw,
  because `encoding/json` refuses to marshal one at all.
- **A user's `--cfg-scale` in the extra arguments now loses to the model.** The extra arguments
  are placed first on the command line and this app's own values last, so a value KiChat now
  sets wins — and that is the intended direction, because the recipe records the scale that was
  really drawn with. What a user wrote for a Flux pipeline therefore stays in force only while
  nobody chooses another: the field above is how to say "always 1", and the panel's
  extra-arguments hint says exactly that.

The live tests are where the honoured value can be *seen*: both servers copy it into their
`info` (`Processed.js()` on A1111, `jsoninfo["cfg_scale"]` on `sd-server`), so
`TestLiveSDWebUI` and `TestLiveSDServer` send 2.5 and assert it comes back. `sd-cli` reports no
such thing, so for that kind the evidence is the command line itself.

The numbers are bounded in **one place**: `images.Request.Normalised()`
(`internal/images/limits.go`) clamps a side to 64–2048 and rounds it to the multiple of 8
every diffusion model's latent grid needs, clamps the step count to 150, clamps the guidance
scale to 1–30, and leaves 0 — and a negative, which is the same statement — alone, because 0
is the spelling of "the server's own". `config.NormaliseImageClamps` applies those bounds to a stored blob too, and the
settings constants are *named* in `internal/config` and **declared** in `internal/images`, so
there is one copy of each.

Both halves of that earn their place, and the measurement is why. Every caller that builds a
request normalises it *before* anything is recorded, so a stored recipe describes the picture
that was actually drawn; **both** providers normalise again at the point of sending, so a
caller that forgets cannot reopen the hole. Before this, three entry points and two providers
each clamped what they happened to remember, and the three results were:

| measured | what happened |
| --- | --- |
| `cli` with a 20000×20000, 100000-step request | `--steps 100000 -W 20000 -H 20000` handed to the binary; the WebUI client clamped the same request |
| `generate_image` with the same numbers | passed to the provider unchanged, and written into the recipe as *asked for* rather than as drawn |
| `steps: -7` | reached the wire as `"steps": -7` — a JSON `omitempty` drops a zero and keeps a negative |

A local server refuses none of these; it attempts them, for hours of sampling or tens of
gigabytes of latent, on the machine the user is sitting in front of. The tool's own schema
advertises 64–2048 and 1–150, so a model that reads it is safe — and the small local models
this app is built for are the ones that most often ignore it.

#### The second pass, and the two switches that make it fit

**Hires fix is a second sampling pass**: the picture is drawn at the requested
`Width`×`Height`, enlarged, and repainted at a low denoise so the new pixels carry generated
detail rather than interpolated blur. It is an `images.Hires` on the request rather than two
requests, because every backend here has its own way to run both passes inside one call, and
splitting it would hand the second pass an opaque picture with no seed, sampler or latent to
continue from.

It is **off by default**, in Settings and in a recipe alike. A second pass costs roughly
another draw's sampling time for the same subject, which is not a cost to opt somebody into.
The two numbers beside the switch are bounded in `internal/images/limits.go`:

| field | range | default when enabled | why |
| --- | --- | --- | --- |
| `HiresScale` | 1.25–2 per side | 1.5 | below 1.25 the extra pass buys a few pixels for its whole cost; above 2 it is two doublings, and the second pass starts painting the first one's artefacts |
| `HiresDenoise` | 0.2–0.6 | 0.4 | 0.2 refines edges; 0.6 is where the pass begins composing rather than enlarging, and a picture that no longer contains the first one reads as "it drew something else" |

A1111's own `denoising_strength` default for hires is 0.7, which is why the ceiling is this
app's number rather than an inherited one. `Hires.Normalised` treats zero the way every other
generation field does — as "nobody chose one" — but only while the switch is on, so an
ordinary draw stays the zero value and a configured-but-disabled pass keeps the numbers the
user typed for the next time it is switched on. `config.NormaliseImageClamps` applies the
same rule to the stored blob.

The base size travels as the request's `Width`/`Height`; the output is their product, aligned
*up* to the latent grid by `Request.OutputSize()` and capped at **4096 a side**
(`MaxOutputDimension`, twice the per-pass `MaxDimension`). Aligning up rather than to the
nearest multiple is what the backends themselves do with a scaled size — A1111's VAE encoder
pads the edge before each stride-2 convolution, and stable-diffusion.cpp's `align_image_size`
moves the scaled target up to the spatial multiple — so 650 becomes 656 rather than 648. The
target is also passed to the backend explicitly, under its own option, whenever the base size
is known: `hr_resize_x`/`hr_resize_y` on the HTTP route and `--hires-width`/`--hires-height` on
a build whose `-h` lists them. That makes the size a request rather than a prediction, and
leaves the scale factor as the fallback for a build that predates those options. That cap is a
backstop rather than a clamp any normalised request meets: 2048² at 2× lands exactly on it,
because the per-pass bound and this one are the same bound twice. The aspect ratio is kept.
When either base side is zero — "the server's own size" — `OutputSize` answers zero too: a
server's own size is not a number this app can multiply, and inventing one would record a
recipe for a picture nobody drew.

**The target is not necessarily what comes back, because the backend aligns it again.** 656 is
on the app's 8-grid, but stable-diffusion.cpp's `GenerationRequest::align_image_size` rounds a
target up to `vae_scale_factor * diffusion_model_down_factor` — 64 for a UNet, 256 for the
widest image path — so the same 656 request produces **704×704** there, while A1111 returns 656.
That is the build drawing the nearest size it can, not a different pass, and the check below is
written to accept it.

Each backend spells the pass its own way:

- **A1111 / Forge / `sd-server`** take `enable_hr`, `hr_scale`, `hr_resize_x`,
  `hr_resize_y` and `denoising_strength` on the one `/sdapi/v1/txt2img` request. The resize
  pair is sent only when the base size is known, and it is the 8-grid `OutputSize`; `hr_scale`
  travels beside it as the fallback a build without the pair can still honour. A1111's
  `hr_resize_x`/`hr_resize_y` are its documented target fields (a target of `0` means "use the
  scale"), and `sd-server`'s compatibility route parses both onto `hires_width`/`hires_height`
  (`routes_sdapi.cpp`) before sd.cpp's own `align_image_size` rounds them to its 64-grid.
  `hr_second_pass_steps` is deliberately not sent (its own default of 0 means "as many as the
  first pass", which is what this app wants) and neither is the upscaler, so the server keeps
  the one its user configured.
- **`sd-cli`** gets `--hires`, `--hires-scale`, `--hires-denoising-strength`,
  `--hires-upscaler Lanczos` and `--hires-width`/`--hires-height` when its `-h` lists them
  (the upscaler is the one below, and the user's own `--hires-upscaler` in Extra wins). The denoise option is spelled
  `--hires-denoising-strength` in the program's own option table, not `--hires-denoise`: an
  unknown flag fails the whole run, so a short invention here would turn every second-pass
  draw into an error. The width/height pair is the same `OutputSize` target as the HTTP route,
  and `--hires-scale` remains the fallback. This is the one probe whose answer is a *refusal*
  rather than a fallback: a build that ignored `--hires` would draw at base size and exit 0,
  and the recipe would then claim an enlargement that never happened. The error wraps
  `ErrHiresUnsupported` and names the missing option. The same refusal covers a narrower
  shape — `--hires` listed with **no** scale and no target — because `--hires` alone enlarges
  by the program's own `hires_scale` default (2 upstream, 1.5 here), which is a real second
  pass in a size the recipe would misdescribe; without a number option the client cannot say
  which, so it refuses before the sampling rather than after `verifyHiresOutput` reads the
  product. Upstream ships all five flags in one option table, so that shape is a fork's.

**"Did the second pass happen?" is answered from the picture, not from the request.** A1111
accepts `enable_hr` without saying whether it honoured it, and a `sd-server` build that does
not parse the keys ignores them silently. So `verifyHiresOutput` reads the returned PNG's
header (`images.Size`) and requires each side to be **at least** `OutputSize()` — a base-size
answer is below it for every allowed scale, because the minimum is 1.25 — and **at most** that
prediction rounded up to the widest backend grid (256), which is what accepts sd.cpp's
704×704 answer to a 656 request while still refusing a server that ignored the target and
used a much larger scale. The error names the expected range and the delivered size, wraps
`ErrHiresUnsupported`, and **nothing is filed**, because a picture filed under a two-pass
recipe would make every redraw repeat the same lie. A format Go cannot read (WebP) leaves the
question unanswered rather than failed, and an unknown base size has no product to compare.
The managed `sd-server` is the same HTTP client, so it gets the same check.

The **recipe stores the base size and the pass**, not the product: `hires`, `hiresScale` and
`hiresDenoise` sit beside `width` and `height`, because a redraw has to ask for the same base
and then enlarge it — recording only the output size would draw an already-large picture and
enlarge it again. A picture drawn without the pass carries none of the three, and a recipe
from before the fields existed simply decodes with them off. The panel's editor has the same
switch and two boxes, so a redraw can turn the pass on or off without opening Settings, and
the card shows the scale (`1.5×`) when the recorded picture used one. `DurationMs` is measured
around the whole draw, both passes included, and the card shows it beside the seed: it is what
the draw cost, and therefore the best predictor a redraw has.

Progress reports carry `pass` and `passes` beside the step. The WebUI watcher knows the pass
count from the request and detects the restart when the step counter goes backwards, so the
window reads "Pass 2 of 2 · step 3 of 20" instead of appearing to lose ground; `sd-cli` reads
the same restart from its step bars. A backend with no progress, and an ordinary one-pass
draw, report `1 of 1` — `api.go` normalises both to at least one, because an absent pass
count would otherwise render as "0 of 0".

**The step is the progress; the preview is the bonus.** Two measured facts about
`master-959-d637cfb` broke that on the CLI kind, and both are pinned by tests now. The
sampler redraws its step bar with a carriage return and ends a *pass* with a newline, so a
scanner split on `\n` alone delivered the whole pass as one line and `stepFromLog` read the
first bar out of it: the row showed "step 1 of 20", once, after the pass was already over.
The log is now read with `splitLogLines`, which breaks on either character. And the program
writes its preview file about once per pass rather than once per step — at 1600×1216 that
one write is a full-latent VAE decode — while the watcher used to report *only* when that
file changed, so the row stood still for minutes with the step count already in hand. A
report now goes out whenever the step changes, preview or not, and the window keeps the last
picture it was given (`frontend/src/store.ts` merges `preview` forward). A third shape is
the one a tiled decode adds: it prints the *same* bar the sampler does (`|…| 24/54 -
27.03it/s`), so the parser tracks the program's `decoding N latents` / `latent N decoded`
lines and ignores bars in between — without that the row jumped from "step 5 of 5" to
"step 24 of 54" after the last pass, a number the draw never had. Driven against the real
binary with a flux-2-klein / qwen3-4b pipeline (640×480, 2× hires): 1/4 → 2/4 → 4/4, then
1/5 → 2/5 → 3/5 → 5/5 with the pass and preview moving with them.

**VAE tiling is a different switch for a related consequence.** It is `--vae-tiling` and
`--vae-tile-size` on a local build, and it makes the VAE decode the latent in tiles instead
of in one piece: a card that can sample a 4096² latent may still be unable to decode it in one
go. The tile side is bounded to 64–2048 like every other size, and zero is the build's own.
Both flags are passed only when the build's `-h` lists them, and unlike hires an unsupported
tiling request is **dropped rather than refused**, because tiling changes how a picture is
decoded for this machine rather than what the picture is. They belong to the model load, so
they are part of the managed server's key (`ServerKey`): changing one while `sd-server` runs
is a restart, and the row says so. The HTTP route does not expose them — a WebUI already has
its own VAE settings, and there is no per-request key this client can depend on.

**A large draw asks for both switches itself, on the CLI kind.** Measured on an RX 7900 XT
(20 GB) with the qwen-image 2.1 pipeline at 800×600 enlarged 2× (1600×1216): the second
pass allocated an attention buffer the card could not fit, printed
`ggml_vulkan: Failed to allocate pinned memory (… ErrorOutOfDeviceMemory)` and then ran the
sampling on eight CPU threads at ~12 % GPU — minutes instead of the 27 s it took with
`--diffusion-fa` — and the final decode wanted a 10 GB VAE compute buffer, failed to
allocate it, and decoded in 20 GB of system RAM instead: **55.75 s on the CPU against 7.75 s
with `--vae-tiling`** (486 MB). Neither switch changes what the picture is — one picks the
attention kernel, the other how the latent is decoded — so `images.CLI` passes them when
`Request.OutputSize()` reaches a megapixel (`largeDrawPixels`), only when the build's `-h`
lists them, and never over a `--fa` or `--diffusion-fa` the user already wrote in the extra
arguments, which keep the last word. Nothing is stored: it is a property of the draw, and a
draw that fits keeps the build's own defaults. The managed server cannot do it — its flags
are fixed at start and belong to `ServerKey` — so a large picture there still needs the
stored switch, and the row still says a change is a restart.

**The upscaler is not the latent.** The second pass starts from an enlarged first picture,
and *where* that enlargement happens decides whether the pass is clean or ghosted.
`sd-cli`'s own default is `--hires-upscaler Latent`: the latent is interpolated and then
decoded, so the second pass begins from an image that never existed. Measured on 2026-10-18
with Qwen-Image 2.1 (`qwen-image-2.1-UC-Q5_K_M.gguf`, its own VAE, `Qwen3-VL-8B-Instruct`),
480² → 960², 30 steps at the app's 0.4 denoise, one seed:

| Upscaler | What the edges look like |
| --- | --- |
| `Latent` (the program's default) | translucent copies of the ear tip, the outline and the bolt, offset by a few pixels, with soft scalloped shells across the sky |
| `Latent (bicubic antialiased)` | the same ghosting, a little softer — the filter is not the problem, the latent is |
| `Lanczos` | one crisp contour, no halo, no shell |

The base draw alone (960², 30 steps, no second pass, same seed) is clean, so this is not the
checkpoint's own output: it is the second pass starting from an interpolated latent. That is
why the app asks for `Lanczos` rather than leaving the program's default in force — a
pixel-space resample, re-encoded and repainted, which is also what A1111-shaped front-ends
do. The argument is not added when the user's own `--hires-upscaler` is already there, and a
build whose `-h` does not list the option keeps its own default and still draws.

**Not covered.** The HTTP kind sends neither an upscaler nor a second-pass step count —
that is deliberate, and documented above in the A1111 bullet: the server keeps the one its
user configured. No WebUI was running to measure whether A1111's own default has the same
problem, so that payload is unchanged.

### Profiles: the same fields, saved under a name

A model is not a server. One WebUI serves several checkpoints, and the `cli` kind is one
program pointed at one pipeline's files at a time, so "switch to the other setup" meant
retyping a checkpoint, a sampler, a step count, a size and a negative prompt every time —
which is the kind of friction that ends with the wrong sampler on the next picture.
`config.ImageProfile` is that set of fields under a name, and `config.Settings.ImageProfiles`
is the list of them; the Images tab offers it as a dropdown with Apply, Delete and a
save/update button.

**A profile *is* an `ImageSettings`, stored next to `Images` rather than inside it.** The panel
applies it by copying those fields into the live ones, so a profile that were a different shape
from the configuration it restores could drift field by field, invisibly. It is the field the
system-prompt presets already established: a named list of saved configurations, kept whole in
the settings blob, so a field added later needs no migration.

**Three fields deliberately do not travel with it**, and `Settings.normalise` clears them so a
hand-written blob cannot put them back:

| not stored | why |
| --- | --- |
| `enabled` | applying a profile must not be able to switch the feature *off* — the switch says whether the model has a `generate_image` tool at all (§5) |
| `saveFolder` | where exports go is about the user's disk, not about how a picture is drawn |
| `apiKey` | the `user:pass` belongs to the *server*, which every profile talks to, so it stays the feature's own keyring entry. Per-profile secrets would also mean one keyring entry per profile for a field most setups leave empty — and a copy of the credential inside a profile is exactly the plain-text settings row `api.sanitize` exists to prevent |

Every profile's numbers go through `NormaliseImageClamps` as well, because applying one writes
them into the live fields: a profile that kept a 20000-pixel side would re-arm, once per pick,
the request §3's clamp exists to prevent. An unnamed profile is dropped and so is a repeated
name — the panel addresses a profile *by name*, so nothing could ever select the first and an
"update this profile" click would have two candidates. Of two profiles that share a name the
*first* wins, so a repair never hands the second one's configuration to the first one's name.

**The three buttons save immediately** rather than editing the draft: a profile that vanished
on the way to another tab is the opposite of what a *save* button promises, and the point of the
list is that switching is one click. Selecting a name also fills the name box, which is what
makes "pick it, correct one field, press update" the whole editing story; the button's label
says which of the two it will do, because the same click is a create or an overwrite and only
the name decides. A delete asks twice, the way §6.1's clear does.

The mock-bridge harness ships three profiles, one per kind. A list of two server profiles would
not render the thing that matters, which is that picking the `cli` one switches the backend as
well as the sampler and the step count; and the third one names the *managed* server, which is
the only profile whose apply can be a *reload* — the only one that can differ from a running
`sdserver` in the fields a start reads, and therefore the only fixture in which the button's
"Apply & restart" label is reachable at all. The other two, applied while a server runs, leave
it alone: they name a kind that is not the managed server, so there is nothing to reload and the
running process is still the row's own Stop to deal with. `?profiles=none` is the empty state a
fresh install is in, where the dropdown, Apply and Delete are all inert.

## 4. The picture is an attachment, and that is the whole trick

`internal/tools/image.go` writes a generated picture with the same two rows an
uploaded image gets (`api.go:AddAttachment`):

```
documents.text   = base64.StdEncoding.EncodeToString(pngBytes)
attachments.kind = "image", mime = "image/png", placement = "inline"
```

No schema change, and three properties fall out for free:

1. **The assembly prices an attached image, and leaves a generated one out.** An inline
   image attachment of a *user* turn becomes `llm.Message.Images`, and
   `llm.EstimateImageTokens` prices it. A generated picture belongs to the assistant
   turn, which no image part can travel on (GROQ.md §6), so it is kept out of the
   request and out of the meter — while still being stored, shown, saved and redrawn.
2. **The frontend renders it.** `ChatView` already renders `message.attachments`; the
   new `MessageImages` component in `MessageParts.tsx` paints the ones whose kind or
   MIME is an image, and `AttachmentChips` skips those so nothing is listed twice.
3. **Deleting it works.** `DeleteMessagesFromSeq` and the chat cascade already clean
   up attachments and their documents.

### The recipe: what makes a picture drawable again

The picture is an attachment; the *recipe* is a column on it. `attachments.generation_json`
holds the prompt, the negative prompt, the size, the steps, the sampler, the guidance scale,
the seed, the checkpoint — and, when the picture went through a second pass, its scale and
denoise — and it is what turns "draw that again" into a control instead of a
conversation: the recipe is data, not a request in words, so no model has to be asked to
use it.

It records what the server **actually used**, not what was asked for, wherever the two
differ. A request that leaves the seed open is given one by the server, and only that
number reproduces the picture — the requested value was "none". The checkpoint follows the
same rule, because the server knows which one it loaded. A size or a step count the server
filled in from its own settings stays 0, exactly as it does on the way in, so a redraw
leaves that choice open rather than freezing a number the user never made.

Three consequences are deliberate:

- **An uploaded picture has no recipe**, and that is a different state from a recipe that
  happens to be empty. The column is `NULL`-like for an attached file, and the redraw
  controls are not rendered at all (`MessageParts.tsx`) — redrawing a photograph would draw
  something unrelated to it.
- **A redraw carries its seed in the filename.** `a-red-fox-1-s4242.png` is distinguishable
  from what it redrew and from another redraw, which matters because that name is what the
  caption under the picture and the save dialog both show. When even that is not enough —
  the same recipe drawn twice with the same seed — the name is bumped to `-2` rather than
  repeated (`uniqueName`), because two pictures under one name are two pictures the user
  cannot tell apart.
- **A corrupt recipe fails the read.** The value is JSON this app wrote, so one that will
  not decode means something outside the app edited the row; reading it as "no recipe"
  would make the picture silently un-repeatable, which is the one outcome the column exists
  to prevent. `config.Load` treats its own stored JSON the same way.

The column is its own **appended** migration, and that is load-bearing rather than tidy: the
runner resumes at the version a database already reports, so an entry inserted earlier in
the slice is skipped forever on every installation that predates it — no error, no column,
and every attachment read then failing. `store_test.go` builds exactly that older database
and reopens it.

The attachment is **linked to the assistant message that called the tool**, not to the
chat. That is not incidental: the frontend renders a *message's* attachments, so an
unlinked row would appear nowhere. The tool is only told its chat id, and the tool
service is one value shared by every chat, so the message id travels in the call's
context (`tools.WithCallingMessage`) rather than in a field two concurrent turns could
overwrite. The engine wraps the context in `executeTool` and emits `chat:attachments`
afterwards, which is what makes the picture appear without a full reload.

## 5. Triggering

`generate_image` rides the existing tool loop, so it works on both paths:

- **Native tool calling**, when the model advertises `tool_use` — the schema is in
  `Service.Definitions`.
- **The text protocol**, for models that cannot call functions, via
  `tools.ImageProtocolHint`. That hint is appended *only when a server is configured*
  (`generate.go`).

Absent means absent in **three** places, for one reason in each: a tool the model can see
but never use costs a wasted tool round every time it tries, and the model's complaint
about the failure reads as a broken feature rather than as a setting that is switched off.

- The native `toolDefinitions` filter drops it when `e.Tools.Images` is nil (`engine.go`).
- The `{{tool_list}}` line of the system prompt is rendered from that **same filtered
  slice** (`context.go`), not from `Service.Definitions()`. This is the one a later edit
  could leak by building the prompt's tool list separately, so it is asserted rather than
  assumed.
- `ImageProtocolHint` is a constant of its own that travels with the tool, instead of
  being folded into the always-present `TextProtocolHint`.

### It draws when asked, and not otherwise

The rule is the user's: `generate_image` is a tool like `search_web`, not a mode. Having
image generation switched on says the picture backend is available, not that every answer
should carry a picture. That has to be **told** to the model in every place it reads the
tool, because the tool itself cannot tell whether the user asked for one:

- The native definition opens with the restriction rather than the capability
  (`Service.Definitions`): an explicit request — including "mach das Bild blauer" about a
  picture that already exists — is the only reason to call it, an unprompted call is
  forbidden, and diagrams, charts, tables, UML and schematics are named as text the
  backend cannot draw.
- `tools.ImageUseHint` repeats that as one line of the system prompt, appended by
  `generate.go` only while the tool is offered and only on the native path — a model
  reads a description once, while already choosing a tool.
- `tools.ImageProtocolHint` carries the same restriction for models without native tool
  calling, since for them that hint is the whole of what the tool is.

**Measured**, on two local models (Qwen3.5-4B agentic coder and Qwen3.5-4B super coder,
through Ollama's OpenAI-compatible route, the tool list the app sends and the small-local
preset):

| prompt | before | after |
| --- | --- | --- |
| "Zeichne ein UML-Klassendiagramm für eine Bibliothek." | `generate_image` on **both** models | text, on both |
| "Erkläre den Unterschied zwischen TCP und UDP als Tabelle." | text | text |
| "Erstelle mir ein Bild von einem roten Fuchs im Schnee." | `generate_image` | `generate_image` |
| "Was ist 17 mal 23?" / "Schreib ein kurzes Haiku …" | text | text |

Every row was run before and after on at least one of the two models; the UML row, the one
that failed, on both. This is a spot check rather than a benchmark — a single sample per
cell, reported because it is what turned a report into a reproducible failure.

The old description said what the tool *is* ("ein Bild, eine Illustration, eine Grafik oder
ein Foto") and never what it is not for, and "Zeichne … Grafik" is a request a diffusion
backend cannot satisfy — it draws pixels, not a class diagram. Tightening the description
alone fixed one model and not the other: the second still drew the UML diagram until the
system-prompt rule was added, which is why both places exist rather than only the
schema. `TestTheImageToolAsksForAnExplicitRequest` pins the three texts a model can read,
and `TestTheImageToolIsInvisibleUntilAServerIsConfigured` pins that the new rule is absent
when the tool is.

A third way exists that needs no model at all: the **controls under a picture**
(`MessageParts.tsx` → `api.go:RegenerateImage` → `tools.Service.Regenerate`). They read the
recipe off the attachment (§4) and call the provider directly, so drawing again costs GPU
seconds and no tokens. The result is filed onto the **message the original is on**, which is
what puts it beside its source — an attachment on no message renders nowhere — and the
backend emits `chat:attachments` afterwards, the same event the tool path uses, which is
what makes the new picture appear without a reload.

There are four of them, and the binding takes a **whole recipe** rather than a flag, which
is what makes four controls out of one call:

- **Redraw** passes the stored recipe back untouched — the same picture.
- **Variation** passes it with the seed cleared, so the server chooses a new one.
- **FHD `<width>×<height>`** is the second pass as one click: the same recipe at a base whose
  2× enlargement lands on **1920 px on the long side**, aspect ratio kept. 800×600 becomes a
  960×720 base drawing 1920×1440; a 1024×1024 picture keeps its base and is enlarged 1.875×,
  because the bound (`MaxHiresScale`, 2×) can already reach the target. The base only grows
  when it cannot, and the label names the resulting size rather than the word alone — a
  raised base is a different *first* pass, not a bigger version of the old one, and hiding
  that behind a three-letter button would be the same kind of unlabelled substitution the
  empty-field rule exists to avoid. The denoise stays if the recipe had one and becomes 0.4
  otherwise, the seed is kept, and the button disappears on a picture already at 1920 or more
  and on a recipe whose base size the server chose (there is no number to multiply). What it
  files is an ordinary two-pass picture, redrawn later from its own recipe like any other.
- **4K `<width>×<height>`** is the same shortcut at four times the pixels: **3840 px on the
  long side**, aspect kept, so a 16:9 picture is 3840×2160 and a 4:3 one 3840×2880. The base
  is raised to 1920 — still inside the 2048 per-pass bound, and the largest size the 2× pass
  can take to 3840. Measured on an RX 7900 XT (20 GB) with the qwen-image 2.1 UC Q5 pipeline,
  `--diffusion-fa` and `--vae-tiling`, at 1920×1080 → 3840×2176: the hires pass ran at
  98.8–105.5 s/it (303 s for its three steps) on a 6.2 GB compute buffer, the tiled decode
  took 36.1 s over 464 tiles, and the whole call — two base steps included — 362 s, with the
  GPU at 100 % and **19.1 of the card's 21.4 GB** in use. A normal 20-step draw is therefore
  roughly nine minutes, which is why this is a separate button with the wait in its hint
  rather than a menu default. Unlike the FHD button, a 4K draw that does not fit is **allowed
  to fall back to the CPU** rather than refused: the picture arrives either way and only the
  time changes. That fallback is what `images.DrawTimeout` exists for: at 8.35 megapixels the
  CLI bound is the base 30 minutes times the output's megapixels — four hours at 16:9 4K, and
  the cap for anything larger. That is a bound rather than a measurement, since a 4K pass on
  CPU threads was not timed here; what it rules out is the failure where the sampling is
  finished and the process is killed for taking longer than an FHD draw is allowed. Fitting on
  the card is a separate claim and not a guarantee: the first 4K run here died silently in its
  third hires step, with no error line and no picture, and the identical retry succeeded — so
  4K is "fits, with nothing to spare" on this card and an honest wait everywhere else.
- **Edit recipe** opens the recipe in a form under the picture — prompt, negative prompt,
  size, steps, guidance scale, seed, checkpoint, sampler — prefilled from the picture's own,
  and **Draw**
sends what is in the boxes. This is the iteration loop: change the prompt, draw, look at the
result beside the original, change it again. The editor stays open after a draw for exactly
that reason.

The recipe the editor sends is the **whole** of what will be drawn, not a patch over the
stored one: a box emptied means "the server's own" rather than "keep the stored value",
which is the same reading an empty field has in Settings (§2) and in the tool's arguments.
Two consequences worth stating because they are the design rather than an accident:

- An edit never rewrites the source picture's recipe. A picture records how it was actually
  made, and an edit is a statement about the *next* draw; a redraw would otherwise stop
  describing the picture above it. The new picture records what the **server** used — the
  seed it reported, not the one that was asked for — so an editor that left the seed open
  still produces a picture that can be redrawn.
- The same bounds apply here as everywhere else (`images.Request.Normalised`, §3). A stored
  recipe came from this app, an edited one comes from a box the user filled in, and the tool's
  arguments come from a model — and a local server **attempts** a 100000-step request for hours
  rather than refusing it. A prompt-less request is refused before the
  server is contacted, for the same class of reason: it is a blank canvas the server draws
  something arbitrary on and charges for.

`internal/tools/image_test.go` pins the round trip: the drawn request carries the edited
fields, the source's recipe is unchanged afterwards, a blank prompt never reaches the
provider, and a step count outside the bound is clamped in both directions.

### One draw at a time — an app-wide slot, not a queue

`App.drawSlot` (`sync.Mutex`, taken with `TryLock`) is the rule that only one picture is
being drawn at a time, and it is a property of the app rather than of a chat or of a
backend: the machine has one GPU, and the `cli` and managed kinds *load a checkpoint per
draw*, so a second draw started beside the first is minutes spent waiting for the card, not
progress on a second picture.

Three callers take it, and that is all of them (`(*App).tryDraw`):

| caller | why it needs the slot |
| --- | --- |
| the `generate_image` tool (`tools.Service.takeDrawSlot`) | a model that calls it twice in one turn |
| `RegenerateImage` | two clicks on Redraw / Variation / Draw |
| `TestImageBackend` | the Settings test is a real draw (§8) and must not run beside one the user asked for |

**It is refused, not queued.** A queue would make the second click look like it did
nothing until the first picture finishes, which on a CPU server is minutes; the answer
instead is `images.ErrDrawInProgress` and one sentence in the window. A user can act on
"already drawing"; they cannot act on a spinner.

**The slot is taken *before* the draw is registered, and that ordering is the whole
point.** `registerDraw` replaces whatever is registered under the same id, cancelling it —
so a second draw under the same attachment that registered first and was refused afterwards
would have ended the picture the user is already waiting for: "please wait" costing them
the thing it asked them to wait for. `TestASecondDrawIsRefusedWithoutEndingTheOneRunning`
pins exactly that: after the refusal, `StopImageDraw` still finds the *first* draw, and it
ends as a stop rather than as a failure. The release is deferred on every path — a failed
draw must not wedge the feature for the rest of the session
(`TestASecondDrawIsRefusedWhileOneHoldsTheSlot`).

**The test draw has an id that is not a uuid.** It has no attachment and no message to name
it by, so it registers under the fixed `imageTestDrawID` (`"image-test"` in `api.go`), and
the Stop button beside the Settings test sends the same string — which is what lets that
button exist at all. The panel shows it only while the test runs, and a stopped test is
reported as a stop rather than as a red failure (`TestTheSettingsTestDrawCanBeStopped`).
No progress callback is wired to that id, so a test draw never paints a progress row.

### Stopping a draw

A picture a CPU server takes minutes over has to be abandonable, and `StopImageDraw` does
it in **two halves that each do something the other cannot**:

- It **cancels the request's context** ([`api.go`](../api.go) → `tools.Service.Regenerate` →
  `http.NewRequestWithContext`). That is what ends the *wait*, on every server, at the
  click — the ten-minute bound on the call is a guard against a wedged server, not a
  promise the user is held to.
- It **asks the server to abandon the work** (`POST /sdapi/v1/interrupt`). That is what
  stops the GPU, and it is the half that is only sometimes available.

The route is registered by AUTOMATIC1111 and by Forge —
`self.add_api_route("/sdapi/v1/interrupt", self.interruptapi, methods=["POST"])`, whose
handler calls `shared.state.interrupt()` and answers `{}` — and **stable-diffusion.cpp has
no equivalent**: its server cancels a *job* by id (`POST /sdcpp/v1/jobs/{id}/cancel`), and
this client does not submit jobs. So `images.Interrupter` is a **separate interface from
`images.Provider`**, a provider that cannot interrupt simply does not implement it, and the
call is **not retried** — a second attempt a moment later can only interrupt whatever the
server has since moved on to.

Three consequences, each of which is a decision rather than an oversight:

- **`shared.state.interrupt()` is global to the server, not per request.** A picture the
  user is drawing in their own WebUI tab stops too. KiChat itself draws one picture at a time
  (the slot above is what enforces it), so the window this app can open is its own draws; a
  caller pressing Stop should still know that the interrupt reaches past them.
- **The Stop result is three facts, not a bool** (`StopDrawResult`): whether a draw was in
  flight and is now over, whether the server was asked, and — separately — why it could not
  be. A 404 from a server without the route is *reported*, never raised: the wait the button
  exists for is already over, and a stable-diffusion.cpp user would otherwise get a failure
  toast every time they stopped a draw.
- **A stopped draw is not a failure.** The backend answers with `images.ErrCancelled` rather
  than the HTTP client's own `context.Canceled` wrapper, which is what lets the UI report a
  state ("draw stopped") instead of a red error for the user's own click. A cancelled
  *parent* context — the app closing — is deliberately not translated, and still reads as
  the failure it is.

Nothing is stored either way: the picture exists only once the server answers with it, so a
stopped draw leaves the transcript exactly as it was, and `image_stop_test.go` asserts the
attachment count is unchanged.

`image_stop_test.go` measures both halves against a fake WebUI that draws the first picture
and then takes forever: the cancel is *measured* (the call returns with the sentinel well
inside the ten-minute bound — removing the cancel fails the test at its own timeout), the
interrupt is a request the server sees, and a server answering 404 produces
`Interrupted: false` with the reason carried and the draw still abandoned. Deleting
`draw.cancel()` is the falsification: the test then fails with "the draw is still waiting
after Stop".

`internal/chat/image_tool_test.go` pins all three, on both the native and the
text-protocol path, and from **both** sides: "absent when off" would also pass if the
tool were never advertised at all, and that is the regression the test exists to catch.

### Watching a draw

A draw that takes minutes is the one case where "nothing to report" and "stuck" look
identical in the window, so both backends report while they work. Neither source is invented
here — each is the program's own, and each is read at the moment it is useful:

- **The WebUI** polls `/sdapi/v1/progress`, which answers a step counter, a fraction and — on
  most builds — a JPEG of the latent decoded so far. The step comes from `state`, **not** from
  the flat `progress` fraction, which is measured rather than cautious:
  stable-diffusion.cpp's `sd-server` streams `current_image` about once a second while
  `progress` stays `0.0` for the whole draw. A1111 fills in both, so the step counter is the
  one that works everywhere and the fraction is only the fallback for a server that reports
  nothing else. The POST is a blocking call that answers when the picture is finished, so this
  is a second route polled *beside* it — one request a second, not two, because the preview
  comes from the same answer. A frame larger than 4 MB is dropped while its step is still
  reported (`sdPreviewMaxBytes`), mirroring the program backend's own cap
  (`cliPreviewMaxBytes`): a build that returns the full picture rather than a thumbnail
  would otherwise push megabytes through the bridge once a second.
- **The program** (`sd-cli`) writes a preview image to the path `--preview-path` names — the
  file is rewritten on every denoiser step, so a poll of it is a live picture of the draw — and
  prints the sampling step into its log as a bar (`  |=====>    | 3/12 - 2.20s/it`). The log is
  now read **line by line** rather than collected and read at the end, because two of the three
  things in it are only useful while the draw is running: the step, which is the progress, and
  the seed, which makes a recipe record the number the program really used. The recent lines are
  kept in a ring (`cliLogLines`), which is what a failure prints.

Both sources can tell a second pass from a step counter that went backwards. The WebUI
watcher is told how many passes the request asks for and detects the reset; `sd-cli` reads the
same reset from its step bars. The report therefore carries `pass`/`passes` as well as
`step`/`steps`, and the window renders "Pass 2 of 2 · step 3 of 20" for a hires draw and a
plain "Step 3 of 20" for an ordinary one. A backend that cannot report, or a draw from before
the fields existed, arrives with zeros — `api.go` normalises them to `1`/`1`, so a missing
count never renders as "0 of 0".

Four decisions, each of which is the difference between a feature and a footgun:

- **The flags are asked for, not assumed.** `sd-cli`'s preview options are newer than its first
  builds, and an unknown option fails the **whole run** on that binary — so a build without them
  would turn "no progress display" into "no picture". The client runs `-h` once
  (`CLI.previewSupport`, memoised in `CLI.helpOnce`, which is also the probe `Health` needs),
  appends the flags only when the build's own help lists them, and does nothing when the help
  does not answer at all. A build whose help cannot be read is `previewSupport == false`, so
  `previewSupport` must not be implemented as "assume yes on error".
- **The user's own command line still wins.** `--preview` or `--preview-path` in
  `CLIExtraArgs` means this app appends neither: there is no control for the method or the
  interval, and appending ours would silently overrule a command line the user wrote. An
  unwatched draw is a far smaller loss than a draw with the wrong method.
- **A missing progress route is asked about once.** A 404 on `/sdapi/v1/progress` — a stop of
  A1111 that predates it, or something else sharing the port — sets a flag on the client and is
  never asked again: a request per second for the length of every draw is not a cost a missing
  feature should carry. The draw is unaffected either way.
- **The interface is optional**, like `Interrupter` and for the same reason: a backend that
  cannot say anything halfway through is not a broken backend. `images.Progressive` is a
  separate interface, `tools.Service.draw` falls back to a plain `Generate` when the provider
  does not implement it, and nothing about the result depends on the reports having happened.

How it reaches the window is one event and two kinds of id. `image:progress` carries
`drawId`, `chatId`, `step`, `steps`, a `preview` as a **`data:` URL** (the webview loads from an
embedded assets handler, so a path would be blocked by the same origin rule; the bytes are
already in hand, so a second bridge round trip would be a request per step), and `done`. The
`drawId` is the attachment of a redraw and the **assistant message** a model-started draw
belongs to — the row each is rendered on, which is what makes a draw the *model* asked for
stoppable at all: every Stop in the window lives under a picture, and there is no picture until
the draw has finished. `image:progress` is in `events.All()`, so `internal/events`' test
requires it in the frontend's `EV` map.

- **A draw announces itself before the backend says anything.** Registering it
  (`tools.Service.Draws`) is followed by one zero report through `tools.Service.Progress`, so
  the row — and the Stop button under it — exists from the first moment on *every* backend.
  Without it a model-started draw was only stoppable once a backend happened to report, which
  an `sd-cli` build without `--preview` and a server without the progress route never do: the
  draw was registered and cancellable, and the button that would have cancelled it was never
  rendered.
- **The end of a draw is the same event with `done` set**, not a second name, and it is sent
  once per registered draw — the start report marks it as reported, so the release always emits
  it and a progress row cannot outlive the thing it stops.
- **The release runs on the drawing goroutine**, so it only touches the emitter, which is safe
  from any goroutine; the `reported` flag it reads lives under the app's `drawsMu`.

**A measured trap, written down because the first version had it.** The drain of the program's
output was placed *before* `cmd.Wait()` and bounded by a flat two seconds. That reads well —
"the lines it wrote on its way out are in hand before Wait closes the pipe" — and it is wrong:
the bound then applies to the whole *draw*, so the preview watcher was stopped two seconds in
and every longer draw reported exactly one step and then went quiet. Measured while fixing it:
a three-second fake draw reported `{Step:1 Steps:2}` and nothing else. What replaced it is an
`os.Pipe()` this file owns (rather than `cmd.StdoutPipe()`, which the *command* closes) as the
program's stdout, so EOF means "it and everything it started are done writing": `Wait`, then
close the watcher, then a **bounded drain** of the tail. The bound now costs a truncated
diagnosis in the pathological case and never a report.

**A second trap in the same place: the watcher is waited for, not only stopped.** `close(stopWatch)`
asks the goroutine to stop; it does not make it stop, so a report already being delivered finished
*after* `GenerateWithProgress` had returned. That is a visible bug on this backend and not only a
test's problem — the callback the app passes emits `image:progress`, so a finished draw kept
repainting its own progress row — and it is a data race for any caller that accumulates the
reports in a plain variable, which is how `-race` found it. The stopper is therefore
`close(stopWatch)` **and** `<-watcherDone`, the shape the WebUI backend's watcher already had
(`sdwebui.go`, `watchProgress`): no report outlives the result it belongs to, and the return does
not happen while one is half-delivered. It is pinned by
`TestCLIDoesNotReportAfterTheDrawIsOver`, which is a test of the reported behaviour rather than a
`-race`-only one: it ends the fake draw while a report is held open, and against the previous code
it fails in an ordinary `go test` run — 0.73 s, *"1 progress report(s) were still running when the
draw returned"* — where only the race detector had noticed before.

`internal/images/cli_test.go` drives a fake `sd-cli` that answers `-h`, rewrites a preview file
and prints step bars: two reports arrive, the first carries the step and a real PNG preview,
the last carries the second step, and the seed line is still read — the draw is unaffected by
having been watched. A third holds the watcher's other half — that no report is still running, or
arrives at all, once the draw's caller has its result — and the fake draw it needs is the same stub
with its sleeps replaced by a wait the test releases, so the window is chosen rather than awaited.
Two more tests hold the negative side, which is the one that costs a user
a picture: no `--preview` flags are passed to a build whose help does not list them, and none
when the user's own arguments already decide the preview.
`internal/images/sdwebui_test.go` does the same for the HTTP route, including that a 404 is
asked about exactly once across two draws. `internal/tools/image_test.go` pins the ids: the
draw is registered under the **calling message**, the reports are filed under it, and the
release runs exactly once. `image_progress_test.go` pins the event's keys, because a renamed
key is not a compile error on either side of the bridge — it is a progress row that silently
stops appearing.

The tool result tells the model, in as many words, that **it cannot see the picture and
must not describe its contents**. The chat model that called the tool has not seen it;
a reply that invents the contents of its own drawing reads as a broken feature rather
than as a capability that is absent.

## 6. Saving where the user wants

`api.go:SaveAttachmentAs` is the only place a picture leaves the app's storage. It
opens the **native save dialog** (`runtime.SaveFileDialog`), so the location is chosen
per picture, decodes the stored base64, and writes the bytes. The folder it was saved
into becomes the next dialog's starting point, because re-navigating to the same place
every time is what makes an export button annoying enough not to use.

A cancelled dialog returns an empty path, which is neither success nor failure, so the
UI says nothing at all. A failure to *remember* the folder is reported as a warning and
does not turn the save into an error: the picture is already on disk.

The dialog's file filter follows the stored MIME type — PNG, JPEG, WebP or GIF — so a
picture the server returned as JPEG is not offered under a `*.png` filter, which is what
made such a file look like it could not be saved here.

`GetAttachmentImage` returns the payload as a `data:` URL for the webview. The stored
document is already base64, so nothing is re-encoded, and a `data:` URL rather than a
`file://` path is deliberate — the webview loads from an embedded assets handler, and a
local path would be blocked by the same origin rule that stops any other local file
from being read.

The frontend caches the data URL per attachment id (`MessageParts.tsx`). A 1024×1024
PNG is hundreds of kilobytes, and the message tree re-renders on every delta of a
streamed reply; re-reading it each time would push that payload through the bridge for
every keystroke. A rejected promise is dropped rather than cached, so a failure that
lasted one startup is not permanent.

A successful save also **records where the copy went** (`attachments.saved_path`, migration 5,
`Store.SetAttachmentSavedPath`). A separate statement rather than a field on
`UpdateAttachment`: that one persists what a reader may edit, and an export is not an edit of
the attachment — it is a note that a copy now exists elsewhere. The note is what tells a
picture its owner has a file of from one that lives only in this database, and that reading is
the whole of §6.1. **Failing to write it is a warning, not a failed save**: the bytes are on
disk, and the note is a reading.

### 6.1 What KiChat keeps, and the button that clears it

An unsaved picture is not nowhere, which is the question the panel answers: a drawn picture is
an ordinary inline attachment (§4), so it lives in the app's own SQLite store — `attachments`
row plus the base64 payload in a `documents` row — for as long as the chat does. Nothing ever
removed one, so a session of drawing quietly grows the data directory and a user who never
presses Save has no way to say they are finished with a picture.

`GetImageCache` and `ClearImageCache` are that way. Three rules make them safe rather than
merely useful:

- **Only pictures KiChat drew are candidates.** A photograph the user dropped into the
  composer is an image attachment too, and it is emphatically not a cache entry: the file it
  came from still exists, this app was simply never told where. The **recipe** is what tells
the two apart — a drawn picture records how it was made, an attached one has nothing to record
  (`Generation != nil`) — and both the reading and the clear go through one helper
  (`App.drawnPictures`) so they cannot disagree about what a candidate is. Deleting an attached
  photo would take a picture out of a conversation to free nothing.
- **Saved pictures stay.** The clear skips every attachment with a recorded export, so what it
  frees is exactly what the panel counts as unsaved.
- **Nothing happens without the press.** There is no automatic cleanup, no size threshold and no
  age rule: this is the one destructive control in the feature, and it is a button.

Both numbers come from **one walk of the attachment table**, so `count`/`bytes` and
`unsavedCount`/`unsavedBytes` cannot contradict each other, and `unsavedChats` is what makes the
warning honest — it is how many conversations lose a picture. The byte counts are the **stored**
size, base64 included: the row a clear deletes is the encoded `documents.text`, about a third
larger than the decoded picture, so the number is what deleting frees rather than what the PNG
weighs. `image_cache_test.go` asserts the
three rules separately, because they fail differently: the attachment survives, the saved
picture survives, and the *cleared* one is gone from the chat's own attachment list rather than
merely from the totals.

**The honest limit, printed in the panel rather than left implicit.** "Unsaved" means this app
recorded no export, which is not quite "there is no copy": a picture saved before this version
carries no note, so it reads exactly like one nobody ever saved. The panel says so above the
button, and the migration comment takes the same line — nothing recorded a copy, so the app
cannot claim there is one. The alternative (treating an empty note as "assume the user has it")
would silently keep every picture, which is the state the button exists to end.

The panel **asks twice**: the second press names the count, the size and how many chats are
affected. "Clear cache" is the kind of label that sounds like it costs a re-download, and what
it actually costs is a picture the chat will never show again — so the last thing before the
deletion is a sentence that says that, in numbers. After it, the row re-reads itself, which is
what proves the press did anything.

## 7. Where it lives

```
internal/images/        types + the two providers: the /sdapi/v1 client (sdwebui.go,
                        sdwebui_test.go) and the sd-cli one (cli.go, proc_unix.go,
                        proc_windows.go,
                        proc_other.go), the optional Interrupter and Progressive,
                        Progress, ErrCancelled
internal/tools/image.go the generate_image tool: arguments, storage, the model's reply,
                        the recipe, Regenerate (a whole recipe in, one picture out),
                        InterruptDraw, and the optional Draws/Progress hooks
internal/config/        config.ImageSettings, DefaultImageBaseURL, NormaliseImageClamps
internal/store/         GenerationRecipe on the attachment, migration 4; SavedPath and
                        migration 5, AllAttachments, SetAttachmentSavedPath
app.go                  rebuildServices builds the provider, or nil when off; inflightDraw
                        and the draws registry, one map for both kinds of draw
api.go                  GetImageStatus, TestImageBackend, GetAttachmentImage,
                        SaveAttachmentAs, PickSaveFolder, RegenerateImage, StopImageDraw,
                        GetImageCache, ClearImageCache
imgs  (frontend)        MessageParts.MessageImages / MessageImage / LiveDraw /
                        RecipeEditor / ToolCallCard, ChatView, Settings.PictureStore
```

## 8. Verification

- `internal/images/sdwebui_test.go` — an `httptest` server for every shape above.
  No network, no keyring.
- `internal/images/live_test.go` — `TestLiveSDWebUI`, the opt-in counterpart and the
  **only** check that can turn §2's flags and shapes from the vendors' claims into
  measurements. It is skipped by default, because drawing on a server the user happens to
  be running for their own work is not something a test suite may do on its own:

  ```bash
  KICHAT_LIVE=1 go test ./internal/images/ -run TestLiveSDWebUI -v
  #   KICHAT_SD_URL (default http://127.0.0.1:7860), KICHAT_SD_AUTH=user:pass
  ```

  It asserts the four things the offline tests can only fake: the server names its
  checkpoints on `/sdapi/v1/sd-models`; the sampler this client sends is echoed back in
  `info`, which is what proves a real server recognised it instead of ignoring it; the
  guidance scale is echoed there too — 2.5, a value neither default would produce —
  which is the only place a guidance scale can be shown *honoured* rather than merely sent;
  and the payload carries PNG magic, so a server answering with a URL or with a `data:`
  prefix — bytes that are not a picture — is told apart from one that answered properly.
  A closed port and a reachable-but-empty server are both reported as *skips* rather than
  failures: the test describes a running server, it does not insist that one exists. It
  draws one 64×64 picture at one step, because the wire shape is what is being verified
  and not the art.
- `internal/images/limits_test.go` — the bounds, and the proof they reach the wire: a table
  over `Normalised()` (zero, negative, over the maximum, and the rounding in both directions,
  plus idempotence), the `httptest` request body the WebUI client actually sent — where a
  negative step count has to be **absent**, not clamped, and where `n` has to be **present and
  1** (the batch nobody asked the server's settings about, §3) — and the command line the stub
  binary recorded. Four guards, each checked by mutating its own fix: dropping the clamp in either
  backend, dropping the tool path's, loosening the locator's ambiguity case, or dropping
  `uniqueName` makes the matching test fail. `TestKeptBoundsOneAnswersPictures` holds the
  answer side of §3's batch rule: an answer inside the bound is passed through untouched, one
  over it is cut to the first four, and the difference stays readable as the count the tool
  result reports.
- `internal/chat/image_tool_test.go` — the tool's visibility contract: with no server
  configured, `generate_image` is absent from the native tool definitions, from the
  rendered `{{tool_list}}` line and from the text-protocol hint, and present in each of
  the three once a provider exists. It drives a real turn and reads the request the model
  actually received, which is the only place all three are visible together.
- `internal/tools/image_test.go` — the whole feature in one test: the precedence of §3 in
  *both* directions — the model's numbers and words reach the provider when the field is empty
  (`TestTheModelDecidesEveryFieldTheUserLeftEmpty`), a fixed value beats what the model asked
  for (`TestASettingTheUserFixedBeatsTheModelsArgument`), and one pinned field does not take the
  other three with it (`TestAFixedFieldLeavesTheRestToTheModel`), which a test of the pinning
  half alone would pass for a builder that dropped the model's arguments entirely — the
  attachment landing
  on the calling message, the document holding base64, the filename following the payload's
  real format, and the "do not describe it" instruction. It runs against a real SQLite
  store in a temp directory. The recipe is asserted against the seed and checkpoint the
  *server* reported rather than the ones requested; a repeat pins the recorded seed while
  a variation drops it; both land on the message the original is on, under names that do
  not collide; and a picture with no recipe is refused before the provider is called. The
  editor's own contract is pinned there too (§5): an edited recipe changes the request,
  leaves the source's recipe untouched, a blank prompt never reaches the provider, and a
  step count outside the bound is clamped in both directions. The batch bound and the slot of
  §3/§5 live here as well, because both are the *service's* rules rather than the app's:
  `TestOneAnswerFilesAtMostTheBound` files four of six pictures and checks that the other two
  reach the model in words, and `TestASecondDrawIsRefusedWhileOneHoldsTheSlot` shows the
  refusal reaching the caller as `ErrDrawInProgress` with the provider never called — and the
  slot released afterwards whether the draw succeeded or failed. The second pass has three of
  its own: the settings-to-request direction plus the recipe it leaves behind
  (`TestGenerateImageAsksForTheSecondPassConfiguredInSettings`, which also asserts a measured
  `DurationMs` from a provider that actually sleeps), the round trip through a redraw
  (`TestRegenerateKeepsTheRecordedSecondPass`), and the refusal of a base-size answer
  (`TestGenerateImageRefusesASecondPassThatDidNotEnlargeThePicture` — the error wraps
  `ErrHiresUnsupported`, names 768x512 and 1152x768, and **no attachment is filed**).
- `image_stop_test.go` (package `main`) — the stop, end to end against a real
  `httptest` WebUI driven through the bound methods: the wait ends and the sentinel comes
  back, the server sees exactly one interrupt, nothing is filed, a server without the
  route still abandons the draw while reporting the refusal, and a Stop with nothing in
  flight says so rather than inventing one.
- `image_draw_slot_test.go` (package `main`) — the slot of §5, through the bound methods: a
  second `RegenerateImage` while one is in flight is refused with `images.ErrDrawInProgress`
  and the first is **still Stop's to end** (the assertion the take-before-register ordering
  exists for), after which the slot is free again; and `TestImageBackend` — the Settings test,
  which on the `cli` kind pays a model load — is stoppable at the fixed `image-test` id and
  answers `ErrCancelled` rather than a red failure.
- `internal/store/store_test.go` — the recipe round-trips, a corrupt one fails the read
  rather than degrading to "no recipe", and an **older database gains the column**: the
  upgrade path is built by dropping the column and rolling `user_version` back, because a
  migration placed at the wrong index is skipped silently and breaks every installation. The
  recipe's second pass and its measured duration survive the same JSON trip, and a recipe
  written before those fields existed decodes with them off.
- `internal/images/limits_test.go` — the guidance scale's own table, because its reading of "unset"
  has to survive a float: zero stays zero rather than being raised to the floor, a negative says
  the same as an empty box, 250 lands on the ceiling, and a NaN — reachable through
  `strconv.ParseFloat("nan")`, and fatal to a draw because `encoding/json` refuses to marshal one
  — folds to unset. The same file's provider tests assert `cfg_scale` on the wire and its absence
  when nobody chose one, and `--cfg-scale` in the command line the other kind runs.
- `internal/config/config_test.go` — the feature ships off, the fields stay empty, and
  the clamps hold; that now includes the second pass (off, with both numbers kept while it is
  off and defaulted to 1.5×/0.4 only when it is on) and the VAE tile side. `Kind` keeps
  exactly the four spellings it has, so `cli`, `sdserver` and `nim` survive as themselves and
  everything else — `server` written out in full included — folds to the empty value that
  means the HTTP server. The hosted model id also gets its own test
  (`TestHostedImageModelDefaultsWithoutAMigration`): a blob written before the field exists
  keeps a model that can draw, and an id this build has never heard of is kept as typed.
- `internal/images/nim_test.go` — the hosted kind, against a stand-in server: the request is
  `POST /v1/genai/black-forest-labs/flux.1-schnell` with the bearer token and a body of
  `prompt`/`mode`/`width`/`height`/`steps`/`seed`, and *carries none of* the OpenAI route's
  `size`/`n`/`response_format`/`sampler_name`/`negative_prompt`; an empty model draws the
  default and a seed the client chose and reports back; unchosen numbers are omitted; both
  answer envelopes (`artifacts[].base64`, `data[].b64_json`) and a stored `url` are read, with
  the type sniffed from the bytes; a hires request is refused with `ErrHiresUnsupported`
  before anything is sent; no key means `ErrNotConfigured` and no request; a 401 is wrapped in
  `ErrKeyRejected` with the echoed key redacted; a 404 becomes `ErrModelNotServed` naming the
  model; a request that never answers says so instead of reporting a raw deadline, while a
  user's Stop stays `context.Canceled`; and `Models` offers only `publisher/model` ids while
  `Samplers` is empty.
- `internal/images/sdserver_test.go` — the managed kind's own unit: the model half of the
  command line with `--listen-ip`/`--listen-port` appended (the user's arguments first, the
  listener last so a typed `--listen-port` cannot outvote the port the app will probe), the two
  new refusals before a process exists — no model at all, and a build whose own `-h` does not
  list the listener flag — and `ServerKey` ignoring the port while following every field that
  reaches the command line.
- `internal/runtime/supervise_test.go` — the supervisor generalisation, on a real process: a
  caller-built `Args` is run *as given* rather than wrapped in llama.cpp's `-m`/`--host`, the
  probe lands on the caller's `ReadyPath`, and the messages carry the caller's `Label`.
  `TestAFailedServerKeepsNoAddress` is the crash *after* ready: the row is running with an
  address, the process exits 4, and the failed row has neither a URL nor a port — the state
  that made a red badge carry a clickable link to a listener that was gone — and a Stop
  afterwards (what a cleanup does) leaves the failure as it is.
- `image_server_test.go` (package `main`) — the whole kind through the app, with this test
  binary playing `sd-server` (the same trick `cleanup_running_test.go` uses for llama-server,
  so nothing outside the Go toolchain has to be installed): the folder supplies the program,
  the start reaches `running` on the route the app probes, the address the row names is the
  address the process answers on, a draw through the tool becomes an ordinary attachment with
  the seed the *server* reported, and Stop takes the provider and the tool away again. A save
  that changes the model leaves the server running and sets the restart reading; the restart
  loads the edited set. `startWithApp` starts it only when set, and the app's own shutdown
  leaves no process behind. `TestTheServerKindSurvivesASaveAndAReload` is the round trip: the
  kind is saved, loaded back through `config.Load`, and used to start a server again — the
  assertion that catches the normaliser folding a known kind away.
- `internal/images/live_sdserver_test.go` — `TestLiveSDServer`, the opt-in counterpart that
  starts a **real** `sd-server` and measures the claim this kind is built on:

  ```bash
  KICHAT_LIVE=1 KICHAT_SD_FOLDER=~/Downloads/sd go test ./internal/images/ -run TestLiveSDServer -v
  #   optional: KICHAT_SD_DIFFUSION_MODEL, KICHAT_SD_MODEL, KICHAT_SD_VAE, KICHAT_SD_LLM,
  #   KICHAT_SD_CLIP_L, KICHAT_SD_CLIP_G, KICHAT_SD_T5XXL
  ```

  Two skips guard it, and both are deliberate: `KICHAT_LIVE=1` says real work may happen, and
  `KICHAT_SD_FOLDER` names the folder — there is no default, because this test does not merely
  talk to a process, it starts one that loads gigabytes and holds them until it is killed. It
  settles what no offline test can: that the published archive really carries `sd-server`
  beside `sd-cli`, that the build accepts the listener flag the app refuses it without, that a
  loaded server answers the readiness route the start hangs on, that this build parses the
  `cfg_scale` key (it comes back in the server's own `info`, and a build that refused an
  unexpected key would fail every draw rather than one), and — the measurement — the
  time to ready followed by the wall time of two pictures, which is where "the load is paid
  once" stops being an argument and becomes a number. The arithmetic is *reported rather than
  asserted*: whether a draw is cheaper than a load depends on the model, the accelerator and
  the step count, so a threshold would fail for a reason that has nothing to do with the
  client. What is asserted is what the client depends on: two pictures come back as real PNGs
  with a seed, and the second pays no load again.
- `internal/images/cli_test.go` — the CLI provider against a stub binary: the flag-by-flag
  command line, the `-m` and `--diffusion-model` shapes, empty fields omitted, the extra
  arguments passed through untouched, the failure tail, and the seed that is *sent* versus
  the one the log *reports* winning. The hires and VAE switches are here as the two opposite
  answers of one rule: a build whose `-h` lists `--hires`, `--hires-scale`,
  `--hires-denoising-strength`, `--hires-width`, `--hires-height`, `--vae-tiling` and
  `--vae-tile-size` gets all seven, a build that lists none of them is
  refused for the first five (`ErrHiresUnsupported`, the missing option named) and simply
  loses the other two — and the refusal happens before a drawing process starts, which the
  recorded command line proves. Its stop test is the decisive one: with the process
  group kill removed, the stub's grandchild holds the pipe and the test waits out the
  minute, which is how the `WaitDelay` above was found.
  `TestTheHelpProbeSurvivesAStoppedDraw` is the memoisation's other half: the `-h` probe is
  asked once and kept for the client's life, so it must not inherit the *draw's* context — a
  draw is exactly what a user stops, and a cancelled probe is remembered as "this build cannot
  answer", which `previewSupport` reads as "no preview options" for the rest of the session.
  The probe runs under `context.WithoutCancel` for that reason, and the test asks with an
  already-cancelled context and expects the build's own option table.
- `image_cli_test.go` (package `main`) — the selection and the whole path through the app:
  `kind: "cli"` builds the CLI provider and anything else the WebUI one, a draw records the
  recipe with the seed the binary reported, a redraw sends that seed back and files beside
  its source, the bound is the CLI's own, and `GetImageStatus` names the files and marshals
  `[]` rather than `null`. Its fake `sd-cli` records each command line it is handed, split by
  a named separator the script and the reader share — a helper that split on a different
  string reported every run but the first as absent, which is how a redraw that did run
  looked like one that did not. The offload switch is asserted here too, because this is the
  only place the setting can be seen to travel from the panel to the binary
  (`TestCLIDrawRecordsTheRecipeAndTheFilesItLoaded`), while the provider's own test pins that
  it arrives *bare* and only when it is on (`TestCLIOffloadToCPUIsPassedBare`). The shared
  parameters are asserted from settings to argv there too — `-W 1024 -H 1024 --steps 8
  --sampling-method euler` — because "the size the user picked is the size that gets drawn"
  is a claim about settings reaching a command line, which the provider's own test (starting
  from a `Request`) cannot make. The guidance scale is where that path shows both directions at
  once: the model asks for 9, the settings row pins 2.5, the recipe records 2.5, and the
  command line carries the user's own `--cfg-scale 1.0` from the extra arguments *followed* by
  the 2.5 — which is the order that makes the recorded value the one that wins. The redraw
  afterwards sends the recorded 2.5 back rather than reading the field again, the same rule the
  seed follows. A row that fills both model slots is refused before the
  process starts (`TestCLIRefusesTwoDiffusionModels`, plus the health subtests).
- `internal/images/locate_test.go` — how the program is supplied: the published-build table
  per platform, including the assertion that **CUDA is not offered for Linux**; the three
  shapes a user's folder can be in (beside it, the archive's own root, that root's `bin`); the
  depth limit as a pinned behaviour rather than an implementation detail; and
  `AcceleratorOf` reading a backend library — with `-hip` standing for ROCm, and a directory
  with none of them meaning the CPU build.
- `image_cli_test.go` — the provisioning path through the app: a folder with the archive's
  shape supplies the program with no path stored anywhere, the detected build travels with it,
  and the program found inside the folder is the one that is started. The precedence rule is
  pinned from the other side too: a folder that holds no program is a failure, and the typed
  path beside it is **not** used (`TestAFolderWithoutAProgramIsNotFallenBackFrom`).
- `image_settings_test.go` (package `main`) — the rules the rest of this document
  states, played through the bound methods the panel calls: the credential reaches the
  secret store and **not** the settings row, an unrelated save does not clear it,
  `GetSecretStatus` reports only a boolean, `GetImageStatus` never marshals a `null`
  slice (the bug this codebase has shipped twice), and a switched-off feature reports
  no failure because nothing was contacted.
- `config.ImageSettings` and `main.ImageStatus` are in the generated bindings, so
  `tsc` fails if the two sides drift.
- The **mock-bridge harness** (`tools/uiharness`) carries a `Bildgenerierung` chat
  whose reply has a generated picture attached, and models every new binding. Its flags for
  this kind are `?images=sdserver` (the third kind, with the folder and the model set filled
  in), `?server=running|starting|failed|restart` (the process row: a loaded model with an
  address and a log, a start still loading — which is what the row's own polling interval is
  for — a start that died, whose log and reason the row shows, and the running row the saved
  settings no longer describe), `?startwithapp=1`, `?images=cli` / `?images=off` for the other
  local kind, and `?images=nim` for the hosted one — no folder, no process and no program,
  with the model select and the borrowed key, plus `&key=none` for the state before the
  Backends key exists (red sentence, Test disabled). The `failed` row deliberately carries **no address**: the port is
  still in its log, because the start recorded it, but `url` is empty and `port` is `0` — the
  shape the supervisor's own `fail()` answers now (§2), because a live-looking link beside a
  red badge points at nothing. `StartImageServer`/`StopImageServer` are modelled as the
  *transitions* they are: a start flips the row to `starting` and settles on a timer, and the
  restart reading is computed from the stored command line rather than stored as a constant,
  so a save that changes the model under a running server really does raise the badge. It
  proves the rendering, the caption, the save button, the Settings section with its
  populated checkpoint and sampler pickers, the test-connection flow, and all five draw
  controls — including the `FHD 1,920×1,280` and `4K 3,840×2,560` shortcuts its 768×512
  recipe produces, which the mock logs as a 960×640 and a 1920×1280 base at 2× rather than
  as a flag. Its recipe is a **two-pass**
  one — `hires` with its scale and denoise beside a
  measured `durationMs` — so the caption's `1.5×` and duration chips and the editor's
  second-pass switch are renderable rather than conditions no fixture ever enters, and the
  `?drawfinish=1` draw streams the same shape: two steps in each of two passes, so the row
  really renders "Pass 2 of 2 · step 1 of 2" instead of a pass count no report ever carries.
  The Settings side is filled the same way: `?pinned=1` also fixes the second pass (on, 1.5×
  at 0.4), because a switch and two boxes that are only ever empty are the state a fresh
  install has and nothing more, and the local fixture runs with `--vae-tiling` on so the tile
  box and the restart badge the `startupKey` comparison raises are rendered too. The mock
  logs the recipe it is handed rather than a flag, which is what makes
  the three separable: an edit shows the changed prompt and step count in the same object
  as the untouched negative prompt and checkpoint, a variation shows `seed: 0`, and a
  repeat shows the stored seed. It also files the drawn picture onto the message (so the
  new one really appears beside its source) under a colliding name bumped to `-2`, and
  refuses a blank prompt — the three behaviours a mock that only toasts a filename would
  hide. Its reply carries a **second, uploaded** picture beside the generated one, because
  a fixture whose only picture has a recipe never renders the state where the draw
  controls are absent. It proves no backend behaviour, which lives in the Go tests.
- Two **draw-stop flags** make the stop path renderable, since a mock that answered
  instantly would leave the whole thing — the button, the pending state, and a cancellation
  arriving as a state rather than as an error — unexercised: `?slowdraw=1` leaves a draw
  pending until Stop is pressed, and `?nointerrupt=1` makes the interrupt route answer 404,
  which is stable-diffusion.cpp. Both print their own `[harness] ... injection active` line
  to the console. `?drawfinish=1` is the third shape and the one the store used to get wrong:
  a redraw reports a few steps, files its picture and emits the `done` **by itself** — start
  it, switch to another chat, and come back. The `done` used to be dropped with the rest of a
  background chat's traffic, so the entry stayed in the store and the chat painted a progress
  row, and a Stop button, for a draw that was already over; the fix is that `done` is applied
  before the open-chat guard (`frontend/src/store.ts`), because it costs one map delete and
  the alternative is a row that lies. `StopImageDraw` **holds its answer for 150 ms**, the
  real interrupt request, because that is the interval the view's controls are disabled
  across — answering at once would make that state unrenderable. Driven with them: the
  editor's Draw becomes a red Stop in place (the button the click came from is the button
  that stops it), the caption grows a Stop beside Redraw and Variation while one of those is
  running, the toast for the abandoned draw appears **while the other controls are still
  disabled** and they come back once the interrupt has answered, stopping files **no**
  picture, `wasDrawing` false on a later call produces no toast at all, and the 404 case shows
  the stop toast rather than an error.
  Its fixture picture is an SVG data URL rather than a PNG because a fixture only has to be
  visible at size; the bytes the real binding returns are asserted in Go.
- `?images=cli` switches the harness fixture to the second kind and fills its fields, so both
  halves of the Settings section — the server's address and credential, the program's path, its
  model set and its offload switch — are renderable from one page. The badge changes with it:
  the CLI kind counts the files it will load, because "0 models" on  a backend that cannot be asked for a listing would read as a broken setup rather than as a
  question it cannot answer. Its folder field is filled and its program path left empty — the
  state a user is in when KiChat found `sd-cli` for them — and `PickImageFolder` answers with a
  different folder, so a click is observable as a change rather than as the same string coming
  back.
  The checkbox is left **checked** in that fixture for the reason the second picture exists:
  a control that only ever renders one state cannot show whether its binding is wired at all.
  Two more states of the same fixture are the ones a user actually hit. `?images=off` keeps
  every field of the row filled in and switches the feature off, which is what an unticked box
  means — no `generate_image` tool at all, however complete the row is (§5) — and the panel
  says so under the checkbox instead of leaving it to a badge. `?typedbinary=1` adds a typed
  program path pointing at a *different* folder, the pair that reads as double: it is the
  state the disabled field, its note and the `Gefunden:` line exist for, and a fixture that
  only ever held one of the two could not show that rule at all.
- `?profiles=none` is the profile list's empty state — a fresh install, or one whose profiles
  were all deleted — where the dropdown has only its placeholder and Apply, Delete and the
  save button are all inert. Driven with the fixture list against the mock bridge: picking the
  `cli` profile switched the backend selector, the sampler, the step count and the size while
  the feature switch and the export folder stayed as they were; a save under a new name appeared
  in the dropdown with the button relabelled *Update profile*; updating it changed what a later
  Apply restored (steps 20 → 12, and the list still held four entries rather than five); and the
  delete asked once with the profile's name in the sentence, let Cancel put the plain button
  back, and on the second press removed the row and cleared the selection and the name box.

## 9. The caption, because FLUX 3 was trained on one

A FLUX 3 draw does not take the sentence a user typed. BFL trained the model on captions
written by a prompt upsampler — a 9 B vision-language model they call the PUP — and their
README says not to run the model without it: a raw prompt gives "noticeably weaker" results,
and `--no-pup` is how their CLI spells that weaker path. The PUP's own model card says the
model is replaceable — *"Any other LLM can do the job with the system prompts in
`system_prompts.json`"* — and that sentence is the whole feature here: **the chat model that
already called `generate_image` writes the caption**, so the 18.8 GB upsampler is never
downloaded and no second model is configured.

### 9.1 The prompt is BFL's, verbatim

`internal/images/caption.go` holds the `t2i` prompt from
`src/flux3/pup_prompts.json` (also published as `system_prompts.json` beside the PUP
weights), Apache-2.0. Measured from that file: **506 words**, no backtick and no brace, so it
lives in one raw string literal. A test pins the structural invariants — the opening sentence,
`TARGET CANVAS`, `Output only the caption.` — rather than a hash, which any whitespace edit
would break. Paraphrasing it is not an option: a paraphrase is a different prompt with
different behaviour, and calling it BFL's would be a lie.

The `i2i` prompt (420 words) is deliberately absent: it captions an *edit*, and a request
has no reference image to attach until §10's `img2img` work exists. The same file's
`t2i_bbox`/`i2i_bbox` are absent for a sharper reason — a bounding box is a **separate JSON
file** of `[y_min, x_min, y_max, x_max]` in 0–1 in the reference CLI, not prompt syntax, and
`sd.cpp` documents neither. A box prompt today would be a caption with coordinates in it.

### 9.2 Who writes it

`ImageSettings.Captioning`, four values. The empty one is **"the profile decides"**, not
"off": a checkpoint that was trained on captions knows it, and the user should not have to.

| Value | Who writes the caption | Cost |
| --- | --- | --- |
| `""` | the pipeline (§9.5) — `auto` for FLUX 3, nobody for every other profile | as below, per pipeline |
| `off` | nobody, even where the profile wants one | none, and BFL's weak path |
| `auto` | KiChat asks the turn's own model before drawing | one non-streaming completion |
| `model` | the chat model, as the `prompt` argument of its own tool call | none, but a long argument replayed on later turns |

`off` exists because it is the only value that can override a profile: "nobody chose" and "the
user chose nothing" looked identical in the first version, which made a profile's default
impossible to refuse.

The two are exclusive and never chained: `model` after `auto` would expand an expansion. A
request that is already caption-length (120 words, BFL's own lower bound) is passed through
untouched in either mode, for the same reason.

### 9.3 The call

`generate_image` → `tools.Service.Caption` (a callback, because `tools` must not import
`chat`) → `app.captionImage` → `chat.Caption` → `chat.Complete`. The turn's backend and model
travel in the tool context (`tools.WithTurn`), written by the engine when it executes the call,
because two chats can draw at once and the service outlives a turn.

- Messages: system = the prompt verbatim; user = `TARGET CANVAS: W:H` and then the request.
  The canvas line is a t2i convention — the `i2i` prompt has none.
- Decoding: **greedy**, thinking off, **3,584** new tokens, no tools, no streaming. Greedy is a
  zero *and* `TemperatureSet`, because `ollama.go` drops a temperature of zero unless the
  caller said it meant it — otherwise the modelfile's temperature samples the caption.
- `chat.Complete` returns the text channel only, so a reasoning model's analysis cannot reach
  the backend. A model that thinks its whole budget away returns an error, not an empty prompt.
- `CleanCaption` strips code fences, wrapping quotes, a JSON wrapper and a "Here is the
  caption:" line, collapses newlines to one block, and refuses an empty result or more than
  800 words. Bounds: 120 words is "already a caption"; 800 is a runaway generation, refused
  rather than trimmed mid-sentence.

Why this cannot deadlock a single-slot server: tool calls run **after** the provider's stream
has been consumed (`generate.go`), so the caption request reaches an idle backend. That is a
property of this loop, and it is why the same model can be both.

### 9.4 What happens around the draw

- A failed or empty caption **does not fail the draw**. The request is drawn as typed and the
  tool result says the caption did not happen.
- The recipe records the caption as `Prompt` (it is what reproduces the picture), the user's
  words as `SourcePrompt`, and the model as `CaptionModel`. A redraw replays the caption with
  no completion at all — and no re-expansion.
- The caption never enters the transcript: it is a request field, not a message.
- A `captioning` phase goes out on the draw's progress channel before step 1, so seconds of
  silence are not read as a hang. It is a `phase` key in the `image:progress` payload, pinned by
  `image_progress_test.go` on both sides of the bridge.

**Not measured here**: how much the caption buys on a real Klein build at 16 steps. That A/B
(one seed, raw request versus caption) is `FLUX3Plan.md`'s Phase 0, and nothing above claims a
quality result — only the mechanism BFL says the model needs.

### 9.5 The pipeline profile

`internal/images/pipelines.go` turns "which model is configured" into the numbers and
restrictions that model wants, so a FLUX 3 draw is right the moment the diffusion file is
chosen. It is a table, not a set of branches: `Pipeline` carries `Steps`, `CFG`, `Sampler`,
`SupportsNegative`, `SupportsHires`, `Captioning`, the `Encoder` arrangement and
`RequiresVAE`, and nothing outside it asks whether a model is FLUX 3.

| Profile | Steps / CFG / sampler | Negative | Second pass | Caption |
| --- | --- | --- | --- | --- |
| `flux3-klein` | 16 / 1.0 / euler | dropped *(measured inert)* | kept *(measured)* | on |
| `flux3-klein-base` | 20 / 3.5 / euler | kept | dropped *(unmeasured)* | on |
| `generic` (and every unrecognised file) | the build's own | kept | kept | off |

The numbers are stable-diffusion.cpp's own (`docs/flux3.md`: 16 / 1.0 for the distilled
model, 20 / 3.5 for base). `SupportsNegative: false` is the distilled checkpoint's CFG 1.0:
a negative prompt there is sent, recorded and ignored, which reads as a broken setting rather
than as one the model does not have — and that "ignored" is now a measurement, not an
inference: at CFG 1.0 the same seed draws **byte-identical pixels** with and without `-n`,
while at CFG 7 the same pair differs (the control that proves the flag was honoured at all).

`SupportsHires` is where measurement changed the table. It was `false` on both FLUX 3
profiles because a second pass had never been drawn here; the distilled one has now been
drawn, so it is `true`. Base keeps the refusal, because no Base checkpoint exists on this
machine and "like the other checkpoint" is exactly the assumption this field exists to
refuse.

**Resolution.** `ImageSettings.Pipeline` is empty by default: the profile is read from the
diffusion model's own header — the tensor layout, which a rename, a re-quantisation and a
folder name all leave alone — and the file name is consulted only when that header cannot be
read at all. An explicit id overrides both, and `generic` is the deliberate "no profile".
`GetImageStatus` reports the **resolved** id, name and source, and the panel prints them —
because a miss is otherwise silent: a generic profile captions nothing and draws at the
build's defaults, which looks like a broken feature rather than an unread file.

#### How a checkpoint identifies itself

`internal/images/modelmeta.go` reads the header of the file that is about to be loaded: GGUF's
tensor table (after its key/value table) and safetensors' length-prefixed JSON keys. What was
measured on 2026-10-18, by fetching the first megabyte of each published file with a ranged
request and running the shipped reader over it — every row below is reproducible with
`inspectModel` on a partial download:

| File | Format | Metadata that names the model | Tensors | `ref_time_in.*` |
| --- | --- | --- | --- | --- |
| `flux-3-image-klein-comfy` → `flux3-image-klein-bf16.safetensors` | safetensors | `model=f3-image-small`, `dtype=bfloat16` | 359 | present |
| … → `flux3-image-klein-base-bf16.safetensors` | safetensors | the same three keys | 357 | absent |
| `unsloth/flux-3-image-klein-GGUF` → `flux-3-image-klein-BF16.gguf` | GGUF | `general.architecture=flux`, `model=f3-image-small` | 359 | present |
| `unsloth/flux-3-image-klein-base-GGUF` → `…-base-BF16.gguf` | GGUF | the same two | 357 | absent |
| `unsloth/flux-3-image-klein-base` → `model.safetensors` | safetensors | `model=f3-image-small` | 357 | absent |
| `Abiray/flux-3-image-klein-sd-GGUF` → `flux3_image_klein_sd_Q2_K.gguf` | GGUF | **none** — 0 key/value pairs | 359 | present |
| `webmp3/Sakura-FLUX.3-Image-Klein-GGUF` (a fine-tune) | GGUF | `general.architecture=flux` | 359 | present |

Three consequences, all load-bearing:

- **No metadata value says "klein".** BFL's own repack names the model `f3-image-small` in
a distilled and a base file alike, and one community conversion carries no values. A reader
searching the key/value table for the word would recognise almost nothing.
- **The architecture is legible from the tensor names.** `content_mode_blocks.*` and
`txt_mode_blocks.*` appear in FLUX 3 and in neither FLUX 1 (`double_blocks.*`) nor
Qwen-Image, so a family test needs no metadata at all.
- **The distilled checkpoint carries two tensors base does not** — `ref_time_in.in_layer.weight`
and `ref_time_in.out_layer.weight`, 359 entries against 357. That is the only difference
observed between the variants, and it is what separates the profiles.

So `Pipeline.Tensors` lists prefixes that must appear and `Pipeline.NotTensors` those that
must not: base requires the FLUX 3 pair **and** the absence of `ref_time_in.`, so the
distilled layout cannot be mistaken for it whatever order the table is read in.

**The header outranks the name in both directions.** A file named `…klein…` whose tensors are
FLUX 1 draws generically — no 16 steps, no caption — and a file called `weights.gguf` with
Klein's tensors draws as Klein. The name is the fallback only when nothing could be read (a
path absent on this machine, a truncated download, an unknown format), and `Pipeline.Source`
(`chosen`, `weights`, `name`) travels to the panel so the line under the select says which
claim it is: "Recognised from the tensors in the model file: …" or "Detected from the file
name: …".

**Cost and limits.** One bounded read per file: 256 KiB for GGUF, retried at 8 MiB if the
key/value table runs past that; for safetensors the header's own stated length, capped at
16 MiB. The tables above end at 26,274–26,502 bytes as GGUF and 40,328–40,536 bytes as
safetensors. The result is cached by path, size and modification time, because the profile
is resolved on each settings change and on several paths inside one draw.

**The weak spot is the base variant**, which is identified by *absence* — a FLUX 3 layout
without `ref_time_in.` resolves to Base, including a hypothetical future FLUX 3 checkpoint
that has the same shape. A fine-tune that strips those two tensors would be drawn at 20 /
3.5 instead of 16 / 1.0, and the panel would say it came from the weights. No published file
that was measured has that shape.

**What it may change.** Only zeros are filled: a step count the model asked for or the user
fixed wins, exactly as `pinInt`'s rule. What a profile cannot use is removed rather than
refused, because those switches are global and a user who set one for another model would
otherwise be unable to draw at all; the tool result and the recipe both record that it did not
happen. Today that is the negative prompt on the distilled checkpoint and the second pass on
Base — the distilled profile's second pass is *kept*, now that it has been drawn. A redraw
goes through the same adjustment, so a stored recipe cannot draw a second pass under a model
that has none.

**Health names the part, not the option.** With a profiled model the CLI backend reports
"FLUX 3 Image Klein braucht einen Text-Encoder (`--llm`)" (or the VAE, or the diffusion
model) instead of the generic "kein Modell angegeben", which would send the user to the
wrong field and fail after a multi-gigabyte load. That message was checked against the real
binary: with the diffusion model and the encoder set and no `--vae`, `Health` returns
"FLUX 3 Image Klein braucht einen VAE (--vae)", and the draw it pre-empts fails in 0.8 s with
`model metadata validation failed — first_stage_model.*` (below).

#### Measured on a real build

Until 2026-10-18 the profile's numbers were documentation. They have now been drawn on a
machine that has the checkpoint: `sd-cli` commit `95b3090` (Vulkan) on an RX 7900 XT, with
`flux-3-image-klein-UD-Q4_K_XL.gguf` (3.9 GB), `Qwen3VL-4B-Instruct-Q4_K_M.gguf` as `--llm`
and — see below — a FLUX 2 VAE.

| What | Measurement |
| --- | --- |
| Reading the checkpoint | 359 tensors, `general.architecture=flux`, `model=f3-image-small`; 3.2 ms, 2.8 µs cached |
| A renamed copy | a hard link named `weights.gguf`, with no marker anywhere in its path, resolves to `flux3-klein` from `weights` and draws |
| 512², 16 steps, CFG 1.0, euler | 549,661-byte PNG in **10.9 s** through `images.CLI` (17.1 s for a cold run outside the app), 3.57 it/s, 6.3 GB in VRAM (2.4 GB encoder, 3.7 GB diffusion, 0.16 GB VAE) |
| The negative prompt at CFG 1.0 | byte-identical pixels with and without; *different* at CFG 7 |
| A second pass, 512² → 768² at 1.5 / 0.4 | a clean 768² in 18.4 s, against 7.3 s for the base draw alone |
| A direct 768² draw at 16 steps | 13.3 s — 5 s less than enlarging into the same size |
| Progress reports | 12 from the sampler, the last at 14 of 16: no report ever names the final step |

**The VAE is not optional, and a FLUX 2 one works.** With no `--vae` the draw is refused
before any sampling — `model metadata validation failed`, after listing the
`first_stage_model.*` tensors it could not find — which is exactly the state a Klein folder
holding only a diffusion model and an encoder is in, and exactly what the Health message
above pre-empts. `flux2-vae.safetensors` from the FLUX 2 set then drew correctly with no
`--vae-format`: sd.cpp's log calls it `Using 'flux2' preset`. BFL's own `ae.safetensors` was
not tried, so what is measured is that a FLUX 2 VAE is accepted, not that the two files are
the same.

**Still unmeasured.** The caption A/B below (no chat backend was running), a Base checkpoint
(none on this machine), and whether enlarging is *better* than sampling at the target size
directly — it costs 5 s more here, and nothing about quality was judged beyond the picture
being coherent.

## 10. What it does not do yet

- **ComfyUI** (§2) and every *other* hosted image API — OpenAI's images route, Stability,
  fal, Replicate, Cloudflare Workers AI. NVIDIA NIM is the one that was built (§2), because
  the app already stores that key; the rest are surveyed in [`IMAGES-CLOUD.md`](IMAGES-CLOUD.md).
  `Provider` has no HTTP in it, which is what let this be a fourth backend rather than a new
  interface.
- **A per-picture key for the hosted kind.** One NVIDIA token serves chat and pictures, on
  purpose: two stored values for one account is two things to rotate, and the second would
  usually be the WebUI's `user:pass` reused by mistake.
- **A one-shot draw's model load is still silent.** Its sampling reports (§5) — the binary's
  own step lines and the `--preview` file it writes — but the load before the first step prints
  nothing this app can turn into a fraction, so a first draw on a CPU still spends minutes
  behind an indefinite spinner. On the managed server the same load is a `starting` row with a
  bound on it, which is the other half of the same problem.
- **A picture KiChat drew is not sent back to the model, and that is now decided.** It was
  predicted here as unverified: `context.go` attached a message's inline images to the
  message that owns them, which for a generated picture is the assistant turn, and
  `openai.go` rendered an image part for any role. The failure was measured on Groq — the
  next turn was refused with a validation 400, not silently starved of the picture
  (GROQ.md §6) — so `Assemble` and `toWireMessages` keep an image off an assistant turn
  altogether: the model reads its `generate_image` call and the tool result instead. The
  picture is still stored, shown, saved and redrawn, and **no vision capability is needed to
  call the tool** — which is what makes it available to every text-only model that can call
  tools at all. Giving the picture back to a vision model on a later turn would mean filing
  it on a user turn, which the transcript does not do.
- **The store only knows about an export it recorded.** A picture saved before `saved_path`
  existed counts as unsaved (§6.1), and there is no per-picture delete in the panel: the whole
  unsaved set goes at once, with no list to pick from. A list would need a browsing surface
  (thumbnails, a chat per picture, a size per picture) that this panel deliberately is not.
- **A draw that hits its bound leaves the server drawing.** `StopImageDraw` interrupts, but a
  timeout only abandons the wait: KiChat hangs up and the server finishes a picture nobody
  will see. Sending the interrupt on timeout was considered and left out, because the interrupt
  is *global* to the server process — ten minutes after a request was abandoned it could kill a
  generation the user started in their own WebUI tab, which is the same hazard
  `Interrupt`'s doc refuses to risk by retrying.
- **`img2img`, inpainting, upscaling** — same server, another route.
- **A grid for several pictures.** The client already returns every image the server
  produced and files each one; nothing yet asks for more than one per call.
- **The managed server holds its model until the user stops it,** and there is deliberately no
  idle timeout that gives the memory back: the point of a resident model is that it is already
  loaded, so a timer would release it exactly when the next picture was about to be asked for.
  The price is stated in the row, and Stop is in front of the user (§2).
- **One pipeline per process, so a model switch is a restart.** `sd-server` creates its
  context once before it listens and documents no way to replace it, which is why Start doubles
  as Restart and why `ServerKey` and the **Restart needed** badge exist rather than a quiet
  reload. Stopping and starting again costs the whole load — minutes on a CPU.
- **Stop against a *draw* is best effort on this kind.** `sd-server` has no
  `/sdapi/v1/interrupt`, so cancelling ends the wait and the server finishes a picture nobody
  will see, the same hazard §5 describes for the WebUI family. Its native API cancels by *job
  id*, and this client does not submit jobs.
- **The load-once arithmetic is an argument here, not a measurement.** No `sd-server` was
  available on the machine this was written on — the same caveat §2 carries for the WebUI
  flags — so "a server pays the load once and then only the sampling" is the trade the CLI kind
  states, not a number this repo has. `TestLiveSDServer` (§8) is what turns it into one, and it
  also settles the assumption the folder lookup rests on: that the published archives really
  carry `sd-server` beside `sd-cli`.
