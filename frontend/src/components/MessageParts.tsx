import { useState } from 'react'
import { App, formatDuration, formatNumber, formatSpeed } from '../lib/api'
import type { store as storeTypes } from '../lib/api'
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
          <Row label="Backend" value={message.backend || '–'} />
          <Row label={t('metrics.promptTokens')} value={formatNumber(message.promptTokens)} />
          <Row label={t('metrics.completionTokens')} value={formatNumber(message.completionTokens)} />
          <Row label={t('metrics.stopReason')} value={message.stopReason || '–'} />
          <Row
            label={t('metrics.duration')}
            value={message.generationMs ? formatDuration(message.generationMs) : '–'}
          />
          <Row
            label="tok/s"
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
  if (!text.trim()) return null
  return (
    <details className="collapsible mb-2" open={streaming}>
      <summary className="text-[12px] faint">{t('chat.reasoning')}</summary>
      <div
        className="mt-1 rounded p-2 text-[12px] whitespace-pre-wrap"
        style={{ background: 'var(--surface-2)', color: 'var(--text-dim)' }}
      >
        {text}
      </div>
    </details>
  )
}

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
  if (attachments.length === 0) return null
  return (
    <div className="mb-2 flex flex-wrap gap-1.5">
      {attachments.map((a) => (
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
