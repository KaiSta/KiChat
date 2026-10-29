import { useState } from 'react'
import { App as Api } from '../lib/api'
import { copyText } from '../lib/clipboard'
import { errText, useStore } from '../store'

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
 * Three steps, one card: open a terminal in the project folder, run the line there, paste the
 * output back. The hint names the last one because copying alone leaves the answer exactly
 * where it was — the model has no way to know the command ran.
 *
 * "Open terminal" hands the folder to the *user's* terminal emulator (see
 * internal/runtime/terminal.go) rather than growing one here. Two reasons: a terminal embedded
 * in this window would have to answer a password prompt, which is exactly the thing that got
 * refused; and a terminal started by the app is one the app would then be responsible for
 * keeping, which is a window somebody may keep open all afternoon. So the button opens the
 * program they already trust and stops there — the line still gets typed or pasted by them.
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
  // Held for the length of the call, because the call starts a program: a second click on a
  // button that shows nothing for a moment is a second terminal in the same folder.
  const [opening, setOpening] = useState(false)

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
            // A refused copy used to be swallowed, leaving a button that said nothing and a
            // person pasting nothing into their terminal. The clipboard is a permission in a
            // webview, so the refusal is a state this can reach; the sentence says what to do
            // instead, which is select the line and copy it by hand.
            void copyText(handoff.command).then(
              () => {
                setCopied(true)
                window.setTimeout(() => setCopied(false), 1500)
              },
              () => useStore.getState().toast('error', t('chat.copyFailed')),
            )
          }}
        >
          {copied ? t('chat.copied') : `⧉ ${t('tool.copyCommand')}`}
        </button>
        {/* Beside the copy button rather than a second row: the two together are the whole
            hand-off, and a person who has just read the line wants both within reach. */}
        <button
          type="button"
          className="btn btn-sm"
          disabled={opening}
          onClick={() => {
            setOpening(true)
            // The sentence follows the call rather than preceding it: a failure is the
            // interesting half, and a window that takes a second to appear should not be
            // announced before it exists.
            void Api.OpenProjectTerminal(chatId)
              .then(
                () => useStore.getState().toast('info', t('handoff.terminalOpened')),
                (err: unknown) => useStore.getState().toast('error', errText(err)),
              )
              .finally(() => setOpening(false))
          }}
        >
          {t('handoff.openTerminal')}
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
