import { useEffect, useMemo, useState } from 'react'
import { timeAgo } from './lib/api'
import { LANGS } from './lib/i18n'
import { useStore } from './store'
import { ChatView } from './components/ChatView'
import { ErrorBoundary } from './components/ErrorBoundary'
import { Settings } from './components/Settings'

export default function App() {
  const bootstrap = useStore((s) => s.bootstrap)
  const ready = useStore((s) => s.ready)
  const bootError = useStore((s) => s.bootError)
  const view = useStore((s) => s.view)
  // Declared above the early returns below, like every other hook here: a hook
  // that runs on some renders but not others changes the hook count and React
  // unmounts the whole tree.
  const t = useStore((s) => s.t)

  useEffect(() => {
    void bootstrap()
  }, [bootstrap])

  if (!ready) {
    return (
      <div className="flex h-full items-center justify-center faint">…</div>
    )
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

  const [filter, setFilter] = useState('')
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)

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

      <div className="px-3 pb-2">
        <button
          type="button"
          className="btn btn-primary w-full justify-center"
          onClick={async () => {
            await useStore.getState().newChat()
            setView('chat')
          }}
        >
          + {t('nav.newChat')}
        </button>
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
        {filtered.map((c) => {
          const active = c.id === activeChatId && view === 'chat'
          return (
            <div key={c.id} className="group relative">
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
                <div className="faint flex items-center gap-2 text-[11px]">
                  <span className="mono truncate" style={{ maxWidth: '8rem' }}>
                    {c.model || '–'}
                  </span>
                  <span>·</span>
                  <span>{timeAgo(c.updatedAt, lang)}</span>
                </div>
              </button>
              <button
                type="button"
                className="btn btn-sm btn-ghost absolute top-1.5 right-1 opacity-0 group-hover:opacity-100"
                title={t('nav.delete')}
                onClick={() => setConfirmDelete(c.id)}
              >
                ✕
              </button>
            </div>
          )
        })}
      </nav>

      <div className="border-t px-3 py-2" style={{ borderColor: 'var(--border)' }}>
        <button
          type="button"
          className={`btn w-full justify-center ${view === 'settings' ? 'btn-primary' : ''}`}
          onClick={() => setView(view === 'settings' ? 'chat' : 'settings')}
        >
          ⚙ {t('nav.settings')}
        </button>
      </div>

      {confirmDelete && (
        <div className="absolute inset-0 z-40 flex items-center justify-center" style={{ background: 'rgba(0,0,0,0.5)' }}>
          <div className="card w-80 p-4">
            <div className="mb-3">{t('nav.delete')}?</div>
            <div className="flex justify-end gap-2">
              <button type="button" className="btn" onClick={() => setConfirmDelete(null)}>
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

