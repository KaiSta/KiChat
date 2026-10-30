import type { DiffHunk, DiffLine } from '../lib/api'
import { useStore } from '../store'

/**
 * A change as the hunks that show it.
 *
 * This is what replaced the two whole texts an approval used to carry. Those were honest and
 * unreadable: the change a coding agent makes most often is one line inside a four-hundred-line
 * file, and two pages that differ by a character somewhere in the middle is a card nobody can
 * check. A hunk is that line with the three around it that say which function it is in, and the
 * unchanged span between two hunks is not drawn at all — the row of dots is where it was.
 *
 * Line numbers are on both sides because they are what makes the card usable after the fact:
 * "line 200 of `internal/store/store.go`" is a sentence a person can act on, and a diff without
 * numbers is a picture of a change rather than a place to go and look.
 *
 * It is one component for the confirmation card and for the changes panel because the two are
 * showing the same thing — the panel's diff is against the file as it is now rather than against
 * the previous write — and two renderings would drift the moment one of them was touched.
 */
export function HunkDiff({ hunks, newlineOnly }: { hunks: DiffHunk[]; newlineOnly?: boolean }) {
  const t = useStore((s) => s.t)

  // No rows to draw, and two different reasons for it: a model that rewrote a file with the
  // text it already had (or created an empty one), and a change that is only the newline at the
  // end of the file. A line diff cannot show the second one, and a card claiming "no line
  // differs" over a file whose last byte changed would be saying something false.
  if (hunks.length === 0) {
    return (
      <div className="faint text-[11px]">
        {t(newlineOnly ? 'change.newlineOnly' : 'change.noLines')}
      </div>
    )
  }

  return (
    <div className="mono text-[11px] leading-[1.45]">
      {hunks.map((hunk, index) => (
        <div key={index}>
          {/* Where an unchanged stretch was left out. The numbers on the next row say how far
              the jump is, and the dots say there is a jump — without them two hunks look like
              consecutive lines. */}
          {index > 0 && (
            <div className="faint select-none py-0.5 text-[10px]" aria-hidden="true">
              ⋯
            </div>
          )}
          {hunk.lines.map((line, row) => (
            <Row key={row} line={line} />
          ))}
        </div>
      ))}
    </div>
  )
}

/** One row: its number on each side, the sign, and the line itself. */
function Row({ line }: { line: DiffLine }) {
  const added = line.kind === 'added'
  const removed = line.kind === 'removed'
  // The sign carries the colour and the text does not: a red line is a line that goes, and the
  // text still has to be read as the code it is.
  const tone = added ? 'var(--ok)' : removed ? 'var(--danger)' : 'var(--text-dim)'

  return (
    <div className="flex whitespace-pre">
      {/* 12 units of the 11px mono face is six digits: a right-aligned number wider than its
          gutter spills into the one beside it, and two numbers running together read as one. */}
      <span
        className="w-12 shrink-0 select-none pr-2 text-right"
        style={{ color: 'var(--text-dim)', opacity: line.old === 0 ? 0 : 0.85 }}
      >
        {line.old === 0 ? '' : line.old}
      </span>
      <span
        className="w-12 shrink-0 select-none pr-2 text-right"
        style={{ color: 'var(--text-dim)', opacity: line.new === 0 ? 0 : 0.85 }}
      >
        {line.new === 0 ? '' : line.new}
      </span>
      <span className="w-4 shrink-0 select-none" style={{ color: tone }}>
        {added ? '+' : removed ? '−' : ' '}
      </span>
      {/* A blank line still needs a row: an empty span collapses and the row would read as a
          missing line rather than as the blank one it is. */}
      <span style={{ color: 'var(--text)' }}>{line.text === '' ? ' ' : line.text}</span>
    </div>
  )
}
