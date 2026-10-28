import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { Virtuoso } from 'react-virtuoso'
import { OnFileDrop, OnFileDropOff } from '../../wailsjs/runtime/runtime'
import { App, folderName, formatNumber, modelLabel, type config, type store as storeTypes } from '../lib/api'
import { type MessageKey } from '../lib/i18n'
import { messageText, pendingAttachments, useStore } from '../store'
import {
  ContextInspector,
  ContextMeter,
  ContextRecheck,
  MissingModelNotice,
  ModelSwitcher,
} from './ModelSwitcher'
import {
  AttachmentChips,
  CompactionCard,
  MessageImages,
  MessageStats,
  ReasoningBlock,
  ToolCallCard,
} from './MessageParts'
import { Markdown } from './Markdown'
import { Confirmation } from './Confirmation'
import { Handoff } from './Handoff'

type Item =
  | { kind: 'message'; key: string; message: storeTypes.Message }
  | { kind: 'compaction'; key: string; compaction: storeTypes.Compaction }

/** The one slash command the composer answers itself: empty this conversation, keep the chat. */
const CLEAR_COMMAND = '/clear'

export function ChatView() {
  const t = useStore((s) => s.t)
  const chatState = useStore((s) => s.chatState)
  const activeChatId = useStore((s) => s.activeChatId)
  const chatLoading = useStore((s) => s.chatLoading)
  const running = useStore((s) => (s.activeChatId ? !!s.running[s.activeChatId] : false))
  const phase = useStore((s) => (s.activeChatId ? s.phase[s.activeChatId] : undefined))
  const phaseWaitMs = useStore((s) => (s.activeChatId ? (s.phaseWaitMs[s.activeChatId] ?? 0) : 0))
  const send = useStore((s) => s.send)
  const stop = useStore((s) => s.stop)
  const toast = useStore((s) => s.toast)

  const [input, setInput] = useState('')
  const [inspectorOpen, setInspectorOpen] = useState(false)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  // Whether the newest content should be followed, the element that shows it, and the
  // furthest down the view has been. All three are refs: they belong to scroll and height
  // callbacks, and none of that should cost a render of the transcript.
  const following = useRef(true)
  const scroller = useRef<HTMLElement | null>(null)
  const lowestOffset = useRef(0)
  // Two things about this line are load-bearing:
  //  * It must stay above the conditional returns below. A hook that runs on
  //    some renders but not others changes the hook count and makes React throw
  //    #310, which unmounts the whole tree and leaves a blank window.
  //  * useShallow is required because the selector builds a new array on every
  //    call. Without it useSyncExternalStore sees a fresh snapshot each time and
  //    React spins in an infinite update loop (#185), which blanks the window
  //    the same way.
  const pending = useStore(useShallow(pendingAttachments))
  // What the composer's permission note has to say. Read from the stored settings rather
  // than from the chat, because the answer is the same for every chat in the window.
  const autoApprove = useStore((s) => s.settings?.autoApprove)

  /* What a screen reader hears when a turn ends.
   *
   * A live region over the transcript would read every delta of a streaming answer,
   * and scrolling a virtualised list mounts messages the reader is only passing on
   * the way to a different one — both are noise. So the list is a plain `log` with
   * announcements off, and the finished reply is read once, when `running` drops.
   * Captured from `content` rather than the stream buffer: by the time the flag is
   * false the store has the settled message, and that is the text worth hearing.
   */
  const [announcement, setAnnouncement] = useState('')
  const wasRunning = useRef(false)
  useEffect(() => {
    if (wasRunning.current && !running) {
      const messages = chatState?.messages ?? []
      const last = [...messages].reverse().find((m) => m.role === 'assistant' && m.content)
      if (last) setAnnouncement(last.content.slice(0, 800))
    }
    wasRunning.current = running
  }, [running, chatState?.messages])

  /* Following the newest content.
   *
   * Virtuoso's `followOutput` is not enough for a streamed reply, and the shortfall is
   * measurable: it reacts to *appended items* ("keep the list aligned when new items are
   * appended"), while a reply arrives as one item whose text grows. The scroller then
   * stays at the offset it had when that item was appended, which in the mock-bridge
   * harness left the last ~740px of the answer below the fold — and still there once the
   * turn had ended, so every reply had to be fetched with a scroll of its own.
   *
   * So the signal is the list's height, which changes for both events: an appended item
   * and a growing one. Acting on it is a decision about the user, and the test is the
   * direction of their scroll rather than the distance from the bottom. Growth pushes the
   * bottom away without moving the scroll position at all, so a position that has moved
   * *up* is a user reading history, and one that reaches the bottom again is a user who is
   * done with it. That is also why Virtuoso's own
   * `atBottomStateChange` is not the flag here: a single frame of growth between a follow
   * and that callback's next test reads as "not at the bottom", and the harness run above
   * showed one such frame stopping the follow for the rest of the turn.
   */
  const followNewest = useCallback(() => {
    if (!following.current) return
    const element = scroller.current
    if (!element) return
    // The live DOM height rather than an item index: the measured size of the item that
    // is being written trails the text by a frame, and a scroll that lands short leaves
    // the newest tokens out of view.
    element.scrollTop = element.scrollHeight
  }, [])

  const watchScroll = useCallback(() => {
    const element = scroller.current
    if (!element) return
    const bottom = element.scrollHeight - element.clientHeight
    const offset = element.scrollTop
    // The furthest down the view has been. Every follow writes the bottom, so this tracks
    // it without an opinion of its own; clamping it to the current bottom drops the whole
    // record when content is removed and the browser pulls the position up with it.
    if (offset > lowestOffset.current) lowestOffset.current = offset
    if (lowestOffset.current > bottom) lowestOffset.current = bottom
    // Upward movement is a user reading, with one exception measured in the harness: the
    // engine settled a follow of its own with a one-pixel move backwards, which under a
    // strict comparison switched following off for the rest of the turn. Eight is under
    // half a line of text, and a real gesture — a wheel notch, a trackpad slide, an arrow
    // key — is past it in one event or two.
    if (lowestOffset.current - offset > 8) {
      following.current = false
      return
    }
    // A downward move only ever turns the follow back on, and only at the bottom: one
    // that stops short is a user who is still reading, not one who wants to be moved.
    if (bottom - offset <= 4) following.current = true
  }, [])

  const trackScroller = useCallback(
    (element: HTMLElement | null | Window) => {
      const next = element instanceof HTMLElement ? element : null
      if (next === scroller.current) return
      scroller.current?.removeEventListener('scroll', watchScroll)
      scroller.current = next
      next?.addEventListener('scroll', watchScroll)
    },
    [watchScroll],
  )

  useEffect(
    () => () => {
      scroller.current?.removeEventListener('scroll', watchScroll)
    },
    [watchScroll],
  )

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

  // The only compaction that can be undone without contradicting a newer summary.
  const newestCompactionId = useMemo(() => {
    const list = chatState?.compactions ?? []
    let newest = ''
    let top = -1
    for (const c of list) {
      if (c.toSeq > top) {
        top = c.toSeq
        newest = c.id
      }
    }
    return newest
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
      const compaction = byBoundary.get(m.seq)
      if (!hidden.has(m.id)) {
        out.push({ kind: 'message', key: m.id, message: m })
      }
      // The card is emitted whether or not its boundary message is hidden. The
      // boundary is the last folded message, so it is always in `hidden` while
      // the originals are collapsed — skipping the card with it removed the
      // summary, the undo button and the only control that shows the originals
      // again, leaving the user no way back short of a reload.
      if (compaction) {
        out.push({ kind: 'compaction', key: `c-${compaction.id}`, compaction })
      }
    }
    return out
  }, [chatState, collapsed])

  const submit = async () => {
    const text = input.trim()
    if (!text) return
    // A slash command is answered by the window rather than sent to the model: it names
    // something the app does to the conversation, and there is no "clear this chat" a model
    // could be told to do. The match is exact, so a message that merely begins with a slash
    // — a path, a fragment of Markdown — still travels as a message. It is handled before
    // the running guard on purpose: a clear during a turn is refused by the backend with a
    // sentence, and swallowing the command instead would look like the app ignoring it.
    if (text.toLowerCase() === CLEAR_COMMAND) {
      if (!activeChatId) return
      setInput('')
      await useStore.getState().clearChat(activeChatId)
      return
    }
    if (running) return
    setInput('')
    // Sending is a deliberate move to the newest content, so the list follows again even
    // if the user had scrolled up to read: their own message and the reply to it belong
    // in view without a scroll first.
    following.current = true
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
  }    const permission = permissionBadge(t, autoApprove)

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
        {/*
          A project says where it works and, while the file tools can only read, that
          they can only read. Both belong in the header rather than in a settings
          screen: which folder an agent is looking at is the one fact that decides
          whether its answer is about your code or about code in general, and "it can
          read this" is the difference between a tool you hand your project to and one
          you would not.
        */}
        {chatState?.chat.workspace && (
          <div className="flex shrink-0 items-center gap-2 text-[11px]">
            <span className="badge" title={chatState.chat.workspace}>
              📁 {folderName(chatState.chat.workspace)}
            </span>
            {/* What the folder's permission is, in one word. It used to read "Read-only"
                because there was no write tool, and a badge that still said that now would be
                the one place in the window that lies about what the model can do. The same
                argument applies to the other direction: with the confirmation switched off in
                the settings, "with confirmation" would be that lie. */}
            <span className="faint" title={permission.hint}>
              {permission.badge}
            </span>
          </div>
        )}
        <SearchFirstToggle />
      </div>

      {/* Messages */}
      <div
        className="min-h-0 flex-1"
        role="log"
        aria-label={t('chat.transcript')}
        // A conversation, not a live region: the finished reply is announced once
        // by the status region below, rather than a token at a time.
        aria-live="off"
      >
        {items.length === 0 ? (
          <div className="flex h-full items-center justify-center p-8">
            <div className="max-w-md text-center">
              <div className="mb-2 text-lg font-medium">
                {chatState?.chat.workspace ? folderName(chatState.chat.workspace) : t('chat.empty.title')}
              </div>
              <p className="muted text-[13px]">
                {chatState?.chat.workspace ? t('project.emptyHint') : t('chat.empty.hint')}
              </p>
            </div>
          </div>
        ) : (
          <Virtuoso
            data={items}
            scrollerRef={trackScroller}
            // A new item and a growing item are both a height change, and that is the one
            // signal that covers an appended message and a streamed reply alike.
            totalListHeightChanged={followNewest}
            // Opening a chat lands on the newest message, not on the start of the last
            // item: a bare index aligns that item's top with the top of the viewport,
            // which for a long reply is the middle of it.
            initialTopMostItemIndex={{ index: 'LAST', align: 'end' }}
            increaseViewportBy={{ top: 400, bottom: 600 }}
            computeItemKey={(_i, item) => item.key}
            itemContent={(_index, item) =>
              item.kind === 'message' ? (
                <div className="px-4 pb-1">
                  <MessageRow message={item.message} folded={item.message.seq <= foldedUpTo} />
                </div>
              ) : (
                <div className="px-4">
                  <CompactionCard
                    compaction={item.compaction}
                    undoable={item.compaction.id === newestCompactionId}
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
        {/* Before everything else, because the turn is stopped behind it: this chat cannot
            make progress until it is answered, so it is the only thing here that is about
            right now rather than about the next request. */}
        <Confirmation />
        {/* A question the app answered itself, by refusing: the line is the user's to run, so
            it sits where they type rather than only on the card it was recorded on. */}
        <Handoff />
        {/* Next, because it is about the next request: a chat whose model has gone
            keeps working only after this switch, and everything below is the row that
            would otherwise fail. */}
        <MissingModelNotice />
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
                  aria-label={`${t('files.remove')} ${a.filename}`}
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
          // Polite, because a wait is not an error: the reader is told that something
          // is happening, and the reply itself arrives through the region below.
          <div className="mb-1 text-[11px]" role="status" style={{ color: 'var(--accent)' }}>
            {phase === 'compacting'
              ? t('compact.running')
              : phase === 'tool'
                ? t('chat.toolCalls')
                : phase === 'stopping'
                  ? t('chat.stopping')
                  : phase === 'throttled'
                    ? t('chat.throttled', {
                        // Seconds, and never zero: a wait that rounds down to "0 s"
                        // would read as no wait at all, which is the impression this
                        // line exists to remove.
                        seconds: formatNumber(Math.max(1, Math.round(phaseWaitMs / 1000))),
                      })
                    : phase === 'paced'
                      ? t('chat.paced', {
                          seconds: formatNumber(Math.max(1, Math.round(phaseWaitMs / 1000))),
                        })
                      : phase === 'spread'
                        ? t('chat.spread', {
                            seconds: formatNumber(Math.max(1, Math.round(phaseWaitMs / 1000))),
                          })
                        : phase}
          </div>
        )}

        <div className="flex items-end gap-2">
          <textarea
            ref={textareaRef}
            className="textarea min-h-[42px] max-h-40 flex-1"
            rows={1}
            placeholder={t('chat.placeholder')}
            // A placeholder is a hint, not a name: it disappears the moment the
            // field has text, and a screen reader would then have nothing to call it.
            aria-label={t('chat.composerLabel')}
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
          <span className="faint ml-auto text-[11px]">
            {t('chat.clearHint')} · {t('files.dropHint')}
          </span>
        </div>
      </div>

      {/* The one announcement a turn makes. Visually hidden, and empty until a reply
          settles, so it never reads a partial answer. */}
      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {announcement}
      </div>
    </div>
  )
}

