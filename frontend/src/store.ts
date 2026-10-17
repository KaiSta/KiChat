import { create } from 'zustand'
import {
  App,
  EV,
  on,
  type chat,
  type config,
  type ImageProgressPayload,
  type llm,
  type main,
  type research,
  type store as storeTypes,
} from './lib/api'
import { WindowSetDarkTheme, WindowSetLightTheme } from '../wailsjs/runtime/runtime'
import { asLang, translate, type Lang } from './lib/i18n'

/**
 * Applies the theme to the document and asks the native window to match.
 *
 * The document attribute is what actually themes this app: the `color-scheme` it
 * selects in style.css is what makes the engine draw its own widgets — a <select>
 * popup, scrollbars — in the app's palette instead of the desktop's.
 *
 * The Wails calls additionally put the native window frame and menus into the
 * same theme on the platforms that implement them. Linux does not: in Wails
 * v2.15 all three are empty stubs, so on this build they cost a call and change
 * nothing. They are kept because the app targets more than Linux; do not mistake
 * them for the reason the dropdowns are readable.
 */
function applyTheme(theme: 'dark' | 'light') {
  document.documentElement.setAttribute('data-theme', theme)
  if (theme === 'dark') {
    WindowSetDarkTheme()
  } else {
    WindowSetLightTheme()
  }
}

/**
 * Builds the translator for one language.
 *
 * A new function per language is the point, not an accident: components read the
 * translator with `useStore((s) => s.t)`, and a selector that keeps returning
 * the same reference never re-renders. A translator that read the language at
 * call time (`translate(get().lang, ...)`) therefore re-rendered only the one
 * component that also selected `lang` — the sidebar — and left the chat area in
 * the previous language until some unrelated state change happened to repaint
 * it.
 */
function makeT(lang: Lang): (key: string, vars?: Record<string, string | number>) => string {
  return (key, vars) => translate(lang, key, vars)
}

/** Live text for a message that is still streaming. */
interface StreamBuffer {
  text: string
  reasoning: string
}

/**
 * One picture being drawn, as the window knows it.
 *
 * `drawId` is the attachment of a redraw or the assistant message a model's draw belongs to
 * — the row each is rendered on — and the two are one map because nothing that reads this
 * needs to tell them apart. `preview` is a ready `data:` URL: the backend sends the picture
 * with the step counter rather than behind a second call, so a row can paint the draw as it
 * goes without a request per step.
 */
export interface DrawProgress {
  chatId: string
  step: number
  steps: number
  /** Which sampling pass this step belongs to, and how many the draw runs in total. */
  pass: number
  passes: number
  preview?: string
}

export interface Toast {
  id: number
  kind: 'error' | 'warning' | 'info'
  message: string
}

interface State {
  ready: boolean
  bootError: string | null

  lang: Lang
  theme: 'dark' | 'light'
  view: 'chat' | 'settings'

  settings: config.Settings | null
  models: llm.ModelInfo[]
  health: llm.HealthStatus[]
  modelsLoading: boolean

  chats: storeTypes.Chat[]
  activeChatId: string | null
  chatState: main.ChatState | null
  chatLoading: boolean

  /** messageId -> live buffer, only while that message is streaming. */
  streaming: Record<string, StreamBuffer>
  /**
   * drawId -> what is known about a picture being drawn, present only while it is being
   * drawn: the end of a draw removes the entry, because the picture itself takes over the
   * moment the draw answers. A row that reads this is reading "this is happening now".
   */
  draws: Record<string, DrawProgress>
  running: Record<string, boolean>
  /** chatId -> phase such as "tool" or "compacting". */
  phase: Record<string, string>
  /**
   * chatId -> how long the current phase is waiting, in ms, for the phases that carry
   * a duration. Zero for every other phase, including the ones that are merely set.
   */
  phaseWaitMs: Record<string, number>
  toasts: Toast[]

  /**
   * Model to use for the *next* message only, leaving the chat's default
   * untouched. Cleared as soon as it is used.
   */
  nextModel: { backend: string; model: string } | null
  setNextModel: (value: { backend: string; model: string } | null) => void

  /* actions */
  t: (key: string, vars?: Record<string, string | number>) => string
  toast: (kind: Toast['kind'], message: string) => void
  dismissToast: (id: number) => void

  bootstrap: () => Promise<void>
  setLang: (lang: Lang) => void
  setTheme: (theme: 'dark' | 'light') => void
  setView: (view: 'chat' | 'settings') => void

