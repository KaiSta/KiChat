import { useEffect, useMemo, useState } from 'react'
// The bound bridge is imported under a different name because this file exports the
// component called App; `Api.PickProjectFolder` reads as the binding it is.
import { modelLabel, timeAgo, App as Api, type store as storeTypes } from './lib/api'
import { LANGS } from './lib/i18n'
import { errText, useStore } from './store'
import { ChatView, permissionBadge } from './components/ChatView'
import { ErrorBoundary } from './components/ErrorBoundary'
import { Settings } from './components/settings/Settings'
import { UnlockScreen } from './components/UnlockScreen'

export default function App() {
  const bootstrap = useStore((s) => s.bootstrap)
  const ready = useStore((s) => s.ready)
  const bootError = useStore((s) => s.bootError)
  const dbStatus = useStore((s) => s.dbStatus)
  const view = useStore((s) => s.view)
  // Declared above the early returns below, like every other hook here: a hook
  // that runs on some renders but not others changes the hook count and React
  // unmounts the whole tree.
  const t = useStore((s) => s.t)
  // The badge the daily check exists for: visible without opening the screen that
  // would already show the answer. The reading arrives from the backend's background
  // look (app:update) or from the one bootstrap asks for.

  useEffect(() => {
    void bootstrap()
  }, [bootstrap])

  if (!ready) {
    return (
      <div className="flex h-full items-center justify-center faint">…</div>
    )
  }

  // A locked database is not an error: it is a prompt. It comes before the error
  // branch because ensure() would report the lock as a failed start, and the unlock
  // screen is what makes that recoverable.
  if (dbStatus?.locked) {
    return <UnlockScreen status={dbStatus} />
  }

  if (bootError) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <div className="card max-w-lg p-4">
          <div className="mb-2 font-semibold" style={{ color: 'var(--danger)' }}>
            {t('app.startFailed')}
          </div>
          <pre className="mono text-[12px] whitespace-pre-wrap">{bootError}</pre>
        </div>
      </div>
    )
  }

  return (
    <div className="flex h-full">
      <Sidebar />
      <main className="flex min-w-0 flex-1 flex-col">
        {/*
          Keyed on the view and kept inside the shell, so a crash in the chat
          view leaves the sidebar usable and switching views recovers instead of
          taking the whole window down.
        */}
        <ErrorBoundary key={view} area={`view:${view}`}>
          <ViewSwitch view={view} />
        </ErrorBoundary>
      </main>
      <Toasts />
    </div>
  )
}

function ViewSwitch({ view }: { view: 'chat' | 'settings' }) {
  return view === 'settings' ? <Settings /> : <ChatView />
}

