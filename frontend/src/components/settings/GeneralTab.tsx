import { useEffect, useState } from 'react'
import { App, toDate, type config, type main, type runtime } from '../../lib/api'
import { errText, useStore } from '../../store'
import { asLang, LANGS } from '../../lib/i18n'
import { ModelPicker } from '../ModelPicker'
import { Section, Field } from './shared'

export function GeneralTab({
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
  const models = useStore((s) => s.models)
  const [info, setInfo] = useState<main.AppInfo | null>(null)

  useEffect(() => {
    void App.GetAppInfo().then(setInfo).catch(() => undefined)
  }, [])

  // Read defensively, because an older settings blob and the harness fixture
  // predate the field.
  const defaultModel = draft.defaultModel ?? { backend: '', model: '' }

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
        {/* The model a new chat opens on. The empty option says what the fallback
            is rather than reading as "no model": it is what every installation
            predating this setting has, and what a machine with nothing running
            falls back to either way. */}
        <Field
          label={t('settings.defaultModel')}
          hint={t('settings.defaultModelHint')}
          labelledBy="default-model-label"
        >
          <ModelPicker
            id="default-model"
            labelledBy="default-model-label"
            models={models}
            value={defaultModel}
            emptyLabel={t('settings.defaultModelAuto')}
            unavailableLabel={t('settings.defaultModelUnavailable')}
            onChange={(ref) => patch({ defaultModel: ref ?? { backend: '', model: '' } })}
          />
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
              max="100"
              value={draft.toolIterations}
              onChange={(e) => patch({ toolIterations: Number(e.target.value) })}
            />
          </Field>
          {/* The project's figure, beside the chat's so the difference is one glance. Two
              fields because the two replies are different shapes, and one number tuned for
              a question was cutting a refactor short. */}
          <Field label={t('settings.codingToolIterations')} hint={t('settings.codingToolIterationsHint')}>
            <input
              className="input"
              type="number"
              min="1"
              max="100"
              value={draft.codingToolIterations}
              onChange={(e) => patch({ codingToolIterations: Number(e.target.value) })}
            />
          </Field>
        </div>
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
  const [copiedTrace, setCopiedTrace] = useState(false)
  const [tracePath, setTracePath] = useState('')
  // The terminal the coding hand-off would open. Asked for here because this is the one
  // screen a person opens to check that their platform's launcher resolves to something:
  // the list that picks it can only be exercised on the operating system it belongs to.
  const [term, setTerm] = useState<main.TerminalStatus | null>(null)
  const [termFailed, setTermFailed] = useState(false)
  const [update, setUpdate] = useState<main.AppUpdate | null>(null)
  const [checking, setChecking] = useState(false)
  // The trace belongs to the start rather than to a chat, so it is asked for once
  // and shown even when no model is configured yet.
  useEffect(() => {
    void App.GetTracePath().then(setTracePath).catch(() => undefined)
    void App.DetectTerminal()
      .then(setTerm)
      .catch(() => setTermFailed(true))
  }, [])
  // The cached reading first, and one request only if there is none: the answer is
  // held on the Go side, so opening Settings twice does not spend a second request
  // out of GitHub's unauthenticated budget. The button re-checks on demand.
  useEffect(() => {
    let live = true
    void App.GetAppUpdate()
      .then((cached) => {
        if (!live) return
        setUpdate(cached)
        if (cached.checked) return
        setChecking(true)
        return App.CheckAppUpdate().then((reading) => {
          if (live) setUpdate(reading)
        })
      })
      .catch(() => undefined)
      .finally(() => {
        if (live) setChecking(false)
      })
    return () => {
      live = false
    }
  }, [])
  // The row only appears once the backend has answered, so a failed bridge
  // shows nothing rather than a row of blanks.
  if (!info) return null

  const checkNow = () => {
    setChecking(true)
    void App.CheckAppUpdate()
      .then(setUpdate)
      .catch(() => undefined)
      .finally(() => setChecking(false))
  }
  // The download is a link to the release page and never a fetch: see api_update.go
  // for why the app does not replace its own package.
  const downloadURL = update?.available ? update.url : ''

  // The commit is what makes the version actionable, so it travels with it. A
  // build with no commit (the package defaults) shows the bare version.
  const version = info.commit && info.commit !== 'none' ? `${info.version} (${info.commit})` : info.version
  const built = toDate(info.date)?.toLocaleString() ?? info.date
  const rows: { label: string; value: string }[] = [
    { label: t('settings.version'), value: version },
    { label: t('settings.buildDate'), value: built },
    { label: t('settings.platform'), value: info.platform },
    { label: t('settings.runtime'), value: info.goVersion },
    // Only once the backend has answered: a row of blanks is worse than no row, and an
    // older Go side has no such binding at all — which is what `termFailed` covers.
    ...(term && !termFailed
      ? [{ label: t('settings.terminal'), value: term.program || t('settings.terminalNone') }]
      : []),
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
      {/* The one row here that costs a request. It says what it could not ask rather
          than "current": a rate limit is not an up-to-date installation. */}
      <div
        className="mt-3 flex flex-wrap items-center gap-3 border-t pt-3 text-[12px]"
        style={{ borderColor: 'var(--border)' }}
      >
        <span className="faint w-24 shrink-0">{t('settings.update')}</span>
        {checking ? (
          <span className="faint min-w-0 flex-1">{t('settings.updateChecking')}</span>
        ) : update?.error ? (
          <span
            className="min-w-0 flex-1 truncate"
            style={{ color: 'var(--danger)' }}
            title={errText(update.error)}
          >
            {errText(update.error)}
          </span>
        ) : update?.devBuild ? (
          <span className="faint min-w-0 flex-1">{t('settings.updateDevBuild')}</span>
        ) : update?.available ? (
          <>
            <span className="badge badge-warn shrink-0">{t('settings.updateAvailable')}</span>
            {/* The pair is the answer, so it never breaks at the arrow and never
                shrinks into an ellipsis: at a narrow window the wrapping row gives it a
                line of its own, which is what the 560 px check showed it doing. */}
            <span
              className="mono shrink-0 whitespace-nowrap"
              title={`${update.current} → ${update.latest}`}
            >
              {update.current} → {update.latest}
            </span>
          </>
        ) : (
          <span className="faint min-w-0 flex-1">{t('settings.updateCurrent')}</span>
        )}
        {downloadURL && (
          <button
            type="button"
            className="btn btn-sm shrink-0"
            onClick={() => void App.OpenExternal(downloadURL)}
          >
            {t('settings.updateDownload')}
          </button>
        )}
        <button type="button" className="btn btn-sm shrink-0" disabled={checking} onClick={checkNow}>
          {t('settings.updateCheck')}
        </button>
      </div>
      {/* What changed, in the release's own words. Only when an update exists: the notes
          of a version you are already running are not news. Plain text rather than the
          chat's markdown renderer, because a release body is a short bullet list written
          for a page, and the page — one click away — is the reader for the links in it. */}
      {update?.available && update.notes && (
        <div className="mt-2 text-[11px]">
          <div className="faint">{t('settings.updateNotes')}</div>
          {/* Unboxed and three lines: the notes answer "what changed" at a glance, and the
              release page is the reader for the rest. A filled panel made a changelog the
              loudest thing on the screen a fresh install opens first.

              The body is --text-dim rather than .faint. At 11px the faint colour measures
              about 3.2:1 in the light theme, which is a note nobody can read; dim is still
              secondary text, and it clears the contrast the size needs. */}
          <div
            className="line-clamp-3 whitespace-pre-line break-words"
            style={{ color: 'var(--text-dim)' }}
          >
            {update.notes}
          </div>
        </div>
      )}

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

      {tracePath && (
        <div className="mt-4 border-t pt-3 text-[12px]" style={{ borderColor: 'var(--border)' }}>
          <div className="flex items-center gap-3">
            <span className="faint w-24 shrink-0">{t('settings.trace')}</span>
            <span className="mono min-w-0 flex-1 truncate" title={tracePath}>
              {tracePath}
            </span>
          </div>
          {/* Says what the file is for and, in the same breath, what it never holds:
              the promise is the reason it can be handed over in a bug report. */}
          <p className="faint mt-1">{t('settings.traceNote')}</p>
          <button
            type="button"
            className="btn btn-sm mt-2"
            onClick={() => {
              navigator.clipboard?.writeText(tracePath).then(
                () => {
                  setCopiedTrace(true)
                  window.setTimeout(() => setCopiedTrace(false), 1500)
                },
                () => undefined,
              )
            }}
          >
            {copiedTrace ? t('chat.copied') : t('settings.copyTrace')}
          </button>
        </div>
      )}
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
