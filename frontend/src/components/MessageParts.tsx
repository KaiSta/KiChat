import { memo, useEffect, useRef, useState } from 'react'
import { App, formatDuration, formatNumber, formatSpeed } from '../lib/api'
import type { store as storeTypes } from '../lib/api'
import { useThrottled } from '../lib/stream'
import { useStore } from '../store'

/* -------------------------------------------------------------------------
 * Speed and token statistics for one generation.
 * ---------------------------------------------------------------------- */

export function MessageStats({ message }: { message: storeTypes.Message }) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)

  if (message.role !== 'assistant') return null
  if (!message.tokensPerSecond && !message.completionTokens && !message.promptTokens) return null

  // Being explicit about provenance matters: a number we timed ourselves is not
  // the same as one the backend reported.
  const estimated = message.statsSource === 'estimated'

  return (
    <div className="mt-2 text-[11px]" style={{ color: 'var(--text-faint)' }}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span title={t('metrics.tokensPerSecond')}>
          <strong style={{ color: 'var(--text-dim)' }}>{formatSpeed(message.tokensPerSecond)}</strong>{' '}
          {t('metrics.tokensPerSecond')}
        </span>
        {message.ttftMs > 0 && (
          <span title={t('metrics.ttft')}>
            {t('metrics.ttft')} <strong style={{ color: 'var(--text-dim)' }}>{formatDuration(message.ttftMs)}</strong>
          </span>
        )}
        {message.completionTokens > 0 && (
          <span>
            <strong style={{ color: 'var(--text-dim)' }}>{formatNumber(message.completionTokens)}</strong>{' '}
            {t('metrics.tokens')}
          </span>
        )}
        {message.generationMs > 0 && (
          <span>
            {t('metrics.duration')} <strong style={{ color: 'var(--text-dim)' }}>{formatDuration(message.generationMs)}</strong>
          </span>
        )}
        {estimated && <span className="badge">{t('metrics.estimated')}</span>}
        {message.status === 'interrupted' && (
          <span className="badge badge-warn">{t('chat.interrupted')}</span>
        )}
        <button
          type="button"
          className="underline decoration-dotted"
          onClick={() => setOpen((v) => !v)}
        >
          {t('metrics.details')}
        </button>
      </div>

      {open && (
        <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-0.5 max-w-md">
          <Row label={t('metrics.model')} value={message.model || '–'} />
          <Row label={t('metrics.backend')} value={message.backend || '–'} />
          <Row label={t('metrics.promptTokens')} value={formatNumber(message.promptTokens)} />
          <Row label={t('metrics.completionTokens')} value={formatNumber(message.completionTokens)} />
          <Row label={t('metrics.stopReason')} value={message.stopReason || '–'} />
          <Row
            label={t('metrics.duration')}
            value={message.generationMs ? formatDuration(message.generationMs) : '–'}
          />
          <Row
            label={t('metrics.tokensPerSecond')}
            value={
              message.tokensPerSecond
                ? `${formatSpeed(message.tokensPerSecond)} ${estimated ? `(${t('metrics.estimated')})` : `(${t('metrics.measured')})`}`
                : '–'
            }
          />
        </dl>
      )}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt style={{ color: 'var(--text-faint)' }}>{label}</dt>
      <dd className="mono truncate" style={{ color: 'var(--text-dim)' }} title={value}>
        {value}
      </dd>
    </>
  )
}

/* -------------------------------------------------------------------------
 * Tool calls
 * ---------------------------------------------------------------------- */

