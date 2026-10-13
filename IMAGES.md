# IMAGES.md

Text-to-image generation: the tool the chat model calls, the server behind it, and
how a picture reaches the window and the user's disk. `AGENTS.md` points here; the
measured detail lives here and nowhere else.

---

## 1. What it is, and what it deliberately is not

The chat model gets one more tool, `generate_image`. When it calls the tool, KiChat
draws the picture through a **Stable Diffusion WebUI the user runs themselves**,
files it as an attachment of the reply that asked for it, and the window shows it.

It is the first part of KiChat that produces a file rather than text, and the design
follows from one rule: **the picture is an image attachment, exactly like one the
user uploaded.** Nothing downstream needed a new concept. The assembly already turns
an inline image attachment into an image part of the request
(`internal/chat/context.go`, `TestAssembleSendsImagesAsImageParts`), so a generated
picture can be handed to a vision model on the next turn with no new plumbing; the
frontend already renders a message's attachments, so it needed only a component that
paints a picture instead of a chip. §4 is that argument in full.

Two things it is **not**:

- It is not a hosted image service. There is no API key, no metered endpoint, and
  nothing leaves the machine. That is the same hard requirement the rest of the app
  has, and it is why the first backend is a local one.
- It is not `img2img`, inpainting or a LoRA browser. Those are routes on the same
  server, and adding them is a request field each rather than a new design.

## 2. One client, three servers — and a second kind that needs no server

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
outside that verified set travels in `ExtraArgs`, which is how a turbo model's `--sigmas`,
`--cfg-scale` and `--diffusion-fa` stay reachable without a new KiChat release. A working
command line copies across field for field:

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
server's. Under the size boxes sit three buttons — 512², 768², 1024² — because they are the
square sizes diffusion models are trained at or near and the alternative is typing the same
number into two fields. Anything else is still typed, so the presets add a shortcut and take
nothing away.

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
`--cfg-scale`, `-v` and `--diffusion-fa` are the extra arguments, because each of them
carries a value or a spelling this app has no reason to model.

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
Stop and keep grinding. `cmd.WaitDelay` bounds the wait regardless: `Wait` returns only once
the output pipes close, and a killed grandchild holding the write end open is exactly what a
Stop that "did nothing" looks like from the window. That bound was found by a test, not by
reading the docs.
- **CPU offload is a switch, not text.** `--offload-to-cpu` gets a checkbox
(`ImageSettings.CLIOffloadToCPU`) because it is a *mode* a user flips per model — Flux on a
card too small to hold it is the case it was asked for — and it is passed **bare**, with no
value, spelled the way a working command line spells it. It is deliberately the *only* flag
with a control of its own: every flag that carries a value stays in the extra arguments,
where a renamed option costs nothing. It is also not part of a recipe — it changes where the
work happens, not what the picture is — so a redraw reads it from settings rather than from
the stored picture.
- **The draw bound is this backend's own** (`images.DefaultCLITimeout`, 30 minutes). A bound
carried over from the server unchanged would cut a real draw off mid-load and report it as a
failure; Stop, not the bound, is the control for a draw that is merely slow.

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
because a build that does not answer that option can still draw. The `Kind` itself is
normalised by `config.NormaliseImageKinds`: only the literal `cli` is stored as itself, and
everything else — including the server kind spelled out in full — folds to the empty string
that already means the HTTP server, so a hand-edited blob degrades to the backend that used to
work rather than to none at all.

## 3. Empty means "leave it to your server"

Every generation field in Settings may be left empty, and an empty one is **omitted
from the request** rather than sent as a default:

| field | empty means | why |
| --- | --- | --- |
| server address | `http://127.0.0.1:7860` | AUTOMATIC1111's own default, and what Forge and sd-server use |
| credentials | no `Authorization` header | a WebUI ships with no authentication; sending one anyway is a request it may refuse |
| checkpoint | no `override_settings` | the user's WebUI has a model loaded; overriding it with a guess ignores their setup |
| sampler, steps, size | omitted | same reason — their WebUI already has a choice |
| negative prompt | omitted | an empty *sent* string would **replace** a negative prompt configured in their WebUI; omitting the field leaves it in force |

