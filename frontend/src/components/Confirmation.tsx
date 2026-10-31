import { useState } from 'react'
import { folderName } from '../lib/api'
import { useStore } from '../store'
import type { ApprovalChange, ApprovalCommand, ApprovalDecision } from '../lib/api'
import { HunkDiff } from './HunkDiff'

/**
 * The question a project tool asks, and the buttons that answer it.
 *
 * It is a blocking question, not a notification: the turn that asked is suspended in Go until
 * `ResolveToolApproval` answers, so nothing the model does can pass this card. That is why it
 * renders at the composer rather than inside the transcript — the answer is the next thing
 * this chat needs, and the transcript is where it will end up recorded anyway (the tool call
 * it belongs to is already there, in the `pending` state).
 *
 * There are two kinds of question here, and they are answered differently because they *are*
 * different. A change to a file is shown as the hunks that differ, which is the only form in
 * which "replace the string in `internal/store/store.go`" can be agreed to. A command cannot
 * be previewed at all — `rm -rf build` is its own description — so what is shown is
 * everything a person needs to judge it: the exact line, the folder it runs in, which shell
 * interprets it, and how long it may take.
 *
 * The blanket answer ("always in this chat") is offered beside the single one because a
 * coding turn is usually a dozen of these, and a dialog nobody reads is not a confirmation
 * either. It is scoped to the kind of question on screen and worded with that kind in it —
 * "always allow commands in this chat" — because it grants nothing else: a file card answered
 * that way still leaves the command question open, which is the distinction the settings
 * switch makes and the one a person actually means. What it grants dies with the chat, and the
 * header shows it while it lasts (ChatConsent in the chat's state, revocable there).
 *
 * The third choice is the one that outlives the chat: *stop asking about file changes* (or
 * about commands) allows this change and turns the matching switch in Settings on. It is a
 * link rather than a fourth button because it is a different kind of answer — a setting — and
 * because a permission given by pressing a friendly-looking button is exactly the permission
 * nobody remembers granting. It names the switch it turns on, says it survives a restart, and
 * says where to undo it.
 */
export function Confirmation() {
  const t = useStore((s) => s.t)
  const resolve = useStore((s) => s.resolveApproval)
  // One card per chat, read by the chat that is open: a question asked in a chat the user
  // clicked away from is still waiting when they come back, because the entry is keyed by
  // chat and only its *rendering* follows the view.
  const approval = useStore((s) => (s.activeChatId ? s.approvals[s.activeChatId] : undefined))
  const stopAsking = useStore((s) => s.stopAsking)
  const [busy, setBusy] = useState<ApprovalDecision | null>(null)
  const [granting, setGranting] = useState(false)
  // The command as it stands in the card's field. Keyed by the question rather than kept as a
  // bare string, so the card that replaces this one shows *its* line rather than the tail of
  // an edit somebody made to the previous command.
  const [draft, setDraft] = useState<{ id: string; line: string } | null>(null)

  if (!approval) return null

  // What the field holds: the model's line until somebody types, then their version. A field
  // emptied altogether is not an answer — there is nothing to run — so Allow stays disabled
  // rather than sending a blank line the backend would refuse after the click.
  const line =
    approval.kind === 'command'
      ? draft && draft.id === approval.approvalId
        ? draft.line
        : approval.command.line
      : ''
  const edited =
    approval.kind === 'command' && line.trim() !== '' && line.trim() !== approval.command.line.trim()

  const answer = async (decision: ApprovalDecision) => {
    setBusy(decision)
    try {
      // The line travels only when it was changed: the plain answer is the plain binding, and
      // an unchanged field has nothing to add to it.
      const sent = approval.kind === 'command' && edited ? line.trim() : undefined
      await resolve(approval.chatId, approval.approvalId, decision, sent)
    } finally {
      setBusy(null)
    }
  }

  // The setting is saved first and the question answered second, inside the store: a failed
  // save therefore leaves this card where it is, rather than closing a question nobody has
  // actually decided. The line travels with it when it was edited, because the answer this
  // click gives is still an answer to the command on screen — a person who fixed a typo and
  // then switched the question off meant their line, not the model's.
  const never = async () => {
    setGranting(true)
    try {
      await stopAsking(
        approval.chatId,
        approval.approvalId,
        approval.kind,
        approval.kind === 'command' && edited ? line.trim() : undefined,
      )
    } finally {
      setGranting(false)
    }
  }

  const disabled = busy !== null || granting
  // Allow is the one button an empty field blocks: there is nothing to run. Declining a
  // question whose line somebody cleared still makes sense, and so does the setting.

  return (
    <div
      className="card mb-2 px-3 py-2"
      role="group"
      aria-label={t('confirm.title')}
      style={{ borderColor: 'var(--accent)' }}
    >
      {approval.kind === 'command' ? (
        <CommandQuestion
          command={approval.command}
          line={line}
          edited={edited}
          disabled={disabled}
          onChange={(next) => setDraft({ id: approval.approvalId, line: next })}
          onReset={() => setDraft({ id: approval.approvalId, line: approval.command.line })}
        />
      ) : (
        <ChangeQuestion change={approval.change} />
      )}

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <button
          type="button"
          className="btn btn-sm btn-primary"
          disabled={disabled || (approval.kind === 'command' && line.trim() === '')}
          onClick={() => void answer('once')}
        >
          {t('change.once')}
        </button>
        <button
          type="button"
          className="btn btn-sm"
          title={t('change.alwaysHint')}
          disabled={disabled}
          onClick={() => void answer('always')}
        >
          {/* What it stops asking about, not just that it stops asking: the earlier label said
              "always in this chat" on a file card while the scope — changes *and commands* — was
              only in the tooltip, so the wider permission was the invisible half. */}
          {approval.kind === 'command' ? t('change.alwaysCommands') : t('change.alwaysFiles')}
        </button>
        <button
          type="button"
          className="btn btn-sm btn-danger"
          disabled={disabled}
          onClick={() => void answer('deny')}
        >
          {t('change.decline')}
        </button>
        <span className="faint text-[11px]">{t('change.declineHint')}</span>
      </div>

      {/* The setting, offered where the question is. A link rather than a button: it is not
          another answer to *this* question, it is the switch that makes there not be a next
          one — and it says so in the tooltip rather than in a quieter colour alone. */}
      <div className="mt-1">
        <button
          type="button"
          className="text-[11px] underline decoration-dotted"
          style={{ color: 'var(--text-dim)' }}
          title={t('confirm.stopAskingHint')}
          disabled={disabled}
          onClick={() => void never()}
        >
          {approval.kind === 'command'
            ? t('confirm.stopAskingCommands')
            : t('confirm.stopAskingFiles')}
        </button>
      </div>
    </div>
  )
}

