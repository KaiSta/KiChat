# FLUX3Plan.md

What FLUX 3 is, what KiChat already does with it, and what it would take to support it
properly. A plan, not a subsystem document: once the phases below are measured and
built, the facts belong in `IMAGES.md` and this file shrinks to a pointer.

Research date: **2026-10-17**. *Verified* means the page was read here (BFL's release
post and repository README, both prompt files, the stable-diffusion.cpp guide);
*Reported* is a claim from a source that was not opened. Nothing in this file was
measured on this machine: there is no FLUX 3 checkpoint and no `sd-cli` from after
2026-10-16 in this checkout, so §8 is the list of things to measure before any number
here is quoted as a KiChat fact.

---

## 1. The model

**Verified** ([BFL release post](https://bfl.ai/blog/flux-3-image-klein) and the
[reference repository README](https://github.com/black-forest-labs/flux-3-image-klein)):

- FLUX 3 is a family — images, video, audio, action. `FLUX 3 Image` is the hosted model;
  `FLUX 3 Image Klein` is the **open-weights** image model, released 2026-10-15.
- Two checkpoints: **distilled** (16 steps, guidance none) and **Base** (50 steps,
  CFG 4; built for fine-tuning). Both do text-to-image, editing and multi-reference
  generation with **up to 10 reference images**, up to **4K**, and **bounding boxes**.
- 5.6 B parameters in the generator. Weights **11.2 GB BF16**, **5.6 GB FP8**; the INT8
  build is claimed at **8 GB VRAM** and the distilled model at ~4 s for 1 MP on an RTX
  5090 — but BFL's own memory table is **31 GB peak at 1024² in BF16 with no PUP**, 26 GB
  with FP8, on a B200. The 8 GB figure is the ComfyUI INT8 file, not this stack.
- Licence: weights under the **FLUX Kommunity License** (free commercial use under
  US$5 M annual revenue); the reference **code is Apache-2.0**; the **prompt upsampler
  is Apache-2.0**.
- **"Do not use the model without the prompt upsampler."** The model is trained on the
  captions that upsampler writes; a prompt sent as typed gives "noticeably weaker"
  results, and BFL turns it on by default in its CLI, demo and Python API. This single
  sentence reshapes this plan: the caption is not a quality knob, it is part of the
  pipeline.

**Verified** ([stable-diffusion.cpp `docs/flux3.md`](https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/flux3.md)):

```sh
sd-cli --diffusion-model flux-3-image-klein.safetensors \
       --llm   Qwen3-VL-4B-Instruct-Q4_K_M.gguf \
       --vae   flux2_ae.safetensors \
       -p "a lovely cat holding a sign says 'flux3.cpp'" \
       --cfg-scale 1.0 --steps 16 --sampling-method euler
```

- A pipeline is **three files**: a FLUX 3 diffusion transformer (`.safetensors`), the
  **Qwen3-VL 4B** encoder (`--llm`, GGUF or safetensors), and the **FLUX 2 VAE**
  (`ae.safetensors`, from FLUX.2-dev or shipped with FLUX 3).
- The guide uses **16 steps / CFG 1.0** for distilled and **20 steps / CFG 3.5** for
  Base — BFL's own README says 50 / CFG 4 for Base, so the two disagree (§8.6).
- Reference images are `-r <file>`, repeatable up to **10**. The scheduler's default
  time shift is **6.93**; `--flow-shift` overrides it.
- **There is no prompt upsampler and no flag for one**: `upsampl`, `pup` and `caption`
  appear nowhere in the project's README, `docs/flux3.md` or `docs/flux2.md`. Whatever
  reaches `-p` is what the model sees.

**Reported**: family announced 2026-07-23; `sd.cpp` support landed 2026-10-16; per
Diffusers the transformer is 5.5 B reading eight Qwen3-VL layers, the VAE packs 32 latent
channels into 128-channel tokens per 16×16 patch; **no official GGUF of the FLUX 3
diffusion weights** exists — the guide links safetensors only.

## 2. Where KiChat already fits

The pipeline shape is not new: FLUX 1 already split into a diffusion model, separate
encoders and a VAE, and `ModelFiles` carries the slots.

| FLUX 3 part | KiChat today | Flag |
| --- | --- | --- |
| Diffusion transformer (`.safetensors`) | `CLIDiffusionModel` | `--diffusion-model` |
| Qwen3-VL 4B encoder | `CLILLM` | `--llm` |
| FLUX 2 VAE | `CLIVAE` | `--vae` |
| Reference images | — | `-r` (**missing**) |
| Caption | the tool's `prompt` — the loaded model never asked for one | `-p` (**no caption contract**) |
| Steps / CFG | `Steps`, `CfgScale`; bounds 1–150 and 1.0–30 | `--steps`, `--cfg-scale` |
| Sampler | `Sampler` (`euler` is in the list) | `--sampling-method` |
| Time shift, preview, everything else | `CLIExtraArgs` | pass-through |

So the backend is done. `kind = "sdserver"` starts the same `ModelFiles` with a listener,
so Klein costs one model load instead of one per picture — the right lifetime for 5.6 B.

**It draws today, with no code change**: point Settings → Images at an `sd-cli` from
2026-10-16 or later, fill the three files, set 16 steps / CFG 1.0 / euler, and ask. That
is Phase 0's starting point, and it is the honest scope of "support": everything after it
makes that path *correct* rather than merely possible.

## 3. The gaps, and the failure each one produces

1. **No caption, so the model runs on raw requests.** `generate_image` asks for a `prompt`
   and says nothing about its shape, so a small chat model sends "a cat in a hat" — and the
   model that should be writing the caption, the one already loaded, is never asked. BFL
   calls that the weak path. §4 puts the loaded model in the PUP's chair.
2. **A negative prompt that does nothing.** Klein distilled is guidance-free; the hosted
   API has no `negative_prompt` field. KiChat sends `-n` whenever the field is filled and
   records it in the recipe — a setting that silently does nothing.
3. **No way to hand the model a picture.** Editing and multi-reference are the headline
   capabilities; `images.Request` has no input images (`IMAGES.md` §10 lists `img2img` as
   absent from every backend).
4. **Wrong numbers by default.** An empty steps field gets sd.cpp's default, not 16;
   nothing says an 11.2 GB BF16 transformer wants `--offload-to-cpu` on a 12 GB card.
5. **A second pass that may be another model's trick.** `hiresArgs` sends `--hires` for
   any request that asks; a 1.5× repaint at denoise 0.4 on a 16-step flow model is
   unmeasured, and only the size is verified afterwards (`verifyHiresOutput`).
6. **"Native 4K" is not reachable.** `MaxDimension` is 2048, `MaxOutputDimension` 4096 via
   the second pass — KiChat's 4K is an enlargement, not a native draw.
7. **No hosted API of any kind** (`IMAGES.md` §10), which is the only route to the full
   `FLUX 3 Image` model and to users without a fast GPU.

## 4. The caption, and who writes it

### 4.1 What the upsampler actually is

A **9 B vision-language model fine-tuned from Qwen3.5-9B** (Apache-2.0), whose only job is
to turn a short request — and, for editing, the reference images — into one detailed
caption. It is a text transformer, not an image model. BFL supports replacing it outright:
*"An external vision-language model can also write the caption in place of the upsampler:
give it the system prompts and pass its answer with `--no-pup`."*

The card puts it more directly still: **"Any other LLM can do the job with the system
prompts in `system_prompts.json`."** That is the whole opening for KiChat. The chat model
that already calls `generate_image` is an LLM with a system prompt, and it already holds
the conversation's images — so it is a drop-in for the PUP, and a better one than BFL's own
note describes, because it also knows what the user asked two turns ago.

The 9 B PUP is the wrong thing to ship here anyway: **18.8 GB bfloat16** (`model.safetensors`,
stated on the card) on top of the image model is the VRAM problem Klein was released to
avoid, and `sd-cli` cannot run it.

### 4.2 The prompts are published and Apache-2.0

The same four prompts ship in two places, both read here: as `system_prompts.json` beside
the PUP weights, and as `src/flux3/pup_prompts.json` in the reference code.

| File | Size | Contents |
| --- | --- | --- |
| `src/flux3/pup_prompts.json` (≈ `system_prompts.json`) | 12.6 KB | `t2i`, `i2i`, `t2i_bbox`, `i2i_bbox` — the compact prompts |
| `src/flux3/pup_prompts_external_vlm.json` | 103 KB | `t2i` and `i2i` plus **20 real training captions** and a much longer rule set (~26k tokens at four characters per token) |

The compact `t2i`/`i2i` pair is what the brief's strings are, verbatim — same wording, same
`TARGET CANVAS: W:H` convention, same "output only the caption" ending, same 100–350 /
50–150 word ranges — plus, in the brief, one added header line per prompt naming `sd-cli`
and `-p` as the consumer. That header is worth keeping: it is true, and it is the one
sentence that stops the model writing a "here is the prompt for the image tool" preamble.

The 103 KB file is stricter and shows the model what a caption looks like, but at roughly
26,000 tokens it does not fit a small local model's context alongside a conversation. **Ship
the compact pair by default**; offer the long one where the model has the window.

### 4.3 The loaded model is the captioner

BFL's escape hatch — "any other LLM can do the job with the system prompts" — is KiChat's
shape exactly: `generate_image` is already called by a model that is **loaded**, in a turn
that holds the request and, for editing, the picture. No second model is configured,
downloaded or kept resident; the model the user already chose writes the caption.

`ImageSettings.Captioning` decides where. The zero value is `""` (off), so nothing changes
for a pipeline that does not want captions; the Klein profile sets `auto`, because that is
what the checkpoint was trained on.

| Value | Who writes the caption | Cost |
| --- | --- | --- |
| `auto` — default for FLUX 3 | the tool, by one non-streaming completion with BFL's prompt, on the turn's own backend and model | one model round trip per draw |
| `model` | the chat model, as the `prompt` argument of its own tool call | none — the caption *is* the argument |
| `""` | nobody: the request goes to `-p` as typed | none, and BFL's weak path |

Why `auto` rather than `model` as the default: a 350-word tool argument is where a small
local model truncates, and it would sit in the transcript for every later turn, while a
completion the tool makes is invisible — the tool returns a picture, as it always did. It
is also the same code for local and cloud, because the turn already carries `Provider`,
`BackendID`, `ModelID` and `Model` (`generateInput`), and `chat.Complete` is the existing
non-streaming helper that compaction and titling use.

`model` stays for the case where the round trip itself is the problem — a rate-limited free
tier, or a model that writes good captions unprompted. The two are exclusive, never
chained: `model` after `auto` would expand an already expanded caption and drift from what
the user asked for.

The call, following the card's own usage rather than inventing one:

- system message = the published `t2i` or `i2i` text;
- user message = the request, preceded by `TARGET CANVAS: W:H` when the size is known; for
  `i2i` the references as `Message.Images` first (base64, in the same order as the `-r`
  flags), then the card's three-line scaffold — `Reference images follow in order (the first
  image, the second image, ...)`, `User request: …`, `Output the caption.`
- decoding = BFL's own recipe: **greedy** (`do_sample=False`, so temperature 0), **no
  thinking** (`enable_thinking=False`, which is `Think: false` here), and **up to 3,584 new
  tokens** — the value the card passes. A cap well below that truncates a complex caption,
  which is the one failure the fallback cannot tell from a short one.

Wiring: `tools` must not import `chat` (`chat` imports `tools`), so `tools.Service` gains one
callback, `Caption func(ctx, CaptionRequest) (string, error)`, shaped like `Progress` and
`Draws`. `app.go` supplies it and resolves the turn's backend through `Registry.Provider(id)`.
The backend and model id travel in the tool context the way `WithCallingMessage` already
carries the message id — two chats can run turns at once and the service is one value for
the whole app.

Why this is safe mid-turn: tool calls run **after** the provider's stream has been consumed
(`generate.go`: the `runTool` loop runs once `streamErr` is handled and `calls` is
non-empty), so the same backend is idle when the caption request arrives. A single-slot
llama-server answers it instead of deadlocking behind a stream that is waiting for the tool.
That is a property of this loop rather than of the providers, so it belongs in a comment at
the call.

The loaded model has to be able to do it: a model below ~7 B writes thin captions, and `i2i`
needs one that advertises `llm.CapVision` — without vision there is nothing to see and the
edit caption would be fiction. Both are refusals with a sentence, not silent downgrades.

### 4.4 The parts that make it work in this codebase

- **Output hygiene.** `CleanCaption` strips code fences, wrapping quotes, a leading "Here
  is the caption:" line and a JSON wrapper, collapses newlines to spaces (BFL's rule: one
  continuous block), trims, and fails on empty. `chat.Complete` returns only the text
  channel, which is what keeps a thinking model's analysis out of `-p` — and a reasoning
  model that spends its whole budget thinking returns empty text, which is its own error
  (`Summarize` already distinguishes this case; the captioner must too, rather than
  sending an empty `-p`).
- **Provenance.** `store.GenerationRecipe` gains `SourcePrompt` (what the user asked) and
  `CaptionModel` (which model wrote the caption, empty when none did). A redraw replays the
  caption — the picture it reproduces is the captioned one, at no extra model call; "redraw
  from the request" is a different action. The card can then say *what the model actually
  drew*, which is the cure for "why does this look nothing like my prompt?".
- **A failed caption does not fail the draw.** The short request is drawn instead, the tool
  result says so, and the recipe records `CaptionModel: ""`. Refusing to draw a picture the
  user asked for because the captioner stumbled is the worse answer.
- **No double expansion.** In `model` mode the instruction tells the model to write the
  caption directly and to pass an already-detailed prompt through unchanged; in `auto` mode
  a request over ~120 words is treated as a caption and skips the completion.
- **Bounding boxes stay out.** `t2i_bbox`/`i2i_bbox` are published, but the reference CLI
  takes boxes as a **separate JSON file** (`[y_min, x_min, y_max, x_max]`, 0–1, with ids),
  the hosted API is documented with a 0–1000 table inside the prompt (*reported*, not read
  here), and `sd.cpp` documents neither. Until a build is measured accepting one of those,
  a box prompt is a caption with coordinates in it — worse than no boxes. §6 keeps it out.

### 4.5 Optimising the draw path when FLUX 3 is the pipeline

Everything here is conditional on the active pipeline. An A1111 or SDXL draw sees **none** of
it — no extra call, no caption instructions, no new tool schema — which is what keeps the
feature from taxing the setups that do not need it.

1. **Gate on the pipeline, not on a global setting.** `Pipeline.NeedsCaption` is what makes
   `auto` possible; a pipeline that does not set it never pays. The hosted BFL kind never
   fires it either: that API runs its own upsampling server-side, so a caption from here
   would be a second expansion of the same prompt.
2. **Skip a request that is already a caption.** ≥ ~120 words, or an opening that already
   reads as a medium convention, passes through untouched. Captioning a caption drifts.
3. **Bound the call the way BFL does.** Greedy, `Think: false`, `MaxTokens` 3,584 (the
   card's number), the draw's own context, no tools, no streaming, and `CleanCaption` on the
   way out. 3,584 output tokens plus an ~850-token prompt still fits an 8 K-context model.
4. **Report the phase.** The caption costs seconds before the first sampling step, and a
   window that is silent until step 1 is indistinguishable from a hang. The draw's progress
   channel gains a `captioning` phase before `sampling`, so the existing step counter does
   not start on a number nobody is drawing.
5. **Let Stop stop it.** The completion inherits the draw's context: a Stop during captioning
   ends before any GPU time is spent, which is the cheapest possible cancel.
6. **Discard a partial caption.** A cancelled or truncated completion is not repaired into a
   caption; the short request is drawn instead, as in §4.4.
7. **Check vision before `i2i`.** `ModelInfo.Has(llm.CapVision)` on the turn's model and on
   the backend that would receive the images: a text-only model would caption the edit blind,
   which is worse than asking the user to describe it.
8. **Cap the reference payload.** The card says the PUP resizes to at most 1 MP for one
   reference and less for several, so small details can be described wrongly. KiChat should
   do the same downscale before sending: it keeps the caption request small, and it keeps the
   caption's claims inside what the image really shows.
9. **Keep the caption out of the transcript.** It is a `Request.Caption`, not a message; the
   model's own tool call stays short, and a 350-word caption is not replayed on later turns.
   `Request.EffectivePrompt()` returns the caption when there is one; the CLI kind appends it
   as `-p` and the WebUI/sd-server kinds put it in the body's `prompt` field, which is why
   the caption belongs on `Request` rather than at the flag site.
10. **Reuse on redraw.** A redraw replays the stored caption; no completion, no drift. Only a
    fresh request pays again.
11. **Say what it cost.** The tool result and the picture card name the caption model and the
    caption's word count, so a weak caption is visible as a cause rather than as "the model
    drew badly".

## 5. The plan

Ordered by "how much it fixes for how little it costs". Phase 0 is not optional: every
default later is a number it has to pin.

**Decisions this version fixes**, because each was argued twice and the argument should not be
reopened by accident:

1. **No "FLUX 3 support" switch.** A boolean can disagree with the weights: off, and the model
   draws on BFL's weak path; on, and an SDXL draw pays for a caption it never asked for. The
   two settings that cannot disagree with reality are the model file and a `Pipeline` value.
2. **The restriction lives in data, not in branches.** `NeedsCaption`, `DefaultSteps`,
   `DefaultCFG` and `SupportsNegative` are fields of a pipeline row, and a tool that asks them
   knows nothing about FLUX 3. The only code that names a pipeline is one description string.
3. **Phase 1 gates on `Captioning`, Phase 2 on the profile.** Phase 0's A/B has to caption a
   FLUX 3 draw before a profile exists, so the captioner must be reachable by setting alone;
   Phase 2 only adds the row that selects it. Gating Phase 1 on the pipeline would make the
   one measurement that justifies the feature impossible.
4. **The tool's arguments do not change.** `prompt`, `width`, `height`, `seed`, `steps`,
   `sampler`, `cfg_scale` and `negative_prompt` stay exactly as they are. A caption is
   written by the tool, or by the model into the existing `prompt` field — never into a new
   argument.

### Phase 0 — Measure, on a real build (½ day, no code)

1. Build `stable-diffusion.cpp` at a commit from 2026-10-16 or later; fetch the three
   files from §1.
2. Draw 1024×1024 at 16 steps / CFG 1.0 with the flat command. Record time to first step,
   s/step, peak VRAM (BF16 vs `--offload-to-cpu`), and the exact log lines `cliLog` parses
   (seed, step counter) — progress and seed were measured on Qwen-Image, not here.
3. **A/B the caption**, same seed, same size: the raw request versus BFL's `t2i` prompt
   applied by a local chat model. Without that comparison the caption feature is an
   argument, not a supported claim — and it is the single most useful number this phase
   produces.
4. Ask the three behavioural questions: does `-n` change anything on distilled; does
   `--hires` enlarge *and repaint* usefully or degenerate; does `-r` work through
   `sd-server`'s HTTP route or only through the CLI.
5. Record a 1 MP and a 2048×1152 draw, and one 4-image batch, so `MaxSteps`, `DrawTimeout`
   and the draw slot are argued from numbers.

Write the results into `IMAGES.md` as they are found; this file keeps only the plan.

**Partly measured, 2026-10-18**, on the machine that has the checkpoint — the numbers are in
`IMAGES.md` §9.5: a 512² at 16 steps / CFG 1.0 draws in 10.9 s through `images.CLI`, `-n` is
provably inert there (byte-identical pixels at CFG 1.0, different at CFG 7), a renamed copy of
the file still resolves from its tensors, and a 1.5× second pass works — which is why
`SupportsHires` turned true for the distilled profile and stays false for Base. **Still open**:
item 3, the caption A/B, which remains the only thing that would show the caption helps (no
chat backend was running); `-r` through `sd-server`; and item 5's larger sizes and batches.

### Phase 1 — The captioner (2–3 days, and it comes first)

**Implemented.** The prompt, `CleanCaption`, `Request.Caption`/`EffectivePrompt`, the
`Captioning` setting, `Service.Caption`, `chat.Caption`, `TemperatureSet`, the recipe fields
and the `captioning` phase are in the tree, with the tests listed at the end of this section;
`IMAGES.md` §9 is the description of what was built. What is *not* done is Phase 0: the A/B
that says whether it helps.

Without it, the rest of this plan supports a model on its weak path.

- `internal/images/caption.go`: BFL's `t2i` and `i2i` strings verbatim from
  `src/flux3/pup_prompts.json`, with a provenance comment naming the file and the Apache-2.0
  licence. Checked against that file rather than assumed (the sizes are in §7): neither
  prompt contains a backtick or a brace, so both are raw string literals, and `t2i` carries
  the `TARGET CANVAS` convention while `i2i` does not — the canvas line is a t2i fact and must
  not be appended to an edit. Also `CanvasLine(w, h)`, `CleanCaption`, and the length bounds (cap
  at 800 words, refuse empty). A test pins the structural invariants — opens with the
  upsampler sentence, ends with "Output only the caption." or "instruction." — rather than a
  hash, which any whitespace edit would break.
- `images.Request.Caption` beside `Prompt`, and `EffectivePrompt()` as the one accessor the
  three kinds share. `Normalised` is untouched: a caption is not a bound.
- `config.ImageSettings.Captioning string` (`""`/`auto`/`model`), a Settings select with its
  help text, and i18n in all three languages. Until Phase 2 exists there is no pipeline to key
  off, so `auto` is chosen by hand; `off` stays the default, so this version changes no
  existing draw.
- `tools.Service.Caption func(ctx, CaptionRequest) (string, error)` with `CaptionRequest`
  carrying the request, mode (`t2i`/`i2i`), size and reference bytes; `tools.WithTurnModel(ctx,
  backendID, modelID)` alongside `WithCallingMessage`. `app.go` wires the closure over
  `Registry.Provider(id)` and `chat.Complete`.
- `llm.ChatRequest` needs one addition for BFL's decoding recipe to be real:
  `TemperatureSet bool` (or an equivalent `Greedy` flag). `openai.go` always sends
  `temperature`, so `0` is greedy there — llama.cpp's documented sampler behaviour, not
  measured here — but `ollama.go` sends the key only when the value is `> 0`, so an Ollama
  caption runs at the modelfile's temperature. Without this, the Phase 0 A/B measures
  sampling noise and reports it as caption variance. The test reads the options map a
  provider sends, not the request it was given.
- The **only** tool-facing change: `Definitions()`'s `generate_image` description and
  `ImageProtocolHint` gain one sentence for a captioning pipeline — "write a brief request;
  the caption is added for you; steps and guidance are fixed". The argument list is
  untouched, and in `model` mode the sentence it adds — "a long, already detailed prompt is
  used as is" — is what stops a double expansion. `ImageUseHint` needs nothing: it is about
  *when* to draw, not how to phrase the prompt.
- A `captioning` phase on the draw's progress channel, ahead of the first sampling step.
  This is not a private addition: `drawProgressPayload` (`api.go`) builds the key set the
  window reads and `image_progress_test.go` pins it, so the payload, that test and the row
  rendering `pass`/`passes` change together. A renamed key is not a compile error on either
  side of the bridge — it is a progress row that silently stops appearing.
- Tests: `CleanCaption` against fenced, quoted, JSON-wrapped, prefixed, empty and
  newline-mangled input; `auto` sending the caption and never the short request; `model`
  leaving the request alone; `off` producing byte-identical arguments to today; a failed or
  empty caption falling back with `CaptionModel: ""`; a vision-less model refusing `i2i`;
  a redraw issuing zero caption calls.

### Phase 2 — Pipeline profiles (1–2 days)

**Implemented.** The table, the inference from the checkpoint's own tensors (with its file
name as the fallback), the zero-filling, the
dropped negative prompt and second pass, the profile-aware caption default, `Health`'s
named part and the panel's resolved line are in the tree; `IMAGES.md` §9.5 describes them.
Two choices differ from what this section first said: a pass the profile cannot use is
**dropped with a note** rather than refused (the switch is global, so refusing would block
every draw until the user turned it off), and an explicit `off` was added to `Captioning` —
the empty value now means "the profile decides", which is what lets the profile turn the
caption on at all. The second pass is then no longer dropped for the distilled checkpoint at
all: it was measured on a real build (item 0.4 below, `IMAGES.md` §9.5) and now goes through,
while Base keeps the drop because no Base checkpoint was measured.
A third change is in the tree: the profile is read from the **checkpoint's own tensor table**,
not from its file name, with the name kept only for a file whose header cannot be read. The
measurements that made that possible — no metadata value names the model, but the FLUX 3
tensors and the distilled checkpoint's extra `ref_time_in.*` pair both do — are `IMAGES.md`
§9.5.1.

- `internal/images/pipelines.go`: a `Pipeline` value with `Name`, `Encoder`
  (`llm`/`clip_l+t5xxl`/none), `RequiresVAE`, `DefaultSteps`, `DefaultCFG`,
  `SupportsNegative`, `SupportsHires`, `NativePixels`, `MaxSide`, and `DefaultCaptioning`.
  Entries for `flux3-klein`, `flux3-klein-base`, plus the existing Qwen-Image/FLUX 1
  shapes as the empty value.
- `ImageSettings.Pipeline` (default `""` = today's behaviour; an absent field keeps its
  default, so no migration). `ImageProfile` already wraps `ImageSettings`, so saved
  profiles carry it.
- Inference, and its failure mode. `auto` opens `CLIDiffusionModel` and reads its header —
  `internal/images/modelmeta.go`, bounded at 256 KiB and cached by path, size and mtime — so
  a renamed or re-quantised file is recognised by its tensors. The file name is the fallback
  for a header that cannot be read, and Settings prints which of the two answered, because a
  miss still captions nothing and looks exactly like a broken feature. Picking a profile by
  hand overrides both without editing anything.
- The profile fills a request's **zeros** in `internal/tools/image.go` before
  `Normalised()`; bounds stay in `limits.go`. `SupportsNegative: false` drops the negative at
  both places it would otherwise surface — `drawArgs`'s `-n` (`cli.go`, emitted whenever the
  field is non-empty) and the WebUI/sd-server body's `negative_prompt` — and says so in the
  tool result instead of recording a no-op.
- `Health` names the missing part by role: "this pipeline needs a text encoder (`--llm`)",
  and `pipeline: generic — no caption` when inference found nothing.
- `ServerKey` changes when the pipeline changes — a restart, reported as such.
- Tests: only unset values are filled; a value fixed in Settings still wins (`pinInt`'s
  rule); the dropped negative prompt is visible; `Health` names the role.

### Phase 3 — Reference images (3–5 days)

- `images.Request.References []Reference` (bytes, MIME, label), bounded to **10**, each
  downscaled above **16 MP**.
- CLI: write each into the draw's temp directory, append `-r <path>` per reference, stable
  order asserted. New sentinel `ErrReferencesUnsupported` for the `server` kind, used the
  way `ErrHiresUnsupported` is: refuse before spending a draw, name the kind.
- Entry point: the picture card gains "Edit this…"/"Use as reference", and the tool gains
  `reference_attachment_ids` — `tools.Service` already holds `Store` and
  `ListAttachments(chatID)` exists, so bytes come from the attachment's document.
- `GenerationRecipe` gains `References []string`; a redraw re-reads them and refuses by
  name when one was deleted.
- With captioning on, the `i2i` prompt is used and the references are attached to the
  caption request in the same order as `-r`.

### Phase 4 — Aspect ratios and the 4K story (1 day, after Phase 0 settles 4K)

- A preset row (BFL's 15 ratios) computing aligned width/height at ~1 MP
  (`21:9 → 1536×640`, `1:1 → 1024×1024`), aligned to `DimensionStep`. `Request` is
  untouched; the presets are `Width`/`Height` with better labels, in Settings, the tool
  schema (`enum`) and the message controls.
- **Do not raise `MaxDimension` in this phase.** A single-pass 4096 latent is a different
  memory argument from two 2048 passes, which is the distinction `MaxOutputDimension`
  exists to keep. Label the second-pass button honestly and record in `IMAGES.md` that
  native 4K is reachable only through `CLIExtraArgs` for now.

### Phase 5 — Hosted BFL provider (2–3 days, independent of 1–4)

- `internal/images/bfl.go`: `Provider` over `POST /v1/flux-3-image[-klein]` with the key in
  `x-key`, then poll `polling_url`, then fetch the result URL. Async is the shape, so
  `Generate` is submit+poll bounded by the draw's context.
- `ImageKindBFL`. Key under its own secret name (`images-bfl`) — the existing `images` key
  is a `user:pass` pair for basic auth.
- Visible limits: no seed in the documented schema, so `Result.Seed` is 0 and a redraw is a
  variation; `CfgScale`, `Steps` and `NegativePrompt` are not request fields, so a non-zero
  one is a refusal with a reason, not a silently ignored field.
- `safety_tolerance`, `grounding`, `resolution` and `aspect_ratio` become this kind's
  settings; `models`/`samplers` return no listing and the panel offers a fixed list.
- The response's `cost` goes into the result's `Info`, which is the one thing a local draw
  never has to report.
- Tests against `httptest`: submit, poll-with-backoff, 429 retryable, failed job, result URL
  404, and a key redacted in every error.

### Phase 6 — Docs and licence

- `IMAGES.md`: a FLUX 3 section with the Phase 0 numbers, the three-file pipeline, the
  caption contract and both prompt files, the reference limit, the ratio list.
  `IMAGES.md` §10 loses "img2img" for the CLI kind and gains "bounding boxes, LoRA".
- `THIRD-PARTY-NOTICES.md`: **unchanged** while KiChat ships no weights. The two prompt
  files are Apache-2.0 and must be attributed in the source comment and in the notices if
  their text is embedded — that is a change to make when the file is landed, not a claim
  to leave to memory. A "download Klein" button would be redistribution under the FLUX
  Kommunity License and needs its own look.
- `.claude/skills/live-tests`: a gated Klein case pinning 16 steps, the caption A/B and the
  reference count.

## 6. Not in this plan

- **FLUX 3 Image** (the hosted full model) as open weights — none are public.
- **Video and audio** from the family, and **FLUX 3 Action** — different sub-models.
- **Bounding boxes** — see §4.4: the three candidate formats (separate 0–1 JSON for the
  reference CLI, a 0–1000 table in the prompt for the hosted API, and whatever `sd.cpp`
  might accept) do not agree, and nothing documents that `sd-cli` consumes any of them.
- **The 9 B PUP itself** — Apache-2.0, but 18.8 GB BF16 and not runnable by `sd-cli`. The
  already-loaded chat model replaces it, which is exactly what §4.3 builds.
- **LoRA training** on Klein Base — out of scope for this app.

## 7. Risks

- **The caption is now a second model in the loop** — the same weights, a second job. It
  can invent content, and in `auto` mode it costs a round trip per draw before any GPU work
  starts. Both are bounded by "keep every explicit fact" in the prompt and by the fallback,
  but a small chat model will still write thinner captions than a 9 B fine-tune.
- **Prompt length.** The compact `t2i` prompt is **506 words / 3,404 characters** (~850
  tokens) on top of the system prompt and history; the 103 KB variant is ~26,000 tokens at
  four characters per token. On an 8 K-context local model the long file is unusable, which
  is why it is not the default.
- **Provenance of the prompts.** They are BFL's, Apache-2.0, and must be copied verbatim
  with a source pointer — a paraphrase would be a different prompt with different
  behaviour, and calling it BFL's would be a lie. The card's own limitations travel with
  them: output is English whatever the request's language, the prompts were written for
  FLUX 3 Image Klein and no other image model was in training, and the model does not
  refuse anything (safety is a wrapper in BFL's code that KiChat does not have).
- **No official GGUF of the diffusion weights.** Klein is 11.2 GB BF16 / 5.6 GB FP8, so a
  small card depends on `--offload-to-cpu` and pays for it in time. The INT8/FP8/NVFP4
  packs are ComfyUI-shaped; whether `sd.cpp` loads them is unverified.
- **One pipeline per process** — switching models is a restart, which `ServerKey` reports.
- **Licence drift** between Klein and the hosted model's terms: link, never paraphrase.

## 8. Open questions

1. ~~Does `sd-cli`'s log for this pipeline still print the seed and the step counter that
   progress and seed parsing depend on?~~ **Answered 2026-10-18: yes.** A draw through
   `images.CLI` returned the seed the binary drew with and 12 step reports. It never reports
   the *final* step (the last was 14 of 16), so a row can look one step short of done.
2. ~~Does `-n` do anything on the distilled checkpoint?~~ **Answered 2026-10-18: no.** At
   CFG 1.0 the same seed draws byte-identical pixels with and without `-n`, while the same
   pair at CFG 7 differs — so the flag is honoured by the binary and inert at 1.0.
3. Does `--hires` produce an enlargement worth keeping, or should the profile set
   `SupportsHires: false` and refuse it before the draw (`ErrHiresUnsupported` already
   exists and is tested)? **Half answered:** the pass works and is clean (512² → 768² in
   18.4 s against 7.3 s for the base draw, and 13.3 s for a direct 768²), so the distilled
   profile now allows it. Whether the *picture* is better than a direct larger draw is still
   open, and it costs 5 s more here.
4. Peak VRAM at 1 MP and at 2048 wide: BF16, FP8 if loadable, with and without
   `--offload-to-cpu`, with and without `--vae-tiling`. **Only 512² is measured** (6.3 GB in
   VRAM for the Q4 checkpoint plus the Qwen3VL encoder and the VAE); the larger sizes are
   still open.
5. Does `sd-server`'s `/sdapi/v1/txt2img` take references through any key, or is editing
   CLI-only?
6. Base: the sd.cpp guide says 20 steps / CFG 3.5, BFL says 50 / CFG 4. Which does this
   build draw well with, and what does the profile default to?
7. The caption A/B: how much does the caption actually buy on *this* build, at 16 steps,
   measured as a preference between two pictures of one seed?
8. Does the loaded model follow BFL's `t2i` invariants well enough that `model` mode is the
   better default for it? (Then the extra completion never runs, and the caption arrives as
   a tool argument.)
9. How much latency does `auto` add on the smallest supported backend — a 3–4 B model on a
   CPU — and does the `captioning` phase make that wait legible rather than alarming?

## 9. Sources

Read here:

- [FLUX 3 Image Klein release, 2026-10-15](https://bfl.ai/blog/flux-3-image-klein)
- [Reference repository README](https://github.com/black-forest-labs/flux-3-image-klein) — model table, PUP requirement, memory, licence, box format
- [`src/flux3/pup_prompts.json`](https://github.com/black-forest-labs/flux-3-image-klein/blob/main/src/flux3/pup_prompts.json) — the four compact prompts
- [`src/flux3/pup_prompts_external_vlm.json`](https://github.com/black-forest-labs/flux-3-image-klein/blob/main/src/flux3/pup_prompts_external_vlm.json) — the long variant with 20 reference captions
- [stable-diffusion.cpp `docs/flux3.md`](https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/flux3.md)
- [Prompt upsampler model card](https://huggingface.co/black-forest-labs/flux-3-image-klein-pup) — the "any other LLM" sentence, 18.8 GB BF16, the decoding recipe, the `i2i` scaffold, the reference downscale and the limitations

Cited, not read here:

- [FLUX 3 announcement](https://bfl.ai/blog/flux-3)
- [BFL API reference, FLUX 3 Image](https://docs.bfl.ai/api-reference/utility/generate-an-image-with-flux-3)
- [Diffusers FLUX 3 pipeline](https://huggingface.co/docs/diffusers/main/en/api/pipelines/flux3)
- [ComfyUI FLUX 3 Klein guide](https://docs.comfy.org/tutorials/flux/flux-3-klein)
- [Klein weights](https://huggingface.co/black-forest-labs/flux-3-image-klein), [Base](https://huggingface.co/black-forest-labs/flux-3-image-klein-base), [ComfyUI repack](https://huggingface.co/black-forest-labs/flux-3-image-klein-comfy)

KiChat ground truth for the integration points: `internal/images/cli.go` (`ModelFiles`,
`Options`, `modelArgs`), `internal/images/sdserver.go` (`ServerSpec`),
`internal/images/limits.go`, `internal/tools/image.go`, `internal/tools/tools.go`
(`ImageUseHint`, `ImageProtocolHint`), `internal/chat/compact.go` (`Complete`),
`internal/chat/generate.go` (the tool loop runs after the stream), `internal/llm/types.go`
(`Message.Images`, `CapVision`), `internal/config/config.go` (`ImageSettings`),
`IMAGES.md` §2 and §10.
