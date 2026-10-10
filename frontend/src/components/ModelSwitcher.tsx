import { useEffect, useMemo, useRef, useState } from 'react'
import {
  App,
  formatNumber,
  formatPercentSafe,
  modelFolder,
  modelLabel,
  usageTone,
  type chat,
  type llm,
} from '../lib/api'
import { useStore } from '../store'

/* -------------------------------------------------------------------------
 * Context meter
 * ---------------------------------------------------------------------- */

export function ContextMeter({ onOpen }: { onOpen: () => void }) {
  const t = useStore((s) => s.t)
  const budget = useStore((s) => s.chatState?.budget)
  const phase = useStore((s) => (s.activeChatId ? s.phase[s.activeChatId] : undefined))

  if (!budget || budget.limit <= 0) {
    return (
      <button type="button" className="btn btn-sm btn-ghost" onClick={onOpen} title={t('context.inspector')}>
        {t('context.title')}: {t('context.unknown')}
      </button>
    )
  }

  const tone = usageTone(budget.percent)
  const remeasuring = !budget.measuredValid && budget.measured > 0

  return (
    <button
      type="button"
      onClick={onOpen}
      className="flex items-center gap-2 rounded-md px-2 py-1 text-[11px] transition-colors hover:bg-[var(--surface-2)]"
      title={
        budget.overLimit
          ? t('context.overLimit')
          : `${formatNumber(budget.used)} / ${formatNumber(budget.limit)} ${t('metrics.tokens')}`
      }
    >
      <span className="relative block h-2 w-24 overflow-hidden rounded-full" style={{ background: 'var(--surface-3)' }}>
        <span
          className="absolute inset-y-0 left-0 rounded-full transition-[width] duration-300"
          style={{
            width: `${Math.min(100, budget.percent)}%`,
            background: `var(--${tone})`,
          }}
        />
        {/* The projected portion shows what the next request would add. */}
        {budget.projected > budget.used && (
          <span
            className="absolute inset-y-0 opacity-40"
            style={{
              left: `${Math.min(100, budget.percent)}%`,
              width: `${Math.min(100 - budget.percent, budget.projectedRatio * 100 - budget.percent)}%`,
              background: `var(--${usageTone(budget.projectedRatio * 100)})`,
            }}
          />
        )}
      </span>
      <span style={{ color: 'var(--text-dim)' }}>
        {formatNumber(budget.used)}/{formatNumber(budget.limit)}
      </span>
      <span style={{ color: `var(--${tone})` }}>{formatPercentSafe(budget.percent)}</span>
      {remeasuring && <span className="faint" title={t('context.remeasuring')}>~</span>}
      {budget.overLimit && <span className="badge badge-danger">!</span>}
      {phase === 'compacting' && <span className="badge badge-accent">{t('compact.running')}</span>}
    </button>
  )
}

/* -------------------------------------------------------------------------
 * Re-check
 * ---------------------------------------------------------------------- */

/**
 * Re-reads the context window from the backend on demand.
 *
 * The listing is cached, and the model switcher badge renders cached numbers, so
 * a model that was loaded — or reloaded with a different context length — since
 * the app last looked keeps reporting the old window. That is the difference
 * between the 128K a model's maximum allows and the 8k it was actually loaded
 * with. Forcing the listing refreshes the badge, and recomputing the chat state
 * refreshes the meter, which is derived from the same catalogue server-side.
 */
export function ContextRecheck() {
  const t = useStore((s) => s.t)
  const refreshModels = useStore((s) => s.refreshModels)
  const [checking, setChecking] = useState(false)

  const recheck = async () => {
    if (checking) return
    setChecking(true)
    try {
      // Order matters: the budget is computed from the catalogue, so refreshing
      // the chat state first would rebuild it from the stale listing.
      await refreshModels(true)
    } finally {
      await useStore.getState().refreshChatState()
      setChecking(false)
    }
  }

  return (
    <button
      type="button"
      className="btn btn-sm btn-ghost"
      disabled={checking}
      onClick={() => void recheck()}
      title={t('context.recheckTitle')}
    >
      {checking ? t('context.rechecking') : `↻ ${t('context.recheck')}`}
    </button>
  )
}

/* -------------------------------------------------------------------------
 * Context inspector
 * ---------------------------------------------------------------------- */

