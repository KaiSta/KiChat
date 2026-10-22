import { useMemo, type ReactNode } from 'react'
import { matchRanges } from '../lib/modelSearch'

/**
 * The parts of `text` the query matched, marked so a row explains itself.
 *
 * Ranges come from the raw text and are already merged, so a query of two adjacent
 * terms cannot produce overlapping marks.
 */
export function Highlighted({ text, query }: { text: string; query: string }) {
  const ranges = useMemo(() => matchRanges(text, query), [text, query])
  if (ranges.length === 0) return <>{text}</>

  const parts: ReactNode[] = []
  let at = 0
  ranges.forEach(([start, end], i) => {
    if (start > at) parts.push(text.slice(at, start))
    parts.push(
      <mark
        key={i}
        style={{
          background: 'color-mix(in srgb, var(--accent) 28%, transparent)',
          color: 'inherit',
          borderRadius: 3,
          padding: 0,
        }}
      >
        {text.slice(start, end)}
      </mark>,
    )
    at = end
  })
  if (at < text.length) parts.push(text.slice(at))
  return <>{parts}</>
}
