import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { Virtuoso, type VirtuosoHandle } from 'react-virtuoso'
import { OnFileDrop, OnFileDropOff } from '../../wailsjs/runtime/runtime'
import { App, formatNumber, type store as storeTypes } from '../lib/api'
import { messageText, pendingAttachments, useStore } from '../store'
import { ContextInspector, ContextMeter, ContextRecheck, ModelSwitcher } from './ModelSwitcher'
import { AttachmentChips, CompactionCard, MessageStats, ReasoningBlock, ToolCallCard } from './MessageParts'
import { Markdown } from './Markdown'

type Item =
  | { kind: 'message'; key: string; message: storeTypes.Message }
  | { kind: 'compaction'; key: string; compaction: storeTypes.Compaction }

export function ChatView() {
  const t = useStore((s) => s.t)
  const chatState = useStore((s) => s.chatState)
  const activeChatId = useStore((s) => s.activeChatId)
  const chatLoading = useStore((s) => s.chatLoading)
  const running = useStore((s) => (s.activeChatId ? !!s.running[s.activeChatId] : false))
  const phase = useStore((s) => (s.activeChatId ? s.phase[s.activeChatId] : undefined))
  const streaming = useStore((s) => s.streaming)
  const send = useStore((s) => s.send)
  const stop = useStore((s) => s.stop)
  const toast = useStore((s) => s.toast)

  const [input, setInput] = useState('')
  const [inspectorOpen, setInspectorOpen] = useState(false)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const listRef = useRef<VirtuosoHandle>(null)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  // Two things about this line are load-bearing:
  //  * It must stay above the conditional returns below. A hook that runs on
  //    some renders but not others changes the hook count and makes React throw
  //    #310, which unmounts the whole tree and leaves a blank window.
  //  * useShallow is required because the selector builds a new array on every
  //    call. Without it useSyncExternalStore sees a fresh snapshot each time and
  //    React spins in an infinite update loop (#185), which blanks the window
  //    the same way.
  const pending = useStore(useShallow(pendingAttachments))

  /* Files can be dropped anywhere on the window. Wails hands us real paths,
   * which is why the native drop handler is used instead of the HTML5 one. */
  useEffect(() => {
    OnFileDrop((_x: number, _y: number, paths: string[]) => {
      void addFiles(paths)
    }, false)
    return () => OnFileDropOff()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeChatId])

  const addFiles = useCallback(
    async (paths: string[]) => {
      if (!activeChatId) {
        toast('warning', t('nav.noChats'))
        return
      }
      for (const path of paths) {
        try {
          const attachment = await App.AddAttachment({ chatId: activeChatId, path, placement: '' })
          if (attachment.warning) toast('warning', `${attachment.filename}: ${attachment.warning}`)
        } catch (err) {
          toast('error', `${path}: ${String(err)}`)
        }
      }
      await useStore.getState().refreshChatState()
    },
    [activeChatId, toast, t],
  )

  // Folded history: everything up to the newest compaction's end.
  const foldedUpTo = useMemo(() => {
    const list = chatState?.compactions ?? []
    return list.reduce((max, c) => Math.max(max, c.toSeq), 0)
  }, [chatState?.compactions])

  const items = useMemo<Item[]>(() => {
    if (!chatState) return []
    const out: Item[] = []
    const byBoundary = new Map<number, storeTypes.Compaction>()
    for (const c of chatState.compactions ?? []) byBoundary.set(c.toSeq, c)

    const hidden = new Set<string>()
    for (const [id, isCollapsed] of Object.entries(collapsed)) {
      if (!isCollapsed) continue
      const c = chatState.compactions.find((x) => x.id === id)
      if (!c) continue
      for (const m of chatState.messages) {
        if (m.seq >= c.fromSeq && m.seq <= c.toSeq) hidden.add(m.id)
      }
    }

    for (const m of chatState.messages) {
      if (hidden.has(m.id)) continue
      out.push({ kind: 'message', key: m.id, message: m })
      const compaction = byBoundary.get(m.seq)
      if (compaction) {
        out.push({ kind: 'compaction', key: `c-${compaction.id}`, compaction })
      }
    }
    return out
  }, [chatState, collapsed])

  const submit = async () => {
    const text = input.trim()
    if (!text || running) return
    setInput('')
    await send(text)
    // Keep the caret where the user is already looking.
    textareaRef.current?.focus()
  }

  const onKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      void submit()
    }
    // Escape stops a running generation, matching the Stop button.
    if (event.key === 'Escape' && running) {
      event.preventDefault()
      void stop()
    }
  }

  if (!activeChatId) {
    return (
      <div className="flex flex-1 items-center justify-center">
        <div className="text-center">
          <div className="mb-3 text-lg">{t('chat.noChat')}</div>
          <button type="button" className="btn btn-primary" onClick={() => void useStore.getState().newChat()}>
            {t('nav.newChat')}
          </button>
        </div>
      </div>
    )
  }

  if (chatLoading && !chatState) {
    return <div className="flex flex-1 items-center justify-center faint">{t('common.loading')}</div>
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {/* Title bar for the chat */}
      <div className="flex items-center gap-2 border-b px-4 py-2" style={{ borderColor: 'var(--border)' }}>
        <input
          className="min-w-0 flex-1 bg-transparent text-[15px] font-medium outline-none"
          value={chatState?.chat.title ?? ''}
          placeholder={t('chat.empty.title')}
          onChange={(e) =>
            useStore.setState((s) =>
              s.chatState ? { chatState: { ...s.chatState, chat: { ...s.chatState.chat, title: e.target.value } } } : {},
            )
          }
          onBlur={(e) => void useStore.getState().renameChat(activeChatId, e.target.value)}
        />
        <SearchFirstToggle />
      </div>

      {/* Messages */}
      <div className="min-h-0 flex-1">
        {items.length === 0 ? (
          <div className="flex h-full items-center justify-center p-8">
            <div className="max-w-md text-center">
              <div className="mb-2 text-lg font-medium">{t('chat.empty.title')}</div>
              <p className="muted text-[13px]">{t('chat.empty.hint')}</p>
            </div>
          </div>
        ) : (
          <Virtuoso
            ref={listRef}
            data={items}
            // followOutput keeps the newest tokens in view while streaming.
            followOutput="auto"
            initialTopMostItemIndex={Math.max(0, items.length - 1)}
            increaseViewportBy={{ top: 400, bottom: 600 }}
            computeItemKey={(_i, item) => item.key}
            itemContent={(_index, item) =>
              item.kind === 'message' ? (
                <div className="px-4 pb-1">
                  <MessageRow
                    message={item.message}
                    streaming={streaming}
                    folded={item.message.seq <= foldedUpTo}
                  />
                </div>
              ) : (
                <div className="px-4">
                  <CompactionCard
                    compaction={item.compaction}
                    collapsed={!!collapsed[item.compaction.id]}
                    onToggle={() =>
                      setCollapsed((prev) => ({ ...prev, [item.compaction.id]: !prev[item.compaction.id] }))
                    }
                  />
                </div>
              )
            }
          />
        )}
      </div>

      {/* Composer */}
      <div className="border-t px-4 py-3" style={{ borderColor: 'var(--border)' }}>
        {pending.length > 0 && (
          <div className="mb-2 flex flex-wrap items-center gap-1.5">
            {pending.map((a) => (
              <span key={a.id} className="badge" title={a.warning || undefined}>
                <span>{a.filename}</span>
                <span className="faint">
                  {a.placement === 'indexed' ? t('files.indexed') : t('files.inline')}
                </span>
                {a.placement === 'inline' && a.tokenEstimate > 0 && (
                  <span className="faint">{formatNumber(a.tokenEstimate)} {t('files.tokens')}</span>
                )}
                <button
                  type="button"
                  className="ml-1"
                  title={t('files.remove')}
                  onClick={async () => {
                    await App.RemoveAttachment(a.id)
                    await useStore.getState().refreshChatState()
                  }}
                >
                  ✕
                </button>
              </span>
            ))}
          </div>
        )}

        {phase && phase !== 'idle' && (
          <div className="mb-1 text-[11px]" style={{ color: 'var(--accent)' }}>
            {phase === 'compacting'
              ? t('compact.running')
              : phase === 'tool'
                ? t('chat.toolCalls')
                : phase === 'stopping'
                  ? t('chat.stopping')
                  : phase}
          </div>
        )}

        <div className="flex items-end gap-2">
          <textarea
            ref={textareaRef}
            className="textarea min-h-[42px] max-h-40 flex-1"
            rows={1}
            placeholder={t('chat.placeholder')}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={onKeyDown}
            disabled={running && phase === 'compacting'}
          />
          {running ? (
            <button type="button" className="btn btn-danger h-[42px]" onClick={() => void stop()}>
              ⏹ {t('chat.stop')}
            </button>
          ) : (
            <button
              type="button"
              className="btn btn-primary h-[42px]"
              disabled={!input.trim()}
              onClick={() => void submit()}
            >
              {t('chat.send')}
            </button>
          )}
        </div>

        <div className="mt-2 flex flex-wrap items-center gap-2">
          <ModelSwitcher />
          <button
            type="button"
            className="btn btn-sm"
            onClick={async () => {
              const paths = await App.PickFiles()
              if (paths && paths.length > 0) await addFiles(paths)
            }}
          >
            📎 {t('files.add')}
          </button>
          <div className="relative">
            <ContextMeter onOpen={() => setInspectorOpen((v) => !v)} />
            {inspectorOpen && <ContextInspector onClose={() => setInspectorOpen(false)} />}
          </div>
          <ContextRecheck />
          <span className="faint ml-auto text-[11px]">{t('files.dropHint')}</span>
        </div>
      </div>
    </div>
  )
}

