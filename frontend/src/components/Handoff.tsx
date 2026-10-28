import { useState } from 'react'
import { useStore } from '../store'

/**
 * A command the app handed back to the person, with the line to run.
 *
 * The confirmation card asks before KiChat acts; this is the opposite case — a line the app
 * cannot act on at all, because it needs administrator rights and there is no terminal here to
 * answer a password prompt. The refusal is recorded on the tool card in the transcript, and
 * that is where it belongs for the record; what it cannot do there is be *acted on*, because
 * the next thing a person does is type in the composer, several screens below a card they may
 * have scrolled past. So the line is repeated here, where the typing happens.
 *
 * Two steps, one card: copy the line, run it in a terminal, paste the output back. The hint
 * names the second one because copying alone leaves the answer exactly where it was — the model
 * has no way to know the command ran.
 *
 * It is dismissed rather than timed out. A banner that vanishes while somebody is in another
 * window switching to their terminal is a banner that has to be found again; the transcript's
 * card keeps the line either way, so dismissing loses nothing.
 */
export function Handoff() {
  const t = useStore((s) => s.t)
  const chatId = useStore((s) => s.activeChatId)
  const handoff = useStore((s) => (s.activeChatId ? s.handoffs[s.activeChatId] : undefined))
  const dismiss = useStore((s) => s.dismissHandoff)
  const [copied, setCopied] = useState(false)

  if (!handoff || !chatId) return null

  return (
    <div
      className="card mb-2 px-3 py-2"
      role="group"
      aria-label={t('handoff.title')}
      style={{ borderColor: 'var(--warn)' }}
    >
      <div className="flex flex-wrap items-center gap-2 text-[12px]">
        <span className="badge badge-warn">{t('handoff.title')}</span>
        {/* The line exactly as the model wrote it, one row, scrollable: a wrapped command
            pushes everything below it down, and the copy button is what it is here for. */}
        <span className="mono min-w-0 flex-1 truncate" title={handoff.command}>
          {handoff.command}
        </span>
        <button
          type="button"
          className="btn btn-sm btn-primary"
          onClick={() => {
            navigator.clipboard.writeText(handoff.command).then(
              () => {
                setCopied(true)
                window.setTimeout(() => setCopied(false), 1500)
              },
              () => undefined,
            )
          }}
        >
          {copied ? t('chat.copied') : `⧉ ${t('tool.copyCommand')}`}
        </button>
        <button
          type="button"
          className="btn btn-sm btn-ghost"
          aria-label={t('common.close')}
          title={t('common.close')}
          onClick={() => dismiss(chatId)}
        >
          ✕
        </button>
      </div>
      <p className="faint mt-1 text-[11px]">{t('handoff.hint')}</p>
    </div>
  )
}
