import { create } from 'zustand'
import {
  App,
  EV,
  on,
  type ApprovalDecision,
  type ApprovalPayload,
  type chat,
  type config,
  type ImageProgressPayload,
  type llm,
  type main,
  type research,
  type store as storeTypes,
  type ToolOutputPayload,
} from './lib/api'
import { WindowSetDarkTheme, WindowSetLightTheme } from '../wailsjs/runtime/runtime'
import {
  asLang,
  errorCode,
  translate,
  translateDynamic,
  translateError,
  type Lang,
  type MessageKey,
} from './lib/i18n'

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
function makeT(lang: Lang): (key: MessageKey, vars?: Record<string, string | number>) => string {
  return (key, vars) => translate(lang, key, vars)
}

/** The unchecked half of the pair above: see `translateDynamic` for which keys are
 * assembled at runtime rather than written out. */
function makeTDynamic(lang: Lang): (key: string, vars?: Record<string, string | number>) => string {
  return (key, vars) => translateDynamic(lang, key, vars)
}

/**
 * A command the user has to run themselves, with the line to do it.
 *
 * `toolCallId` names the card the refusal sits on, so the hand-off and the transcript point at
 * the same call rather than at two descriptions of it.
 */
export interface Handoff {
  command: string
  toolCallId: string
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
  /** A non-sampling part of the draw in progress (`captioning`), absent for a step report. */
  phase?: string
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
  /**
   * Whether the database can be read, and why not. Null until the first answer
   * arrives; the app shows the unlock screen while it is locked.
   */
  dbStatus: main.DatabaseStatus | null

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
  /**
   * toolCallId -> the output a command has printed so far, present only while that call is
   * running. The record the call ends with carries the whole output, so the entry is dropped
   * then: shown beside it, the same text would appear twice, and the live view would keep a
   * Stop-era snapshot of a command that is over.
   */
  toolOutput: Record<string, string>
  /**
   * chatId -> a command the app refused to run because it needs administrator rights, with
   * the exact line. Present from the moment the refusal arrives until the user dismisses it:
   * the app cannot run it, so the next move is theirs, and the line has to be where the next
   * thing they type is. Derived from the event rather than from the chat's history, so a chat
   * opened tomorrow does not offer a hand-off for a refusal it answered yesterday.
   */
  handoffs: Record<string, Handoff>
  /**
   * chatId -> the change to that chat's project folder waiting for an answer, present
   * only while the question is on screen. The turn that asked is blocked on it, so an
   * entry here means that chat is not progressing until it is answered.
   *
   * Keyed by chat because two chats can run turns at once, each stuck on its own
   * question; the entry is what the card renders from, and `approvalId` is what the
   * answer names.
   */
  approvals: Record<string, ApprovalPayload>
  /**
   * chatId -> what that chat has changed in its folder, most recent first, with the diff and
   * whether undo would be safe. Loaded when a project chat is opened and refreshed when a
   * change lands, so the header's count and the panel agree with the folder.
   */
  fileChanges: Record<string, main.FileChange[]>
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

  /** Re-read a chat's changed files after a change, a revert, or a switch to it. */
  loadFileChanges: (chatId: string) => Promise<void>
  /**
   * Put one file back the way the chat found it, then reload the list.
   *
   * The backend refuses in the two cases a revert would be wrong — the file was edited
   * afterwards, or a turn is still running — and its sentence is what the toast shows. A
   * successful revert emits nothing, so the list is re-read here rather than waited for.
   */
  revertFile: (chatId: string, path: string) => Promise<void>

