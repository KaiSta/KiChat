import { useState } from 'react'
import { App, type main } from '../lib/api'
import { errText, useStore } from '../store'
import { Field } from './settings/shared'

/**
 * The window while the database cannot be read.
 *
 * It renders without settings on purpose: a locked database refuses every binding but
 * the ones in api_database.go, so there is no language or theme to read. English and
 * the app's default theme are what the screen can honestly show, and both switch back
 * as soon as an unlock succeeds and the real settings load.
 *
 * Setup mode is the different case and says so: with no keyring and no key, the
 * passphrase is not unwrapping anything — it becomes the only copy of the key, and the
 * sentence above the field has to make that clear before the user picks one.
 */
export function UnlockScreen({ status }: { status: main.DatabaseStatus }) {
  const t = useStore((s) => s.t)
  const tDynamic = useStore((s) => s.tDynamic)
  const bootstrap = useStore((s) => s.bootstrap)

  const [passphrase, setPassphrase] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [askFresh, setAskFresh] = useState(false)
  const [restoreOpen, setRestoreOpen] = useState(false)
  const [restorePassphrase, setRestorePassphrase] = useState('')

  const mismatch = status.setup && confirm.length > 0 && passphrase !== confirm
  const short = status.setup && passphrase.length > 0 && passphrase.length < 8
  const canSubmit =
    passphrase.length > 0 &&
    (!status.setup || (passphrase === confirm && passphrase.length >= 8)) &&
    !busy

  const submit = async () => {
    if (!canSubmit) return
    setBusy(true)
    setError('')
    try {
      await App.UnlockDatabase(passphrase)
      // The passphrase is not kept in the window once it has been used.
      setPassphrase('')
      setConfirm('')
      await bootstrap()
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
    }
  }

  const restore = async () => {
    if (!restorePassphrase || busy) return
    setBusy(true)
    setError('')
    try {
      // The Go side asks for the two files and does not touch this installation until
      // their passphrase and key have opened the copy in a temporary directory.
      await App.RestoreDatabase(
        restorePassphrase,
        t('db.restore.pickDatabase'),
        t('db.restore.pickKey'),
      )
      setRestorePassphrase('')
      setRestoreOpen(false)
      await bootstrap()
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
    }
  }

  const startFresh = async () => {
    setBusy(true)
    setError('')
    try {
      await App.StartFreshDatabase()
      await bootstrap()
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
      setAskFresh(false)
    }
  }

  return (
    <div className="flex h-full items-center justify-center p-6">
      <form
        className="card w-full max-w-md p-5"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <h1 className="mb-1 text-[16px] font-semibold">
          {t(status.setup ? 'db.unlock.setupTitle' : 'db.unlock.title')}
        </h1>
        <p className="faint mb-4 text-[12px]">
          {t(status.setup ? 'db.unlock.setupHint' : 'db.unlock.hint')}
        </p>
        {status.reason && <p className="mb-3 text-[12px]">{tDynamic(status.reason)}</p>}

        <Field label={t('db.unlock.passphrase')}>
          <input
            className="input"
            type="password"
            autoFocus
            autoComplete={status.setup ? 'new-password' : 'current-password'}
            value={passphrase}
            onChange={(e) => setPassphrase(e.target.value)}
          />
        </Field>
        {status.setup && (
          <Field label={t('db.unlock.confirm')}>
            <input
              className="input"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
          </Field>
        )}
        {mismatch && (
          <p className="mb-2 text-[12px]" style={{ color: 'var(--danger)' }}>
            {t('db.unlock.mismatch')}
          </p>
        )}
        {short && (
          <p className="mb-2 text-[12px]" style={{ color: 'var(--danger)' }}>
            {t('err.db.shortPassphrase', { '1': '8' })}
          </p>
        )}
        {error && (
          <p className="mb-2 text-[12px]" style={{ color: 'var(--danger)' }}>
            {error}
          </p>
        )}

        <button type="submit" className="btn btn-sm btn-primary" disabled={!canSubmit}>
          {busy
            ? t('db.unlock.unlocking')
            : t(status.setup ? 'db.unlock.setupButton' : 'db.unlock.button')}
        </button>

        {/* A copied installation arrives as two files with fixed names. They are
            checked before the current database is moved anywhere, so a wrong
            passphrase or a key from another database is safe to try. */}
        <div className="mt-4 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
          {!restoreOpen ? (
            <button
              type="button"
              className="btn btn-sm btn-ghost"
              onClick={() => {
                setRestoreOpen(true)
                setError('')
              }}
              disabled={busy}
            >
              {t('db.restore.button')}
            </button>
          ) : (
            <>
              <p className="faint mb-2 text-[12px]">{t('db.restore.hint')}</p>
              <Field label={t('db.restore.passphrase')}>
                <input
                  className="input"
                  type="password"
                  autoComplete="current-password"
                  value={restorePassphrase}
                  onChange={(e) => setRestorePassphrase(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault()
                      void restore()
                    }
                  }}
                />
              </Field>
              <div className="flex gap-2">
                <button
                  type="button"
                  className="btn btn-sm btn-primary"
                  onClick={() => void restore()}
                  disabled={!restorePassphrase || busy}
                >
                  {busy ? t('db.restore.busy') : t('db.restore.confirm')}
                </button>
                <button
                  type="button"
                  className="btn btn-sm btn-ghost"
                  onClick={() => {
                    setRestoreOpen(false)
                    setRestorePassphrase('')
                  }}
                  disabled={busy}
                >
                  {t('common.cancel')}
                </button>
              </div>
            </>
          )}
        </div>

        {/* Moving the database aside is the only way out of a lost key, so it takes a
            second click and says what happens to the old file. */}
        {!status.setup && (
          <div className="mt-4 border-t pt-3" style={{ borderColor: 'var(--border)' }}>
            {!askFresh ? (
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                onClick={() => setAskFresh(true)}
                disabled={busy}
              >
                {t('db.unlock.fresh')}
              </button>
            ) : (
              <>
                <p className="mb-2 text-[12px]" style={{ color: 'var(--danger)' }}>
                  {t('db.unlock.freshHint')}
                </p>
                <div className="flex gap-2">
                  <button type="button" className="btn btn-sm" onClick={() => void startFresh()} disabled={busy}>
                    {t('db.unlock.freshConfirm')}
                  </button>
                  <button type="button" className="btn btn-sm btn-ghost" onClick={() => setAskFresh(false)} disabled={busy}>
                    {t('common.cancel')}
                  </button>
                </div>
              </>
            )}
          </div>
        )}
      </form>
    </div>
  )
}
