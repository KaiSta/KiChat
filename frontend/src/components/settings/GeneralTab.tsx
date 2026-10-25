import { useEffect, useState } from 'react'
import { App, toDate, type config, type main, type runtime } from '../../lib/api'
import { useStore } from '../../store'
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
  const [copiedTrace, setCopiedTrace] = useState(false)
  const [tracePath, setTracePath] = useState('')
  // The trace belongs to the start rather than to a chat, so it is asked for once
  // and shown even when no model is configured yet.
  useEffect(() => {
    void App.GetTracePath().then(setTracePath).catch(() => undefined)
  }, [])
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
