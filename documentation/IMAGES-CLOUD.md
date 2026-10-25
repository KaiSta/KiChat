# IMAGES-CLOUD.md

Hosted picture generation: which providers give pictures away for free, what each
one's free tier actually is, and what each would cost to wire into this app.

**Status: a survey, plus the one provider it selected.** KiChat now draws through a fourth
kind, **NVIDIA NIM** (`ImageSettings.Kind = "nim"`, `internal/images/nim.go`), which is the
provider §1 ranks second — it reuses the key the Backends tab already holds, and it is the
only hosted kind implemented. The rest of this document is the survey that decision came
from: what each free cloud image API really gives, and what the others would still cost to
support. Everything below was read on **22 October 2026** from the pages linked in §8, and
§7 separates what a vendor published from what this document worked out. The implementation
itself is documented in [`IMAGES.md`](IMAGES.md) §2.

The question it answers is the one the chat side makes look easy: Groq and Mistral
hand out a recurring free token quota, so what is the equivalent for pictures? The
answer is "nearly nothing" — exactly one provider has a recurring, no-card
allowance, and the second-best offer is a prototyping allowance rather than a tier.

## 1. The ranking

| # | provider | free allowance | models reachable for free | output licence | API |
| --- | --- | --- | --- | --- | --- |
| 1 | **Cloudflare Workers AI** | 10,000 Neurons/day, resets 00:00 UTC, no card; text-to-image capped at 720 requests/min | FLUX.1 [schnell], SDXL-Lightning, SDXL Base 1.0 | Apache 2.0 / OpenRAIL++-M — commercial output allowed | bespoke: `POST /client/v4/accounts/{id}/ai/run/@cf/…`, base64 under `result.image` |
| 2 | **NVIDIA NIM** (build.nvidia.com) | free for prototyping with a Developer account; community reports ~1,000 initial requests; **no published recurring quota** | the two the hosted route was measured to serve: FLUX.1 [schnell], FLUX.1 [dev] (the NIM *containers* support more) | per model; **FLUX.1 [dev] is non-commercial for production** | `POST ai.api.nvidia.com/v1/genai/<publisher>/<model>`, picture at `artifacts[0].base64` |
| 3 | **Pollinations.ai** | keyed access, per-model "Pollen" pricing, `402` when exhausted; **no verifiable recurring allowance** | whatever the live catalogue prices | per model; service terms not readable in the capture | documented OpenAI-Images-compatible base at `/v1` |

Ranking rule: a recurring allowance first, then quality, then how little the
credential and request shape cost this app. Cloudflare wins on the first and third;
NVIDIA wins on quality and reuses a key the app already stores, but its free part is
finite; Pollinations is third because its free part could not be verified at all.

## 2. Cloudflare Workers AI

**The free part is real.** 10,000 Neurons per day on both the Free and Paid Workers
plans, resetting at 00:00 UTC, with no credit card. Text-to-image carries its own
rate limit of 720 requests per minute, which a desktop app will not approach.

**What a picture costs.** FLUX is metered in Neurons rather than per request.
Cloudflare's published rates are $0.0000528 per 512×512 tile and $0.0001056 per step,
and a Neuron is $0.011 per 1,000. A 1024² four-step picture is therefore about
50–60 Neurons — so **on the order of 150–200 free pictures a day**. That figure is
arithmetic on the published rates, not a vendor statement, and the call itself does
not confirm it: the REST answer is `{result, success, errors, messages}` with no
Neuron count in it. Reading the dashboard's usage before and after one picture is
what settles the real cost.

**The shape.** It is not OpenAI-compatible. The REST route is

```
POST https://api.cloudflare.com/client/v4/accounts/{ACCOUNT_ID}/ai/run/@cf/black-forest-labs/flux-1-schnell
Authorization: Bearer {API_TOKEN}
{ "prompt": "cyberpunk cat", "seed": "Random positive integer" }
```

and the picture comes back base64-encoded as JPEG under `result.image` (the docs'
own example builds a `data:image/jpeg;base64,` URI from it).

**What it cannot do** matters more here than the quota:

- **FLUX.1 [schnell] takes `prompt`, `steps` and `seed` only.** No `width`, no
  `height`, no `negative_prompt` — so every picture is the model's own square-ish
  default. The size presets in the Images panel (`SIZE_PRESETS`, 1280×720 and
  1920×1080) would be silently ignored.
