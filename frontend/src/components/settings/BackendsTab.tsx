import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { App, defaultManagedModel, folderName, formatBytes, formatNumber, isAppleSilicon, modelLabel, timeAgo, type config, type llm, type main, type runtime } from '../../lib/api'
import { errText, useStore } from '../../store'
import { ModelPicker, type ModelRef } from '../ModelPicker'
import { Section, Field } from './shared'

function LocalModelSection({
  backend,
  draft,
  patch,
  status,
  onRefresh,
}: {
  backend: llm.BackendConfig
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
  // Shared with the whole tab: one status call serves every summary row and the
  // expanded card, so eight collapsed backends do not each mount a poll.
  status: main.RuntimeStatus | null
  onRefresh: () => Promise<void>
}) {
  const t = useStore((s) => s.t)
  // Keys built from a value the backend reports, rather than written out here.
  const tDynamic = useStore((s) => s.tDynamic)
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
      await onRefresh()
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

  // The tab owns the status poll, but this card still refreshes once when it opens:
  // the installed-build list is its own question, and a model may have been started
  // or an install may have finished while the card was closed.
  useEffect(() => {
    void refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

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

  // Read as an array even though the backend sends one: a nil Go slice marshals to
  // JSON `null`, and a null here threw inside this section's render and took the
  // whole Settings view down to the error boundary. The backend no longer sends it
  // (pinned by runtime_status_test.go), and this is the second line of defence — a
  // shape mistake should cost one row, not the window.
  const mine = (status?.models ?? []).find((m) => m.backendId === backend.id)
  const state = mine?.state ?? 'stopped'
  // What the *running* server is serving, which is not the selection: picking another
  // file moves `model.modelPath` at once, while the process in memory keeps the model
  // it loaded until it is started again. The backend sends this only while a server is
  // up, so empty means "nothing is loaded" rather than "nothing to say".
  const loadedModel = mine?.loadedModel ?? ''

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
    tDynamic(`settings.managed.accel${id.charAt(0).toUpperCase()}${id.slice(1)}`)

  // The release sources the app can install from, straight from the backend for the
  // same reason as the families: a source with no repository behind it must not be
  // offered. The families above belong to the selected source — the fork publishes
  // five of upstream's six — so the two pickers read from the same status.
  const sources = status?.distributions?.length ? status.distributions : ['llamacpp']
  const chosenSource = draft.runtime?.distribution ?? 'llamacpp'
  const sourceOptions = sources.includes(chosenSource) ? sources : [...sources, chosenSource]
  const sourceLabel = (id: string) =>
    tDynamic(`settings.managed.dist${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  const sourceHint = chosenSource === 'prism'
    ? t('settings.managed.distPrismHint')
    : t('settings.managed.distLlamacppHint')

  // Filled from the backend, like the accelerator picker: a value the app cannot
  // pass to llama-server must not be offered here.
  const flashModes = status?.flashAttentionModes?.length ? status.flashAttentionModes : ['auto', 'on', 'off']
  const cacheTypes = status?.cacheTypes?.length ? status.cacheTypes : ['f16']
  const flashLabel = (id: string) =>
    tDynamic(`settings.managed.flash${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  const cacheLabel = (id: string) =>
    tDynamic(`settings.managed.cache${id.charAt(0).toUpperCase()}${id.slice(1)}`)
  // The offload modes have labels in the same shape, but only the words: the modes
  // are this app's own vocabulary for `--gpu-layers` rather than a list llama.cpp
  // reports, so the fallback below is what a status older than this field sends.
  const offloadModes = status?.offloadModes?.length ? status.offloadModes : ['auto', 'all', 'off']
  const offloadLabel = (id: string) =>
    tDynamic(`settings.managed.offload${id.charAt(0).toUpperCase()}${id.slice(1)}`)

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
              title={errText(status.update.error)}
            >
              {errText(status.update.error)}
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
          {tDynamic(`settings.managed.state.${state}`)}
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
          {errText(mine.error)}
        </div>
      )}
      {error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {error}
        </div>
      )}
      {status?.error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {errText(status.error)}
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
 * MLX
 * ---------------------------------------------------------------------- */

/**
 * The managed MLX section: a card whose program is not llama.cpp.
 *
 * It is a sibling of LocalModelSection rather than a branch inside it, because
 * almost nothing it shows has an MLX equivalent: there is no GGUF to scan, no
 * projector, no flash attention and no `--gpu-layers`. What it shares with the
 * llama.cpp card is the shell around it (enable, name, address, state badge) and
 * the supervisor behind it, which is the same one.
 *
 * The one behaviour worth stating out loud: MLX's `/health` answers 200 before the
 * weights are loaded, so "running" means the process answers and the first message
 * pays the load. The note under the buttons says so rather than letting a first
 * reply that takes minutes look like a fault.
 */
function MLXModelSection({
  backend,
  draft,
  patch,
  status,
  onRefresh,
}: {
  backend: llm.BackendConfig
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
  status: main.RuntimeStatus | null
  onRefresh: () => Promise<void>
}) {
  const t = useStore((s) => s.t)
  const tDynamic = useStore((s) => s.tDynamic)
  const [error, setError] = useState('')
  const saved = draft.runtime?.models?.[backend.id]
  const [model, setModel] = useState<config.ManagedModel>(() => ({
    ...defaultManagedModel(),
    ...saved,
  }))
  const mine = (status?.models ?? []).find((m) => m.backendId === backend.id)
  const state = mine?.state ?? 'stopped'
  const mlx = model.mlx ?? {}

  const refresh = async () => {
    try {
      await onRefresh()
      setError('')
    } catch (e) {
      setError(String(e))
    }
  }

  // Ask once when the card opens: the shared status may predate an install or a load
  // that finished while this card was closed.
  useEffect(() => {
    void refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const update = (changes: Partial<config.ManagedModel>) => {
    const next: config.ManagedModel = { ...model, ...changes }
    setModel(next)
    const models = { ...(draft.runtime?.models ?? {}), [backend.id]: next }
    patch({ runtime: { ...draft.runtime, models } })
    return next
  }

  const updateMLX = (changes: Partial<config.MLXOptions>) => update({ mlx: { ...mlx, ...changes } })

  const commit = async (next: config.ManagedModel) => {
    setError('')
    try {
      await App.SetManagedModel(backend.id, next)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  const install = async () => {
    setError('')
    try {
      await App.InstallMLXRuntime()
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  const pick = async () => {
    setError('')
    try {
      const picked = await App.PickMLXModelFolder(t('settings.mlx.pickTitle'))
      if (picked) await commit(update({ modelPath: picked }))
    } catch (e) {
      setError(String(e))
    }
  }

  const start = async () => {
    setError('')
    try {
      const next = update({})
      await App.SetManagedModel(backend.id, next)
      await App.StartManagedModel(backend.id)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  const stop = async () => {
    setError('')
    try {
      await App.StopManagedModel(backend.id)
    } catch (e) {
      setError(String(e))
    }
    await refresh()
  }

  return (
    <div className="mt-3 border-t pt-3" style={{ borderColor: 'var(--line)' }}>
      <div className="mb-2 text-[13px] font-medium">{t('settings.mlx.title')}</div>
      <p className="faint mb-2 text-[11px]">{t('settings.mlx.hint')}</p>

      <div className="mb-2 flex flex-wrap items-center gap-2 text-[11px]">
        <span className={`badge ${status?.mlxInstalled ? 'badge-ok' : 'badge-warn'}`}>
          {status?.mlxServer
            ? t('settings.mlx.customInstalled')
            : status?.mlxInstalled
              ? t('settings.mlx.installed', { version: status?.mlxVersion || t('settings.managed.versionUnknown') })
              : t('settings.mlx.notInstalled')}
        </span>
        {status?.mlxPython && (
          <span className="mono min-w-0 truncate faint">
            {t('settings.mlx.python')}: {status.mlxPython}
          </span>
        )}
      </div>
      {status?.mlxBusy && (
        <div className="mb-2 text-[11px] faint">
          {t('settings.mlx.installing')}
          {status.mlxPhase ? ` · ${t('settings.mlx.phase', { phase: status.mlxPhase })}` : ''}
        </div>
      )}
      {/* A supplied launcher owns its own environment; installing into the app's
          venv would build something the start never runs. The row explains which
          program is in charge instead of offering that dead end. */}
      {!status?.mlxServer && (
        <div className="mb-2 flex flex-wrap items-center gap-2">
          <button type="button" className="btn btn-sm" disabled={!!status?.mlxBusy} onClick={() => void install()}>
            {status?.mlxInstalled ? t('settings.mlx.upgrade') : t('settings.mlx.install')}
          </button>
        </div>
      )}

      {/* A launcher you already have — a conda environment, a checkout — skips the
          venv entirely. It is a runtime-wide setting, so it lives in the draft and is
          persisted by the panel's Save, like llama.cpp's own binary path. */}
      <label className="mb-1 block text-[12px]">{t('settings.mlx.serverPath')}</label>
      <input
        className="input mono"
        value={draft.runtime?.mlx?.serverPath ?? ''}
        placeholder={t('settings.mlx.serverPlaceholder')}
        onChange={(e) =>
          patch({
            runtime: {
              ...draft.runtime,
              mlx: { ...(draft.runtime?.mlx ?? { version: '' }), serverPath: e.target.value },
            },
          })
        }
      />
      <p className="faint mt-1 text-[11px]">{t('settings.mlx.serverHint')}</p>

      <label className="mb-1 block text-[12px]">{t('settings.mlx.model')}</label>
      <div className="flex gap-2">
        <input
          className="input mono"
          value={model.modelPath}
          placeholder={t('settings.mlx.modelPlaceholder')}
          onChange={(e) => update({ modelPath: e.target.value })}
          onBlur={(e) => void commit(update({ modelPath: e.target.value }))}
        />
        <button type="button" className="btn btn-sm shrink-0" onClick={() => void pick()}>
          {t('settings.mlx.pick')}
        </button>
      </div>

      <div className="mt-2 grid grid-cols-2 gap-2">
        <div>
          <label className="mb-1 block text-[12px]">{t('settings.mlx.kv')}</label>
          <select
            className="input"
            value={mlx.kvBits ?? 0}
            onChange={(e) => void commit(updateMLX({ kvBits: Number(e.target.value) }))}
          >
            <option value={0}>{t('settings.mlx.kvOff')}</option>
            {[2, 3, 4, 6, 8].map((bits) => (
              <option key={bits} value={bits}>
                {bits} bit
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="mb-1 block text-[12px]">{t('settings.mlx.prefill')}</label>
          <input
            className="input"
            type="number"
            min={0}
            value={mlx.prefillStep ?? 0}
            onChange={(e) => updateMLX({ prefillStep: Number(e.target.value) })}
            onBlur={(e) => void commit(updateMLX({ prefillStep: Number(e.target.value) }))}
          />
        </div>
        <div>
          <label className="mb-1 block text-[12px]">{t('settings.mlx.maxTokens')}</label>
          <input
            className="input"
            type="number"
            min={0}
            value={mlx.maxTokens ?? 0}
            onChange={(e) => updateMLX({ maxTokens: Number(e.target.value) })}
            onBlur={(e) => void commit(updateMLX({ maxTokens: Number(e.target.value) }))}
          />
        </div>
        <div>
          <label className="mb-1 block text-[12px]">{t('settings.mlx.draft')}</label>
          <input
            className="input mono"
            value={mlx.draftModel ?? ''}
            placeholder={t('settings.mlx.draftPlaceholder')}
            onChange={(e) => updateMLX({ draftModel: e.target.value })}
            onBlur={(e) => void commit(updateMLX({ draftModel: e.target.value }))}
          />
        </div>
      </div>
      <p className="faint mt-1 text-[11px]">{t('settings.mlx.kvHint')}</p>

      <div className="mt-2">
        <label className="mb-1 block text-[12px]">{t('settings.mlx.chatTemplate')}</label>
        <input
          className="input mono"
          value={mlx.chatTemplate ?? ''}
          placeholder={t('settings.mlx.chatTemplatePlaceholder')}
          onChange={(e) => updateMLX({ chatTemplate: e.target.value })}
          onBlur={(e) => void commit(updateMLX({ chatTemplate: e.target.value }))}
        />
      </div>

      <label className="mt-2 flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={mlx.trustRemoteCode ?? false}
          onChange={(e) => void commit(updateMLX({ trustRemoteCode: e.target.checked }))}
        />
        {t('settings.mlx.trust')}
      </label>
      <p className="faint mt-1 text-[11px]">{t('settings.mlx.trustHint')}</p>

      <div className="mt-2">
        <label className="mb-1 block text-[12px]">{t('settings.mlx.extra')}</label>
        <input
          className="input mono"
          value={model.extraArgs}
          onChange={(e) => update({ extraArgs: e.target.value })}
          onBlur={(e) => void commit(update({ extraArgs: e.target.value }))}
        />
      </div>

      {/* The same switch llama.cpp's card has, and for the same reason: a managed
          model starts at launch only when somebody said so. `enabled` here is the
          model entry's, not the backend's — the header checkbox above is whether the
          address is contacted at all. */}
      <label className="mt-2 flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={model.enabled}
          onChange={(e) => void commit(update({ enabled: e.target.checked }))}
        />
        {t('settings.managed.enable')}
      </label>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <button
          type="button"
          className="btn btn-sm"
          disabled={!status?.mlxInstalled || !model.modelPath}
          onClick={() => void start()}
        >
          {t('settings.mlx.start')}
        </button>
        <button type="button" className="btn btn-sm" disabled={state === 'stopped'} onClick={() => void stop()}>
          {t('settings.mlx.stop')}
        </button>
        <span className={`badge ${state === 'running' ? 'badge-ok' : state === 'failed' ? 'badge-danger' : ''}`}>
          {tDynamic(`settings.managed.state.${state}`)}
        </span>
        {mine?.url && <span className="mono min-w-0 flex-1 truncate text-[11px] faint">{mine.url}</span>}
      </div>
      <p className="faint mt-1 text-[11px]">{t('settings.mlx.loadNote')}</p>

      {mine?.error && (
        <div className="mt-2 whitespace-pre-wrap text-[11px]" style={{ color: 'var(--danger)' }}>
          {errText(mine.error)}
        </div>
      )}
      {error && (
        <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
          {error}
        </div>
      )}
      {status?.mlxError && (
        <div className="mt-2 whitespace-pre-wrap text-[11px]" style={{ color: 'var(--danger)' }}>
          {errText(status.mlxError)}
        </div>
      )}
      {state === 'starting' && mine?.log?.length ? (
        <pre className="mt-2 max-h-24 overflow-auto whitespace-pre-wrap text-[10px] faint">{mine.log.join('\n')}</pre>
      ) : null}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Backends
 * ---------------------------------------------------------------------- */

export function BackendsTab({
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
  const tDynamic = useStore((s) => s.tDynamic)
  const health = useStore((s) => s.health)
  const models = useStore((s) => s.models)
  const [secrets, setSecrets] = useState<main.SecretStatus | null>(null)
  // The platform this window is running on. MLX is Apple-silicon only, and its
  // card is not shown anywhere else; the chat's picker asks the same question.
  const [apple, setApple] = useState(false)
  useEffect(() => {
    void App.GetAppInfo()
      .then((info) => setApple(isAppleSilicon(info.platform)))
      .catch(() => undefined)
  }, [])

  // One status call for the whole tab. A collapsed row still has to say whether a
  // managed model is running or failed, and the expanded card reads the same value,
  // so the poll lives here rather than inside every managed section.
  const [runtimeStatus, setRuntimeStatus] = useState<main.RuntimeStatus | null>(null)
  const refreshRuntime = useCallback(async () => {
    setRuntimeStatus(await App.GetRuntimeStatus())
  }, [])
  useEffect(() => {
    void refreshRuntime().catch(() => undefined)
  }, [refreshRuntime])
  const runtimeMoving = useMemo(() => {
    const st = runtimeStatus
    if (!st) return false
    if (st.busy || st.mlxBusy) return true
    return (st.models ?? []).some((m) => m.state === 'starting')
  }, [runtimeStatus])
  // Only while something is actually moving: an install and a model load are both
  // minutes long, and an idle tab should not ask twice a second for the session.
  useEffect(() => {
    if (!runtimeMoving) return
    const id = window.setInterval(() => void refreshRuntime().catch(() => undefined), 1200)
    return () => window.clearInterval(id)
  }, [runtimeMoving, refreshRuntime])

  // The cards the user opened, not the closed ones: a newly added backend starts
  // closed like the rest. A failure is on the summary line, so closing a card never
  // hides the reason to open it.
  const [openCards, setOpenCards] = useState<Record<string, boolean>>({})
  const [filter, setFilter] = useState('')
  const [enabledOnly, setEnabledOnly] = useState(false)
  const [errorsOnly, setErrorsOnly] = useState(false)

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

  const kindLabel = (kind: string) => {
    const key = `settings.backends.kind.${kind}`
    const label = tDynamic(key)
    // translate() echoes the key when it has no entry; a kind this build does not
    // name yet should still read as something rather than as "settings.backends.kind.x".
    return label === key ? kind : label
  }
  const groupOf = (kind: string): 'local' | 'hosted' | 'custom' =>
    ['lmstudio', 'ollama', 'llamacpp', 'mlx'].includes(kind)
      ? 'local'
      : ['deepseek', 'mistral', 'nvidia', 'groq'].includes(kind)
        ? 'hosted'
        : 'custom'
  const query = filter.trim().toLowerCase()
  const matching = draft.backends
    .map((backend, index) => ({ backend, index }))
    .filter(({ backend }) => {
      if (backend.kind === 'mlx' && !apple) return false
      if (enabledOnly && !backend.enabled) return false
      const status = health.find((h) => h.id === backend.id)
      if (errorsOnly && !(status && status.enabled && !status.ok)) return false
      if (!query) return true
      const mine = (runtimeStatus?.models ?? []).find((m) => m.backendId === backend.id)
      return [
        backend.name,
        backend.id,
        kindLabel(backend.kind),
        backend.baseUrl,
        status?.error ?? '',
        mine?.url ?? '',
      ]
        .join(' ')
        .toLowerCase()
        .includes(query)
    })
  // Grouped and stable: rows do not move while a probe changes state, and the order
  // inside a group is the order the backends are saved in.
  const groupOrder: ('local' | 'hosted' | 'custom')[] = ['local', 'hosted', 'custom']
  const ordered = matching
    .map((row) => ({ ...row, group: groupOf(row.backend.kind) }))
    .sort((a, b) => groupOrder.indexOf(a.group) - groupOrder.indexOf(b.group))
  const errorCount = matching.filter(({ backend }) => {
    const status = health.find((h) => h.id === backend.id)
    return !!status && status.enabled && !status.ok
  }).length
  const onCount = matching.filter(({ backend }) => backend.enabled).length
  const offCount = matching.length - onCount

  return (
    <>
      <Section
        title={t('settings.backends')}
        hint={t('settings.backendsHint')}
      >
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <input
            className="input min-w-[180px] flex-1"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder={t('settings.backends.filter')}
            aria-label={t('settings.backends.filter')}
          />
          <label className="flex items-center gap-1 text-[12px]">
            <input
              type="checkbox"
              checked={enabledOnly}
              onChange={(e) => setEnabledOnly(e.target.checked)}
            />
            {t('settings.backends.enabledOnly')}
          </label>
          <button
            type="button"
            className={`btn btn-sm ${errorsOnly ? 'btn-primary' : 'btn-ghost'}`}
            disabled={errorCount === 0 && !errorsOnly}
            aria-pressed={errorsOnly}
            onClick={() => setErrorsOnly((v) => !v)}
          >
            {t('settings.backends.errors', { count: errorCount })}
          </button>
          <span className="faint text-[11px]">
            {t('settings.backends.summary', { on: onCount, off: offCount })}
          </span>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() =>
              setOpenCards(
                matching.some(({ backend }) => openCards[backend.id])
                  ? {}
                  : Object.fromEntries(matching.map(({ backend }) => [backend.id, true])),
              )
            }
          >
            {matching.some(({ backend }) => openCards[backend.id])
              ? t('settings.backends.collapseAll')
              : t('settings.backends.expandAll')}
          </button>
        </div>
        {ordered.map(({ backend, index, group }, position) => {
          // MLX is not shown on a machine that cannot run it. The entry is still in
          // the settings blob — settings travel, and dropping it would throw away a
          // Mac user's configuration on the first save from a Linux box — so this is
          // a render decision, not a schema one (the filter above already dropped it).
          const groupStart = position === 0 || ordered[position - 1].group !== group
          const status = health.find((h) => h.id === backend.id)
          const modelCount = models.filter((m) => m.backend === backend.id).length
          // The state the running app reported, not the checkbox: an entry that is
          // off was never contacted, and that is what the hint below explains.
          const offSaved = status ? !status.enabled : false
          // A key counts as present whether it was just typed or is already in the
          // secret store. The stored one cannot be read back — that is what being a
          // secret means here — so the backend reports only which entries have one.
          const typedKey = (backend.apiKey ?? '').trim() !== ''
          const keySaved = secrets?.backendKeys?.[backend.id] ?? false
          const keyPresent = typedKey || keySaved
          const usesKey = ['custom', 'deepseek', 'mistral', 'nvidia', 'groq', 'llamacpp'].includes(
            backend.kind,
          )
          const open = !!openCards[backend.id]
          const mine = (runtimeStatus?.models ?? []).find((m) => m.backendId === backend.id)
          const enableAndApply = () => {
            const backends = draft.backends.map((b, i) => (i === index ? { ...b, enabled: true } : b))
            patch({ backends })
            void onApply({ ...draft, backends })
          }
          return (
            <Fragment key={backend.id}>
              {groupStart && (
                <div className="faint mb-1 flex items-center gap-2 text-[10px] font-semibold uppercase tracking-wider">
                  {t(`settings.backends.group.${group}`)}
                  <span className="badge">
                    {ordered.filter((row) => row.group === group).length}
                  </span>
                </div>
              )}
            <div className="mb-2 rounded-md" style={{ background: 'var(--surface-2)' }}>
              <div className="flex items-center gap-2 p-2">
                <input
                  type="checkbox"
                  checked={backend.enabled}
                  aria-label={t('settings.enabled')}
                  title={t('settings.enabled')}
                  onChange={(e) => updateBackend(index, { enabled: e.target.checked })}
                />
                <button
                  type="button"
                  className="flex min-w-0 flex-1 flex-wrap items-center gap-2 text-left"
                  aria-expanded={open}
                  aria-controls={`backend-body-${backend.id}`}
                  onClick={() =>
                    setOpenCards((prev) => ({ ...prev, [backend.id]: !prev[backend.id] }))
                  }
                >
                  <span className="min-w-0 truncate text-[13px] font-medium">
                    {backend.name || backend.id}
                  </span>
                  <span className="badge">{kindLabel(backend.kind)}</span>
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
                    {status.ok ? t('settings.ok') : t('settings.fail')}
                  </span>
                )}
                {mine && mine.state !== 'stopped' && (
                  <span
                    className={`badge ${
                      mine.state === 'running'
                        ? 'badge-ok'
                        : mine.state === 'failed'
                          ? 'badge-danger'
                          : ''
                    }`}
                  >
                    {tDynamic(`settings.managed.state.${mine.state}`)}
                  </span>
                )}
                </button>
                <button
                  type="button"
                  className="btn btn-sm btn-ghost"
                  aria-label={open ? t('settings.backends.collapse') : t('settings.backends.expand')}
                  aria-expanded={open}
                  aria-controls={`backend-body-${backend.id}`}
                  onClick={() =>
                    setOpenCards((prev) => ({ ...prev, [backend.id]: !prev[backend.id] }))
                  }
                >
                  {open ? '▾' : '▸'}
                </button>
              </div>
              <div className="faint flex flex-wrap items-center gap-x-3 gap-y-1 px-2 pb-2 text-[11px]">
                <span className="mono min-w-0 max-w-full truncate">{backend.baseUrl}</span>
                {modelCount > 0 && (
                  <span>
                    {modelCount} {t('settings.modelsFound')}
                  </span>
                )}
                {usesKey && (typedKey || keySaved || secrets) && (
                  <span>
                    {keySaved
                      ? t('settings.backends.keyStored')
                      : typedKey
                        ? t('settings.backends.keyTyped')
                        : t('settings.backends.noKey')}
                  </span>
                )}
              </div>
              {open && (
                <div
                  id={`backend-body-${backend.id}`}
                  className="border-t p-2"
                  style={{ borderColor: 'var(--line)' }}
                >
                  <label
                    className="mb-1 block text-[12px]"
                    htmlFor={`backend-name-${backend.id}`}
                  >
                    {t('settings.backends.name')}
                  </label>
                  <input
                    id={`backend-name-${backend.id}`}
                    className="input mb-2 w-full"
                    value={backend.name}
                    onChange={(e) => updateBackend(index, { name: e.target.value })}
                  />
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
                <div className="px-2 pb-2 text-[11px]" style={{ color: 'var(--danger)' }}>
                  {errText(status.error)}
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
              {/* The switch belongs to the backend rather than to the settings page
                  because what it costs depends on the backend: on a rated one it is
                  the difference between reading a page and being refused for its
                  size, on a local server only the latency of the extra request. */}
              <label className="mt-2 flex items-center gap-2 text-[13px]">
                <input
                  type="checkbox"
                  checked={backend.digest ?? false}
                  onChange={(e) => updateBackend(index, { digest: e.target.checked })}
                />
                {t('settings.backendDigest')}
              </label>
              {backend.digest && (
                <p className="faint mt-1 text-[11px]">{t('settings.backendDigestHint')}</p>
              )}
              {/* Only the kind that KiChat knows how to run itself gets the
                  controls; every other entry stays an address it contacts. */}
              {backend.kind === 'llamacpp' && (
                <LocalModelSection
                  backend={backend}
                  draft={draft}
                  patch={patch}
                  status={runtimeStatus}
                  onRefresh={refreshRuntime}
                />
              )}
              {backend.kind === 'mlx' && (
                <MLXModelSection
                  backend={backend}
                  draft={draft}
                  patch={patch}
                  status={runtimeStatus}
                  onRefresh={refreshRuntime}
                />
              )}
                </div>
              )}
            </div>
            </Fragment>
          )
        })}
        {ordered.length === 0 && (
          <p className="faint mb-2 text-[12px]">{t('settings.backends.noMatches')}</p>
        )}
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
            onClick={() => {
              const id = `custom-${Date.now().toString(36)}`
              // The row would otherwise be invisible behind an active filter and
              // closed, which reads as "nothing was added".
              setFilter('')
              setEnabledOnly(false)
              setErrorsOnly(false)
              setOpenCards({ [id]: true })
              patch({
                backends: [
                  ...draft.backends,
                  {
                    id,
                    name: 'Custom',
                    kind: 'custom',
                    baseUrl: 'http://127.0.0.1:8080',
                    apiKey: '',
                    enabled: true,
                  } as llm.BackendConfig,
                ],
              })
            }}
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
  // The chosen model, by `backend/id`. Found in `models` rather than in the filtered
  // candidate list, so toggling the filter cannot close the editor mid-edit.
  const [selected, setSelected] = useState('')
  const [windowValue, setWindowValue] = useState('')
  const [ceilingValue, setCeilingValue] = useState('')
  const [onlyMissing, setOnlyMissing] = useState(false)
  const [saving, setSaving] = useState(false)
  // Closed by default: the backend list is the tab's subject, and the overrides are
  // a second, longer form underneath it. The count on the button is what says there
  // is anything in there worth opening.
  const [open, setOpen] = useState(false)

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
  // the settings row rather than on what is on screen.
  const groqBackends = useMemo(
    () => new Set(backends.filter((b) => b.kind === 'groq').map((b) => b.id)),
    [backends],
  )

  const missing = useMemo(() => models.filter((m) => !m.contextLimit), [models])
  // Every model, not only the ones missing a window. "Change this model's window" is
  // the intention, and hiding the models that already report a figure made the old
  // list read as if the missing ones were the only editable models. The filter above
  // keeps the old shortcut for a catalogue with many unreported windows.
  const candidates = onlyMissing ? missing : models

  const selectedModel = useMemo(
    () => models.find((m) => `${m.backend}/${m.id}` === selected),
    [models, selected],
  )
  const stored = overrides[selected]
  const isGroq = selectedModel ? groqBackends.has(selectedModel.backend) : false

  const listed = useMemo(() => new Set(models.map((m) => `${m.backend}/${m.id}`)), [models])
  // Overrides kept for a model this catalogue no longer lists: the backend is switched
  // off, the file was deleted, or the vendor retired the model. They used to vanish
  // with the row, which left no way to remove them.
  const orphans = useMemo(
    () =>
      Object.keys(overrides).filter(
        (key) =>
          !listed.has(key) &&
          ((overrides[key].contextLimit ?? 0) > 0 || (overrides[key].requestCeiling ?? 0) > 0),
      ),
    [overrides, listed],
  )

  const select = (ref: ModelRef | null) => {
    if (!ref) {
      setSelected('')
      setWindowValue('')
      setCeilingValue('')
      return
    }
    const key = `${ref.backend}/${ref.model}`
    setSelected(key)
    // Seeded from the override, never from the merged listing: the listing already
    // carries the override's number, and saving that back would read as an edit while
    // changing nothing.
    setWindowValue(overrides[key]?.contextLimit ? String(overrides[key].contextLimit) : '')
    setCeilingValue(overrides[key]?.requestCeiling ? String(overrides[key].requestCeiling) : '')
  }

  const write = async (backend: string, id: string, next: llm.ModelOverride) => {
    const key = `${backend}/${id}`
    const before = overrides[key]?.contextLimit ?? 0
    await App.SetModelOverride(backend, id, next)
    onSaved(key, next)
    // Only the window is part of a model's listed metadata, so only a window change
    // asks for a re-listing — and on a metered backend a listing is a request.
    if (next.contextLimit !== before) {
      await useStore.getState().refreshModels(true)
    }
  }

  const save = async () => {
    if (!selectedModel) return
    setSaving(true)
    try {
      await write(selectedModel.backend, selectedModel.id, {
        contextLimit: Number(windowValue) || 0,
        // A ceiling this backend does not show is carried over rather than cleared by
        // a save of the window alone.
        requestCeiling: isGroq ? Number(ceilingValue) || 0 : (stored?.requestCeiling ?? 0),
        capabilities: stored?.capabilities ?? [],
      } as llm.ModelOverride)
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setSaving(false)
    }
  }

  /**
   * Removes the numbers, and only the numbers. The capability list is not edited on
   * this panel, so a save or a clear here must not be able to drop it.
   */
  const clear = async () => {
    if (!selectedModel) return
    setSaving(true)
    try {
      await write(selectedModel.backend, selectedModel.id, {
        contextLimit: 0,
        requestCeiling: 0,
        capabilities: stored?.capabilities ?? [],
      } as llm.ModelOverride)
      setWindowValue('')
      setCeilingValue('')
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setSaving(false)
    }
  }

  const clearOrphan = async (key: string) => {
    // The key is `backend/id`, and a model id may itself contain slashes, so only the
    // first one separates the two.
    const at = key.indexOf('/')
    if (at < 0) return
    const backend = key.slice(0, at)
    const id = key.slice(at + 1)
    const next = {
      contextLimit: 0,
      requestCeiling: 0,
      capabilities: overrides[key]?.capabilities ?? [],
    } as llm.ModelOverride
    try {
      await App.SetModelOverride(backend, id, next)
      onSaved(key, next)
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    }
  }

  return (
    <Section title={t('model.overrides')} hint={t('model.overrideHint')}>
      <div className="mb-2 flex flex-wrap items-center gap-3 text-[12px]">
        <span className="faint">
          {t('model.overridesSummary', { missing: missing.length, total: models.length })}
        </span>
        <button
          type="button"
          className="btn btn-sm ml-auto"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          {open
            ? t('settings.overrides.hide')
            : t('settings.overrides.show', { count: Object.keys(overrides).length })}
        </button>
      </div>

      {open && (
        <>
      <div className="mb-2 flex flex-wrap items-center gap-2 text-[12px]">
        <label className="flex items-center gap-2">
          <input
            type="checkbox"
            checked={onlyMissing}
            onChange={(e) => setOnlyMissing(e.target.checked)}
          />
          {t('model.overridesOnlyMissing')}
        </label>
      </div>
      <Field label={t('model.overridesPick')} labelledBy="override-model-label">
        <ModelPicker
          id="override-model"
          labelledBy="override-model-label"
          models={candidates}
          value={
            selectedModel ? { backend: selectedModel.backend, model: selectedModel.id } : null
          }
          placeholder={t('model.overridesSearch')}
          // Nothing to clear here: with no model chosen the editor is simply closed,
          // and an empty row would suggest the override itself had an "off" state.
          onChange={select}
        />
      </Field>

      {!selectedModel ? (
        <p className="faint text-[11px]">{t('model.overridesPickHint')}</p>
      ) : (
        <div className="mt-1 rounded p-3" style={{ background: 'var(--surface-2)' }}>
          <div className="mb-2 flex items-baseline gap-2">
            <span
              className="mono min-w-0 truncate text-[12px]"
              title={`${selectedModel.backend}/${selectedModel.id}`}
            >
              {modelLabel(selectedModel.id)}
            </span>
            <span className="faint shrink-0 text-[11px]">{selectedModel.backend}</span>
            {stored && <span className="badge shrink-0">{t('model.overridden')}</span>}
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <Field label={t('model.overrideWindow')}>
              <input
                className="input"
                type="number"
                min={0}
                value={windowValue}
                placeholder={
                  selectedModel.maxContextLimit ? String(selectedModel.maxContextLimit) : '32768'
                }
                onChange={(e) => setWindowValue(e.target.value)}
              />
            </Field>
            {isGroq && (
              <Field label={t('model.ceiling')} hint={t('model.ceilingHint')}>
                <input
                  className="input"
                  type="number"
                  min={0}
                  value={ceilingValue}
                  placeholder="8000"
                  onChange={(e) => setCeilingValue(e.target.value)}
                />
              </Field>
            )}
          </div>

          <div className="faint mb-2 mt-1 text-[11px]">
            {selectedModel.contextLimit > 0
              ? t('model.windowCurrent', { tokens: formatNumber(selectedModel.contextLimit) })
              : t('model.windowUnknown')}
          </div>

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              className="btn btn-sm btn-primary"
              disabled={saving}
              onClick={() => void save()}
            >
              {saving ? '…' : t('settings.save')}
            </button>
            <button
              type="button"
              className="btn btn-sm"
              disabled={!stored || saving}
              onClick={() => void clear()}
            >
              {t('model.overrideClear')}
            </button>
          </div>
        </div>
      )}

      {orphans.length > 0 && (
        <div className="mt-3 border-t pt-2" style={{ borderColor: 'var(--border)' }}>
          <p className="faint mb-1 text-[11px]">{t('model.overridesOrphanHint')}</p>
          {orphans.map((key) => (
            <div key={key} className="flex items-center gap-2 text-[12px]">
              <span className="mono min-w-0 flex-1 truncate">{key}</span>
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() => void clearOrphan(key)}
              >
                {t('model.overrideClear')}
              </button>
            </div>
          ))}
        </div>
      )}
        </>
      )}
    </Section>
  )
}

/* -------------------------------------------------------------------------
 * System prompt
 * ---------------------------------------------------------------------- */