This is the same instinct as the model picker offering the GGUF files a user already
has: the app must not quietly substitute its own guess for a decision the user made
elsewhere. The Settings section says so under the fields.

The numbers are bounded in **one place**: `images.Request.Normalised()`
(`internal/images/limits.go`) clamps a side to 64–2048 and rounds it to the multiple of 8
every diffusion model's latent grid needs, clamps the step count to 150, and leaves 0 — and a
negative, which is the same statement — alone, because 0 is the spelling of "the server's
own". `config.NormaliseImageClamps` applies those bounds to a stored blob too, and the
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

## 4. The picture is an attachment, and that is the whole trick

`internal/tools/image.go` writes a generated picture with the same two rows an
uploaded image gets (`api.go:AddAttachment`):

```
documents.text   = base64.StdEncoding.EncodeToString(pngBytes)
attachments.kind = "image", mime = "image/png", placement = "inline"
```

No schema change, and three properties fall out for free:

1. **The assembly sends it to a vision model.** `context.go` already routes an inline
   image attachment into `llm.Message.Images`, and `llm.EstimateImageTokens` prices it,
   so the context meter counts a generated picture the way it counts an attached one.
2. **The frontend renders it.** `ChatView` already renders `message.attachments`; the
   new `MessageImages` component in `MessageParts.tsx` paints the ones whose kind or
   MIME is an image, and `AttachmentChips` skips those so nothing is listed twice.
3. **Deleting it works.** `DeleteMessagesFromSeq` and the chat cascade already clean
   up attachments and their documents.

### The recipe: what makes a picture drawable again

The picture is an attachment; the *recipe* is a column on it. `attachments.generation_json`
holds the prompt, the negative prompt, the size, the steps, the sampler, the seed and the
checkpoint, and it is what turns "draw that again" into a control instead of a
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

A third way exists that needs no model at all: the **controls under a picture**
(`MessageParts.tsx` → `api.go:RegenerateImage` → `tools.Service.Regenerate`). They read the
recipe off the attachment (§4) and call the provider directly, so drawing again costs GPU
seconds and no tokens. The result is filed onto the **message the original is on**, which is
what puts it beside its source — an attachment on no message renders nowhere — and the
backend emits `chat:attachments` afterwards, the same event the tool path uses, which is
what makes the new picture appear without a reload.

There are three of them, and the binding takes a **whole recipe** rather than a flag, which
is what makes three controls out of one call:

- **Redraw** passes the stored recipe back untouched — the same picture.
- **Variation** passes it with the seed cleared, so the server chooses a new one.
- **Edit recipe** opens the recipe in a form under the picture — prompt, negative prompt,
  size, steps, seed, checkpoint, sampler — prefilled from the picture's own, and **Draw**
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

### Stopping a draw

A picture a CPU server takes minutes over has to be abandonable, and `StopImageDraw` does
it in **two halves that each do something the other cannot**:

- It **cancels the request's context** ([`api.go`](api.go) → `tools.Service.Regenerate` →
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
  user is drawing in their own WebUI tab stops too. KiChat draws one picture at a time
  against a server the user runs for themselves, so the window is narrow; a caller pressing
  Stop should still know that is what it means.
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
  comes from the same answer.
- **The program** (`sd-cli`) writes a preview image to the path `--preview-path` names — the
  file is rewritten on every denoiser step, so a poll of it is a live picture of the draw — and
  prints the sampling step into its log as a bar (`  |=====>    | 3/12 - 2.20s/it`). The log is
  now read **line by line** rather than collected and read at the end, because two of the three
  things in it are only useful while the draw is running: the step, which is the progress, and
  the seed, which makes a recipe record the number the program really used. The recent lines are
  kept in a ring (`cliLogLines`), which is what a failure prints.

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

- **The end of a draw is the same event with `done` set**, not a second name, and it is sent
  **only for a draw that was reported on**. Its whole purpose is to clear a progress row off
  the screen; a draw nobody reported on has no row, and the event exists so a Stop button
  cannot outlive the thing it stops.
