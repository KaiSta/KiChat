import { useEffect, useMemo, useRef, useState } from 'react'
import {
  App,
  defaultManagedModel,
  folderName,
  formatBytes,
  formatNumber,
  formatPercentSafe,
  modelFolder,
  modelLabel,
  usageTone,
  type chat,
  type llm,
  type main,
  type runtime,
} from '../lib/api'
import { useStore } from '../store'

/* -------------------------------------------------------------------------
 * Context meter
 * ---------------------------------------------------------------------- */

export function ContextMeter({ onOpen }: { onOpen: () => void }) {
  const t = useStore((s) => s.t)
  const budget = useStore((s) => s.chatState?.budget)
  const phase = useStore((s) => (s.activeChatId ? s.phase[s.activeChatId] : undefined))

  // The number the next request has to fit under: the model's window, or a backend's own
  // smaller cap on one request (Groq), which makes a percentage of the window read as
  // free room that does not exist. Either number is a reading — a backend that publishes
  // a per-request budget and no window still bounds the request — so the meter waits for
  // both to be missing, not for the window alone.
  const cap = budget ? (budget.effectiveLimit > 0 ? budget.effectiveLimit : budget.limit) : 0

  if (!budget || cap <= 0) {
    return (
      <button type="button" className="btn btn-sm btn-ghost" onClick={onOpen} title={t('context.inspector')}>
        {t('context.title')}: {t('context.unknown')}
      </button>
    )
  }

  const tone = usageTone(budget.percent)
  const remeasuring = !budget.measuredValid && budget.measured > 0
  const ceilingBinds = budget.ceiling > 0 && budget.ceiling < budget.limit
  const ceilingNote = t('context.ceilingNote', {
    tokens: formatNumber(budget.ceiling),
    window: formatNumber(budget.limit),
  })

  return (
    <button
      type="button"
      onClick={onOpen}
      className="flex items-center gap-2 rounded-md px-2 py-1 text-[11px] transition-colors hover:bg-[var(--surface-2)]"
      title={
        budget.overLimit
          ? t('context.overLimit')
          : ceilingBinds
            ? `${formatNumber(budget.used)} / ${formatNumber(cap)} ${t('metrics.tokens')} — ${ceilingNote}`
            : `${formatNumber(budget.used)} / ${formatNumber(cap)} ${t('metrics.tokens')}`
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
        {formatNumber(budget.used)}/{formatNumber(cap)}
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
  const cap = budget.effectiveLimit > 0 ? budget.effectiveLimit : budget.limit
  const ceilingBinds = budget.ceiling > 0 && budget.ceiling < budget.limit

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
          {t('context.of')} {cap > 0 ? formatNumber(cap) : t('context.unknown')} {t('metrics.tokens')}
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

      {ceilingBinds && (
        <div className="mb-3 text-[11px]" style={{ color: 'var(--text-dim)' }}>
          {t('context.ceilingNote', {
            tokens: formatNumber(budget.ceiling),
            window: formatNumber(budget.limit),
          })}
        </div>
      )}

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
        {/* Where the per-request limit came from is part of believing it: a number
            learned from a refusal means a request was already lost to it, and one
            read off an ordinary answer means none was. */}
        {budget.ceiling > 0 && (
          <>
            <dt className="faint">{t('context.ceiling')}</dt>
            <dd className="mono">
              {formatNumber(budget.ceiling)}{' '}
              <span className="faint">({ceilingSourceLabel(budget.ceilingSource, t)})</span>
            </dd>
          </>
        )}
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

/**
 * The two places a per-request limit can come from, in the backend's own words.
 *
 * The distinction is not decoration: a ceiling learned from a refusal cost a request
 * that could not be sent, while one a response published cost nothing — and only the
 * second makes the first oversized turn of a session avoidable.
 */
function ceilingSourceLabel(source: string, t: (k: string) => string): string {
  switch (source) {
    case 'header':
      return t('context.ceilingFrom.header')
    case 'refusal':
      return t('context.ceilingFrom.refusal')
    case 'manual':
      return t('context.ceilingFrom.manual')
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
  const settings = useStore((s) => s.settings)

  const [open, setOpen] = useState(false)
  const [filter, setFilter] = useState('')
  const [nextOnly, setNextOnly] = useState(false)
  const [preview, setPreview] = useState<chat.ContextBudget | null>(null)
  const [candidate, setCandidate] = useState<llm.ModelInfo | null>(null)
  // The availability sweep: which backend is being asked, how far it has got, and
  // how many models the last one hid. Read through a bound method rather than
  // pushed, because a sweep of eighty models outlives any call it could be
  // attached to — the same reason the runtime panel polls a download.
  const [check, setCheck] = useState<main.ModelCheckStatus | null>(null)
  // The managed server's own status, read while the panel is open. It is what says
  // which file a *running* llama-server loaded and whether a launcher is installed —
  // the catalogue this panel lists cannot answer either question, because a stopped
  // managed server answers with nothing at all. That is the whole reason the local
  // section below starts a model rather than only offering one.
  const [runtimeStatus, setRuntimeStatus] = useState<main.RuntimeStatus | null>(null)
  const [statusError, setStatusError] = useState('')
  // The start that is being waited for, and the failure of one that is over. Per
  // backend, because more than one managed entry can be configured.
  const [localStart, setLocalStart] = useState<{ backend: string; path: string } | null>(null)
  // The stop that is in flight, by backend id: the button that asked for it says so and
  // cannot be pressed twice, because a second stop of a server that is on its way down
  // is a click with nothing left to do.
  const [localStop, setLocalStop] = useState('')
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})
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

  // Read the sweep state when the panel opens, so one that was started before it
  // was closed — or that finished while it was — is still shown on the way back in.
  useEffect(() => {
    if (!open) return
    let live = true
    App.GetModelCheckStatus()
      .then((st) => {
        if (live) setCheck(st)
      })
      .catch((err) => {
        if (live) setCheck({ busy: false, done: 0, total: 0, hidden: 0, hiddenTotal: 0, error: String(err) })
      })
    return () => {
      live = false
    }
  }, [open])

  // Poll only while a sweep is running. An idle picker asking for the status twice
  // a second for the rest of the session would be a request per second to learn
  // something that changed once.
  useEffect(() => {
    if (!open || !check?.busy) return
    const id = window.setInterval(() => {
      void App.GetModelCheckStatus()
        .then(async (st) => {
          setCheck(st)
          if (!st.busy) {
            // The verdict changed the list the backend builds, so re-read it
            // instead of leaving the hidden rows on screen.
            await refreshModels(false)
          }
        })
        .catch((err) => {
          // A failed poll is not worth a toast while the row can say it, but it
          // has to stop the interval rather than spin against a broken bridge.
          setCheck((prev) => (prev ? { ...prev, busy: false, error: String(err) } : prev))
        })
    }, 900)
    return () => window.clearInterval(id)
  }, [open, check?.busy, refreshModels])

  /**
   * Asks one backend's models whether this account may actually call them.
   *
   * It is what makes a vendor catalogue honest. build.nvidia.com lists eighty
   * models and answers 404 for the ones a given key is not entitled to — a fact
   * no metadata route reports, which is why the check costs one real request per
   * model and is a button rather than something the app does on its own.
   */
  const runCheck = async (backend: string) => {
    try {
      setCheck(await App.StartModelCheck(backend))
    } catch (err) {
      // A sweep that could not even start — an unknown backend, an unreachable
      // one, a listing that came back empty — has to be said out loud, because
      // nothing else in the panel would show it.
      useStore.getState().toast('error', String(err))
    }
  }

  const unhide = async () => {
    try {
      setCheck(await App.ClearHiddenModels(''))
      await refreshModels(false)
    } catch (err) {
      useStore.getState().toast('error', String(err))
    }
  }

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

  /* -----------------------------------------------------------------------
   * Starting a local model from here
   * -------------------------------------------------------------------------
   *
   * The managed llama.cpp server is reached through the same registry as every other
   * backend, and while it is stopped it answers with nothing: the list above has no row
   * for it. Starting one therefore used to mean leaving the conversation for Settings →
   * Backends, naming the file there, waiting for the load, and coming back to pick the
   * model — three screens for one intention, in the panel whose whole job is picking a
   * model. The section below is that intention, in the place it belongs.
   */

  /**
   * The entries KiChat runs itself, which are the only ones a model can be started on.
   * Every other kind is addressed over HTTP: there is no server to start and no folder
   * to list.
   */
  const localBackends = (settings?.backends ?? []).filter((b) => b.kind === 'llamacpp')

  /** The runtime's row for one entry, or undefined while the status has not answered. */
  const localStatus = (backendID: string) =>
    (runtimeStatus?.models ?? []).find((m) => m.backendId === backendID)

  // A row that is still loading, whoever started it. The poll below follows the status
  // rather than only the click this panel made: a load begun in Settings, or one that
  // was abandoned and picked back up, has to move here too — the state line it feeds is
  // what carries the button that can call it off, and a reading frozen at "loading …"
  // would offer to stop a server that came up minutes ago.
  const localStarting = (runtimeStatus?.models ?? []).some((m) => m.state === 'starting')

  useEffect(() => {
    if (!open) return
    let live = true
    const read = () =>
      App.GetRuntimeStatus()
        .then((st) => {
          if (!live) return
          setRuntimeStatus(st)
          setStatusError('')
        })
        .catch((err) => {
          // Said on the section instead of swallowed. Without an answer the rows would
          // read as "nothing is loaded", which is a statement about a server this app
          // could not see — and the start button they carry would be a guess.
          if (live) setStatusError(String(err))
        })
    void read()
    // Polled only while a load is being waited for. A load is minutes long and has no
    // event of its own to arrive on, while an idle picker asking twice a second for the
    // rest of the session would be a request per second to learn something that changed
    // when the user pressed a button.
    if (!localStart?.backend && !localStarting) return
    const id = window.setInterval(() => void read(), 1200)
    return () => {
      live = false
      window.clearInterval(id)
    }
  }, [open, localStart?.backend, localStarting])

  /**
   * Switches the chat to a model that has come up.
   *
   * It goes through the same path a click on a model in the list takes, including the
   * "next message only" checkbox: that is a choice the user already made about how a
   * switch is to be applied, and a start is not the moment to overrule it. The window
   * check is the same one as well — a switch into a smaller window than the conversation
   * uses fails on the next request rather than here — but a start that fits is committed
   * straight away (the user has already said which file they want by clicking it), and
   * one that does not is left as a decision with the numbers in front of it.
   */
  const finishLocalStart = async (backendID: string, path: string) => {
    try {
      await refreshModels(true)
      await refreshHealth()
      // Re-read the settings a second time, because the backend has by now written more
      // than the entry: when a managed server comes up, its address goes into that
      // backend's row and the row is switched on (onManagedState), so the store's copy
      // is stale again. Pressing Save in Settings from a stale copy would put the old
      // address back on a server that is answering at the new one.
      await useStore.getState().refreshSettings()
      const model = { backend: backendID, id: path } as llm.ModelInfo
      const chatId = chatState?.chat.id
      let budget: chat.ContextBudget | null = null
      if (chatId) {
        budget = await App.GetBudget(chatId, backendID, path).catch(() => null)
      }
      // overLimit is the server's own verdict, which is measured against whatever the
      // next request really has to fit under — the window, or a backend's smaller
      // per-request limit — so a start is not committed on a reading the numbers on
      // screen would contradict.
      if (budget && budget.overLimit) {
        setCandidate(model)
        setPreview(budget)
        return
      }
      await commit(model, false)
    } catch (err) {
      useStore.getState().toast('error', String(err))
    }
  }

  /**
   * What a start becomes.
   *
   * The chat is pointed at the model only once the server says it is up. A switch made
   * while the weights are still coming in would fail the next request, and a load takes
   * long enough that a silent switch would look like nothing happened. A failure lands
   * on the section that asked for it, where the file is still one click away.
   */
  useEffect(() => {
    if (!localStart) return
    const row = localStatus(localStart.backend)
    if (!row) return
    if (row.state === 'running') {
      const started = localStart
      setLocalStart(null)
      void finishLocalStart(started.backend, started.path)
      return
    }
    if (row.state === 'failed') {
      const failed = localStart
      setLocalStart(null)
      setLocalErrors((prev) => ({
        ...prev,
        [failed.backend]: row.error || t('model.localFailed'),
      }))
    }
  }, [runtimeStatus, localStart])

  /**
   * Loads one file into the managed server and remembers it as the entry's model.
   *
   * The entry is written through the same call Settings uses, so the picker's choice is
   * the stored choice: the panel and the chat cannot end up naming different files, and
   * an entry that is switched on starts this model on the next launch too.
   */
  const startLocal = async (backendID: string, path: string) => {
    setLocalErrors((prev) => ({ ...prev, [backendID]: '' }))
    const row = localStatus(backendID)
    if (row?.state === 'running' && row.loadedModel === path) {
      // Already in memory: this is a selection, not a start. Reloading a model that is
      // up would be minutes of loading for a file the server is already serving.
      await finishLocalStart(backendID, path)
      return
    }
    try {
      const stored = settings?.runtime?.models?.[backendID]
      // The entry's own values are kept and only the file moves: a start from the picker
      // must not quietly reset a context size or an offload choice made in Settings.
      // Defaults come from the one place that defines them for an entry that has none.
      await App.StartManagedModelWith(backendID, {
        ...defaultManagedModel(),
        ...stored,
        modelPath: path,
      })
      // The backend wrote the entry, so the store's copy of the settings is stale by
      // definition — and the Settings panel reads that copy, where its Save would write
      // the previous file back and undo the pick. The same reason the installer re-reads
      // when its download finishes behind a call that has already returned.
      await useStore.getState().refreshSettings()
      // Read once right away so the row says "loading" rather than keeping the file
      // looking idle until the first poll comes back.
      setRuntimeStatus(await App.GetRuntimeStatus())
      setLocalStart({ backend: backendID, path })
    } catch (err) {
      setLocalErrors((prev) => ({ ...prev, [backendID]: String(err) }))
    }
  }

  /**
   * Releases a model that is up, and calls off one that is still coming in.
   *
   * The mirror of `startLocal`, and it rests on the same argument: a managed server holds
   * the whole file in memory, and the panel that put it there by a click is the panel
   * where giving that memory back belongs — walking to Settings → Backends to stop what
   * the chat just started would be the same walk the start exists to save. It is also the
   * only way out of a load begun by mistake: a 27B file takes minutes to come in, and the
   * section that waits for it is the one that has to be able to call it off.
   *
   * Nothing is written. The entry keeps its file, so a model that was stopped is one
   * click from running again — the difference from `SetManagedModel`, where switching an
   * entry off is a decision about the next launch. What does change is the address
   * `onManagedState` wrote when the server came up: the store's copy of it is stale
   * either way, and a health probe now reports that row refused, which is what a stopped
   * server is.
   */
  const stopLocal = async (backendID: string) => {
    setLocalErrors((prev) => ({ ...prev, [backendID]: '' }))
    setLocalStop(backendID)
    try {
      await App.StopManagedModel(backendID)
      // A start this panel was waiting for is over and produced no server: leaving it
      // set would keep every row disabled and the header busy for a load that is gone.
      setLocalStart((prev) => (prev?.backend === backendID ? null : prev))
      // Read rather than assume. A failed start keeps saying "failed" — `markStopped`
      // does not downgrade it — and that is a fact the state line has to carry.
      try {
        setRuntimeStatus(await App.GetRuntimeStatus())
      } catch {
        // The section keeps the reading it has; opening it again reads a fresh one.
      }
      // The catalogue loses the row it had, because a stopped server answers with
      // nothing, and the health probe has to stop reporting the port it was on.
      void refreshModels(true)
      void refreshHealth()
      void useStore.getState().refreshSettings()
    } catch (err) {
      setLocalErrors((prev) => ({ ...prev, [backendID]: String(err) }))
    } finally {
      setLocalStop('')
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

      {/* The panel hangs from the button's LEFT edge, and that direction is not a
          matter of taste.

          This is the first control in the composer row, so a right-anchored panel
          grows leftwards from the button's right edge and crosses the window's left
          edge as soon as the button is narrow enough — and because the button is
          left-anchored in the row, widening the window never moved it. Measured
          with an NVIDIA model selected, whose button loses the window number (this
          backend reports no context window, so `contextLimit > 0` is false) and
          whose label is short: the button fell from 263px to 124px, the panel's
          left edge went to −68px, and it stayed at −68px from a 1440px viewport to
          a 2200px one.

          Left-anchored, the panel starts at the button's left edge and runs 30rem
          from there. That edge is fixed by the layout — a 17rem sidebar (`App.tsx`)
          plus the row's padding — so the panel spans roughly 288px to 768px and
          cannot follow the button's width into the window edge. It also clears the
          app's minimum window width of 900px (`main.go`), which is what makes the
          one-directional fix safe rather than a trade.

          The context inspector below stays anchored the other way on purpose: its
          trigger sits on the right of the row, where a left-anchored panel is what
          would leave the window. */}
      {open && (
        <div
          className="card absolute bottom-full left-0 z-30 mb-2 flex max-h-[28rem] w-[30rem] flex-col p-2 shadow-lg"
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
            {/* Before the catalogue, because it is the section that *has* something to
                offer when the catalogue is empty: a stopped managed server answers with
                no models at all, and "No models found. Is LM Studio or Ollama running?"
                was the whole answer on a machine with a folder full of GGUFs. */}
            {localBackends.map((b) => (
              <LocalModelStart
                key={b.id}
                backend={b}
                status={localStatus(b.id)}
                installed={!!runtimeStatus?.installed}
                statusError={statusError}
                busyPath={localStart?.backend === b.id ? localStart.path : ''}
                stopping={localStop === b.id}
                error={localErrors[b.id] ?? ''}
                onStart={(path) => void startLocal(b.id, path)}
                onStop={() => void stopLocal(b.id)}
              />
            ))}
            {grouped.length === 0 && <div className="p-3 text-center faint">{t('model.noModels')}</div>}
            {grouped.map(([backend, list]) => (
              <div key={backend} className="mb-2">
                <div className="flex items-center gap-2 px-1 py-1">
                  <span className="faint mono text-[11px] uppercase tracking-wide">{backend}</span>
                  {/* Per backend, because the answer is per backend: one
                      vendor's catalogue needs this and a local server's does
                      not, and a sweep costs one request per model. */}
                  <button
                    type="button"
                    className="btn btn-sm btn-ghost ml-auto text-[11px]"
                    disabled={!!check?.busy}
                    title={t('model.checkTitle')}
                    onClick={() => void runCheck(backend)}
                  >
                    {check?.busy && check.backend === backend
                      ? t('model.checkProgress', { done: check.done, total: check.total })
                      : t('model.check')}
                  </button>
                </div>
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

          {check?.error && (
            <div
              className="mt-2 rounded p-2 text-[11px]"
              style={{ background: 'var(--surface-2)', color: 'var(--warn)' }}
            >
              {/* The sweep's own verdict has a sentence per language; every other
                  failure is the backend's, and passing its text through is what
                  the rest of the app does with a Go error. The two are not
                  interchangeable: this row stays on screen, and a German
                  sentence in a Chinese window is a bug the user cannot dismiss. */}
              {check.reason === 'all-refused' ? t('model.checkRefusedAll') : check.error}
            </div>
          )}

          {(check?.hiddenTotal ?? 0) > 0 && (
            <div className="mt-2 flex items-center gap-2 text-[11px]" style={{ color: 'var(--text-dim)' }}>
              <span title={t('model.hiddenTitle')}>
                {t('model.hidden', { count: check?.hiddenTotal ?? 0 })}
              </span>
              <button type="button" className="btn btn-sm btn-ghost ml-auto" onClick={() => void unhide()}>
                {t('model.unhide')}
              </button>
            </div>
          )}

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
                    {formatNumber(preview.used)} /{' '}
                    {formatNumber(preview.effectiveLimit > 0 ? preview.effectiveLimit : preview.limit)}{' '}
                    {t('metrics.tokens')} ({formatPercentSafe(preview.percent)})
                  </div>
                  {preview.overLimit ? (
                    <div className="mt-2" style={{ color: 'var(--warn)' }}>
                      {t('model.switchWarning')}
                    </div>
                  ) : null}
                </>
              ) : (
                <div className="faint">{t('model.overrideHint')}</div>
              )}
              <div className="mt-2 flex flex-wrap gap-2">
                {preview && preview.overLimit ? (
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

/**
 * The managed server's own models, startable from the picker.
 *
 * Collapsed until it is wanted: the list above is what a chat is chosen from, and a folder
 * of dozens of GGUFs shown open would bury it. The header carries the two facts that
 * decide whether the click is worth making — which folder is in use and how many files are
 * in it — plus the current state while a load runs, so a user looking at this panel is not
 * left wondering whether their last click did anything.
 */
function LocalModelStart({
  backend,
  status,
  installed,
  statusError,
  busyPath,
  stopping,
  error,
  onStart,
  onStop,
}: {
  backend: llm.BackendConfig
  /** The runtime's row for this entry; undefined while the status has not answered. */
  status: main.ManagedStatus | undefined
  installed: boolean
  /** Why the runtime status could not be read, empty when there is no such failure. */
  statusError: string
  /** The file whose load is being waited for, empty when none is. */
  busyPath: string
  /** A stop of this entry is in flight. */
  stopping: boolean
  /** The last failed start on this entry, empty when there is none. */
  error: string
  onStart: (path: string) => void
  onStop: () => void
}) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)
  const [scan, setScan] = useState<runtime.ModelFolderScan | null>(null)
  const [scanning, setScanning] = useState(false)
  const [scanError, setScanError] = useState('')

  // Read when it is opened rather than when the panel is: a folder with hundreds of GGUFs
  // in it is a walk of the disk, and most visits to this panel are about choosing between
  // models that are already reachable. The empty folder asks the backend for the one
  // KiChat would use — the same folder Settings lists, and the one a start from here keeps
  // using.
  useEffect(() => {
    if (!open) return
    let live = true
    setScanning(true)
    App.ScanModelFolder('')
      .then((found) => {
        if (!live) return
        // `files` is read as an array the moment it arrives: a null one would blank the
        // view instead of reporting "no models here".
        setScan({ ...found, files: found?.files ?? [] })
        setScanError('')
      })
      .catch(() => {
        // "That folder is not there" is the useful half of this failure; which syscall
        // said so is not, and the Go error for it is not a sentence in three languages.
        if (!live) return
        setScan(null)
        setScanError(t('settings.managed.folderMissing'))
      })
      .finally(() => {
        if (live) setScanning(false)
      })
    return () => {
      live = false
    }
  }, [open])

  const files = scan?.files ?? []
  const loaded = status?.loadedModel ?? ''
  /** The three states the header and the state line share a word for. */
  const state = status?.state ?? 'stopped'

  return (
    <div className="mb-2">
      <button
        type="button"
        className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[12px] hover:bg-[var(--surface-2)]"
        title={t('model.localHint')}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="faint shrink-0">{open ? '▾' : '▸'}</span>
        <span className="shrink-0">{t('model.local')}</span>
        {/* The entry the files would be started on, since more than one managed entry
            can be configured and they need not share a build or a port. */}
        <span className="min-w-0 flex-1 truncate faint">{backend.name}</span>
        {scan?.dir && (
          <span className="faint shrink-0" title={scan.dir}>
            {folderName(scan.dir)}
          </span>
        )}
        {files.length > 0 && (
          <span className="faint shrink-0">
            {formatNumber(files.length)}{' '}
            {t(files.length === 1 ? 'settings.managed.folderModel' : 'settings.managed.folderModels')}
          </span>
        )}
        {/* Collapsed or open, the section says whether the server is up: a file list
            alone would read the same whether the model is in memory or not, and this
            row is the only place outside Settings that knows about the process at all. */}
        {state !== 'stopped' && (
          <span
            className={`badge shrink-0 ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}
            title={status?.error || undefined}
          >
            {t(`settings.managed.state.${state}`)}
          </span>
        )}
      </button>

      {open && (
        <div className="rounded p-1" style={{ background: 'var(--surface-2)' }}>
          {/* The state line, and the one button that changes it. A load can be called
              off and a model can be released without leaving the picker — the same
              argument the start above rests on: the section that began it is where it
              has to be undoable. `stopped` offers nothing here, because there is
              nothing left to stop. */}
          {state !== 'stopped' && (
            <div className="mb-1 flex items-center gap-2 px-2 py-1.5">
              <span
                className={`badge shrink-0 ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}
                title={status?.error || undefined}
              >
                {t(`settings.managed.state.${state}`)}
              </span>
              <span className="mono min-w-0 flex-1 truncate text-[11px] faint" title={loaded || undefined}>
                {loaded ? folderName(loaded) : ''}
              </span>
              <button
                type="button"
                className="btn btn-sm shrink-0"
                disabled={stopping}
                title={t('model.localStopHint')}
                onClick={onStop}
              >
                {stopping ? '…' : t('settings.managed.stop')}
              </button>
            </div>
          )}
          {statusError ? (
            <div className="px-1 py-1 text-[11px]" style={{ color: 'var(--danger)' }}>
              {statusError}
            </div>
          ) : !installed ? (
            <div className="px-1 py-1 text-[11px] faint">{t('settings.managed.noBinary')}</div>
          ) : scanning ? (
            <div className="px-1 py-1 text-[11px] faint">{t('settings.managed.folderScanning')}</div>
          ) : scanError ? (
            <div className="px-1 py-1 text-[11px] faint">{scanError}</div>
          ) : files.length === 0 ? (
            <div className="px-1 py-1 text-[11px] faint">
              {/* A folder that is known and empty and one that was never chosen are
                  different answers, and the first one is the common case on a machine
                  whose models live somewhere KiChat does not know about. */}
              {scan?.dir ? t('settings.managed.folderEmpty') : t('model.localNoFolder')}
            </div>
          ) : (
            <>
              {busyPath && <div className="px-1 py-1 text-[11px] faint">{t('model.localLoading')}</div>}
              {files.map((f) => (
                <button
                  key={f.path}
                  type="button"
                  // One at a time: a second start would stop the first load and begin
                  // another, which is minutes of the user's time spent twice.
                  disabled={!!busyPath}
                  className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[12px] hover:bg-[var(--surface-3)] disabled:opacity-60"
                  title={t('model.localRowHint')}
                  onClick={() => onStart(f.path)}
                >
                  <span className="min-w-0 flex-1 truncate mono" title={f.path}>
                    {f.name}
                  </span>
                  {busyPath === f.path && (
                    <span className="badge shrink-0">{t('settings.managed.state.starting')}</span>
                  )}
                  {busyPath !== f.path && f.path === loaded && (
                    <span className="badge badge-ok shrink-0">{t('settings.managed.folderLoaded')}</span>
                  )}
                  <span className="faint shrink-0">{formatBytes(f.size)}</span>
                </button>
              ))}
              {scan?.truncated && (
                <div className="px-1 py-1 text-[11px] faint">
                  {t('settings.managed.folderTruncated')}
                </div>
              )}
            </>
          )}
          {error && (
            <div className="px-1 py-1 text-[11px]" style={{ color: 'var(--danger)' }}>
              {error}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/**
 * Says that the model this chat is pinned to is not in the catalogue, and offers the
 * switch that a new chat would have made.
 *
 * A model can disappear from under a conversation without the user touching it: a
 * managed server is stopped, a backend is switched off, the availability check hides
 * a model the account cannot call, or the vendor withdraws one. The chat then keeps a
 * name nothing resolves — the switcher shows a red dot and the old name, and the next
 * Send fails on a model no backend serves. Nothing said so before the click, and the
 * switcher is not where a user looks for an explanation.
 *
 * So the notice names what happened and offers the one model worth one click: the one
 * Settings names for new chats, or — when that one is gone too — the same pick the
 * backend makes for a new chat, a loaded model first, then the first one listed
 * (app.defaultChatModel). The button carries the target's name, so this is a decision
 * the user reads before it happens rather than a silent repair, and a chat that is
 * already on a reachable model shows nothing at all.
 */
export function MissingModelNotice() {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const chat = useStore((s) => s.chatState?.chat)
  const settings = useStore((s) => s.settings)
  const running = useStore((s) => (s.activeChatId ? s.running[s.activeChatId] : false))
  const updateChat = useStore((s) => s.updateChat)
  const [switching, setSwitching] = useState(false)

  // Computed on every render rather than memoised. It is a scan of a list the picker
  // already holds, and a memo keyed on the chat object went stale once the object behind
  // it was mutated instead of replaced — the notice then named a model that had just
  // become reachable. Cheap and always current beats cached and sometimes wrong here.
  const target = (() => {
    // An empty catalogue is a listing that has not come back yet, not a verdict that
    // every model is gone — offering a switch from it would be a guess about a machine
    // that may have every model running.
    if (!chat || models.length === 0) return null
    if (models.some((m) => m.backend === chat.backend && m.id === chat.model)) return null
    const configured = settings?.defaultModel
    const preferred = configured?.model
      ? models.find((m) => m.backend === configured.backend && m.id === configured.model)
      : undefined
    return preferred ?? models.find((m) => m.state === 'loaded') ?? models[0]
  })()

  if (!chat || !target) return null

  const switchTo = async () => {
    setSwitching(true)
    try {
      // updateChat reports its own failure as a toast and refreshes the budget, and the
      // notice disappears on its own once the chat names a model that is listed.
      await updateChat({ backend: target.backend, model: target.id })
    } finally {
      setSwitching(false)
    }
  }

  return (
    <div
      className="mb-2 flex flex-wrap items-center gap-2 rounded p-2 text-[12px]"
      style={{
        background: 'color-mix(in srgb, var(--warn) 12%, transparent)',
        color: 'var(--warn)',
      }}
    >
      <span className="min-w-0 flex-1">
        {t('chat.missingModel', { model: modelLabel(chat.model), backend: chat.backend })}
      </span>
      <button
        type="button"
        className="btn btn-sm btn-primary shrink-0"
        // The engine is mid-turn with the model it already resolved; switching under it
        // would report a change this turn did not make. The switcher disables itself
        // while a turn runs for the same reason.
        disabled={running || switching}
        onClick={() => void switchTo()}
      >
        {switching
          ? '…'
          : t('chat.missingModelSwitch', { model: `${modelLabel(target.id)} (${target.backend})` })}
      </button>
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

