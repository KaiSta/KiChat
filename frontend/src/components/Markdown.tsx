import { memo, useMemo, useRef, useState } from 'react'
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeKatex from 'rehype-katex'
import rehypeHighlight from 'rehype-highlight'
import { openFence, splitSettled, useThrottled } from '../lib/stream'
import { App } from '../lib/api'
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
}

export const Markdown = memo(function Markdown({ text, streaming = false }: MarkdownProps) {
  return (
    <div className="md">
      {/*
        A finished message is one document again, so the markdown that reaches
        the screen is exactly what the spec says it is: a list written with
        blank lines between its items stays one loose list, `[text][ref]`
        resolves against a definition further down, footnotes collect at the
        end. Streaming splits the text, and a split cannot promise those.
      */}
      {streaming ? <StreamingMarkdown text={text} /> : <Block source={text} />}
    </div>
  )
})

/**
 * A reply that is still arriving, in the two parts `stream.ts` describes: the
 * pieces that are finished and can be rendered once, and the piece being
 * written.
 */
function StreamingMarkdown({ text }: { text: string }) {
  // Re-splitting is a cheap scan; the parse the pieces trigger is not, and that
  // is what the memo inside Block keeps from happening per delta.
  const { settled, live } = useMemo(() => splitSettled(text), [text])
  return (
    <>
      {/* An index is the right key here: pieces only ever get appended, so the
          key of a piece never changes and its memo keeps holding. */}
      {settled.map((chunk, index) => (
        <Block key={index} source={chunk} />
      ))}
      <LivePiece source={live} />
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
function LivePiece({ source }: { source: string }) {
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
        {fence.before.trim() ? <Block source={fence.before} /> : null}
        <CodeBlock>
          <code className={language}>{fence.code}</code>
        </CodeBlock>
      </>
    )
  }
  return <Block source={shown} />
}

/**
 * One piece of a reply, parsed once. This memo is the fix, not an optimisation:
 * a piece the reply has already finished writing never changes again, so
 * parsing it again — which is what happened on every delta — is pure waste that
 * grows with the length of the answer.
 */
const Block = memo(function Block({ source }: { source: string }) {
  return (
    <ReactMarkdown remarkPlugins={REMARK_PLUGINS} rehypePlugins={REHYPE_PLUGINS} components={COMPONENTS}>
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
}

