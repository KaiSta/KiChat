/*
 * Pins what the chat view shows while a reply is still arriving.
 *
 * `frontend/src/lib/stream.ts` splits the arriving text into the pieces that are
 * finished and the piece being written, so that a finished piece is parsed once
 * instead of on every delta — without that, a long answer costs seconds of
 * markdown parsing and the text stutters. A split can go wrong in ways the eye
 * only notices as a flicker, so this renders every prefix of a few replies both
 * ways and compares what a browser would show:
 *
 *   - once as a finished message (one document), and
 *   - once as an arriving one (settled pieces + live piece + plain text inside
 *     an open code fence + the cursor), the way Markdown.tsx renders it.
 *
 * It fails when the two differ, when the pieces would change as the text grows
 * (the memo depends on their being append-only), or when a line is lost in the
 * split. It found the trailing newline of a delta being read as a blank line,
 * which settled the piece being written on every delta that ended at a line
 * boundary and unsettled it again on the next one.
 *
 *   node tools/streamcheck/check.mjs
 *
 * Needs Node 22.18+ (it imports the TypeScript module directly) and the
 * frontend's node_modules: `cd frontend && npm install` first.
 */
import { createRequire } from 'node:module'

const frontend = new URL('../../frontend/', import.meta.url)
const req = createRequire(new URL('package.json', frontend))
const React = req('react')
const { renderToStaticMarkup } = req('react-dom/server')
const load = (name) => import(new URL(`node_modules/${name}`, frontend).href)
const ReactMarkdown = (await load('react-markdown/index.js')).default
const remarkGfm = (await load('remark-gfm/index.js')).default
const remarkMath = (await load('remark-math/index.js')).default
const rehypeKatex = (await load('rehype-katex/index.js')).default
const rehypeHighlight = (await load('rehype-highlight/index.js')).default
const { openFence, splitSettled } = await import(new URL('src/lib/stream.ts', frontend).href)

const h = React.createElement
const components = { pre: ({ children }) => h('pre', null, children) }

function renderMarkdown(text) {
  return renderToStaticMarkup(
    h(
      ReactMarkdown,
      { remarkPlugins: [remarkGfm, remarkMath], rehypePlugins: [rehypeKatex, rehypeHighlight], components },
      text,
    ),
  )
}

/** What Markdown.tsx renders for a reply that is still arriving. */
function renderStreaming(text) {
  const { settled, live } = splitSettled(text)
  const parts = settled.map((chunk) => renderMarkdown(chunk))
  const fence = openFence(live)
  if (fence) {
    if (fence.before.trim()) parts.push(renderMarkdown(fence.before))
    const cls = fence.lang ? `hljs language-${fence.lang}` : ''
    parts.push(
      renderToStaticMarkup(
        h('div', { className: 'relative group' }, h('pre', null, h('code', { className: cls }, fence.code))),
      ),
    )
  } else {
    parts.push(renderMarkdown(live))
  }
  parts.push('<span class="streaming-cursor"></span>')
  // Each part is a block, so block layout puts them on separate lines — which is
  // the newline markdown itself writes between the blocks of one document.
  return parts.join('\n')
}

/** The text a browser would show: tags off, entities decoded, space collapsed. */
function visible(html) {
  return html
    .replace(/<[^>]*>/g, '')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#x27;|&#39;/g, "'")
    .replace(/&amp;/g, '&')
    .replace(/\s+/g, ' ')
    .trim()
}

/* ------------------------------------------------------------------ samples */

const PROSE = `## Ergebnis

Die Auswertung zeigt **drei** Muster, die sich in allen Läufen wiederholen.

- Das erste Muster betrifft die Vorverarbeitung.
- Das zweite Muster betrifft die Fenstergröße.
- Das dritte Muster ist eine Folge der beiden anderen.

Im Einzelnen gilt $x_t \\in \\mathbb{R}^{L}$ für jedes Fenster, und die
Normalisierung erfolgt pro Kanal.

> Die Aussage gilt nur für den getesteten Datensatz.

| Modell | MSE | Parameter |
| --- | --- | --- |
| PatchTST | 0.21 | 1.2M |
| DLinear | 0.28 | 0.01M |

Ein Beispiel:

\`\`\`python
import torch

def normalise(x: torch.Tensor) -> torch.Tensor:
    # a < b on purpose: the escape has to survive the round trip
    return (x - x.mean(dim=1, keepdim=True)) / (x.std(dim=1, keepdim=True) + 1e-5)
\`\`\`

Siehe [die Quelle](https://example.com/a) und \`inline code\` im Satz.

### Offene Fragen

1. Gilt das auch für kurze Reihen?
2. Wie viel kostet die Inferenz auf CPU?
`

// Tilde fences, a four-backtick fence around a three-backtick one, and a blank
// line inside a fence: the three shapes a naive splitter gets wrong.
const FENCES = `Einleitung mit Text vor dem Code.

~~~js
const a = 1
if (a < 2) { console.log('a & b') }
~~~

Danach noch ein Absatz, der länger ist als der vorige.

\`\`\`\`markdown
\`\`\`python
print('verschachtelter Zaun')
\`\`\`
\`\`\`\`
`

