import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { formatNumber, modelLabel, type llm } from '../lib/api'
import {
  buildModelSearchIndex,
  groupEntries,
  modelQualifiers,
  searchModels,
} from '../lib/modelSearch'
import { useStore } from '../store'
import { Highlighted } from './Highlighted'

export interface ModelRef {
  backend: string
  model: string
}

interface ModelPickerProps {
  /** Element id of the search field, and the prefix of its option ids. */
  id: string
  /** Id of the visible label, linked through `aria-labelledby`. */
  labelledBy: string
  models: llm.ModelInfo[]
  value: ModelRef | null | undefined
  onChange: (ref: ModelRef | null) => void
  /**
   * The row that clears the choice: "Automatic", "Off", "Same as the chat".
   * Omitted by pickers where a choice is required rather than clearable — the
   * model-override editor has no meaningful empty state.
   */
  emptyLabel?: string
  /** Appended to a stored model the catalogue does not currently contain. */
  unavailableLabel?: string
  /** Shown when nothing is chosen and there is no `emptyLabel`. */
  placeholder?: string
}

interface PinnedOption {
  key: string
  ref: ModelRef | null
  label: string
  sublabel?: string
  title?: string
}

/**
 * A searchable model picker for the settings panels.
 *
 * It is an ARIA combobox: a real text field that filters, and a listbox under it.
 * The list renders **in flow** rather than as a floating popup, because the settings
 * pane is the scroll container (`Settings.tsx`, the `overflow-y-auto` wrapper) — a
 * popup near the bottom of a section would be clipped by that pane, and an in-flow
 * list simply extends the scrollable area.
 *
 * A stored model that is missing from the catalogue gets a pinned row of its own.
 * Without it, switching a backend off would make the setting look as if it had been
 * reset to the empty option, which is exactly what a settings screen must not do.
 */