  /**
   * Re-read the settings blob. An install writes the recorded build — its tag, the
   * source and the family — from the backend's own goroutine, after the bound call
   * that started it has already returned. A snapshot read before that would be saved
   * back over the record on the next full save, so the view has to ask again when it
   * notices the two disagree.
   */
  refreshSettings: () => Promise<void>

  refreshModels: (force?: boolean) => Promise<void>
  refreshHealth: () => Promise<void>
  loadChats: () => Promise<void>
  selectChat: (id: string) => Promise<void>
  refreshChatState: () => Promise<void>
  newChat: (backend?: string, model?: string) => Promise<string | null>
  deleteChat: (id: string) => Promise<void>
  renameChat: (id: string, title: string) => Promise<void>

  send: (text: string) => Promise<void>
  stop: () => Promise<void>
  regenerate: (backend?: string, model?: string) => Promise<void>

  updateChat: (patch: Partial<storeTypes.Chat>) => Promise<void>
}

let toastSeq = 0

export const useStore = create<State>((set, get) => {
  /** Insert or replace a message, keeping the list ordered by seq. */
  const upsertMessage = (message: storeTypes.Message) => {
    const state = get()
    if (!state.chatState || state.chatState.chat.id !== message.chatId) return
    const messages = state.chatState.messages.filter((m) => m.id !== message.id)
    messages.push(message)
    messages.sort((a, b) => a.seq - b.seq)
    set({ chatState: { ...state.chatState, messages } })
  }

  /** Drop the streaming buffers for a chat once its turn is finished. */
  const clearStreaming = (chatId: string) => {
    const state = get()
    if (!state.chatState || state.chatState.chat.id !== chatId) {
      set({ streaming: {} })
      return
    }
    // Buffers are keyed by message id, so only drop the ones in this chat.
    const ids = new Set(state.chatState.messages.map((m) => m.id))
    const next: Record<string, StreamBuffer> = {}
    for (const [id, buf] of Object.entries(state.streaming)) {
      if (!ids.has(id)) next[id] = buf
    }
    set({ streaming: next })
  }

  return {
    ready: false,
    bootError: null,

    lang: 'en',
    theme: 'dark',
    view: 'chat',

    settings: null,
    models: [],
    health: [],
    modelsLoading: false,

    chats: [],
    activeChatId: null,
    chatState: null,
    chatLoading: false,

    streaming: {},
    draws: {},
    running: {},
    phase: {},
    phaseWaitMs: {},
    toasts: [],
    nextModel: null,

    setNextModel: (value) => set({ nextModel: value }),

    t: makeT('en'),

    toast: (kind, message) => {
      const id = ++toastSeq
      set((s) => ({ toasts: [...s.toasts.slice(-4), { id, kind, message }] }))
      if (kind !== 'error') {
        setTimeout(() => get().dismissToast(id), 6000)
      }
    },
    dismissToast: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),

    bootstrap: async () => {
      try {
        const settings = await App.GetSettings()
        // Settings may carry a hand-edited or older language value.
        const lang: Lang = asLang(settings.language)
        const theme = settings.theme === 'light' ? 'light' : 'dark'
        applyTheme(theme)
        // The translator is replaced alongside the language it speaks, so every
        // subscriber repaints.
        set({ settings, lang, theme, ready: true, t: makeT(lang) })

        wireEvents(set, get, clearStreaming, upsertMessage)

        const [health, chats, models] = await Promise.all([
          App.GetHealth().catch(() => [] as llm.HealthStatus[]),
          App.ListChats(false).catch(() => [] as storeTypes.Chat[]),
          App.ListModels(false).catch(() => [] as llm.ModelInfo[]),
        ])
        set({ health, chats, models })

        if (chats.length > 0) {
          await get().selectChat(chats[0].id)
        }
      } catch (err) {
        set({ bootError: String(err), ready: true })
      }
    },

    setLang: (lang) => {
      set({ lang, t: makeT(lang) })
      const settings = get().settings
      if (settings) {
        const next = { ...settings, language: lang }
        set({ settings: next })
        void App.SaveSettings(next)
          .then((stored) => {
            // The built-in system prompts follow the interface language, so the
            // stored copy replaces ours. Guarded because two quick switches can
            // resolve out of order and the older answer must not win.
            if (get().lang === lang) set({ settings: stored })
          })
          .catch(() => undefined)
      }
    },

    setTheme: (theme) => {
      applyTheme(theme)
      set({ theme })
      const settings = get().settings
      if (settings) {
        const next = { ...settings, theme }
        set({ settings: next })
        void App.SaveSettings(next).catch(() => undefined)
      }
    },

    // The install is the one writer of the recorded build, and it runs behind a
    // bound method that has already returned by the time the download finishes.
    // Re-reading is how the rest of the view learns about it; the Settings draft
    // follows the store, so the pickers end up naming the build that is really there.
    refreshSettings: async () => {
      try {
        set({ settings: await App.GetSettings() })
      } catch {
        // A re-read is a courtesy; a failure must not take the view down.
      }
    },

    setView: (view) => set({ view }),

    refreshModels: async (force = false) => {
      set({ modelsLoading: true })
      try {
        const models = await App.ListModels(force)
        set({ models })
      } catch (err) {
        get().toast('error', String(err))
      } finally {
        set({ modelsLoading: false })
      }
    },

    refreshHealth: async () => {
      try {
        set({ health: await App.GetHealth() })
      } catch {
        /* a probe failure is already visible as "unreachable" */
      }
    },

    loadChats: async () => {
      try {
        set({ chats: await App.ListChats(false) })
      } catch (err) {
        get().toast('error', String(err))
      }
    },

    selectChat: async (id) => {
      // Streaming buffers are keyed by message id, so they deliberately survive a
      // switch: a chat that is still generating keeps whatever text has already
      // arrived when the user comes back to it. The done/error handlers are what
      // clear them. Blanking the map here made the streamed prefix vanish and
      // left only the newest delta rendering.
      set({ activeChatId: id, chatLoading: true })
      try {
        const state = await App.GetChatState(id)
        // Ignore a response for a chat the user has already navigated away from.
        if (get().activeChatId !== id) return
        set({ chatState: state })
        const running = { ...get().running, [id]: state.running }
        set({ running })
      } catch (err) {
        get().toast('error', String(err))
      } finally {
        set({ chatLoading: false })
      }
    },

    refreshChatState: async () => {
      const id = get().activeChatId
      if (!id) return
      try {
        const state = await App.GetChatState(id)
        if (get().activeChatId !== id) return
        set({ chatState: state })
      } catch {
        /* transient read failure; the next event will refresh */
      }
    },

    newChat: async (backend = '', model = '') => {
      try {
        const created = await App.CreateChat(backend, model)
        await get().loadChats()
        await get().selectChat(created.id)
        set({ view: 'chat' })
        return created.id
      } catch (err) {
        get().toast('error', String(err))
        return null
      }
    },

    deleteChat: async (id) => {
      try {
        await App.DeleteChat(id)
        const chats = get().chats.filter((c) => c.id !== id)
        set({ chats })
        if (get().activeChatId === id) {
          set({ activeChatId: null, chatState: null })
          if (chats.length > 0) await get().selectChat(chats[0].id)
        }
      } catch (err) {
        get().toast('error', String(err))
      }
    },

    renameChat: async (id, title) => {
      try {
        await App.RenameChat(id, title)
        set({
          chats: get().chats.map((c) => (c.id === id ? { ...c, title } : c)),
          chatState:
            get().chatState?.chat.id === id
              ? { ...get().chatState!, chat: { ...get().chatState!.chat, title } }
              : get().chatState,
        })
      } catch (err) {
        get().toast('error', String(err))
      }
    },

    send: async (text) => {
      const state = get()
      const chatId = state.activeChatId
      if (!chatId || !state.chatState) return
      if (state.running[chatId]) return

      // Attachments that have not been assigned to a message yet travel with
      // this turn.
      const attachmentIds = state.chatState.attachments
        .filter((a) => !a.messageId)
        .map((a) => a.id)

      const override = state.nextModel
      set({ running: { ...state.running, [chatId]: true }, nextModel: null })
      try {
        await App.SendMessage({
          chatId,
          text,
          attachmentIds,
          backend: override?.backend ?? '',
          model: override?.model ?? '',
          regenerate: false,
        })
      } catch (err) {
        set({ running: { ...get().running, [chatId]: false } })
        get().toast('error', String(err))
      }
    },

    stop: async () => {
      const chatId = get().activeChatId
      if (!chatId) return
      try {
        await App.StopGeneration(chatId)
      } catch (err) {
        get().toast('error', String(err))
      }
    },

    regenerate: async (backend = '', model = '') => {
      const chatId = get().activeChatId
      if (!chatId || get().running[chatId]) return
      set({ running: { ...get().running, [chatId]: true } })
      try {
        await App.Regenerate(chatId, backend, model)
      } catch (err) {
        set({ running: { ...get().running, [chatId]: false } })
        get().toast('error', String(err))
      }
    },

    updateChat: async (patch) => {
      const state = get()
      if (!state.chatState) return
      const merged = { ...state.chatState.chat, ...patch }
      try {
        const saved = await App.UpdateChat(merged)
        if (get().activeChatId !== saved.id) return
        const current = get().chatState
        if (!current) return
        set({ chatState: { ...current, chat: saved } })
        // A model change must refresh the budget and the switch warning.
        void get().refreshChatState()
      } catch (err) {
        get().toast('error', String(err))
      }
    },
  }
})

