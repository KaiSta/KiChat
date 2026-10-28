import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { App, type config } from '../../lib/api'
import { errText, useStore } from '../../store'
import { GeneralTab } from './GeneralTab'
import { BackendsTab } from './BackendsTab'
import { ImagesTab } from './ImagesTab'
import { PromptTab } from './PromptTab'
import { CodingTab } from './CodingTab'
import { ResearchTab } from './ResearchTab'
import { PayloadTab } from './PayloadTab'
import { ContextTab } from './ContextTab'
import { DataTab } from './DataTab'

type Tab =
  | 'general'
  | 'backends'
  | 'images'
  | 'prompt'
  | 'coding'
  | 'research'
  | 'context'
  | 'payload'
  | 'data'

export function Settings() {
  const t = useStore((s) => s.t)
  const loaded = useStore((s) => s.settings)
  const [tab, setTab] = useState<Tab>('general')
  const [draft, setDraft] = useState<config.Settings | null>(loaded)
  const [saving, setSaving] = useState(false)
  const paneRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    setDraft(loaded)
  }, [loaded])

  /*
   * A tab switch starts at the top of the new tab.
   *
   * The pane is one element for every tab, so it keeps the scroll offset of the tab
   * that was just left. The engine is expected to clamp that offset when the content
   * that justified it is replaced by something shorter, and WebKitGTK does in the plain
   * case — measured: leaving Backends at scrollTop 1954 for General, whose content ends
   * at 215, lands on 10 with the heading in view. But the clamp follows the engine's own
   * layout and scroll-animation timing, not the switch, and an offset that survives past
   * the end of the new content paints an empty pane while the tab strip stays in place.
   * That is the blank tab as reported, and why reopening Settings cures it: a fresh pane
   * starts at 0. Resetting here, in a layout effect, runs after the new tab is committed
   * and before the frame is painted, so the stale position is never shown at all.
   */
  useLayoutEffect(() => {
    if (paneRef.current) paneRef.current.scrollTop = 0
  }, [tab])

  if (!draft) return <div className="flex flex-1 items-center justify-center faint">{t('common.loading')}</div>

  const patch = (changes: Partial<config.Settings>) => setDraft({ ...draft, ...changes })

  // A row's own "turn on and check" button changes the draft and applies it in the
  // same click, and setState has not landed by the time this runs, so the payload
  // can be handed in rather than always read from state.
  const save = async (next?: config.Settings) => {
    const payload = next ?? draft
    setSaving(true)
    try {
      // Adopt what the backend stored rather than the draft: the untouched
      // built-in prompts are rewritten into the language being saved, so the
      // draft and the stored settings would otherwise disagree until a restart.
      const stored = await App.SaveSettings(payload)
      setDraft(stored)
      useStore.setState({ settings: stored })
      useStore.getState().toast('info', t('settings.saved'))
      await useStore.getState().refreshModels(true)
      await useStore.getState().refreshHealth()
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setSaving(false)
    }
  }

  const tabs: { id: Tab; label: string }[] = [
    { id: 'general', label: t('settings.general') },
    { id: 'backends', label: t('settings.backends') },
    // Next to the chat backends, because it is the other server a user has to run
    // themselves — but a tab of its own, because the setup does not fit beside one.
    { id: 'images', label: t('settings.images') },
    { id: 'prompt', label: t('settings.prompt') },
    // Beside the prompts rather than with the general settings: this is what the coding
    // mode's tools may do unasked, which is the other half of how a project chat behaves.
    { id: 'coding', label: t('settings.coding') },
    { id: 'research', label: t('settings.research') },
    { id: 'context', label: t('settings.compaction') },
    // Its own tab rather than a section of Compaction: a fold rewrites a history the
    // model has already read, this decides what a tool result becomes on its way in,
    // and the two are configured with a different model each.
    { id: 'payload', label: t('settings.payload') },
    // Last, because it is about the installation rather than about how a chat behaves:
    // everything above configures what KiChat does, this one moves it elsewhere.
    { id: 'data', label: t('settings.data') },
  ]

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center gap-2 border-b px-4 py-2" style={{ borderColor: 'var(--border)' }}>
        <strong>{t('settings.title')}</strong>
        {/* The tab strip is one control to a screen reader, not seven buttons: the
            selected tab is marked, and each names the panel it shows. */}
        <div className="ml-4 flex gap-1" role="tablist" aria-label={t('settings.title')}>
          {tabs.map((item) => (
            <button
              key={item.id}
              id={`settings-tab-${item.id}`}
              type="button"
              role="tab"
              aria-selected={tab === item.id}
              aria-controls={`settings-panel-${item.id}`}
              className={`btn btn-sm ${tab === item.id ? 'btn-primary' : 'btn-ghost'}`}
              onClick={() => setTab(item.id)}
            >
              {item.label}
            </button>
          ))}
        </div>
        <button type="button" className="btn btn-sm btn-primary ml-auto" disabled={saving} onClick={() => void save()}>
          {saving ? '…' : t('settings.save')}
        </button>
      </div>

      <div
        ref={paneRef}
        role="tabpanel"
        id={`settings-panel-${tab}`}
        aria-labelledby={`settings-tab-${tab}`}
        // The pane is the scroll container, so it has to be reachable by keyboard.
        tabIndex={0}
        className="min-h-0 flex-1 overflow-y-auto scroll-thin p-4"
      >
        <div className="mx-auto max-w-3xl space-y-4">
          {tab === 'general' && <GeneralTab draft={draft} patch={patch} />}
          {tab === 'backends' && <BackendsTab draft={draft} patch={patch} onApply={save} />}
          {tab === 'images' && <ImagesTab draft={draft} patch={patch} onApply={save} />}
          {tab === 'prompt' && <PromptTab draft={draft} patch={patch} />}
          {tab === 'coding' && <CodingTab draft={draft} patch={patch} />}
          {tab === 'research' && <ResearchTab draft={draft} patch={patch} />}
          {tab === 'context' && <ContextTab draft={draft} patch={patch} />}
          {tab === 'payload' && <PayloadTab draft={draft} patch={patch} />}
          {tab === 'data' && <DataTab />}
        </div>
      </div>
    </div>
  )
}

