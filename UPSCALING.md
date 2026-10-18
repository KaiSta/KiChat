# Fast upscaling

The second way to make a picture larger, beside the generative one: a classical resampler
that always works, an optional Real-ESRGAN pass on the GPU, and the panel that chooses
between them and a hires redraw. `IMAGES.md` covers drawing; this file covers enlarging,
and `AGENTS.md` is the index.

## 0. What is implemented, and what is not

Implemented: `Fit`/`TargetFor`, the Lanczos3 resampler with premultiplied alpha and an
unsharp pass, EXIF orientation, PNG/JPEG/GIF/WebP/BMP decode, source-container encode,
`images.Upscale`, `tools.Service.Upscale`, the derived attachment and migration 6, the
`UpscaleAttachment`/`GetUpscalerStatus`/`TestUpscaler`/`PickUpscalerBinary` bindings, the
Enlarge panel, the Settings group, the harness fixtures — and, for a neural run that is
still going, one phase report with a Stop that kills the program instead of waiting out its
bound.

Implemented as well: **the download.** `InstallUpscaler` fetches the pinned archive for
this machine, compares it to the pinned size and SHA-256, unpacks it, makes the program
executable, checks that the weights really are there, and writes the path and the switch
into the settings; `GetUpscalerInstall` is the progress the panel reads while it runs. The
pinned table is `internal/images/upscaler_install.go` (Linux, Windows, and one universal
macOS archive for both architectures), and the four digests in it were computed here
because upstream publishes none. §6 has the button, what it writes, and the by-hand
procedure that remains for a platform with no published archive or a program installed
somewhere else.

Not implemented, deliberately or not yet:
- **No progress finer than a phase, and no Stop for the fast method.** The program's own
  output is a bar per tile and nothing a client may parse, so a step counter would be
  invented. The classical pass is offered no Stop on purpose: it is local arithmetic with
  no cancellation point inside a pass, and a button that does nothing for a second is
  worse than no button.
- **No model of whose Vulkan card it is.** The probe reports the program's own error, not
  a GPU name.

---

## 1. What was asked for

> a more classical upscaler that is fast for images, like Real-ESRGAN 4x+ as an ONNX
> model or something — beside AI upscaling, an option of classical upscaling for FHD
> and 4K or custom.

The useful reading is a second axis on the picture card:

| | redraw (what existed) | enlarging (this feature) |
| --- | --- | --- |
| **FHD**, **4K**, custom size | one hires **redraw**: the model samples a second pass | the same targets, with a **method** choice |
| cost | minutes, GPU, needs the image server | *resample*: a second or two, no server; *Real-ESRGAN*: seconds, no server |
| works on | only pictures KiChat drew (a recipe is required) | **every** image attachment, including uploaded photos |
| what it adds | new detail, a new interpretation | pixels only; the picture stays the picture |

The last two rows are the reason this is worth doing at all. The existing FHD/4K
buttons `enlargedRecipe` in `frontend/src/components/MessageParts.tsx` are a recipe
edit with one click: they raise the base size until a ≤2× hires pass lands on 1920 or
3840, then send a whole new draw. They are excellent at what they do and completely
unusable for a photograph the user attached, or for a machine with no image server
running.

## 2. What it builds on, and what it must not reuse

- `internal/images/upscale.go`, `resample.go`, `exif.go` and `esrgan.go` are this feature.
  `Size` still only calls `image.DecodeConfig`, and stays the cheap "how big is it" path.
- `internal/images/limits.go` bounds a *draw*: `MaxDimension` 2048 per pass,
  `MaxOutputDimension` 4096, `Hires` 1.25–2×. Those numbers are about diffusion
  sampling and are not the right bounds for a resample.
- A picture is stored as bytes in `documents.text` (base64) and served whole to the
  webview by `GetAttachmentImage` as a `data:` URL. Size is therefore not free: a 4K
  PNG is several megabytes of base64 in SQLite and in one bridge call.
- `store.GenerationRecipe` records *how a picture was drawn*. It is deliberately nil
  for an uploaded file, which is what hides Redraw/Variation today.
- `images.SniffImage` already recognises PNG, JPEG, WebP and GIF from the bytes.
- EXIF handling exists only in `exif.go`, and only for the orientation tag: nothing
  else in the tree reads image metadata, so a new metadata need is new code.
- `internal/runtime/install.go` already downloads, verifies and extracts a managed
  binary (llama.cpp), which is the pattern a downloadable upscaler would copy.

## 3. Three tiers, one control

| tier | method | time | detail | install | server |
| --- | --- | --- | --- | --- | --- |
| 1 | **Resample (Lanczos)** | **measured: 0.13 s** for 512²→1920², **0.47 s** for 512²→3840² | none invented; a clean, slightly soft enlargement | none, pure Go | none |
| 2 | **Neural upscale (Real-ESRGAN 4×)** | **measured: 1.4–2.3 s** for a small picture on a Vulkan GPU, dominated by loading 33 MB of weights; **22.6 s** for the same picture on llvmpipe | plausible texture, can hallucinate | a portable binary plus weights (x4plus weights alone 33.4 MB) | none |
| 3 | **AI redraw (hires)** — exists | minutes | a new interpretation | the current image backend | required |

