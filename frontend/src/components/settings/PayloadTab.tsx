import { type config } from '../../lib/api'
import { useStore } from '../../store'
import { ModelPicker } from '../ModelPicker'
import { Section, Field } from './shared'

export function PayloadTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  // An older settings blob and the harness fixture both predate the group, so it is
  // read defensively rather than assumed: the panel has to render before the first
  // save, and an undefined group would throw on the first keystroke.
  const digest = draft.digest ?? {
    model: { backend: '', model: '' },
    threshold: 2000,
    maxTokens: 600,
  }
  const active = (draft.backends ?? []).filter((b) => b.digest).map((b) => b.name || b.id)
  return (
    <Section title={t('settings.payload')} hint={t('settings.payloadHint')}>
      <Field
        label={t('settings.digestModel')}
        hint={t('settings.digestModelHint')}
        labelledBy="digest-model-label"
      >
        <ModelPicker
          id="digest-model"
          labelledBy="digest-model-label"
          models={models}
          value={digest.model}
          emptyLabel={t('settings.digestModelDefault')}
          unavailableLabel={t('settings.digestModelUnavailable')}
          onChange={(ref) =>
            patch({ digest: { ...digest, model: ref ?? { backend: '', model: '' } } })
          }
        />
      </Field>
      <Field label={t('settings.digestThreshold')} hint={t('settings.digestThresholdHint')}>
        <input
          className="input"
          type="number"
          min={200}
          step={100}
          value={digest.threshold}
          onChange={(e) =>
            patch({ digest: { ...digest, threshold: Number(e.target.value) || 0 } })
          }
        />
      </Field>
      {/* A blob written before the field existed has no `maxTokens`, so the zero
          case is rendered as the shipped default rather than as an empty box — the
          backend resolves a 0 to exactly that number, and the panel must show the
          value that is really used. */}
      <Field label={t('settings.digestMaxTokens')} hint={t('settings.digestMaxTokensHint')}>
        <input
          className="input"
          type="number"
          min={100}
          step={100}
          value={digest.maxTokens || 600}
          onChange={(e) =>
            patch({ digest: { ...digest, maxTokens: Number(e.target.value) || 0 } })
          }
        />
      </Field>
      <p className="faint text-[11px]">
        {active.length > 0
          ? `${t('settings.payloadActive')} ${active.join(', ')}`
          : t('settings.payloadInactive')}
      </p>
    </Section>
  )
}

/* -------------------------------------------------------------------------
 * Context and compaction
 * ---------------------------------------------------------------------- */