export function ContextInspector({ onClose }: { onClose: () => void }) {
  const t = useStore((s) => s.t)
  const budget = useStore((s) => s.chatState?.budget)
  const chatId = useStore((s) => s.activeChatId)
  // A running turn owns the newest messages, and the backend refuses to compact
  // while it does. Disabling the buttons says that before the click instead of
  // reporting it after.
  const running = useStore((s) => (s.activeChatId ? !!s.running[s.activeChatId] : false))
  const [compactOpen, setCompactOpen] = useState(false)
  const [focus, setFocus] = useState('')
  const [busy, setBusy] = useState(false)

  if (!budget) return null

  const blocked = busy || running

  const runCompact = async (target: number) => {
    if (!chatId) return
    setBusy(true)
    try {
      const outcome = await App.CompactNow({ chatId, focus, target, fromSeq: 0, toSeq: 0 })
      if (!outcome.created) {
        // Nothing was folded. That is an ordinary answer to a manual request
        // rather than a failure, so it is said plainly — and the panel stays
        // open, because closing it would look like it had worked.
        useStore.getState().toast('info', t('compact.noNeed'))
        return
      }
      onClose()
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="card absolute bottom-full right-0 z-30 mb-2 w-[26rem] p-3 shadow-lg" style={{ boxShadow: 'var(--shadow)' }}>
      <div className="mb-2 flex items-center justify-between">
        <strong>{t('context.title')}</strong>
        <button type="button" className="btn btn-sm btn-ghost" onClick={onClose}>
          {t('context.close')}
        </button>
      </div>

      <div className="mb-3 flex items-baseline gap-2">
        <span className="text-2xl font-semibold">{formatNumber(budget.used)}</span>
        <span className="muted">
          {t('context.of')} {budget.limit > 0 ? formatNumber(budget.limit) : t('context.unknown')}{' '}
          {t('metrics.tokens')}
        </span>
        <span className="ml-auto badge" title={budget.measuredValid ? t('metrics.measured') : t('metrics.estimated')}>
          {budget.measuredValid ? t('metrics.measured') : t('metrics.estimated')}
        </span>
      </div>

      <div className="mb-3 h-2 w-full overflow-hidden rounded-full" style={{ background: 'var(--surface-3)' }}>
        <div
          className="h-full transition-[width] duration-300"
          style={{ width: `${Math.min(100, budget.percent)}%`, background: `var(--${usageTone(budget.percent)})` }}
        />
      </div>

      <div className="mb-3 space-y-1 text-[12px]">
        {(budget.segments ?? []).map((seg, i) => (
          <div key={`${seg.kind}-${i}`} className="flex items-baseline gap-2">
            <span className="w-40 shrink-0" style={{ color: 'var(--text-dim)' }}>
              {seg.label}
            </span>
            <span className="mono w-16 shrink-0 text-right">{formatNumber(seg.tokens)}</span>
            <span className="faint truncate" title={seg.detail}>
              {seg.detail}
            </span>
          </div>
        ))}
      </div>

      <dl className="mb-3 grid grid-cols-2 gap-x-3 text-[11px]">
        <dt className="faint">{t('context.limitSource')}</dt>
        <dd>{limitSourceLabel(budget.limitSource, t)}</dd>
        <dt className="faint">{t('context.reserve')}</dt>
        <dd className="mono">{formatNumber(budget.reserve)}</dd>
        <dt className="faint">{t('context.projected')}</dt>
        <dd className="mono" style={{ color: budget.overLimit ? 'var(--danger)' : undefined }}>
          {formatNumber(budget.projected)} ({formatPercentSafe(budget.projectedRatio * 100)})
        </dd>
        {budget.deltaSince > 0 && (
          <>
            <dt className="faint">{t('context.deltaSince')}</dt>
            <dd className="mono">+{formatNumber(budget.deltaSince)}</dd>
          </>
        )}
      </dl>

      {budget.overLimit && (
        <div className="mb-2 rounded p-2 text-[12px]" style={{ background: 'color-mix(in srgb, var(--danger) 14%, transparent)', color: 'var(--danger)' }}>
          {t('context.overLimit')}
        </div>
      )}

      {!compactOpen ? (
        <div className="space-y-2">
          {/* The action buttons inside the form are disabled while a turn runs;
              saying so here means the reason is visible before the click that
              opens the form, not only after it. */}
          {running && (
            <div className="text-[12px]" style={{ color: 'var(--warn)' }}>
              {t('compact.busy')}
            </div>
          )}
          <div className="flex gap-2">
            <button
              type="button"
              className="btn btn-sm btn-primary"
              disabled={blocked}
              onClick={() => setCompactOpen(true)}
            >
              {t('context.compactNow')}
            </button>
          {budget.overLimit && (
            <button
              type="button"
              className="btn btn-sm"
              disabled={blocked}
              onClick={() => void runCompact(0.6)}
            >
              {t('context.compactToFit')}
            </button>
          )}
          </div>
        </div>
      ) : (
        <div className="space-y-2">
          <label className="block text-[12px]">
            <span className="faint">{t('compact.focus')}</span>
            <input
              className="input mt-1"
              value={focus}
              onChange={(e) => setFocus(e.target.value)}
              placeholder={t('compact.focusHint')}
            />
          </label>
          {running && (
            <div className="text-[12px]" style={{ color: 'var(--warn)' }}>
              {t('compact.busy')}
            </div>
          )}
          <div className="flex flex-wrap gap-2">
            <button type="button" className="btn btn-sm btn-primary" disabled={blocked} onClick={() => void runCompact(0)}>
              {busy ? t('compact.running') : t('compact.run')}
            </button>
            <button type="button" className="btn btn-sm" disabled={blocked} onClick={() => void runCompact(0.6)}>
              → 60%
            </button>
            <button type="button" className="btn btn-sm" disabled={blocked} onClick={() => void runCompact(0.4)}>
              → 40%
            </button>
            <button type="button" className="btn btn-sm btn-ghost" onClick={() => setCompactOpen(false)}>
              {t('common.cancel')}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

function limitSourceLabel(source: string, t: (k: string) => string): string {
  switch (source) {
    case 'backend':
      return 'Backend'
    case 'override':
      return t('model.overrides')
    default:
      return t('context.unknown')
  }
}

/* -------------------------------------------------------------------------
 * Model switcher
 * ---------------------------------------------------------------------- */

export function ModelSwitcher() {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const modelsLoading = useStore((s) => s.modelsLoading)
  const chatState = useStore((s) => s.chatState)
  const running = useStore((s) => (s.activeChatId ? s.running[s.activeChatId] : false))
  const refreshModels = useStore((s) => s.refreshModels)
  const updateChat = useStore((s) => s.updateChat)
  const refreshHealth = useStore((s) => s.refreshHealth)
  const setNextModel = useStore((s) => s.setNextModel)

  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState('')
  const [nextOnly, setNextOnly] = useState(false)
  const [preview, setPreview] = useState<chat.ContextBudget | null>(null)
  const [candidate, setCandidate] = useState<llm.ModelInfo | null>(null)
  const boxRef = useRef<HTMLDivElement>(null)

  const current = useMemo(
    () => models.find((m) => m.backend === chatState?.chat.backend && m.id === chatState?.chat.model),
    [models, chatState],
  )

  // Close on outside click so the switcher behaves like a real dropdown.
  useEffect(() => {
    if (!open) return
    const handler = (event: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(event.target as Node)) {
        setOpen(false)
        setCandidate(null)
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  useEffect(() => {
    if (open) void refreshModels(false)
  }, [open, refreshModels])

  const filtered = useMemo(() => {
    const needle = filter.trim().toLowerCase()
    const list = needle
      ? models.filter(
          (m) =>
            m.id.toLowerCase().includes(needle) ||
            m.backend.toLowerCase().includes(needle) ||
            (m.publisher ?? '').toLowerCase().includes(needle),
        )
      : models
    return [...list].sort((a, b) => {
      // Loaded models first, then by backend and name.
      if (a.state !== b.state) return a.state === 'loaded' ? -1 : 1
      if (a.backend !== b.backend) return a.backend.localeCompare(b.backend)
      return a.id.localeCompare(b.id)
    })
  }, [models, filter])

  const grouped = useMemo(() => {
    const map = new Map<string, llm.ModelInfo[]>()
    for (const m of filtered) {
      const list = map.get(m.backend) ?? []
      list.push(m)
      map.set(m.backend, list)
    }
    return [...map.entries()]
  }, [filtered])

  /**
   * How often a model name repeats, per backend. A file name alone can collide —
   * two publishers ship the same quantization — and the picker is where that has to
   * stay visible, so a repeated name is shown with its folder. Counted over every
   * model rather than the filtered list, so the labels do not shift while a user
   * types.
   */
  const repeats = useMemo(() => {
    const counts = new Map<string, number>()
    for (const m of models) {
      const key = `${m.backend}/${modelLabel(m.id)}`
      counts.set(key, (counts.get(key) ?? 0) + 1)
    }
    return counts
  }, [models])

  /** The model's name, without the path it happens to live at. */
  const labelFor = (m: llm.ModelInfo) => modelLabel(m.id)

  /**
   * What to add when two models in one backend share a file name, and nothing when
   * they do not — two publishers shipping the same quantization is the ordinary
   * case, and a list of two identical rows is what has to be avoided. The publisher
   * is short and is what a user recognises, and a model without one (a GGUF from the
   * managed runtime, keyed by its path) falls back to the folder it sits in.
   *
   * Measured in the harness: the *folder* is the wrong segment for this — two
   * publishers both call theirs `Ornith-1.5-9B-GGUF`, so qualifying by it produced
   * two identical labels again.
   */
  const qualifierFor = (m: llm.ModelInfo) => {
    if ((repeats.get(`${m.backend}/${modelLabel(m.id)}`) ?? 0) < 2) return ''
    return m.publisher || modelFolder(m.id)
  }

  /**
   * Selecting a model checks the conversation against that model's window first.
   * Switching to a model with a smaller context would otherwise fail on the next
   * request with an opaque backend error.
   */
  const choose = async (model: llm.ModelInfo) => {
    const chatId = chatState?.chat.id
    if (!chatId) return
    setCandidate(model)
    try {
      const budget = await App.GetBudget(chatId, model.backend, model.id)
      setPreview(budget)
    } catch {
      setPreview(null)
    }
  }

  const commit = async (model: llm.ModelInfo, compactFirst: boolean) => {
    const chatId = chatState?.chat.id
    if (!chatId) return
    try {
      if (nextOnly) {
        setNextModel({ backend: model.backend, model: model.id })
      } else {
        await updateChat({ backend: model.backend, model: model.id })
      }
      if (compactFirst) {
        // Target roughly 60% of the new window so there is room to work.
        await App.CompactNow({ chatId, focus: '', target: 0.6, fromSeq: 0, toSeq: 0 })
      }
      setOpen(false)
      setCandidate(null)
      setPreview(null)
      setNextOnly(false)
    } catch (err) {
      useStore.getState().toast('error', String(err))
    }
  }

  // The id is what the app stores and what the tooltip shows; the button reads as
  // the model's name, because that is what the conversation is with.
  const fullId = current?.id ?? chatState?.chat.model ?? ''
  const label = current ? labelFor(current) : modelLabel(fullId) || t('model.switch')

  return (
    <div className="relative" ref={boxRef}>
      <button
        type="button"
        className="btn btn-sm"
        onClick={() => setOpen((v) => !v)}
        title={t('model.switch')}
        disabled={running}
      >
        <span
          className="inline-block h-2 w-2 rounded-full"
          style={{
            background:
              current?.state === 'loaded' ? 'var(--ok)' : current ? 'var(--text-faint)' : 'var(--danger)',
          }}
        />
        <span className="max-w-[16rem] truncate mono" title={fullId || undefined}>
          {label}
        </span>
        {current && qualifierFor(current) && <span className="faint">{qualifierFor(current)}</span>}
        {current && current.contextLimit > 0 && (
          <span className="faint">{formatNumber(current.contextLimit)}</span>
        )}
        <span className="faint">▾</span>
      </button>

      {open && (
        <div
          className="card absolute bottom-full right-0 z-30 mb-2 flex max-h-[28rem] w-[30rem] flex-col p-2 shadow-lg"
          style={{ boxShadow: 'var(--shadow)' }}
        >
          <div className="mb-2 flex gap-2">
            <input
              autoFocus
              className="input"
              placeholder={t('model.search')}
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => {
                void refreshModels(true)
                void refreshHealth()
              }}
            >
              {modelsLoading ? '…' : t('settings.refresh')}
            </button>
          </div>

          <label className="mb-2 flex items-center gap-2 text-[12px] muted">
            <input type="checkbox" checked={nextOnly} onChange={(e) => setNextOnly(e.target.checked)} />
            {t('model.nextOnly')}
          </label>

          <div className="min-h-0 flex-1 overflow-y-auto scroll-thin">
            {grouped.length === 0 && <div className="p-3 text-center faint">{t('model.noModels')}</div>}
            {grouped.map(([backend, list]) => (
              <div key={backend} className="mb-2">
                <div className="faint mono px-1 py-1 text-[11px] uppercase tracking-wide">{backend}</div>
                {list.map((m) => (
                  <ModelRow
                    key={`${m.backend}/${m.id}`}
                    model={m}
                    label={labelFor(m)}
                    qualifier={qualifierFor(m)}
                    active={m.backend === chatState?.chat.backend && m.id === chatState?.chat.model}
                    onClick={() => void choose(m)}
                  />
                ))}
              </div>
            ))}
          </div>

          {candidate && (
            <div className="mt-2 rounded p-2 text-[12px]" style={{ background: 'var(--surface-2)' }}>
              <div className="mb-1 flex items-baseline gap-2" title={candidate.id}>
                <span className="font-semibold mono truncate">{labelFor(candidate)}</span>
                {qualifierFor(candidate) && (
                  <span className="faint shrink-0">{qualifierFor(candidate)}</span>
                )}
              </div>
              {preview && preview.limit > 0 ? (
                <>
                  <div className="muted">
                    {formatNumber(preview.used)} / {formatNumber(preview.limit)} {t('metrics.tokens')} (
                    {formatPercentSafe(preview.percent)})
                  </div>
                  {preview.projected > preview.limit ? (
                    <div className="mt-2" style={{ color: 'var(--warn)' }}>
                      {t('model.switchWarning')}
                    </div>
                  ) : null}
                </>
              ) : (
                <div className="faint">{t('model.overrideHint')}</div>
              )}
              <div className="mt-2 flex flex-wrap gap-2">
                {preview && preview.projected > preview.limit ? (
                  <button type="button" className="btn btn-sm btn-primary" onClick={() => void commit(candidate, true)}>
                    {t('model.switchAndCompact')}
                  </button>
                ) : (
                  <button type="button" className="btn btn-sm btn-primary" onClick={() => void commit(candidate, false)}>
                    {t('model.switch')}
                  </button>
                )}
                <button
                  type="button"
                  className="btn btn-sm btn-ghost"
                  onClick={() => {
                    setCandidate(null)
                    setPreview(null)
                  }}
                >
                  {t('common.cancel')}
                </button>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function ModelRow({
  model,
  label,
  qualifier,
  active,
  onClick,
}: {
  model: llm.ModelInfo
  /** The model's name, without its path; the full id is the tooltip. */
  label: string
  /** Shown beside the name when two models share it, e.g. the publisher. */
  qualifier: string
  active: boolean
  onClick: () => void
}) {
  const t = useStore((s) => s.t)
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[12px] hover:bg-[var(--surface-2)]"
      style={active ? { background: 'color-mix(in srgb, var(--accent) 14%, transparent)' } : undefined}
    >
      <span
        className="inline-block h-2 w-2 shrink-0 rounded-full"
        style={{ background: model.state === 'loaded' ? 'var(--ok)' : 'var(--text-faint)' }}
        title={model.state === 'loaded' ? t('model.loaded') : t('model.notLoaded')}
      />
      <span className="min-w-0 flex-1 truncate mono" title={model.id}>
        {label}
      </span>
      {qualifier && <span className="faint shrink-0">{qualifier}</span>}
      {model.quantization && <span className="faint shrink-0">{model.quantization}</span>}
      {model.contextLimit > 0 && (
        <span className="faint shrink-0" title={t('model.context')}>
          {formatNumber(model.contextLimit)}
        </span>
      )}
      {(model.capabilities ?? []).includes('tool_use') && (
        <span className="badge badge-accent shrink-0">{t('model.tools')}</span>
      )}
      {(model.capabilities ?? []).includes('vision') && (
        <span className="badge shrink-0">{t('model.vision')}</span>
      )}
    </button>
  )
}

