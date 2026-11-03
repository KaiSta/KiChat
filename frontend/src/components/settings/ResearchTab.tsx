import { useEffect, useState } from 'react'
import { App, formatNumber, type config, type main } from '../../lib/api'
import { errText, useStore } from '../../store'
import { Section, Field } from './shared'

// The search providers this build knows. Keeping the list here means the panel can show a
// provider that is currently disabled — the old buttons could only show the enabled ones.
const PROVIDERS = ['brave', 'duckduckgo']

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

  // The list is the order the chain walks: the first enabled provider answers, the rest are
  // fallbacks. It used to be drawn as two independent on/off buttons in a fixed order and
  // enabling appended to the end, so turning Brave back on quietly made DuckDuckGo the first
  // choice while the panel still read "Brave" first. Showing the stored order, and letting it
  // be moved, is the whole fix.
  const order = search.providerOrder
  const providerName = (name: string) =>
    name === 'brave'
      ? t('search.brave')
      : name === 'duckduckgo'
        ? t('search.duckduckgo')
        : name
  const setOrder = (next: string[]) => patch({ search: { ...search, providerOrder: next } })
  const moveProvider = (from: number, to: number) => {
    if (to < 0 || to >= order.length) return
    const next = [...order]
    const [moved] = next.splice(from, 1)
    next.splice(to, 0, moved)
    setOrder(next)
  }

  return (
    <>
      <Section title={t('settings.research')}>
        <Field label={t('search.providers')} hint={t('settings.providerOrderHint')}>
          <ol className="flex flex-col gap-2">
            {order.map((name, index) => (
              <li key={name} className="flex items-center gap-2">
                <span className="mono faint w-4 text-[12px]">{index + 1}.</span>
                <span className="text-[13px]">{providerName(name)}</span>
                <span className="flex-1" />
                <button
                  type="button"
                  className="btn btn-sm"
                  title={t('settings.providerMoveUp')}
                  aria-label={t('settings.providerMoveUp')}
                  disabled={index === 0}
                  onClick={() => moveProvider(index, index - 1)}
                >
                  ↑
                </button>
                <button
                  type="button"
                  className="btn btn-sm"
                  title={t('settings.providerMoveDown')}
                  aria-label={t('settings.providerMoveDown')}
                  disabled={index === order.length - 1}
                  onClick={() => moveProvider(index, index + 1)}
                >
                  ↓
                </button>
                <button
                  type="button"
                  className="btn btn-sm"
                  disabled={order.length === 1}
                  title={order.length === 1 ? t('settings.providerLastEnabled') : undefined}
                  onClick={() => setOrder(order.filter((p) => p !== name))}
                >
                  {t('settings.providerDisable')}
                </button>
              </li>
            ))}
            {PROVIDERS.filter((name) => !order.includes(name)).map((name) => (
              <li key={name} className="flex items-center gap-2">
                <span className="mono faint w-4 text-[12px]">–</span>
                <span className="faint text-[13px]">{providerName(name)}</span>
                <span className="flex-1" />
                <button
                  type="button"
                  className="btn btn-sm"
                  onClick={() => setOrder([...order, name])}
                >
                  {t('settings.providerEnable')}
                </button>
              </li>
            ))}
          </ol>
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

