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
  /** The llama.cpp KiChat installs and runs itself. */
  export type RuntimeSettings = Plain<Gen.config.RuntimeSettings>
  export type ManagedModel = Plain<Gen.config.ManagedModel>
  /** The picture server generate_image draws through. */
  export type ImageSettings = Plain<Gen.config.ImageSettings>
  /** A named, saved picture configuration: what the image section's dropdown applies. */
  export type ImageProfile = Plain<Gen.config.ImageProfile>
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

export namespace runtime {
  export type ModelFolder = Plain<Gen.runtime.ModelFolder>
  export type ModelFile = Plain<Gen.runtime.ModelFile>
  export type ModelFolderScan = Plain<Gen.runtime.ModelFolderScan>
}

export namespace main {
  export type AppInfo = Plain<Gen.main.AppInfo>
  export type ChatState = Plain<Gen.main.ChatState>
  export type SearchStatus = Plain<Gen.main.SearchStatus>
  export type SecretStatus = Plain<Gen.main.SecretStatus>
  export type PromptVariable = Plain<Gen.main.PromptVariable>
  export type CompactRequest = Plain<Gen.main.CompactRequest>
  export type AttachmentInput = Plain<Gen.main.AttachmentInput>
  export type RuntimeStatus = Plain<Gen.main.RuntimeStatus>
  export type ManagedStatus = Plain<Gen.main.ManagedStatus>
  /** The last look at llama.cpp's releases, carried inside RuntimeStatus. */
  export type RuntimeUpdate = Plain<Gen.main.RuntimeUpdate>
  /** One llama.cpp build on disk, as the cleanup list reads it. */
  export type InstalledBuild = Plain<Gen.main.InstalledBuild>
  /** A model availability sweep, as the picker follows it. */
  export type ModelCheckStatus = Plain<Gen.main.ModelCheckStatus>
  /** The picture server's reachability and what it offers. */
  export type ImageStatus = Plain<Gen.main.ImageStatus>
  /** The managed sd-server's process row, inside ImageStatus. */
  export type ImageServerStatus = Plain<Gen.main.ImageServerStatus>
  /** How much of KiChat's own storage is pictures it drew, and what a clear would free. */
  export type ImageCache = Plain<Gen.main.ImageCache>
  /** What clearing the unsaved pictures removed. */
  export type ImageCacheClear = Plain<Gen.main.ImageCacheClear>
}

export namespace research {
  export type SearchResult = Plain<Gen.research.SearchResult>
  export type SearchOutcome = Plain<Gen.research.SearchOutcome>
  export type ScrapeResult = Plain<Gen.research.ScrapeResult>
}

export namespace store {
  export type Attachment = Plain<Gen.store.Attachment>
  /** What a picture this app drew was made with, so it can be drawn again. */
  export type GenerationRecipe = Plain<Gen.store.GenerationRecipe>
  export type Chat = Plain<Gen.store.Chat>
  export type Chunk = Plain<Gen.store.Chunk>
  export type Compaction = Plain<Gen.store.Compaction>
  export type Document = Plain<Gen.store.Document>
  export type Message = Plain<Gen.store.Message>
  export type ToolCallRecord = Plain<Gen.store.ToolCallRecord>
}

/**
 * The bound backend API. Raw bindings are used as-is, except for the calls that take
 * a whole settings, chat or managed-model object: those are converted back into the
 * generated class instances, whose `convertValues` helper does the nested conversion
 * on the way in.
 */
export const App = {
  ...WailsApp,
  SaveSettings: (settings: config.Settings) =>
    WailsApp.SaveSettings(Gen.config.Settings.createFrom(settings)),
  UpdateChat: (c: store.Chat) => WailsApp.UpdateChat(Gen.store.Chat.createFrom(c)),
  StartManagedModelWith: (backendID: string, mm: config.ManagedModel) =>
    WailsApp.StartManagedModelWith(backendID, Gen.config.ManagedModel.createFrom(mm)),
}

/**
 * What a managed model entry starts as, for a backend that has none yet.
 *
 * One function rather than a literal in each place, because the two places have to
 * agree: the Settings panel shows these values for an entry that does not exist, and
 * the chat's picker sends them when it starts a local model for the first time. Two
 * copies would drift, and the drift would show up as a model that runs with one
 * context size and reports another the moment the panel is opened.
 */