Tier 1 is the answer to "classical and fast": it is the only tier with no install, no
GPU and no server, and it covers the ordinary case (get this 512² picture to 1920 or
3840 without waiting). Tier 2 is the answer to "Real-ESRGAN"; it ships in the same
change but behind its own switch, off by default (§13), so a machine that never enables
it never downloads a binary. Tier 3 is unchanged and is relabelled so the three are not
confused.

## 4. Why not ONNX in-process

The suggestion was an ONNX model. It is the right model and the wrong container for
this build:

- `yalue/onnxruntime_go` requires CGO and a native ONNX Runtime library per OS.
  KiChat cross-builds Windows from Linux with `GOOS=windows go vet ./...` and
  `wails build` (see `AGENTS.md`), so a cgo dependency breaks the check matrix before
  it ships — and a Windows `.exe` would then need `onnxruntime.dll` plus an execution
  provider (`DirectML.dll`, CUDA, CoreML) next to it, unsigned.
- A pure-Go ONNX runtime (`onnx-go`, `GONNX`) does not credibly run an RRDB network:
  operator coverage is partial, and no benchmark exists for this workload. An
  RRDB x4plus is 23 blocks of convolution at 64 channels — the opposite of a toy graph.
- A purego binding to ONNX Runtime avoids CGO, but it is a young, explicitly unstable
  binding and it still ships the same native runtime and provider DLLs, for no speed
  advantage over ncnn on the GPU.

Real-ESRGAN is published as a portable **ncnn-Vulkan** binary, which is the same
network compiled for the GPU with no CGO in our process. That is the tier 2 route.
ONNX stays a fallback for the day ncnn's 2022 binary stops working on a target
platform — not the first implementation.

## 5. Tier 1 — the classical resampler

New file `internal/images/resample.go`, no provider interface, no settings backend:

```go
// Target is where a picture should end up, in pixels.
type Target struct{ Width, Height int }

// Fit is the arithmetic, kept separate from the pixels so it can be tested.
func Fit(srcW, srcH, longSide int) Target
```

- **Kernel:** separable Lanczos3 (`sinc(x)·sinc(x/3)`, support 3, weights normalised
  per output pixel and precomputed). This is the classical expectation and is visibly
  better than bilinear on text and edges. `golang.org/x/image/draw` has
  `CatmullRom` but no Lanczos, so either implement it here (~120 lines plus tests) or
  name the control after the kernel actually used. Do not label Catmull-Rom "Lanczos".
- **Alpha:** resample premultiplied (or premultiply around the kernel), or transparent
  PNG edges get dark fringes. Go decodes 16-bit PNG to `NRGBA64`; downconvert to 8-bit
  first, which is also what the encoder writes.
- **Decode:** PNG/JPEG/GIF from the standard library; add `golang.org/x/image/webp`
  and `.../bmp` with blank imports, because a WebUI configured for WebP produces
  exactly the files this must accept. GIF: first frame only.
- **Encode:** keep the source container — PNG→PNG, JPEG→JPEG at quality 92, and
  WebP/GIF/BMP→PNG (there is no pure-Go WebP encoder). Report the real format through
  `SniffImage`; a JPEG named `.png` is the bug `Image.Ext` exists to prevent.
- **EXIF orientation:** a phone JPEG carries its rotation in APP1, and the browser
  honours it while `image/jpeg` does not. Without normalising the eight orientation
  values, an uploaded portrait is upscaled sideways. Parse the tag (a small reader, or
  a pinned dependency) and apply the transform before resampling. This is a v1
  requirement, not a polish item.
- **Sharpening (optional):** a small unsharp mask after the resize — a 4-neighbour
  cross blur, radius about one pixel, amount 0–1 defaulting to 0.25 — recovers the
  perceived crispness Lanczos gives up. The amount is a setting and a slider, and 0
  turns it off. It is deliberately not a kernel choice: a wider radius is a different
  effect with visible halos.
- **Size cap:** `MaxUpscaleDimension = 8192` on the long side and
  `MaxUpscalePixels = 64 MP`, both well above 4K (8.3 MP) and 4K square (14.7 MP).
  Memory is the reason for a cap, not quality: an 8K NRGBA is 132 MB plus one
  intermediate pass.
- **Single-threaded, and now measured rather than assumed.** On this machine (Ryzen 7
  5800X3D) a 512²→1920² pass costs 132 ms and a 512²→3840² pass 469 ms, encoding
  included, allocating 21 MB and 70 MB. Splitting destination rows across
  `runtime.NumCPU()` would shorten the 4K case and is deliberately not done: half a second
  does not buy a second set of goroutines that the caller cannot stop. The two benchmarks
  below are the measurement, so the claim can be re-taken on any machine.
- **Cost:** measured, in the benchmarks above: 132 ms and 469 ms for the two presets, so
  the honest sentence for the panel is "under a second", not "instant". The arithmetic
  behind it is ~44 M multiply-adds for the FHD case and about 4× that for 4K.

Not in v1: ICC/wide-gamut conversion, gamma-aware resampling, animation (GIF/APNG
frames), and a choice of kernels. Each is a real limitation and belongs in the doc.