/* -------------------------------------------------------------------------
 * One message
 * ---------------------------------------------------------------------- */

function MessageRow({
  message,
  folded,
}: {
  message: storeTypes.Message
  folded: boolean
}) {
  const t = useStore((s) => s.t)
  // Only the message being written subscribes to its own stream buffer. Reading
  // the whole buffer map here instead meant every delta re-rendered the chat
  // view, the list and every row in it — the reason the rest of the window
  // twitched along with the text.
  const live = useStore(useShallow((s) => messageText(message, s.streaming)))
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
        {/* The model that answered, by name: a managed model's id is a file path, and
            a transcript is not the place to read one. The detail row below the
            message keeps the full id for whoever needs it. */}
        {!isUser && message.model && (
          <span className="faint mono" title={message.model}>
            {modelLabel(message.model)}
          </span>
        )}
        {folded && <span className="badge" title={t('compact.title')}>🗜</span>}
        {/* A reply the backend stopped delivering is not a finished answer, and it
            has to say so next to the text rather than only in a toast that is gone
            by the time the conversation is read again. */}
        {message.status === 'failed' && (
          <span className="badge badge-danger" title={t('chat.failedHint')}>
            {t('chat.failed')}
          </span>
        )}
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
        <MessageImages attachments={message.attachments ?? []} />
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
          <Markdown text={live.text} streaming={live.streaming} sources={message.sources} />
        ) : live.streaming ? (
          <span className="streaming-cursor" />
        ) : (
          <span className="faint">—</span>
        )}
        <SourcesList sources={message.sources} />
      </div>

      <MessageStats message={message} />
    </div>
  )
}

