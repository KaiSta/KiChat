import { memo, useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeKatex from 'rehype-katex'
import rehypeHighlight from 'rehype-highlight'
import { App } from '../lib/api'
import { useStore } from '../store'

/*
 * Raw HTML is deliberately NOT enabled. Scraped pages reach the model's context
 * and can be echoed back in a reply; rendering their markup would turn a
 * research tool into an injection surface. Markdown is plenty.
 */

/**
 * Throttle a rapidly changing value while it is live. Markdown parsing is the
 * expensive part of streaming, so during generation the text is committed at
 * most every `ms`; once streaming stops the final value is applied immediately.
 */
function useThrottled(value: string, active: boolean, ms = 90): string {
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

export const Markdown = memo(function Markdown({ text, streaming = false }: MarkdownProps) {
  const shown = useThrottled(text, streaming)

  return (
    <div className="md">
      <ReactMarkdown
        remarkPlugins={[remarkGfm, remarkMath]}
        rehypePlugins={[rehypeKatex, rehypeHighlight]}
        components={{
          pre: CodeBlock as never,
          // Links must open in the user's browser: letting the webview navigate
          // would replace the entire app UI with the target page.
          a: ({ href, children, ...rest }) => (
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
          ),
        }}
      >
        {shown}
      </ReactMarkdown>
      {streaming && shown !== text ? <span className="streaming-cursor" /> : null}
    </div>
  )
})