- SDXL-Lightning and SDXL Base do accept width/height up to 2048, so aspect ratio is
  reachable only by choosing one of those models.
- No progress route: a draw is one request that answers when it is done, so
  `images.Progress` would never fire for this kind.

**Integration cost:** a backend of its own, because it needs an **account id in
addition to an API key** and a result envelope no other kind uses. Two extra settings
and one response shape.

## 3. NVIDIA NIM

**The free part is a prototyping allowance.** NVIDIA's Developer Program lets a member
use hosted NIM endpoints free for research, development, testing and prototyping;
community reports put the initial allocation at about 1,000 requests. NVIDIA publishes
no per-day figure and its terms do not license production use, so this is "enough to
build and demo", not "free forever".

**The catalogue is the best of the three**, but the *route* is not the one first written
down here, and that mistake produced a live 404 (22 October 2026). NVIDIA's visual-genai
NIM documentation shows an **OpenAI-compatible image route**:

```
POST {base}/v1/images/generations
{ "model": "flux.1-schnell", "prompt": "…", "n": 1,
  "response_format": "b64_json", "seed": 0, "steps": 4 }
```

but its Getting Started page runs that route on the **self-hosted** container
(`http://localhost:8000`). The **hosted** catalogue answers on a different host and path:

```
POST https://ai.api.nvidia.com/v1/genai/black-forest-labs/flux.1-schnell
{ "prompt": "…", "mode": "base", "width": 1024, "height": 1024, "seed": 0, "steps": 4 }
→ { "artifacts": [ { "base64": "…", "finishReason": "SUCCESS", "seed": 0 } ] }
```

Measured with a real key on that date: `POST https://integrate.api.nvidia.com/v1/images/generations`
answers `400 {"error":{"message":"model field is required"}}` for an empty body and a plain
`404 page not found` for every documented model name (`flux.1-schnell`, `flux.1-dev`,
`black-forest-labs/flux.1-schnell`, `stable-diffusion-3.5-large`, `qwen-image`), while
`ai.api.nvidia.com/v1/genai/black-forest-labs/flux.1-schnell` and `…/flux.1-dev` are routable
(a bad key gets 401, and any other publisher path gets 404 immediately). Three `genai` paths
were found to exist — those two and `…/flux-3-image-klein`, which answers 403 for a junk key
and 404 for its `-base` sibling. The longer model list above is what the NIM *containers*
support, not what the hosted catalogue publishes.

**`mode` is per model, and that is a real incompatibility.** FLUX.1 takes `"base"`; FLUX.3
Klein rejects it with `422 — Input should be 't2i' or 'i2i'`, so its route takes `"t2i"`. One
request shape for the whole host is therefore wrong, and the client carries the value with the
model id.

**A draw on the hosted route, measured 22 October 2026 on a free-trial key:**
`flux-3-image-klein`, `mode: "t2i"`, prompt `a lighthouse on a cliff at dusk, painted` →
`200` in **5.1 s**, a 1024×1024 JPEG of 130 441 bytes, `finishReason: "SUCCESS"` and a seed
echoed back. The same route answered `200` in 0.6 s with `finishReason: "CONTENT_FILTERED"`
and no bytes for `a red bicycle leaning on a brick wall`, so the safety pass fires on prompts
that are not obviously risky and has to be reported rather than retried. `flux.1-schnell`
with `mode: "base"` did not return headers within 240 s, twice: the route and the key are
fine, so what is left is that model's own queue on the free trial.

**Why that matters for this app:** the NVIDIA chat backend's key works unchanged — the chat
API lives on `integrate.api.nvidia.com`, and the picture API on `ai.api.nvidia.com`, and an
`nvapi-…` token is accepted by both. No new credential and no new field.

**The licence trap.** FLUX.1 [dev]'s *model* licence is non-commercial and
non-production. Its terms say outputs are not model derivatives and may be used
commercially, but that does not license running the model in a commercial workflow.
A shipped default should be `flux.1-schnell` (Apache 2.0) or SD 3.5, not `flux.1-dev`.
FLUX.3 Klein is a third case: its weights are under the FLUX Kommunity License (free
commercial use below US$5 M of annual revenue), so it is offered beside schnell — and it is
the one that actually answered on a free-trial key — but it is not the default.

