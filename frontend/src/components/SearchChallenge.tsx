import { useEffect, useState } from 'react'
import { App } from '../lib/api'
import { errText, useStore } from '../store'

/**
 * The DuckDuckGo bot check, answered in the window.
 *
 * Tiles are fetched through the bound method rather than by pointing `<img>` at the site: the
 * check was issued to this app's connection, and a webview request is a different one — its own
 * ClientHello, its own cookies — which the host has no reason to answer for this identity. The
 * answer is posted the same way, so it counts towards this client's clearance instead of proving
 * something about the webview.
 *
 * A wrong answer comes back as another check, as an ordinary event, and replaces what is shown
 * here. The dialog therefore never judges the answer: the host does, in its reply.
 */

/** How many tile requests may be in flight at once. Every picture is its own handshake to the
 * site, and nine at once is exactly the burst the check is there to stop: a few at a time fills
 * the grid almost as quickly without looking like a flood. */
const TILE_CONCURRENCY = 3

export function SearchChallenge() {
  const t = useStore((s) => s.t)
  const challenge = useStore((s) => s.searchChallenge)
  const setChallenge = useStore((s) => s.setSearchChallenge)
  const toast = useStore((s) => s.toast)
  const [ticked, setTicked] = useState<Record<string, boolean>>({})
  // One entry per tile that has an answer: its picture as a data URL, or null when the request
  // failed. A tile with no entry is still loading. A failed picture stays ticable — that is the
  // network talking, not evidence about the check.
  const [pictures, setPictures] = useState<Record<string, string | null>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!challenge) return
    let live = true
    setTicked({})
    setPictures({})
    // A shared queue drained by a few workers: each takes the next name as its own tile settles,
    // so one slow picture holds one slot rather than the whole grid. The queue is abandoned when
    // the challenge goes, so a dismissed check does not keep knocking after its dialog is gone.
    const names = (challenge.tiles ?? []).map((tile) => tile.name)
    const load = async () => {
      while (live) {
        const name = names.shift()
        if (!name) return
        try {
          const src = await App.SearchChallengeTile(name)
          if (live) setPictures((prev) => ({ ...prev, [name]: src }))
        } catch {
          if (live) setPictures((prev) => ({ ...prev, [name]: null }))
        }
      }
    }
    for (let worker = 0; worker < TILE_CONCURRENCY; worker++) void load()
    return () => {
      live = false
    }
  }, [challenge])

  if (!challenge) return null

  const tiles = challenge.tiles ?? []
  const tickedNames = tiles.filter((tile) => ticked[tile.name]).map((tile) => tile.name)

  const close = () => {
    setChallenge(null)
    // The backend forgets it too, so a check the reader gave up on cannot be answered later by
    // a window that no longer shows it.
    void App.DismissSearchChallenge().catch(() => undefined)
  }

  const answer = async () => {
    setBusy(true)
    try {
      const outcome = await App.SolveSearchChallenge(tickedNames)
      setChallenge(null)
      toast('info', t('search.challengeSolved', { count: outcome.results?.length ?? 0 }))
    } catch (err) {
      // Left open on purpose: a rejected answer usually means a few tiles were wrong, and the
      // next check has already replaced this one's tiles through the event.
      toast('error', errText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      className="absolute inset-0 z-40 flex items-center justify-center p-6"
      style={{ background: 'rgba(0,0,0,0.55)' }}
    >
      <div
        className="card scroll-thin max-h-full w-[34rem] overflow-y-auto p-4"
        role="dialog"
        aria-modal="true"
        aria-labelledby="search-challenge-title"
        onKeyDown={(event) => {
          if (event.key === 'Escape' && !busy) {
            event.preventDefault()
            close()
          }
        }}
      >
        <div id="search-challenge-title" className="mb-2 font-semibold">
          {t('search.challengeTitle')}
        </div>
        <p className="muted mb-1 text-[12px]">{t('search.challengeIntro')}</p>
        {challenge.query && (
          <p className="mono faint mb-3 text-[12px]">
            {t('search.query')}: {challenge.query}
          </p>
        )}
        <p className="mb-3 text-[13px] font-medium">
          {challenge.instruction || t('search.challengeInstruction')}
        </p>

        <div className="grid grid-cols-3 gap-2">
          {tiles.map((tile) => {
            const on = Boolean(ticked[tile.name])
            const picture = pictures[tile.name]
            return (
              <button
                key={tile.name}
                type="button"
                aria-pressed={on}
                aria-label={tile.name}
                className="relative aspect-square overflow-hidden rounded-md border"
                style={{
                  borderColor: on ? 'var(--accent)' : 'var(--border)',
                  background: 'var(--surface-2)',
                  outline: on ? '2px solid var(--accent)' : undefined,
                }}
                onClick={() => setTicked((prev) => ({ ...prev, [tile.name]: !prev[tile.name] }))}
              >
                {picture ? (
                  <img src={picture} alt="" className="h-full w-full object-cover" />
                ) : (
                  <span className="faint flex h-full items-center justify-center text-[11px]">
                    {picture === null ? '—' : '…'}
                  </span>
                )}
                {on && (
                  <span
                    className="absolute top-1 right-1 rounded-full px-1.5 text-[11px]"
                    style={{ background: 'var(--accent)', color: '#fff' }}
                  >
                    ✓
                  </span>
                )}
              </button>
            )
          })}
        </div>

        <div className="mt-4 flex items-center justify-end gap-2">
          <button type="button" className="btn" autoFocus disabled={busy} onClick={close}>
            {t('common.cancel')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={busy || tickedNames.length === 0}
            onClick={() => void answer()}
          >
            {busy ? '…' : t('search.challengeSolve')}
          </button>
        </div>
      </div>
    </div>
  )
}