## 6. Tier 2 — Real-ESRGAN, as an optional portable binary

- **What to run:** `realesrgan-ncnn-vulkan` (MIT code; Real-ESRGAN weights
  BSD-3-Clause). Latest release is still **v0.2.0 (April 2022)** for the ncnn-vulkan
  repository; the parent repo also publishes ncnn binaries at v0.2.5.0. Windows,
  Linux and macOS are published; ARM64 coverage is not promised by the release names
  and must be checked before offering it on Apple silicon.
- **Models:** `realesrgan-x4plus` for photographs (≈33 MB of weights) and
  `realesrgan-x4plus-anime` for flat art; `realesr-animevideov3` is a smaller,
  faster option. Default: x4plus, with a per-picture override.
- **Native scale only:** run the model at its own 4× (`-s 4`), then let the Go
  resampler land on the exact target (1920/3840 long side, or custom). Do not ask the
  binary for an arbitrary factor: the weights are trained for 4×, and every non-4×
  path ends in a resample anyway. This also makes the output size predictable and
  testable.
- **Tiling:** `-t` (0 = auto) bounds VRAM; expose "Auto / 128 / 256 / 512" only if a
  real machine needs it. `-f png` for a lossless hand-off; `-g` stays auto.
- **Install:** "point at an existing executable", or find it on `PATH`, is what exists
  today; **the download itself is not implemented**. What it has to fetch was measured
  rather than assumed, because the obvious archive is the wrong one (both zips were
  downloaded and unpacked on this machine, and the sizes below are from the files, not
  from a release page):

  | archive | size | contains |
  | --- | --- | --- |
  | sibling repo `Real-ESRGAN-ncnn-vulkan` v0.2.0, ubuntu | 3,654,741 B | the 11.4 MB executable, LICENSE, README — **no `models/`** |
  | sibling repo `Real-ESRGAN-ncnn-vulkan` v0.2.0, windows | 2,159,606 B | the `.exe`, `vcomp140.dll`, LICENSE, README — no `models/` |
  | `Real-ESRGAN` v0.2.5.0 `realesrgan-ncnn-vulkan-20220424-ubuntu.zip` | 46,931,474 B | binary + `models/realesrgan-x4plus.{bin,param}` (33.4 MB of weights) + `-anime` + animevideov3 x2/x3/x4 + demo files |

  The small archive is the one a reader of the repository name finds first, and it is
  also the trap: the program runs, reports `fopen …/models/realesrgan-x4plus.param
  failed`, writes an all-black picture and **exits 0**. The parent repository's 20220424
  archive is the one that makes the feature work in one download. Two more measured
  details an installer has to handle: **the zip carries no exec bit** (entries are
  `-rw-a--`, so the unpack has to `chmod 0755` the executable), and the models folder is
  found beside the **executable** rather than beside the working directory, so the whole
  archive has to land in one directory.

  Upstream publishes **no checksums** — the release API reports `digest: null` for every
  asset and neither release body lists any — so a pinned hash is one we compute ourselves.
  That is worth stating plainly: it protects against the asset changing under a fixed
  version, not against the asset being wrong on the day we pinned it. Choosing that
  digest means downloading 46.9 MB during development, which is a one-time cost.

  **That download is what `InstallUpscaler` does**, and it is the procedure a user has
  today; the manual at the end of this section is for a platform with no published archive
  and for a program installed somewhere else. It copies `internal/runtime/install.go`'s
  shape — pinned version, pinned SHA-256, verified before unpacking, unpacked into
  `<app data dir>/upscalers/realesrgan-ncnn-vulkan/<version>/` (which is
  `~/.config/kichat/upscalers/…` on Linux; the app's data directory already ends in
  `kichat`) — and it keeps the archive as published, demo `.jpg`/`.mp4` files and all, so
  that what is on disk is what the digest describes. It is not a redesign of anything
  above: the switch, the status and the probe are already the surface a downloaded binary
  plugs into. Never bundle it in the installer: it is 47 MB plus the weights for a feature
  many users will not touch, and a release asset that changes outside our versioning is
  worse than a download the user asks for.
- **Availability probe:** a Settings "Test" button runs a 16×16 upscale and reports
  either the GPU it used or the binary's own error. The released build is Vulkan-only
  and has no CPU fallback switch; when Vulkan is missing the honest answer is
  "Real-ESRGAN needs a Vulkan GPU; Lanczos is available", not a silent downgrade.
- **Run it:** temp directory, `exec.CommandContext`, existing `proc_*` helpers to kill
  the whole process group on Stop, output read back and re-encoded by the same code
  path tier 1 uses. Timeout scales with output pixels, like `MaxDrawTimeout`.
- **A run that loaded no weights is not a failed run.** Measured: without `models/`, the
  program exits **0**, writes a PNG of exactly the right dimensions, and every sampled
  pixel of it is black. Exit code, file size and dimensions all look correct, so the only
  honest signal is the program's own output — which is why `ESRGAN.Enlarge` checks the
  log for `find_blob_index_by_name`/`fopen … failed` and refuses the result as
  `ErrUpscalerNoModels`, and why it also refuses *before* starting when a sibling
  `models/` folder exists without the weights of the model the panel is set to. Before
  that check the app filed the black picture as the enlargement.