## 4. Pollinations.ai

It documents an OpenAI-Images-compatible base (`https://gen.pollinations.ai/v1`, with a
native `GET /image/{prompt}` form beside it), which would drop into the same client as
NVIDIA. The reason it is third is the free part: the current documentation requires a
key, prices generation per model in "Pollen", answers `402 PAYMENT_REQUIRED` when the
budget is spent, and describes legacy raw publishable keys as rate-limited. No
recurring free allowance was verifiable from the vendor pages, and the exact image
JSON schema and response body were not readable in the captured documentation.

Treat it as a convenience fallback a user may enable, never as a quota the panel
promises.

## 5. What is *not* free, and since when

Several providers look like the chat-side equivalents and are not:

- **Google AI Studio / Gemini.** The 2026 image models — Nano Banana 2.1, Gemini 3.1
  Flash Image, Gemini 3.1 Flash Lite Image — are listed **"Free Tier: Not available"**
  on the pricing page. A free Gemini *text* tier does not imply free image output.
  Free image generation in the consumer Gemini app is not API access, and its terms
  differ (unpaid-service prompts may be reviewed and used to improve products). The
  Imagen endpoints were retired on **17 August 2026**.
- **Together AI.** No free trial: access needs a prepaid balance and a $15 first
  purchase. The OpenAI-compatible image route exists, but its free model
  (`black-forest-labs/FLUX.1-schnell-free`) was **removed from serverless on 23
  December 2025**, so there is no free model left behind the compatible shape.
- **Hugging Face Inference Providers.** Free accounts get **no monthly credits**; PRO
  ($2/month of compute) is paid, and its OpenAI-compatible router is for chat, not for
  `/v1/images/generations`.
- **Stability AI.** 25 signup credits (1 credit = $0.01) — a one-time trial. Its
  model licence (Community License, under $1M annual revenue) is a separate thing from
  hosted credit.
- **Nebius Token Factory.** $1 trial credit for 30 days, card required at signup.
- **OpenRouter.** Its 20 RPM / 50 requests-per-day free allowance applies to free
  *text* models; its image route is billed per image.
- **Replicate, fal, Leonardo, DeepAI.** Pay-as-you-go, campaign credit, or
  subscription; none publishes a recurring free API image allowance.

## 6. What this means for the code

1. **The hosted backend was built against the vendor's own genai route, not the
   OpenAI-compatible one.** `internal/images/nim.go`, chosen in a fourth
   `ImageSettings.Kind`, posts `prompt`/`mode`/`width`/`height`/`steps`/`seed` to
   `/v1/genai/<publisher>/<model>` and reads `artifacts[].base64`; the key comes from the
   NVIDIA row of the Backends tab, so no new credential is asked for. The OpenAI-compatible
   shape would still be the right one for Pollinations and Together, and the client accepts
   `data[].b64_json` as a second envelope for exactly that reason.
2. **Cloudflare was not built, and is the next one if it ever is**: a second, smaller
   backend for the account-scoped route, the `result.image` envelope and the missing
   width/height — plus its own account id and token, which is two new fields for a
   provider whose model list is shorter.
3. **The two interface gaps were handled rather than worked around:** the provider does not
   implement `images.Progressive`, so progress simply never fires (no fake percentage), and
   the panel hides the fields the hosted route does not take instead of accepting them and
   dropping them. A hires request is refused with `ErrHiresUnsupported` rather than answered
   with a base-size picture.
4. **Defaults are a commercially licensed model** — `flux.1-schnell` (Apache 2.0) — because
   the free host does not change the model's licence; `flux-3-image-klein` and `flux.1-dev`
   are selectable and not the default, and the panel offers exactly the ids the route was
   measured to serve. The same rule would apply to Cloudflare's SDXL models.

## 7. Observed, derived, unverified

- **Observed on vendor pages (22 Oct 2026):** Cloudflare's 10,000 Neurons/day, 00:00
  UTC reset, 720 requests/min and the FLUX parameter list; NVIDIA's
  OpenAI-compatible image route and model names; Gemini's "Free Tier: Not available"
  for its current image models and the Imagen retirement date; Together's deprecation
  of the free FLUX model; Hugging Face's zero monthly credits for free accounts;
  Stability's 25-credit signup.
