import { useEffect, useState } from 'react'
import { App, type config } from '../../lib/api'
import { errText, useStore } from '../../store'
import { ModelPicker } from '../ModelPicker'
import { Field, Section } from './shared'

type Status = Awaited<ReturnType<typeof App.GetGatewayStatus>>

export function GatewayTab({ draft, patch }: {
  draft: config.Settings
  patch: (changes: Partial<config.Settings>) => void
}) {
  const t = useStore((s) => s.t)
  const models = useStore((s) => s.models)
  const saved = useStore((s) => s.settings)
  const [status, setStatus] = useState<Status | null>(null)
  const [error, setError] = useState('')
  const [key, setKey] = useState('')
  const [revealing, setRevealing] = useState(false)
  const g = draft.gateway
  useEffect(() => {
    let active = true
    const poll = async () => {
      try {
        const next = await App.GetGatewayStatus()
        if (active) { setStatus(next); setError('') }
      } catch (err) { if (active) setError(errText(err)) }
    }
    void poll()
    const timer = window.setInterval(() => void poll(), 2000)
    return () => { active = false; window.clearInterval(timer) }
  }, [saved])
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      useStore.getState().toast('info', t('gateway.copied'))
    } catch (err) { setError(errText(err)) }
  }
  const reveal = async () => {
    setRevealing(true)
    try { setKey(await App.GetGatewayKey()) } catch (err) { setError(errText(err)) }
    finally { setRevealing(false) }
  }
  const last = status?.last
  return <>
    <Section title={t('gateway.title')} hint={t('gateway.hint')}>
      <label className="mb-3 flex items-center gap-2 text-[13px]">
        <input type="checkbox" checked={g.enabled} onChange={(e) => patch({ gateway: { ...g, enabled: e.target.checked } })} />
        {t('gateway.enable')}
      </label>
      <Field label={t('gateway.model')} labelledBy="gateway-model-label" hint={t('gateway.modelHint')}>
        <ModelPicker id="gateway-model" labelledBy="gateway-model-label" models={models} value={g.model}
          placeholder={t('err.gateway.chooseModel')} unavailableLabel={t('settings.summaryModelUnavailable')}
          onChange={(model) => patch({ gateway: { ...g, model: model ?? { backend: '', model: '' } } })} />
      </Field>
      <div className="grid grid-cols-3 gap-3">
        <Field label={t('gateway.port')}><input className="input" type="number" min="1024" max="65535" value={g.port}
          onChange={(e) => patch({ gateway: { ...g, port: Number(e.target.value) } })} /></Field>
        <Field label={t('gateway.context')}><input className="input" type="number" min="2048" max="1048576" value={g.contextLimit}
          onChange={(e) => patch({ gateway: { ...g, contextLimit: Number(e.target.value) } })} /></Field>
        <Field label={t('gateway.reserve')}><input className="input" type="number" min="128" max={g.contextLimit / 2} value={g.reserveTokens}
          onChange={(e) => patch({ gateway: { ...g, reserveTokens: Number(e.target.value) } })} /></Field>
      </div>
      <label className="mb-2 flex items-center gap-2 text-[13px]">
        <input type="checkbox" checked={g.compact} onChange={(e) => patch({ gateway: { ...g, compact: e.target.checked } })} />
        {t('gateway.compact')}
      </label>
      <p className="faint text-[12px]">{t('gateway.compactHint')}</p>
      <p className="faint mt-2 text-[12px]">{t('gateway.saveHint')}</p>
    </Section>
    <Section title={t('gateway.connection')} hint={t('gateway.connectionHint')}>
      <p className="mb-3 text-[13px]" role="status">{status ? t(status.running ? (status.busy ? 'gateway.busy' : 'gateway.running') : 'gateway.stopped') : t('common.loading')}</p>
      <Field label={t('gateway.url')}>
        <div className="flex gap-2"><input className="input flex-1" readOnly value={status?.url || `http://127.0.0.1:${g.port}/v1`} />
          <button type="button" className="btn btn-sm" onClick={() => void copy(status?.url || `http://127.0.0.1:${g.port}/v1`)}>{t('gateway.copy')}</button></div>
      </Field>
      <Field label={t('gateway.clientModel')} hint={t('gateway.clientModelHint')}>
        <input className="input" value={g.clientModel} placeholder="kichat"
          onChange={(e) => patch({ gateway: { ...g, clientModel: e.target.value } })} />
      </Field>
      <Field label={t('gateway.key')}>
        <div className="flex gap-2"><input className="input flex-1" type="password" readOnly value={key} />
          <button type="button" className="btn btn-sm" disabled={revealing} onClick={() => key ? void copy(key) : void reveal()}>{t(key ? 'gateway.copy' : 'gateway.reveal')}</button></div>
      </Field>
      {(error || status?.error) && <p className="text-[12px] text-red-400" role="alert">{error || errText(status?.error)}</p>}
    </Section>
    <Section title={t('gateway.last')} hint={t('gateway.lastHint')}>
      {last ? <div className="space-y-2 text-[13px]">
        <p>{t('gateway.estimated')}: {last.before.toLocaleString()} → {last.after.toLocaleString()}</p>
        <p>{t('gateway.compaction')}: {t(last.compacted ? (last.cacheHit ? 'gateway.cached' : 'gateway.applied') : 'gateway.notNeeded')}</p>
        <p>{t('gateway.compactTime')}: {last.compactMs} ms · {t('gateway.generationTime')}: {last.generationMs} ms</p>
        <p>{t('gateway.actual')}: {last.usageReported ? `${last.promptTokens.toLocaleString()} / ${last.completionTokens.toLocaleString()}` : t('gateway.unknown')}</p>
        {last.error && <p className="text-red-400" role="alert">{errText(last.error)}</p>}
      </div> : <p className="faint text-[13px]">{t('gateway.empty')}</p>}
    </Section>
  </>
}