/** One file's change: the lines that differ, with enough around them to be read. */
function ChangeQuestion({ change }: { change: ApprovalChange }) {
  const t = useStore((s) => s.t)

  return (
    <>
      <div className="flex flex-wrap items-center gap-2 text-[12px]">
        <span className="badge badge-accent">
          {change.operation === 'create'
            ? t('change.create')
            : change.operation === 'overwrite'
              ? t('change.overwrite')
              : t('change.edit')}
        </span>
        <span className="mono truncate" style={{ color: 'var(--text)' }} title={change.path}>
          {change.path}
        </span>
        {/* The diff's own counts, so they match the rows below rather than the size of the
            file: a one-line change in a long file reads −1 +1, which is what the card is
            showing. */}
        <span className="faint mono">
          {change.removed > 0 && `−${change.removed}`}
          {change.removed > 0 && change.added > 0 && ' '}
          {change.added > 0 && `+${change.added}`}
        </span>
        <span className="faint ml-auto">{t('change.waiting')}</span>
      </div>

      {change.createsDirs.length > 0 && (
        <div className="mt-1 text-[11px]" style={{ color: 'var(--warn)' }}>
          {t('change.createsDirs', { folders: change.createsDirs.join(', ') })}
        </div>
      )}

      {/* A replace_all that matched more than once, said out loud. The diff below can only show
          one place — the model's tool call carries one span — so approving it is approving the
          other occurrences blind, and that has to be visible rather than inferred from a total
          that looks like a single line. */}
      {change.occurrences > 1 && (
        <div className="mt-1 text-[11px]" style={{ color: 'var(--warn)' }}>
          {t('change.occurrences', { count: change.occurrences })}
        </div>
      )}

      <div
        className="mt-2 max-h-64 overflow-auto rounded p-2"
        style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
      >
        <HunkDiff hunks={change.hunks} newlineOnly={change.newlineOnly} />
      </div>

      {change.truncated && <div className="faint mt-1 text-[11px]">{t('change.truncated')}</div>}
    </>
  )
}

/** One command: the line, the folder, the shell and the limit.
 *
 * The line is an editable field rather than a quotation. What is being approved is a sentence
 * somebody wrote — a typo in a path, a flag the model guessed — and the person reading it is
 * the one who knows. Declining and typing it again in the composer would cost the turn a round
 * and lose the folder, the timeout and the shell that the tool chose, so the fix belongs here.
 *
 * The app still runs the line through its own refusals after the answer (internal/tools), so
 * the field cannot be used to slip past the one check that runs before the question: an edited
 * line asking for administrator rights is refused exactly like the model's own.
 */
function CommandQuestion({
  command,
  line,
  edited,
  disabled,
  onChange,
  onReset,
}: {
  command: ApprovalCommand
  line: string
  edited: boolean
  disabled: boolean
  onChange: (line: string) => void
  onReset: () => void
}) {
  const t = useStore((s) => s.t)
  return (
    <>
      <div className="flex flex-wrap items-center gap-2 text-[12px]">
        <span className="badge badge-accent">{t('command.title')}</span>
        {/* The shell is on the card because it is part of what is being approved: the same
            line means different things to cmd, PowerShell and sh, and a person who cannot
            see which one interprets it cannot judge the line. */}
        <span className="mono faint">{command.shell}</span>
        <span className="faint ml-auto">{t('change.waiting')}</span>
      </div>

      <textarea
        className="mono mt-2 max-h-40 w-full resize-y rounded p-2 text-[12px]"
        style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
        rows={Math.min(4, Math.max(1, line.split('\n').length))}
        spellCheck={false}
        autoComplete="off"
        aria-label={t('command.editLabel')}
        disabled={disabled}
        value={line}
        onChange={(event) => onChange(event.target.value)}
      />

      {edited && (
        <div className="mt-1 flex flex-wrap items-center gap-2 text-[11px]" style={{ color: 'var(--warn)' }}>
          {/* Said out loud, because the card has stopped being a preview: the person has to
              know that what runs is their line and that the app checked it as such. */}
          <span>{t('command.edited')}</span>
          <button
            type="button"
            className="underline decoration-dotted"
            disabled={disabled}
            onClick={onReset}
          >
            {t('command.reset')}
          </button>
        </div>
      )}

      <div className="faint mt-1 flex flex-wrap items-center gap-2 text-[11px]">
        <span title={command.dir}>{t('command.dir', { folder: folderName(command.dir) })}</span>
        <span>·</span>
        <span>{t('command.timeout', { seconds: command.timeoutSec })}</span>
      </div>
    </>
  )
}

