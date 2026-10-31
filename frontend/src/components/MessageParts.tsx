import { memo, useEffect, useRef, useState } from 'react'
import { App, formatDuration, formatNumber, formatSpeed } from '../lib/api'
import type { store as storeTypes, tools as toolTypes } from '../lib/api'
import { copyText } from '../lib/clipboard'
import type { MessageKey } from '../lib/i18n'
import { useThrottled } from '../lib/stream'
import { errText, useStore } from '../store'

/* -------------------------------------------------------------------------
 * Speed and token statistics for one generation.
 * ---------------------------------------------------------------------- */

export function MessageStats({ message }: { message: storeTypes.Message }) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)

  if (message.role !== 'assistant') return null
  if (!message.tokensPerSecond && !message.completionTokens && !message.promptTokens) return null

  // Being explicit about provenance matters: a number we timed ourselves is not
  // the same as one the backend reported.
  const estimated = message.statsSource === 'estimated'
  // How much of this request the backend served from its own prompt cache. Absent
  // means the backend reported nothing, which is not the same fact as "nothing was
  // cached" — so the row is left out rather than shown as zero.
  const cacheHits = message.cacheHitTokens ?? 0

  return (
    <div className="mt-2 text-[11px]" style={{ color: 'var(--text-faint)' }}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span title={t('metrics.tokensPerSecond')}>
          <strong style={{ color: 'var(--text-dim)' }}>{formatSpeed(message.tokensPerSecond)}</strong>{' '}
          {t('metrics.tokensPerSecond')}
        </span>
        {message.ttftMs > 0 && (
          <span title={t('metrics.ttft')}>
            {t('metrics.ttft')} <strong style={{ color: 'var(--text-dim)' }}>{formatDuration(message.ttftMs)}</strong>
          </span>
        )}
        {message.completionTokens > 0 && (
          <span>
            <strong style={{ color: 'var(--text-dim)' }}>{formatNumber(message.completionTokens)}</strong>{' '}
            {t('metrics.tokens')}
          </span>
        )}
        {message.generationMs > 0 && (
          <span>
            {t('metrics.duration')} <strong style={{ color: 'var(--text-dim)' }}>{formatDuration(message.generationMs)}</strong>
          </span>
        )}
        {estimated && <span className="badge">{t('metrics.estimated')}</span>}
        {message.status === 'interrupted' && (
          <span className="badge badge-warn">{t('chat.interrupted')}</span>
        )}
        <button
          type="button"
          className="underline decoration-dotted"
          onClick={() => setOpen((v) => !v)}
        >
          {t('metrics.details')}
        </button>
      </div>

      {open && (
        <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-0.5 max-w-md">
          <Row label={t('metrics.model')} value={message.model || '–'} />
          <Row label={t('metrics.backend')} value={message.backend || '–'} />
          <Row label={t('metrics.promptTokens')} value={formatNumber(message.promptTokens)} />
          {cacheHits > 0 && (
            <Row
              label={t('metrics.cacheHits')}
              value={t('metrics.cacheHitsValue', {
                tokens: formatNumber(cacheHits),
                percent: message.promptTokens > 0 ? Math.round((cacheHits / message.promptTokens) * 100) : 0,
              })}
            />
          )}
          <Row label={t('metrics.completionTokens')} value={formatNumber(message.completionTokens)} />
          <Row label={t('metrics.stopReason')} value={message.stopReason || '–'} />
          <Row
            label={t('metrics.duration')}
            value={message.generationMs ? formatDuration(message.generationMs) : '–'}
          />
          <Row
            label={t('metrics.tokensPerSecond')}
            value={
              message.tokensPerSecond
                ? `${formatSpeed(message.tokensPerSecond)} ${estimated ? `(${t('metrics.estimated')})` : `(${t('metrics.measured')})`}`
                : '–'
            }
          />
        </dl>
      )}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt style={{ color: 'var(--text-faint)' }}>{label}</dt>
      <dd className="mono truncate" style={{ color: 'var(--text-dim)' }} title={value}>
        {value}
      </dd>
    </>
  )
}

/* -------------------------------------------------------------------------
 * Tool calls
 * ---------------------------------------------------------------------- */

export function ToolCallCard({ call }: { call: storeTypes.ToolCallRecord }) {
  const t = useStore((s) => s.t)
  // The tool's name is the model's, not a key this build knows: see translateDynamic.
  const tDynamic = useStore((s) => s.tDynamic)
  const [open, setOpen] = useState(false)
  // A draw the model asked for is registered under the message this call is rendered on,
  // because that is the only row the view has until the picture exists. Reading it here is
  // what turns a tool call that means "minutes of GPU" into something the user can watch and
  // stop — the two things a tool card could not do for a picture until now.
  const draw = useStore((s) => s.draws[call.messageId])
  // A command's output while it runs. The store drops it when the call's record arrives, so
  // its presence means "this is happening now" — a build that prints nothing for two minutes
  // is otherwise indistinguishable from a card that stopped working.
  const liveOutput = useStore((s) => s.toolOutput[call.id])
  const [stopping, setStopping] = useState(false)

  // A declined change is not a fault, so it is not shown as one: the app worked, the
  // person decided, and a red "Error" at the moment they took control would read as the
  // app breaking. It is toned like a cancellation because it is the same kind of fact —
  // the call was not executed.
  const tone =
    call.status === 'ok'
      ? 'badge-ok'
      : call.status === 'error'
        ? 'badge-danger'
        : call.status === 'cancelled' || call.status === 'denied'
          ? 'badge-warn'
          : 'badge-accent'

  const parsedArgs = prettyJSON(call.argsJson)
  const result = parseResult(call.resultJson)
  // The window's own sentence about the result, when the tool reported one. The text under
  // the expand toggle stays exactly what the model was given — that is the record — while
  // this line is the same fact in the reader's language.
  const summary = parseSummary(call.resultJson)
  // The line a command ran — or, when the app refused it, the line the user has to run
  // themselves. Offered on every terminal call rather than only on a refusal: a command worth
  // refusing is a command worth copying, and a button that appears only once something broke
  // is one nobody knows is there.
  const command = call.name === 'run_command' ? commandArgument(call.argsJson) : ''

  return (
    <div
      className="card my-2 px-3 py-2 text-[12px]"
      style={{ background: 'var(--surface-2)', borderColor: 'var(--border)' }}
    >
      <button
        type="button"
        className="flex w-full items-center gap-2 text-left"
        onClick={() => setOpen((v) => !v)}
      >
        <span className={`badge shrink-0 ${tone}`}>
          {call.status === 'ok'
            ? t('tool.ok')
            : call.status === 'error'
              ? t('tool.error')
              : call.status === 'cancelled'
                ? t('tool.cancelled')
                : call.status === 'denied'
                  ? t('tool.denied')
                  : t('tool.running')}
        </span>
        <strong className="shrink-0 whitespace-nowrap" style={{ color: 'var(--text)' }}>
          {tDynamic(`tool.${call.name}`) !== `tool.${call.name}`
            ? tDynamic(`tool.${call.name}`)
            : call.name}
        </strong>
        {/* The arguments take whatever room is left and ellipsise into it. They used to be an
            empty span for most tools, so nothing here needed to shrink; now that every call
            shows its arguments, a fixed width made the tool's own name wrap in a narrow
            window — the one part of the row that must never move. */}
        <span className="faint mono min-w-0 flex-1 truncate">
          {summariseArgs(call.argsJson)}
        </span>
        {/* What the result cost and what of it the model reads. One number when they
            are the same; two when they are not, because a reader who is told the page
            cost 12,000 tokens and then sees the turn fit has to be told where the rest
            went. A withheld result is neither: the model got a notice rather than a
            prefix, so its size is not 41 tokens of anything and the card says so in
            words. A zero is a record that measured nothing, so it renders nothing. */}
        {call.status === 'ok' && call.sourceTokens > 0 &&
          (call.withheld ? (
            <span className="mono shrink-0" style={{ color: 'var(--warn)' }} title={tokenHint(t, call)}>
              {t('tool.tokensWithheld', { tokens: formatNumber(call.sourceTokens) })}
            </span>
          ) : (
            <span className="faint mono shrink-0" title={tokenHint(t, call)}>
              {call.contextTokens > 0 && call.contextTokens < call.sourceTokens
                ? t('tool.tokensCut', {
                    source: formatNumber(call.sourceTokens),
                    context: formatNumber(call.contextTokens),
                  }) + (call.condensed ? '' : ` (${t('tool.tokensBudgetTag')})`)
                : t('tool.tokensOne', { tokens: formatNumber(call.sourceTokens) })}
            </span>
          ))}
        <span className="ml-auto faint shrink-0 whitespace-nowrap">
          {formatDuration(call.durationMs)}
        </span>
      </button>

      {/* What the call produced, in the window's language. It sits outside the toggle
          because it is the part worth reading without opening anything, and it is a project
          tool's own summary rather than a translation of the model's text — the two halves
          are written separately so neither can drift into describing the other. */}
      {summary && (
        <div className="faint mt-1 truncate text-[11px]" title={result}>
          {tDynamic(summary.code, summaryVars(summary.args))}
        </div>
      )}

      {/* Why a call failed, and the line to take away, without opening anything: an error is
          the one part of a tool's detail a person has to read to act on it. A refusal that sat
          behind the expand toggle — as every tool error did — is a sentence nobody reads, and
          this one is the app telling them which command to run themselves. */}
      {(call.error || command) && (
        <div className="mt-1 flex items-start gap-2 text-[11px]">
          {call.error && (
            <div className="min-w-0 flex-1" style={{ color: 'var(--danger)' }}>
              {errText(call.error)}
            </div>
          )}
          {command && <CopyLine text={command} label={t('tool.copyCommand')} />}
        </div>
      )}

      {liveOutput && (
        <div className="mt-2">
          <div className="faint mb-1">{t('tool.liveOutput')}</div>
          {/* Fixed height, scrolled to the newest line: the text is a bounded tail of the
              output, and a card that grew with it would push the reply off the screen for
              the whole length of a build. */}
          <pre
            className="mono max-h-40 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
            style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
            ref={(el) => {
              if (el) el.scrollTop = el.scrollHeight
            }}
          >
            {liveOutput}
          </pre>
        </div>
      )}
      {draw && (
        <LiveDraw
          preview={draw.preview}
          step={draw.step}
          steps={draw.steps}
          pass={draw.pass}
          passes={draw.passes}
          phase={draw.phase}
          className="mt-2"
        />
      )}
      {draw && (
        <div className="mt-1 flex items-center gap-2">
          <button
            type="button"
            className="btn btn-sm btn-danger"
            disabled={stopping}
            title={t('images.stopHint')}
            onClick={async () => {
              setStopping(true)
              try {
                await App.StopImageDraw(call.messageId)
              } catch (err) {
                useStore.getState().toast('error', errText(err))
              } finally {
                setStopping(false)
              }
            }}
          >
            ⏹ {t('images.stop')}
          </button>
        </div>
      )}

      {/* The turn's Stop and this button are different promises: Stop abandons the answer, so
          a command started with the wrong flags would cost the whole turn. This ends the
          command alone, and the tool reports it to the model as a result — the output so far
          and "the person stopped this" — so the turn carries on with something to work from.
          Offered only while the call is still pending, because that is the only time there is
          something to end. */}
      {call.name === 'run_command' && call.status === 'pending' && (
        <div className="mt-1 flex items-center gap-2">
          <button
            type="button"
            className="btn btn-sm"
            disabled={stopping}
            title={t('tool.stopCommandHint')}
            onClick={async () => {
              setStopping(true)
              try {
                await App.StopToolCommand(call.chatId, call.id)
              } catch (err) {
                useStore.getState().toast('error', errText(err))
              } finally {
                setStopping(false)
              }
            }}
          >
            ⏹ {t('tool.stopCommand')}
          </button>
        </div>
      )}

      {open && (
        <div className="mt-2 space-y-2">
          <div>
            <div className="faint mb-1">{t('tool.arguments')}</div>
            <pre
              className="mono overflow-x-auto rounded p-2 text-[11px]"
              style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
            >
              {parsedArgs}
            </pre>
          </div>
          <div>
            <div className="faint mb-1">{t('tool.result')}</div>
            <pre
              className="mono max-h-72 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
              style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
            >
              {result}
            </pre>
          </div>
        </div>
      )}
    </div>
  )
}

