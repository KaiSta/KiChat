import { create } from 'zustand'
import {
  App,
  EV,
  on,
  type chat,
  type config,
  type llm,
  type main,
  type research,
  type store as storeTypes,
} from './lib/api'
import { translate, type Lang } from './lib/i18n'

/** Live text for a message that is still streaming. */
interface StreamBuffer {
  text: string
  reasoning: string
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
  running: Record<string, boolean>
  /** chatId -> phase such as "tool" or "compacting". */
  phase: Record<string, string>
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

    lang: 'de',
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
    running: {},
    phase: {},
    toasts: [],
    nextModel: null,

    setNextModel: (value) => set({ nextModel: value }),

    t: (key, vars) => translate(get().lang, key, vars),

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
        const lang: Lang = settings.language === 'en' ? 'en' : 'de'
        const theme = settings.theme === 'light' ? 'light' : 'dark'
        document.documentElement.setAttribute('data-theme', theme)
        set({ settings, lang, theme, ready: true })

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
      set({ lang })
      const settings = get().settings
      if (settings) {
        const next = { ...settings, language: lang }
        set({ settings: next })
        void App.SaveSettings(next).catch(() => undefined)
      }
    },

    setTheme: (theme) => {
      document.documentElement.setAttribute('data-theme', theme)
      set({ theme })
      const settings = get().settings
      if (settings) {
        const next = { ...settings, theme }
        set({ settings: next })
        void App.SaveSettings(next).catch(() => undefined)
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
      set({ activeChatId: id, chatLoading: true, streaming: {} })
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

  on<{ chatId: string; phase: string }>(EV.status, (p) => {
    if (p.chatId !== active()) return
    set((s) => ({ phase: { ...s.phase, [p.chatId]: p.phase } }))
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
    clearStreaming(p.chatId)
    void get().refreshChatState()
    void get().loadChats()
  })

  on<{ chatId: string; messageId?: string; error: string }>(EV.error, (p) => {
    set((s) => ({
      running: { ...s.running, [p.chatId]: false },
    }))
    if (p.chatId === active()) {
      clearStreaming(p.chatId)
      void get().refreshChatState()
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
