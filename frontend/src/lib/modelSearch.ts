import { modelFolder, modelLabel, type llm } from './api'

/*
 * The search behind every model picker.
 *
 * A catalogue is not a list of tidy slugs: a vendor ships `Llama-3.3-70B-Instruct`,
 * a local server answers with `/home/me/models/Qwen3-8B-Q4_K_M.gguf`, and the same
 * model is reachable under an alias. Typing what a person remembers — "llama 3.3
 * 70b", "qwen3 8b q4" — has to find those, so separators are erased before a
 * comparison and every whitespace-separated term must appear somewhere. Raw
 * substring matching, which the chat switcher used to do, fails on every one of
 * those examples.
 */

/** Lowercase, without accents, with every run of separators folded to one space. */
export function normalizeModelText(value: string): string {
  return value
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, ' ')
    .trim()
}

export interface ModelSearchEntry {
  model: llm.ModelInfo
  /** The name without the path it happens to live at. */
  label: string
  labelNorm: string
  /** The label with no separators, so a one-word query can reach `Qwen-2.5`. */
  labelNormCompact: string
  backendNorm: string
  publisherNorm: string
  /** Every searchable field, normalised, separators already collapsed to spaces. */
  haystack: string
  /** The haystack with no separators at all, so `qwen25` finds `Qwen-2.5`. */
  compact: string
}

export interface ModelSearchIndex {
  /** The whole catalogue in browsing order: by backend, loaded models first. */
  entries: ModelSearchEntry[]
  groups: [string, ModelSearchEntry[]][]
}

function entryFor(model: llm.ModelInfo): ModelSearchEntry {
  const label = modelLabel(model.id)
  const labelNorm = normalizeModelText(label)
  const backendNorm = normalizeModelText(model.backend)
  const publisherNorm = normalizeModelText(model.publisher ?? '')
  const haystack = normalizeModelText(
    [
      label,
      model.id,
      model.backend,
      model.publisher ?? '',
      model.arch ?? '',
      model.quantization ?? '',
      model.parameterCount ?? '',
    ].join(' '),
  )
  return {
    model,
    label,
    labelNorm,
    labelNormCompact: labelNorm.replace(/ /g, ''),
    backendNorm,
    publisherNorm,
    haystack,
    compact: haystack.replace(/ /g, ''),
  }
}

/**
 * Builds the indexes once per catalogue.
 *
 * The catalogue changes only when the backend listing is refreshed, while a query
 * changes on every keystroke: normalising a few hundred ids per keystroke would be
 * work that never had to be repeated, so this is the only place it happens.
 */
export function buildModelSearchIndex(models: llm.ModelInfo[]): ModelSearchIndex {
  const entries = models.map(entryFor)
  entries.sort((a, b) => {
    if (a.model.backend !== b.model.backend) return a.model.backend.localeCompare(b.model.backend)
    // Loaded models first inside a backend: they are the ones a switch can make
    // without waiting for a load, which is the only ordering fact the catalogue has.
    if (a.model.state !== b.model.state) return a.model.state === 'loaded' ? -1 : 1
    return a.labelNorm.localeCompare(b.labelNorm) || a.model.id.localeCompare(b.model.id)
  })

  const groups = new Map<string, ModelSearchEntry[]>()
  for (const entry of entries) {
    const list = groups.get(entry.model.backend) ?? []
    list.push(entry)
    groups.set(entry.model.backend, list)
  }
  return { entries, groups: [...groups.entries()] }
}

/**
 * How often a model name repeats, per backend, and what tells the copies apart.
 *
 * Two publishers ship the same quantization under the same file name, so a bare
 * name is ambiguous exactly where it matters. The publisher is short and is what a
 * user recognises; a model without one (a GGUF from the managed runtime, keyed by
 * its path) falls back to the folder it sits in. Measured in the harness: the folder
 * is the wrong segment when both publishers use the same folder name, which is why
 * the publisher is tried first.
 */
