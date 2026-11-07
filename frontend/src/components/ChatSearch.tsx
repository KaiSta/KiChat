import { useEffect, useRef } from 'react'
import { useStore } from '../store'
import { Highlighted } from './Highlighted'

/**
 * The find bar over the transcript.
 *
 * It owns no state: the query, the match the user is on and the total come from the chat
 * view, because the same values decide which message is scrolled to and outlined. What
 * lives here is the input and its keys — Enter for the next match, Shift+Enter for the
 * previous one, Escape to close — which is the half a user reaches for without looking.
 */
export function ChatSearch({
  query,
  at,
  total,
  snippet,
  onQuery,
  onNext,
  onPrev,
  onClose,
}: {
  query: string
  /** 0-based index of the match in view, meaningless when `total` is 0. */
  at: number
  total: number
  /** A line around the match in view, empty when the query has no hits. */
  snippet: string
  onQuery: (value: string) => void
  onNext: () => void
  onPrev: () => void
  onClose: () => void
}) {
  const t = useStore((s) => s.t)
  const input = useRef<HTMLInputElement>(null)

  // Opening the bar is the request to type in it, and the previous query is selected so a
  // second Ctrl+F retypes over it rather than appending to it.
  useEffect(() => {
    input.current?.focus()
    input.current?.select()
  }, [])

  const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault()
      if (event.shiftKey) onPrev()
      else onNext()
      return
    }
    if (event.key === 'Escape') {
      event.preventDefault()
      onClose()
    }
  }

  return (
    <div
      role="search"
      className="flex items-center gap-2 border-b px-4 py-1.5"
      style={{ borderColor: 'var(--border)', background: 'var(--surface-2)' }}
    >
      <input
        ref={input}
        className="min-w-0 flex-1 bg-transparent text-[13px] outline-none"
        placeholder={t('chat.search')}
        aria-label={t('chat.search')}
        value={query}
        onChange={(event) => onQuery(event.target.value)}
        onKeyDown={onKeyDown}
      />
      {/* Polite rather than assertive: the counter changes on every keystroke, and a
          screen reader interrupting itself per character would say nothing useful. */}
      <span className="faint mono shrink-0 text-[11px]" aria-live="polite">
        {total > 0
          ? t('chat.searchCount', { '1': String(at + 1), '2': String(total) })
          : query.trim()
            ? t('chat.searchNoResults')
            : ''}
      </span>
      {snippet && (
        <span className="faint min-w-0 flex-1 truncate text-[11px]" title={snippet}>
          <Highlighted text={snippet} query={query} />
        </span>
      )}
      <button
        type="button"
        className="btn btn-sm btn-ghost"
        title={t('chat.searchPrevious')}
        aria-label={t('chat.searchPrevious')}
        disabled={total === 0}
        onClick={onPrev}
      >
        ↑
      </button>
      <button
        type="button"
        className="btn btn-sm btn-ghost"
        title={t('chat.searchNext')}
        aria-label={t('chat.searchNext')}
        disabled={total === 0}
        onClick={onNext}
      >
        ↓
      </button>
      <button
        type="button"
        className="btn btn-sm btn-ghost"
        title={t('chat.searchClose')}
        aria-label={t('chat.searchClose')}
        onClick={onClose}
      >
        ✕
      </button>
    </div>
  )
}
