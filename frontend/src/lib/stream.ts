import { useEffect, useRef, useState } from 'react'

/*
 * Text that is still arriving.
 *
 * A reply streams in deltas, up to twenty a second. Rendering each one is
 * cheap; re-parsing the markdown of the *whole reply* for each one is not,
 * because a parse costs what the text is long. Measured with this app's
 * pipeline (react-markdown + gfm + math + katex + highlight), one streamed
 * reply used to cost:
 *
 *     4 kB   237 ms of parsing     12 kB  1710 ms     24 kB  6068 ms
 *
 * spread over deltas three seconds apart — the webview spent its time in the
 * parser, which is what made long answers stutter and flash. The way out is to
 * parse less per delta, not less often: a throttle alone still re-parses the
 * whole answer, just fewer times a second.
 *
 *   - `splitSettled` cuts the arriving text at its own boundaries, so every
 *     piece that is finished can be rendered once and never again, and only the
 *     piece being written is parsed again — a bounded amount of text per delta
 *     instead of the whole answer. The same three replies now cost 46, 137 and
 *     266 ms.
 *   - `openFence` finds the fenced code block the model is still inside. Such a
 *     block's content is plain text as far as markdown is concerned, so it is
 *     shown as plain text and highlighted once, when the fence closes.
 *     Highlighting it on every delta was the single most expensive thing the
 *     chat view did: 6.7 s of parsing for one 45 kB code answer, now 33 ms and
 *     a single parse.
 *   - `useThrottled` rations what is left of the per-delta work.
 */

interface FenceLine {
  /** The run of backticks or tildes that opens or closes a fence. */
  marker: string
  /** What follows the marker on that line: a language, on an opening fence. */
  info: string
}

/** A line that opens or closes a fence, if it is one. */
function fenceLine(line: string): FenceLine | null {
  const match = /^\s{0,3}(`{3,}|~{3,})(.*)$/.exec(line)
  if (!match) return null
  return { marker: match[1], info: match[2].trim() }
}

/**
 * Splits arriving text into the pieces that can no longer change (`settled`)
 * and the piece still being written (`live`).
 *
 * A boundary is a blank line outside a fenced code block. Markdown gives such a
 * line no meaning that reaches backwards, with one exception: a blank line
 * between two list items is what makes a list "loose", and markdown then spaces
 * the items a little wider. A reply that writes its lists that way is a touch
 * tighter while it streams and exact again once it is finished, because a
 * finished message is parsed as one document.
 *
 * The split is stable as the text grows: a piece, once settled, is the same
 * string inside every longer text that contains it. That is what lets the
 * components memoise on it — a memo that missed would parse the piece again.
 */
export function splitSettled(text: string): { settled: string[]; live: string } {
  const settled: string[] = []
  let live = ''
  // The marker of the fence we are inside, or '' when we are not in one.
  let fence = ''
  const lines = text.split('\n')
  // A text that ends with a newline splits into one more element than it has
  // lines, and that empty last element is not a blank line: mistaking it for
  // one settled the piece being written on every delta that happened to end at
  // a line boundary, and unsettled it again on the next delta.
  if (text.endsWith('\n')) lines.pop()
  for (const line of lines) {
    const current = fenceLine(line)
    if (fence) {
      // Only the same character, at least as long, with nothing behind it
      // closes the fence we are in.
      if (current && current.marker[0] === fence[0] && current.marker.length >= fence.length && current.info === '') {
        fence = ''
      }
    } else if (current) {
      fence = current.marker
    }
    // A blank line settles what came before it — except inside a fence, where
    // it is part of the code.
    if (!fence && live.trim() !== '' && line.trim() === '') {
      settled.push(live)
      live = ''
      continue
    }
    live += line + '\n'
  }
  return { settled, live }
}

/**
 * The fenced code block the text is still inside, if it is inside one.
 *
 * `before` is everything ahead of the opening fence — already settled text that
 * markdown must still read — and `code` is the raw content written so far.
 */
export function openFence(text: string): { before: string; code: string; lang: string } | null {
  const lines = text.split('\n')
  let opened = -1
  let marker = ''
  let lang = ''
  for (let i = 0; i < lines.length; i++) {
    const current = fenceLine(lines[i])
    if (!current) continue
    if (opened === -1) {
      opened = i
      marker = current.marker
      lang = current.info.split(/\s+/)[0] ?? ''
      continue
    }
    if (current.marker[0] === marker[0] && current.marker.length >= marker.length && current.info === '') {
      opened = -1
      marker = ''
      lang = ''
    }
  }
  if (opened === -1) return null
  return { before: lines.slice(0, opened).join('\n'), code: lines.slice(opened + 1).join('\n'), lang }
}

/**
 * Throttle a value that changes faster than it is worth rendering, committing
 * at most every `ms` while `active`, and the final value immediately once it is
 * not.
 *
 * The value has to be consumed by a memoised child rather than used next to
 * this call. A component re-renders whenever its props change, so work done in
 * its body runs once per delta whatever this hook decides, and a throttle would
 * only add a second render on top of that — which is how this app managed to
 * parse every reply twice per delta.
 */
export function useThrottled(value: string, active: boolean, ms = 90): string {
  const [shown, setShown] = useState(value)
  const last = useRef(0)
  const timer = useRef<number | null>(null)

  useEffect(() => {
    if (!active) {
      if (timer.current !== null) {
        window.clearTimeout(timer.current)
        timer.current = null
      }
      setShown(value)
      return
    }
    const now = Date.now()
    const elapsed = now - last.current
    if (elapsed >= ms) {
      last.current = now
      setShown(value)
      return
    }
    if (timer.current === null) {
      timer.current = window.setTimeout(() => {
        timer.current = null
        last.current = Date.now()
        setShown(value)
      }, ms - elapsed)
    }
    return () => {
      if (timer.current !== null) {
        window.clearTimeout(timer.current)
        timer.current = null
      }
    }
  }, [value, active, ms])

  return shown
}