export function ModelPicker({
  id,
  labelledBy,
  models,
  value,
  onChange,
  emptyLabel,
  unavailableLabel,
  placeholder,
}: ModelPickerProps) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  // Index into pinned rows followed by the ranked results; what Enter commits.
  const [active, setActive] = useState(0)
  const boxRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const index = useMemo(() => buildModelSearchIndex(models), [models])
  const results = useMemo(() => searchModels(index, query), [index, query])
  const grouped = useMemo(() => groupEntries(results), [results])
  const qualifiers = useMemo(() => modelQualifiers(models), [models])

  const selectedKey = value?.model ? `${value.backend}/${value.model}` : ''
  const unavailable = selectedKey !== '' && !models.some((m) => `${m.backend}/${m.id}` === selectedKey)

  const pinned = useMemo(() => {
    const list: PinnedOption[] = []
    if (emptyLabel) list.push({ key: '\u0000empty', ref: null, label: emptyLabel })
    if (unavailable && value?.model && unavailableLabel) {
      list.push({
        key: selectedKey,
        ref: { backend: value.backend, model: value.model },
        label: modelLabel(value.model),
        sublabel: `${value.backend} · ${unavailableLabel}`,
        title: value.model,
      })
    }
    return list
  }, [emptyLabel, unavailable, unavailableLabel, value, selectedKey])

  const resultIndex = useMemo(() => {
    const map = new Map<string, number>()
    results.forEach((entry, i) => map.set(`${entry.model.backend}/${entry.model.id}`, pinned.length + i))
    return map
  }, [results, pinned.length])

  const listId = `${id}-list`
  const optionId = (i: number) => `${id}-opt-${i}`

  // Close on outside click so the field behaves like every other dropdown on the screen.
  useEffect(() => {
    if (!open) return
    const handler = (event: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(event.target as Node)) {
        setOpen(false)
        setQuery('')
      }
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  /**
   * Opens the list with an empty query and puts the keyboard cursor on the current
   * choice, so opening and pressing Enter is a no-op rather than a change.
   */
  const openWithSelection = () => {
    setQuery('')
    setOpen(true)
    if (!value?.model) {
      setActive(0)
      return
    }
    if (unavailable && unavailableLabel) {
      setActive(1)
      return
    }
    const at = index.entries.findIndex(
      (entry) => entry.model.backend === value.backend && entry.model.id === value.model,
    )
    setActive(at >= 0 ? pinned.length + at : pinned.length)
  }

  const commitAt = (i: number) => {
    if (i < 0 || i >= pinned.length + results.length) return
    if (i < pinned.length) {
      const option = pinned[i]
      if (option) onChange(option.ref)
    } else {
      const entry = results[i - pinned.length]
      if (entry) onChange({ backend: entry.model.backend, model: entry.model.id })
    }
    setQuery('')
    setOpen(false)
    setActive(0)
    inputRef.current?.blur()
  }

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    const total = pinned.length + results.length
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault()
        if (!open) {
          openWithSelection()
          return
        }
        setActive((i) => (total === 0 ? 0 : (i + 1) % total))
        return
      case 'ArrowUp':
        event.preventDefault()
        if (!open) {
          openWithSelection()
          return
        }
        setActive((i) => (total === 0 ? 0 : (i - 1 + total) % total))
        return
      case 'Home':
        if (open) {
          event.preventDefault()
          setActive(0)
        }
        return
      case 'End':
        if (open) {
          event.preventDefault()
          setActive(total === 0 ? 0 : total - 1)
        }
        return
      case 'Enter':
        event.preventDefault()
        if (!open) {
          openWithSelection()
          return
        }
        // Typing narrows the list; Enter takes a match rather than falling through to
        // the pinned empty row, because "clear my choice" is not what a search means.
        if (results.length > 0) {
          commitAt(active >= pinned.length ? active : pinned.length)
        } else if (query.trim() === '' && pinned.length > 0) {
          commitAt(Math.min(active, pinned.length - 1))
        }
        return
      case 'Escape':
        event.preventDefault()
        setOpen(false)
        setQuery('')
        return
      case 'Tab':
        setOpen(false)
        setQuery('')
        return
    }
  }

  const activeId = open && active < pinned.length + results.length ? optionId(active) : undefined

  // Opening on a selection far down a long catalogue has to bring that row into
  // view; otherwise the keyboard cursor is somewhere the eye cannot follow.
  useEffect(() => {
    if (!activeId) return
    document.getElementById(activeId)?.scrollIntoView({ block: 'nearest' })
  }, [activeId])

  return (
    <div ref={boxRef} className="relative">
      <input
        id={id}
        ref={inputRef}
        className="input model-picker"
        type="text"
        role="combobox"
        autoComplete="off"
        spellCheck={false}
        value={query}
        placeholder={value?.model ? modelLabel(value.model) : (emptyLabel ?? placeholder ?? '')}
        aria-labelledby={labelledBy}
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-autocomplete="list"
        aria-activedescendant={activeId}
        onClick={() => {
          if (!open) openWithSelection()
        }}
        onChange={(event) => {
          setQuery(event.target.value)
          setOpen(true)
          setActive(pinned.length)
        }}
        onKeyDown={onKeyDown}
      />

      {open && (
        <div className="card mt-1 p-1" style={{ boxShadow: 'var(--shadow)' }}>
          <div id={listId} role="listbox" aria-labelledby={labelledBy} className="max-h-64 overflow-y-auto scroll-thin">
            {pinned.map((option, i) => (
              <PickerRow
                key={option.key}
                id={optionId(i)}
                label={option.label}
                title={option.title}
                sublabel={option.sublabel}
                selected={
                  option.ref
                    ? option.ref.backend === value?.backend && option.ref.model === value?.model
                    : !value?.model
                }
                active={active === i}
                onEnter={() => setActive(i)}
                onSelect={() => commitAt(i)}
              />
            ))}

            {grouped.map(([backend, entries]) => (
              <div key={backend}>
                <div
                  role="presentation"
                  className="faint mono px-2 pb-0.5 pt-1.5 text-[10px] uppercase tracking-wide"
                >
                  {backend}
                </div>
                {entries.map((entry) => {
                  const key = `${entry.model.backend}/${entry.model.id}`
                  const i = resultIndex.get(key) ?? 0
                  const qualifier = qualifiers.get(key) ?? ''
                  return (
                    <PickerRow
                      key={key}
                      id={optionId(i)}
                      label={<Highlighted text={entry.label} query={query} />}
                      title={entry.model.id}
                      sublabel={qualifier ? `${entry.model.backend} · ${qualifier}` : entry.model.backend}
                      contextLimit={entry.model.contextLimit}
                      selected={selectedKey === key}
                      active={active === i}
                      onEnter={() => setActive(i)}
                      onSelect={() => commitAt(i)}
                    />
                  )
                })}
              </div>
            ))}

            {results.length === 0 && (
              <div role="presentation" className="faint px-2 py-3 text-center text-[12px]">
                {models.length === 0
                  ? t('model.noModels')
                  : t('model.noMatches', { query: query.trim() })}
              </div>
            )}
          </div>

          <div
            className="faint mt-1 flex items-center justify-between gap-2 border-t px-2 pt-1 text-[10px]"
            style={{ borderColor: 'var(--border)' }}
          >
            <span>{t('model.resultCount', { shown: results.length, total: models.length })}</span>
            {query.trim() !== '' && (
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onMouseDown={(event) => event.preventDefault()}
                onClick={() => {
                  setQuery('')
                  setActive(pinned.length)
                  inputRef.current?.focus()
                }}
              >
                {t('model.clearSearch')}
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function PickerRow({
  id,
  label,
  title,
  sublabel,
  contextLimit,
  selected,
  active,
  onEnter,
  onSelect,
}: {
  id: string
  label: ReactNode
  title?: string
  sublabel?: string
  contextLimit?: number
  selected: boolean
  active: boolean
  onEnter: () => void
  onSelect: () => void
}) {
  return (
    <div
      id={id}
      role="option"
      aria-selected={selected}
      title={title}
      // Keep the caret in the search field: a click must pick a row, not blur the
      // input and close the list before the click lands.
      onMouseDown={(event) => event.preventDefault()}
      onMouseEnter={onEnter}
      onClick={onSelect}
      className="flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-[12px]"
      style={{
        background: selected
          ? 'color-mix(in srgb, var(--accent) 14%, transparent)'
          : active
            ? 'var(--surface-2)'
            : undefined,
      }}
    >
      <span className="mono min-w-0 flex-1 truncate">{label}</span>
      {sublabel && <span className="faint shrink-0 text-[11px]">{sublabel}</span>}
      {contextLimit ? (
        <span className="mono faint shrink-0 text-[11px]">{formatNumber(contextLimit)}</span>
      ) : null}
      {selected && (
        <span className="shrink-0 text-[11px]" style={{ color: 'var(--accent)' }}>
          ✓
        </span>
      )}
    </div>
  )
}