/* -------------------------------------------------------------------------
 * One message
 * ---------------------------------------------------------------------- */

function MessageRow({
  message,
  streaming,
  folded,
}: {
  message: storeTypes.Message
  streaming: Record<string, { text: string; reasoning: string }>
  folded: boolean
}) {
  const t = useStore((s) => s.t)
  const live = messageText(message, streaming)
  const isUser = message.role === 'user'
  const isTool = message.role === 'tool'

  if (isTool) {
    // Tool results are shown inside their call card, not as their own bubble.
    return null
  }

  // A pre-flight search is injected as a system message. It is context, not
  // conversation, so it renders as a collapsible source card.
  if (message.role === 'system') {
    return (
      <details className="collapsible my-2 text-[12px]">
        <summary className="faint">
          {message.name === 'search' ? `🔎 ${t('search.results')}` : t('chat.system')}
        </summary>
        <pre
          className="mt-1 max-h-64 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
          style={{ background: 'var(--surface-2)', color: 'var(--text-dim)' }}
        >
          {message.content}
        </pre>
      </details>
    )
  }

  return (
    <div className="group py-2" style={folded ? { opacity: 0.72 } : undefined}>
      <div className="mb-1 flex items-center gap-2 text-[11px]">
        <span className="font-semibold" style={{ color: isUser ? 'var(--text-dim)' : 'var(--accent)' }}>
          {isUser ? t('chat.you') : t('chat.assistant')}
        </span>
        {!isUser && message.model && <span className="faint mono">{message.model}</span>}
        {folded && <span className="badge" title={t('compact.title')}>🗜</span>}
        <CopyButton text={live.text} />
      </div>

      <div
        className="rounded-lg px-3 py-2"
        style={
          isUser
            ? { background: 'var(--user-bubble)', border: '1px solid var(--border)' }
            : { background: 'var(--surface)', border: '1px solid var(--border)' }
        }
      >
        <AttachmentChips attachments={message.attachments ?? []} />
        {!isUser && <ReasoningBlock text={live.reasoning} streaming={live.streaming} />}

        {(message.toolCalls ?? []).length > 0 && (
          <div className="mb-1">
            {(message.toolCalls ?? []).map((call) => (
              <ToolCallCard key={call.id} call={call} />
            ))}
          </div>
        )}

        {live.text ? (
          <Markdown text={live.text} streaming={live.streaming} />
        ) : live.streaming ? (
          <span className="streaming-cursor" />
        ) : (
          <span className="faint">—</span>
        )}
      </div>

      <MessageStats message={message} />
    </div>
  )
}

function CopyButton({ text }: { text: string }) {
  const t = useStore((s) => s.t)
  const [copied, setCopied] = useState(false)
  if (!text) return null
  return (
    <button
      type="button"
      className="btn btn-sm btn-ghost opacity-0 transition-opacity group-hover:opacity-100"
      onClick={() => {
        navigator.clipboard.writeText(text).then(
          () => {
            setCopied(true)
            window.setTimeout(() => setCopied(false), 1500)
          },
          () => undefined,
        )
      }}
    >
      {copied ? t('chat.copied') : t('chat.copy')}
    </button>
  )
}

function SearchFirstToggle() {
  const t = useStore((s) => s.t)
  const chatState = useStore((s) => s.chatState)
  const updateChat = useStore((s) => s.updateChat)
  if (!chatState) return null
  const on = !!chatState.chat.searchFirst
  return (
    <button
      type="button"
      className={`btn btn-sm ${on ? 'btn-primary' : ''}`}
      title={t('search.searchFirst')}
      onClick={() => void updateChat({ searchFirst: !on })}
    >
      🔎 {t('search.searchFirst')}
    </button>
  )
}
