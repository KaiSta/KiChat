import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  App,
  defaultManagedModel,
  folderName,
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
  // The stored entry, or what a new one starts as — the same defaults the chat's
  // picker sends when it starts a local model for the first time (see
  // defaultManagedModel), so a model started from either screen is configured alike.
  const [model, setModel] = useState<config.ManagedModel>(() => ({
    ...defaultManagedModel(),
    ...saved,
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
  // What the *running* server is serving, which is not the selection: picking another
  // file moves `model.modelPath` at once, while the process in memory keeps the model
  // it loaded until it is started again. The backend sends this only while a server is
  // up, so empty means "nothing is loaded" rather than "nothing to say".
  const loadedModel = mine?.loadedModel ?? ''

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

  // The second half of a vision model. Not always downloaded beside the first — a folder
  // assembled by hand, or a projector kept on another drive, is what this button is for.
  // An empty field is not a missing answer: it means "the one beside the model", which is
  // what the app looks for itself. The same file dialog as the model above, because an
  // mmproj is a GGUF and nothing else; only the title differs.
  const chooseProjector = async () => {
    try {
      const picked = await App.PickManagedModelFile(t('settings.managed.mmprojPickTitle'))
      if (picked) update({ mmproj: picked })
    } catch (e) {
      setError(String(e))
    }
  }

  // An empty folder is not "no folder": it asks the backend for the folder it
  // would use, which is the first remembered folder that is there when the user
  // picked one, and LM Studio's otherwise. That is what makes the models a user
  // already has show up without anyone configuring anything.
  const modelDir = draft.runtime?.modelDir ?? ''
  // The folders the user has picked, most recently first. The list is part of the
  // draft and the Save above commits it, like every other field here; the folder in
  // use heads it either way, because the backend adds it when a draft arrives without
  // it — a path typed into the box, or a store written before this list existed.
  const remembered = draft.runtime?.modelDirs ?? []
  const rememberFolder = (dir: string) => [
    dir,
    ...remembered.filter((d) => d !== dir),
  ]

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

  // The chips: the folders the user has picked, in the order they picked them, then
  // the places LM Studio keeps models, each path shown once. The picked ones come from
  // the draft's list rather than from the backend's — forgetting one has to drop its
  // chip on the spot, and a list the store has not seen yet (a pick made a moment ago)
  // is already here, read as present because it came out of a folder dialog. Drawing
  // them from the backend instead put a forgotten chip straight back, which reads as
  // the button not working.
  //
  // A path the backend did answer for keeps its reading, which is how a folder on a
  // drive that is not mounted loses its chip instead of offering a click that can only
  // report an error.
  const folderChips = (() => {
    const readings = new Map(folders.map((f) => [f.path, f]))
    const ordered = [...remembered]
    // The folder in use is the one entry the row cannot do without: it is what the
    // field shows and what the list above it scans. It can be missing from the list —
    // a path typed straight into the box, or a store written before there was a list —
    // and then it belongs at the head, where the backend would put it too.
    const active = modelDir.trim()
    if (active && !ordered.includes(active)) ordered.unshift(active)

    const chips: runtime.ModelFolder[] = []
    const seen = new Set<string>()
    const push = (path: string, kind: string) => {
      const dir = path.trim()
      if (!dir || seen.has(dir)) return
      seen.add(dir)
      chips.push(readings.get(dir) ?? { path: dir, kind, exists: true })
    }
    for (const dir of ordered) push(dir, 'custom')
    // LM Studio's own places are detected rather than remembered, so they are shown
    // whether or not they are in anyone's list.
    for (const f of folders) {
      if (f.kind !== 'custom') push(f.path, f.kind)
    }
    return chips.filter((c) => c.exists)
  })()

  const chooseFolder = async () => {
    try {
      const picked = await App.PickModelFolder(t('settings.managed.folderPickTitle'))
      if (!picked) return
      patch({ runtime: { ...draft.runtime, modelDir: picked, modelDirs: rememberFolder(picked) } })
    } catch (e) {
      setError(String(e))
    }
  }

  const useFolder = (dir: string) => patch({ runtime: { ...draft.runtime, modelDir: dir } })

  // Forgetting is not deleting: the folder stays on disk and only leaves this row.
  // It needs to exist because the list is bounded — a path picked once by mistake
  // would otherwise be a chip until eight more picks pushed it off the end.
  const forgetFolder = (dir: string) =>
    patch({ runtime: { ...draft.runtime, modelDirs: remembered.filter((d) => d !== dir) } })

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

  const folderLabel = (kind: string, path: string) =>
    kind === 'custom'
      ? folderName(path)
      : kind === 'lmstudio-configured'
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

      {/* A multimodal GGUF is only half a model: the vision tower lives in a separate
          mmproj file, and llama.cpp refuses every image without it — "image input is not
          supported", as a 500 on the turn, which reads as this app being broken rather
          than as a model that cannot see. The app pairs the file beside the model on its
          own, so the line under this field is the answer to "can this model read an
          image?": a name means yes and nothing to do, the box is for the two halves that
          were downloaded apart, and the warning is the case the app will not guess.

          Only once a model is chosen: a projector on its own is half of nothing, and a
          warning about the file beside a model that does not exist yet is noise on every
          backend the user has not configured. */}
      {model.modelPath && (
        <>
          <div className="mb-1 flex items-center gap-2">
            <input
              className="input mono flex-1"
              placeholder={
                mine?.mmproj && mine.mmprojDetected
                  ? modelLabel(mine.mmproj)
                  : t('settings.managed.mmprojPlaceholder')
              }
              value={model.mmproj ?? ''}
              onChange={(e) => update({ mmproj: e.target.value })}
            />
            <button type="button" className="btn btn-sm" onClick={() => void chooseProjector()}>
              {t('settings.managed.choose')}
            </button>
          </div>
          <div className="mb-2 text-[11px]">
            {model.mmproj ? (
              <span className="faint">{t('settings.managed.mmprojTyped')}</span>
            ) : mine?.mmproj ? (
              <span style={{ color: 'var(--accent)' }}>
                ✓ {t('settings.managed.mmprojPaired', { name: modelLabel(mine.mmproj) })}
              </span>
            ) : (
              // A sentence rather than a badge: this one names the file the model needs
              // and what happens without it, and a nowrap badge cut it off at the edge of
              // the card — which is how the meanest of the three states ends up unread.
              <span className="block" style={{ color: 'var(--warn)' }}>
                {t('settings.managed.mmprojMissing')}
              </span>
            )}
          </div>
        </>
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
        {folderChips.length > 0 && (
          <div className="mb-1 flex flex-wrap gap-1">
            {folderChips.map((f) => (
              <span key={f.path} className="inline-flex items-center">
                <button
                  type="button"
                  className={`btn btn-sm ${f.path === modelDir ? '' : 'btn-ghost'}`}
                  title={f.path}
                  onClick={() => useFolder(f.path)}
                >
                  {folderLabel(f.kind, f.path)}
                </button>
                {/* Only the folders the user picked can be forgotten: LM Studio's
                    own are detected, not stored, so dropping one would only make it
                    come back on the next refresh. */}
                {f.kind === 'custom' && (
                  <button
                    type="button"
                    className="btn btn-sm btn-ghost"
                    title={`${t('settings.managed.folderForget')}: ${f.path}`}
                    aria-label={`${t('settings.managed.folderForget')}: ${f.path}`}
                    onClick={() => forgetFolder(f.path)}
                  >
                    ×
                  </button>
                )}
              </span>
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
                  {/* The mark is about the *server*, not the selection: this is the
                      file a running llama-server has in memory, so the list answers
                      "what is in use" beside "what is on disk". It appears while the
                      model is still loading too, because that is the moment a user
                      waiting on it looks here. */}
                  {file.path === loadedModel && (
                    <span className={`badge shrink-0 ${state === 'starting' ? '' : 'badge-ok'}`}>
                      {t(
                        state === 'starting'
                          ? 'settings.managed.folderLoading'
                          : 'settings.managed.folderLoaded',
                      )}
                    </span>
                  )}
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

      {/* A running server is not reconfigured by a pick: the model in memory stays
          loaded until the server is started again, which is why the listing above
          marks the loaded file rather than the selected one. Naming that file is the
          difference between a mark that looks wrong and one that reads as "this is
          still what is answering" — the header line above it is already showing the
          selection, so without this the two look like a contradiction. */}
      {(state === 'running' || state === 'starting') && loadedModel && loadedModel !== model.modelPath && (
        <div className="mt-1 text-[11px] faint">
          {t('settings.managed.loadedOther', { model: folderName(loadedModel) })}
        </div>
      )}

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
                backend.kind === 'groq' ||
                backend.kind === 'llamacpp') && (
                <input
                  className="input mono mt-2"
                  type="password"
                  value={backend.apiKey ?? ''}
                  onChange={(e) => updateBackend(index, { apiKey: e.target.value })}
                  placeholder={
                    backend.kind === 'deepseek' ||
                    backend.kind === 'mistral' ||
                    backend.kind === 'nvidia' ||
                    backend.kind === 'groq'
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

      <ModelOverridesTab
        overrides={draft.modelOverrides ?? {}}
        backends={draft.backends}
        onSaved={(key, ov) => {
          // The tab writes through the backend, not through this form, so its result
          // has to reach the draft by hand: otherwise the next Save of an unrelated
          // setting would put back the map that predates the override. The backend
          // keeps its own copy as well (SaveSettings re-reads it), so this is what the
          // panel shows, not the only thing that survives.
          const next = { ...(draft.modelOverrides ?? {}) }
          if (!ov.contextLimit && !ov.requestCeiling && !(ov.capabilities?.length ?? 0)) {
            delete next[key]
          } else {
            next[key] = ov
          }
          patch({ modelOverrides: next })
        }}
      />
    </>
  )
}

/** The file filters the CLI backend's pickers offer. Two extensions plus `*` for a build
 * called something else; an executable has no extension at all, which is why the binary's
 * own picker passes an empty list instead. */
const MODEL_PATTERNS = ['*.gguf', '*.safetensors', '*']

/**
 * The square sizes diffusion work actually starts from.
 *
 * Three rather than a picker of everything: these are the widths the models in the
 * wild were trained at or near, and the point of them is to spare a user typing the
 * same number into two boxes. Anything else is still typed, so this adds a shortcut
 * and takes nothing away.
 */
const SIZE_PRESETS = [512, 768, 1024]

// The second-pass bounds, mirrored from internal/images/limits.go. They are not bridge
// constants — Go's exported names do not reach JavaScript — so the panel states its own
// and the backend clamps again when the setting is saved.
const IMAGE_HIRES_LIMITS = {
  minScale: 1.25,
  maxScale: 2,
  defaultScale: 1.5,
  minDenoise: 0.2,
  maxDenoise: 0.6,
  defaultDenoise: 0.4,
}

/**
 * Fills one CLI path from the native file dialog.
 *
 * A cancelled dialog answers with an empty string, and that must leave the field alone: a
 * user who opened the picker to look around would otherwise lose the path that was already
 * there.
 */
async function pickFile(
  update: (changes: Partial<config.ImageSettings>) => void,
  title: string,
  patterns: string[],
  field: keyof config.ImageSettings,
) {
  try {
    const path = await App.PickImageFile(title, patterns)
    if (path) update({ [field]: path } as Partial<config.ImageSettings>)
  } catch (err) {
    useStore.getState().toast('error', String(err))
  }
}

/**
 * Fills the program folder from the native folder dialog.
 *
 * A folder, not a file: that is the shape a stable-diffusion.cpp download arrives in,
 * and KiChat finds the program inside it. The binding checks the choice before
 * answering, so a folder with no `sd-cli` in it comes back as an error — which is why
 * this reports one instead of quietly storing a folder nothing will be drawn with.
 *
 * A cancelled dialog answers with an empty string and must leave the field alone, the
 * same rule the file picker follows.
 */
async function pickImageFolder(
  update: (changes: Partial<config.ImageSettings>) => void,
  title: string,
) {
  try {
    const dir = await App.PickImageFolder(title)
    if (dir) update({ cliFolder: dir })
  } catch (err) {
    useStore.getState().toast('error', String(err))
  }
}

/**
 * One path in the CLI backend's command line: a labelled field with a browse button.
 *
 * The eight of them are the same control with different text, and writing them out eight
 * times would mean eight chances to forget the button or the hint.
 *
 * `disabled` and `note` exist for the one field whose value can be beside the point: the
 * explicit program path, which the backend ignores outright while a program folder is set.
 * It is drawn dimmed rather than hidden, because a stored path that vanishes from the panel
 * is a path its owner cannot clear — and the sentence that has to be read without hovering
 * anything is the `note`, since `hint` is a tooltip.
 */
function PathField({
  label,
  hint,
  value,
  placeholder,
  onChange,
  onPick,
  disabled,
  note,
}: {
  label: string
  hint: string
  value: string
  placeholder?: string
  onChange: (value: string) => void
  onPick: () => void
  disabled?: boolean
  note?: string
}) {
  const t = useStore((s) => s.t)
  return (
    <label className="text-[12px]" title={hint}>
      {label}
      <div className="mt-1 flex items-center gap-2">
        <input
          className="input mono flex-1"
          value={value}
          placeholder={placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
        <button type="button" className="btn btn-sm" disabled={disabled} onClick={onPick}>
          {t('settings.images.browse')}
        </button>
      </div>
      {note ? <span className="faint mt-1 block text-[11px]">{note}</span> : null}
    </label>
  )
}

/**
 * The picture backend, on a tab of its own.
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
  // Set by the Stop button beside the test, and read by the rejection it causes: a stopped
  // test rejects like a failed one, and only this flag can tell the two apart.
  const stoppingTest = useRef(false)
  const images = draft.images ?? ({} as config.ImageSettings)
  // The kind key is stored empty for the server backend, so the selector's two values are
  // this string and the empty one. It is read once here rather than compared at every field:
  // which half of the form is on screen has to be one answer.
  const isCLI = images.kind === 'cli'
  // A program folder is a statement about a *place*, and the backend lets it win outright
  // (images.ResolveProgram): the typed program path is the fallback for an install that has
  // no folder at all. Two fields that both look required are exactly what reads as "seems
  // double", so the panel says which of the two is supplying the executable.
  const folderSet = (images.cliFolder ?? '').trim() !== ''
  // The program the folder resolved to, and only while it still describes the folder in the
  // draft: `status` answers for the *saved* settings, so a path printed under a folder the
  // user has just finished typing would be a reading of some other folder.
  const foundProgram =
    folderSet && status?.folder === images.cliFolder ? status.binary ?? '' : ''
  // The resolved profile is a reading of the *saved* settings, like the found program: it
  // comes from the diffusion model's own header — its tensor layout — or, when that cannot
  // be read, from its file name. Printing it under a path the user is still typing would
  // describe a different model, so it is shown only while the setting is automatic and the
  // saved file list really holds the draft's path.
  const resolvedPipeline =
    !images.pipeline &&
    status?.files?.includes((images.cliDiffusionModel ?? '').trim()) &&
    status.pipeline &&
    status.pipeline !== 'generic'
      ? { name: status.pipelineName ?? '', source: status.pipelineSource ?? '' }
      : null
  // The third kind: the *same* local build, run as a server KiChat starts and stops. It is
  // not a variant of the one-shot kind in the UI either — the fields are shared because the
  // flags are, but the process, the port and the lifetime are the server's own.
  const isSD = images.kind === 'sdserver'
  // Both local kinds are one build from one archive in two lifetimes, which is what makes the
  // folder and the whole model set *shared* rather than duplicated per kind: the same fields
  // feed `sd-cli -p …` and `sd-server --listen-port …`.
  const isLocal = isCLI || isSD
  // Which executable the folder is expected to hold. The backend reports it rather than the
  // view hard-coding one of the two names, so a renamed row cannot describe the other
  // program.
  const program = status?.program || (isSD ? 'sd-server' : 'sd-cli')
  const server = status?.server
  const sdState = server?.state || 'stopped'
  const sdRunning = isSD && sdState === 'running'
  const sdFailed = isSD && sdState === 'failed'
  // What an empty field means, for the one field where the backend decides: the checkpoint.
  // An empty sampler, size, step count or negative prompt does *not* mean this — the model
  // decides those, and the shared fields below say so themselves.
  const auto = t('settings.images.auto')

  // The builds stable-diffusion.cpp publishes for this machine, and which one the panel
  // is pointing the user at. KiChat does not download the program, so these rows are the
  // whole of how a user is told which archive to take: the project's own list, filtered
  // to this platform by the backend — a CUDA archive is published for Windows and not
  // for Linux, and offering one would send them to a file that does not exist.
  const builds = status?.builds ?? []
  const [sdBuild, setSdBuild] = useState('')
  const [serverBusy, setServerBusy] = useState(false)
  // What is already installed is the useful default — "you are on the Vulkan build, here
  // is the file it came from" — and the first published build otherwise, which is the
  // CPU one and runs anywhere.
  const chosenBuild =
    builds.find((b) => b.id === sdBuild) ??
    builds.find((b) => b.id === (status?.build || 'cpu')) ??
    builds[0]

  const update = (changes: Partial<config.ImageSettings>) =>
    patch({ images: { ...images, ...changes } as config.ImageSettings })

  // Read again without the mount's "checking" dance: the server row polls this while a
  // model loads, and a spinner that reappears every 1.2 s would be noise rather than a
  // reading.
  const readStatus = async () => {
    try {
      setStatus(await App.GetImageStatus())
    } catch (err) {
      useStore.getState().toast('error', String(err))
    }
  }

  /*
   * The managed server's two buttons. Both commit the draft first, for the reason the
   * connection test does: the settings are what a start reads, so starting an unsaved
   * configuration would load the previous model and report a failure about something the
   * user already corrected.
   *
   * Start doubles as Restart — the backend has one call for both, because sd-server loads
   * one pipeline at startup and has no way to be told about another. The label says which of
   * the two the click will be, and both are the user's own click: nothing in this app
   * reloads a model that may have taken minutes to load.
   */
  const startServer = async () => {
    setServerBusy(true)
    try {
      await onApply({ ...draft, images })
      await App.StartImageServer()
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setServerBusy(false)
      await readStatus()
    }
  }

  const stopServer = async () => {
    setServerBusy(true)
    try {
      await App.StopImageServer()
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setServerBusy(false)
      await readStatus()
    }
  }

  // Only while something is moving. A server loading a model takes minutes and the row has to
  // follow it, while an idle window must not ask for a status twice a second for the rest of
  // the session.
  useEffect(() => {
    if (sdState !== 'starting') return
    const id = window.setInterval(() => void readStatus(), 1200)
    return () => window.clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sdState])

  // A save can leave the running server loading a model the settings no longer describe, and
  // "restart needed" is a *reading* the backend answers rather than a field the panel keeps —
  // so the row has to ask again after one. Without this, the badge for the very edit that
  // creates the condition would only appear once the section was left and re-entered.
  //
  // Keyed on the stored command line rather than on the draft (a keystroke is not a save), and
  // only while a server is up: a stopped one reads these settings when it starts, so there is
  // nothing to ask about and no reason to probe a backend the user may not be running.
  const storedImages = useStore((s) => s.settings?.images)
  const storedStartup = storedImages ? startupKey(storedImages) : ''
  useEffect(() => {
    if (sdState !== 'running') return
    void readStatus()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [storedStartup])

  /*
   * Profiles: the saved configurations, one per checkpoint or per pipeline a user
   * switches between. They are part of the settings blob, so every one of the three
   * buttons below commits through onApply instead of only writing into the draft — a
   * profile that disappeared on the way to another tab is the opposite of what a
   * "save" button promises, and the point of the list is to make switching a single
   * click.
   */
  const profiles = draft.imageProfiles ?? []
  const [picked, setPicked] = useState('')
  const [profileName, setProfileName] = useState('')
  // Deleting a configuration is not undoable, so it asks twice, the way the picture
  // store's clear button does.
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [profileBusy, setProfileBusy] = useState(false)
  const trimmedName = profileName.trim()
  // The button says which of the two it will do, because "save" and "update" are the
  // same click on the same form and only the name decides which one the user gets.
  const replacing = profiles.some((p) => p.name === trimmedName)
  const selected = profiles.find((p) => p.name === picked)

  /*
   * What a profile holds, and what it deliberately leaves behind: the drawing
   * configuration, not the feature switch (applying one must not be able to turn image
   * generation off), not the export folder (that is where the user keeps pictures, not
   * how they are made) and not the credential. The server's "user:pass" belongs to the
   * server a profile is a way of using, so it stays with the feature as one keyring
   * entry — which is also what keeps a secret out of the settings blob, where a copy of
   * it would sit in plain text.
   */
  const profileSettings = (): config.ImageSettings =>
    ({ ...images, enabled: false, saveFolder: '', apiKey: '' }) as config.ImageSettings

  const upsertProfile = (name: string): config.ImageProfile[] => {
    const at = profiles.findIndex((p) => p.name === name)
    if (at < 0) return [...profiles, { name, settings: profileSettings() }]
    const next = profiles.slice()
    // Written back in place: a row that jumped to the end of the list on every update
    // would be a dropdown reordering itself under the cursor.
    next[at] = { ...next[at], settings: profileSettings() }
    return next
  }

  const saveProfile = async () => {
    if (!trimmedName) return
    const next = upsertProfile(trimmedName)
    setProfileBusy(true)
    try {
      patch({ imageProfiles: next })
      // Handed in rather than read from state: setState has not landed yet, which is
      // the same reason the connection test passes its payload along.
      await onApply({ ...draft, images, imageProfiles: next })
      setPicked(trimmedName)
    } finally {
      setProfileBusy(false)
    }
  }

  /*
   * Whether applying would have to reload the model: the managed server loads one pipeline when
   * it starts and cannot be told about another, so a profile that names a different model set is
   * a different *process*. The comparison is over the fields a start reads (startupKey) and the
   * label of the button says so before the click, because a reload is minutes — and the click
   * performs it, because picking a model is how a user asks to draw with it.
   */
  const applyRestarts =
    sdRunning &&
    !!selected &&
    // The profile has to *be* the managed server as well. One that switches the kind away
    // leaves the running process alone — it keeps the model it loaded, and the row above is
    // still how it is stopped — so asking for a start here would only come back with "the
    // picture generation is not set to the local server", about a choice the user just made.
    selected.settings.kind === 'sdserver' &&
    startupKey(selected.settings) !== startupKey(images)

  const applyProfile = async () => {
    if (!selected) return
    setProfileBusy(true)
    try {
      // The three fields the profile does not describe come from the draft, so
      // switching configurations never switches the feature off or moves the export
      // folder.
      const next = {
        ...selected.settings,
        enabled: images.enabled,
        saveFolder: images.saveFolder,
        apiKey: images.apiKey,
      } as config.ImageSettings
      patch({ images: next })
      await onApply({ ...draft, images: next })
      if (applyRestarts) {
        // After the save: the settings are what a start reads, so restarting first would
        // load the model the user just moved away from.
        await App.StartImageServer()
        await readStatus()
      }
    } catch (err) {
      useStore.getState().toast('error', String(err))
    } finally {
      setProfileBusy(false)
    }
  }

  const deleteProfile = async () => {
    const name = picked
    const next = profiles.filter((p) => p.name !== name)
    setProfileBusy(true)
    try {
      patch({ imageProfiles: next })
      await onApply({ ...draft, images, imageProfiles: next })
      setPicked('')
      setProfileName('')
    } finally {
      setProfileBusy(false)
      setConfirmDelete(false)
    }
  }

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
    stoppingTest.current = false
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
      // A stop is not a failure: the click that ended it is the user's own, and
      // reporting it in red would describe a working server as a broken one. Same
      // split as the picture card's Stop.
      if (stoppingTest.current) {
        useStore.getState().toast('info', t('images.drawStopped'))
      } else {
        useStore.getState().toast('error', String(err))
      }
      // The row is re-read even so, because what a failed generation needs to be
      // read against is whether the address answers at all: a WebUI that is
      // running with nothing loaded is a different repair from one that is not up.
      try {
        setStatus(await App.GetImageStatus())
      } catch (statusErr) {
        useStore.getState().toast('error', String(statusErr))
      }
    } finally {
      stoppingTest.current = false
      setTesting(false)
    }
  }

  /* Stops a test draw. The button says "test" and looks like a listing, but on the one-shot
   * kind it is a *model load* followed by a draw, and the bound behind it is that
   * backend's own — up to half an hour — so without this the panel would sit on a running
   * button with nothing to press. The id is the fixed one the backend registers the test
   * under (`image-test` in api.go), since a test has no attachment to name it by. Nothing is
   * stored either way: the picture is discarded when it arrives, so a stopped test leaves
   * the settings exactly as they were. */
  const stopTest = async () => {
    stoppingTest.current = true
    try {
      const result = await App.StopImageDraw('image-test')
      // Nothing was running — a second click, or a draw that finished between the render
      // and the click. Clearing the flag is what keeps the next rejection reported as the
      // failure it is, and nothing is toasted for a stop that did not happen.
      if (!result.wasDrawing) stoppingTest.current = false
    } catch (err) {
      stoppingTest.current = false
      useStore.getState().toast('error', String(err))
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
        ) : isSD ? (
          /* The managed server is not probed as an address: it may simply be stopped, which
             is a state and not a failure, and the row below carries the same word plus the
             button that changes it. */
          <span
            className={`badge ${sdRunning && status?.ok ? 'badge-ok' : sdFailed ? 'badge-danger' : ''}`}
            title={server?.error || undefined}
          >
            {sdRunning && status?.ok
              ? `${t('settings.images.ok')} · ${status.models.length} ${t('settings.images.models')}`
              : t(`settings.managed.state.${sdState}`)}
          </span>
        ) : status?.ok ? (
          // The CLI backend has no listing to count, so its badge counts the files it will
          // load instead. Reporting "0 models" on a configured backend would read as a
          // broken setup rather than as a question this backend cannot be asked.
          isCLI ? (
            <span className="badge badge-ok">
              {t('settings.images.ok')} · {status.files.length} {t('settings.images.files')}
            </span>
          ) : (
            <span className={`badge ${status.models.length > 0 ? 'badge-ok' : 'badge-warn'}`}>
              {t('settings.images.ok')} · {status.models.length} {t('settings.images.models')}
            </span>
          )
        ) : (
          <span className="badge">{checking ? '…' : t('settings.notChecked')}</span>
        )}
      </div>
      {!images.enabled ? (
        /* The sentence the rest of this panel depends on: with the feature off there is no
           tool for the model to call and every field below is inert, which a badge alone
           does not say to somebody who has just finished filling them in. */
        <p className="faint mb-2 text-[11px]">{t('settings.images.offHint')}</p>
      ) : null}

      {/* Profiles come before the fields rather than after them: picking one is how a user
          starts, and the row underneath is what the pick applies. Their own save commits
          at once — see saveProfile — so the list is never only in the draft. */}
      <div className="mb-3 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
        <div className="text-[12px]">{t('settings.images.profiles')}</div>
        <p className="faint mb-2 text-[11px]">{t('settings.images.profilesHint')}</p>
        <div className="flex flex-wrap items-center gap-2">
          <select
            className="input"
            aria-label={t('settings.images.profilePick')}
            title={t('settings.images.profilePick')}
            value={picked}
            onChange={(e) => {
              setPicked(e.target.value)
              // Picking also fills the name box, so the same pick is how a stored
              // configuration is corrected: pick it, change a field, press update.
              setProfileName(e.target.value)
              setConfirmDelete(false)
            }}
          >
            <option value="">{t('settings.images.profileNone')}</option>
            {profiles.map((p) => (
              <option key={p.name} value={p.name}>
                {p.name}
              </option>
            ))}
          </select>
          <button
            type="button"
            className="btn btn-sm"
            disabled={!selected || profileBusy}
            onClick={() => void applyProfile()}
          >
            {profileBusy
              ? t('settings.images.profileBusy')
              : applyRestarts
                ? t('settings.images.profileApplyRestart')
                : t('settings.images.profileApply')}
          </button>
          {confirmDelete ? (
            <>
              <span className="text-[11px]" style={{ color: 'var(--danger)' }}>
                {t('settings.images.profileDeleteConfirm', { name: picked })}
              </span>
              <button
                type="button"
                className="btn btn-sm btn-danger"
                disabled={profileBusy}
                onClick={() => void deleteProfile()}
              >
                {t('settings.images.profileDeleteYes')}
              </button>
              <button
                type="button"
                className="btn btn-sm"
                disabled={profileBusy}
                onClick={() => setConfirmDelete(false)}
              >
                {t('common.cancel')}
              </button>
            </>
          ) : (
            <button
              type="button"
              className="btn btn-sm"
              disabled={!selected || profileBusy}
              onClick={() => setConfirmDelete(true)}
            >
              {t('settings.images.profileDelete')}
            </button>
          )}
        </div>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <input
            className="input"
            value={profileName}
            placeholder={t('settings.images.profileName')}
            aria-label={t('settings.images.profileName')}
            onChange={(e) => setProfileName(e.target.value)}
          />
          <button
            type="button"
            className="btn btn-sm"
            disabled={!trimmedName || profileBusy}
            onClick={() => void saveProfile()}
          >
            {replacing ? t('settings.images.profileUpdate') : t('settings.images.profileSave')}
          </button>
          {/* Said rather than implied: the credential is not part of a profile, and a user
              who filled the field in below and then stored a profile would otherwise have
              to guess whether it travelled with it. */}
          <span className="faint text-[11px]">{t('settings.images.profileKeyHint')}</span>
        </div>
      </div>

      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-[12px]">{t('settings.images.kind')}</span>
        <select
          className="input"
          aria-label={t('settings.images.kind')}
          value={images.kind ?? ''}
          onChange={(e) => update({ kind: e.target.value })}
        >
          <option value="">{t('settings.images.kindServer')}</option>
          <option value="cli">{t('settings.images.kindCLI')}</option>
          <option value="sdserver">{t('settings.images.kindSD')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {isSD
            ? t('settings.images.kindSDHint')
            : isCLI
              ? t('settings.images.kindCLIHint')
              : t('settings.images.kindServerHint')}
        </span>
      </div>

      {/* The pipeline of the configured model set. It is a profile rather than a list of
          flags: steps, guidance, whether a negative prompt means anything, whether a second
          pass was ever measured, and whether the model wants a caption all travel with it.
          Automatic reads the diffusion model's own header, falling back to its file name only
          when that cannot be read, and the resolved name is shown here because a miss is
          otherwise silent — a generic profile captions nothing and draws at the build's
          defaults. */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-[12px]">{t('settings.images.pipeline')}</span>
        <select
          className="input"
          aria-label={t('settings.images.pipeline')}
          value={images.pipeline ?? ''}
          onChange={(e) => update({ pipeline: e.target.value })}
        >
          <option value="">{t('settings.images.pipelineAuto')}</option>
          <option value="flux3-klein">{t('settings.images.pipelineFlux3')}</option>
          <option value="flux3-klein-base">{t('settings.images.pipelineFlux3Base')}</option>
          <option value="generic">{t('settings.images.pipelineGeneric')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {images.pipeline === 'generic'
            ? t('settings.images.pipelineGenericHint')
            : images.pipeline !== ''
              ? t('settings.images.pipelineChosen')
              : resolvedPipeline
                ? // Which evidence resolved it, because the two are different claims: the
                  // tensors are the model's own, the file name is a label somebody chose.
                  t(
                    resolvedPipeline.source === 'weights'
                      ? 'settings.images.pipelineResolvedWeights'
                      : 'settings.images.pipelineResolved',
                    { name: resolvedPipeline.name },
                  )
                : t('settings.images.pipelineHint')}
        </span>
      </div>

      {/* Who writes the caption a caption-trained model is drawn from. Empty is "the
          pipeline decides", which is how a FLUX 3 model gets one without the user finding
          this row; "off" is the explicit no that overrides it. */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="text-[12px]">{t('settings.images.captioning')}</span>
        <select
          className="input"
          aria-label={t('settings.images.captioning')}
          value={images.captioning ?? ''}
          onChange={(e) => update({ captioning: e.target.value })}
        >
          <option value="">{t('settings.images.captioningPipeline')}</option>
          <option value="off">{t('settings.images.captioningOff')}</option>
          <option value="auto">{t('settings.images.captioningAuto')}</option>
          <option value="model">{t('settings.images.captioningModel')}</option>
        </select>
        <span className="faint flex-1 text-[11px]">
          {images.captioning === 'off'
            ? t('settings.images.captioningOffHint')
            : images.captioning === 'auto'
              ? t('settings.images.captioningAutoHint')
              : images.captioning === 'model'
                ? t('settings.images.captioningModelHint')
                : t('settings.images.captioningPipelineHint')}
        </span>
      </div>

      {/* The process row: what is running right now, and the one control that starts or
          stops it. It is rendered whenever the kind is the managed server *or* a process is
          actually up, because switching the kind away must not hide a loaded model — a
          server with no row is a multi-gigabyte process with nothing left to stop it. */}
      <ImageServerRow
        images={images}
        server={server}
        isSD={isSD}
        busy={serverBusy}
        onUpdate={update}
        onStart={() => void startServer()}
        onStop={() => void stopServer()}
      />

      <div className="grid gap-2 sm:grid-cols-2">
        {isLocal ? (
          <>
            {/* How stable-diffusion.cpp gets here. KiChat does not download it: the
                project publishes one archive per accelerator, so this panel's job is to
                say which one to take for this machine, then to find the program inside
                whatever folder it was unpacked into. */}
            <PathField
              label={t('settings.images.cliFolder')}
              hint={t('settings.images.cliFolderHint', { program })}
              value={images.cliFolder ?? ''}
              placeholder="/home/user/Downloads/sd"
              onChange={(v) => update({ cliFolder: v })}
              onPick={() => pickImageFolder(update, t('settings.images.cliFolder'))}
            />
            {/* Which archive to download, for this machine. The list is the project's own,
                filtered to this platform by the backend: a CUDA archive is published for
                Windows and not for Linux, and a row for it would send a user to a file
                that does not exist. */}
            {builds.length > 0 ? (
              <div className="text-[12px] sm:col-span-2">
                {t('settings.images.cliBuild')}
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <select
                    className="input"
                    aria-label={t('settings.images.cliBuild')}
                    value={chosenBuild?.id ?? ''}
                    onChange={(e) => setSdBuild(e.target.value)}
                  >
                    {builds.map((b) => (
                      <option key={b.id} value={b.id}>
                        {b.id}
                      </option>
                    ))}
                  </select>
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => {
                      const url = status?.releasePage
                      if (url) void App.OpenExternal(url)
                    }}
                  >
                    {t('settings.images.cliBuildOpen')}
                  </button>
                </div>
                {chosenBuild?.asset ? (
                  <span className="faint mono mt-1 block text-[11px]">{chosenBuild.asset}</span>
                ) : null}
                {chosenBuild?.noteKey ? (
                  <span className="faint mt-1 block text-[11px]">{t(chosenBuild.noteKey)}</span>
                ) : null}
                {/* What is installed, read off the folder's own libraries — which is a
                    different question from the archive the picker above is pointing at,
                    and the one that decides what a draw actually runs. */}
                {status?.binary ? (
                  <span className="faint mt-1 block text-[11px]">
                    {t('settings.images.cliDetected')}:{' '}
                    {status.build || t('settings.images.cliBuildCpu')}
                  </span>
                ) : null}
              </div>
            ) : (
              <span className="faint text-[11px] sm:col-span-2">
                {t('settings.images.cliBuildNone')}
              </span>
            )}
            {/* The fallback, and drawn as one: while a folder is set this field is
                disabled and says why. Not hidden, because the path is still stored and a
                user who typed it has to be able to see it and clear it. */}
            <PathField
              label={t('settings.images.cliBinary')}
              hint={t('settings.images.cliBinaryHint', { program })}
              value={images.cliBinary ?? ''}
              placeholder={program}
              disabled={folderSet}
              note={folderSet ? t('settings.images.cliBinaryIgnored') : undefined}
              onChange={(v) => update({ cliBinary: v })}
              onPick={() => pickFile(update, t('settings.images.cliBinary'), [], 'cliBinary')}
            />
            {/* What the folder resolved to, so the answer to "isn't that path already
                clear?" is on screen rather than in a hover. Empty while the draft's
                folder differs from the one the status describes. */}
            {foundProgram ? (
              <span className="faint mono text-[11px] sm:col-span-2">
                {t('settings.images.cliFound')}: {foundProgram}
              </span>
            ) : null}
            {isSD && !foundProgram ? (
              /* The one setup the server kind needs and the one-shot kind does not: a folder
                 that holds sd-cli but not sd-server is an *older or partial* unpack, and the
                 sentence names the file to look for rather than silently falling back to
                 the other program. */
              <span className="faint text-[11px] sm:col-span-2">
                {t('settings.images.sdProgramHint', { program })}
              </span>
            ) : null}
            {/* Named one per flag, because the flags are the only documentation there is: a
                user with a working command line can copy the paths across one for one, and
                an empty one is simply not passed. */}
            <PathField
              label={t('settings.images.cliModel')}
              hint={t('settings.images.cliModelHint')}
              value={images.cliModel ?? ''}
              placeholder="v1-5-pruned-emaonly.safetensors"
              onChange={(v) => update({ cliModel: v })}
              onPick={() => pickFile(update, t('settings.images.cliModel'), MODEL_PATTERNS, 'cliModel')}
            />
            <PathField
              label={t('settings.images.cliDiffusionModel')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliDiffusionModel ?? ''}
              placeholder="qwen-image-2.1-Q5_K_M.gguf"
              onChange={(v) => update({ cliDiffusionModel: v })}
              onPick={() =>
                pickFile(update, t('settings.images.cliDiffusionModel'), MODEL_PATTERNS, 'cliDiffusionModel')
              }
            />
            <PathField
              label={t('settings.images.cliVae')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliVae ?? ''}
              placeholder="qwen_image_2.1_vae_bf16.safetensors"
              onChange={(v) => update({ cliVae: v })}
              onPick={() => pickFile(update, t('settings.images.cliVae'), MODEL_PATTERNS, 'cliVae')}
            />
            <PathField
              label={t('settings.images.cliLlm')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliLlm ?? ''}
              placeholder="Qwen3-VL-8B-Instruct-Q5_K_M.gguf"
              onChange={(v) => update({ cliLlm: v })}
              onPick={() => pickFile(update, t('settings.images.cliLlm'), MODEL_PATTERNS, 'cliLlm')}
            />
            <PathField
              label={t('settings.images.cliClipL')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliClipL ?? ''}
              onChange={(v) => update({ cliClipL: v })}
              onPick={() => pickFile(update, t('settings.images.cliClipL'), MODEL_PATTERNS, 'cliClipL')}
            />
            <PathField
              label={t('settings.images.cliClipG')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliClipG ?? ''}
              onChange={(v) => update({ cliClipG: v })}
              onPick={() => pickFile(update, t('settings.images.cliClipG'), MODEL_PATTERNS, 'cliClipG')}
            />
            <PathField
              label={t('settings.images.cliT5xxl')}
              hint={t('settings.images.cliPartsHint')}
              value={images.cliT5xxl ?? ''}
              onChange={(v) => update({ cliT5xxl: v })}
              onPick={() => pickFile(update, t('settings.images.cliT5xxl'), MODEL_PATTERNS, 'cliT5xxl')}
            />
            {/* A mode, not a value: Flux on a card too small to hold it is why the switch
                exists, and a user flips it per model rather than per picture. It is sent
                as a bare flag, so nothing here has to agree with a build that spells the
                value differently. */}
            <label
              className="flex items-center gap-2 text-[12px] sm:col-span-2"
              title={t('settings.images.cliOffloadToCpuHint')}
            >
              <input
                type="checkbox"
                checked={images.cliOffloadToCpu ?? false}
                aria-label={t('settings.images.cliOffloadToCpu')}
                onChange={(e) => update({ cliOffloadToCpu: e.target.checked })}
              />
              <span>{t('settings.images.cliOffloadToCpu')}</span>
              <span className="faint flex-1 text-[11px]">
                {t('settings.images.cliOffloadToCpuHint')}
              </span>
            </label>
            {/* Tiling decides how the VAE decodes a picture larger than the card's memory
                can hold at once. It is a bare flag like the offload switch, so it does not
                depend on what the installed build calls the value; a build that lacks the
                flag ignores it rather than failing the draw, and that is stated rather
                than left to be discovered. */}
            <label
              className="flex items-center gap-2 text-[12px] sm:col-span-2"
              title={t('settings.images.cliVaeTilingHint')}
            >
              <input
                type="checkbox"
                checked={images.cliVaeTiling ?? false}
                aria-label={t('settings.images.cliVaeTiling')}
                onChange={(e) => update({ cliVaeTiling: e.target.checked })}
              />
              <span>{t('settings.images.cliVaeTiling')}</span>
              <span className="faint flex-1 text-[11px]">
                {t('settings.images.cliVaeTilingHint')}
              </span>
            </label>
            {images.cliVaeTiling ? (
              <label className="text-[12px] sm:col-span-2">
                {t('settings.images.cliVaeTileSize')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={64}
                  max={2048}
                  step={64}
                  value={images.cliVaeTileSize || ''}
                  onChange={(e) => update({ cliVaeTileSize: Number(e.target.value) || 0 })}
                  placeholder={t('settings.images.auto')}
                />
              </label>
            ) : null}
            <label className="text-[12px] sm:col-span-2" title={t('settings.images.cliExtraHint')}>
              {t('settings.images.cliExtra')}
              <input
                className="input mono mt-1"
                value={images.cliExtraArgs ?? ''}
                onChange={(e) => update({ cliExtraArgs: e.target.value })}
                placeholder="--diffusion-fa --cfg-scale 1.0 --sigmas 1.0,0.5,0.0"
              />
            </label>
          </>
        ) : (
          <>
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
                <option value="">{auto}</option>
                {(status?.models ?? []).map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </label>
          </>
        )}
        {/* The one sentence that says what this whole group does, where it can be read before
            a field is filled in: empty means the model chooses it per picture, and a typed
            value is fixed for every picture — including the ones the model did not ask for. */}
        <p className="faint text-[11px] sm:col-span-2">{t('settings.images.pinHint')}</p>
        <label className="text-[12px]" title={t('settings.images.pinHint')}>
          {t('settings.images.sampler')}
          <select
            className="input mt-1"
            value={images.sampler ?? ''}
            onChange={(e) => update({ sampler: e.target.value })}
          >
            <option value="">{t('settings.images.modelDecides')}</option>
            {(status?.samplers ?? []).map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </label>
        <label className="text-[12px]" title={t('settings.images.pinHint')}>
          {t('settings.images.steps')}
          <input
            className="input mt-1"
            type="number"
            min={0}
            max={150}
            value={images.steps || ''}
            onChange={(e) => update({ steps: Number(e.target.value) || 0 })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        <label className="text-[12px]" title={t('settings.images.cfgHint')}>
          {t('settings.images.cfgScale')}
          <input
            className="input mt-1"
            type="number"
            min={1}
            max={30}
            step={0.5}
            value={images.cfgScale || ''}
            onChange={(e) => update({ cfgScale: Number(e.target.value) || 0 })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        <label className="text-[12px]" title={t('settings.images.pinHint')}>
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
          {/* The trained-at squares, one click each. Setting both numbers is the whole
              of what a preset does, so a size that is not square — or not one of these
              three — is still typed into the boxes above. */}
          <div className="mt-1 flex items-center gap-1">
            {SIZE_PRESETS.map((n) => (
              <button
                key={n}
                type="button"
                className="btn btn-sm"
                aria-label={`${t('settings.images.sizePreset')} ${n}`}
                title={`${t('settings.images.sizePreset')} ${n}×${n}`}
                onClick={() => update({ width: n, height: n })}
              >
                {n}²
              </button>
            ))}
          </div>
        </label>
        <label className="text-[12px] sm:col-span-2" title={t('settings.images.pinHint')}>
          {t('settings.images.negative')}
          <input
            className="input mt-1"
            value={images.negativePrompt ?? ''}
            onChange={(e) => update({ negativePrompt: e.target.value })}
            placeholder={t('settings.images.modelDecides')}
          />
        </label>
        {/* The second sampling pass. It is a fixed setting rather than something the model
            may ask for, because it doubles the time a draw costs and turns a medium
            picture into a large one — a decision for the person paying for the GPU. Its
            two numbers stay stored while the switch is off, so flipping it back restores
            what was chosen instead of resetting it. */}
        <div className="sm:col-span-2">
          <label className="flex items-center gap-2 text-[12px]" title={t('settings.images.hiresHint')}>
            <input
              type="checkbox"
              checked={images.hiresEnabled ?? false}
              aria-label={t('settings.images.hires')}
              onChange={(e) => update({ hiresEnabled: e.target.checked })}
            />
            <span>{t('settings.images.hires')}</span>
            <span className="faint flex-1 text-[11px]">{t('settings.images.hiresHint')}</span>
          </label>
          {images.hiresEnabled ? (
            <div className="mt-2 grid gap-2 sm:grid-cols-2">
              <label className="text-[12px]">
                {t('settings.images.hiresScale')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={IMAGE_HIRES_LIMITS.minScale}
                  max={IMAGE_HIRES_LIMITS.maxScale}
                  step={0.05}
                  value={images.hiresScale || ''}
                  onChange={(e) => update({ hiresScale: Number(e.target.value) || 0 })}
                  placeholder={String(IMAGE_HIRES_LIMITS.defaultScale)}
                />
              </label>
              <label className="text-[12px]" title={t('settings.images.hiresDenoiseHint')}>
                {t('settings.images.hiresDenoise')}
                <input
                  className="input mono mt-1"
                  type="number"
                  min={IMAGE_HIRES_LIMITS.minDenoise}
                  max={IMAGE_HIRES_LIMITS.maxDenoise}
                  step={0.05}
                  value={images.hiresDenoise || ''}
                  onChange={(e) => update({ hiresDenoise: Number(e.target.value) || 0 })}
                  placeholder={String(IMAGE_HIRES_LIMITS.defaultDenoise)}
                />
              </label>
            </div>
          ) : null}
        </div>
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
          disabled={testing || !images.enabled || (isSD && !sdRunning)}
          onClick={() => void test()}
        >
          {testing ? t('settings.images.testing') : t('settings.images.test')}
        </button>
        {/* The test is a real draw, and on the one-shot kind that is a model load before
            anything is drawn — so a Stop belongs beside it rather than only under a
            picture. It appears only while the test runs: at rest there is nothing to
            stop, and a permanently visible button would read as a second action. */}
        {testing ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            title={t('images.stopHint')}
            onClick={() => void stopTest()}
          >
            ⏹ {t('images.stop')}
          </button>
        ) : null}
        {/* What the test actually costs, which is different for all three kinds: a WebUI
            answers in seconds, the one-shot program loads the model from disk first (minutes
            on a CPU), and the managed server is already loaded — unless it is stopped, in
            which case the test is disabled and the Start button next to it is the action. */}
        <span className="faint text-[11px]">
          {isSD
            ? sdRunning
              ? t('settings.images.sdTestHint')
              : t('settings.images.sdStartFirst')
            : isCLI
              ? t('settings.images.cliDefaults')
              : t('settings.images.defaults')}
        </span>
      </div>

      {images.enabled && status?.error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {status.error}
        </div>
      )}
      {images.enabled && !isCLI && status?.ok && status.models.length === 0 && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--warn)' }}>
          {t('settings.images.noModels')}
        </div>
      )}

      <PictureStore />
    </Section>
  )
}

/**
 * The fields a start of the managed server reads: the half of its command line that describes
 * the load.
 *
 * It mirrors `images.CLI.ServerKey` on the backend — the folder, the program, the model set,
 * the offload switch, the user's own arguments and the VAE decode switches — and nothing else.
 * The listener is deliberately not part of it: a port is chosen when the process starts, so
 * including one would make every running server look out of date the moment it came up. This is
 * the only place the frontend knows which fields those are, and it knows them for one question:
 * will applying this profile need a reload?
 */
function startupKey(s: config.ImageSettings): string {
  return JSON.stringify([
    s.kind,
    s.cliFolder,
    s.cliBinary,
    s.cliModel,
    s.cliDiffusionModel,
    s.cliVae,
    s.cliLlm,
    s.cliClipL,
    s.cliClipG,
    s.cliT5xxl,
    s.cliOffloadToCpu,
    s.cliExtraArgs,
    // The VAE decode is part of the load rather than of a request, so turning tiling on —
    // or changing the tile under it — is the same kind of change as a different checkpoint.
    s.cliVaeTiling,
    s.cliVaeTileSize,
  ])
}

/**
 * The managed picture server's process row.
 *
 * It is rendered whenever the kind is the managed server, or when a process is really up —
 * including the state a user reaches by switching the kind away while a model is loaded. That
 * second case is the reason this is not simply part of the server half of the form: a running
 * server with no row is a multi-gigabyte process with nothing left in the window that stops it,
 * which is exactly the state the chat backends' own model switch refuses to leave behind.
 */
function ImageServerRow({
  images,
  server,
  isSD,
  busy,
  onUpdate,
  onStart,
  onStop,
}: {
  images: config.ImageSettings
  server?: main.ImageServerStatus
  isSD: boolean
  busy: boolean
  onUpdate: (changes: Partial<config.ImageSettings>) => void
  onStart: () => void
  onStop: () => void
}) {
  const t = useStore((s) => s.t)
  const state = server?.state || 'stopped'
  const running = state === 'running' || state === 'starting'
  if (!isSD && !running && state !== 'failed') return null

  return (
    <div className="mb-3 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
      <div className="mb-1 text-[12px]">{t('settings.images.sdTitle')}</div>
      <p className="faint mb-2 text-[11px]">{t('settings.images.sdHint')}</p>
      <div className="flex flex-wrap items-center gap-2">
        {/* Start and Restart belong to the kind this section selects. A server left running
            under another backend choice is a process the row can only *stop* — starting it
            again is not something this form can ask for, because the command line a start
            builds comes from the settings the other kind replaced. */}
        {isSD ? (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busy}
            title={t('settings.images.sdStartHint')}
            onClick={onStart}
          >
            {state === 'stopped' || state === 'failed'
              ? t('settings.managed.start')
              : t('settings.images.sdRestart')}
          </button>
        ) : null}
        <button type="button" className="btn btn-sm" disabled={busy || state === 'stopped'} onClick={onStop}>
          {t('settings.managed.stop')}
        </button>
        <span className={`badge ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}>
          {t(`settings.managed.state.${state}`)}
        </span>
        {/* The address the process settled on, so a user who wants to look at the server's own
            page, or point another tool at it, does not have to guess which port was free. */}
        {server?.url ? <span className="mono min-w-0 flex-1 truncate text-[11px] faint">{server.url}</span> : null}
        {server?.restartNeeded && isSD ? (
          <span className="badge badge-warn" title={t('settings.images.sdRestartNeededHint')}>
            {t('settings.images.sdRestartNeeded')}
          </span>
        ) : null}
      </div>

      {isSD ? (
        <label className="mt-2 flex items-center gap-2 text-[11px]" title={t('settings.images.sdStartWithAppHint')}>
          <input
            type="checkbox"
            checked={images.startWithApp ?? false}
            aria-label={t('settings.images.sdStartWithApp')}
            onChange={(e) => onUpdate({ startWithApp: e.target.checked })}
          />
          <span>{t('settings.images.sdStartWithApp')}</span>
          <span className="faint flex-1">{t('settings.images.sdStartWithAppHint')}</span>
        </label>
      ) : null}

      {/* Said where it can be acted on rather than left to be discovered: with the server down
          the model has no generate_image tool at all, so a user who asks for a picture would
          be told by the model that it cannot draw. */}
      {isSD && !running && state !== 'failed' ? (
        <p className="faint mt-1 text-[11px]">{t('settings.images.sdNoTool')}</p>
      ) : null}
      {!isSD && running ? (
        <p className="faint mt-1 text-[11px]">{t('settings.images.sdOtherKind')}</p>
      ) : null}
      {server?.error ? (
        <div className="mt-2 whitespace-pre-wrap text-[11px]" style={{ color: 'var(--danger)' }}>
          {server.error}
        </div>
      ) : null}
      {state === 'failed' && server?.log?.length ? (
        <pre className="faint mono mt-2 max-h-32 overflow-y-auto scroll-thin whitespace-pre-wrap text-[10px]">
          {server.log.join('\n')}
        </pre>
      ) : null}
    </div>
  )
}

/**
 * The pictures KiChat is holding, and the one control that frees them.
 *
 * A drawn picture is an attachment like any other, which means it lives in the app's own
 * database — that is where an unsaved picture "is", and it is what this row answers. The
 * store keeps them for as long as the chat exists, and nothing ever removes them: a long
 * session of drawing quietly grows the data directory, and a user who never presses Save
 * has no way to say they are finished with a picture.
 *
 * The removal is deliberately narrow. It touches neither a picture its owner has a file of
 * (the export note, written by SaveAttachmentAs) nor a picture the *user* attached — the
 * backend tells those apart by the recipe, which only a drawn picture has, so a photo dropped
 * into the composer can never be swept up by a cache button.
 *
 * It also asks twice. "Clear cache" is the kind of label that sounds like it costs a
 * re-download, and what it actually costs is a picture the chat will never show again — so
 * the second press names the count, the size and how many conversations lose something.
 */
function PictureStore() {
  const t = useStore((s) => s.t)
  const [cache, setCache] = useState<main.ImageCache | null>(null)
  const [busy, setBusy] = useState(false)
  const [confirming, setConfirming] = useState(false)

  const refresh = async () => {
    try {
      setCache(await App.GetImageCache())
    } catch (err) {
      useStore.getState().toast('error', String(err))
    }
  }

  // Read once when the panel opens, like the backend probe above: the count is what the
  // store holds right now, and re-reading it on every keystroke in an unrelated field would
  // be a database walk per character.
  useEffect(() => {
    void refresh()
  }, [])

  const clear = async () => {
    setBusy(true)
    try {
      const result = await App.ClearImageCache()
      useStore
        .getState()
        .toast('info', t('settings.images.cacheCleared', { count: result.removed, bytes: formatBytes(result.bytes) }))
    } catch (err) {
      useStore.getState().toast('error', `${t('settings.images.cacheFailed')}: ${String(err)}`)
    } finally {
      setBusy(false)
      setConfirming(false)
      await refresh()
    }
  }

  // Read as an object with numbers defaulted rather than trusted: this renders on a fresh
  // install, where every field is zero, and a missing one must not print "undefined".
  const count = cache?.count ?? 0
  const bytes = cache?.bytes ?? 0
  const unsavedCount = cache?.unsavedCount ?? 0
  const unsavedBytes = cache?.unsavedBytes ?? 0
  const unsavedChats = cache?.unsavedChats ?? 0

  return (
    <div className="mt-4 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
      <div className="mb-1 text-[12px]">{t('settings.images.cache')}</div>
      <p className="faint mb-2 text-[11px]">{t('settings.images.cacheHint')}</p>

      {count === 0 ? (
        <div className="faint text-[11px]">{t('settings.images.cacheEmpty')}</div>
      ) : (
        <>
          <div className="text-[12px]">
            {t('settings.images.cacheCount', { count: formatNumber(count), bytes: formatBytes(bytes) })}
          </div>
          {unsavedCount > 0 ? (
            <div className="text-[12px]" style={{ color: 'var(--warn)' }}>
              {t('settings.images.cacheUnsaved', {
                count: formatNumber(unsavedCount),
                bytes: formatBytes(unsavedBytes),
                chats: formatNumber(unsavedChats),
              })}
            </div>
          ) : (
            <div className="faint text-[12px]">{t('settings.images.cacheAllSaved')}</div>
          )}
        </>
      )}

      {/* The honest limit of the reading, said before the button rather than after the
          loss: this app only knows about a copy it was told about. */}
      <p className="faint mt-1 text-[11px]">{t('settings.images.cacheOlder')}</p>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        {confirming ? (
          <>
            <span className="text-[11px]" style={{ color: 'var(--danger)' }}>
              {t('settings.images.cacheConfirm', {
                count: formatNumber(unsavedCount),
                bytes: formatBytes(unsavedBytes),
                chats: formatNumber(unsavedChats),
              })}
            </span>
            <button
              type="button"
              className="btn btn-sm btn-danger"
              disabled={busy}
              onClick={() => void clear()}
            >
              {t('settings.images.cacheConfirmYes', { count: formatNumber(unsavedCount) })}
            </button>
            <button type="button" className="btn btn-sm" disabled={busy} onClick={() => setConfirming(false)}>
              {t('settings.images.cacheCancel')}
            </button>
          </>
        ) : (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busy || unsavedCount === 0}
            title={t('settings.images.cacheClearHint')}
            onClick={() => setConfirming(true)}
          >
            {t('settings.images.cacheClear')}
          </button>
        )}
      </div>
    </div>
  )
}

function ModelOverridesTab({
  overrides,
  backends,
  onSaved,
}: {
  // The panel's own draft, rather than the store's snapshot: this tab is the one
  // control that writes outside the form, and a row that read the snapshot would show
  // "not set" for every value it had just saved.
  overrides: Record<string, llm.ModelOverride>
  backends: llm.BackendConfig[]
  onSaved: (key: string, ov: llm.ModelOverride) => void
}) {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const [editing, setEditing] = useState<string | null>(null)
  const [value, setValue] = useState('')

  // Groq is the one backend whose per-request budget cannot be fetched before the
  // first request: the listing publishes windows only, the Rate Limits and Models
  // docs pages disagree by 31× for the same models, and the exact figure for an
  // organization is on its Limits page — a page for a person, not for a client. The
  // response header does carry it, but only on an answer, so a chat that is already
  // long has nothing to plan against until it has been refused once. A number typed
  // here is the only source that speaks first, which is why this field exists for
  // this backend alone (GROQ.md §6.2).
  // The kind alone decides it: the model list only ever contains models of backends
  // that are switched on, so asking for `enabled` here would make the field depend on
  // the settings row rather than on what is on screen — and a switched-off backend has
  // no models to show it for anyway.
  const groqBackends = useMemo(
    () => new Set(backends.filter((b) => b.kind === 'groq').map((b) => b.id)),
    [backends],
  )

  // A model needs a row when its window is unknown (the case this tab was built for)
  // or when it belongs to a backend whose request budget nothing publishes.
  const rows = useMemo(
    () => models.filter((m) => !m.contextLimit || groqBackends.has(m.backend)),
    [models, groqBackends],
  )

  const saveOverride = async (
    backend: string,
    id: string,
    patch: { contextLimit?: number; requestCeiling?: number },
  ) => {
    const key = `${backend}/${id}`
    const current = overrides[key]
    // The other field is carried over from the row's own state, so saving the window of
    // a model whose ceiling was saved a moment ago cannot send a 0 and clear it.
    const next = {
      contextLimit: current?.contextLimit ?? 0,
      capabilities: current?.capabilities ?? [],
      requestCeiling: current?.requestCeiling ?? 0,
      ...patch,
    } as llm.ModelOverride
    await App.SetModelOverride(backend, id, next)
    onSaved(key, next)
    setEditing(null)
    // Only the window is part of a model's listed metadata, so only a window save asks
    // for a re-listing — and on a metered backend a listing is a request.
    if (patch.contextLimit !== undefined) {
      await useStore.getState().refreshModels(true)
    }
  }

  return (
    <Section title={t('model.overrides')} hint={t('model.overrideHint')}>
      {rows.length === 0 ? (
        <div className="faint text-[12px]">Alle Modelle melden ihr Kontextfenster.</div>
      ) : (
        <div className="space-y-3">
          {rows.map((m) => {
            const key = `${m.backend}/${m.id}`
            const ceiling = overrides[key]?.requestCeiling ?? 0
            const needsWindow = !m.contextLimit
            const needsCeiling = groqBackends.has(m.backend)
            return (
              <div key={key} className="space-y-1">
                <div className="flex items-center gap-2">
                  <span className="mono min-w-0 flex-1 truncate text-[12px]">
                    {m.backend}/{m.id}
                  </span>
                  {needsWindow ? (
                    editing === `window:${key}` ? (
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
                          onClick={() => saveOverride(m.backend, m.id, { contextLimit: Number(value) || 0 })}
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
                            setEditing(`window:${key}`)
                            setValue(String(overrides[key]?.contextLimit ?? 32768))
                          }}
                        >
                          {t('model.overrides')}
                        </button>
                      </>
                    )
                  ) : null}
                </div>
                {needsCeiling ? (
                  <div className="flex items-center gap-2">
                    <span className="faint min-w-0 flex-1 truncate text-[11px]" title={t('model.ceilingHint')}>
                      {t('model.ceiling')}
                    </span>
                    {editing === `ceiling:${key}` ? (
                      <>
                        <input
                          className="input w-32"
                          type="number"
                          value={value}
                          onChange={(e) => setValue(e.target.value)}
                          placeholder="8000"
                          title={t('model.ceilingHint')}
                        />
                        <button
                          type="button"
                          className="btn btn-sm btn-primary"
                          onClick={() => saveOverride(m.backend, m.id, { requestCeiling: Number(value) || 0 })}
                        >
                          {t('settings.save')}
                        </button>
                      </>
                    ) : (
                      <>
                        <span className="mono text-[11px]">
                          {ceiling > 0 ? formatNumber(ceiling) : t('model.ceilingUnset')}
                        </span>
                        <button
                          type="button"
                          className="btn btn-sm"
                          title={t('model.ceilingHint')}
                          onClick={() => {
                            setEditing(`ceiling:${key}`)
                            // Empty when nothing is set: the placeholder shows what a
                            // typical figure looks like, but an untouched field must
                            // not save a number nobody measured.
                            setValue(ceiling > 0 ? String(ceiling) : '')
                          }}
                        >
                          {t('model.ceiling')}
                        </button>
                      </>
                    )}
                  </div>
                ) : null}
              </div>
            )
          })}
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