/** The command a `run_command` call carries, empty when its arguments are not readable.
 *
 * A model can send arguments that are not JSON — that is what `decodeLoose` on the Go side
 * exists for — so this is a copy convenience and never a reason to render nothing else.
 */
function commandArgument(argsJSON: string): string {
  try {
    const parsed = JSON.parse(argsJSON) as { command?: unknown }
    return typeof parsed.command === 'string' ? parsed.command.trim() : ''
  } catch {
    return ''
  }
}

/** One line, with a button that puts it on the clipboard.
 *
 * `text` is the line itself, not a rendering of it: what lands in the clipboard has to be
 * runnable when it is pasted, so it is the unedited string the tool was given.
 */
function CopyLine({ text, label }: { text: string; label: string }) {
  const t = useStore((s) => s.t)
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="btn btn-sm shrink-0"
      title={text}
      onClick={() => {
        // A refusal is reported rather than swallowed: this button exists for a line the app
        // would not run, and a click that silently copies nothing is worse than no button.
        void copyText(text).then(
          () => {
            setCopied(true)
            window.setTimeout(() => setCopied(false), 1500)
          },
          () => useStore.getState().toast('error', t('chat.copyFailed')),
        )
      }}
    >
      {copied ? t('chat.copied') : `⧉ ${label}`}
    </button>
  )
}

/** Why a tool result is the size it is, in the words the numbers need.
 *
 * Three causes produce the same pair of sizes and only one of them is a setting the user
 * switched on: the summary model, this answer's tool budget, or neither. The inline line
 * carries the sizes, so the tooltip is where the cause can be named without making every
 * card three lines taller.
 */
function tokenHint(
  t: (key: MessageKey, vars?: Record<string, string | number>) => string,
  call: storeTypes.ToolCallRecord,
): string {
  const source = formatNumber(call.sourceTokens)
  const context = formatNumber(call.contextTokens)
  if (call.withheld) return t('tool.tokensWithheldHint', { tokens: source })
  if (call.condensed) return t('tool.tokensCondensedHint', { source, context })
  if (call.contextTokens > 0 && call.contextTokens < call.sourceTokens) {
    return t('tool.tokensBudgetHint', { source, context })
  }
  return t('tool.tokensHint', { tokens: source })
}

/** The one sentence a running draw is described by, wherever it is shown.
 *
 * A hires draw runs two sampling passes and the step counter restarts at 1 for the
 * second. The pass is therefore part of the sentence rather than a second line: "step 3
 * of 20" twice in a row is what a counter that went backwards looks like, while "pass 2
 * of 2, step 3 of 20" is what busy looks like. An ordinary draw says nothing about
 * passes at all — there is only one.
 */
function drawProgressText(
  t: (key: MessageKey, vars?: Record<string, string | number>) => string,
  draw: { step: number; steps: number; pass: number; passes: number; phase?: string },
): string {
  // A phase comes before any sampling: a caption-trained model spends seconds on its
  // caption, and a row that only said "step 0" there would look stalled.
  if (draw.phase === 'captioning') return t('images.captioning')
  // And one wait has no counter to report at all: the neural upscale's program prints a bar
  // per tile and nothing a client may parse, so the sentence is the whole report — which is
  // why the Stop beside it is the control that matters.
  if (draw.phase === 'upscaling') return t('images.upscaling')
  const pass = draw.pass || 1
  const passes = draw.passes || 1
  if (draw.steps > 0) {
    const vars = {
      step: formatNumber(draw.step),
      steps: formatNumber(draw.steps),
      pass: formatNumber(pass),
      passes: formatNumber(passes),
    }
    return passes > 1 ? t('images.progressPass', vars) : t('images.progress', vars)
  }
  return passes > 1
    ? t('images.progressPassUnknown', { pass: formatNumber(pass), passes: formatNumber(passes) })
    : t('images.progressUnknown')
}

/**
 * A picture that is still being drawn: what it looks like so far, and how far along it is.
 *
 * The preview is a real picture — the backend decodes a thumbnail of the latent as the draw
 * proceeds — and it is the same element in both places a draw can be watched from: under the
 * picture a redraw replaces, and inside the tool card of a draw the model asked for. Which is
 * why it is one component and not two: a draw that looked different depending on who started
 * it would be two features that have to be kept in step.
 *
 * A draw that cannot report its steps still renders: the step counter is simply absent and
 * the thumbnail, when there is one, is what says the machine is working.
 */
