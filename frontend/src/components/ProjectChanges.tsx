import { useState } from 'react'
import { type main } from '../lib/api'
import { useStore } from '../store'

/**
 * What this chat has done to the project folder, and the one action over it.
 *
 * It is a panel rather than a section of the transcript because it answers a different
 * question. The transcript is history: it says what happened, in order, one tool card at a
 * time — and a file the model rewrote in four rounds appears four times there, with nothing
 * saying the fourth one replaced the first three. This is state: which files are different from
 * what the chat found, how, and whether that can be taken back.
 *
 * The undo is per file and it is the *chat's* change, not the last round's, because that is the
 * sentence people mean — "put `main.go` back the way it was before you started". The backend
 * decides whether pressing it is safe (a file edited since is refused rather than overwritten),
 * so the panel's job is only to say so plainly: a disabled button would be a dead end, and a
 * button that looked available and then failed would be worse.
 */
export function ProjectChanges({ chatId }: { chatId: string }) {
  const t = useStore((s) => s.t)
  const changes = useStore((s) => s.fileChanges[chatId]) ?? EMPTY
  const revert = useStore((s) => s.revertFile)
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)

  const undo = async (path: string) => {
    setBusy(path)
    try {
      await revert(chatId, path)
    } finally {
      setBusy(null)
    }
  }

  return (
    <>
      <button
        type="button"
        className={`btn btn-sm shrink-0 ${open ? 'btn-primary' : ''}`}
        aria-expanded={open}
        title={t('changes.hint')}
        onClick={() => setOpen((v) => !v)}
      >
        {t('changes.button')}
        {/* The count is the reason the button exists: a chat that has changed eleven files and
            a chat that has changed none look the same in the header without it. */}
        {changes.length > 0 && <span className="mono ml-1 opacity-70">{changes.length}</span>}
      </button>

      {open && (
        <div
          className="fixed inset-y-0 right-0 z-30 flex w-[26rem] max-w-full flex-col border-l"
          role="dialog"
          aria-label={t('changes.title')}
          style={{ background: 'var(--surface)', borderColor: 'var(--border)' }}
        >
          <div
            className="flex items-center gap-2 border-b px-3 py-2"
            style={{ borderColor: 'var(--border)' }}
          >
            <span className="text-[13px] font-medium">{t('changes.title')}</span>
            <button
              type="button"
              className="btn btn-sm ml-auto"
              onClick={() => setOpen(false)}
              aria-label={t('changes.close')}
            >
              ✕
            </button>
          </div>

          <div className="min-h-0 flex-1 overflow-auto p-3">
            {changes.length === 0 ? (
              <p className="faint text-[12px]">{t('changes.empty')}</p>
            ) : (
              changes.map((change) => (
                <Row
                  key={change.path}
                  change={change}
                  busy={busy === change.path}
                  onUndo={() => void undo(change.path)}
                />
              ))
            )}
          </div>
        </div>
      )}
    </>
  )
}

/** A module-level empty array, so a chat with nothing loaded does not re-render on every read. */
const EMPTY: main.FileChange[] = []

/** One changed file: what it was, what it is, and whether it can go back. */
function Row({
  change,
  busy,
  onUndo,
}: {
  change: main.FileChange
  busy: boolean
  onUndo: () => void
}) {
  const t = useStore((s) => s.t)
  const before = lines(change.before)
  const after = lines(change.after)

  return (
    <div
      className="card mb-2 px-3 py-2 text-[12px]"
      style={{ background: 'var(--surface-2)', borderColor: 'var(--border)' }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className={`badge ${change.operation === 'created' ? 'badge-ok' : 'badge-accent'}`}>
          {change.operation === 'created' ? t('changes.created') : t('changes.changed')}
        </span>
        <span className="mono truncate" title={change.path}>
          {change.path}
        </span>
        <span className="faint mono ml-auto shrink-0">
          {change.removed > 0 && `−${change.removed}`}
          {change.removed > 0 && change.added > 0 && ' '}
          {change.added > 0 && `+${change.added}`}
        </span>
      </div>

      <div
        className="mono mt-2 max-h-56 overflow-auto rounded p-2 text-[11px] leading-[1.45]"
        style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
      >
        {before.map((line, index) => (
          <div key={`b${index}`} className="flex whitespace-pre" style={{ color: 'var(--danger)' }}>
            <span className="w-3 shrink-0 select-none opacity-70">−</span>
            {/* The line itself stays the text's colour: a whole row in red is a row nobody
                can read, and this one has to be checkable. */}
            <span style={{ color: 'var(--text)' }}>{line}</span>
          </div>
        ))}
        {after.map((line, index) => (
          <div key={`a${index}`} className="flex whitespace-pre" style={{ color: 'var(--ok)' }}>
            <span className="w-3 shrink-0 select-none opacity-70">+</span>
            <span style={{ color: 'var(--text)' }}>{line}</span>
          </div>
        ))}
        {before.length === 0 && after.length === 0 && (
          <div className="faint">{t('change.empty')}</div>
        )}
      </div>

      {change.truncated && <div className="faint mt-1 text-[11px]">{t('changes.truncated')}</div>}

      <div className="mt-1 flex flex-wrap items-center gap-2 text-[11px]">
        {change.revisions > 1 && (
          <span className="faint">{t('changes.revisions', { count: change.revisions })}</span>
        )}
        {change.canRevert ? (
          <button
            type="button"
            className="btn btn-sm ml-auto"
            disabled={busy}
            title={t('changes.revertHint')}
            onClick={onUndo}
          >
            ↺ {t('changes.revert')}
          </button>
        ) : (
          // Why the button is not there, in the row rather than in a tooltip: a person who came
          // to undo a change needs to know whether it is done or impossible.
          <span className="ml-auto" style={{ color: 'var(--warn)' }}>
            {change.blocked === 'outside' ? t('changes.blockedOutside') : t('changes.blockedBinary')}
          </span>
        )}
      </div>
    </div>
  )
}

/** The lines of one side, with a file's trailing newline ignored — the same rule the
 * confirmation card's diff uses, so the two panes count a file the same way. */
function lines(text: string): string[] {
  if (text === '') return []
  const body = text.endsWith('\n') ? text.slice(0, -1) : text
  return body.split('\n')
}