export function ToolCallCard({ call }: { call: storeTypes.ToolCallRecord }) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)
  // A draw the model asked for is registered under the message this call is rendered on,
  // because that is the only row the view has until the picture exists. Reading it here is
  // what turns a tool call that means "minutes of GPU" into something the user can watch and
  // stop — the two things a tool card could not do for a picture until now.
  const draw = useStore((s) => s.draws[call.messageId])
  const [stopping, setStopping] = useState(false)

  const tone =
    call.status === 'ok'
      ? 'badge-ok'
      : call.status === 'error'
        ? 'badge-danger'
        : call.status === 'cancelled'
          ? 'badge-warn'
          : 'badge-accent'

  const parsedArgs = prettyJSON(call.argsJson)
  const result = parseResult(call.resultJson)

  return (
    <div
      className="card my-2 px-3 py-2 text-[12px]"
      style={{ background: 'var(--surface-2)', borderColor: 'var(--border)' }}
    >
      <button
        type="button"
        className="flex w-full items-center gap-2 text-left"
        onClick={() => setOpen((v) => !v)}
      >
        <span className={`badge ${tone}`}>
          {call.status === 'ok'
            ? t('tool.ok')
            : call.status === 'error'
              ? t('tool.error')
              : call.status === 'cancelled'
                ? t('tool.cancelled')
                : t('tool.running')}
        </span>
        <strong style={{ color: 'var(--text)' }}>
          {t(`tool.${call.name}`) !== `tool.${call.name}` ? t(`tool.${call.name}`) : call.name}
        </strong>
        <span className="faint mono truncate" style={{ maxWidth: 360 }}>
          {summariseArgs(call.argsJson)}
        </span>
        <span className="ml-auto faint">{formatDuration(call.durationMs)}</span>
      </button>

      {draw && <LiveDraw preview={draw.preview} step={draw.step} steps={draw.steps} className="mt-2" />}
      {draw && (
        <div className="mt-1 flex items-center gap-2">
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={stopping}
            title={t('images.stopHint')}
            onClick={async () => {
              setStopping(true)
              try {
                await App.StopImageDraw(call.messageId)
              } catch (err) {
                useStore.getState().toast('error', String(err))
              } finally {
                setStopping(false)
              }
            }}
          >
            ⏹ {t('images.stop')}
          </button>
        </div>
      )}

      {open && (
        <div className="mt-2 space-y-2">
          <div>
            <div className="faint mb-1">{t('tool.arguments')}</div>
            <pre
              className="mono overflow-x-auto rounded p-2 text-[11px]"
              style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
            >
              {parsedArgs}
            </pre>
          </div>
          {call.error && (
            <div>
              <div className="faint mb-1">{t('tool.error')}</div>
              <div style={{ color: 'var(--danger)' }}>{call.error}</div>
            </div>
          )}
          <div>
            <div className="faint mb-1">{t('tool.result')}</div>
            <pre
              className="mono max-h-72 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
              style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
            >
              {result}
            </pre>
          </div>
        </div>
      )}
    </div>
  )
}

/**
 * A picture that is still being drawn: what it looks like so far, and how far along it is.
 *
 * The preview is a real picture — the backend decodes a thumbnail of the latent as the draw
 * proceeds — and it is the same element in both places a draw can be watched from: under the
 * picture a redraw replaces, and inside the tool card of a draw the model asked for. Which is
 * why it is one component and not two: a draw that looked different depending on who started
 * it would be two features that have to be kept in step.
 *
 * A draw that cannot report its steps still renders: the step counter is simply absent and
 * the thumbnail, when there is one, is what says the machine is working.
 */
function LiveDraw({
  preview,
  step,
  steps,
  className,
}: {
  preview?: string
  step: number
  steps: number
  className?: string
}) {
  const t = useStore((s) => s.t)
  return (
    <div className={`flex items-center gap-2 ${className ?? ''}`}>
      {preview ? (
        <img
          src={preview}
          alt={t('images.preview')}
          title={t('images.preview')}
          className="rounded"
          style={{ maxWidth: 160, maxHeight: 160, border: '1px solid var(--border)' }}
        />
      ) : null}
      <span className="faint text-[11px]">
        {steps > 0
          ? t('images.progress', { step: formatNumber(step), steps: formatNumber(steps) })
          : t('images.progressUnknown')}
      </span>
    </div>
  )
}