- **Progress:** the binary's own progress output is not a stable interface. Emit an
  indeterminate "upscaling" phase through `events.ImageProgress` (pass 1 of 1) and
  leave the step counter at zero; do not parse a progress bar.
- **Licences:** add both to `THIRD-PARTY-NOTICES.md` where the managed llama.cpp
  builds are described. The notices generator only walks the Go module graph, so this
  entry is written by hand and says why.
- **Risks:** an unsigned downloaded `.exe` and an unsigned macOS binary (Gatekeeper
  quarantine), a Vulkan driver that works in one game and not in ncnn, and the fact
  that the upstream binary is four years old. Each is a reason to keep tier 2 optional
  and tier 1 always available.

### The download

Settings → Image generation → **Enlarging** has one button, **Download Real-ESRGAN**, in
the same group as the switch and deliberately outside the block the switch gates: a user
who has not switched the neural method on yet is exactly the user who needs the program.
The row names the cost before it is paid (`47 MB from GitHub — checksum and model weights
included`, the size read from the pinned table rather than written into the text). While it
runs it shows the phase and, for the download, the percentage; then the switch is on, the
Program field holds the installed path, and the status under **Test** reads *Ready*.

What it does, in order — any failure stops there and leaves the settings untouched:

| step | what it answers |
| --- | --- |
| fetch the pinned archive for this platform | 45,474,481 B on Windows, 46,931,474 B on Linux, 51,817,124 B on macOS |
| compare SHA-256 and byte count to the table | the archive is the pinned one, not a re-upload, a truncation, or a captive-portal page |
| unpack into `<version>.installing`, then rename | an interrupt leaves no half-install, and the previous one is replaced only when the new one is complete |
| `chmod 0755` the launcher | the zips carry mode 0 on every entry, having been written by Windows |
| check the executable and `models/realesrgan-x4plus.{bin,param}` | the download is a usable install, not a program that writes a black picture and exits 0 |
| write `esrganBinary` and `upscalerEnabled`, rebuild the tool service | the neural method works without a restart, and the next start reads the same settings |

Where the pins come from is the honest part: upstream publishes no checksums (the release
API answers `digest: null`), so the four rows in `internal/images/upscaler_install.go` were
computed from the archives themselves on 2026-10-18, and the sizes were then checked against
the release API's figures. `TestLiveUpscalerInstallFetchesThePinnedArchive` re-downloads the
archive for this platform and installs it, which is what catches a typo in a digest or an
asset that was replaced under its version:

```sh
KICHAT_LIVE=1 go test ./ -run TestLiveUpscalerInstall -v
```

A platform with no published archive (arm64 Linux or Windows) says so in the row, naming
the platform, instead of failing at the end of a download — that is what the manual below is
for. Nothing in this feature is bundled with the app.

### Installing it by hand

For a platform with no published archive, or a program installed somewhere else. It is one
folder and one setting, and it took about two minutes here, most of it the download.

**1. Download the archive that has the weights.** Two repositories are in play and both
call something v0.2.0: the sibling repository `xinntao/Real-ESRGAN-ncnn-vulkan` publishes a
3,654,741 B Linux zip under that tag holding the program and no weights at all, which is
the trap in the table above. The parent repository's `realesrgan-ncnn-vulkan-20220424-*`
archive is the one to take, from
<https://github.com/xinntao/Real-ESRGAN/releases/tag/v0.2.5.0>:

| platform | asset | size | sha256 (computed here; upstream publishes none) |
| --- | --- | --- | --- |
| Linux | `realesrgan-ncnn-vulkan-20220424-ubuntu.zip` | 46,931,474 B | `e5aa6eb131234b87c0c51f82b89390f5e3e642b7b70f2b9bbe95b6a285a40c96` |
| Windows | `realesrgan-ncnn-vulkan-20220424-windows.zip` | 45,474,481 B | `abc02804e17982a3be33675e4d471e91ea374e65b70167abc09e31acb412802d` |
| macOS | `realesrgan-ncnn-vulkan-20220424-macos.zip` | 51,817,124 B | `e0ad05580abfeb25f8d8fb55aaf7bedf552c375b5b4d9bd3c8d59764d2cc333a` |

All three were downloaded and hashed here, and each size agrees with the figure the
release API reports for that asset. Only the Linux one was *run*: the Windows archive is a
PE32+ x86-64 `.exe` beside `vcomp140.dll`, so it is x86-64 only, and the macOS executable is
a universal binary with x86_64 and arm64 slices (both read with `file`), which is why Apple
silicon does not need an archive of its own.

**2. Unpack the whole archive into one folder.** The program looks for its weights beside
*itself*, so `models/` and the executable have to stay siblings — a binary copied out on
its own will not find them. KiChat's data directory is `~/.config/kichat` on Linux,
`~/Library/Application Support/kichat` on macOS and `%APPDATA%\kichat` on Windows:

```
~/.config/kichat/upscalers/realesrgan-ncnn-vulkan/20220424/
├── realesrgan-ncnn-vulkan            11,399,776 B
├── models/
│   ├── realesrgan-x4plus.bin         33,424,520 B   what the panel uses by default
│   ├── realesrgan-x4plus.param
│   ├── realesrgan-x4plus-anime.bin   8,943,500 B
│   ├── realesrgan-x4plus-anime.param
│   └── realesr-animevideov3-x2/x3/x4.{bin,param}
├── README_ubuntu.md
└── LICENSE-ncnn-vulkan-mit.txt
```

Anywhere it can sit works, as long as those two stay together — `runtime/` does not,
because that is where the llama.cpp builds live and the app lists whatever it finds there.
The archive's `input.jpg`, `input2.jpg` and `onepiece_demo.mp4` are demos and can go; the
three `animevideov3` pairs are kept for completeness and are not offered in the panel.

**3. Make it executable (Linux and macOS).** The zip stores no exec bit, so an unpacked
copy will not start:

```sh
chmod 0755 ~/.config/kichat/upscalers/realesrgan-ncnn-vulkan/20220424/realesrgan-ncnn-vulkan
~/.config/kichat/upscalers/realesrgan-ncnn-vulkan/20220424/realesrgan-ncnn-vulkan -h
```