function Sidebar() {
  const t = useStore((s) => s.t)
  const view = useStore((s) => s.view)
  const setView = useStore((s) => s.setView)
  const chats = useStore((s) => s.chats)
  const activeChatId = useStore((s) => s.activeChatId)
  const lang = useStore((s) => s.lang)
  const setLang = useStore((s) => s.setLang)
  const theme = useStore((s) => s.theme)
  const setTheme = useStore((s) => s.setTheme)
  const running = useStore((s) => s.running)
  // The badge the daily check exists for: visible without opening the screen that would
  // already show the answer. The reading arrives from the backend's background look
  // (app:update) or from the one bootstrap asks for.
  const updateAvailable = useStore((s) => Boolean(s.appUpdate?.available))
  // What a project row's permission note says. Read from the stored settings rather than from
  // the chat, for the reason the composer's note is: the answer is the same for every project
  // in the window, and only the sentence about *this* folder belongs in the row.
  const autoApprove = useStore((s) => s.settings?.autoApprove)

  const [filter, setFilter] = useState('')
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)
  // Inline rename: the id being edited and the draft it holds. Renaming from the
  // list is the point — the list is where the chat to continue is chosen, and the
  // header field in the chat view reads as a static heading.
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  // The "new project" menu behind the chevron beside the new-chat button.
  const [newMenu, setNewMenu] = useState(false)

  // Any click outside closes the menu. The toggle stops its own click from reaching
  // the window, so the chevron still toggles while the menu is open.
  useEffect(() => {
    if (!newMenu) return
    const close = () => setNewMenu(false)
    window.addEventListener('click', close)
    return () => window.removeEventListener('click', close)
  }, [newMenu])

  /**
   * A new project: pick a folder, then let the backend make the chat.
   *
   * The folder is chosen before anything is created, so a cancelled dialog leaves no
   * row behind. Whether the folder can actually be used is the backend's answer to
   * give — it resolves the path and stores the resolved one — and its refusal arrives
   * as a translated toast rather than a second opinion from the window, which would be
   * the same check written twice and free to disagree.
   */
  const startProject = async () => {
    setNewMenu(false)
    try {
      const folder = await Api.PickProjectFolder(t('project.pickTitle'))
      if (!folder) return
      await useStore.getState().newProject(folder)
    } catch (err) {
      // Through errText, like every other catch that puts a failure on screen: the dialog can
      // fail with a code, and a raw `i18n:{…}` in a toast is the app's plumbing leaking out.
      useStore.getState().toast('error', errText(err))
    }
  }

  // The cycle follows the order LANGS declares, so this button and the picker in
  // Settings cannot disagree about what "next" means. Math.max also absorbs a
  // language this build does not ship: it lands on the first entry, English.
  const langIndex = Math.max(0, LANGS.findIndex((l) => l.id === lang))
  const nextLang = LANGS[(langIndex + 1) % LANGS.length]

  const filtered = useMemo(() => {
    const needle = filter.trim().toLowerCase()
    if (!needle) return chats
    return chats.filter((c) => c.title.toLowerCase().includes(needle))
  }, [chats, filter])

  /*
   * Two lists, not one, because they are two different things: a chat is a conversation,
   * and a project is a conversation *about a folder* — it has file tools, a permission
   * gate and a prompt written for a codebase. Mixed into one column, the only clue was a
   * folder glyph in the row and a name that was sometimes a directory.
   *
   * The heading is the grouping; the row carries the rest. A project row therefore ends with
   * the folder it works in and what the model may do there, because those are the two facts the
   * heading does not say and the only two that decide whether opening it is safe.
   *
   * The order inside each list is the backend's (most recently updated first) and is not
   * re-sorted here. A grouping is not a ranking: interleaving them by time would answer
   * "what did I touch last" and lose "where are my projects".
   */
  const { projects, conversations } = useMemo(() => {
    const projects: typeof filtered = []
    const conversations: typeof filtered = []
    for (const c of filtered) {
      // The same rule store.Chat.IsProject asks on the Go side. Both halves matter: a
      // coding chat whose folder was never set has nothing to work in.
      if (c.mode === 'coding' && c.workspace) projects.push(c)
      else conversations.push(c)
    }
    return { projects, conversations }
  }, [filtered])

  /**
   * One row of either list. A project and a chat differ in what they *are*, not in how
   * a row is built — both offer rename and delete, both mark the running one — so the
   * two lists share this and no row has to be told which list it is in.
   */
  const renderRow = (c: storeTypes.Chat) => {
    const active = c.id === activeChatId && view === 'chat'
    // The same rule the grouping above used, asked again for the row: a coding chat with no
    // folder is a chat, and a chat has no folder to name.
    const project = c.mode === 'coding' && !!c.workspace
    const permission = permissionBadge(t, autoApprove, true)
    return (
      <div key={c.id} className="group relative">
        {/*
          An editing <input> must not be nested inside the row <button> —
          that is invalid HTML and eats the field's focus and clicks. So the
          row renders either the normal click target or the edit field.
        */}
        {renamingId === c.id ? (
          <input
            autoFocus
            className="input"
            aria-label={t('nav.rename')}
            value={renameDraft}
            // Enter just blurs; the onBlur below is the single commit path
            // (blurring twice would rename twice). Escape unmounts the field,
            // so no blur fires and nothing is written. An empty or unchanged
            // value is skipped, so the title can never be cleared to blank.
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                e.currentTarget.blur()
              }
              if (e.key === 'Escape') setRenamingId(null)
            }}
            onChange={(e) => setRenameDraft(e.target.value)}
            onBlur={(e) => {
              const value = e.target.value.trim()
              if (value && value !== c.title) void useStore.getState().renameChat(c.id, value)
              setRenamingId(null)
            }}
            style={active ? { background: 'var(--surface-3)' } : undefined}
          />
        ) : (
          <button
            type="button"
            onClick={() => {
              setView('chat')
              void useStore.getState().selectChat(c.id)
            }}
            className="w-full rounded-md px-2 py-2 text-left text-[13px] transition-colors hover:bg-[var(--surface-2)]"
            style={active ? { background: 'var(--surface-3)' } : undefined}
          >
            <div className="flex items-center gap-2">
              {running[c.id] && (
                <span className="spin inline-block h-2 w-2 shrink-0 rounded-full" style={{ background: 'var(--accent)' }} />
              )}
              <span className="truncate">{c.title || t('chat.empty.title')}</span>
            </div>
            {/* Where a project works, and what may happen there without a question. The path is
                the *end* of it rather than the name: a project is usually already titled after
                its folder, so the name would repeat the line above and the parent segments are
                the part that tells two folders called `api` apart. The tooltip has all of it. */}
            {project && (
              <div className="mt-0.5 flex items-center gap-1 text-[11px]">
                <span className="mono truncate" style={{ color: 'var(--text-faint)' }} title={c.workspace}>
                  {folderTail(c.workspace)}
                </span>
                <span className="shrink-0 whitespace-nowrap" style={{ color: 'var(--text-faint)' }}>·</span>
                <span className="faint shrink-0 whitespace-nowrap" title={permission.hint}>
                  {permission.badge}
                </span>
              </div>
            )}
            <div className="faint mt-0.5 flex items-center gap-2 text-[11px]">
              {/* The chat list says which model a conversation is on. A local
                  model's id is a path, so the name is what is shown and the
                  id is the tooltip. */}
              <span
                className="mono truncate"
                style={{ maxWidth: '8rem' }}
                title={c.model || undefined}
              >
                {modelLabel(c.model) || '–'}
              </span>
              <span>·</span>
              <span>{timeAgo(c.updatedAt, lang)}</span>
            </div>
          </button>
        )}
        {/* Both row actions live in one hover-revealed group; they are hidden
            while the field is open so nothing overlaps the input. */}
        {renamingId !== c.id && (
          <div className="absolute top-1.5 right-1 flex gap-1 opacity-0 group-hover:opacity-100">
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              title={t('nav.rename')}
              aria-label={t('nav.rename')}
              onClick={() => {
                setRenameDraft(c.title)
                setRenamingId(c.id)
              }}
            >
              ✎
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              title={t('nav.delete')}
              aria-label={t('nav.delete')}
              onClick={() => setConfirmDelete(c.id)}
            >
              ✕
            </button>
          </div>
        )}
      </div>
    )
  }

  return (
    <aside
      className="flex w-[17rem] shrink-0 flex-col border-r"
      style={{ borderColor: 'var(--border)', background: 'var(--surface)' }}
    >
      <div className="flex items-center gap-2 px-3 py-3">
        <div className="min-w-0 flex-1">
          <div className="truncate font-semibold">{t('app.title')}</div>
          <div className="faint truncate text-[11px]">{t('app.subtitle')}</div>
        </div>
        <button
          type="button"
          className="btn btn-sm btn-ghost"
          title={theme === 'dark' ? t('settings.themeLight') : t('settings.themeDark')}
          onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
        >
          {theme === 'dark' ? '☀' : '☾'}
        </button>
        <button
          type="button"
          className="btn btn-sm btn-ghost"
          title={nextLang.label}
          onClick={() => setLang(nextLang.id)}
        >
          {LANGS[langIndex].label}
        </button>
      </div>

      {/*
        The new-chat button carries the project beside it rather than in a second
        button: both are "start something", and which one it is depends on whether the
        work happens inside your files. The chevron is a menu rather than a split
        button's right half, because pressing "new chat" by accident is cheap while
        pressing "choose a folder" by accident is a dialog to dismiss.
      */}
      <div className="relative px-3 pb-2">
        <div className="flex gap-1">
          <button
            type="button"
            className="btn btn-primary min-w-0 flex-1 justify-center"
            onClick={async () => {
              await useStore.getState().newChat()
              setView('chat')
            }}
          >
            + {t('nav.newChat')}
          </button>
          <button
            type="button"
            className="btn btn-ghost"
            aria-haspopup="menu"
            aria-expanded={newMenu}
            aria-label={t('nav.newProject')}
            title={t('nav.newProject')}
            onClick={(event) => {
              event.stopPropagation()
              setNewMenu((open) => !open)
            }}
          >
            ▾
          </button>
        </div>
        {newMenu && (
          <div className="card absolute right-3 left-3 z-30 mt-1 p-1" role="menu">
            <button
              type="button"
              role="menuitem"
              className="w-full rounded-md px-2 py-2 text-left text-[13px] transition-colors hover:bg-[var(--surface-2)]"
              onClick={() => void startProject()}
            >
              <div className="truncate font-medium">{t('nav.newProject')}</div>
              <div className="faint text-[11px]">{t('nav.newProjectHint')}</div>
            </button>
          </div>
        )}
      </div>

      <div className="px-3 pb-2">
        <input
          className="input"
          placeholder={t('nav.searchPlaceholder')}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>

      <nav className="min-h-0 flex-1 overflow-y-auto scroll-thin px-2 pb-2">
        {filtered.length === 0 && <div className="px-2 py-3 faint text-[12px]">{t('nav.noChats')}</div>}
        {/* A heading is drawn only when its list is not empty, so a filter that matches
            one kind does not leave the other's heading standing over nothing. */}
        {projects.length > 0 && <GroupHeading label={t('nav.projects')} />}
        {projects.map(renderRow)}
        {conversations.length > 0 && <GroupHeading label={t('nav.chats')} />}
        {conversations.map(renderRow)}
      </nav>

      <div className="border-t px-3 py-2" style={{ borderColor: 'var(--border)' }}>
        <button
          type="button"
          className={`btn w-full justify-center ${view === 'settings' ? 'btn-primary' : ''}`}
          onClick={() => setView(view === 'settings' ? 'chat' : 'settings')}
        >
          ⚙ {t('nav.settings')}
          {/* A dot, not a word pill: this sits on screen for as long as an update is
              outstanding, and a coloured badge there turns every glance at the sidebar
              into a reminder. The tooltip and the label carry the meaning. */}
          {updateAvailable && (
            <span
              className="ml-2 inline-block h-1.5 w-1.5 rounded-full align-middle"
              style={{ background: 'var(--warn)' }}
              role="img"
              aria-label={t('settings.updateAvailable')}
              title={t('settings.updateAvailable')}
            />
          )}
        </button>
      </div>

      {confirmDelete && (
        <div className="absolute inset-0 z-40 flex items-center justify-center" style={{ background: 'rgba(0,0,0,0.5)' }}>
          {/* A confirmation that covers the window has to behave like one for a keyboard
              and a screen reader too: it is a modal dialog, it takes focus to the safe
              choice, Escape closes it, and Tab cycles inside it rather than walking
              into the sidebar behind the overlay. */}
          <div
            className="card w-80 p-4"
            role="dialog"
            aria-modal="true"
            aria-labelledby="confirm-delete-title"
            onKeyDown={(event) => {
              if (event.key === 'Escape') {
                event.preventDefault()
                setConfirmDelete(null)
                return
              }
              if (event.key !== 'Tab') return
              const buttons = event.currentTarget.querySelectorAll<HTMLButtonElement>('button')
              const first = buttons[0]
              const last = buttons[buttons.length - 1]
              if (!first || !last) return
              if (event.shiftKey && document.activeElement === first) {
                event.preventDefault()
                last.focus()
              } else if (!event.shiftKey && document.activeElement === last) {
                event.preventDefault()
                first.focus()
              }
            }}
          >
            <div id="confirm-delete-title" className="mb-3">
              {t('nav.delete')}?
            </div>
            <div className="flex justify-end gap-2">
              <button type="button" className="btn" autoFocus onClick={() => setConfirmDelete(null)}>
                {t('common.cancel')}
              </button>
              <button
                type="button"
                className="btn btn-danger"
                onClick={async () => {
                  await useStore.getState().deleteChat(confirmDelete)
                  setConfirmDelete(null)
                }}
              >
                {t('common.delete')}
              </button>
            </div>
          </div>
        </div>
      )}
    </aside>
  )
}