function prettyJSON(raw: string): string {
  if (!raw) return '{}'
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function parseResult(raw: string): string {
  if (!raw) return '–'
  try {
    const parsed = JSON.parse(raw)
    if (parsed && typeof parsed === 'object' && typeof parsed.content === 'string') {
      return parsed.content
    }
    return JSON.stringify(parsed, null, 2)
  } catch {
    return raw
  }
}

/** One-line hint of what a tool was called with, for the collapsed state. */
function summariseArgs(raw: string): string {
  if (!raw) return ''
  try {
    const parsed = JSON.parse(raw)
    const parts: string[] = []
    for (const key of ['query', 'url', 'document', 'from', 'to']) {
      if (parsed[key] !== undefined) parts.push(`${key}=${String(parsed[key])}`)
    }
    return parts.join(' ')
  } catch {
    return raw.slice(0, 80)
  }
}

/* -------------------------------------------------------------------------
 * Reasoning
 * ---------------------------------------------------------------------- */

export function ReasoningBlock({ text, streaming }: { text: string; streaming: boolean }) {
  const t = useStore((s) => s.t)
  // Thinking arrives as fast as the answer does, and it is plain text: the
  // cost here is laying out a growing block, so it is committed on the same
  // ration and the memo below keeps it from being laid out twice.
  const shown = useThrottled(text, streaming, 120)
  if (!shown.trim()) return null
  return (
    <details className="collapsible mb-2" open={streaming}>
      <summary className="text-[12px] faint">{t('chat.reasoning')}</summary>
      <ReasoningText text={shown} />
    </details>
  )
}

const ReasoningText = memo(function ReasoningText({ text }: { text: string }) {
  return (
    <div
      className="mt-1 rounded p-2 text-[12px] whitespace-pre-wrap"
      style={{ background: 'var(--surface-2)', color: 'var(--text-dim)' }}
    >
      {text}
    </div>
  )
})

/* -------------------------------------------------------------------------
 * Compaction
 * ---------------------------------------------------------------------- */

export function CompactionCard({
  compaction,
  collapsed,
  onToggle,
  undoable,
}: {
  compaction: storeTypes.Compaction
  collapsed: boolean
  onToggle: () => void
  // Only the newest compaction of a chat may be undone. Compactions are
  // cumulative — each one summarises on top of the memory in force when it ran —
  // so undoing an older one would leave a newer summary quoting messages that are
  // back in the live context.
  undoable: boolean
}) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)
  const [undoing, setUndoing] = useState(false)
  const saved = Math.max(0, compaction.tokensBefore - compaction.tokensAfter)

  const undo = async () => {
    setUndoing(true)
    try {
      await App.DeleteCompaction(compaction.chatId, compaction.id)
      await useStore.getState().refreshChatState()
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setUndoing(false)
    }
  }

  return (
    <div
      className="my-3 rounded-lg px-3 py-2 text-[12px]"
      style={{
        background: 'color-mix(in srgb, var(--accent) 7%, transparent)',
        border: '1px dashed color-mix(in srgb, var(--accent) 40%, var(--border))',
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="badge badge-accent">
          {/* `too_large` is the fold nobody asked for: the backend refused the request
              for its own size and the history had to be folded to answer at all. It
              gets its own word rather than reading as a button somebody pressed. */}
          {compaction.trigger === 'auto'
            ? t('compact.auto')
            : compaction.trigger === 'too_large'
              ? t('compact.tooLarge')
              : t('compact.manual')}
        </span>
        <strong style={{ color: 'var(--text)' }}>{t('compact.title')}</strong>
        <span className="faint">
          #{compaction.fromSeq}–#{compaction.toSeq} · {formatNumber(compaction.tokensBefore)} →{' '}
          {formatNumber(compaction.tokensAfter)} {t('metrics.tokens')} ({formatNumber(saved)}{' '}
          {t('compact.saved')})
        </span>
        <button type="button" className="btn btn-sm btn-ghost ml-auto" onClick={onToggle}>
          {collapsed ? t('compact.originals') : t('compact.hideOriginals')}
        </button>
        <button type="button" className="btn btn-sm btn-ghost" onClick={() => setOpen((v) => !v)}>
          {open ? t('common.close') : t('compact.summary')}
        </button>
        {undoable && (
          <button type="button" className="btn btn-sm btn-ghost" disabled={undoing} onClick={() => void undo()}>
            {t('compact.undo')}
          </button>
        )}
      </div>
      {compaction.focus && (
        <div className="faint mt-1">
          {t('compact.focus')}: {compaction.focus}
        </div>
      )}
      {open && (
        <pre
          className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
          style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
        >
          {compaction.summary}
        </pre>
      )}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Attachment chips
 * ---------------------------------------------------------------------- */

export function AttachmentChips({
  attachments,
  compact = false,
}: {
  attachments: storeTypes.Attachment[]
  compact?: boolean
}) {
  const t = useStore((s) => s.t)
  // Pictures are rendered as pictures by MessageImages, so a chip for one would
  // be the same file listed twice — once as an image and once as a filename.
  const chips = attachments.filter((a) => !isImageAttachment(a))
  if (chips.length === 0) return null
  return (
    <div className="mb-2 flex flex-wrap gap-1.5">
      {chips.map((a) => (
        <span
          key={a.id}
          className="badge"
          title={a.warning || `${formatNumber(a.tokenEstimate)} ${t('files.tokens')}`}
        >
          <span>{fileIcon(a)}</span>
          <span className="truncate" style={{ maxWidth: compact ? 140 : 240 }}>
            {a.filename}
          </span>
          <span className="faint">
            {a.placement === 'indexed' ? t('files.indexed') : t('files.inline')} ·{' '}
            {formatNumber(a.tokenEstimate)}
          </span>
          {a.warning && <span style={{ color: 'var(--warn)' }}>⚠</span>}
        </span>
      ))}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Pictures
 * ---------------------------------------------------------------------- */

/**
 * Data URLs are fetched once per attachment and kept, most recent first.
 *
 * The payload is a whole picture — hundreds of kilobytes for a 1024x1024 PNG —
 * and the message tree re-renders on every delta of a streamed reply. Reading it
 * again each time would push that payload through the bridge for every keystroke,
 * so the promise is cached. The key is the attachment id, which never changes, so
 * an entry cannot go stale; a rejected promise is dropped rather than remembered,
 * because a failure that lasted one startup should not be permanent.
 *
 * It is bounded on purpose. A session that draws pictures keeps every payload it
 * ever showed, and a chat with a dozen 1024x1024 pictures held tens of megabytes
 * of base64 in the webview with the decoded bitmaps on top — memory the user pays
 * for and cannot see. The cap is well above what is on screen at once (the chat
 * view is virtualised, so only a window of messages is mounted), and the oldest
 * entry is the one that has been off screen longest: for a picture the user scrolls
 * back to, re-reading it through the bridge costs one call, which is the cheap half
 * of the trade. The Map iterates in insertion order, so refreshing an entry is a
 * delete-and-set rather than a bookkeeping structure of its own.
 */
const IMAGE_CACHE_LIMIT = 24
const imageCache = new Map<string, Promise<string>>()

function attachmentImage(id: string): Promise<string> {
  const known = imageCache.get(id)
  if (known) {
    // Re-inserted so it is the newest rather than the oldest: the picture the user is
    // looking at now must not be the one evicted by the next one they open.
    imageCache.delete(id)
    imageCache.set(id, known)
    return known
  }
  const entry = App.GetAttachmentImage(id).catch((err) => {
    imageCache.delete(id)
    throw err
  })
  imageCache.set(id, entry)
  while (imageCache.size > IMAGE_CACHE_LIMIT) {
    const oldest = imageCache.keys().next()
    if (oldest.done) break
    imageCache.delete(oldest.value)
  }
  return entry
}

/**
 * True for an attachment whose payload is a picture the webview can paint. The
 * test matches the backend's own: an image is one whose kind or MIME says so,
 * which is what makes a generated picture and an attached photo take the same
 * path through the UI.
 */
export function isImageAttachment(a: storeTypes.Attachment): boolean {
  return a.kind === 'image' || (a.mime ?? '').toLowerCase().startsWith('image/')
}

/**
 * Renders a message's pictures.
 *
 * Nothing here knows whether a picture was generated by a model or dropped into
 * the composer, and that is the point: the backend files both as an image
 * attachment, so both are shown, saved and (on a later turn) handed back to a
 * vision model by the same code.
 */
export function MessageImages({ attachments }: { attachments: storeTypes.Attachment[] }) {
  const images = attachments.filter(isImageAttachment)
  if (images.length === 0) return null
  return (
    <div className="mb-2 flex flex-wrap gap-2">
      {images.map((a) => (
        <MessageImage key={a.id} attachment={a} />
      ))}
    </div>
  )
}

/* The bounds the editor's number boxes carry, mirrored from internal/config
 * (MaxImageDimension, MaxImageSteps). There is no bridge for a constant, and the
 * settings screen's own inputs carry the same numbers by hand. They matter more here
 * than there: a stored recipe came from this app, but an edited one comes from a box
 * the user filled in, and a local server attempts a 100000-step request rather than
 * refusing it.
 */
const MAX_IMAGE_DIMENSION = 2048
const MAX_IMAGE_STEPS = 150
const MAX_IMAGE_CFG = 30

/** The recipe fields as the strings a form holds.
 *
 * Numbers live as text rather than as numbers so an emptied box can mean "the server's
 * own" — the backend reads a zero exactly that way. A `0` rendered into the box would
 * look like a value the user chose, and one they then could not clear. */
type RecipeDraft = {
  prompt: string
  negativePrompt: string
  width: string
  height: string
  steps: string
  cfgScale: string
  seed: string
  sampler: string
  model: string
}

function draftFromRecipe(recipe: storeTypes.GenerationRecipe): RecipeDraft {
  const num = (value: number | undefined) => (value ? String(value) : '')
  return {
    prompt: recipe.prompt ?? '',
    negativePrompt: recipe.negativePrompt ?? '',
    width: num(recipe.width),
    height: num(recipe.height),
    steps: num(recipe.steps),
    cfgScale: num(recipe.cfgScale),
    seed: num(recipe.seed),
    sampler: recipe.sampler ?? '',
    model: recipe.model ?? '',
  }
}

/** The inverse, with the same reading of an empty box the settings screen uses: text
 * that is not a number, or a number that is not positive, is "the server's own". The
 * bounds are applied here so the field's own max is honoured before the request leaves;
 * the backend clamps again, because a request can arrive from anywhere. */
function recipeFromDraft(draft: RecipeDraft): storeTypes.GenerationRecipe {
  const num = (value: string, max?: number) => {
    const n = Math.floor(Number(value))
    if (!Number.isFinite(n) || n <= 0) return 0
    return max === undefined ? n : Math.min(n, max)
  }
  // The guidance scale is the one fractional field: 7.5 is a scale a user may well want,
  // so it is parsed as a float rather than through `num` above, which floors. Zero still
  // means "nobody chose one" and is what an emptied box becomes.
  const scale = (value: string) => {
    const n = Number(value)
    if (!Number.isFinite(n) || n <= 0) return 0
    return Math.min(Math.max(n, 1), MAX_IMAGE_CFG)
  }
  return {
    prompt: draft.prompt.trim(),
    negativePrompt: draft.negativePrompt.trim(),
    width: num(draft.width, MAX_IMAGE_DIMENSION),
    height: num(draft.height, MAX_IMAGE_DIMENSION),
    steps: num(draft.steps, MAX_IMAGE_STEPS),
    cfgScale: scale(draft.cfgScale),
    seed: num(draft.seed),
    sampler: draft.sampler.trim(),
    model: draft.model.trim(),
  }
}

function MessageImage({ attachment }: { attachment: storeTypes.Attachment }) {
  const t = useStore((s) => s.t)
  const [src, setSrc] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // Click to see the whole picture. A thumbnail keeps a long chat readable, and
  // the enlarged view is what makes "is this the one I want?" answerable before
  // the file is saved.
  const [expanded, setExpanded] = useState(false)
  // Which draw is in flight, if any. Every control here spends GPU seconds, so they are
  // mutually disabled while one runs — otherwise a second click draws a second picture.
  const [busy, setBusy] = useState<'same' | 'new' | 'edit' | null>(null)
  // The editor is closed until the user opens it: a chat full of pictures would
  // otherwise be a chat full of forms.
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<RecipeDraft | null>(null)
  // Whether a draw in flight is one the user has asked to stop. The rejection that
  // follows is that cancellation rather than a failure, and this is what tells the two
  // apart: a red error toast for the user's own click reads as a crash, and the picture
  // list is unchanged either way. It is a ref rather than state because the draw's own
  // promise, not a re-render, is what reads it.
  const stopping = useRef(false)
  // And while the stop itself is in flight. The draw's own promise rejects the moment the
  // request is cancelled, but the server is interrupted a moment later — a *global*
  // interrupt of whatever it is drawing — so re-enabling the controls as soon as the draw
  // unwinds would let a second click start a picture the previous stop could then kill. It
  // is a window of milliseconds and it costs four lines to close.
  const [stoppingUi, setStoppingUi] = useState(false)
  // What the backend knows about a draw running under *this* picture. A redraw is registered
  // under the attachment it will replace, so this is the row's own draw and nothing else's.
  const live = useStore((s) => s.draws[attachment.id])

  // Only a picture KiChat drew carries a recipe. An uploaded photo has none, and
  // offering to redraw one would offer to draw something unrelated to it.
  const recipe = attachment.generation

  useEffect(() => {
    let cancelled = false
    setError(null)
    attachmentImage(attachment.id)
      .then((value) => {
        if (!cancelled) setSrc(value)
      })
      .catch((err) => {
        if (!cancelled) setError(String(err))
      })
    return () => {
      cancelled = true
    }
  }, [attachment.id])

  const save = async () => {
    setSaving(true)
    try {
      const path = await App.SaveAttachmentAs(attachment.id)
      // An empty path is the user closing the dialog. Reporting it as a success
      // or a failure would both be wrong, so it says nothing at all.
      if (path) useStore.getState().toast('info', `${t('images.saved')}: ${path}`)
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setSaving(false)
    }
  }

  /* Draws from a recipe, and the one place all three controls meet: a repeat passes the
   * stored recipe back, a variation passes it with the seed cleared, and Draw passes what
   * the editor holds. Nothing goes to the model — a recipe is the prompt, the size and the
   * seed, so the picture is drawn from data rather than from a request in words. What was
   * typed is what is drawn: a box left empty means "the server's own" rather than the
   * stored value, so the editor's promise is that it shows the whole recipe.
   *
   * The new picture is filed onto this same message, which is what puts it beside the
   * original; the backend's attachments event is what makes this component see it. */
  // One condition for every control that spends GPU seconds, so a state where some of them
  // are live cannot be reached by editing one of them alone.
  const blocked = busy !== null || stoppingUi

  const draw = async (next: storeTypes.GenerationRecipe, mode: 'same' | 'new' | 'edit') => {
    setBusy(mode)
    stopping.current = false
    try {
      const drawn = await App.RegenerateImage(attachment.id, next)
      useStore.getState().toast('info', `${t('images.redrawn')}: ${drawn.filename}`)
    } catch (err) {
      if (stopping.current) {
        useStore.getState().toast('info', t('images.drawStopped'))
      } else {
        useStore.getState().toast('error', String(err))
      }
    } finally {
      setBusy(null)
      stopping.current = false
    }
  }

  /* Ends a draw the user will not wait for. The backend does both halves: it cancels the
   * request, which is what makes this take effect at once, and asks the image server to
   * abandon the work behind it — a 40-step draw on a CPU server is minutes of fan noise
   * for a picture nobody wants any more.
   *
   * `wasDrawing` false means there was nothing to stop, which is a second click or a draw
   * that finished between the render and the click: the flag is cleared so the next
   * rejection is reported as the failure it is, and nothing is toasted for a stop that
   * did not happen. */
  const stop = async () => {
    stopping.current = true
    setStoppingUi(true)
    try {
      const result = await App.StopImageDraw(attachment.id)
      if (!result.wasDrawing) stopping.current = false
    } catch (err) {
      stopping.current = false
      useStore.getState().toast('error', String(err))
    } finally {
      setStoppingUi(false)
    }
  }

  /* Opens the editor prefilled with the picture's own recipe. Closing and reopening is
   * also how a half-finished edit is abandoned, which is why there is no reset button. */
  const openEditor = () => {
    if (!recipe) return
    setDraft(draftFromRecipe(recipe))
    setEditing(true)
  }

  if (error) {
    return (
      <span className="badge badge-danger" title={error}>
        {t('images.unreadable')}: {attachment.filename}
      </span>
    )
  }

  if (!src) {
    // A box of the final size rather than a spinner, so the message does not
    // jump when a large picture lands a moment later.
    return (
      <div
        className="flex items-center justify-center rounded"
        style={{
          width: expanded ? 384 : 200,
          height: expanded ? 384 : 200,
          background: 'var(--code-bg)',
          border: '1px solid var(--border)',
        }}
      >
        <span className="faint text-[11px]">{t('images.loading')}</span>
      </div>
    )
  }

  return (
    <figure className="m-0 flex flex-col gap-1">
      <img
        // While a redraw runs, this picture is the one on its way out — the new one replaces
        // it the moment the draw answers. Showing the draw instead of the old picture is what
        // makes a two-minute wait on a CPU server something other than a frozen window.
        src={live?.preview ?? src}
        alt={attachment.filename}
        className="rounded"
        style={{
          maxWidth: expanded ? '100%' : 240,
          maxHeight: expanded ? 560 : 240,
          border: '1px solid var(--border)',
          background: 'var(--code-bg)',
          cursor: 'zoom-in',
        }}
        title={live ? t('images.preview') : t('images.zoom')}
        onClick={() => setExpanded((v) => !v)}
      />
      {live && (
        <span className="faint text-[11px]" title={t('images.preview')}>
          {live.steps > 0
            ? t('images.progress', { step: formatNumber(live.step), steps: formatNumber(live.steps) })
            : t('images.progressUnknown')}
        </span>
      )}
      <figcaption className="flex flex-wrap items-center gap-2 text-[11px]">
        <span
          className="faint truncate"
          style={{ maxWidth: 200 }}
          title={recipe?.prompt || attachment.filename}
        >
          {attachment.filename}
        </span>
        {/* The seed is the number that reproduces a picture, and it is what tells two
            redraws of one prompt apart once both are on screen. */}
        {recipe?.seed ? (
          <span className="faint" title={t('images.seed')}>
            s{recipe.seed}
          </span>
        ) : null}
        <button type="button" className="btn btn-sm btn-ghost" disabled={saving} onClick={() => void save()}>
          {saving ? t('images.saving') : t('images.save')}
        </button>
        {recipe && (
          <>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.redrawHint')}
              onClick={() => void draw(recipe, 'same')}
            >
              {busy === 'same' ? t('images.redrawing') : t('images.redraw')}
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.variationHint')}
              onClick={() => void draw({ ...recipe, seed: 0 }, 'new')}
            >
              {busy === 'new' ? t('images.redrawing') : t('images.variation')}
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.editHint')}
              onClick={() => (editing ? setEditing(false) : openEditor())}
            >
              {editing ? t('images.editClose') : t('images.edit')}
            </button>
            {/* Stop appears beside the two buttons it stops, and only while one is
                running: a draw from the editor gets its Stop on the editor's own button
                instead, so there is never a Stop for a draw it cannot reach. */}
            {(busy === 'same' || busy === 'new') && (
              <button
                type="button"
                className="btn btn-sm btn-danger"
                title={t('images.stopHint')}
                onClick={() => void stop()}
              >
                ⏹ {t('images.stop')}
              </button>
            )}
          </>
        )}
      </figcaption>
      {/* The editor sits inside the figure, under the caption it belongs to: it is a form
          about *this* picture, and a variation or a Draw from it lands beside it. */}
      {editing && draft && (
        <RecipeEditor
          draft={draft}
          onChange={setDraft}
          drawing={busy === 'edit'}
          busy={blocked}
          onDraw={() => void draw(recipeFromDraft(draft), 'edit')}
          onStop={() => void stop()}
        />
      )}
    </figure>
  )
}

/**
 * The recipe editor: the whole recipe in boxes, prefilled from the picture's own, with
 * one Draw that sends what is in them.
 *
 * Every field is the *whole* of what will be drawn rather than a patch over the stored
 * recipe, because that is what the backend reads: a box emptied is a choice left to the
 * server, not a field quietly kept. It is also what makes this an iteration loop rather
 * than two buttons — change the prompt, draw, look, change it again.
 *
 * The checkpoint and the sampler are free text rather than dropdowns. The settings screen
 * can offer a list because it also asks the server what it has; here that would mean a
 * request per picture rendered, and a name that is no longer installed is refused by the
 * server with a message, which is a better answer than a stale list.
 */
function RecipeEditor({
  draft,
  onChange,
  drawing,
  busy,
  onDraw,
  onStop,
}: {
  draft: RecipeDraft
  onChange: (next: RecipeDraft) => void
  drawing: boolean
  busy: boolean
  onDraw: () => void
  onStop: () => void
}) {
  const t = useStore((s) => s.t)
  const set = (patch: Partial<RecipeDraft>) => onChange({ ...draft, ...patch })
  // A prompt-less generation is a blank canvas the server draws something arbitrary on,
  // and the backend refuses it. Disabling the button here is what turns that refusal into
  // a control the user can see rather than an error after the wait.
  const ready = draft.prompt.trim() !== ''

  return (
    <div
      className="mt-1 flex flex-col gap-2 rounded p-2 text-[11px]"
      style={{ border: '1px solid var(--border)', background: 'var(--code-bg)', maxWidth: 460 }}
    >
      <span className="faint">{t('images.editTitle')}</span>
      <label className="text-[11px]">
        {t('images.editPrompt')}
        <textarea
          className="input mt-1"
          rows={3}
          value={draft.prompt}
          onChange={(e) => set({ prompt: e.target.value })}
        />
      </label>
      <label className="text-[11px]">
        {t('settings.images.negative')}
        <input
          className="input mt-1"
          value={draft.negativePrompt}
          onChange={(e) => set({ negativePrompt: e.target.value })}
          placeholder={t('settings.images.auto')}
        />
      </label>
      <label className="text-[11px]">
        {t('settings.images.size')}
        <div className="mt-1 flex items-center gap-2">
          <input
            className="input mono"
            type="number"
            min={0}
            max={MAX_IMAGE_DIMENSION}
            step={8}
            value={draft.width}
            onChange={(e) => set({ width: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
          <span className="faint">×</span>
          <input
            className="input mono"
            type="number"
            min={0}
            max={MAX_IMAGE_DIMENSION}
            step={8}
            value={draft.height}
            onChange={(e) => set({ height: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </div>
      </label>
      <div className="grid grid-cols-3 gap-2">
        <label className="text-[11px]">
          {t('settings.images.steps')}
          <input
            className="input mono mt-1"
            type="number"
            min={0}
            max={MAX_IMAGE_STEPS}
            value={draft.steps}
            onChange={(e) => set({ steps: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]" title={t('settings.images.cfgHint')}>
          {t('settings.images.cfgScale')}
          <input
            className="input mono mt-1"
            type="number"
            min={0}
            max={MAX_IMAGE_CFG}
            step={0.5}
            value={draft.cfgScale}
            onChange={(e) => set({ cfgScale: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]" title={t('settings.images.defaults')}>
          {t('images.seed')}
          <input
            className="input mono mt-1"
            type="number"
            value={draft.seed}
            onChange={(e) => set({ seed: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
      </div>
      <div className="grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.images.model')}
          <input
            className="input mono mt-1"
            value={draft.model}
            onChange={(e) => set({ model: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]">
          {t('settings.images.sampler')}
          <input
            className="input mono mt-1"
            value={draft.sampler}
            onChange={(e) => set({ sampler: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {/* The button the click came from is the button that stops it: a 40-step draw on a
            CPU server is minutes, and sending the user to the far side of the picture for
            the Stop is the difference between a control that helps and one they hunt for. */}
        {drawing ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            title={t('images.stopHint')}
            onClick={onStop}
          >
            ⏹ {t('images.stop')}
          </button>
        ) : (
          <button
            type="button"
            className="btn btn-sm btn-primary"
            disabled={!ready || busy}
            title={ready ? t('images.editDrawHint') : t('images.editEmptyPrompt')}
            onClick={onDraw}
          >
            {t('images.editDraw')}
          </button>
        )}
        <span className="faint">{ready ? t('settings.images.defaults') : t('images.editEmptyPrompt')}</span>
      </div>
    </div>
  )
}

export function fileIcon(a: storeTypes.Attachment): string {
  switch (a.kind) {
    case 'pdf':
      return '📄'
    case 'image':
      return '🖼'
    case 'xlsx':
      return '📊'
    case 'pptx':
      return '📽'
    case 'docx':
      return '📝'
    case 'web':
      return '🌐'
    default:
      return '📃'
  }
}