The second line is the check: it prints the usage block (so does running it with no
arguments — both exit 255, which is simply this program's answer). Seeing it means the
binary starts. It says nothing about Vulkan yet.

**4. Point KiChat at it.** Settings → Image generation → **Enlarging**:

- **Enable Real-ESRGAN** — on. Nothing runs before this, and a neural request while it is
  off is refused with an error that names the switch rather than the missing program.
- **Program** → *Choose program* → the executable from step 2. Left empty, KiChat looks
  for `realesrgan-ncnn-vulkan` on `PATH`, which suits a system-wide install.
- **Model** — `realesrgan-x4plus` for photographs, `-anime` for flat art. A model that is
  not in `models/` is refused before the program starts, and the error names the file.
- **Tile size** — leave empty (the program's own choice) unless VRAM is tight at a 4K
  target.
- **Test** — runs a 16×16 picture through the program. It prints the program's path on
  success; on failure it prints the program's own error, which is where a missing Vulkan
  driver shows up.

None of this touches the image server: the switch works with generation off, and no server
needs to be running.

**5. Enlarge something.** Open any picture, press **Enlarge…**, choose Real-ESRGAN, and the
panel names the result size before anything starts. Under the picture the row reads
`Enlarging with Real-ESRGAN…` with a Stop beside it. Measured here: a 256² source reached
1920² in 2.4 s, and a 512² source reached 2048² in 2.3 s.

#### What the failures mean

| what you see | what it is |
| --- | --- |
| *Real-ESRGAN ist nicht eingerichtet — in den Einstellungen herunterladen oder den Pfad wählen* | the switch is on and no program was found: an empty Program field with nothing on `PATH`, or a path that no longer exists. **Download Real-ESRGAN** installs it without leaving the window |
| *Real-ESRGAN hat keine Modellgewichte …* with a `fopen … failed` line after it | the program is there and the weights are not — the small sibling-repository archive, or a binary moved away from its `models/` folder, or an archive unpacked somewhere its `models/` did not follow. Without the check the program would have "succeeded" here, writing a black picture |
| the program's own message about a Vulkan device, or an instant exit with no picture | no Vulkan loader or no driver. Nothing in KiChat fixes that, and the released build has no CPU switch. A software Vulkan device (llvmpipe) does work, just slowly: 22.6 s for the 220²→880² that took 1.36 s on the GPU measured here |
| Test says ok but the picture is soft | the model's own softness. The Sharpen slider belongs to the **resample** method and is not applied to this one; the anime weights are for flat art, not for photographs |

To undo it: turn the switch off and clear the Program field (or delete those two keys from
the settings row), then delete the folder. Nothing else depends on it.

Keep the licence beside the files: the program is MIT — the small archive carries the file,
which is where the copy in the tree above came from — and the weights are
Real-ESRGAN's BSD-3-Clause. The released archives state no separate terms for the weights
themselves.

## 7. Targets and arithmetic

Presets are **long-side** based, because a photograph is not 16:9:

| preset | long side | 512×512 becomes | 800×600 becomes | 3000×2000 becomes |
| --- | --- | --- | --- | --- |
| FHD | 1920 | 1920×1920 | 1920×1440 | 1920×1280 (down) |
| 4K | 3840 | 3840×3840 | 3840×2880 | 3840×2560 |
| Custom | typed long side | — | — | — |

- Aspect ratio is preserved; nothing is stretched or cropped. Short side is
  `round(longSide × short/long)`, minimum 1.
- "Custom" in v1 is a **long side** in pixels (prefilled with `2 × current`), not a
  free W×H box: an exact box on a different ratio forces a choice between letterbox
  and crop, and that deserves its own control later. A free W×H box that fits inside
  the ratio (no crop) can be added without changing the model.
- A target smaller than the source is a downscale, and is allowed: it is the same
  operation with a scale below 1. Label it "Resize" in that case.
- Effective scale is shown before the click (`512² → 1920², 3.75×`), and a scale above
  8× gets a warning sentence — at that point Lanczos is a blur and even Real-ESRGAN is
  inventing more than it knows.
- `MaxDimension`/`MaxOutputDimension` from `limits.go` are **not** reused. A resample
  has no latent grid, no 8-pixel rounding and no 4096 cap; it has the 8192/64 MP cap
  above. Conflating the two would either forbid a legal 4K square or pretend a
  diffusion bound applies to a resize.

## 8. UI

On the picture card (`MessageImage` in `MessageParts.tsx`):

- One **Enlarge…** button for every image attachment. It opens an inline panel in the
  same place and style as `RecipeEditor`:
  - **Target:** FHD · 4K · Custom (long side) — with the resulting size rendered live.
  - **Method:** three radio rows with a one-line cost each:
    *Resample (Lanczos) — about a second, no extra software*;
    *Neural (Real-ESRGAN 4×) — seconds, needs a Vulkan GPU* — with the switch off it
    reads *not set up* and points at Settings; with the switch on but the binary
    missing or Vulkan absent it is disabled with the backend's own reason;
    *AI redraw (hires) — minutes, draws a new picture* (disabled when the attachment
    has no recipe).
  - **Sharpen** slider (tier 1 only), **Model** (photo/anime, tier 2 only).
  - One primary button naming the result: `Enlarge to 3840×2160`, with a **Stop** beside
    it while a run is in flight and only for the two methods that can act on one — a
    redraw and the neural pass. The panel's own hint says what is *not* happening: unlike
    a draw, no image server is interrupted, because none is involved.
- While a run is in flight the picture carries the row it belongs to, the way a redraw
  does: `Enlarging with Real-ESRGAN…` and no step counter, because the program's output
  cannot be read. That is the whole progress surface on purpose — an indeterminate bar
  and a Stop are the honest pair for a wait of unknown length, and a fabricated percentage
  would be a lie the user could time.
- The existing `FHD 1920×…` / `4K 3840×…` buttons stay, but become **shortcuts into
  this panel** pre-set to that target and to the last method used, rather than an
  immediate hires redraw. Users who want the old one-click get one more click; users
  who want the fast one stop paying minutes for it. (Alternative: keep both pairs and
  label the old ones "AI redraw FHD/4K" — rejected as four buttons for two ideas.)
- Generalise the existing FHD/4K visibility rule: they currently require a recipe with
  a known base size. The new buttons require only a decodable image with a known
  size — `Size` on the stored document answers that.
- Settings → Images gets an **Enlarging** group: default method (Resample), sharpen,
  the **Enable Real-ESRGAN** switch (off by default), its status/install/Test/model,
  and the note that resampling works without an image server. The whole group is
  **not** gated on `images.enabled`: upscaling an attachment is a picture operation,
  not a generation. The new fields (`upscalerEnabled`, `upscaleDefaultMethod`,
  `upscaleSharpen`, `esrganPath`, `esrganModel`) need no migration — the blob decodes
  over the defaults, so an existing install reads them as off and zero.
- Every string goes through `frontend/src/lib/i18n.ts` in `de`, `en`, `zh`. Suggested
  keys: `images.enlarge`, `images.enlargeTarget`, `images.enlargeMethodResample`,
  `images.enlargeMethodNeural`, `images.enlargeMethodRedraw`,
  `images.enlargeHintResample`, `images.enlargeSharpen`, `images.enlargeWarning`,
  `settings.images.upscaler*`.
- The mock bridge needs `UpscaleAttachment` and `GetUpscalerStatus` fixtures, and the
  harness needs a `?upscale=esrgan` flag to render the three neural states: switch off,
  switch on but not installed, and ready. `&upscalehold=1` is the fourth state — a neural
  run still going — and it shares the mock's pending-draw slot, which is the real shape:
  one picture operation at a time, found by the source attachment's id, ended by the same
  Stop.

## 9. Backend surface

```go
// api_upscale.go
func (a *App) UpscaleAttachment(attachmentID string, req images.UpscaleRequest) (store.Attachment, error)
func (a *App) GetUpscalerStatus() (tools.UpscalerStatus, error) // switch, found, path, models, error
func (a *App) TestUpscaler() (string, error)                    // tier 2; one tiny picture through the GPU
func (a *App) PickUpscalerBinary(title string) (string, error)  // the file dialog; no filter
```

The status is `tools.UpscalerStatus` rather than a type in `images`, because "is the
switch on" is a settings question and the tool service is what holds the settings; the
resampler itself needs neither. `InstallUpscaler` is the one that is absent, because
there is nothing to install yet (§6). There is deliberately **no `StopUpscale`**: stopping
an enlargement goes through `StopImageDraw` with the source attachment's id, which is the
same call a redraw makes, because one picture has one thing running on it and a second
Stop binding would be a second answer to the same click.

`UpscaleRequest` carries `Target` (`fhd`/`uhd`/`custom`), `LongSide`, `Method`
(`lanczos`/`esrgan`), `Model`, `Sharpen`. It carries no width and height: the target is a
long side and the ratio comes from the source, so a request cannot ask for a shape the
source does not have. The *redraw* method does
not travel through this call: it is the existing `RegenerateImage` with an enlarged
recipe, so tier 3 keeps its current code and its current progress reporting.

Concurrency: an upscale takes the same `tryDraw` slot a draw does, and is refused with
`ErrDrawInProgress` while one is held. A 4K resample does not compete for the image
server, but a second simultaneous encode would compete for memory, so the existing
one-at-a-time rule is simpler than a second slot with a different meaning.

Stop is the same path, and it is registered under the **source attachment's** id — the
same id a redraw uses, so the Stop beside a picture stops whichever of the two is running
on it. One difference is real: an upscale is registered with no service (`registerUpscale`),
because Stop's second half is asking a backend to abandon the work behind a request, and
there is no backend here. Cancelling the context is the whole of it — the classical pass is
local, and the neural one is a child process the app kills through that cancellation. Asking
a WebUI server to interrupt would abandon whatever *it* happens to be drawing, which is
someone else's picture.

## 10. Data model

The enlarged copy is a **new attachment**, filed on the source's message, never an
overwrite. That is what lets a user compare, keep both, and save whichever they want.

It is not a generation, so it gets its own provenance rather than a fake recipe:

```go
type Derivation struct {
    SourceAttachmentID string  `json:"sourceAttachmentId"`
    SourceWidth        int     `json:"sourceWidth,omitempty"`
    SourceHeight       int     `json:"sourceHeight,omitempty"`
    Method             string  `json:"method"`   // "lanczos" | "esrgan"
    Model              string  `json:"model,omitempty"`
    Scale              float64 `json:"scale,omitempty"`
    Sharpen            float64 `json:"sharpen,omitempty"`
    DurationMs         int64   `json:"durationMs,omitempty"`
}
```

`Scale` is read from the pixels rather than copied from the request, so a target the cap
clamped reports what actually happened. `DurationMs` is what the card would show beside a
seed; nothing renders it yet.

- `attachments.derivation_json TEXT NOT NULL DEFAULT ''` as **migration 6**, exactly
  the shape of migrations 4 (`generation_json`) and 5 (`saved_path`), and for the same
  reason: an existing database must run only the new entry.
- `Attachment.Derivation *Derivation`. `Generation` stays nil for an upscaled copy
  unless it was a redraw (which already has a recipe). The card renders
  `3.75× Lanczos from cat.png` in place of a seed; Redraw/Variation appear only when a
  generation recipe exists, pointing at the *source* picture's recipe rather than
  inventing one.
- The source is named by its attachment id, so a second enlarge of the same
  attachment is a second, independent row. No dedupe: two different methods of the
  same source are the comparison the feature is for.

## 11. Tests

Offline, no network, no keyring:

- `Fit` table tests: landscape, portrait, square, exact, downscale, odd dimensions,
  the caps, and a target equal to the source.
- Round-trip, in `internal/images/upscale_test.go`: encode a checkerboard and a
gradient, upscale, decode, assert dimensions and that corner colours survive; assert
the output container matches the input, that a JPEG stays a JPEG, and that a
transparent edge stays transparent.
- Alpha test: a transparent PNG's edges stay transparent and do not darken.
- EXIF test: the eight orientation values on a small JPEG rotate as documented (or are
  rejected explicitly, never silently ignored).
- `image_upscale_test.go` on the bridge: the source is untouched, a second attachment
  is filed on the same message, `events.Attachments` fires, the result's `Derivation`
  records the method and both sizes, and a second call while one runs returns
  `ErrDrawInProgress`.
- The stopped run, end to end, without a GPU: a stub program that copies its input after a
  minute stands in for `realesrgan-ncnn-vulkan`. The row appears under the **source
  attachment's** id and is scoped to its chat, `StopImageDraw` reports it was drawing, the
  call comes back as `ErrCancelled` rather than as a failure, the `done` report arrives so
  the row leaves the screen, and nothing is filed — a stopped enlargement is not a picture.
- The phase is reported once, for the neural method, and **not** when the request is
  refused (switch off, program missing): a request that cannot run must not leave a row
  behind, and the classical pass must not flash one.
- Cancelling kills the program instead of waiting it out: the stub writes its pid, and the
  test asserts the process is gone and that the call returned in far less than the timeout.
- A request for the neural method with the switch off is refused before a provider or a
  binary is touched, and the error names the switch rather than the missing program.
- A run that loaded no weights is a failure, not a black picture: a stub that writes a
  decodable output and exits 0 while saying `find_blob_index_by_name data failed` must be
  refused as `ErrUpscalerNoModels`; and a `models/` folder holding the *other* model's
  weights is refused before the program starts, naming the file it wanted.
- The redraw path keeps its existing tests; `UpscaleRequest` never reaches a provider.
- Real-ESRGAN, the real program: `TestLiveESRGAN`, gated by `KICHAT_LIVE=1` plus
  `KICHAT_ESRGAN=/path/to/realesrgan-ncnn-vulkan`, skipped when absent. It runs the probe,
  requires the weights beside the program, upscales a 64² checkerboard and prints how long
  it took. **Run on this machine** against the 20220424 archive: pass, `64² → 256×128 in
  1.133 s`. Pointed at the same program with its `models/` folder removed it fails with
  `ErrUpscalerNoModels` and the program's own `fopen … failed` line — which is the case
  that used to look like success.
- UI: `make frontend && go run ./tools/uiharness`, all three methods, the three neural
  availability states, the 4K result rendered, and `&upscalehold=1` for the running row
  and its Stop.

**Timings measured on one machine** (Ryzen 7 5800X3D; AMD Radeon RX 7900 XT via RADV
NAVI31; llvmpipe present as device 1). They are one machine's numbers, taken to replace
guesses, not a promise for another:

| pass | time |
| --- | --- |
| Lanczos 512²→1920² | 132 ms (21 MB allocated) |
| Lanczos 512²→3840² | 469 ms (70 MB allocated) |
| x4plus 220²→880², GPU | 1.36 s (program's wall time, weights load included) |
| x4plus 512²→2048², GPU | 2.31 s |
| x4plus 220²→880², llvmpipe (`-g 1`) | 22.6 s |
| our client, x4plus 64²→256², GPU | 1.13 s end to end |

The shape worth remembering: the neural tier's cost is dominated by starting the program
and loading 33 MB of weights, not by the picture, so a 2-megapixel result and a
0.2-megapixel one differ by under a second. On a machine with no usable Vulkan card the
same work is ~17× slower through llvmpipe and still finishes, which is why the panel says
"seconds on a Vulkan GPU" rather than claiming a device.

## 12. Build order

1. **Shipped:** resampler, EXIF, derived attachment and migration 6, `UpscaleAttachment`,
   the Enlarge panel and the FHD/4K shortcuts, the Settings group, i18n, harness fixtures,
   the neural path behind a switch that is off by default, and its phase row with the Stop
   that kills the program.
2. **Next, in this order:** the managed Real-ESRGAN download (pinned checksum, temp dir,
   notices), then a live test against the real binary alongside the benchmark it needs —
   that one needs a Vulkan machine, so it is the piece that may wait.
3. **Later, driven by use:** defaulting the switch on once measured, model choice, tile
   control, a free W×H box with fit, and "enlarge every picture in this chat".

Nothing here is gated on `images.enabled`: enlarging an attachment neither needs nor
starts an image server.

## 13. Decisions taken

1. **Both tiers in the first change, Real-ESRGAN behind a switch, off by default.** The
   feature ships complete in one pass; the switch is the answer if a Vulkan driver
   misbehaves on one machine. The managed download landed behind the same switch rather
   than with it, because a switch that only ever refused would have been untestable; what
   remains is the download, not the gate.
2. **The FHD/4K buttons open the method panel.** One concept for two targets, and the
   resulting size and cost are visible before a minutes-long hires pass. The old
   one-click hires becomes two clicks; the panel's default method is a setting, so a
   user who always wants hires is not re-choosing each time.
3. **The output follows the source container.** PNG→PNG, JPEG→JPEG at q92,
   WebP/GIF/BMP→PNG, alpha preserved, long side capped at 8192. If 4K pictures make the
   chat feel heavy, the fix is a preview derivative served to the card, not re-encoding
   the stored picture — that is its own change.

Still open, deliberately: whether the Real-ESRGAN switch defaults on after the
benchmark, and whether "Custom" grows a free W×H box with crop/fit. Both wait for
measurement and use.

## 14. Sources for the vendor claims

Archive contents, sizes and hashes above were **measured here** on 2026-10-18: both zips
downloaded, unpacked, and run. The release pages themselves, read for names and coverage:

- Real-ESRGAN-ncnn-vulkan — <https://github.com/xinntao/Real-ESRGAN-ncnn-vulkan>,
  releases <https://github.com/xinntao/Real-ESRGAN-ncnn-vulkan/releases/tag/v0.2.0>
  (v0.2.0, April 2022; 3,654,741 B for ubuntu, 2,159,606 B for windows; `digest: null`,
  so no upstream checksum exists)
  licence <https://github.com/xinntao/Real-ESRGAN-ncnn-vulkan/blob/master/LICENSE>
- The archive that has the weights —
  <https://github.com/xinntao/Real-ESRGAN/releases/tag/v0.2.5.0>, asset
  `realesrgan-ncnn-vulkan-20220424-ubuntu.zip`, 46,931,474 B,
  sha256 `e5aa6eb131234b87c0c51f82b89390f5e3e642b7b70f2b9bbe95b6a285a40c96` (computed
  here, since upstream publishes none). The macOS and Windows assets of the same release
  are 51,817,124 B and 45,474,481 B.
- Real-ESRGAN itself (weights, BSD-3-Clause) —
  <https://github.com/xinntao/Real-ESRGAN>
- ncnn, and the Vulkan back end — <https://github.com/Tencent/ncnn>
- ONNX Runtime Go binding requiring CGO — <https://github.com/yalue/onnxruntime_go>;
  the purego attempt — <https://github.com/shota3506/onnxruntime-purego>
- Pure-Go ONNX engines and their partial operator coverage —
  <https://github.com/oramasearch/onnx-go>, <https://github.com/AdvancedClimateSystems/gonnx>
- ONNX exports of x4plus (community, ≈64–69 MB fp32, opset 17) —
  <https://huggingface.co/SceneWorks/real-esrgan-onnx>
- `golang.org/x/image` — <https://pkg.go.dev/golang.org/x/image/draw> (CatmullRom, no
  Lanczos), <https://pkg.go.dev/golang.org/x/image/webp> (decode only)