/** The sites an answer's citations number, in the order the model read them.
 *
 * The numbering is the turn's, not this component's: the backend continues the sequence
 * across the searches of one turn, so `[7]` in the text and the seventh row here are the
 * same page. That is why the list is stored with the answer rather than rebuilt here —
 * a client that numbered its own rows would be a second opinion about what `[7]` means.
 *
 * A short list is open, because it is the thing the reader came for. A long one starts
 * collapsed: a twenty-source answer should not push its own text off the screen, and the
 * markers in the text are links anyway.
 */
function SourcesList({ sources }: { sources?: storeTypes.Source[] }) {
  const t = useStore((s) => s.t)
  if (!sources || sources.length === 0) return null

  return (
    <details className="collapsible mt-2 text-[12px]" open={sources.length <= 4}>
      <summary className="faint">
        {t('chat.sources')} ({formatNumber(sources.length)})
      </summary>
      <ol className="mt-1 space-y-0.5">
        {sources.map((source) => (
          <li key={`${source.index}-${source.url}`} className="flex gap-1.5">
            <span className="faint mono shrink-0">[{source.index}]</span>
            <button
              type="button"
              className="min-w-0 truncate text-left underline decoration-dotted"
              title={source.url}
              onClick={() => source.url && void App.OpenExternal(source.url)}
            >
              {source.title || source.url}
            </button>
            <span className="faint shrink-0">{domainOf(source.url)}</span>
          </li>
        ))}
      </ol>
    </details>
  )
}