function LiveDraw({
  preview,
  step,
  steps,
  pass,
  passes,
  phase,
  className,
}: {
  preview?: string
  step: number
  steps: number
  pass: number
  passes: number
  /** A non-sampling part of the draw (`captioning`), absent for an ordinary step report. */
  phase?: string
  className?: string
}) {
  const t = useStore((s) => s.t)
  return (
    <div className={`flex items-center gap-2 ${className ?? ''}`}>
      {preview ? (
        <img
          src={preview}
          alt={t('images.preview')}
          title={t('images.preview')}
          className="rounded"
          style={{ maxWidth: 160, maxHeight: 160, border: '1px solid var(--border)' }}
        />
      ) : null}
      <span className="faint text-[11px]">{drawProgressText(t, { step, steps, pass, passes, phase })}</span>
    </div>
  )
}

function prettyJSON(raw: string): string {
  if (!raw) return '{}'
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function parseResult(raw: string): string {
  if (!raw) return '–'
  try {
    const parsed = JSON.parse(raw)
    if (parsed && typeof parsed === 'object' && typeof parsed.content === 'string') {
      return parsed.content
    }
    return JSON.stringify(parsed, null, 2)
  } catch {
    return raw
  }
}

/**
 * The window's half of a tool result: a code into the dictionaries and its arguments.
 *
 * A result carries it beside the model's text (`{"content": …, "summary": {"code": …}}`),
 * which is what lets the card say "3 hits in 12 files" in the reader's language while the
 * expand toggle still shows the exact text the turn was handed. A result without one — every
 * research tool — renders its text alone, which is what it did before.
 */
function parseSummary(raw: string): { code: string; args: string[] } | null {
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as { summary?: { code?: unknown; args?: unknown } }
    const summary = parsed?.summary
    if (!summary || typeof summary.code !== 'string' || summary.code === '') return null
    const args = Array.isArray(summary.args) ? summary.args.map((arg) => String(arg)) : []
    return { code: summary.code, args }
  } catch {
    return null
  }
}

/** `{{1}}`, `{{2}}`… in order, as the dictionary substitutes them. */
function summaryVars(args: string[]): Record<string, string> {
  const vars: Record<string, string> = {}
  args.forEach((arg, index) => {
    vars[String(index + 1)] = arg
  })
  return vars
}

/** How much of one argument value the collapsed line keeps. */
const ARG_VALUE_CHARS = 48

/** One-line hint of what a tool was called with, for the collapsed state.
 *
 * Every argument, not a list of the keys somebody thought of: the line used to name five of
 * them, so a `read_file` card looked like a call with no arguments and a reader could not tell
 * a whole-file read from an outline (`outline=true`) without opening the card. The keys are
 * the tool's own and the values the model's, so nothing here needs translating — and the
 * pretty-printed JSON in the body is still the record.
 *
 * A value is flattened and cut, because `write_file` carries an entire file in `content` and
 * `run_command` in `command`: the beginning is enough to recognise the call, and a line that
 * long is not a hint of anything. Nested objects are skipped rather than rendered — no tool
 * takes one today, and `[object Object]` is worse than nothing.
 */
function summariseArgs(raw: string): string {
  if (!raw) return ''
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return flatten(raw, 80)
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return ''
  const entries = Object.entries(parsed as Record<string, unknown>).filter(
    ([, value]) => value !== null && value !== undefined && value !== '' && typeof value !== 'object',
  )
  // Scalars first. `outline=true`, `offset=63`, `timeout_seconds=120` are short, and they are
  // the part a glance is for; the path or the query is the long value the line truncates, and
  // for a project tool the sentence under the card names the file anyway. A plain sort is
  // stable, so each group keeps the order the model wrote.
  entries.sort((a, b) => rankArg(a[1]) - rankArg(b[1]))
  return entries
    .map(([key, value]) => `${key}=${flatten(String(value), ARG_VALUE_CHARS)}`)
    .join(' ')
}

/** Where a value sits in the collapsed line: numbers and booleans before text. */
function rankArg(value: unknown): number {
  return typeof value === 'string' ? 1 : 0
}

/** One line, cut at a length and marked when it was cut. */
function flatten(text: string, max: number): string {
  const flat = text.replace(/\s+/g, ' ').trim()
  return flat.length > max ? `${flat.slice(0, max)}…` : flat
}

/* -------------------------------------------------------------------------
 * Reasoning
 * ---------------------------------------------------------------------- */

export function ReasoningBlock({ text, streaming }: { text: string; streaming: boolean }) {
  const t = useStore((s) => s.t)
  // Thinking arrives as fast as the answer does, and it is plain text: the
  // cost here is laying out a growing block, so it is committed on the same
  // ration and the memo below keeps it from being laid out twice.
  const shown = useThrottled(text, streaming, 120)
  if (!shown.trim()) return null
  return (
    <details className="collapsible mb-2" open={streaming}>
      <summary className="text-[12px] faint">{t('chat.reasoning')}</summary>
      <ReasoningText text={shown} />
    </details>
  )
}

const ReasoningText = memo(function ReasoningText({ text }: { text: string }) {
  return (
    <div
      className="mt-1 rounded p-2 text-[12px] whitespace-pre-wrap"
      style={{ background: 'var(--surface-2)', color: 'var(--text-dim)' }}
    >
      {text}
    </div>
  )
})

/* -------------------------------------------------------------------------
 * Compaction
 * ---------------------------------------------------------------------- */

