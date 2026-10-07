import * as WailsApp from '../../wailsjs/go/main/App'
import { EventsOn } from '../../wailsjs/runtime/runtime'
import * as Gen from '../../wailsjs/go/models'
// Type-only, and i18n.ts imports nothing itself, so this cannot make a cycle.
import type { Lang } from './i18n'

/**
 * Wails generates a class per Go struct, and gives every class that contains a
 * nested struct a `convertValues` helper method. That makes the generated types
 * awkward to use in the UI: a spread such as `{ ...state.chatState }` or a
 * hand-built budget object would have to fabricate that method.
 *
 * The UI therefore works with plain structural twins. Stripping the helper
 * recursively keeps every field — including nested structs and arrays — usable
 * as a literal, while generated values stay assignable to the twins, because a
 * class instance is allowed to carry extra members.
 */
type Plain<T> = 0 extends 1 & T
  ? T
  : T extends readonly unknown[]
    ? { [K in keyof T]: Plain<T[K]> }
    : T extends object
      ? { [K in keyof T as K extends 'convertValues' ? never : K]: Plain<T[K]> }
      : T

export namespace chat {
  export type ContextSegment = Plain<Gen.chat.ContextSegment>
  export type ContextBudget = Plain<Gen.chat.ContextBudget>
}

export namespace config {
  export type SearchSettings = Plain<Gen.config.SearchSettings>
  export type CompactSettings = Plain<Gen.config.CompactSettings>
  export type SamplerSettings = Plain<Gen.config.SamplerSettings>
  export type PromptPreset = Plain<Gen.config.PromptPreset>
  export type Settings = Plain<Gen.config.Settings>
}

export namespace llm {
  export type BackendConfig = Plain<Gen.llm.BackendConfig>
  export type HealthStatus = Plain<Gen.llm.HealthStatus>
  export type ModelInfo = Plain<Gen.llm.ModelInfo>
  export type ModelOverride = Plain<Gen.llm.ModelOverride>

  /**
   * Token accounting for one request. These two types are not in the generated
   * bindings, because no bound method mentions them directly — they travel
   * inside the `chat:done` event payload, so they are declared here.
   */
  export interface Usage {
    promptTokens: number
    completionTokens: number
    totalTokens: number
    cachedPromptTokens?: number
  }

  /** Speed measurements for one generation. `source` is "estimated" when KiChat
   * timed the stream itself because the backend reported nothing. */
  export interface GenerationStats {
    tokensPerSecond: number
    ttftMs: number
    generationMs: number
    totalMs?: number
    loadMs?: number
    stopReason?: string
    source?: string
  }
}

export namespace main {
  export type ChatState = Plain<Gen.main.ChatState>
  export type SearchStatus = Plain<Gen.main.SearchStatus>
  export type SecretStatus = Plain<Gen.main.SecretStatus>
  export type PromptVariable = Plain<Gen.main.PromptVariable>
  export type CompactRequest = Plain<Gen.main.CompactRequest>
  export type AttachmentInput = Plain<Gen.main.AttachmentInput>
}

export namespace research {
  export type SearchResult = Plain<Gen.research.SearchResult>
  export type SearchOutcome = Plain<Gen.research.SearchOutcome>
  export type ScrapeResult = Plain<Gen.research.ScrapeResult>
}

export namespace store {
  export type Attachment = Plain<Gen.store.Attachment>
  export type Chat = Plain<Gen.store.Chat>
  export type Chunk = Plain<Gen.store.Chunk>
  export type Compaction = Plain<Gen.store.Compaction>
  export type Document = Plain<Gen.store.Document>
  export type Message = Plain<Gen.store.Message>
  export type ToolCallRecord = Plain<Gen.store.ToolCallRecord>
}

/**
 * The bound backend API. Raw bindings are used as-is, except for the two calls
 * that take a whole settings or chat object: those are converted back into the
 * generated class instances, whose `convertValues` helper does the nested
 * conversion on the way in.
 */
export const App = {
  ...WailsApp,
  SaveSettings: (settings: config.Settings) =>
    WailsApp.SaveSettings(Gen.config.Settings.createFrom(settings)),
  UpdateChat: (c: store.Chat) => WailsApp.UpdateChat(Gen.store.Chat.createFrom(c)),
}

/* --------------------------------------------------------------------------
 * Event names, mirrored from the Go constants in app.go.
 * ------------------------------------------------------------------------ */