- **Derived here:** the 50–60 Neurons / 150–200 pictures per day figure, from
  Cloudflare's per-tile, per-step and per-Neuron rates. A live call can settle it only
  through the dashboard's usage counter — the REST response carries no Neuron count.
- **Measured against the live service (22 Oct 2026, one free-trial key):** the three
  `genai` paths and the 404-for-unknown-path rule; `mode: "t2i"` for klein and the 422 for
  `base`; a 5.1 s 1024×1024 JPEG from klein; the `CONTENT_FILTERED` answer with no bytes;
  and no response from `flux.1-schnell` within 240 s. The client was changed to match, so
  these are facts about the service, not about a stand-in server.
- **Unverified:** NVIDIA's ~1,000 initial requests (community reports, not a vendor page),
  and whether `flux.1-schnell` ever answers on the free trial or is permanently queued;
  Pollinations' free allowance and exact image schema; whether Cloudflare's SDXL models
  accept the full 2048 bound in practice.
- **Not tested at all:** picture quality from any of these hosts. The judgements in
  §1 are about allowances and request shapes, not about whether the pictures are good.

## 8. Sources

- Cloudflare Workers AI pricing — <https://developers.cloudflare.com/workers-ai/platform/pricing/>
- Cloudflare Workers AI limits — <https://developers.cloudflare.com/workers-ai/platform/limits/>
- Workers AI REST API (account id, token, `{result, success, errors, messages}` envelope) — <https://developers.cloudflare.com/workers-ai/get-started/rest-api/>
- `flux-1-schnell` on Workers AI (route, parameters, `result.image`) — <https://developers.cloudflare.com/workers-ai/models/flux-1-schnell/>
- Workers AI OpenAI compatibility scope (chat and embeddings, not images) — <https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/>
- NVIDIA NIM, OpenAI-compatible image generation and its model names — <https://docs.nvidia.com/nim/visual-genai/latest/api/openai-image-generation.html>
- NVIDIA NIM getting started: the `Curl - OpenAI` request bodies (`steps`, `seed`, `n`, `response_format`), FLUX.3 Klein's unsupported `negative_prompt`, and the fact that the OpenAI route runs on the *self-hosted* container at `localhost:8000` — <https://docs.nvidia.com/nim/visual-genai/latest/getting-started.html>
- Hosted FLUX.1-schnell reference, the model's own `ai.api.nvidia.com/v1/genai/…` route — <https://docs.api.nvidia.com/nim/reference/black-forest-labs-flux_1-schnell-infer>
- Hosted FLUX.1-schnell model page (the `artifacts[0].base64` answer) — <https://build.nvidia.com/black-forest-labs/flux_1-schnell>
- FLUX.3 Klein release and licence — <https://bfl.ai/blog/flux-3-image-klein>
- NVIDIA NIM for developers — <https://developer.nvidia.com/nim>
- FLUX.1 [schnell] model card and licence — <https://github.com/black-forest-labs/flux/blob/main/model_cards/FLUX.1-schnell.md>
- FLUX.1 [dev] non-commercial licence — <https://bfl.ai/legal/non-commercial-license-terms>
- Gemini API pricing ("Free Tier: Not available" for the image models) — <https://ai.google.dev/gemini-api/docs/pricing>
- Gemini image generation and the Imagen migration — <https://ai.google.dev/gemini-api/docs/imagen>
- Gemini API terms (unpaid-service data use) — <https://ai.google.dev/gemini-api/terms>
- Together API deprecations (free FLUX model removed 2025-12-23) — <https://docs.together.ai/docs/deprecations>
- Together billing and credits — <https://docs.together.ai/docs/billing-credits>
- Hugging Face Inference Providers pricing — <https://huggingface.co/docs/inference-providers/en/pricing>
- Hugging Face text-to-image task API — <https://huggingface.co/docs/inference-providers/en/tasks/text-to-image>
- Stability AI pricing (25 signup credits) — <https://docs.stability.ai/pricing>
- Stability AI licence (Community License terms) — <https://stability.ai/license>
- Nebius Token Factory billing — <https://docs.tokenfactory.nebius.com/other-capabilities/billing-new>
- OpenRouter rate limits — <https://openrouter.ai/docs/api_reference/limits>
- OpenRouter image generation — <https://openrouter.ai/docs/guides/overview/multimodal/image-generation>
- Pollinations API documentation — <https://gen.pollinations.ai/docs>
