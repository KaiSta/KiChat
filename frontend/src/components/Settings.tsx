import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  App,
  formatBytes,
  formatNumber,
  modelLabel,
  timeAgo,
  toDate,
  type config,
  type llm,
  type main,
  type runtime,
} from '../lib/api'
import { useStore } from '../store'
import { asLang, LANGS } from '../lib/i18n'

type Tab = 'general' | 'backends' | 'images' | 'prompt' | 'research' | 'context'

/**
 * Translate a key, falling back to text the backend supplied. translate() echoes
 * the key back when it has no entry, and that echo is the signal: a preset or a
 * template variable the backend knows but this build of the dictionary does not
 * still reads as something rather than as "preset.my-id".
 */
function translatedOr(t: (key: string) => string, key: string, fallback: string): string {
  const translated = t(key)
  return translated === key ? fallback : translated
}

export function Settings() {
  const t = useStore((s) => s.t)
  const loaded = useStore((s) => s.settings)
  const [tab, setTab] = useState<Tab>('general')
  const [draft, setDraft] = useState<config.Settings | null>(loaded)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    setDraft(loaded)
  }, [loaded])

  if (!draft) return <div className="flex flex-1 items-center justify-center faint">{t('common.loading')}</div>

  const patch = (changes: Partial<config.Settings>) => setDraft({ ...draft, ...changes })

  // A row's own "turn on and check" button changes the draft and applies it in the
  // same click, and setState has not landed by the time this runs, so the payload
  // can be handed in rather than always read from state.
  const save = async (next?: config.Settings) => {
    const payload = next ?? draft
    setSaving(true)
    try {
      // Adopt what the backend stored rather than the draft: the untouched
      // built-in prompts are rewritten into the language being saved, so the
      // draft and the stored settings would otherwise disagree until a restart.
      const stored = await App.SaveSettings(payload)
      setDraft(stored)
      useStore.setState({ settings: stored })
      useStore.getState().toast('info', t('settings.saved'))
      await useStore.getState().refreshModels(true)
      await useStore.getState().refreshHealth()
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setSaving(false)
    }
  }

  const tabs: { id: Tab; label: string }[] = [
    { id: 'general', label: t('settings.general') },
    { id: 'backends', label: t('settings.backends') },
    // Next to the chat backends, because it is the other server a user has to run
    // themselves — but a tab of its own, because the setup does not fit beside one.
    { id: 'images', label: t('settings.images') },
    { id: 'prompt', label: t('settings.prompt') },
    { id: 'research', label: t('settings.research') },
    { id: 'context', label: t('settings.compaction') },
  ]

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center gap-2 border-b px-4 py-2" style={{ borderColor: 'var(--border)' }}>
        <strong>{t('settings.title')}</strong>
        <div className="ml-4 flex gap-1">
          {tabs.map((item) => (
            <button
              key={item.id}
              type="button"
              className={`btn btn-sm ${tab === item.id ? 'btn-primary' : 'btn-ghost'}`}
              onClick={() => setTab(item.id)}
            >
              {item.label}
            </button>
          ))}
        </div>
        <button type="button" className="btn btn-sm btn-primary ml-auto" disabled={saving} onClick={() => void save()}>
          {saving ? '…' : t('settings.save')}
        </button>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto scroll-thin p-4">
        <div className="mx-auto max-w-3xl space-y-4">
          {tab === 'general' && <GeneralTab draft={draft} patch={patch} />}
          {tab === 'backends' && <BackendsTab draft={draft} patch={patch} onApply={save} />}
          {tab === 'images' && <ImagesTab draft={draft} patch={patch} onApply={save} />}
          {tab === 'prompt' && <PromptTab draft={draft} patch={patch} />}
          {tab === 'research' && <ResearchTab draft={draft} patch={patch} />}
          {tab === 'context' && <ContextTab draft={draft} patch={patch} />}
        </div>
      </div>
    </div>
  )
}

function Section({ title, children, hint }: { title: string; children: ReactNode; hint?: string }) {
  return (
    <section className="card p-4">
      <h2 className="mb-1 text-[14px] font-semibold">{title}</h2>
      {hint && <p className="faint mb-3 text-[12px]">{hint}</p>}
      <div className={hint ? '' : 'mt-3'}>{children}</div>
    </section>
  )
}

function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="mb-3 block">
      <span className="mb-1 block text-[12px]" style={{ color: 'var(--text-dim)' }}>
        {label}
      </span>
      {children}
      {hint && <span className="faint mt-1 block text-[11px]">{hint}</span>}
    </label>
  )
}

/* -------------------------------------------------------------------------
 * General
 * ---------------------------------------------------------------------- */

function GeneralTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const setLang = useStore((s) => s.setLang)
  const theme = useStore((s) => s.theme)
  const setTheme = useStore((s) => s.setTheme)
  const [info, setInfo] = useState<main.AppInfo | null>(null)

  useEffect(() => {
    void App.GetAppInfo().then(setInfo).catch(() => undefined)
  }, [])

  return (
    <>
      <Section title={t('settings.general')}>
        {/* The list comes from LANGS so a language added there appears here
            without touching this screen, and so the stored value stays typed. */}
        <Field label={t('settings.language')} hint={t('settings.languageHint')}>
          <select
            className="select"
            value={asLang(draft.language)}
            onChange={(e) => {
              const next = asLang(e.target.value)
              patch({ language: next })
              setLang(next)
            }}
          >
            {LANGS.map((l) => (
              // The endonym: a language is listed in its own script so it is
              // legible to the person looking for it.
              <option key={l.id} value={l.id}>
                {l.label}
              </option>
            ))}
          </select>
        </Field>
        <Field label={t('settings.theme')}>
          <select
            className="select"
            value={theme}
            onChange={(e) => {
              const value = e.target.value as 'dark' | 'light'
              patch({ theme: value })
              setTheme(value)
            }}
          >
            <option value="dark">{t('settings.themeDark')}</option>
            <option value="light">{t('settings.themeLight')}</option>
          </select>
        </Field>
        <Field label={t('settings.location')}>
          <input className="input" value={draft.location} onChange={(e) => patch({ location: e.target.value })} />
        </Field>
        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={draft.autoTitle}
            onChange={(e) => patch({ autoTitle: e.target.checked })}
          />
          {t('settings.autoTitle')}
        </label>
      </Section>

      <Section title={t('settings.sampler')}>
        <div className="grid grid-cols-2 gap-3">
          <Field label={t('settings.temperature')}>
            <input
              className="input"
              type="number"
              step="0.05"
              min="0"
              max="2"
              value={draft.sampler.temperature}
              onChange={(e) => patch({ sampler: { ...draft.sampler, temperature: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.topP')}>
            <input
              className="input"
              type="number"
              step="0.05"
              min="0"
              max="1"
              value={draft.sampler.topP}
              onChange={(e) => patch({ sampler: { ...draft.sampler, topP: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.numCtx')} hint={t('settings.numCtxHint')}>
            <input
              className="input"
              type="number"
              min="0"
              value={draft.sampler.numCtx}
              onChange={(e) => patch({ sampler: { ...draft.sampler, numCtx: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.maxTokens')}>
            <input
              className="input"
              type="number"
              min="0"
              value={draft.sampler.maxTokens}
              onChange={(e) => patch({ sampler: { ...draft.sampler, maxTokens: Number(e.target.value) } })}
            />
          </Field>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <Field label={t('settings.toolIterations')}>
            <input
              className="input"
              type="number"
              min="1"
              max="20"
              value={draft.toolIterations}
              onChange={(e) => patch({ toolIterations: Number(e.target.value) })}
            />
          </Field>
          <Field label={t('settings.nativeTools')} hint={t('settings.nativeToolsHint')}>
            <label className="flex items-center gap-2 pt-1 text-[13px]">
              <input
                type="checkbox"
                checked={draft.nativeTools}
                onChange={(e) => patch({ nativeTools: e.target.checked })}
              />
              {draft.nativeTools ? t('common.yes') : t('common.no')}
            </label>
          </Field>
        </div>
      </Section>

      <AboutSection info={info} />
    </>
  )
}

/* -------------------------------------------------------------------------
 * About
 * ---------------------------------------------------------------------- */

/**
 * The build identity: which version is running, and enough provenance to quote
 * in a bug report. The copy button exists because the usual way to answer "what
 * version are you on?" is a screenshot — this makes it one click of text.
 */
function AboutSection({ info }: { info: main.AppInfo | null }) {
  const t = useStore((s) => s.t)
  const [copied, setCopied] = useState(false)
  // The row only appears once the backend has answered, so a failed bridge
  // shows nothing rather than a row of blanks.
  if (!info) return null

  // The commit is what makes the version actionable, so it travels with it. A
  // build with no commit (the package defaults) shows the bare version.
  const version = info.commit && info.commit !== 'none' ? `${info.version} (${info.commit})` : info.version
  const built = toDate(info.date)?.toLocaleString() ?? info.date
  const rows: { label: string; value: string }[] = [
    { label: t('settings.version'), value: version },
    { label: t('settings.buildDate'), value: built },
    { label: t('settings.platform'), value: info.platform },
    { label: t('settings.runtime'), value: info.goVersion },
  ]

  return (
    <Section title={t('settings.about')}>
      <div className="space-y-1 text-[12px]">
        {rows.map((row) => (
          <div key={row.label} className="flex items-center gap-3">
            <span className="faint w-24 shrink-0">{row.label}</span>
            <span className="mono min-w-0 flex-1 truncate" title={row.value}>
              {row.value}
            </span>
          </div>
        ))}
      </div>
      <button
        type="button"
        className="btn btn-sm mt-3"
        onClick={() => {
          navigator.clipboard?.writeText(`${version} · ${info.platform} · ${built}`).then(
            () => {
              setCopied(true)
              window.setTimeout(() => setCopied(false), 1500)
            },
            () => undefined,
          )
        }}
      >
        {copied ? t('chat.copied') : t('settings.copyVersion')}
      </button>
    </Section>
  )
}

/* -------------------------------------------------------------------------
 * Local models (llama.cpp that KiChat runs itself)
 * ---------------------------------------------------------------------- */

/**
 * LocalModelSection is the half of a backend that KiChat provides rather than
 * contacts: it fetches llama-server, starts it on a port of its own choosing with
 * a GGUF the user picked, and stops it again.
 *
 * The status is polled rather than pushed. A download or a model load outlives the
 * call that asked for it, and polling cannot miss the end of one — an event sent
 * while the window was reloading would be lost, and the row would sit on "starting"
 * until something else happened to refresh it.
 */
function LocalModelSection({
  backend,
  draft,
  patch,
}: {
  backend: llm.BackendConfig
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const [status, setStatus] = useState<main.RuntimeStatus | null>(null)
  const [error, setError] = useState('')
  // Where models may already be, and what was found in the folder in use. The
  // folder itself is a setting; which folders exist is a question asked of the
  // machine, because LM Studio's own choice of folder is a setting of its own.
  const [folders, setFolders] = useState<runtime.ModelFolder[]>([])
  const [scan, setScan] = useState<runtime.ModelFolderScan | null>(null)
  const [scanError, setScanError] = useState('')
  const [scanning, setScanning] = useState(false)
  // Whether a look at llama.cpp's releases is in flight. The call is awaited —
  // it blocks on one HTTP request, not on a download — so the control can say so.
  const [checking, setChecking] = useState(false)
  // What the installer has on disk. An update never removes the build it replaced —
  // 17.8 MB for the CPU family, 593 MB for a CUDA install with its runtime bundle —
  // so this list is where an old download becomes visible before it becomes a
  // gigabyte. It is measured on every refresh: a directory listing is cheap next to
  // the status call it arrives with, and a size that lags is worse than none.
  const [builds, setBuilds] = useState<main.InstalledBuild[]>([])
  // Which removal is running, by directory name or 'all', so the control that was
  // pressed is the one that says it is working.
  const [cleaning, setCleaning] = useState('')
  const lang = useStore((s) => s.lang)
  // The state a managed server was last seen in, so a change can be told from a
  // poll that found the same thing again.
  const lastState = useRef<string | null>(null)
  const saved = draft.runtime?.models?.[backend.id]
  const [model, setModel] = useState<config.ManagedModel>(() => ({
    enabled: saved?.enabled ?? false,
    modelPath: saved?.modelPath ?? '',
    ctxSize: saved?.ctxSize ?? 8192,
    flashAttention: saved?.flashAttention ?? 'auto',
    cacheTypeK: saved?.cacheTypeK ?? 'f16',
    cacheTypeV: saved?.cacheTypeV ?? 'f16',
    offload: saved?.offload ?? 'auto',
    extraArgs: saved?.extraArgs ?? '',
  }))

  const refresh = async () => {
    try {
      setStatus(await App.GetRuntimeStatus())
      setError('')
    } catch (e) {
      setError(String(e))
    }
    try {
      setBuilds((await App.ListLlamaBuilds()) ?? [])
    } catch (e) {
      setBuilds([])
      setError(String(e))
    }
  }

  const removeBuild = async (dir: string) => {
    setCleaning(dir)
    setError('')
    try {
      await App.RemoveLlamaBuild(dir)
    } catch (e) {
      setError(String(e))
    }
    setCleaning('')
    await refresh()
  }

  const removeOldBuilds = async () => {
    setCleaning('all')
    setError('')
    try {
      await App.RemoveOldLlamaBuilds()
    } catch (e) {
      setError(String(e))
    }
    setCleaning('')
    await refresh()
  }

  useEffect(() => {
    void refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Read as an array even though the backend sends one: a nil Go slice marshals to
  // JSON `null`, and a null here threw inside this section's render and took the
  // whole Settings view down to the error boundary. The backend no longer sends it
  // (pinned by runtime_status_test.go), and this is the second line of defence — a
  // shape mistake should cost one row, not the window.
  const mine = (status?.models ?? []).find((m) => m.backendId === backend.id)
  const state = mine?.state ?? 'stopped'
  const working = !!status?.busy || state === 'starting'

  // Only poll while something is actually moving: an idle window should not ask
  // the backend for status twice a second for the rest of the session.
  useEffect(() => {
    if (!working) return
    const id = window.setInterval(() => void refresh(), 1200)
    return () => window.clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [working])

  // Ask once, when the card first sees an installed build — not on every poll. A
  // reading is only shown for the build it was made for, so a save that switches the
  // family or the source is the other reason to ask again: without that, the row would
  // keep a statement about a build the user no longer runs, or about a different
  // llama.cpp's releases entirely.
  useEffect(() => {
    if (!status?.installed || status?.customBinary) return
    if (
      status.update?.checked &&
      status.update.family === status.accelerator &&
      status.update.distribution === status.distribution
    )
      return
    void checkUpdate()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    status?.installed,
    status?.customBinary,
    status?.accelerator,
    status?.distribution,
    status?.update?.checked,
    status?.update?.family,
    status?.update?.distribution,
  ])

  // A managed server changes the entry it belongs to: the address it landed on is
  // written into the backend row and the registry is rebuilt around it. The row
  // above and the model switcher keep showing the previous answer until they are
  // read again, so the transition is what says to re-read them — polling the same
  // state twice must not turn into a permanent extra round trip.
  useEffect(() => {
    const previous = lastState.current
    lastState.current = state
    if (previous === null || previous === state) return
    void useStore.getState().refreshHealth()
    void useStore.getState().refreshModels()
  }, [state])

  // An install writes the build it fetched — its tag, the source and the family —
  // straight into the settings, from the backend's goroutine, without this view
  // knowing. The draft here and the store's snapshot were read before that, so saving
  // them back would undo the install: the app would start the build the update
  // replaced while the panel showed the new one. When the status names a build the
  // store's copy does not, adopt the backend's. It is a comparison rather than a
  // "busy went false" edge because a fast install can finish between two polls, and it
  // settles: the refresh changes the store, not the status, so it runs at most once
  // per build the status names.
  useEffect(() => {
    if (!status) return
    const stored = useStore.getState().settings?.runtime
    if (
      (status.tag ?? '') !== (stored?.tag ?? '') ||
      (status.distribution ?? '') !== (stored?.distribution ?? '') ||
      (status.accelerator ?? '') !== (stored?.accelerator ?? '')
    ) {
      void useStore.getState().refreshSettings()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status?.tag, status?.distribution, status?.accelerator])

  const update = (changes: Partial<config.ManagedModel>) => {
    const next: config.ManagedModel = { ...model, ...changes }
    setModel(next)
    // Mirrored into the settings draft as well, so the panel's own Save persists
    // the fields the user typed even if they never press a start button.
    const models = { ...(draft.runtime?.models ?? {}), [backend.id]: next }
    patch({ runtime: { ...draft.runtime, models } })
    return next
  }

  const commit = async (next: config.ManagedModel) => {
    setError('')
    try {
      await App.SetManagedModel(backend.id, next)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  // Both values are passed in rather than read from the draft: the store's button
  // downloads the build and the source the pickers show, while the update row
  // downloads what the installed build belongs to. An empty value asks the backend
  // for the stored one, which is what the update row means.
  const install = async (accelerator: string, source: string) => {
    setError('')
    try {
      // The selected build and source, not the saved ones: the pickers edit the
      // draft, and downloading the stored family here is how "I picked ROCm" used
      // to install the CPU build. With the source in the draft as well, reading the
      // stored one would install the wrong llama.cpp entirely — the backend records
      // both of what it really fetched.
      await App.InstallLlamaRuntime(accelerator, source)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  // The one question in this card that costs a request to GitHub's API, which
  // allows 60 an hour per address unauthenticated. It is asked for rather than
  // polled, and the backend caches the answer so the row keeps showing it.
  const checkUpdate = async () => {
    setChecking(true)
    try {
      await App.CheckLlamaRuntimeUpdate()
    } catch (e) {
      setError(String(e))
    }
    setChecking(false)
    await refresh()
  }

  const chooseModel = async () => {
    try {
      const picked = await App.PickManagedModelFile(t('settings.managed.pickTitle'))
      if (picked) update({ modelPath: picked })
    } catch (e) {
      setError(String(e))
    }
  }

  // An empty folder is not "no folder": it asks the backend for the folder it
  // would use, which is LM Studio's when the user never chose one. That is what
  // makes the models a user already has show up without anyone configuring
  // anything.
  const modelDir = draft.runtime?.modelDir ?? ''

  const rescan = async (dir: string) => {
    setScanning(true)
    try {
      const found = await App.ScanModelFolder(dir)
      // `files` is read as an array the moment it arrives (see above): a null one
      // would blank the view instead of reporting "no models".
      setScan({ ...found, files: found?.files ?? [] })
      setScanError('')
    } catch (e) {
      setScan(null)
      setScanError(t('settings.managed.folderMissing'))
      void e
    } finally {
      setScanning(false)
    }
  }

  useEffect(() => {
    void App.ModelFolders()
      .then((list) => setFolders(list ?? []))
      .catch(() => undefined)
  }, [modelDir])

  useEffect(() => {
    void rescan(modelDir)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [modelDir])

  const chooseFolder = async () => {
    try {
      const picked = await App.PickModelFolder(t('settings.managed.folderPickTitle'))
      if (!picked) return
      patch({ runtime: { ...draft.runtime, modelDir: picked } })
    } catch (e) {
      setError(String(e))
    }
  }

  const useFolder = (dir: string) => patch({ runtime: { ...draft.runtime, modelDir: dir } })

  // The families this platform actually publishes, straight from the installer:
  // ROCm and SYCL are x64 only and macOS ships one Metal-enabled build, so a list
  // of its own would offer downloads that do not exist. A stored family this
  // platform does not publish stays in the list and stays selected — swapping the
  // user to CPU without saying so is the substitution the whole picker avoids;
  // installing it reports what the platform has instead.
  const published = status?.variants?.length ? status.variants : ['cpu', 'vulkan']
  const chosenAccel = draft.runtime?.accelerator ?? 'cpu'
  const accelOptions = published.includes(chosenAccel) ? published : [...published, chosenAccel]
  const accelLabel = (id: string) =>
    t(`settings.managed.accel${id.charAt(0).toUpperCase()}${id.slice(1)}`)

  // The release sources the app can install from, straight from the backend for the
  // same reason as the families: a source with no repository behind it must not be
  // offered. The families above belong to the selected source — the fork publishes
  // five of upstream's six — so the two pickers read from the same status.
  const sources = status?.distributions?.length ? status.distributions : ['llamacpp']
  const chosenSource = draft.runtime?.distribution ?? 'llamacpp'
  const sourceOptions = sources.includes(chosenSource) ? sources : [...sources, chosenSource]
  const sourceLabel = (id: string) =>
    t(`settings.managed.dist${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  const sourceHint = chosenSource === 'prism'
    ? t('settings.managed.distPrismHint')
    : t('settings.managed.distLlamacppHint')

  // Filled from the backend, like the accelerator picker: a value the app cannot
  // pass to llama-server must not be offered here.
  const flashModes = status?.flashAttentionModes?.length ? status.flashAttentionModes : ['auto', 'on', 'off']
  const cacheTypes = status?.cacheTypes?.length ? status.cacheTypes : ['f16']
  const flashLabel = (id: string) =>
    t(`settings.managed.flash${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  const cacheLabel = (id: string) =>
    t(`settings.managed.cache${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  // The offload modes have labels in the same shape, but only the words: the modes
  // are this app's own vocabulary for `--gpu-layers` rather than a list llama.cpp
  // reports, so the fallback below is what a status older than this field sends.
  const offloadModes = status?.offloadModes?.length ? status.offloadModes : ['auto', 'all', 'off']
  const offloadLabel = (id: string) =>
    t(`settings.managed.offload${id.charAt(0).toUpperCase()}${id.slice(1)}`)

  const folderLabel = (kind: string) =>
    kind === 'lmstudio-configured'
      ? t('settings.managed.folderLmStudioConfigured')
      : kind === 'lmstudio'
        ? t('settings.managed.folderLmStudio')
        : kind === 'lmstudio-legacy'
          ? t('settings.managed.folderLmStudioLegacy')
          : kind === 'lmstudio-bundled'
            ? t('settings.managed.folderLmStudioBundled')
            : t('settings.managed.folderCustom')

  const start = async () => {
    // The fields are saved first: starting with a context size the panel does not
    // show would be a server the user did not configure.
    await commit(model)
    try {
      await App.StartManagedModel(backend.id)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  const toggle = async (on: boolean) => {
    const next = update({ enabled: on })
    await commit(next)
    if (on) {
      try {
        await App.StartManagedModel(backend.id)
      } catch (e) {
        setError(String(e))
      }
      await refresh()
    }
  }

  const stop = async () => {
    try {
      await App.StopManagedModel(backend.id)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  const percent =
    status && status.total > 0 ? Math.min(100, Math.round((status.done / status.total) * 100)) : 0

  // The selected model's weights against the memory the reported devices have
  // free. It is the file size and not an estimate of the whole footprint — that is
  // the only part this app can measure without loading the model — so the verdict
  // reads "does not fit in full", which is the cautious direction.
  //
  // The stored row's size is used when it is the file on screen; a model picked
  // but not yet saved has no row yet, and the folder scan has already measured it,
  // so a stale size from the previous file cannot produce a stale warning.
  const selectedBytes =
    mine && mine.modelPath === model.modelPath && mine.modelBytes > 0
      ? mine.modelBytes
      : ((scan?.files ?? []).find((f) => f.path === model.modelPath)?.size ?? 0)

  // The scanned entry the chosen path points at. It carries what the path cannot:
  // the model's own name, the folder it came from and its size.
  const pickedFile = (scan?.files ?? []).find((f) => f.path === model.modelPath)
  // What the app has actually stored, against the draft this panel edits. A pick in
  // the list below changes the draft, and the header's Save (or a start) is what
  // makes it real — so a pick that is not yet saved has to say so rather than look
  // like the model in use. Starting saves first, which is why this clears on start.
  const storedPath = useStore((s) => s.settings?.runtime?.models?.[backend.id]?.modelPath) ?? ''
  const modelPending = !!model.modelPath && model.modelPath !== storedPath
  const deviceFreeBytes = status?.deviceFreeBytes ?? 0
  // A CPU family is not a mistake to warn about, and a build that reported no
  // memory has nothing to compare against — that case already has its own row.
  const tooLarge = chosenAccel !== 'cpu' && deviceFreeBytes > 0 && selectedBytes > deviceFreeBytes

  // What the backend knows about a newer llama.cpp build. The reading counts only
  // when it is about the build the app installs for — the family and the source the
  // pickers edit are still a draft, and a reading taken for another build, or from
  // another source's releases, says nothing about this one.
  const updateIsAboutThisBuild =
    !!status?.update?.checked &&
    status.update.family === status.accelerator &&
    status.update.distribution === status.distribution
  const updateAvailable = updateIsAboutThisBuild && !!status?.update?.available && !status?.update?.error
  // A server that is already running keeps the build it was started from, so this is
  // the one place an update needs a sentence — both before it is installed (the
  // running model will not change) and after, because the line has to survive the
  // update it is about. A managed start clears the backend's flag.
  const updateNeedsRestart = state === 'running' && (updateAvailable || !!status?.restartPending)

  // The builds nothing is using. The backend refuses the other two by name — the one
  // KiChat would start, and the one a server is running from — so this is the set the
  // buttons may act on, and the sum is what one press would reclaim.
  const removable = builds.filter((b) => !b.inUse && !b.running)
  const reclaimBytes = removable.reduce((sum, b) => sum + b.size, 0)

  return (
    <div className="mt-3 rounded-md p-3" style={{ background: 'var(--surface)' }}>
      <div className="mb-2 text-[12px] font-semibold">{t('settings.managed.title')}</div>
      <div className="mb-2 text-[11px] faint">{t('settings.managed.hint')}</div>

      <div className="mb-2 flex items-center gap-2">
        <span className="badge">{status?.installed ? t('settings.managed.installed') : t('settings.managed.notInstalled')}</span>
        <span className="mono min-w-0 flex-1 truncate text-[11px]" title={status?.binary ?? status?.customBinary ?? ''}>
          {status?.customBinary || status?.binary || t('settings.managed.noBinary')}
        </span>
        {status?.tag && <span className="mono text-[11px] faint">{status.tag}</span>}
      </div>

      {/* What the launcher itself reports. The tag above is a name this app chose
          for a download; this is the build actually on disk, which is the only
          thing that describes a binary the user supplied instead. */}
      <div className="mb-2 flex items-center gap-2 text-[11px]">
        <span className="faint shrink-0">{t('settings.managed.version')}</span>
        <span
          className="mono min-w-0 flex-1 truncate"
          title={status?.version || t('settings.managed.versionUnknown')}
        >
          {status?.version || t('settings.managed.versionUnknown')}
        </span>
      </div>

      {/* Is there a newer llama.cpp build than the one on disk? The tag above is
          this app's name for the download; this row is about the build itself, and
          it is what turns "reinstall and hope" into "b11535 → b11541". */}
      {status?.installed && !status?.customBinary && (
        <div className="mb-2 flex flex-wrap items-center gap-2 text-[11px]">
          <span className="faint shrink-0">{t('settings.managed.update')}</span>
          {!updateIsAboutThisBuild ? (
            <span className="faint min-w-0 flex-1">{t('settings.managed.updateNotChecked')}</span>
          ) : status.update?.error ? (
            // The reason and not a generic "could not check": a rate limit and an
            // unreachable API are told apart by the API's own words.
            <span
              className="min-w-0 flex-1 truncate"
              style={{ color: 'var(--danger)' }}
              title={status.update.error}
            >
              {status.update.error}
            </span>
          ) : status.update?.available ? (
            <>
              <span className="badge badge-warn shrink-0">{t('settings.managed.updateAvailable')}</span>
              <span className="mono min-w-0 truncate" title={`${status.update.installed} → ${status.update.latest}`}>
                {status.update.installed} → {status.update.latest}
              </span>
              {!!status.update.size && (
                <span className="mono faint shrink-0">{formatBytes(status.update.size)}</span>
              )}
            </>
          ) : (
            <span className="faint min-w-0 flex-1">{t('settings.managed.updateCurrent')}</span>
          )}
          <button
            type="button"
            className="btn btn-sm shrink-0"
            disabled={checking || !!status?.busy}
            onClick={() => void checkUpdate()}
          >
            {checking ? t('settings.managed.updateChecking') : t('settings.managed.updateCheck')}
          </button>
          {updateAvailable && (
            <button
              type="button"
              className="btn btn-sm shrink-0"
              disabled={!!status?.busy}
              onClick={() => void install(status?.accelerator || '', status?.distribution || '')}
            >
              {t('settings.managed.updateInstall')}
            </button>
          )}
        </div>
      )}

      {updateNeedsRestart && (
        <div className="mb-2 text-[11px] faint">{t('settings.managed.updateRestart')}</div>
      )}

      {/* What is on disk, and what can go. It stays out of the way while there is
          nothing to remove, and appears with the sizes the moment an update has left
          a build behind — that is the only moment this list is interesting. */}
      {removable.length > 0 && (
        <div className="mb-2 rounded-md p-2 text-[11px]" style={{ background: 'var(--surface-2)' }}>
          <div className="mb-1 flex flex-wrap items-center gap-2">
            <span className="faint shrink-0">{t('settings.managed.builds')}</span>
            <span className="mono faint">{formatBytes(reclaimBytes)}</span>
            <button
              type="button"
              className="btn btn-sm ml-auto shrink-0"
              disabled={!!cleaning || !!status?.busy}
              onClick={() => void removeOldBuilds()}
            >
              {cleaning === 'all'
                ? t('settings.managed.buildsRemoving')
                : t('settings.managed.buildsRemoveOld', { count: String(removable.length) })}
            </button>
          </div>
          <div className="space-y-0.5">
            {builds.map((b) => (
              <div key={b.dir} className="flex items-center gap-2">
                <span className="mono min-w-0 truncate" title={b.dir}>
                  {b.dir}
                </span>
                {/* Which line may not be deleted, and why. The two badges are kept
                    apart on purpose: one is what KiChat would start, the other is
                    what is answering right now. */}
                {b.inUse && <span className="badge badge-ok shrink-0">{t('settings.managed.buildInUse')}</span>}
                {b.running && <span className="badge badge-ok shrink-0">{t('settings.managed.buildRunning')}</span>}
                {b.incomplete && (
                  <span className="badge badge-warn shrink-0">{t('settings.managed.buildIncomplete')}</span>
                )}
                <span className="faint ml-auto shrink-0">{timeAgo(b.installedAt, lang)}</span>
                <span className="mono faint shrink-0">{formatBytes(b.size)}</span>
                {!b.inUse && !b.running && (
                  <button
                    type="button"
                    className="btn btn-sm shrink-0"
                    disabled={!!cleaning}
                    aria-label={`${t('settings.managed.buildRemove')}: ${b.dir}`}
                    title={t('settings.managed.buildRemove')}
                    onClick={() => void removeBuild(b.dir)}
                  >
                    ✕
                  </button>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Which compute devices the installed build can actually use. This is what
          makes a silent CPU fallback visible: a GPU build that finds no device,
          or a CPU build kept by mistake, says so here instead of only showing up
          as a model that feels slow. */}
      {status?.installed && (
        <div className="mb-2 flex items-start gap-2 text-[11px]">
          <span className="faint shrink-0">{t('settings.managed.devices')}</span>
          {status.devices?.length ? (
            <div className="min-w-0 flex-1 space-y-0.5">
              {status.devices.map((d) => (
                <div key={d} className="mono truncate" title={d}>
                  {d}
                </div>
              ))}
            </div>
          ) : (
            <div className="min-w-0 flex-1">
              <span
                className="block"
                style={chosenAccel !== 'cpu' && !status.devicesError ? { color: 'var(--danger)' } : undefined}
              >
                {status.devicesError ||
                  (chosenAccel !== 'cpu'
                    ? t('settings.managed.devicesNoneGpu')
                    : t('settings.managed.devicesNone'))}
              </span>
              {/* Why there is no device, when the build itself is the reason. A
                  GPU build whose own runtime libraries are missing reports no
                  device and no error, so without this the row can only say "none"
                  — which is what sends someone looking at their card instead of at
                  their ROCm version. */}
              {!!status.devicesMissingLibs?.length && (
                <span className="mt-0.5 block" style={{ color: 'var(--danger)' }}>
                  {t('settings.managed.devicesMissingLibs', {
                    libs: (status.devicesMissingLibs ?? []).join(', '),
                  })}
                </span>
              )}
            </div>
          )}
        </div>
      )}

      {!status?.customBinary && (
        <div className="mb-2 flex items-center gap-2">
          <button
            type="button"
            className="btn btn-sm"
            disabled={!!status?.busy}
            onClick={() => void install(chosenAccel, chosenSource)}
          >
            {status?.busy ? t('settings.managed.installing') : t('settings.managed.install')}
          </button>
          {status?.busy && (
            <span className="text-[11px] faint">
              {status.phase === 'downloading'
                ? `${t('settings.managed.downloading')} ${percent}%`
                : status.phase === 'extracting'
                  ? t('settings.managed.extracting')
                  : t('settings.managed.preparing')}
            </span>
          )}
        </div>
      )}

      {/* Which llama.cpp the app installs from. It is one setting for the whole
          runtime because there is one runtime directory and one installed launcher:
          the two sources publish different releases under different tag schemes, and
          only the fork carries the ternary kernels a model like Bonsai 2 needs. */}
      <div className="mb-2 grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.managed.distribution')}
          <select
            className="input mono mt-1"
            value={chosenSource}
            onChange={(e) => patch({ runtime: { ...draft.runtime, distribution: e.target.value } })}
          >
            {sourceOptions.map((id) => (
              <option key={id} value={id}>
                {sourceLabel(id)}
              </option>
            ))}
          </select>
        </label>
        <div className="self-end pb-1 text-[11px] faint">{sourceHint}</div>
      </div>

      <div className="mb-2 grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.managed.accelerator')}
          <select
            className="input mono mt-1"
            value={chosenAccel}
            onChange={(e) => patch({ runtime: { ...draft.runtime, accelerator: e.target.value } })}
          >
            {accelOptions.map((id) => (
              <option key={id} value={id}>
                {accelLabel(id)}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px]">
          {t('settings.managed.binaryPath')}
          <input
            className="input mono mt-1"
            placeholder={t('settings.managed.binaryPlaceholder')}
            value={draft.runtime?.binaryPath ?? ''}
            onChange={(e) => patch({ runtime: { ...draft.runtime, binaryPath: e.target.value } })}
          />
        </label>
      </div>

      <label className="mb-2 flex items-center gap-2 text-[12px]">
        <input type="checkbox" checked={!!model.enabled} onChange={(e) => void toggle(e.target.checked)} />
        {t('settings.managed.enable')}
      </label>

      <div className="mb-2 flex items-center gap-2">
        <input
          className="input mono flex-1"
          placeholder={t('settings.managed.modelPlaceholder')}
          value={model.modelPath}
          onChange={(e) => update({ modelPath: e.target.value })}
        />
        <button type="button" className="btn btn-sm" onClick={() => void chooseModel()}>
          {t('settings.managed.choose')}
        </button>
      </div>

      {/* The field above holds a path, and a path is not how anyone recognises a
          model. Picking in the list below used to change nothing but that string,
          which is why a click read as "did that do anything?" — so the choice is
          stated here by name, with the folder and size the folder scan measured and
          the full path kept as the tooltip. */}
      {model.modelPath && (
        <div className="mb-2 flex items-center gap-2 text-[11px]">
          {/* The same accent as the marked row below, so the two read as one
              statement rather than as a state badge and a tick. */}
          <span className="shrink-0" style={{ color: 'var(--accent)' }}>
            ✓
          </span>
          <span className="faint shrink-0">{t('settings.managed.selected')}</span>
          <span className="mono min-w-0 truncate" title={model.modelPath}>
            {modelLabel(pickedFile?.name ?? model.modelPath)}
          </span>
          {pickedFile && <span className="faint max-w-[40%] truncate">{pickedFile.model}</span>}
          {pickedFile && <span className="mono faint shrink-0">{formatBytes(pickedFile.size)}</span>}
          {modelPending && (
            <span className="badge badge-warn shrink-0">{t('settings.managed.unsaved')}</span>
          )}
        </div>
      )}

      {/* A user who already has LM Studio has models on disk. Listing them here is
          what turns "pick a GGUF from anywhere" into "use the one you already
          downloaded", without KiChat copying or re-downloading anything. */}
      <div className="mb-2 rounded-md p-2" style={{ background: 'var(--surface-2)' }}>
        <div className="mb-1 text-[11px]">{t('settings.managed.folder')}</div>
        <div className="mb-1 flex items-center gap-2">
          <input
            className="input mono flex-1"
            placeholder={folders.find((f) => f.exists)?.path ?? t('settings.managed.modelPlaceholder')}
            value={modelDir}
            onChange={(e) => patch({ runtime: { ...draft.runtime, modelDir: e.target.value } })}
          />
          <button type="button" className="btn btn-sm" onClick={() => void chooseFolder()}>
            {t('settings.managed.folderPick')}
          </button>
        </div>
        {folders.filter((f) => f.exists).length > 0 && (
          <div className="mb-1 flex flex-wrap gap-1">
            {folders
              .filter((f) => f.exists)
              .map((f) => (
                <button
                  key={f.path}
                  type="button"
                  className={`btn btn-sm ${f.path === modelDir ? '' : 'btn-ghost'}`}
                  title={f.path}
                  onClick={() => useFolder(f.path)}
                >
                  {folderLabel(f.kind)}
                </button>
              ))}
          </div>
        )}
        {scan && scan.files.length > 0 ? (
          <div className="max-h-40 overflow-auto rounded-md" style={{ border: '1px solid var(--border)' }}>
            {scan.files.map((file) => {
              const picked = file.path === model.modelPath
              return (
                <button
                  key={file.path}
                  type="button"
                  aria-current={picked ? 'true' : undefined}
                  className="flex w-full items-center gap-2 px-2 py-1 text-left text-[11px] transition-colors hover:bg-[var(--surface-3)]"
                  // A tint rather than a surface colour: the picked row used to be
                  // `--surface-2`, which is exactly the colour of the box it sits in,
                  // so the selection was invisible in this very list.
                  style={
                    picked
                      ? { background: 'color-mix(in srgb, var(--accent) 16%, transparent)' }
                      : undefined
                  }
                  title={file.path}
                  onClick={() => update({ modelPath: file.path })}
                >
                  <span
                    className="shrink-0"
                    style={{ width: '0.75rem', color: 'var(--accent)' }}
                    aria-hidden="true"
                  >
                    {picked ? '✓' : ''}
                  </span>
                  <span
                    className={`mono min-w-0 flex-1 truncate ${picked ? 'font-semibold' : ''}`}
                  >
                    {file.name}
                  </span>
                  <span className="faint max-w-[40%] truncate">{file.model}</span>
                  <span className="mono faint shrink-0">{formatBytes(file.size)}</span>
                </button>
              )
            })}
          </div>
        ) : (
          <div className="text-[11px] faint">
            {scanning
              ? t('settings.managed.folderScanning')
              : scanError || t('settings.managed.folderEmpty')}
          </div>
        )}
        {scan && scan.files.length > 0 && (
          <div className="mt-1 text-[11px] faint">
            {formatNumber(scan.files.length)}{' '}
            {t(scan.files.length === 1 ? 'settings.managed.folderModel' : 'settings.managed.folderModels')}
            {scan.truncated ? ` — ${t('settings.managed.folderTruncated')}` : ''}
          </div>
        )}
      </div>

      <div className="mb-2 grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.managed.ctx')}
          <input
            className="input mono mt-1"
            type="number"
            min={2048}
            step={1024}
            value={model.ctxSize}
            onChange={(e) => update({ ctxSize: Number(e.target.value) || 0 })}
          />
        </label>
        <label className="text-[11px]">
          {t('settings.managed.extra')}
          {/* A layer count is the one flag this box no longer has to advertise: the
              row below sets `--gpu-layers`, and a count typed here still wins over
              it, because the user's own arguments go last on the command line. */}
          <input
            className="input mono mt-1"
            placeholder="--lora /pfad/adapter.gguf"
            value={model.extraArgs}
            onChange={(e) => update({ extraArgs: e.target.value })}
          />
        </label>
      </div>

      {/* Flash attention and a quantised KV cache trade memory for precision, and
          the choice only takes effect on the next start — a running server is
          not reconfigured from here. */}
      <div className="mb-2 grid grid-cols-3 gap-2">
        <label className="text-[11px]">
          {t('settings.managed.flashAttention')}
          <select
            className="input mono mt-1"
            value={model.flashAttention}
            onChange={(e) => update({ flashAttention: e.target.value })}
          >
            {flashModes.map((id) => (
              <option key={id} value={id}>
                {flashLabel(id)}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px]">
          {t('settings.managed.cacheTypeK')}
          <select
            className="input mono mt-1"
            value={model.cacheTypeK}
            onChange={(e) => update({ cacheTypeK: e.target.value })}
          >
            {cacheTypes.map((id) => (
              <option key={id} value={id}>
                {cacheLabel(id)}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[11px]">
          {t('settings.managed.cacheTypeV')}
          <select
            className="input mono mt-1"
            value={model.cacheTypeV}
            onChange={(e) => update({ cacheTypeV: e.target.value })}
          >
            {cacheTypes.map((id) => (
              <option key={id} value={id}>
                {cacheLabel(id)}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="mb-2 text-[11px] faint">{t('settings.managed.cacheHint')}</div>

      {/* Before the Start button, because this is the last moment it can be said
          without a model already loading: the weights alone are bigger than what
          the devices have free, so part or all of the model is going to be
          computed on the CPU. A file smaller than the free memory is not a
          promise that it fits — the KV cache needs its share too — which is why
          only the failing direction is shown. */}
      {tooLarge && (
        <div className="mb-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {t('settings.managed.tooLarge', {
            model: formatBytes(selectedBytes),
            free: formatBytes(deviceFreeBytes),
          })}
        </div>
      )}

      {/* How much of the model goes on the GPU. It sits above the flash/cache row
          because it is the setting the other two react to: a quantised KV cache is
          what buys the room for more layers, and a model too large for the card is
          the reason to leave them off. Changing it applies from the next start — a
          running server is not reconfigured from here. */}
      <div className="mb-2">
        <label className="text-[11px]">
          {t('settings.managed.offload')}
          <select
            className="input mono mt-1"
            value={model.offload}
            onChange={(e) => update({ offload: e.target.value })}
          >
            {offloadModes.map((id) => (
              <option key={id} value={id}>
                {offloadLabel(id)}
              </option>
            ))}
          </select>
        </label>
        <div className="mt-1 text-[11px] faint">{t('settings.managed.offloadHint')}</div>
      </div>

      <div className="flex items-center gap-2">
        <button type="button" className="btn btn-sm" disabled={!status?.installed || !model.modelPath} onClick={() => void start()}>
          {t('settings.managed.start')}
        </button>
        <button type="button" className="btn btn-sm" disabled={state === 'stopped'} onClick={() => void stop()}>
          {t('settings.managed.stop')}
        </button>
        <span className={`badge ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}>
          {t(`settings.managed.state.${state}`)}
        </span>
        {mine?.url && <span className="mono min-w-0 flex-1 truncate text-[11px] faint">{mine.url}</span>}
      </div>

      {/* What the server itself said about the placement, which is the only answer
          to "is this model actually running on the GPU?" — the free-memory warning
          above is a prediction, this is the reading. The numbers come from
          llama.cpp's own log at the trace level, so an empty one means "not
          reported" rather than "nothing offloaded": a launcher the app did not
          install is never asked for that level, and neither is one that failed
          before it got to the layers. It appears while the model is still loading,
          because that is when the server says it. */}
      {(mine?.offloadTotal ?? 0) > 0 && (
        <div className="mt-1 text-[11px] faint">
          {/* The device is the whole test, and the backend decides it: it sends a
              name only when layers really are on a card. A CPU-only build reports
              every layer offloaded without naming one — llama.cpp's wording — so
              the count alone would be claiming an accelerator that is not there. */}
          {mine?.offloadDevice
            ? t('settings.managed.offloadReport', {
                device: mine.offloadDevice,
                layers: formatNumber(mine.offloadLayers),
                total: formatNumber(mine.offloadTotal),
              })
            : t('settings.managed.offloadCpu', { total: formatNumber(mine?.offloadTotal ?? 0) })}
        </div>
      )}
      {mine?.error && (
        <div className="mt-2 whitespace-pre-wrap text-[11px]" style={{ color: 'var(--danger)' }}>
          {mine.error}
        </div>
      )}
      {error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {error}
        </div>
      )}
      {status?.error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {status.error}
        </div>
      )}
      {state === 'starting' && mine?.log?.length ? (
        <pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap text-[10px] faint">
          {mine.log.join('\n')}
        </pre>
      ) : null}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Backends
 * ---------------------------------------------------------------------- */

function BackendsTab({
  draft,
  patch,
  onApply,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
  // Persisting is what applies a typed-in API key to the registry, so the
  // refresh control commits the draft first and then re-reads models/health.
  // The payload is optional so a row can apply a change of its own in one click.
  // It returns the save's promise so a caller that has to *ask the running app*
  // something afterwards — the image section's connection test — can wait for the
  // rebuild instead of racing it.
  onApply: (next?: config.Settings) => Promise<void>
}) {
  const t = useStore((s) => s.t)
  const health = useStore((s) => s.health)
  const models = useStore((s) => s.models)
  const [secrets, setSecrets] = useState<main.SecretStatus | null>(null)

  // Re-read with the health report: a save is what puts a key in the store, and
  // the flag saying which entries have one would otherwise stay stale for the
  // rest of the session — the panel would forget a key it had just stored, which
  // is precisely the state this flag exists to explain.
  useEffect(() => {
    void App.GetSecretStatus().then(setSecrets).catch(() => undefined)
  }, [health])

  const updateBackend = (index: number, changes: Partial<llm.BackendConfig>) => {
    const backends = draft.backends.map((b, i) => (i === index ? { ...b, ...changes } : b))
    patch({ backends })
  }

  return (
    <>
      <Section
        title={t('settings.backends')}
        hint={t('settings.backendsHint')}
      >
        {draft.backends.map((backend, index) => {
          const status = health.find((h) => h.id === backend.id)
          const modelCount = models.filter((m) => m.backend === backend.id).length
          // The state the running app reported, not the checkbox: an entry that is
          // off was never contacted, and that is what the hint below explains.
          const offSaved = status ? !status.enabled : false
          // A key counts as present whether it was just typed or is already in the
          // secret store. The stored one cannot be read back — that is what being a
          // secret means here — so the backend reports only which entries have one.
          const keyPresent =
            (backend.apiKey ?? '').trim() !== '' || (secrets?.backendKeys?.[backend.id] ?? false)
          const enableAndApply = () => {
            const backends = draft.backends.map((b, i) => (i === index ? { ...b, enabled: true } : b))
            patch({ backends })
            void onApply({ ...draft, backends })
          }
          return (
            <div key={backend.id} className="mb-3 rounded-md p-3" style={{ background: 'var(--surface-2)' }}>
              <div className="mb-2 flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={backend.enabled}
                  aria-label={t('settings.enabled')}
                  title={t('settings.enabled')}
                  onChange={(e) => updateBackend(index, { enabled: e.target.checked })}
                />
                <input
                  className="input flex-1"
                  value={backend.name}
                  onChange={(e) => updateBackend(index, { name: e.target.value })}
                />
                {/* Three states, not two. A backend that is switched off was
                    never contacted, so it is not "unreachable" — that label sent
                    a user who had just pasted a key to fix an address nothing
                    had asked. And a row the backend did not report at all is a
                    draft that has not been saved, which is also not a failure.
                    The badge follows the reported state rather than the
                    checkbox, because it describes the connection the running
                    app actually has; saving is what changes it. */}
                {!status ? (
                  <span className="badge">{t('settings.notChecked')}</span>
                ) : !status.enabled ? (
                  <span className="badge">{t('settings.disabled')}</span>
                ) : (
                  <span className={`badge ${status.ok ? 'badge-ok' : 'badge-danger'}`}>
                    {status.ok ? `${t('settings.ok')} · ${modelCount} ${t('settings.modelsFound')}` : t('settings.fail')}
                  </span>
                )}
              </div>
              <input
                className="input mono"
                value={backend.baseUrl}
                onChange={(e) => updateBackend(index, { baseUrl: e.target.value })}
                placeholder="http://127.0.0.1:1234"
              />
              {/* llama.cpp belongs here because `llama-server --api-key` makes the
                  server reject every request without the matching bearer token;
                  a keyed server would otherwise be unreachable from the UI. */}
              {(backend.kind === 'custom' ||
                backend.kind === 'deepseek' ||
                backend.kind === 'mistral' ||
                backend.kind === 'nvidia' ||
                backend.kind === 'llamacpp') && (
                <input
                  className="input mono mt-2"
                  type="password"
                  value={backend.apiKey ?? ''}
                  onChange={(e) => updateBackend(index, { apiKey: e.target.value })}
                  placeholder={
                    backend.kind === 'deepseek' ||
                    backend.kind === 'mistral' ||
                    backend.kind === 'nvidia'
                      ? t('settings.apiKeyRequired')
                      : t('settings.apiKeyOptional')
                  }
                />
              )}
              {status && status.enabled && !status.ok && (
                <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
                  {status.error}
                </div>
              )}
              {/* A key in an entry that is switched off is stored and then never
                  used: no provider is built for an off entry, so no probe is sent
                  and there is no connection to report. "off" on its own leaves the
                  user who just pasted a key with nothing to do next, so the reason
                  and the one click that settles it sit together here. */}
              {offSaved && keyPresent && (
                <div
                  className="mt-2 flex items-center gap-2 text-[11px]"
                  style={{ color: 'var(--text-dim)' }}
                >
                  <span className="min-w-0 flex-1">{t('settings.keyStoredWhileOff')}</span>
                  <button type="button" className="btn btn-sm" onClick={enableAndApply}>
                    {t('settings.enableAndCheck')}
                  </button>
                </div>
              )}
              {/* Only the kind that KiChat knows how to run itself gets the
                  controls; every other entry stays an address it contacts. */}
              {backend.kind === 'llamacpp' && (
                <LocalModelSection backend={backend} draft={draft} patch={patch} />
              )}
            </div>
          )
        })}
        <div className="flex gap-2">
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => void onApply()}
          >
            {t('settings.refresh')}
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() =>
              patch({
                backends: [
                  ...draft.backends,
                  {
                    id: `custom-${Date.now().toString(36)}`,
                    name: 'Custom',
                    kind: 'custom',
                    baseUrl: 'http://127.0.0.1:8080',
                    apiKey: '',
                    enabled: true,
                  } as llm.BackendConfig,
                ],
              })
            }
          >
            + {t('settings.addCustom')}
          </button>
        </div>
      </Section>

      <Section title={t('settings.secrets')} hint={secrets?.warning}>
        <div className="text-[13px]">
          {t('settings.secrets')}: <strong>{secrets?.backend ?? '…'}</strong>
          {secrets && !secrets.keyring && (
            <span className="badge badge-warn ml-2">{t('settings.keyringMissing')}</span>
          )}
        </div>
      </Section>

      <ModelOverridesTab />
    </>
  )
}

/**
 * The picture server, on a tab of its own.
 *
 * It began as a section of the backends tab and moved out once the setup outgrew a
 * row beside a chat backend. Three things have no counterpart there: the server has
 * to be started with its API exposed and a checkpoint installed, the generation
 * fields each need a sentence about what leaving them empty means, and the
 * connection test spends real GPU seconds drawing a picture. None of that applies to
 * a chat backend — there is no context window to show, no per-turn model to pick,
 * and a request answers with a picture instead of a stream. The one hard rule,
 * stated under the fields, is that an empty box leaves the setting to the server: a
 * WebUI user has already chosen a sampler, a step count and a size, and filling
 * those in with guesses would replace their setup.
 */
function ImagesTab({
  draft,
  patch,
  onApply,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
  onApply: (next?: config.Settings) => Promise<void>
}) {
  const t = useStore((s) => s.t)
  const [status, setStatus] = useState<main.ImageStatus | null>(null)
  const [checking, setChecking] = useState(false)
  const [testing, setTesting] = useState(false)
  const images = draft.images ?? ({} as config.ImageSettings)

  const update = (changes: Partial<config.ImageSettings>) =>
    patch({ images: { ...images, ...changes } as config.ImageSettings })

  // Read once on mount. The probe is a request to a server the user may not be
  // running, so re-asking on every keystroke in the draft would hammer a port that
  // is probably closed; the test button is what re-reads it deliberately. The
  // effect is written out rather than shared with that button because it depends
  // on nothing: a helper would either be a dependency that changes every render or
  // a stale closure, and an empty dependency list is neither.
  useEffect(() => {
    let cancelled = false
    setChecking(true)
    App.GetImageStatus()
      .then((next) => {
        if (!cancelled) setStatus(next)
      })
      .catch((err) => {
        if (!cancelled) useStore.getState().toast('error', String(err))
      })
      .finally(() => {
        if (!cancelled) setChecking(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const test = async () => {
    setTesting(true)
    try {
      // The app probes what was *saved*, so the draft has to be committed first:
      // testing an address that was just typed would otherwise test the previous
      // one and report a failure about a server the user already corrected.
      await onApply({ ...draft, images })
      // A listing cannot tell a server that is up from one that can draw: a WebUI
      // with no checkpoint loaded answers `/sdapi/v1/sd-models` and then fails
      // every generation. So the button draws one small picture and throws it
      // away, which is the only check that exercises the route the tool uses.
      const report = await App.TestImageBackend()
      setStatus(await App.GetImageStatus())
      useStore.getState().toast('info', report)
    } catch (err) {
      useStore.getState().toast('error', String(err))
      // The row is re-read even so, because what a failed generation needs to be
      // read against is whether the address answers at all: a WebUI that is
      // running with nothing loaded is a different repair from one that is not up.
      try {
        setStatus(await App.GetImageStatus())
      } catch (statusErr) {
        useStore.getState().toast('error', String(statusErr))
      }
    } finally {
      setTesting(false)
    }
  }

  return (
    <Section title={t('settings.images')} hint={t('settings.imagesHint')}>
      <div className="mb-2 flex items-center gap-2">
        <input
          type="checkbox"
          checked={images.enabled ?? false}
          aria-label={t('settings.images.enabled')}
          title={t('settings.images.enabled')}
          onChange={(e) => update({ enabled: e.target.checked })}
        />
        <span className="flex-1 text-[13px]">{t('settings.images.enabled')}</span>
        {/* Four states, for the reasons the backend rows already learned. Switched
            off is not "unreachable" — nothing was sent. And a server with no
            checkpoint installed is reachable and still unable to draw, which is
            its own sentence rather than a failure of the address. */}
        {!images.enabled ? (
          <span className="badge">{t('settings.images.off')}</span>
        ) : status?.error ? (
          <span className="badge badge-danger" title={status.error}>
            {t('settings.fail')}
          </span>
        ) : status?.ok ? (
          <span className={`badge ${status.models.length > 0 ? 'badge-ok' : 'badge-warn'}`}>
            {t('settings.images.ok')} · {status.models.length} {t('settings.images.models')}
          </span>
        ) : (
          <span className="badge">{checking ? '…' : t('settings.notChecked')}</span>
        )}
      </div>

      <div className="grid gap-2 sm:grid-cols-2">
        <label className="text-[12px]">
          {t('settings.images.baseUrl')}
          <input
            className="input mono mt-1"
            value={images.baseUrl ?? ''}
            onChange={(e) => update({ baseUrl: e.target.value })}
            placeholder="http://127.0.0.1:7860"
          />
        </label>
        <label className="text-[12px]" title={t('settings.images.keyHint')}>
          {t('settings.images.key')}
          <input
            className="input mono mt-1"
            type="password"
            value={images.apiKey ?? ''}
            onChange={(e) => update({ apiKey: e.target.value })}
            placeholder={status?.keySet ? '••••••••' : 'user:pass'}
          />
        </label>
        <label className="text-[12px]">
          {t('settings.images.model')}
          <select
            className="input mt-1"
            value={images.model ?? ''}
            onChange={(e) => update({ model: e.target.value })}
          >
            <option value="">{t('settings.images.auto')}</option>
            {(status?.models ?? []).map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[12px]">
          {t('settings.images.sampler')}
          <select
            className="input mt-1"
            value={images.sampler ?? ''}
            onChange={(e) => update({ sampler: e.target.value })}
          >
            <option value="">{t('settings.images.auto')}</option>
            {(status?.samplers ?? []).map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[12px]">
          {t('settings.images.steps')}
          <input
            className="input mt-1"
            type="number"
            min={0}
            max={150}
            value={images.steps || ''}
            onChange={(e) => update({ steps: Number(e.target.value) || 0 })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[12px]">
          {t('settings.images.size')}
          <div className="mt-1 flex items-center gap-2">
            <input
              className="input mono"
              type="number"
              min={0}
              max={2048}
              step={8}
              value={images.width || ''}
              onChange={(e) => update({ width: Number(e.target.value) || 0 })}
              placeholder="512"
            />
            <span className="faint">×</span>
            <input
              className="input mono"
              type="number"
              min={0}
              max={2048}
              step={8}
              value={images.height || ''}
              onChange={(e) => update({ height: Number(e.target.value) || 0 })}
              placeholder="512"
            />
          </div>
        </label>
        <label className="text-[12px] sm:col-span-2">
          {t('settings.images.negative')}
          <input
            className="input mt-1"
            value={images.negativePrompt ?? ''}
            onChange={(e) => update({ negativePrompt: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[12px] sm:col-span-2">
          {t('settings.images.saveFolder')}
          <div className="mt-1 flex items-center gap-2">
            <input
              className="input mono flex-1"
              value={images.saveFolder ?? ''}
              onChange={(e) => update({ saveFolder: e.target.value })}
            />
            <button
              type="button"
              className="btn btn-sm"
              onClick={async () => {
                try {
                  const dir = await App.PickSaveFolder()
                  if (dir) update({ saveFolder: dir })
                } catch (err) {
                  useStore.getState().toast('error', String(err))
                }
              }}
            >
              {t('settings.images.pickFolder')}
            </button>
          </div>
        </label>
      </div>

      <div className="mt-3 flex items-center gap-2">
        <button
          type="button"
          className="btn btn-sm"
          disabled={testing || !images.enabled}
          onClick={() => void test()}
        >
          {testing ? t('settings.images.testing') : t('settings.images.test')}
        </button>
        <span className="faint text-[11px]">{t('settings.images.defaults')}</span>
      </div>

      {images.enabled && status?.error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {status.error}
        </div>
      )}
      {images.enabled && status?.ok && status.models.length === 0 && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--warn)' }}>
          {t('settings.images.noModels')}
        </div>
      )}
    </Section>
  )
}

function ModelOverridesTab() {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const draft = useStore((s) => s.settings)
  const [editing, setEditing] = useState<string | null>(null)
  const [value, setValue] = useState('')

  // Only models without a reported window need a manual override.
  const needsOverride = useMemo(() => models.filter((m) => !m.contextLimit), [models])

  return (
    <Section
      title={t('model.overrides')}
      hint={t('model.overrideHint')}
    >
      {needsOverride.length === 0 ? (
        <div className="faint text-[12px]">Alle Modelle melden ihr Kontextfenster.</div>
      ) : (
        <div className="space-y-2">
          {needsOverride.map((m) => (
            <div key={`${m.backend}/${m.id}`} className="flex items-center gap-2">
              <span className="mono min-w-0 flex-1 truncate text-[12px]">
                {m.backend}/{m.id}
              </span>
              {editing === `${m.backend}/${m.id}` ? (
                <>
                  <input
                    className="input w-32"
                    type="number"
                    value={value}
                    onChange={(e) => setValue(e.target.value)}
                    placeholder="32768"
                  />
                  <button
                    type="button"
                    className="btn btn-sm btn-primary"
                    onClick={async () => {
                      await App.SetModelOverride(m.backend, m.id, {
                        contextLimit: Number(value) || 0,
                        capabilities: [],
                      } as llm.ModelOverride)
                      setEditing(null)
                      await useStore.getState().refreshModels(true)
                    }}
                  >
                    {t('settings.save')}
                  </button>
                </>
              ) : (
                <>
                  <span className="badge badge-warn">{t('context.unknown')}</span>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => {
                      setEditing(`${m.backend}/${m.id}`)
                      setValue(String(draft?.modelOverrides?.[`${m.backend}/${m.id}`]?.contextLimit ?? 32768))
                    }}
                  >
                    {t('model.overrides')}
                  </button>
                </>
              )}
            </div>
          ))}
        </div>
      )}
    </Section>
  )
}

/* -------------------------------------------------------------------------
 * System prompt
 * ---------------------------------------------------------------------- */

function PromptTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  // A shipped preset is shown under its translated name, and a shipped variable
  // under its translated description. translatedOr() falls back to the text the
  // backend sent when a key has no entry — that is how a preset the user adds
  // later still shows the name it was given instead of "preset.my-id".
  const presetLabel = (p: config.PromptPreset) => translatedOr(t, `preset.${p.id}`, p.name)
  const variableLabel = (v: main.PromptVariable) => translatedOr(t, v.key, v.description)
  const [variables, setVariables] = useState<main.PromptVariable[]>([])
  const [preview, setPreview] = useState('')
  const [unknown, setUnknown] = useState<string[]>([])
  const [showPreview, setShowPreview] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement | null>(null)

  useEffect(() => {
    void App.GetPromptVariables().then(setVariables).catch(() => undefined)
  }, [])

  // Debounce validation so typing does not spam the backend.
  useEffect(() => {
    const handle = window.setTimeout(() => {
      void App.UnresolvedPromptVars(draft.defaultSystemPrompt).then(setUnknown).catch(() => undefined)
      if (showPreview) {
        void App.PreviewSystemPrompt(draft.defaultSystemPrompt).then(setPreview).catch(() => undefined)
      }
    }, 250)
    return () => window.clearTimeout(handle)
  }, [draft.defaultSystemPrompt, showPreview])

  const insertVariable = (name: string) => {
    const el = textareaRef.current
    const token = `{{${name}}}`
    if (!el) {
      patch({ defaultSystemPrompt: draft.defaultSystemPrompt + token })
      return
    }
    const start = el.selectionStart ?? draft.defaultSystemPrompt.length
    const end = el.selectionEnd ?? start
    const next = draft.defaultSystemPrompt.slice(0, start) + token + draft.defaultSystemPrompt.slice(end)
    patch({ defaultSystemPrompt: next })
    requestAnimationFrame(() => {
      el.focus()
      el.setSelectionRange(start + token.length, start + token.length)
    })
  }

  return (
    <>
      <Section title={t('settings.prompt')} hint={t('settings.promptHint')}>
        <Field label={t('settings.promptPreset')} hint={t('settings.promptPresetHint')}>
          <div className="flex gap-2">
            <select
              className="select"
              value={draft.promptPresets.find((p) => p.prompt === draft.defaultSystemPrompt)?.id ?? ''}
              onChange={(e) => {
                const preset = draft.promptPresets.find((p) => p.id === e.target.value)
                if (preset) patch({ defaultSystemPrompt: preset.prompt })
              }}
            >
              <option value="">–</option>
              {draft.promptPresets.map((p) => (
                <option key={p.id} value={p.id}>
                  {presetLabel(p)}
                </option>
              ))}
            </select>
            <button
              type="button"
              className="btn"
              title={t('settings.promptReload')}
              aria-label={t('settings.promptReload')}
              onClick={() => {
                // Re-read what is stored. Discarding unsaved edits here is what the
                // button means sitting next to the preset list, and the store is
                // updated with it so the rest of the view agrees. This used to fetch
                // the settings and drop the answer, so the button did nothing at all.
                void App.GetSettings()
                  .then((fresh) => {
                    patch(fresh)
                    useStore.setState({ settings: fresh })
                  })
                  .catch((err) => useStore.getState().toast('error', String(err)))
              }}
            >
              ↺
            </button>
          </div>
        </Field>

        <textarea
          ref={textareaRef}
          className="textarea mono text-[12px]"
          rows={18}
          value={draft.defaultSystemPrompt}
          onChange={(e) => patch({ defaultSystemPrompt: e.target.value })}
        />

        {unknown.length > 0 && (
          <div className="mt-2 rounded p-2 text-[12px]" style={{ background: 'color-mix(in srgb, var(--warn) 12%, transparent)', color: 'var(--warn)' }}>
            {t('settings.promptUnknownVars')}: {unknown.map((v) => `{{${v}}}`).join(', ')}
          </div>
        )}

        <div className="mt-3 flex flex-wrap gap-2">
          <button type="button" className="btn btn-sm" onClick={() => setShowPreview((v) => !v)}>
            {t('settings.promptPreview')}
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={async () => {
              const fresh = await App.GetSettings()
              const preset = fresh.promptPresets.find((p) => p.id === 'research')
              patch({
                defaultSystemPrompt: preset?.prompt ?? fresh.defaultSystemPrompt,
              })
            }}
          >
            {t('settings.promptReset')}
          </button>
        </div>

        {showPreview && (
          <pre
            className="mono mt-2 max-h-80 overflow-auto whitespace-pre-wrap rounded p-3 text-[11px]"
            style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
          >
            {preview}
          </pre>
        )}
      </Section>

      <Section title={t('settings.promptVariables')}>
        <div className="space-y-1">
          {variables.map((v) => (
            <div key={v.name} className="flex items-center gap-2 text-[12px]">
              <code className="mono">{`{{${v.name}}}`}</code>
              <span className="faint min-w-0 flex-1 truncate">{variableLabel(v)}</span>
              <button type="button" className="btn btn-sm btn-ghost" onClick={() => insertVariable(v.name)}>
                {t('settings.promptInsert')}
              </button>
            </div>
          ))}
        </div>
      </Section>
    </>
  )
}

/* -------------------------------------------------------------------------
 * Research
 * ---------------------------------------------------------------------- */

function ResearchTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const [key, setKey] = useState('')
  const [status, setStatus] = useState<main.SearchStatus | null>(null)
  const [testing, setTesting] = useState(false)
  const [secretStatus, setSecretStatus] = useState<main.SecretStatus | null>(null)

  const refresh = () => {
    void App.GetSearchStatus().then(setStatus).catch(() => undefined)
    void App.GetSecretStatus().then(setSecretStatus).catch(() => undefined)
  }
  useEffect(refresh, [])

  const search = draft.search

  return (
    <>
      <Section title={t('settings.research')}>
        <Field label={t('search.providers')} hint={t('settings.providerOrderHint')}>
          <div className="flex gap-2">
            {['brave', 'duckduckgo'].map((name) => {
              const enabled = search.providerOrder.includes(name)
              return (
                <button
                  key={name}
                  type="button"
                  className={`btn btn-sm ${enabled ? 'btn-primary' : ''}`}
                  onClick={() => {
                    const order = enabled
                      ? search.providerOrder.filter((p) => p !== name)
                      : [...search.providerOrder, name]
                    patch({ search: { ...search, providerOrder: order } })
                  }}
                >
                  {enabled ? '✓ ' : ''}
                  {name === 'brave' ? t('search.brave') : t('search.duckduckgo')}
                </button>
              )
            })}
          </div>
        </Field>

        <Field
          label={t('settings.braveKey')}
          hint={secretStatus?.braveKeySet ? t('settings.braveKeySet') : t('settings.braveKeyUnset')}
        >
          <div className="flex gap-2">
            <input
              className="input mono"
              type="password"
              value={key}
              placeholder={secretStatus?.braveKeySet ? '••••••••' : 'BSA…'}
              onChange={(e) => setKey(e.target.value)}
            />
            <button
              type="button"
              className="btn"
              disabled={!key.trim()}
              onClick={async () => {
                try {
                  await App.SetBraveKey(key)
                  setKey('')
                  refresh()
                  useStore.getState().toast('info', t('settings.saved'))
                } catch (err) {
                  useStore.getState().toast('error', String(err))
                }
              }}
            >
              {t('settings.braveSave')}
            </button>
            <button
              type="button"
              className="btn"
              disabled={testing}
              onClick={async () => {
                setTesting(true)
                try {
                  const outcome = await App.TestBraveKey(key)
                  useStore
                    .getState()
                    .toast('info', `Brave OK · ${outcome.results?.length ?? 0} Ergebnis(se)`)
                } catch (err) {
                  useStore.getState().toast('error', `Brave: ${String(err)}`)
                } finally {
                  setTesting(false)
                }
              }}
            >
              {testing ? '…' : t('settings.braveTest')}
            </button>
            {secretStatus?.braveKeySet && (
              <button
                type="button"
                className="btn btn-danger"
                onClick={async () => {
                  await App.SetBraveKey('')
                  refresh()
                }}
              >
                {t('settings.braveClear')}
              </button>
            )}
          </div>
        </Field>

        {status && (
          <div className="text-[12px] muted">
            {t('search.quota')}: <strong>{formatNumber(status.braveQueriesThisMonth)}</strong> /{' '}
            {formatNumber(status.braveQuotaEstimate)}
            <div className="faint mt-1">
              {Object.entries(status.providers ?? {})
                .map(([name, ok]) => `${name}: ${ok ? '✓' : '–'}`)
                .join('   ')}
            </div>
          </div>
        )}

        <div className="mt-3 grid grid-cols-2 gap-3">
          <Field label={t('settings.resultCount')}>
            <input
              className="input"
              type="number"
              min="1"
              max="20"
              value={search.resultCount}
              onChange={(e) => patch({ search: { ...search, resultCount: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.cacheMinutes')}>
            <input
              className="input"
              type="number"
              min="0"
              value={search.cacheMinutes}
              onChange={(e) => patch({ search: { ...search, cacheMinutes: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.country')}>
            <input
              className="input"
              value={search.country}
              onChange={(e) => patch({ search: { ...search, country: e.target.value } })}
            />
          </Field>
          <Field label={t('settings.searchLang')}>
            <input
              className="input"
              value={search.searchLang}
              onChange={(e) => patch({ search: { ...search, searchLang: e.target.value } })}
            />
          </Field>
        </div>
      </Section>

      <Section title={t('settings.llmContext')} hint={t('settings.llmContextHint')}>
        <label className="mb-3 flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={search.useLlmContext}
            onChange={(e) => patch({ search: { ...search, useLlmContext: e.target.checked } })}
          />
          {t('settings.llmContextActive')}
        </label>
        <Field
          label={t('settings.llmContextTokens')}
          hint={search.useLlmContext ? t('settings.llmContextTokensHint') : t('settings.llmContextOff')}
        >
          <input
            className="input"
            type="number"
            min="1024"
            max="32768"
            step="1024"
            disabled={!search.useLlmContext}
            value={search.llmContextTokens}
            onChange={(e) => patch({ search: { ...search, llmContextTokens: Number(e.target.value) } })}
          />
        </Field>
      </Section>

      <Section title={t('settings.scraping')}>
        <label className="mb-2 flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={search.scrapeRespectRobots}
            onChange={(e) => patch({ search: { ...search, scrapeRespectRobots: e.target.checked } })}
          />
          {t('settings.respectRobots')}
        </label>
        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={search.scrapeAllowPrivateHosts}
            onChange={(e) => patch({ search: { ...search, scrapeAllowPrivateHosts: e.target.checked } })}
          />
          {t('settings.allowPrivate')}
        </label>
        <p className="faint mt-1 text-[11px]">{t('settings.allowPrivateHint')}</p>
      </Section>
    </>
  )
}

/* -------------------------------------------------------------------------
 * Context and compaction
 * ---------------------------------------------------------------------- */

function ContextTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const compact = draft.compact
  // A model id may contain a slash, so a `backend/model` key would be ambiguous.
  // The pair is encoded instead of concatenated, so the select can carry it as
  // one opaque value.
  const refValue = (backend: string, model: string) => JSON.stringify([backend, model])
  const selectedRef = compact.summaryModel?.model
    ? refValue(compact.summaryModel.backend, compact.summaryModel.model)
    : ''
  const candidates = useMemo(
    () => [...models].sort((a, b) => a.backend.localeCompare(b.backend) || a.id.localeCompare(b.id)),
    [models],
  )
  return (
    <>
      <Section
        title={t('settings.compaction')}
        hint="Kompaktierung ersetzt ältere Nachrichten im Kontext durch eine Zusammenfassung. Die Originale bleiben gespeichert und sichtbar."
      >
        <label className="mb-3 flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={compact.enabled}
            onChange={(e) => patch({ compact: { ...compact, enabled: e.target.checked } })}
          />
          {t('settings.autoCompact')}
        </label>
        <Field label={t('settings.summaryModel')} hint={t('settings.summaryModelHint')}>
          <select
            className="select"
            value={selectedRef}
            onChange={(e) => {
              const [backend, model] = JSON.parse(e.target.value) as [string, string]
              patch({ compact: { ...compact, summaryModel: { backend, model } } })
            }}
          >
            <option value="">{t('settings.summaryModelDefault')}</option>
            {candidates.map((m) => (
              <option key={`${m.backend}/${m.id}`} value={refValue(m.backend, m.id)}>
                {m.id} ({m.backend})
              </option>
            ))}
          </select>
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label={`${t('settings.threshold')} (0–1)`}>
            <input
              className="input"
              type="number"
              step="0.05"
              min="0.1"
              max="0.99"
              value={compact.threshold}
              onChange={(e) => patch({ compact: { ...compact, threshold: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.keepTurns')}>
            <input
              className="input"
              type="number"
              min="1"
              max="50"
              value={compact.keepRecentTurns}
              onChange={(e) => patch({ compact: { ...compact, keepRecentTurns: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.reserve')}>
            <input
              className="input"
              type="number"
              min="0"
              value={compact.reserveForResponse}
              onChange={(e) => patch({ compact: { ...compact, reserveForResponse: Number(e.target.value) } })}
            />
          </Field>
          <Field label={t('settings.maxSummaryTokens')}>
            <input
              className="input"
              type="number"
              min="100"
              value={compact.maxSummaryTokens}
              onChange={(e) => patch({ compact: { ...compact, maxSummaryTokens: Number(e.target.value) } })}
            />
          </Field>
        </div>
      </Section>

      <Section title={t('settings.toolBudget')}>
        <Field label={t('settings.toolBudgetHint')}>
          <input
            className="input"
            type="number"
            min="1000"
            value={draft.toolTokenBudget}
            onChange={(e) => patch({ toolTokenBudget: Number(e.target.value) })}
          />
        </Field>
      </Section>
    </>
  )
}