- **Registration is not reporting.** `tools.Service.Draws` is called for every draw — that is
  what makes a silent backend's draw stoppable — while `tools.Service.Progress` is only called
  by a backend that implements `Progressive`.
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

`internal/images/cli_test.go` drives a fake `sd-cli` that answers `-h`, rewrites a preview file
and prints step bars: two reports arrive, the first carries the step and a real PNG preview,
the last carries the second step, and the seed line is still read — the draw is unaffected by
having been watched. Two more tests hold the negative side, which is the one that costs a user
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

## 7. Where it lives

```
internal/images/        types + the two providers: the /sdapi/v1 client (sdwebui.go,
                        sdwebui_test.go) and the sd-cli one (cli.go, proc_unix.go,
                        proc_other.go), the optional Interrupter, ErrCancelled
internal/tools/image.go the generate_image tool: arguments, storage, the model's reply,
                        the recipe, Regenerate (a whole recipe in, one picture out) and
                        InterruptDraw
internal/config/        config.ImageSettings, DefaultImageBaseURL, NormaliseImageClamps
internal/store/         GenerationRecipe on the attachment, migration 4, the queries
app.go                  rebuildServices builds the provider, or nil when off
api.go                  GetImageStatus, TestImageBackend, GetAttachmentImage,
                        SaveAttachmentAs, PickSaveFolder, RegenerateImage, StopImageDraw
imgs  (frontend)        MessageParts.MessageImages / MessageImage / RecipeEditor, ChatView,
                        Settings
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

  It asserts the three things the offline tests can only fake: the server names its
  checkpoints on `/sdapi/v1/sd-models`; the sampler this client sends is echoed back in
  `info`, which is what proves a real server recognised it instead of ignoring it; and
  the payload carries PNG magic, so a server answering with a URL or with a `data:`
  prefix — bytes that are not a picture — is told apart from one that answered properly.
  A closed port and a reachable-but-empty server are both reported as *skips* rather than
  failures: the test describes a running server, it does not insist that one exists. It
  draws one 64×64 picture at one step, because the wire shape is what is being verified
  and not the art.
- `internal/images/limits_test.go` — the bounds, and the proof they reach the wire: a table
  over `Normalised()` (zero, negative, over the maximum, and the rounding in both directions,
  plus idempotence), the `httptest` request body the WebUI client actually sent — where a
  negative step count has to be **absent**, not clamped — and the command line the stub binary
  recorded. Four guards, each checked by mutating its own fix: dropping the clamp in either
  backend, dropping the tool path's, loosening the locator's ambiguity case, or dropping
  `uniqueName` makes the matching test fail.
- `internal/chat/image_tool_test.go` — the tool's visibility contract: with no server
  configured, `generate_image` is absent from the native tool definitions, from the
  rendered `{{tool_list}}` line and from the text-protocol hint, and present in each of
  the three once a provider exists. It drives a real turn and reads the request the model
  actually received, which is the only place all three are visible together.
- `internal/tools/image_test.go` — the whole feature in one test: defaults from
  settings, the model's arguments winning, the attachment landing on the calling
  message, the document holding base64, the filename following the payload's real
  format, and the "do not describe it" instruction. It runs against a real SQLite
  store in a temp directory. The recipe is asserted against the seed and checkpoint the
  *server* reported rather than the ones requested; a repeat pins the recorded seed while
  a variation drops it; both land on the message the original is on, under names that do
  not collide; and a picture with no recipe is refused before the provider is called. The
  editor's own contract is pinned there too (§5): an edited recipe changes the request,
  leaves the source's recipe untouched, a blank prompt never reaches the provider, and a
  step count outside the bound is clamped in both directions.
- `image_stop_test.go` (package `main`) — the stop, end to end against a real
  `httptest` WebUI driven through the bound methods: the wait ends and the sentinel comes
  back, the server sees exactly one interrupt, nothing is filed, a server without the
  route still abandons the draw while reporting the refusal, and a Stop with nothing in
  flight says so rather than inventing one.
- `internal/store/store_test.go` — the recipe round-trips, a corrupt one fails the read
  rather than degrading to "no recipe", and an **older database gains the column**: the
  upgrade path is built by dropping the column and rolling `user_version` back, because a
  migration placed at the wrong index is skipped silently and breaks every installation.
- `internal/config/config_test.go` — the feature ships off, the fields stay empty, and
  the clamps hold; `Kind` folds to its two spellings, so only `cli` survives as itself.
- `internal/images/cli_test.go` — the CLI provider against a stub binary: the flag-by-flag
  command line, the `-m` and `--diffusion-model` shapes, empty fields omitted, the extra
  arguments passed through untouched, the failure tail, and the seed that is *sent* versus
  the one the log *reports* winning. Its stop test is the decisive one: with the process
  group kill removed, the stub's grandchild holds the pipe and the test waits out the
  minute, which is how the `WaitDelay` above was found.
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
  from a `Request`) cannot make. A row that fills both model slots is refused before the
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
  whose reply has a generated picture attached, and models every new binding. It
  proves the rendering, the caption, the save button, the Settings section with its
  populated checkpoint and sampler pickers, the test-connection flow, and all three draw
  controls. The mock logs the recipe it is handed rather than a flag, which is what makes
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
  to the console. `StopImageDraw` **holds its answer for 150 ms**, the real interrupt
  request, because that is the interval the view's controls are disabled across — answering
  at once would make that state unrenderable. Driven with them: the editor's Draw becomes a
  red Stop in place (the button the click came from is the button that stops it), the
  caption grows a Stop beside Redraw and Variation while one of those is running, the toast
  for the abandoned draw appears **while the other controls are still disabled** and they
  come back once the interrupt has answered, stopping files **no** picture, `wasDrawing`
  false on a later call produces no toast at all, and the 404 case shows the stop toast
  rather than an error.
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

## 9. What it does not do yet

- **ComfyUI** (§2), and any hosted image API (OpenAI's `/v1/images/generations` with
  `b64_image`, Stability, fal, Replicate) behind the same `images.Provider` interface.
  The interface was written for that: `Provider` has no HTTP in it. The CLI kind is the
  second `Provider` and is what showed the interface holds two unlike backends at once.
- **The CLI kind never streams progress**, and there is nothing to poll: the binary writes
  its PNG at the end. A draw reports the tool status pill and then the picture, like the
  server's synchronous route. The child's own log is collected into a buffer and read only when
  the run ends, so a first draw spends minutes on a model load behind an indefinite spinner;
  streaming it would be the difference between "working" and "stuck".
- **A picture KiChat drew travels in an *assistant* message, and that is unverified.**
  `context.go` attaches a message's inline images to the message that owns them, which for a
  generated picture is the assistant turn, and `openai.go`/`ollama.go` render image parts for
  any role. The servers in the table commonly accept image content **only in user turns**, so
  the failure mode is not "the model cannot see it" but "the next turn is refused" — and no
  server was reachable from here to tell the two apart. Everything else about the picture
  works: it is stored, shown, saved and redrawn. Deciding this properly is a per-backend
  answer (send it, or name it in text), which is not written yet.
- **A draw that hits its bound leaves the server drawing.** `StopImageDraw` interrupts, but a
  timeout only abandons the wait: KiChat hangs up and the server finishes a picture nobody
  will see. Sending the interrupt on timeout was considered and left out, because the interrupt
  is *global* to the server process — ten minutes after a request was abandoned it could kill a
  generation the user started in their own WebUI tab, which is the same hazard
  `Interrupt`'s doc refuses to risk by retrying.
- **`img2img`, inpainting, upscaling** — same server, another route.
- **A grid for several pictures.** The client already returns every image the server
  produced and files each one; nothing yet asks for more than one per call.
- **Progress.** A 30-step 1024×1024 generation takes tens of seconds with no feedback
  but a `tool` status pill, because `/sdapi/v1/txt2img` is synchronous and answers only
  when it is done. A real progress bar needs the WebUI's `/sdapi/v1/progress` poll or a
  queued route.