/**
 * A heading over one of the list's two groups.
 *
 * It is not a control and takes no clicks: the grouping is a fact about the rows below it,
 * not something to switch. A count is deliberately absent — the list is short, the number
 * would be read as a counter that matters, and it changes under the reader's hands while a
 * project is being created.
 */
/**
 * The end of a project's path: enough to tell two folders of the same name apart.
 *
 * Two segments rather than one, because the name alone is usually the row's title already —
 * the parent is what a window full of `api` and `web` needs. The separator is always `/`,
 * the app's own: a Windows path shown to someone who reads Unix paths is one more thing to
 * translate, and the tooltip carries the path exactly as stored.
 */
function folderTail(path: string | undefined, segments = 2): string {
  if (!path) return ''
  const parts = path.replace(/[/\\]+$/, '').split(/[/\\]/).filter(Boolean)
  return parts.slice(-segments).join('/')
}

function GroupHeading({ label }: { label: string }) {
  return (
    <div
      className="faint px-2 pt-3 pb-1 text-[10px] tracking-wide uppercase"
      style={{ color: 'var(--text-dim)' }}
    >
      {label}
    </div>
  )
}

function Toasts() {
  const toasts = useStore((s) => s.toasts)
  const dismiss = useStore((s) => s.dismissToast)
  const t = useStore((s) => s.t)
  if (toasts.length === 0) return null
  return (
    <div className="pointer-events-none absolute right-4 bottom-4 z-50 flex w-96 flex-col gap-2">
      {toasts.map((toast) => (
        /* A warning is how this app reports a degraded turn — a search that failed,
         * a reply that could not be saved — so it has to reach a screen reader too.
         * role carries the urgency: an error is announced at once, everything else
         * waits its turn. aria-atomic reads the message as one sentence instead of
         * announcing the icon, the text and the close button separately. */
        <div
          key={toast.id}
          role={toast.kind === 'error' ? 'alert' : 'status'}
          aria-atomic="true"
          className="card pointer-events-auto flex items-start gap-2 p-3 text-[12px]"
          style={{
            boxShadow: 'var(--shadow)',
            borderColor:
              toast.kind === 'error'
                ? 'var(--danger)'
                : toast.kind === 'warning'
                  ? 'var(--warn)'
                  : 'var(--border-strong)',
          }}
        >
          <span>{toast.kind === 'error' ? '⛔' : toast.kind === 'warning' ? '⚠' : 'ℹ'}</span>
          <span className="min-w-0 flex-1 whitespace-pre-wrap break-words">{toast.message}</span>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            aria-label={t('common.close')}
            onClick={() => dismiss(toast.id)}
          >
            ✕
          </button>
        </div>
      ))}
    </div>
  )
}