export function CompactionCard({
  compaction,
  collapsed,
  onToggle,
  undoable,
}: {
  compaction: storeTypes.Compaction
  collapsed: boolean
  onToggle: () => void
  // Only the newest compaction of a chat may be undone. Compactions are
  // cumulative — each one summarises on top of the memory in force when it ran —
  // so undoing an older one would leave a newer summary quoting messages that are
  // back in the live context.
  undoable: boolean
}) {
  const t = useStore((s) => s.t)
  const [open, setOpen] = useState(false)
  const [undoing, setUndoing] = useState(false)
  const saved = Math.max(0, compaction.tokensBefore - compaction.tokensAfter)

  const undo = async () => {
    setUndoing(true)
    try {
      await App.DeleteCompaction(compaction.chatId, compaction.id)
      await useStore.getState().refreshChatState()
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setUndoing(false)
    }
  }

  return (
    <div
      className="my-3 rounded-lg px-3 py-2 text-[12px]"
      style={{
        background: 'color-mix(in srgb, var(--accent) 7%, transparent)',
        border: '1px dashed color-mix(in srgb, var(--accent) 40%, var(--border))',
      }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="badge badge-accent">
          {/* `too_large` is the fold nobody asked for: the backend refused the request
              for its own size and the history had to be folded to answer at all. It
              gets its own word rather than reading as a button somebody pressed. */}
          {compaction.trigger === 'auto'
            ? t('compact.auto')
            : compaction.trigger === 'too_large'
              ? t('compact.tooLarge')
              : t('compact.manual')}
        </span>
        <strong style={{ color: 'var(--text)' }}>{t('compact.title')}</strong>
        <span className="faint">
          #{compaction.fromSeq}–#{compaction.toSeq} · {formatNumber(compaction.tokensBefore)} →{' '}
          {formatNumber(compaction.tokensAfter)} {t('metrics.tokens')} ({formatNumber(saved)}{' '}
          {t('compact.saved')})
        </span>
        <button type="button" className="btn btn-sm btn-ghost ml-auto" onClick={onToggle}>
          {collapsed ? t('compact.originals') : t('compact.hideOriginals')}
        </button>
        <button type="button" className="btn btn-sm btn-ghost" onClick={() => setOpen((v) => !v)}>
          {open ? t('common.close') : t('compact.summary')}
        </button>
        {undoable && (
          <button type="button" className="btn btn-sm btn-ghost" disabled={undoing} onClick={() => void undo()}>
            {t('compact.undo')}
          </button>
        )}
      </div>
      {compaction.focus && (
        <div className="faint mt-1">
          {t('compact.focus')}: {compaction.focus}
        </div>
      )}
      {open && (
        <pre
          className="mt-2 max-h-80 overflow-auto whitespace-pre-wrap rounded p-2 text-[11px]"
          style={{ background: 'var(--code-bg)', border: '1px solid var(--border)' }}
        >
          {compaction.summary}
        </pre>
      )}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Attachment chips
 * ---------------------------------------------------------------------- */

export function AttachmentChips({
  attachments,
  compact = false,
}: {
  attachments: storeTypes.Attachment[]
  compact?: boolean
}) {
  const t = useStore((s) => s.t)
  // Pictures are rendered as pictures by MessageImages, so a chip for one would
  // be the same file listed twice — once as an image and once as a filename.
  const chips = attachments.filter((a) => !isImageAttachment(a))
  if (chips.length === 0) return null
  return (
    <div className="mb-2 flex flex-wrap gap-1.5">
      {chips.map((a) => (
        <span
          key={a.id}
          className="badge"
          title={a.warning || `${formatNumber(a.tokenEstimate)} ${t('files.tokens')}`}
        >
          <span>{fileIcon(a)}</span>
          <span className="truncate" style={{ maxWidth: compact ? 140 : 240 }}>
            {a.filename}
          </span>
          <span className="faint">
            {a.placement === 'indexed' ? t('files.indexed') : t('files.inline')} ·{' '}
            {formatNumber(a.tokenEstimate)}
          </span>
          {a.warning && <span style={{ color: 'var(--warn)' }}>⚠</span>}
        </span>
      ))}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Pictures
 * ---------------------------------------------------------------------- */

/**
 * Data URLs are fetched once per attachment and kept, most recent first.
 *
 * The payload is a whole picture — hundreds of kilobytes for a 1024x1024 PNG —
 * and the message tree re-renders on every delta of a streamed reply. Reading it
 * again each time would push that payload through the bridge for every keystroke,
 * so the promise is cached. The key is the attachment id, which never changes, so
 * an entry cannot go stale; a rejected promise is dropped rather than remembered,
 * because a failure that lasted one startup should not be permanent.
 *
 * It is bounded on purpose. A session that draws pictures keeps every payload it
 * ever showed, and a chat with a dozen 1024x1024 pictures held tens of megabytes
 * of base64 in the webview with the decoded bitmaps on top — memory the user pays
 * for and cannot see. The cap is well above what is on screen at once (the chat
 * view is virtualised, so only a window of messages is mounted), and the oldest
 * entry is the one that has been off screen longest: for a picture the user scrolls
 * back to, re-reading it through the bridge costs one call, which is the cheap half
 * of the trade. The Map iterates in insertion order, so refreshing an entry is a
 * delete-and-set rather than a bookkeeping structure of its own.
 */
const IMAGE_CACHE_LIMIT = 24
const imageCache = new Map<string, Promise<string>>()

function attachmentImage(id: string): Promise<string> {
  const known = imageCache.get(id)
  if (known) {
    // Re-inserted so it is the newest rather than the oldest: the picture the user is
    // looking at now must not be the one evicted by the next one they open.
    imageCache.delete(id)
    imageCache.set(id, known)
    return known
  }
  const entry = App.GetAttachmentImage(id).catch((err) => {
    imageCache.delete(id)
    throw err
  })
  imageCache.set(id, entry)
  while (imageCache.size > IMAGE_CACHE_LIMIT) {
    const oldest = imageCache.keys().next()
    if (oldest.done) break
    imageCache.delete(oldest.value)
  }
  return entry
}

/**
 * True for an attachment whose payload is a picture the webview can paint. The
 * test matches the backend's own: an image is one whose kind or MIME says so,
 * which is what makes a generated picture and an attached photo take the same
 * path through the UI.
 */
export function isImageAttachment(a: storeTypes.Attachment): boolean {
  return a.kind === 'image' || (a.mime ?? '').toLowerCase().startsWith('image/')
}

/**
 * Renders a message's pictures.
 *
 * Nothing here knows whether a picture was generated by a model or dropped into
 * the composer, and that is the point: the backend files both as an image
 * attachment, so both are shown and saved by the same code. Only an uploaded one
 * is also sent back to the model; an image part cannot ride on an assistant turn.
 */
export function MessageImages({ attachments }: { attachments: storeTypes.Attachment[] }) {
  const images = attachments.filter(isImageAttachment)
  if (images.length === 0) return null
  return (
    <div className="mb-2 flex flex-wrap gap-2">
      {images.map((a) => (
        <MessageImage key={a.id} attachment={a} />
      ))}
    </div>
  )
}

/* The bounds the editor's number boxes carry, mirrored from internal/config
 * (MaxImageDimension, MaxImageSteps). There is no bridge for a constant, and the
 * settings screen's own inputs carry the same numbers by hand. They matter more here
 * than there: a stored recipe came from this app, but an edited one comes from a box
 * the user filled in, and a local server attempts a 100000-step request rather than
 * refusing it.
 */
const MAX_IMAGE_DIMENSION = 2048
const MAX_IMAGE_STEPS = 150
const MAX_IMAGE_CFG = 30
// The second-pass bounds, mirrored from internal/images/limits.go. They are applied here
// so a box the user typed into cannot ask for a repaint the backend would silently clamp
// to something else; the backend clamps again, because a request can arrive from anywhere.
const MIN_IMAGE_HIRES_SCALE = 1.25
const MAX_IMAGE_HIRES_SCALE = 2
const MIN_IMAGE_HIRES_DENOISE = 0.2
const MAX_IMAGE_HIRES_DENOISE = 0.6

/** The long sides the two enlargement buttons draw to — 1920 for FHD, 3840 for 4K.
 *
 * The long side rather than a fixed frame, because a picture's aspect ratio is part of
 * what the prompt drew: a 4:3 sketch enlarged to a 16:9 frame would crop or stretch it,
 * and "make this picture bigger" is not "recompose it". So 4K is 3840 px on the long
 * side, which is 3840×2160 for a 16:9 picture and 3840×2880 for a 4:3 one — the label
 * on the button names which.
 *
 * Both are inside the backend's own limits, and that is not a coincidence: a base is
 * capped at 2048 a side (MAX_IMAGE_DIMENSION) and the second pass at 2×, so 1920 is the
 * largest base a 3840 target needs, and 3840 is under the 4096 a picture may end up at.
 * A 4K square would be 3840×3840 and still fit; a 4K 4:3 is 11 megapixels, which is
 * where a 20 GB card stops holding the pass — from there the backend continues on the
 * CPU rather than refusing, which is the trade this button makes.
 */
const FHD_LONG_SIDE = 1920
const UHD_LONG_SIDE = 3840
// The backend's own bound (images.MaxUpscaleLongSide): a resize has no latent grid and
// no 4096 cap, only the memory a 64 MP buffer costs.
const MAX_UPSCALE_LONG_SIDE = 8192
const MAX_UPSCALE_PIXELS = 64_000_000

/** What an enlargement button draws, and the size it will come out at.
 *
 * A second pass is capped at 2× per side (MAX_IMAGE_HIRES_SCALE, from
 * internal/images/limits.go), so a base smaller than half the target cannot reach it —
 * 800×600 needs 2.4× for FHD and 4.8× for 4K. The base is then raised to the smallest
 * size whose 2× lands on the target, keeping the aspect ratio: 800×600 becomes 960×720
 * (1920×1440) or 1920×1440 (3840×2880). That is a different first *pass* rather than a
 * bigger version of the same one, which is why the button names the size it will draw
 * instead of just saying FHD or 4K.
 *
 * Returns null when there is nothing to do or nothing to compute from: a base size the
 * server chose is not a number this can multiply, and a picture already at the target
 * has no larger pass to ask for. */
function enlargedRecipe(
  recipe: storeTypes.GenerationRecipe,
  target: number,
): { recipe: storeTypes.GenerationRecipe; width: number; height: number } | null {
  const width = recipe.width ?? 0
  const height = recipe.height ?? 0
  if (width <= 0 || height <= 0) return null
  const long = Math.max(width, height)
  if (long >= target) return null
  const needed = target / long
  const scale = Math.min(MAX_IMAGE_HIRES_SCALE, needed)
  // Aligned to the 8-pixel latent grid the backend rounds to anyway; the base is only
  // rescaled when the cap forces it, so an ordinary picture keeps the size it was drawn at.
  const align = (value: number) => Math.max(64, Math.round(value / 8) * 8)
  // Above the cap the *base* grows to meet the target — 800×600 becomes 960×720, whose 2×
  // is 1920×1440 — rather than the second pass exceeding the bound it is named for.
  const grow = needed > MAX_IMAGE_HIRES_SCALE ? target / MAX_IMAGE_HIRES_SCALE / long : 1
  const baseWidth = align(width * grow)
  const baseHeight = align(height * grow)
  // What the second pass will actually produce. Rounded up like Request.OutputSize does,
  // so the size on the button is the size the backend will be asked for.
  const alignUp = (value: number) => Math.ceil(value / 8) * 8
  const outWidth = alignUp(baseWidth * scale)
  const outHeight = alignUp(baseHeight * scale)
  return {
    recipe: {
      ...recipe,
      width: baseWidth,
      height: baseHeight,
      hires: true,
      hiresScale: scale,
      hiresDenoise:
        recipe.hiresDenoise && recipe.hiresDenoise >= MIN_IMAGE_HIRES_DENOISE
          ? recipe.hiresDenoise
          : 0.4,
    },
    width: outWidth,
    height: outHeight,
  }
}

type EnlargeTarget = 'fhd' | 'uhd' | 'custom'
type EnlargeMethod = 'lanczos' | 'esrgan' | 'redraw'

type EnlargeDraft = {
  target: EnlargeTarget
  longSide: number
  method: EnlargeMethod
  model: string
  sharpen: number
}

/** Where a long-side target lands for a picture of any aspect ratio.
 *
 * This duplicates images.Fit deliberately: the panel has to show the size before the
 * request is sent, and showing a size the backend would not produce would be worse than
 * the duplication. The bounds are the same two numbers — 8192 px and 64 MP — because a
 * preview that promises more than the backend will write is not a preview.
 */
function fitLongSide(
  width: number,
  height: number,
  longSide: number,
): { width: number; height: number } {
  if (width <= 0 || height <= 0 || longSide <= 0) return { width: 0, height: 0 }
  const long = Math.min(longSide, MAX_UPSCALE_LONG_SIDE)
  const scale = long / Math.max(width, height)
  let w = Math.max(1, Math.round(width * scale))
  let h = Math.max(1, Math.round(height * scale))
  if (width >= height) w = long
  else h = long
  if (w * h > MAX_UPSCALE_PIXELS) {
    const shrink = Math.sqrt(MAX_UPSCALE_PIXELS / (w * h))
    w = Math.max(1, Math.floor(w * shrink))
    h = Math.max(1, Math.floor(h * shrink))
  }
  return { width: w, height: h }
}

/** The recipe fields as the strings a form holds.
 *
 * Numbers live as text rather than as numbers so an emptied box can mean "the server's
 * own" — the backend reads a zero exactly that way. A `0` rendered into the box would
 * look like a value the user chose, and one they then could not clear. */
type RecipeDraft = {
  prompt: string
  negativePrompt: string
  width: string
  height: string
  steps: string
  cfgScale: string
  seed: string
  sampler: string
  model: string
  hires: boolean
  hiresScale: string
  hiresDenoise: string
}

function draftFromRecipe(recipe: storeTypes.GenerationRecipe): RecipeDraft {
  const num = (value: number | undefined) => (value ? String(value) : '')
  return {
    prompt: recipe.prompt ?? '',
    negativePrompt: recipe.negativePrompt ?? '',
    width: num(recipe.width),
    height: num(recipe.height),
    steps: num(recipe.steps),
    cfgScale: num(recipe.cfgScale),
    seed: num(recipe.seed),
    sampler: recipe.sampler ?? '',
    model: recipe.model ?? '',
    hires: recipe.hires ?? false,
    hiresScale: num(recipe.hiresScale),
    hiresDenoise: num(recipe.hiresDenoise),
  }
}

/** The inverse, with the same reading of an empty box the settings screen uses: text
 * that is not a number, or a number that is not positive, is "the server's own". The
 * bounds are applied here so the field's own max is honoured before the request leaves;
 * the backend clamps again, because a request can arrive from anywhere. */
function recipeFromDraft(draft: RecipeDraft): storeTypes.GenerationRecipe {
  const num = (value: string, max?: number) => {
    const n = Math.floor(Number(value))
    if (!Number.isFinite(n) || n <= 0) return 0
    return max === undefined ? n : Math.min(n, max)
  }
  // The guidance scale is the one fractional field: 7.5 is a scale a user may well want,
  // so it is parsed as a float rather than through `num` above, which floors. Zero still
  // means "nobody chose one" and is what an emptied box becomes.
  const scale = (value: string) => {
    const n = Number(value)
    if (!Number.isFinite(n) || n <= 0) return 0
    return Math.min(Math.max(n, 1), MAX_IMAGE_CFG)
  }
  // The second pass keeps zero as "the app's own default" — an emptied box is not a
  // request for the smallest enlargement — and only clamps a number somebody typed.
  const fraction = (value: string, min: number, max: number) => {
    const n = Number(value)
    if (!Number.isFinite(n) || n <= 0) return 0
    return Math.min(Math.max(n, min), max)
  }
  return {
    prompt: draft.prompt.trim(),
    negativePrompt: draft.negativePrompt.trim(),
    width: num(draft.width, MAX_IMAGE_DIMENSION),
    height: num(draft.height, MAX_IMAGE_DIMENSION),
    steps: num(draft.steps, MAX_IMAGE_STEPS),
    cfgScale: scale(draft.cfgScale),
    seed: num(draft.seed),
    sampler: draft.sampler.trim(),
    model: draft.model.trim(),
    hires: draft.hires,
    hiresScale: draft.hires ? fraction(draft.hiresScale, MIN_IMAGE_HIRES_SCALE, MAX_IMAGE_HIRES_SCALE) : 0,
    hiresDenoise: draft.hires
      ? fraction(draft.hiresDenoise, MIN_IMAGE_HIRES_DENOISE, MAX_IMAGE_HIRES_DENOISE)
      : 0,
  }
}

/**
 * The Enlarge form: a target and a method, in that order.
 *
 * It is a panel rather than a menu because the cost of the three methods spans four
 * orders of magnitude — a second, ten seconds, several minutes — and a user should see
 * the size and the cost before the expensive one starts. The presets on the card open
 * this panel with the target already chosen; none of them starts work on its own.
 *
 * The neural row has three states because it has three failure modes that are not the
 * same sentence: the switch is off, the switch is on but no program was found, and the
 * program is there. A disabled radio with "not set up" is what makes the first two
 * fixable; leaving the row out would hide the feature, and enabling it silently would
 * make Lanczos look like a lie.
 */
function EnlargePanel({
  width,
  height,
  recipe,
  status,
  defaultMethod,
  defaultSharpen,
  initialTarget,
  busy,
  onClose,
  onRun,
  onStop,
}: {
  width: number
  height: number
  recipe?: storeTypes.GenerationRecipe
  status: toolTypes.UpscalerStatus | null
  defaultMethod: string
  defaultSharpen: number
  initialTarget: EnlargeTarget
  busy: boolean
  onClose: () => void
  onRun: (draft: EnlargeDraft) => void
  /**
   * Ends the run in flight, and only passed for a method that can act on it: a redraw is a
   * draw the app already knows how to stop, and the neural pass is a program it started.
   * The classical resampler has no cancellation point inside a pass, so it gets no button
   * rather than one that does nothing for a second.
   */
  onStop?: () => void
}) {
  const t = useStore((s) => s.t)
  const [target, setTarget] = useState<EnlargeTarget>(initialTarget)
  const [longSide, setLongSide] = useState(() =>
    String(Math.min(MAX_UPSCALE_LONG_SIDE, Math.max(width, height) * 2)),
  )
  const [method, setMethod] = useState<EnlargeMethod>(() =>
    defaultMethod === 'esrgan' && status?.enabled && status?.available ? 'esrgan' : 'lanczos',
  )
  const [model, setModel] = useState(status?.models?.[0] ?? 'realesrgan-x4plus')
  const [sharpen, setSharpen] = useState(String(defaultSharpen))

  const requested =
    target === 'fhd'
      ? FHD_LONG_SIDE
      : target === 'uhd'
        ? UHD_LONG_SIDE
        : Math.floor(Number(longSide)) || 0
  const out = fitLongSide(width, height, requested)
  const scale = width > 0 && out.width > 0 ? out.width / width : 0
  const neuralReady = Boolean(status?.enabled && status?.available)
  const canRedraw = Boolean(recipe) && target !== 'custom'
  const ready =
    out.width > 0 &&
    !busy &&
    (method !== 'redraw' || canRedraw) &&
    (method !== 'esrgan' || neuralReady)

  const run = () =>
    onRun({
      target,
      longSide: target === 'custom' ? Math.floor(Number(longSide)) || 0 : 0,
      method,
      model,
      sharpen:
        method === 'lanczos'
          ? Math.min(1, Math.max(0, Number(sharpen) || 0))
          : 0,
    })

  return (
    <div
      className="mt-1 flex flex-col gap-2 rounded p-2 text-[11px]"
      style={{ border: '1px solid var(--border)', background: 'var(--code-bg)', maxWidth: 460 }}
    >
      <span className="faint">{t('images.enlargeTitle')}</span>
      <div>
        <span className="faint">{t('images.enlargeTarget')}</span>
        <div className="mt-1 flex flex-wrap items-center gap-1">
          {(['fhd', 'uhd', 'custom'] as const).map((value) => (
            <button
              key={value}
              type="button"
              className={target === value ? 'btn btn-sm btn-primary' : 'btn btn-sm'}
              aria-pressed={target === value}
              onClick={() => setTarget(value)}
            >
              {value === 'fhd'
                ? t('images.fhd')
                : value === 'uhd'
                  ? t('images.uhd')
                  : t('images.enlargeCustom')}
            </button>
          ))}
          {target === 'custom' ? (
            <input
              className="input mono w-24"
              type="number"
              min={64}
              max={MAX_UPSCALE_LONG_SIDE}
              step={8}
              value={longSide}
              aria-label={t('images.enlargeCustom')}
              onChange={(e) => setLongSide(e.target.value)}
            />
          ) : null}
        </div>
        <div className="faint mt-1">
          {t('images.enlargeResult', {
            width: formatNumber(out.width),
            height: formatNumber(out.height),
          })}
          {scale > 8 ? ` — ${t('images.enlargeWarning')}` : ''}
        </div>
      </div>

      <div className="flex flex-col gap-1">
        <span className="faint">{t('images.enlargeMethod')}</span>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            checked={method === 'lanczos'}
            onChange={() => setMethod('lanczos')}
          />
          <span>{t('images.enlargeResample')}</span>
          <span className="faint flex-1">{t('images.enlargeResampleHint')}</span>
        </label>
        <label className="flex items-center gap-2" title={status?.error ?? ''}>
          <input
            type="radio"
            disabled={!neuralReady}
            checked={method === 'esrgan'}
            onChange={() => setMethod('esrgan')}
          />
          <span>{t('images.enlargeNeural')}</span>
          <span className="faint flex-1">
            {neuralReady
              ? t('images.enlargeNeuralHint')
              : status?.enabled
                ? t('images.enlargeNeuralMissing')
                : t('images.enlargeNeuralOff')}
          </span>
        </label>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            disabled={!canRedraw}
            checked={method === 'redraw'}
            onChange={() => setMethod('redraw')}
          />
          <span>{t('images.enlargeRedraw')}</span>
          <span className="faint flex-1">
            {!recipe
              ? t('images.enlargeRedrawNeedsRecipe')
              : target === 'custom'
                ? t('images.enlargeRedrawCustom')
                : t('images.enlargeRedrawHint')}
          </span>
        </label>
      </div>

      {method === 'lanczos' ? (
        <label className="flex items-center gap-2">
          {t('images.enlargeSharpen')}
          <input
            className="input mono w-20"
            type="number"
            min={0}
            max={1}
            step={0.05}
            value={sharpen}
            onChange={(e) => setSharpen(e.target.value)}
          />
        </label>
      ) : null}
      {method === 'esrgan' ? (
        <label className="flex items-center gap-2">
          {t('images.enlargeModel')}
          <select className="input" value={model} onChange={(e) => setModel(e.target.value)}>
            {(status?.models?.length ? status.models : ['realesrgan-x4plus']).map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
      ) : null}

      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          className="btn btn-sm btn-primary"
          disabled={!ready}
          onClick={run}
        >
          {busy
            ? t('images.enlarging')
            : t('images.enlargeTo', {
                width: formatNumber(out.width),
                height: formatNumber(out.height),
              })}
        </button>
        {busy && onStop ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            title={t('images.enlargeStopHint')}
            onClick={onStop}
          >
            ⏹ {t('images.stop')}
          </button>
        ) : null}
        <button type="button" className="btn btn-sm btn-ghost" onClick={onClose}>
          {t('images.editClose')}
        </button>
      </div>
    </div>
  )
}

function MessageImage({ attachment }: { attachment: storeTypes.Attachment }) {
  const t = useStore((s) => s.t)
  const [src, setSrc] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // Click to see the whole picture. A thumbnail keeps a long chat readable, and
  // the enlarged view is what makes "is this the one I want?" answerable before
  // the file is saved.
  const [expanded, setExpanded] = useState(false)
  // Which draw is in flight, if any. Every control here spends GPU seconds, so they are
  // mutually disabled while one runs — otherwise a second click draws a second picture.
  const [busy, setBusy] = useState<'same' | 'new' | 'edit' | null>(null)
  // The editor is closed until the user opens it: a chat full of pictures would
  // otherwise be a chat full of forms.
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState<RecipeDraft | null>(null)
  // The Enlarge panel and the work it may be doing. It is separate from `busy` because
  // the two are different calls — one draws from a recipe, the other resizes the bytes
  // that are already here — and they share only the rule that neither may run twice.
  const [enlarge, setEnlarge] = useState<EnlargeTarget | null>(null)
  const [enlarging, setEnlarging] = useState(false)
  // Which method the run in flight is using, because only two of the three can be stopped:
  // a redraw is a draw the backend already knows how to cancel, and the neural pass is a
  // program this app started. The resampler has no cancellation point inside a pass, so it
  // is offered no Stop rather than a button that does nothing.
  const [enlargingMethod, setEnlargingMethod] = useState<EnlargeMethod | null>(null)
  const [upscaler, setUpscaler] = useState<toolTypes.UpscalerStatus | null>(null)
  // The stored picture's own size, read from the decoded image rather than from the
  // recipe: an uploaded photo has no recipe, and a recipe's numbers describe the draw
  // rather than a picture that may have been replaced since.
  const [pixels, setPixels] = useState<{ width: number; height: number } | null>(null)
  // Whether a draw in flight is one the user has asked to stop. The rejection that
  // follows is that cancellation rather than a failure, and this is what tells the two
  // apart: a red error toast for the user's own click reads as a crash, and the picture
  // list is unchanged either way. It is a ref rather than state because the draw's own
  // promise, not a re-render, is what reads it.
  const stopping = useRef(false)
  // And while the stop itself is in flight. The draw's own promise rejects the moment the
  // request is cancelled, but the server is interrupted a moment later — a *global*
  // interrupt of whatever it is drawing — so re-enabling the controls as soon as the draw
  // unwinds would let a second click start a picture the previous stop could then kill. It
  // is a window of milliseconds and it costs four lines to close.
  const [stoppingUi, setStoppingUi] = useState(false)
  // What the backend knows about a draw running under *this* picture. A redraw is registered
  // under the attachment it will replace, so this is the row's own draw and nothing else's.
  const live = useStore((s) => s.draws[attachment.id])  // Only a picture KiChat drew carries a recipe. An uploaded photo has none, and
  // offering to redraw one would offer to draw something unrelated to it.
  const recipe = attachment.generation
  const imageSettings = useStore((s) => s.settings?.images)
  // The presets are read from the picture's real size, not from a recipe, so they are
  // offered for an uploaded photo too — which is the whole point of the fast path.
  const longSide = pixels ? Math.max(pixels.width, pixels.height) : 0
  const fhdSize = pixels ? fitLongSide(pixels.width, pixels.height, FHD_LONG_SIDE) : null
  const uhdSize = pixels ? fitLongSide(pixels.width, pixels.height, UHD_LONG_SIDE) : null

  useEffect(() => {
    let cancelled = false
    setError(null)
    attachmentImage(attachment.id)
      .then((value) => {
        if (!cancelled) setSrc(value)
      })
      .catch((err) => {
        if (!cancelled) setError(errText(err))
      })
    return () => {
      cancelled = true
    }    }, [attachment.id])

  // The neural row's three states are read when the panel opens, not on every render.
  // A status call only stats a file, but doing it for every picture in a chat would be
  // a bridge round trip per card.
  useEffect(() => {
    if (!enlarge) return
    let cancelled = false
    App.GetUpscalerStatus()
      .then((value) => {
        if (!cancelled) setUpscaler(value)
      })
      .catch(() => {
        if (!cancelled) setUpscaler(null)
      })
    return () => {
      cancelled = true
    }
  }, [enlarge])

  const save = async () => {
    setSaving(true)
    try {
      const path = await App.SaveAttachmentAs(attachment.id)
      // An empty path is the user closing the dialog. Reporting it as a success
      // or a failure would both be wrong, so it says nothing at all.
      if (path) useStore.getState().toast('info', `${t('images.saved')}: ${path}`)
    } catch (err) {
      useStore.getState().toast('error', errText(err))
    } finally {
      setSaving(false)
    }
  }

  /* Draws from a recipe, and the one place all three controls meet: a repeat passes the
   * stored recipe back, a variation passes it with the seed cleared, and Draw passes what
   * the editor holds. Nothing goes to the model — a recipe is the prompt, the size and the
   * seed, so the picture is drawn from data rather than from a request in words. What was
   * typed is what is drawn: a box left empty means "the server's own" rather than the
   * stored value, so the editor's promise is that it shows the whole recipe.
   *
   * The new picture is filed onto this same message, which is what puts it beside the
   * original; the backend's attachments event is what makes this component see it. */
  // One condition for every control that spends GPU seconds, so a state where some of them
  // are live cannot be reached by editing one of them alone.
  const blocked = busy !== null || stoppingUi || enlarging

  const draw = async (
    next: storeTypes.GenerationRecipe,
    mode: 'same' | 'new' | 'edit',
  ) => {
    setBusy(mode)
    stopping.current = false
    try {
      const drawn = await App.RegenerateImage(attachment.id, next)
      useStore.getState().toast('info', `${t('images.redrawn')}: ${drawn.filename}`)
    } catch (err) {
      if (stopping.current) {
        useStore.getState().toast('info', t('images.drawStopped'))
      } else {
        useStore.getState().toast('error', errText(err))
      }
    } finally {
      setBusy(null)
      stopping.current = false
    }
  }

  /* Ends a draw the user will not wait for. The backend does both halves: it cancels the
   * request, which is what makes this take effect at once, and asks the image server to
   * abandon the work behind it — a 40-step draw on a CPU server is minutes of fan noise
   * for a picture nobody wants any more.
   *
   * `wasDrawing` false means there was nothing to stop, which is a second click or a draw
   * that finished between the render and the click: the flag is cleared so the next
   * rejection is reported as the failure it is, and nothing is toasted for a stop that
   * did not happen. */
  const stop = async () => {
    stopping.current = true
    setStoppingUi(true)
    try {
      const result = await App.StopImageDraw(attachment.id)
      if (!result.wasDrawing) stopping.current = false
    } catch (err) {
      stopping.current = false
      useStore.getState().toast('error', errText(err))
    } finally {
      setStoppingUi(false)
    }
  }

  /* Enlarges this picture by the chosen method. The redraw branch is the existing
   * RegenerateImage call with a recipe whose second pass lands on the target — the
   * expensive path keeps its own code and its own progress reporting. The two fast
   * paths file a derived copy and leave this picture untouched. */
  const runEnlarge = async (next: EnlargeDraft) => {
    setEnlarging(true)
    setEnlargingMethod(next.method)
    stopping.current = false
    try {
      if (next.method === 'redraw') {
        if (!recipe) throw new Error(t('images.enlargeRedrawNeedsRecipe'))
        const target = next.target === 'fhd' ? FHD_LONG_SIDE : UHD_LONG_SIDE
        const plan = enlargedRecipe(recipe, target)
        if (!plan) throw new Error(t('images.enlargeRedrawCustom'))
        const drawn = await App.RegenerateImage(attachment.id, plan.recipe)
        useStore.getState().toast('info', `${t('images.redrawn')}: ${drawn.filename}`)
      } else {
        const made = await App.UpscaleAttachment(attachment.id, {
          target: next.target,
          longSide: next.longSide,
          method: next.method,
          model: next.model,
          sharpen: next.sharpen,
        })
        useStore.getState().toast('info', `${t('images.enlarged')}: ${made.filename}`)
      }
      setEnlarge(null)
    } catch (err) {
      // A stop is the user's own click, not a failure: the wait they asked to end did end,
      // and a red error toast for it reads as a crash. Which sentence it gets depends on
      // what was stopped — a redraw is a draw, and the neural pass is not.
      if (stopping.current) {
        const key = next.method === 'redraw' ? 'images.drawStopped' : 'images.enlargeStopped'
        useStore.getState().toast('info', t(key))
      } else {
        useStore.getState().toast('error', errText(err))
      }
    } finally {
      stopping.current = false
      setEnlarging(false)
      setEnlargingMethod(null)
    }
  }

  /* Opens the editor prefilled with the picture's own recipe. Closing and reopening is
   * also how a half-finished edit is abandoned, which is why there is no reset button. */
  const openEditor = () => {
    if (!recipe) return
    setDraft(draftFromRecipe(recipe))
    setEditing(true)
  }

  if (error) {
    return (
      <span className="badge badge-danger" title={error}>
        {t('images.unreadable')}: {attachment.filename}
      </span>
    )
  }

  if (!src) {
    // A box of the final size rather than a spinner, so the message does not
    // jump when a large picture lands a moment later.
    return (
      <div
        className="flex items-center justify-center rounded"
        style={{
          width: expanded ? 384 : 200,
          height: expanded ? 384 : 200,
          background: 'var(--code-bg)',
          border: '1px solid var(--border)',
        }}
      >
        <span className="faint text-[11px]">{t('images.loading')}</span>
      </div>
    )
  }

  return (
    <figure className="m-0 flex flex-col gap-1">
      <img
        // While a redraw runs, this picture is the one on its way out — the new one replaces
        // it the moment the draw answers. Showing the draw instead of the old picture is what
        // makes a two-minute wait on a CPU server something other than a frozen window.
        src={live?.preview ?? src}
        alt={attachment.filename}
        className="rounded"
        style={{
          maxWidth: expanded ? '100%' : 240,
          maxHeight: expanded ? 560 : 240,
          border: '1px solid var(--border)',
          background: 'var(--code-bg)',
          cursor: 'zoom-in',
        }}
        title={live ? t('images.preview') : t('images.zoom')}
        onLoad={(e) =>
            setPixels({
              width: e.currentTarget.naturalWidth,
              height: e.currentTarget.naturalHeight,
            })
          }
          onClick={() => setExpanded((v) => !v)}
        />
      {live && (
        <span className="faint text-[11px]" title={t('images.preview')}>
          {drawProgressText(t, live)}
        </span>
      )}
      <figcaption className="flex flex-wrap items-center gap-2 text-[11px]">
        <span
          className="faint truncate"
          style={{ maxWidth: 200 }}
          title={recipe?.prompt || attachment.filename}
        >
          {attachment.filename}
        </span>
        {/* The seed is the number that reproduces a picture, and it is what tells two
            redraws of one prompt apart once both are on screen. */}
        {recipe?.seed ? (
          <span className="faint" title={t('images.seed')}>
            s{recipe.seed}
          </span>
        ) : null}
        {/* What makes this picture larger than its base size, without opening the
            editor: a second pass that ran is part of how the picture was made. */}
        {recipe?.hires ? (
          <span className="faint" title={t('settings.images.hiresHint')}>
            {recipe.hiresScale ? `${recipe.hiresScale}×` : t('settings.images.hires')}
          </span>
        ) : null}
        {/* An enlarged copy says what it is: which method and scale, so two copies of
            one picture can be told apart without opening either. */}
        {attachment.derivation ? (
          <span className="faint" title={t('images.derivedHint')}>
            {attachment.derivation.method === 'esrgan' ? 'Real-ESRGAN' : 'Lanczos'}
            {attachment.derivation.scale ? ` ${attachment.derivation.scale.toFixed(2)}×` : ''}
          </span>
        ) : null}
        {/* The time the draw cost, recorded rather than guessed — both passes included,
            which is what makes it a better predictor of a redraw than the step count. */}
        {recipe?.durationMs ? (
          <span className="faint" title={t('metrics.duration')}>
            {formatDuration(recipe.durationMs)}
          </span>
        ) : null}
        <button type="button" className="btn btn-sm btn-ghost" disabled={saving} onClick={() => void save()}>
          {saving ? t('images.saving') : t('images.save')}
        </button>
        {/* Every picture, drawn or uploaded, can be enlarged. The two presets are
            shortcuts into the same panel, and none of them starts work on its own: the
            expensive method deserves the size and the cost in front of it. */}
        {fhdSize && longSide < FHD_LONG_SIDE ? (
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            disabled={blocked || !pixels}
            title={t('images.fhdHint', { width: fhdSize.width, height: fhdSize.height })}
            onClick={() => setEnlarge('fhd')}
          >
            {`${t('images.fhd')} ${formatNumber(fhdSize.width)}×${formatNumber(fhdSize.height)}`}
          </button>
        ) : null}
        {uhdSize && longSide < UHD_LONG_SIDE ? (
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            disabled={blocked || !pixels}
            title={t('images.uhdHint', { width: uhdSize.width, height: uhdSize.height })}
            onClick={() => setEnlarge('uhd')}
          >
            {`${t('images.uhd')} ${formatNumber(uhdSize.width)}×${formatNumber(uhdSize.height)}`}
          </button>
        ) : null}
        {pixels ? (
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            disabled={blocked}
            title={t('images.enlargeHint')}
            onClick={() => setEnlarge('custom')}
          >
            {t('images.enlarge')}
          </button>
        ) : null}
        {recipe && (
          <>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.redrawHint')}
              onClick={() => void draw(recipe, 'same')}
            >
              {busy === 'same' ? t('images.redrawing') : t('images.redraw')}
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.variationHint')}
              onClick={() => void draw({ ...recipe, seed: 0 }, 'new')}
            >
              {busy === 'new' ? t('images.redrawing') : t('images.variation')}
            </button>
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              disabled={blocked}
              title={t('images.editHint')}
              onClick={() => (editing ? setEditing(false) : openEditor())}
            >
              {editing ? t('images.editClose') : t('images.edit')}
            </button>
            {/* Stop appears beside the buttons it stops, and only while one is running: a
                draw from the editor gets its Stop on the editor's own button instead, so
                there is never a Stop for a draw it cannot reach. */}
            {(busy === 'same' || busy === 'new') && (
              <button
                type="button"
                className="btn btn-sm btn-danger"
                title={t('images.stopHint')}
                onClick={() => void stop()}
              >
                ⏹ {t('images.stop')}
              </button>
            )}
          </>
        )}
      </figcaption>
      {/* The Enlarge form, under the caption it belongs to. It is a form about *this*
          picture, and what it produces lands beside it on the same message. */}
      {enlarge && pixels ? (
        <EnlargePanel
          width={pixels.width}
          height={pixels.height}
          recipe={recipe}
          status={upscaler}
          defaultMethod={imageSettings?.upscaleDefaultMethod ?? 'lanczos'}
          defaultSharpen={imageSettings?.upscaleSharpen ?? 0.25}
          initialTarget={enlarge}
          busy={enlarging}
          onClose={() => setEnlarge(null)}
          onRun={(next) => void runEnlarge(next)}
          onStop={
            enlargingMethod === 'redraw' || enlargingMethod === 'esrgan'
              ? () => void stop()
              : undefined
          }
        />
      ) : null}
      {/* The editor sits inside the figure, under the caption it belongs to: it is a form
          about *this* picture, and a variation or a Draw from it lands beside it. */}
      {editing && draft && (
        <RecipeEditor
          draft={draft}
          onChange={setDraft}
          drawing={busy === 'edit'}
          busy={blocked}
          onDraw={() => void draw(recipeFromDraft(draft), 'edit')}
          onStop={() => void stop()}
        />
      )}
    </figure>
  )
}

/**
 * The recipe editor: the whole recipe in boxes, prefilled from the picture's own, with
 * one Draw that sends what is in them.
 *
 * Every field is the *whole* of what will be drawn rather than a patch over the stored
 * recipe, because that is what the backend reads: a box emptied is a choice left to the
 * server, not a field quietly kept. It is also what makes this an iteration loop rather
 * than two buttons — change the prompt, draw, look, change it again.
 *
 * The checkpoint and the sampler are free text rather than dropdowns. The settings screen
 * can offer a list because it also asks the server what it has; here that would mean a
 * request per picture rendered, and a name that is no longer installed is refused by the
 * server with a message, which is a better answer than a stale list.
 */
function RecipeEditor({
  draft,
  onChange,
  drawing,
  busy,
  onDraw,
  onStop,
}: {
  draft: RecipeDraft
  onChange: (next: RecipeDraft) => void
  drawing: boolean
  busy: boolean
  onDraw: () => void
  onStop: () => void
}) {
  const t = useStore((s) => s.t)
  const set = (patch: Partial<RecipeDraft>) => onChange({ ...draft, ...patch })
  // A prompt-less generation is a blank canvas the server draws something arbitrary on,
  // and the backend refuses it. Disabling the button here is what turns that refusal into
  // a control the user can see rather than an error after the wait.
  const ready = draft.prompt.trim() !== ''

  return (
    <div
      className="mt-1 flex flex-col gap-2 rounded p-2 text-[11px]"
      style={{ border: '1px solid var(--border)', background: 'var(--code-bg)', maxWidth: 460 }}
    >
      <span className="faint">{t('images.editTitle')}</span>
      <label className="text-[11px]">
        {t('images.editPrompt')}
        <textarea
          className="input mt-1"
          rows={3}
          value={draft.prompt}
          onChange={(e) => set({ prompt: e.target.value })}
        />
      </label>
      <label className="text-[11px]">
        {t('settings.images.negative')}
        <input
          className="input mt-1"
          value={draft.negativePrompt}
          onChange={(e) => set({ negativePrompt: e.target.value })}
          placeholder={t('settings.images.auto')}
        />
      </label>
      <label className="text-[11px]">
        {t('settings.images.size')}
        <div className="mt-1 flex items-center gap-2">
          <input
            className="input mono"
            type="number"
            min={0}
            max={MAX_IMAGE_DIMENSION}
            step={8}
            value={draft.width}
            onChange={(e) => set({ width: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
          <span className="faint">×</span>
          <input
            className="input mono"
            type="number"
            min={0}
            max={MAX_IMAGE_DIMENSION}
            step={8}
            value={draft.height}
            onChange={(e) => set({ height: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </div>
      </label>
      <div className="grid grid-cols-3 gap-2">
        <label className="text-[11px]">
          {t('settings.images.steps')}
          <input
            className="input mono mt-1"
            type="number"
            min={0}
            max={MAX_IMAGE_STEPS}
            value={draft.steps}
            onChange={(e) => set({ steps: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]" title={t('settings.images.cfgHint')}>
          {t('settings.images.cfgScale')}
          <input
            className="input mono mt-1"
            type="number"
            min={0}
            max={MAX_IMAGE_CFG}
            step={0.5}
            value={draft.cfgScale}
            onChange={(e) => set({ cfgScale: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]" title={t('settings.images.defaults')}>
          {t('images.seed')}
          <input
            className="input mono mt-1"
            type="number"
            value={draft.seed}
            onChange={(e) => set({ seed: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
      </div>
      {/* The second pass belongs in the recipe rather than only in Settings: it changes
          the output size and the time the draw costs, which are two things a user
          redrawing a picture is choosing. Its numbers stay in the boxes while the
          switch is off, so turning it back on restores the last choice. */}
      <label className="flex items-center gap-2 text-[11px]" title={t('settings.images.hiresHint')}>
        <input
          type="checkbox"
          checked={draft.hires}
          aria-label={t('settings.images.hires')}
          onChange={(e) => set({ hires: e.target.checked })}
        />
        <span>{t('settings.images.hires')}</span>
        <span className="faint flex-1">{t('settings.images.hiresHint')}</span>
      </label>
      <div className="grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.images.hiresScale')}
          <input
            className="input mono mt-1"
            type="number"
            min={MIN_IMAGE_HIRES_SCALE}
            max={MAX_IMAGE_HIRES_SCALE}
            step={0.05}
            disabled={!draft.hires}
            value={draft.hiresScale}
            onChange={(e) => set({ hiresScale: e.target.value })}
            placeholder="1.5"
          />
        </label>
        <label className="text-[11px]" title={t('settings.images.hiresDenoiseHint')}>
          {t('settings.images.hiresDenoise')}
          <input
            className="input mono mt-1"
            type="number"
            min={MIN_IMAGE_HIRES_DENOISE}
            max={MAX_IMAGE_HIRES_DENOISE}
            step={0.05}
            disabled={!draft.hires}
            value={draft.hiresDenoise}
            onChange={(e) => set({ hiresDenoise: e.target.value })}
            placeholder="0.4"
          />
        </label>
      </div>
      <div className="grid grid-cols-2 gap-2">
        <label className="text-[11px]">
          {t('settings.images.model')}
          <input
            className="input mono mt-1"
            value={draft.model}
            onChange={(e) => set({ model: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
        <label className="text-[11px]">
          {t('settings.images.sampler')}
          <input
            className="input mono mt-1"
            value={draft.sampler}
            onChange={(e) => set({ sampler: e.target.value })}
            placeholder={t('settings.images.auto')}
          />
        </label>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {/* The button the click came from is the button that stops it: a 40-step draw on a
            CPU server is minutes, and sending the user to the far side of the picture for
            the Stop is the difference between a control that helps and one they hunt for. */}
        {drawing ? (
          <button
            type="button"
            className="btn btn-sm btn-danger"
            title={t('images.stopHint')}
            onClick={onStop}
          >
            ⏹ {t('images.stop')}
          </button>
        ) : (
          <button
            type="button"
            className="btn btn-sm btn-primary"
            disabled={!ready || busy}
            title={ready ? t('images.editDrawHint') : t('images.editEmptyPrompt')}
            onClick={onDraw}
          >
            {t('images.editDraw')}
          </button>
        )}
        <span className="faint">{ready ? t('settings.images.defaults') : t('images.editEmptyPrompt')}</span>
      </div>
    </div>
  )
}

export function fileIcon(a: storeTypes.Attachment): string {
  switch (a.kind) {
    case 'pdf':
      return '📄'
    case 'image':
      return '🖼'
    case 'xlsx':
      return '📊'
    case 'pptx':
      return '📽'
    case 'docx':
      return '📝'
    case 'web':
      return '🌐'
    default:
      return '📃'
  }
}