export function modelQualifiers(models: llm.ModelInfo[]): Map<string, string> {
  const counts = new Map<string, number>()
  for (const model of models) {
    const key = `${model.backend}/${modelLabel(model.id)}`
    counts.set(key, (counts.get(key) ?? 0) + 1)
  }
  const qualifiers = new Map<string, string>()
  for (const model of models) {
    const repeated = (counts.get(`${model.backend}/${modelLabel(model.id)}`) ?? 0) > 1
    qualifiers.set(`${model.backend}/${model.id}`, repeated ? model.publisher || modelFolder(model.id) : '')
  }
  return qualifiers
}

function score(entry: ModelSearchEntry, terms: string[], queryNorm: string, queryCompact: string): number {
  // Every term has to appear, either as typed or once separators are erased. The
  // second half is what makes `qwen25` reach `Qwen-2.5` while the rest of a mixed
  // query — `qwen25 70b q8` — still has to match the ordinary way; testing only the
  // whole query against the compact form fails as soon as one term needs the other
  // form.
  const matchesTerm = (term: string) => entry.haystack.includes(term) || entry.compact.includes(term)
  if (!terms.every(matchesTerm)) return -1

  let rank = 120
  if (entry.labelNorm === queryNorm) rank = 1000
  else if (entry.labelNorm.startsWith(queryNorm)) rank = 850
  else if (entry.labelNorm.split(' ').some((word) => word.startsWith(terms[0]))) rank = 700
  else if (entry.labelNorm.includes(queryNorm)) rank = 600
  else if (queryCompact !== '' && entry.compact.includes(queryCompact)) rank = 450
  else if (terms.every((term) => entry.labelNorm.includes(term) || entry.labelNormCompact.includes(term))) rank = 380
  else if (entry.backendNorm.startsWith(terms[0]) || entry.publisherNorm.startsWith(terms[0])) rank = 240

  // A tie between two names is broken towards the model that can answer now.
  return rank + (entry.model.state === 'loaded' ? 20 : 0)
}

/** The ranked matches. An empty query is the browsing order, not an empty list. */
export function searchModels(index: ModelSearchIndex, query: string): ModelSearchEntry[] {
  const queryNorm = normalizeModelText(query)
  if (!queryNorm) return index.entries
  const terms = queryNorm.split(' ')
  const queryCompact = queryNorm.replace(/ /g, '')
  return index.entries
    .map((entry) => ({ entry, rank: score(entry, terms, queryNorm, queryCompact) }))
    .filter((scored) => scored.rank >= 0)
    .sort(
      (a, b) =>
        b.rank - a.rank ||
        a.entry.model.backend.localeCompare(b.entry.model.backend) ||
        a.entry.labelNorm.localeCompare(b.entry.labelNorm) ||
        a.entry.model.id.localeCompare(b.entry.model.id),
    )
    .map((scored) => scored.entry)
}

/** The matches in the order they should be shown: ranked, then grouped by backend. */
export function groupEntries(entries: ModelSearchEntry[]): [string, ModelSearchEntry[]][] {
  const groups = new Map<string, ModelSearchEntry[]>()
  for (const entry of entries) {
    const list = groups.get(entry.model.backend) ?? []
    list.push(entry)
    groups.set(entry.model.backend, list)
  }
  return [...groups.entries()]
}

/**
 * Where the typed terms appear in the original text, for highlighting.
 *
 * Searched on the raw lowercase text rather than the normalised one, because a
 * range has to be usable on the string that is rendered: normalisation changes
 * both length and indices. A term that only matches through the separator-stripped
 * form is simply not highlighted — the row is still shown, which is the point.
 */
export function matchRanges(text: string, query: string): [number, number][] {
  const terms = query
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
  if (terms.length === 0) return []
  const haystack = text.toLowerCase()
  const found: [number, number][] = []
  for (const term of terms) {
    let from = 0
    for (;;) {
      const at = haystack.indexOf(term, from)
      if (at < 0) break
      found.push([at, at + term.length])
      from = at + term.length
    }
  }
  found.sort((a, b) => a[0] - b[0] || a[1] - b[1])
  const merged: [number, number][] = []
  for (const range of found) {
    const last = merged[merged.length - 1]
    if (last && range[0] <= last[1]) last[1] = Math.max(last[1], range[1])
    else merged.push([range[0], range[1]])
  }
  return merged
}
