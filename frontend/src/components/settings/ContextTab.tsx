import { type config } from '../../lib/api'
import { useStore } from '../../store'
import { ModelPicker } from '../ModelPicker'
import { Section, Field } from './shared'

export function ContextTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const compact = draft.compact
  const summaryModel = compact.summaryModel ?? { backend: '', model: '' }
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
        <label className="mb-1 flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={compact.midTurnCheck}
            onChange={(e) => patch({ compact: { ...compact, midTurnCheck: e.target.checked } })}
          />
          {t('settings.midTurnCheck')}
        </label>
        <p className="faint mb-3 mt-1 text-[11px]">{t('settings.midTurnCheckHint')}</p>
        <Field
          label={t('settings.summaryModel')}
          hint={t('settings.summaryModelHint')}
          labelledBy="summary-model-label"
        >
          <ModelPicker
            id="summary-model"
            labelledBy="summary-model-label"
            models={models}
            value={summaryModel}
            emptyLabel={t('settings.summaryModelDefault')}
            unavailableLabel={t('settings.summaryModelUnavailable')}
            onChange={(ref) =>
              patch({ compact: { ...compact, summaryModel: ref ?? { backend: '', model: '' } } })
            }
          />
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

