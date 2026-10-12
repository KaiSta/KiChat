import { memo, useEffect, useState } from 'react'
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
          {compaction.trigger === 'auto' ? t('compact.auto') : t('compact.manual')}
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
 * Data URLs are fetched once per attachment and kept for the session.
 *
 * The payload is a whole picture — hundreds of kilobytes for a 1024x1024 PNG —
 * and the message tree re-renders on every delta of a streamed reply. Reading it
 * again each time would push that payload through the bridge for every keystroke,
 * so the promise is cached. The key is the attachment id, which never changes, so
 * an entry cannot go stale; a rejected promise is dropped rather than remembered,
 * because a failure that lasted one startup should not be permanent.
 */
const imageCache = new Map<string, Promise<string>>()

function attachmentImage(id: string): Promise<string> {
  let entry = imageCache.get(id)
  if (!entry) {
    entry = App.GetAttachmentImage(id).catch((err) => {
      imageCache.delete(id)
      throw err
    })
    imageCache.set(id, entry)
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

function MessageImage({ attachment }: { attachment: storeTypes.Attachment }) {
  const t = useStore((s) => s.t)
  const [src, setSrc] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // Click to see the whole picture. A thumbnail keeps a long chat readable, and
  // the enlarged view is what makes "is this the one I want?" answerable before
  // the file is saved.
  const [expanded, setExpanded] = useState(false)
  // Which redraw is in flight, if any. Two buttons that both spend GPU seconds are
  // safe to press twice otherwise, and the second click would draw a second picture.
  const [redrawing, setRedrawing] = useState<'same' | 'new' | null>(null)

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

  /* Draws again from the stored recipe. Nothing goes to the model: the recipe is the
   * prompt, the settings and the seed, so the picture is redrawn from data rather
   * than from a request in words. The new picture is filed onto this same message,
   * which is what puts it beside the original — the backend's attachments event is
   * what makes this component see it. */
  const redraw = async (freshSeed: boolean) => {
    setRedrawing(freshSeed ? 'new' : 'same')
    try {
      const drawn = await App.RegenerateImage(attachment.id, freshSeed)
      useStore.getState().toast('info', `${t('images.redrawn')}: ${drawn.filename}`)
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setRedrawing(null)
    }
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
        src={src}
        alt={attachment.filename}
        className="rounded"
        style={{
          maxWidth: expanded ? '100%' : 240,
          maxHeight: expanded ? 560 : 240,
          border: '1px solid var(--border)',
          background: 'var(--code-bg)',
          cursor: 'zoom-in',
        }}
        title={t('images.zoom')}
        onClick={() => setExpanded((v) => !v)}
      />
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
              disabled={redrawing !== null}
              title={t('images.redrawHint')}
              onClick={() => void redraw(false)}
            >
              {redrawing === 'same' ? t('images.redrawing') : t('images.redraw')}
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={redrawing !== null}
              title={t('images.variationHint')}
              onClick={() => void redraw(true)}
            >
              {redrawing === 'new' ? t('images.redrawing') : t('images.variation')}
            </button>
          </>
        )}
      </figcaption>
    </figure>
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