  /* actions */
  t: (key: MessageKey, vars?: Record<string, string | number>) => string
  /** For keys built at runtime: see `translateDynamic`. */
  tDynamic: (key: string, vars?: Record<string, string | number>) => string
  toast: (kind: Toast['kind'], message: string) => void
  dismissToast: (id: number) => void
  /**
   * Answer a change waiting for confirmation. `once` allows this one change, `always`
   * allows every later one in the same chat for as long as it is open, anything else
   * declines.
   */
  /**
   * Answer the waiting question.
   *
   * `editedLine` is the command as it stood in the card's field when the answer was given,
   * and only a command answer carries one. It travels as an argument rather than as a second
   * binding for the callers that have no line to give: a change has none, and an unchanged
   * command is the plain answer the backend already reads.
   */
  resolveApproval: (
    chatId: string,
    approvalId: string,
    decision: ApprovalDecision,
    editedLine?: string,
  ) => Promise<void>
  /** Put a hand-off away: the line is still in the transcript's card, so nothing is lost. */
  dismissHandoff: (chatId: string) => void
  /**
   * Answer the waiting question *and* turn its kind of question off for good: the shortcut a
   * card offers so the standing answer does not have to be found in Settings first.
   *
   * `kind` names which switch, not which tool: the two are separate permissions and granting
   * one must not grant the other. The save happens before the answer, so a failed save leaves
   * the card on screen rather than reporting a permission that was never stored.
   */
  stopAsking: (
    chatId: string,
    approvalId: string,
    kind: 'change' | 'command',
  ) => Promise<void>

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
  /**
   * A new coding project on a folder the user picked. The folder is chosen by the
   * caller (App.PickProjectFolder), so a cancelled dialog never reaches the backend and
   * an unusable one is answered by the backend's own refusal, translated.
   */
  newProject: (workspace: string) => Promise<string | null>
  deleteChat: (id: string) => Promise<void>
  renameChat: (id: string, title: string) => Promise<void>
  /**
   * Empty a conversation and keep the chat. It is the only way to start over in a project,
   * because a project *is* its conversation: a second project on the same folder would be a
   * second copy of the settings that make it that project.
   */
  clearChat: (id: string) => Promise<void>

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
    dbStatus: null,

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
    toolOutput: {},
    handoffs: {},
    approvals: {},
    fileChanges: {},
    running: {},
    phase: {},
    phaseWaitMs: {},
    toasts: [],
    nextModel: null,

    setNextModel: (value) => set({ nextModel: value }),

    t: makeT('en'),
    tDynamic: makeTDynamic('en'),

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
        // The database has to answer before settings do: a locked one refuses every
        // other binding, and the unlock screen is the whole window until it opens.
        const dbStatus = await App.GetDatabaseStatus()
        set({ dbStatus })
        if (dbStatus.locked) {
          set({ ready: true, bootError: null })
          return
        }
        const settings = await App.GetSettings()
        // Settings may carry a hand-edited or older language value.
        const lang: Lang = asLang(settings.language)
        const theme = settings.theme === 'light' ? 'light' : 'dark'
        applyTheme(theme)
        // The translator is replaced alongside the language it speaks, so every
        // subscriber repaints. bootError is cleared here because this run is a new
        // attempt: after a locked start the unlock screen calls bootstrap again, and a
        // stale error would replace the window it just opened.
        set({
          settings,
          lang,
          theme,
          ready: true,
          bootError: null,
          t: makeT(lang),
          tDynamic: makeTDynamic(lang),
        })

