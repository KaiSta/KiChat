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

## 2. One client, three servers

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

`config.NormaliseImageClamps` bounds the numbers a stored blob may carry — 64–2048
pixels, 1–150 steps — and 0 stays 0, because 0 is the spelling of "the server's own".
A 20000-pixel request is not refused by a WebUI, it is attempted: minutes of work and
tens of gigabytes. `internal/images.normaliseDimension` rounds to the multiple of 8
every diffusion model needs, as a second line of defence for a number a *model*
invented rather than one a user typed.

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

A third way exists that needs no model at all: the **redraw controls** under a picture
(`MessageParts.tsx` → `api.go:RegenerateImage` → `tools.Service.Regenerate`). They read the
recipe off the attachment (§4) and call the provider directly, so a repeat or a variation
costs GPU seconds and no tokens. The result is filed onto the **message the original is
on**, which is what puts it beside its source — an attachment on no message renders nowhere
— and the backend emits `chat:attachments` afterwards, the same event the tool path uses,
which is what makes the new picture appear without a reload.

`internal/chat/image_tool_test.go` pins all three, on both the native and the
text-protocol path, and from **both** sides: "absent when off" would also pass if the
tool were never advertised at all, and that is the regression the test exists to catch.

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
internal/images/        types + the /sdapi/v1 client (sdwebui.go, sdwebui_test.go)
internal/tools/image.go the generate_image tool: arguments, storage, the model's reply,
                        the recipe, and Regenerate (repeat / variation)
internal/config/        config.ImageSettings, DefaultImageBaseURL, NormaliseImageClamps
internal/store/         GenerationRecipe on the attachment, migration 4, the queries
app.go                  rebuildServices builds the provider, or nil when off
api.go                  GetImageStatus, TestImageBackend, GetAttachmentImage,
                        SaveAttachmentAs, PickSaveFolder, RegenerateImage
imgs  (frontend)        MessageParts.MessageImages / MessageImage, ChatView, Settings
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
  not collide; and a picture with no recipe is refused before the provider is called.
- `internal/store/store_test.go` — the recipe round-trips, a corrupt one fails the read
  rather than degrading to "no recipe", and an **older database gains the column**: the
  upgrade path is built by dropping the column and rolling `user_version` back, because a
  migration placed at the wrong index is skipped silently and breaks every installation.
- `internal/config/config_test.go` — the feature ships off, the fields stay empty, and
  the clamps hold.
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
  populated checkpoint and sampler pickers, the test-connection flow, and both redraw
  controls — including which of the two passes `freshSeed: true`, which is the one thing
  a swap would hide. Its reply carries a **second, uploaded** picture beside the generated
  one, because a fixture whose only picture has a recipe never renders the state where the
  redraw controls are absent. It proves no backend behaviour, which lives in the Go tests.
  Its fixture picture is an SVG data URL rather than a PNG because a fixture only has to be
  visible at size; the bytes the real binding returns are asserted in Go.

## 9. What it does not do yet

- **ComfyUI** (§2), and any hosted image API (OpenAI's `/v1/images/generations` with
  `b64_image`, Stability, fal, Replicate) behind the same `images.Provider` interface.
  The interface was written for that: `Provider` has no HTTP in it.
- **`img2img`, inpainting, upscaling** — same server, another route.
- **A grid for several pictures.** The client already returns every image the server
  produced and files each one; nothing yet asks for more than one per call.
- **Progress.** A 30-step 1024×1024 generation takes tens of seconds with no feedback
  but a `tool` status pill, because `/sdapi/v1/txt2img` is synchronous and answers only
  when it is done. A real progress bar needs the WebUI's `/sdapi/v1/progress` poll or a
  queued route.