const LOOSE_LIST = `Punkte:

- erster Punkt

- zweiter Punkt

- dritter Punkt
`

// Documented difference: a reference used in one piece is defined in another, so
// while the reply streams the link stays literal text. It resolves in the final
// render, which parses the whole message again as one document.
const REFERENCES = `Siehe [die Studie][1] für Details.

[1]: https://example.com/studie
`

const MUST_MATCH = [
  ['prose, code, table, math', PROSE],
  ['tilde fence and nested fence', FENCES],
  ['loose list', LOOSE_LIST],
]

let failures = 0

/* ------------------------------------------------------------------- checks */

for (const [name, sample] of MUST_MATCH) {
  const diffs = []
  // Every prefix, close enough together that a boundary case — a delta ending
  // on a newline, a fence opening — is covered rather than stepped over.
  for (let end = 1; end <= sample.length; end += Math.max(4, Math.floor(sample.length / 120))) {
    const prefix = sample.slice(0, end)
    const whole = visible(renderMarkdown(prefix))
    const streaming = visible(renderStreaming(prefix))
    if (whole !== streaming) diffs.push({ end, whole, streaming })
  }
  if (diffs.length === 0) {
    console.log(`ok   ${name}: ${sample.length} chars, visible text identical at every prefix`)
    continue
  }
  failures++
  console.log(`FAIL ${name}: ${diffs.length} prefixes show different text (first at ${diffs[0].end} chars)`)
  const [d] = diffs
  let at = 0
  while (at < d.whole.length && d.whole[at] === d.streaming[at]) at++
  console.log(`  finished:  …${d.whole.slice(Math.max(0, at - 40), at + 40)}`)
  console.log(`  streaming: …${d.streaming.slice(Math.max(0, at - 40), at + 40)}`)
}

{
  // The one difference that is accepted, and pinned as such: while the reply
  // streams, a reference-style link is shown as it was written instead of
  // resolved, so "die Studie" carries its "[…][1]" until the reply is finished.
  let onlyLiteralReferences = true
  let seen = false
  for (let end = 1; end <= REFERENCES.length; end += 4) {
    const prefix = REFERENCES.slice(0, end)
    const whole = visible(renderMarkdown(prefix))
    const streaming = visible(renderStreaming(prefix))
    if (whole === streaming) continue
    seen = true
    if (streaming.replace('[die Studie][1]', 'die Studie') !== whole) {
      onlyLiteralReferences = false
      failures++
      console.log(`FAIL reference definitions: a difference other than the unresolved link at prefix ${end}`)
      console.log(`  finished:  ${JSON.stringify(whole)}`)
      console.log(`  streaming: ${JSON.stringify(streaming)}`)
      break
    }
  }
  if (onlyLiteralReferences) {
    console.log(`ok   reference definitions: ${seen ? 'only the not-yet-resolved' : 'no'} [text][ref], as documented`)
  }
}

for (const [name, sample] of [...MUST_MATCH, ['reference definitions', REFERENCES]]) {
  let previous = []
  let broke = -1
  let lost = false
  // Char by char: the split has to be append-only at *every* point, and the
  // interesting points are exactly the ones a coarser step would walk past.
  for (let end = 1; end <= sample.length; end += 1) {
    const { settled } = splitSettled(sample.slice(0, end))
    // Pieces only ever get appended: a memo that missed would parse a piece
    // again, and one that shifted would render the text twice.
    if (settled.length < previous.length || !previous.every((chunk, i) => settled[i] === chunk)) broke = end
    previous = settled
  }
  const split = splitSettled(sample)
  const nonBlank = (text) => text.split('\n').filter((line) => line.trim() !== '').join('\n')
  if (nonBlank([...split.settled, split.live].join('')) !== nonBlank(sample)) lost = true
  if (broke === -1 && !lost) {
    console.log(`ok   ${name}: pieces append-only as the text grows, no line lost`)
    continue
  }
  failures++
  console.log(`FAIL ${name}: ${broke === -1 ? '' : `pieces changed at ${broke} chars `}${lost ? 'lines lost in the split' : ''}`)
  if (broke !== -1) {
    const before = splitSettled(sample.slice(0, broke - 1)).settled
    const after = splitSettled(sample.slice(0, broke)).settled
    const at = before.findIndex((chunk, i) => after[i] !== chunk)
    console.log(`  text: ${JSON.stringify(sample.slice(broke - 40, broke + 4))}`)
    console.log(`  piece ${at} before: ${JSON.stringify(before[at])}`)
    console.log(`  piece ${at} after:  ${JSON.stringify(after[at] ?? null)}`)
  }
}

console.log(failures === 0 ? '\nthe streaming renderer shows what the finished one does' : `\n${failures} check(s) failed`)
process.exit(failures === 0 ? 0 : 1)

