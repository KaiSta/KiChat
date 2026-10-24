import { useState } from 'react'
import { App, formatBytes, timeAgo, type main } from '../../lib/api'
import { errText, useStore } from '../../store'
import { Section, Field } from './shared'

/**
 * The size of an archive in a unit that survives being small.
 *
 * formatBytes rounds everything below a megabyte to "0 MB", which is right for a model
 * list and wrong here: a selection of text-only chats produces an archive of a few
 * hundred kilobytes, and the panel would report that it saved nothing.
 */
function size(n: number): string {
  return n >= 1e6 ? formatBytes(n) : `${Math.max(1, Math.round(n / 1e3))} KB`
}

/**
 * Backups: one encrypted archive out, a choice of chats in.
 *
 * The two halves are deliberately separate. Export never touches the archive that is
 * being imported, and the import shows what it would do before it writes anything —
 * the conflict rule (the newer copy wins) is invisible otherwise, and "it replaced my
 * chat" is the kind of surprise a backup feature cannot afford.
 */
export function DataTab() {
  const t = useStore((s) => s.t)
  const lang = useStore((s) => s.lang)
  const chats = useStore((s) => s.chats)
  const toast = useStore((s) => s.toast)

  const [selected, setSelected] = useState<string[]>([])
  const [passphrase, setPassphrase] = useState('')
  const [confirm, setConfirm] = useState('')
  const [exporting, setExporting] = useState(false)
  const [exported, setExported] = useState('')

  const [importPass, setImportPass] = useState('')
  const [opening, setOpening] = useState(false)
  const [preview, setPreview] = useState<main.BackupPreview | null>(null)
  const [importSelection, setImportSelection] = useState<string[]>([])
  const [importing, setImporting] = useState(false)
  const [result, setResult] = useState('')

  const mismatch = confirm.length > 0 && passphrase !== confirm
  const canExport = selected.length > 0 && passphrase.length > 0 && passphrase === confirm && !exporting

  const toggle = (list: string[], id: string, set: (v: string[]) => void) =>
    set(list.includes(id) ? list.filter((x) => x !== id) : [...list, id])

  const doExport = async () => {
    setExporting(true)
    setExported('')
    try {
      const out = await App.ExportBackup({
        chatIds: selected,
        passphrase,
        title: t('data.exportTitle'),
      })
      if (!out.path) return // The dialog was closed; nothing happened.
      setExported(
        t('data.exported', { '1': out.path, '2': String(out.chats), '3': size(out.bytes) }),
      )
      // The passphrase is not kept in the window after it has been used.
      setPassphrase('')
      setConfirm('')
    } catch (err) {
      toast('error', errText(err))
    } finally {
      setExporting(false)
    }
  }

  const openArchive = async () => {
    setOpening(true)
    setPreview(null)
    setResult('')
    try {
      const opened = await App.OpenBackup(importPass, t('data.importTitle'))
      if (!opened.path) return
      setPreview(opened)
      // Newer and new chats are what an import will actually write, so they start
      // checked; the rest are shown but not selected, because selecting them would
      // change nothing.
      setImportSelection(opened.chats.filter((c) => c.state === 'new' || c.state === 'newer').map((c) => c.chatId))
      setImportPass('')
    } catch (err) {
      toast('error', errText(err))
    } finally {
      setOpening(false)
    }
  }

  const doImport = async () => {
    setImporting(true)
    try {
      const out = await App.ImportBackup(importSelection)
      setResult(
        t('data.result', {
          '1': String(out.imported.length),
          '2': String(out.replaced.length),
          '3': String(out.skipped.length),
        }),
      )
      setPreview(null)
      setImportSelection([])
      if (out.imported.length + out.replaced.length > 0) await useStore.getState().loadChats()
    } catch (err) {
      toast('error', errText(err))
    } finally {
      setImporting(false)
    }
  }

  const stateLabel = (state: string) =>
    state === 'new'
      ? t('data.stateNew')
      : state === 'newer'
        ? t('data.stateNewer')
        : state === 'older'
          ? t('data.stateOlder')
          : t('data.stateSame')

  return (
    <>
      <Section title={t('data.exportTitle')} hint={t('data.exportHint')}>
        <div className="mb-2 flex items-center gap-2">
          <button className="btn btn-sm btn-ghost" onClick={() => setSelected(chats.map((c) => c.id))}>
            {t('data.selectAll')}
          </button>
          <button className="btn btn-sm btn-ghost" onClick={() => setSelected([])}>
            {t('data.selectNone')}
          </button>
          <span className="faint ml-auto text-[12px]">
            {t('data.selected', { '1': String(selected.length), '2': String(chats.length) })}
          </span>
        </div>
        <div
          className="mb-3 max-h-64 overflow-y-auto scroll-thin rounded border"
          style={{ borderColor: 'var(--border)' }}
        >
          {chats.length === 0 && <p className="faint p-3 text-[12px]">{t('data.noChats')}</p>}
          {chats.map((c) => (
            <label
              key={c.id}
              className="flex items-center gap-2 border-b px-3 py-1.5 text-[13px] last:border-b-0"
              style={{ borderColor: 'var(--border)' }}
            >
              <input
                type="checkbox"
                checked={selected.includes(c.id)}
                onChange={() => toggle(selected, c.id, setSelected)}
              />
              <span className="truncate">{c.title || '—'}</span>
              <span className="faint ml-auto shrink-0 text-[11px]">{timeAgo(c.updatedAt, lang)}</span>
            </label>
          ))}
        </div>
        <Field label={t('data.passphrase')} hint={t('data.passphraseHint')}>
          <input
            className="input"
            type="password"
            autoComplete="new-password"
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        </Field>
        <Field label={t('data.passphraseConfirm')}>
          <input
            className="input"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
        </Field>
        {mismatch && (
          <p className="mb-2 text-[12px]" style={{ color: 'var(--danger)' }}>
            {t('data.passphraseMismatch')}
          </p>
        )}
        <button className="btn btn-sm btn-primary" disabled={!canExport} onClick={() => void doExport()}>
          {exporting ? '…' : t('data.export')}
        </button>
        {exported && <p className="faint mt-2 break-all text-[12px]">{exported}</p>}
      </Section>

      <Section title={t('data.importTitle')} hint={t('data.importHint')}>
        <Field label={t('data.passphrase')}>
          <input
            className="input"
            type="password"
            autoComplete="off"
            value={importPass}
            onChange={(e) => setImportPass(e.target.value)}
          />
        </Field>
        <button
          className="btn btn-sm"
          disabled={!importPass || opening || importing}
          onClick={() => void openArchive()}
        >
          {opening ? '…' : t('data.open')}
        </button>

        {preview && preview.path && (
          <div className="mt-4">
            <p className="faint mb-2 text-[12px]">
              {t('data.archive', { '1': timeAgo(preview.exported, lang) })} ·{' '}
              {size(preview.chats.reduce((n, c) => n + c.bytes, 0))}
            </p>
            <div className="rounded border" style={{ borderColor: 'var(--border)' }}>
              {preview.chats.map((c) => {
                // Only a chat this machine does not have, or has an older copy of, is
                // selectable: the import would skip the rest anyway, and a tickbox that
                // does nothing is a lie.
                const importable = c.state === 'new' || c.state === 'newer'
                return (
                  <label
                    key={c.chatId}
                    className="flex items-center gap-2 border-b px-3 py-1.5 text-[13px] last:border-b-0"
                    style={{ borderColor: 'var(--border)', opacity: importable ? 1 : 0.55 }}
                  >
                    <input
                      type="checkbox"
                      disabled={!importable}
                      checked={importSelection.includes(c.chatId)}
                      onChange={() => toggle(importSelection, c.chatId, setImportSelection)}
                    />
                    <span className="truncate">{c.title || '—'}</span>
                    <span className="faint ml-auto shrink-0 text-[11px]">
                      {timeAgo(c.updatedAt, lang)} · {stateLabel(c.state)}
                    </span>
                  </label>
                )
              })}
            </div>
            <button
              className="btn btn-sm btn-primary mt-3"
              disabled={importSelection.length === 0 || importing}
              onClick={() => void doImport()}
            >
              {importing ? '…' : t('data.importSelected')}
            </button>
          </div>
        )}
        {result && <p className="faint mt-2 text-[12px]">{result}</p>}
      </Section>
    </>
  )
}
