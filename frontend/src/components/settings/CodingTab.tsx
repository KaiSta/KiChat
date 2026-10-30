import { type config } from '../../lib/api'
import { useStore } from '../../store'
import { Section } from './shared'

/**
 * What the coding mode may do to a folder without being read first.
 *
 * It is a tab rather than a section of General because it is not about how the model writes
 * — it is the one switch in this window that decides whether a tool runs *unread*. The other
 * tabs configure a conversation; this one grants something, and it is set apart for the same
 * reason Images got a tab of its own: a decision with consequences should not be one row
 * among the samplers.
 *
 * The two switches are split by what a tool can do rather than by tool name, because that is
 * the distinction the person is making: a file change is a diff they can read afterwards and
 * a commit they can revert, while a command is neither — it runs with their own rights, can
 * leave the folder, and leaves nothing to review. Trusting the model with the files is not a
 * statement about the machine, so the panel never lets one answer stand for both.
 */
export function CodingTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  // A blob written before the group existed has no `autoApprove`, and the harness fixture may
  // not either: the panel renders before the first save, so the missing case is read as the
  // shipped default — both switches off — rather than assumed away.
  const auto = draft.autoApprove ?? { files: false, commands: false }

  return (
    <Section title={t('settings.autoApprove')} hint={t('settings.autoApproveHint')}>
      <label className="mb-1 flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={auto.files}
          onChange={(e) => patch({ autoApprove: { ...auto, files: e.target.checked } })}
        />
        {t('settings.autoApproveFiles')}
        <span className={auto.files ? 'badge badge-danger' : 'badge'}>
          {auto.files ? t('settings.autoApproveSkip') : t('settings.autoApproveAsk')}
        </span>
      </label>
      <p className="faint mb-3 text-[11px]">{t('settings.autoApproveFilesHint')}</p>

      <label className="mb-1 flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={auto.commands}
          onChange={(e) => patch({ autoApprove: { ...auto, commands: e.target.checked } })}
        />
        {t('settings.autoApproveCommands')}
        <span className={auto.commands ? 'badge badge-danger' : 'badge'}>
          {auto.commands ? t('settings.autoApproveSkip') : t('settings.autoApproveAsk')}
        </span>
      </label>
      <p className="faint text-[11px]">{t('settings.autoApproveCommandsHint')}</p>
    </Section>
  )
}