export const EV = {
  delta: 'chat:delta',
  reasoning: 'chat:reasoning',
  start: 'chat:start',
  message: 'chat:message',
  tool: 'chat:tool',
  status: 'chat:status',
  done: 'chat:done',
  error: 'chat:error',
  warning: 'chat:warning',
  budget: 'chat:budget',
  compaction: 'chat:compaction',
  title: 'chat:title',
  chatsChanged: 'chats:changed',
  attachments: 'chat:attachments',
} as const

export interface DeltaPayload {
  chatId: string
  messageId: string
  text: string
}
export interface StartPayload {
  chatId: string
  messageId: string
  backend: string
  model: string
  iteration?: number
}
export interface ToolPayload {
  chatId: string
  toolCall: store.ToolCallRecord
}
export interface StatusPayload {
  chatId: string
  phase: string
}
export interface DonePayload {
  chatId: string
  messageId: string
  lastMessageId: string
  status: string
  stats?: llm.GenerationStats
  usage?: llm.Usage
}
export interface ErrorPayload {
  chatId: string
  messageId?: string
  error: string
}
export interface BudgetPayload {
  chatId: string
  budget: chat.ContextBudget
}
export interface CompactionPayload {
  chatId: string
  compaction: store.Compaction
}

/**
 * Subscribe to a backend event. Returns an unsubscribe function so React effect
 * cleanup cannot leak listeners across chat switches.
 */
export function on<T>(event: string, handler: (payload: T) => void): () => void {
  const off = EventsOn(event, (payload: T) => handler(payload))
  return typeof off === 'function' ? off : () => {}
}

/* --------------------------------------------------------------------------
 * Formatters
 * ------------------------------------------------------------------------ */

export function formatTokens(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '–'
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 10_000) return `${Math.round(n / 1000)}k`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`
  return String(n)
}

export function formatNumber(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '–'
  return n.toLocaleString()
}

export function formatSpeed(tokensPerSecond: number | undefined | null): string {
  if (!tokensPerSecond || tokensPerSecond <= 0) return '–'
  if (tokensPerSecond >= 100) return `${Math.round(tokensPerSecond)}`
  return tokensPerSecond.toFixed(1)
}

export function formatDuration(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || ms <= 0) return '–'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`
}

/** Format a percentage defensively: a NaN from a missing limit must not render
 * as "NaN%". */
export function formatPercentSafe(percent: number | undefined | null, digits = 0): string {
  if (percent === undefined || percent === null || Number.isNaN(percent) || !Number.isFinite(percent)) {
    return '–'
  }
  return `${percent.toFixed(digits)}%`
}

/** Colour tone for a context-usage percentage. */
export function usageTone(percent: number | undefined | null): 'ok' | 'warn' | 'danger' {
  const p = percent ?? 0
  if (p >= 90) return 'danger'
  if (p >= 70) return 'warn'
  return 'ok'
}

export function toneVar(tone: 'ok' | 'warn' | 'danger'): string {
  return `var(--${tone})`
}

export function timeAgo(value: unknown, lang: Lang): string {
  const date = toDate(value)
  if (!date) return ''
  const seconds = Math.floor((Date.now() - date.getTime()) / 1000)
  if (seconds < 60) return lang === 'de' ? 'gerade eben' : lang === 'zh' ? '刚刚' : 'just now'
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return lang === 'de' ? `${minutes} Min.` : lang === 'zh' ? `${minutes} 分钟前` : `${minutes} min`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return lang === 'de' ? `${hours} Std.` : lang === 'zh' ? `${hours} 小时前` : `${hours} h`
  const days = Math.floor(hours / 24)
  if (days < 30) return lang === 'de' ? `${days} T.` : lang === 'zh' ? `${days} 天前` : `${days} d`
  return date.toLocaleDateString(lang === 'de' ? 'de-DE' : lang === 'zh' ? 'zh-CN' : 'en-GB')
}

export function toDate(value: unknown): Date | null {
  if (!value) return null
  if (value instanceof Date) return value
  if (typeof value === 'string' || typeof value === 'number') {
    const d = new Date(value)
    return Number.isNaN(d.getTime()) ? null : d
  }
  return null
}

export function hostOf(url: string): string {
  try {
    return new URL(url).hostname.replace(/^www\./, '')
  } catch {
    return url
  }
}

/** Escape text before it is embedded in a highlight pattern. */
export function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}
