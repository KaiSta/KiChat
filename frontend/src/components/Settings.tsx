import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { App, formatNumber, type config, type llm, type main } from '../lib/api'
import { useStore } from '../store'
import { asLang, LANGS } from '../lib/i18n'

type Tab = 'general' | 'backends' | 'prompt' | 'research' | 'context'

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

  const save = async () => {
    setSaving(true)
    try {
      // Adopt what the backend stored rather than the draft: the untouched
      // built-in prompts are rewritten into the language being saved, so the
      // draft and the stored settings would otherwise disagree until a restart.
      const stored = await App.SaveSettings(draft)
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
    </>
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
  onApply: () => void
}) {
  const t = useStore((s) => s.t)
  const health = useStore((s) => s.health)
  const models = useStore((s) => s.models)
  const [secrets, setSecrets] = useState<main.SecretStatus | null>(null)

  useEffect(() => {
    void App.GetSecretStatus().then(setSecrets).catch(() => undefined)
  }, [])

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
          return (
            <div key={backend.id} className="mb-3 rounded-md p-3" style={{ background: 'var(--surface-2)' }}>
              <div className="mb-2 flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={backend.enabled}
                  onChange={(e) => updateBackend(index, { enabled: e.target.checked })}
                />
                <input
                  className="input flex-1"
                  value={backend.name}
                  onChange={(e) => updateBackend(index, { name: e.target.value })}
                />
                <span className={`badge ${status?.ok ? 'badge-ok' : 'badge-danger'}`}>
                  {status?.ok ? `${t('settings.ok')} · ${modelCount} ${t('settings.modelsFound')}` : t('settings.fail')}
                </span>
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
                backend.kind === 'llamacpp') && (
                <input
                  className="input mono mt-2"
                  type="password"
                  value={backend.apiKey ?? ''}
                  onChange={(e) => updateBackend(index, { apiKey: e.target.value })}
                  placeholder={
                    backend.kind === 'deepseek' ? t('settings.apiKeyRequired') : t('settings.apiKeyOptional')
                  }
                />
              )}
              {status && !status.ok && (
                <div className="mt-2 text-[11px]" style={{ color: 'var(--danger)' }}>
                  {status.error}
                </div>
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