export function defaultManagedModel(): config.ManagedModel {
  return {
    enabled: false,
    modelPath: '',
    // The projector the app pairs on its own: the empty string means "the one beside the
    // model", and a path pins it. It is here rather than only in Settings because a first
    // start from the chat picker sends this same entry.
    mmproj: '',
    ctxSize: 8192,
    flashAttention: 'auto',
    cacheTypeK: 'f16',
    cacheTypeV: 'f16',
    offload: 'auto',
    extraArgs: '',
  }
}

/* --------------------------------------------------------------------------
 * Event names, mirrored from the Go constants in internal/events. That package
 * exists because these pairs used to drift: a misspelled name silently leaves a
 * listener that never fires. The Go test in that package now reads this map and
 * fails when the two disagree.
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
  // Not a `chat:` name, because a draw is not something a chat engine does: it is the
  // picture backend talking about work it is doing right now, and the row it lands on can
  // be a picture that does not exist yet.
  imageProgress: 'image:progress',
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
 * One report from a picture being drawn.
 *
 * The key is `drawId`, and there are two things a draw can be registered under: the
 * attachment a redraw works on, and the assistant message a draw the model asked for is
 * rendered on (its tool call lives there, and there is no picture to hang anything under
 * until the draw is over). It is one event for both because nothing downstream needs to know
 * which kind it is — the row is found by the id either way.
 *
 * `preview` is a complete `data:` URL rather than bytes or a path: a path would be blocked by
 * the same origin rule that stops any other local file from being read in the webview, and
 * the bytes are already in hand on the Go side — the router would be a request per step for
 * something the emitter has just read.
 */
export interface ImageProgressPayload {
  drawId: string
  chatId: string
  step: number
  steps: number
  /** Which sampling pass this step belongs to, 1-based. A hires draw restarts the counter. */
  pass: number
  /** How many passes the whole draw runs: 1 for an ordinary draw, 2 for a hires fix. */
  passes: number
  preview?: string
  /** True for the report that ends a draw, which exists so a progress row cannot outlive it. */
  done?: boolean
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

/** A file size in the unit a model list needs: gigabytes for a GGUF, megabytes
 * below that. Decimal units rather than binary, because that is what LM Studio
 * and every model card report, so the same file does not appear as 4.37 GB in
 * one place and 4.07 GB in the other. */
export function formatBytes(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n) || n <= 0) return '–'
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)} GB`
  return `${Math.round(n / 1e6)} MB`
}

/**
 * The name a model is known by, without the path it happens to live at.
 *
 * A local model's id *is* its path: the managed llama.cpp server answers with the
 * GGUF file path unless it was started with `--alias`, and LM Studio keys its files
 * as `publisher/model/file.gguf`. A conversation is about which model answered, not
 * where the weights are stored, so the chat shows this name and keeps the id for
 * the tooltip and for the metrics detail where a path is what a user is after. An id
 * that is already a name — Ollama's `llama3:8b`, an alias, a hosted model — comes
 * back unchanged.
 */
export function modelLabel(id: string | undefined | null): string {
  if (!id) return ''
  const name = id.split(/[\\/]/).filter(Boolean).pop() ?? id
  // The extension is packaging, not identity: `Qwen3-8B-Q4_K_M.gguf` is the model.
  return name.replace(/\.(gguf|safetensors|bin|ggml|onnx|mlx)$/i, '') || name
}

/**
 * The folder a path-shaped model id sits in, empty for an id that is only a name.
 *
 * Used only to tell two models with the same file name apart — publishers ship the
 * same quantization under the same name — so a path does not appear in front of
 * every model that is not ambiguous.
 */
export function modelFolder(id: string | undefined | null): string {
  if (!id) return ''
  const parts = id.split(/[\\/]/).filter(Boolean)
  return parts.length > 1 ? parts[parts.length - 2] : ''
}

/**
 * The last segment of a path, which is what names a folder on its own: a chip or a row
 * reading "Chosen folder" says nothing about which folder it is, and the full path is in
 * the tooltip beside it.
 */
export function folderName(path: string | undefined | null): string {
  if (!path) return ''
  const segments = path.replace(/[/\\]+$/, '').split(/[/\\]/)
  return segments[segments.length - 1] || path
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
