import { useEffect, useRef, useState } from 'react'
import { App, type config, type main } from '../../lib/api'
import { errText, useStore } from '../../store'
import { Section, Field, translatedOr } from './shared'

export function PromptTab({
  draft,
  patch,
}: {
  draft: config.Settings
  patch: (c: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  // Built keys, and backend-supplied ones: see translateDynamic.
  const tDynamic = useStore((s) => s.tDynamic)
  // A shipped preset is shown under its translated name, and a shipped variable
  // under its translated description. translatedOr() falls back to the text the
  // backend sent when a key has no entry — that is how a preset the user adds
  // later still shows the name it was given instead of "preset.my-id".
  const presetLabel = (p: config.PromptPreset) => translatedOr(tDynamic, `preset.${p.id}`, p.name)
  const variableLabel = (v: main.PromptVariable) => translatedOr(tDynamic, v.key, v.description)
  const [variables, setVariables] = useState<main.PromptVariable[]>([])
  const [preview, setPreview] = useState('')
  const [unknown, setUnknown] = useState<string[]>([])
  const [showPreview, setShowPreview] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement | null>(null)
  // Which stored prompt the editor above is changing, by id. This is a view state rather than a
  // setting: it only says what the textarea shows. What a conversation starts on is the pair of
  // selectors below, and both of them name one of these same prompts — which is why the editor
  // has to write into the preset rather than into a field of its own: a prompt nobody can name
  // is a prompt neither selector could ever pick.
  const [editingID, setEditingID] = useState(() => {
    const match = draft.promptPresets.find((p) => p.prompt === draft.defaultSystemPrompt)
    return match?.id ?? draft.promptPresets[0]?.id ?? ''
  })
  const editing = draft.promptPresets.find((p) => p.id === editingID) ?? draft.promptPresets[0]
  const prompt = editing?.prompt ?? ''

  const setPrompt = (next: string) => {
    if (!editing) return
    patch({
      promptPresets: draft.promptPresets.map((p) =>
        p.id === editing.id ? { ...p, prompt: next } : p,
      ),
    })
  }

  useEffect(() => {
    void App.GetPromptVariables().then(setVariables).catch(() => undefined)
  }, [])

  // Debounce validation so typing does not spam the backend.
  useEffect(() => {
    const handle = window.setTimeout(() => {
      void App.UnresolvedPromptVars(prompt).then(setUnknown).catch(() => undefined)
      if (showPreview) {
        void App.PreviewSystemPrompt(prompt).then(setPreview).catch(() => undefined)
      }
    }, 250)
    return () => window.clearTimeout(handle)
  }, [prompt, showPreview])

  const insertVariable = (name: string) => {
    const el = textareaRef.current
    const token = `{{${name}}}`
    if (!el) {
      setPrompt(prompt + token)
      return
    }
    const start = el.selectionStart ?? prompt.length
    const end = el.selectionEnd ?? start
    const next = prompt.slice(0, start) + token + prompt.slice(end)
    setPrompt(next)
    requestAnimationFrame(() => {
      el.focus()
      el.setSelectionRange(start + token.length, start + token.length)
    })
  }

  return (
    <>
      <Section title={t('settings.prompt')} hint={t('settings.promptHint')}>
        <Field label={t('settings.promptPreset')} hint={t('settings.promptPresetHint')}>
          <div className="flex gap-2">
            <select
              className="select"
              value={editing?.id ?? ''}
              onChange={(e) => setEditingID(e.target.value)}
            >
              {draft.promptPresets.map((p) => (
                <option key={p.id} value={p.id}>
                  {presetLabel(p)}
                </option>
              ))}
            </select>
            <button
              type="button"
              className="btn"
              title={t('settings.promptReload')}
              aria-label={t('settings.promptReload')}
              onClick={() => {
                // Re-read what is stored. Discarding unsaved edits here is what the
                // button means sitting next to the preset list, and the store is
                // updated with it so the rest of the view agrees. This used to fetch
                // the settings and drop the answer, so the button did nothing at all.
                void App.GetSettings()
                  .then((fresh) => {
                    patch(fresh)
                    useStore.setState({ settings: fresh })
                  })
                  .catch((err) => useStore.getState().toast('error', errText(err)))
              }}
            >
              ↺
            </button>
          </div>
        </Field>

        <textarea
          ref={textareaRef}
          className="textarea mono text-[12px]"
          rows={18}
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
        />

        {unknown.length > 0 && (
          <div className="mt-2 rounded p-2 text-[12px]" style={{ background: 'color-mix(in srgb, var(--warn) 12%, transparent)', color: 'var(--warn)' }}>
            {t('settings.promptUnknownVars')}: {unknown.map((v) => `{{${v}}}`).join(', ')}
          </div>
        )}

        <div className="mt-3 flex flex-wrap gap-2">
          <button type="button" className="btn btn-sm" onClick={() => setShowPreview((v) => !v)}>
            {t('settings.promptPreview')}
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => {
              // Only this prompt: the other presets are the rest of the draft's work, and
              // discarding them from a button beside one textarea would be a surprise.
              void App.GetSettings()
                .then((fresh) => {
                  const stored = fresh.promptPresets.find((p) => p.id === editing?.id)
                  if (stored) setPrompt(stored.prompt)
                })
                .catch((err) => useStore.getState().toast('error', errText(err)))
            }}
          >
            {t('settings.promptReset')}
          </button>
        </div>

        {showPreview && (
          <pre
            className="mono mt-2 max-h-80 overflow-auto whitespace-pre-wrap rounded p-3 text-[11px]"
            style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
          >
            {preview}
          </pre>
        )}
      </Section>

      {/* Which named prompt each mode starts on. The editor above changes one of these prompts;
          these two say which one a new chat and a new project get, so a project no longer has
          to take the coding-agent preset because it is the only one the app names for code. */}
      <Section title={t('settings.promptModes')} hint={t('settings.promptModesHint')}>
        <Field label={t('settings.promptModeChat')} hint={t('settings.promptModeChatHint')}>
          <select
            className="select"
            value={draft.chatPromptPreset || 'research'}
            onChange={(e) => patch({ chatPromptPreset: e.target.value })}
          >
            {draft.promptPresets.map((p) => (
              <option key={p.id} value={p.id}>
                {presetLabel(p)}
              </option>
            ))}
          </select>
        </Field>

        <Field label={t('settings.promptModeCoding')} hint={t('settings.promptModeCodingHint')}>
          <select
            className="select"
            value={draft.codingPromptPreset || 'coding-agent'}
            onChange={(e) => patch({ codingPromptPreset: e.target.value })}
          >
            {draft.promptPresets.map((p) => (
              <option key={p.id} value={p.id}>
                {presetLabel(p)}
              </option>
            ))}
          </select>
        </Field>
      </Section>

      <Section title={t('settings.promptVariables')}>
        <div className="space-y-1">
          {variables.map((v) => (
            <div key={v.name} className="flex items-center gap-2 text-[12px]">
              <code className="mono">{`{{${v.name}}}`}</code>
              <span className="faint min-w-0 flex-1 truncate">{variableLabel(v)}</span>
              <button type="button" className="btn btn-sm btn-ghost" onClick={() => insertVariable(v.name)}>
                {t('settings.promptInsert')}
              </button>
            </div>
          ))}
        </div>
      </Section>
    </>
  )
}

/* -------------------------------------------------------------------------
 * Research
 * ---------------------------------------------------------------------- */