/**
 * Subscribe to backend events once. Wails keeps listeners for the lifetime of
 * the webview, so this is wired a single time during bootstrap.
 */
function wireEvents(
  set: (partial: Partial<State> | ((s: State) => Partial<State>)) => void,
  get: () => State,
  clearStreaming: (chatId: string) => void,
  upsertMessage: (message: storeTypes.Message) => void,
) {
  const active = () => get().activeChatId

  on<{ chatId: string; messageId: string; text: string }>(EV.start, (p) => {
    if (p.chatId !== active()) return
    set((s) => ({
      running: { ...s.running, [p.chatId]: true },
      // Reset any stale buffer: a fresh message id gets a fresh buffer.
      streaming: { ...s.streaming, [p.messageId]: { text: '', reasoning: '' } },
    }))
    void get().refreshChatState()
  })

  on<{ chatId: string; messageId: string; text: string }>(EV.delta, (p) => {
    if (p.chatId !== active()) return
    set((s) => {
      const buf = s.streaming[p.messageId] ?? { text: '', reasoning: '' }
      return { streaming: { ...s.streaming, [p.messageId]: { ...buf, text: buf.text + p.text } } }
    })
  })

  on<{ chatId: string; messageId: string; text: string }>(EV.reasoning, (p) => {
    if (p.chatId !== active()) return
    set((s) => {
      const buf = s.streaming[p.messageId] ?? { text: '', reasoning: '' }
      return {
        streaming: { ...s.streaming, [p.messageId]: { ...buf, reasoning: buf.reasoning + p.text } },
      }
    })
  })

  on<{ chatId: string; message: storeTypes.Message }>(EV.message, (p) => {
    if (p.chatId !== active()) return
    upsertMessage(p.message)
  })

  on<{ chatId: string; toolCall: storeTypes.ToolCallRecord }>(EV.tool, (p) => {
    if (p.chatId !== active()) return
    const state = get()
    if (!state.chatState) return
    const messages = state.chatState.messages.map((m) =>
      m.id === p.toolCall.messageId
        ? {
            ...m,
            toolCalls: [...(m.toolCalls ?? []).filter((t) => t.id !== p.toolCall.id), p.toolCall],
          }
        : m,
    )
    set({ chatState: { ...state.chatState, messages } })
  })

  on<{ chatId: string; phase: string; waitMs?: number }>(EV.status, (p) => {
    if (p.chatId !== active()) return
    set((s) => ({
      phase: { ...s.phase, [p.chatId]: p.phase },
      // The wait belongs to the phase it arrived with: a later phase that names
      // none must not keep showing a stale number beside different words.
      phaseWaitMs: { ...s.phaseWaitMs, [p.chatId]: p.waitMs ?? 0 },
    }))
  })

  on<{ chatId: string; budget: chat.ContextBudget }>(EV.budget, (p) => {
    if (p.chatId !== active()) return
    const state = get()
    if (!state.chatState || state.chatState.chat.id !== p.chatId) return
    set({ chatState: { ...state.chatState, budget: p.budget } })
  })

  on<{ chatId: string; status: string }>(EV.done, (p) => {
    // Always clear the running flag, even for a chat that is no longer open,
    // otherwise the Stop button would stay stuck.
    set((s) => ({ running: { ...s.running, [p.chatId]: false } }))
    if (p.chatId !== active()) return
    // Read the finished messages before dropping the stream buffers. A buffer is
    // keyed by message id and only cleared for ids the chat already holds, so
    // clearing first left the streamed text rendering on top of the stored reply
    // whenever the refresh carrying the finished row had not landed yet.
    void (async () => {
      await get().refreshChatState()
      clearStreaming(p.chatId)
      await get().loadChats()
    })()
  })

  on<{ chatId: string; messageId?: string; error: string }>(EV.error, (p) => {
    set((s) => ({
      running: { ...s.running, [p.chatId]: false },
    }))
    if (p.chatId === active()) {
      // Same order as the done handler: the stored row is authoritative, so it is
      // read before the buffer that renders on top of it is dropped.
      void (async () => {
        await get().refreshChatState()
        clearStreaming(p.chatId)
      })()
    }
    get().toast('error', p.error)
  })

  on<{ chatId: string; warning: string }>(EV.warning, (p) => {
    if (p.chatId !== active()) return
    get().toast('warning', p.warning)
  })

  on<{ chatId: string; compaction: storeTypes.Compaction }>(EV.compaction, (p) => {
    if (p.chatId !== active()) return
    void get().refreshChatState()
  })

  on<{ chatId: string; title: string }>(EV.title, (p) => {
    set((s) => ({
      chats: s.chats.map((c) => (c.id === p.chatId ? { ...c, title: p.title } : c)),
      chatState:
        s.chatState?.chat.id === p.chatId
          ? { ...s.chatState, chat: { ...s.chatState.chat, title: p.title } }
          : s.chatState,
    }))
  })

  on(EV.chatsChanged, () => {
    void get().loadChats()
  })

  on<{ chatId: string }>(EV.attachments, (p) => {
    if (p.chatId !== active()) return
    void get().refreshChatState()
  })

  /*
   * A picture being drawn reports here, and the row it belongs to reads it by draw id —
   * the attachment for a redraw, the assistant message for a draw the model asked for.
   *
   * `done` removes the entry rather than being ignored, and that is what keeps a progress
   * row from outliving its draw: the finished picture arrives as an attachment of that same
   * message, so the moment the draw ends the preview is the wrong thing to show. Nothing is
   * emitted for a draw the window was never told about, so an entry always has a report
   * behind it.
   */
  on<ImageProgressPayload>(EV.imageProgress, (p) => {
    // A `done` is applied whatever chat is open, which is why it is handled before the guard
    // below. A draw keeps running while the user reads another chat, and its end used to be
    // dropped with the rest of that chat's traffic: the entry then stayed in the map, so
    // opening that chat again rendered a progress row — and a Stop button — for a draw that
    // had already finished. The preview is gone either way; only the mark is stale.
    if (p.done) {
      set((s) => {
        const draws = { ...s.draws }
        delete draws[p.drawId]
        return { draws }
      })
      return
    }
    // A report for a chat that is not open is dropped, the way every chat-scoped event is:
    // the row it belongs to is not mounted, and keeping the entry would only leak it. The
    // next report for the open chat is a few hundred milliseconds away.
    if (p.chatId && p.chatId !== active()) return
    set((s) => {
      const draws = { ...s.draws }
      draws[p.drawId] = {
        chatId: p.chatId,
        step: p.step,
        steps: p.steps,
        // A backend that does not run a second pass reports neither field; the payload
        // already normalises that to one pass, and this keeps an older one-side build in
        // the same shape instead of rendering "pass undefined of undefined".
        pass: p.pass || 1,
        passes: p.passes || 1,
        // The preview is kept from the previous report when this one has none: a server
        // that sends its fraction and its picture separately, or a poll that landed
        // between two of them, must not make the thumbnail blink away.
        preview: p.preview ?? s.draws[p.drawId]?.preview,
      }
      return { draws }
    })
  })
}

/** Convenience selector for the active chat's pending (unsent) attachments. */
export function pendingAttachments(state: State): storeTypes.Attachment[] {
  return (state.chatState?.attachments ?? []).filter((a) => !a.messageId)
}

/** The text a message should render: the live buffer while streaming, else the
 * persisted content. */
export function messageText(
  message: storeTypes.Message,
  streaming: Record<string, StreamBuffer>,
): { text: string; reasoning: string; streaming: boolean } {
  const buf = streaming[message.id]
  if (buf) {
    return { text: buf.text, reasoning: buf.reasoning, streaming: true }
  }
  return { text: message.content, reasoning: message.reasoning, streaming: false }
}

export type { research }