        // The unlock screen calls bootstrap a second time, and a second wireEvents
        // would subscribe every listener twice: each stream delta would be appended to
        // the buffer twice and the reply would read double. Subscriptions are app-wide,
        // not per bootstrap, so this is guarded.
        wireEventsOnce(set, get, clearStreaming, upsertMessage)

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
        set({ bootError: errText(err), ready: true })
      }
    },

    setLang: (lang) => {
      set({ lang, t: makeT(lang), tDynamic: makeTDynamic(lang) })
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
        get().toast('error', errText(err))
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
        get().toast('error', errText(err))
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
        // A project's changed files come with it: the header's count is part of what the
        // chat *is*, and loading it on demand would let a count appear only after a click.
        if (state.chat.workspace) void get().loadFileChanges(id)
      } catch (err) {
        get().toast('error', errText(err))
      } finally {
        set({ chatLoading: false })
      }
    },

    loadFileChanges: async (chatId) => {
      try {
        const changes = await App.ProjectFileChanges(chatId)
        // Keyed by chat, so a reply for a chat the user has since left is still wanted: the
        // count belongs to that chat and will be read when it is opened again.
        set((s) => ({ fileChanges: { ...s.fileChanges, [chatId]: changes } }))
      } catch {
        // A chat with no folder, or one whose folder moved, has nothing to list. The panel
        // then shows what it had, and the file tools report the folder where the model asks.
        set((s) => ({ fileChanges: { ...s.fileChanges, [chatId]: [] } }))
      }
    },

    revertFile: async (chatId, path) => {
      try {
        await App.RevertFileChange(chatId, path)
      } catch (err) {
        get().toast('error', errText(err))
      } finally {
        // Reloaded either way: a refused revert has still told the user *why*, and the row's
        // state may have moved on since the list was read.
        await get().loadFileChanges(chatId)
        if (get().activeChatId === chatId) await get().refreshChatState()
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
        get().toast('error', errText(err))
        return null
      }
    },

    newProject: async (workspace) => {
      try {
        const created = await App.CreateProject(workspace)
        await get().loadChats()
        await get().selectChat(created.id)
        set({ view: 'chat' })
        return created.id
      } catch (err) {
        get().toast('error', errText(err))
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
        get().toast('error', errText(err))
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
        get().toast('error', errText(err))
      }
    },

    clearChat: async (id) => {
      try {
        await App.ClearChat(id)
        // The whole state is re-read rather than patched: messages, compactions and
        // attachments all go, and a cleared chat still showing the conversation it was
        // cleared of is the one outcome this must not produce. A clear while a turn runs
        // never gets here — the backend refuses it, and its sentence is what the toast
        // shows.
        if (get().activeChatId === id) await get().refreshChatState()
        clearStreaming(id)
        // A command whose output was on screen belongs to the conversation that was just
        // dropped, so its text goes with it — and so does a hand-off, which is about a turn
        // that no longer exists.
        set({ toolOutput: {}, handoffs: {} })
        get().toast('info', get().t('chat.cleared'))
      } catch (err) {
        get().toast('error', errText(err))
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
        get().toast('error', errText(err))
      }
    },

    stop: async () => {
      const chatId = get().activeChatId
      if (!chatId) return
      try {
        await App.StopGeneration(chatId)
      } catch (err) {
        get().toast('error', errText(err))
      }
    },

    resolveApproval: async (chatId, approvalId, decision, editedLine) => {
      // Taken off screen before the call, not after it: the backend resumes the turn the
      // moment it hears the answer, and a card still sitting there invites a second click
      // on a question that has already been decided. The turn's own end (done, error, or
      // stop) drops the entry for the case where nothing was ever clicked.
      set((s) => {
        const approvals = { ...s.approvals }
        delete approvals[chatId]
        return { approvals }
      })
      try {
        // The edited form answers through its own binding: an empty third argument would be
        // indistinguishable from "the field was cleared", which is not an answer at all.
        if (editedLine !== undefined) {
          await App.ResolveToolApprovalWith(approvalId, decision, editedLine)
        } else {
          await App.ResolveToolApproval(approvalId, decision)
        }
      } catch (err) {
        get().toast('error', errText(err))
      }
    },

    dismissHandoff: (chatId) => {
      set((s) => {
        const handoffs = { ...s.handoffs }
        delete handoffs[chatId]
        return { handoffs }
      })
    },

    stopAsking: async (chatId, approvalId, kind) => {
      const state = get()
      if (!state.settings) return
      // Set, not toggle: the other kind keeps whatever it was, because a save that turned a
      // permission off as a side effect would be the opposite of what was clicked.
      const auto = state.settings.autoApprove ?? { files: false, commands: false }
      const next = {
        ...state.settings,
        autoApprove: kind === 'command' ? { ...auto, commands: true } : { ...auto, files: true },
      }
      try {
        set({ settings: await App.SaveSettings(next) })
      } catch (err) {
        // Nothing is half-granted: the card stays on screen, so the change can still be
        // answered by hand and the window never claims a switch is on when it is not.
        get().toast('error', errText(err))
        return
      }
      // This change was asked about before the switch existed, so it still needs its answer.
      await get().resolveApproval(chatId, approvalId, 'once')
      get().toast(
        'info',
        get().t(kind === 'command' ? 'confirm.stoppedAskingCommands' : 'confirm.stoppedAskingFiles'),
      )
    },

    regenerate: async (backend = '', model = '') => {
      const chatId = get().activeChatId
      if (!chatId || get().running[chatId]) return
      set({ running: { ...get().running, [chatId]: true } })
      try {
        await App.Regenerate(chatId, backend, model)
      } catch (err) {
        set({ running: { ...get().running, [chatId]: false } })
        get().toast('error', errText(err))
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
        get().toast('error', errText(err))
      }
    },
  }
})

/**
 * Subscribe to backend events once, however many times bootstrap runs. Wails keeps
 * every listener for the lifetime of the webview, and bootstrap runs again after an
 * unlock, so a second pass would double every stream delta.
 */
let wired = false
function wireEventsOnce(
  set: (partial: Partial<State> | ((s: State) => Partial<State>)) => void,
  get: () => State,
  clearStreaming: (chatId: string) => void,
  upsertMessage: (message: storeTypes.Message) => void,
) {
  if (wired) return
  wired = true
  wireEvents(set, get, clearStreaming, upsertMessage)
}

