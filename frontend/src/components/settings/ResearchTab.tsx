import { useEffect, useState } from 'react'
import { App, formatNumber, type config, type main } from '../../lib/api'
import { errText, useStore } from '../../store'
import { Section, Field } from './shared'

export function ResearchTab({
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
                  useStore.getState().toast('error', errText(err))
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
                  useStore.getState().toast('error', `Brave: ${errText(err)}`)
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
 * Tool results
 * ---------------------------------------------------------------------- */