/** The host a source lives on, without `www.` — the part that tells two similar titles
 * apart when the list is scanned rather than read. */
function domainOf(url?: string): string {
  if (!url) return ''
  try {
    return new URL(url).hostname.replace(/^www\./, '')
  } catch {
    return ''
  }
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

/** What a project chat's header says about what the model may do to its folder.
 *
 * Three labels rather than one, because the two permissions are independent and rounding
 * them up would be wrong in the case people care about: somebody who trusts the model with
 * their files has not thereby said anything about their machine.
 *
 * Exported because the sidebar's project row says the same thing, and the two have to say it
 * in the same words: a row that called it *asks* while the header said *no question* would be
 * the disagreement this whole gate exists to avoid, one click apart.
 */
export function permissionBadge(
  t: (key: MessageKey) => string,
  auto?: config.AutoApproveSettings,
  // The sidebar row's wording: the same three states, in the words that fit beside a folder
  // path. Which state is claimed is decided here, once — a row and a header that disagreed
  // about *whether* the model asks would be the one lie this badge exists to prevent.
  short = false,
): { badge: string; hint: string } {
  const asked = {
    badge: t(short ? 'project.shortConfirmed' : 'project.confirmedBadge'),
    hint: t('project.confirmed'),
  }
  if (!auto?.files && !auto?.commands) return asked
  const hint = t('project.autoHint')
  if (auto.files && auto.commands) {
    return { badge: t(short ? 'project.shortBoth' : 'project.autoBothBadge'), hint }
  }
  return auto.files
    ? { badge: t(short ? 'project.shortFiles' : 'project.autoFilesBadge'), hint }
    : { badge: t(short ? 'project.shortCommands' : 'project.autoCommandsBadge'), hint }
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
      aria-pressed={on}
      // The label says what is switched, the tooltip says what switching it
      // actually does to the turn — a repeated label taught nothing.
      title={t('search.searchFirstHint')}
      onClick={() => void updateChat({ searchFirst: !on })}
    >
      🔎 {t('search.searchFirst')}
    </button>
  )
}