/** Subscribe to backend events. wireEventsOnce is what the app calls. */
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

  /*
   * A change to the project folder is waiting for an answer here, and the turn is blocked
   * on it. Nothing is filtered by the open chat: a question asked in a chat the user has
   * since clicked away from has to still be answerable when they come back, and dropping it
   * would leave that turn waiting on a card nobody can see. The card is only *rendered* for
   * the open chat; the entry is what survives the switch.
   */
  on<ApprovalPayload>(EV.approval, (p) => {
    set((s) => ({ approvals: { ...s.approvals, [p.chatId]: p } }))
    // Its chatter is rendered on a tool call, and the call is on a message of that chat.
    if (p.chatId === active()) void get().refreshChatState()
  })

  /*
   * A running command's output, while it runs. Keyed by the call rather than the chat: the
   * card that shows it is the one rendering that call, and a chat can run two commands in
   * sequence without the second inheriting the first's text.
   */
  /*
   * A refusal the user has to act on: the app cannot answer an administrator prompt, so the
   * line is handed over rather than left in the transcript. Recognised by its code, not by its
   * sentence — the sentence is translated, and which refusal this is has to survive a language
   * switch.
   */
  on<{ chatId: string; toolCall: storeTypes.ToolCallRecord }>(EV.tool, (p) => {
    // Not filtered by the open chat, for the reason the question above is not: a refused command
    // in a project the user has since clicked away from is still theirs to run, and dropping the
    // entry would lose the only place the line is offered where they type. It is keyed by chat
    // and rendered for the open one, so coming back finds it.
    if (p.toolCall.status !== 'error' || errorCode(p.toolCall.error) !== 'tools.needsElevation') return
    const command = handoffCommand(p.toolCall.argsJson)
    if (!command) return
    set((s) => ({
      handoffs: { ...s.handoffs, [p.chatId]: { command, toolCallId: p.toolCall.id } },
    }))
  })

  on<ToolOutputPayload>(EV.toolOutput, (p) => {
    if (p.chatId !== active()) return
    set((s) => ({ toolOutput: { ...s.toolOutput, [p.toolCallId]: p.text } }))
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
    // The finished record carries the output, so the live one is dropped with it. A call
    // still waiting for its answer (`pending`) keeps its text: the command may be running
    // under an auto-approval or a remembered yes, and the card is the only place it shows.
    const toolOutput = { ...get().toolOutput }
    if (p.toolCall.status !== 'pending') delete toolOutput[p.toolCall.id]
    set({ chatState: { ...state.chatState, messages }, toolOutput })
    // A change that landed is a change the panel has to show, and the list is one query. Read
    // only for the two tools that write a file: a read or a search leaves the folder as it was
    // and the count would not move.
    if (
      (p.toolCall.status === 'ok' || p.toolCall.status === 'error') &&
      (p.toolCall.name === 'write_file' || p.toolCall.name === 'edit_file')
    ) {
      void get().loadFileChanges(p.chatId)
    }
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
    set((s) => {
      const approvals = { ...s.approvals }
      // A question that was never answered ends with its turn: the change it was asking
      // about was refused by the cancellation, so the card must not outlive the turn
      // offering to make it.
      delete approvals[p.chatId]
      return { running: { ...s.running, [p.chatId]: false }, approvals }
    })
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
    set((s) => {
      const approvals = { ...s.approvals }
      delete approvals[p.chatId]
      return { running: { ...s.running, [p.chatId]: false }, approvals }
    })
    if (p.chatId === active()) {
      // Same order as the done handler: the stored row is authoritative, so it is
      // read before the buffer that renders on top of it is dropped.
      void (async () => {
        await get().refreshChatState()
        clearStreaming(p.chatId)
      })()
    }
    get().toast('error', translateError(get().lang, p.error))
  })

  on<{ chatId: string; warning: string }>(EV.warning, (p) => {
    if (p.chatId !== active()) return
    get().toast('warning', translateError(get().lang, p.warning))
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
        // Carried as it arrives: a caption report has no steps and would otherwise look
        // like a draw that has not started, and the next step report clears it.
        phase: p.phase,
        // The preview is kept from the previous report when this one has none: a server
        // that sends its fraction and its picture separately, or a poll that landed
        // between two of them, must not make the thumbnail blink away.
        preview: p.preview ?? s.draws[p.drawId]?.preview,
      }
      return { draws }
    })
  })
}

/**
 * The text of an error for the window's current language.
 *
 * Go answers in codes (see internal/uimsg), and `String(err)` on a rejected binding
 * carries that code as the message. Every `catch` that puts the error in front of a
 * user goes through here instead, so one message is translated in one place rather
 * than at fifty call sites.
 */
export function errText(err: unknown): string {
  return translateError(useStore.getState().lang, String(err))
}

/**
 * The command a `run_command` call carried, empty when the arguments are not readable.
 *
 * A model may send arguments that are not JSON; `decodeLoose` on the Go side is the one that
 * accepts them, and this is only a copy convenience.
 */
function handoffCommand(argsJSON: string): string {
  try {
    const parsed = JSON.parse(argsJSON) as { command?: unknown }
    return typeof parsed.command === 'string' ? parsed.command.trim() : ''
  } catch {
    return ''
  }
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
