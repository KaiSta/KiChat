import { Component, type ErrorInfo, type ReactNode } from 'react'
import { translate } from '../lib/i18n'
import { useStore } from '../store'

type Props = {
  /** Names the part of the UI that failed, so a report is actionable. */
  area: string
  children: ReactNode
}

type State = { error: Error | null; stack: string }

/*
 * React unmounts the entire tree when a render throws, so a single faulty
 * component leaves a blank window with no way back — that is exactly how the
 * original "blank after Neuer Chat" bug presented itself. A boundary is the only
 * place a render error can be caught, so it keeps a readable message on screen,
 * the stack for a report, and a button that retries the same render.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null, stack: '' }

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // Also log: when the UI itself is broken this line is what a bug report is
    // assembled from.
    console.error(`[KiChat] Renderfehler in ${this.props.area}:`, error, info.componentStack)
    this.setState({ stack: info.componentStack ?? '' })
  }

  private reset = () => this.setState({ error: null, stack: '' })

  render() {
    const { error, stack } = this.state
    if (!error) return this.props.children

    // Read the store directly rather than through a hook: a class component
    // cannot subscribe, and the language only matters at the moment of failure.
    const lang = useStore.getState().lang
    const t = (key: string) => translate(lang, key)
    const details = [error.stack ?? error.message, stack].filter(Boolean).join('\n\n')

    return (
      <div className="flex h-full items-center justify-center p-8">
        <div className="card w-full max-w-2xl p-4">
          <div className="mb-1 font-semibold" style={{ color: 'var(--danger)' }}>
            {t('error.title')}
          </div>
          <p className="faint mb-3 text-[12px]">{t('error.hint')}</p>
          <div className="mono faint mb-2 text-[11px]">{this.props.area}</div>
          <pre className="mono max-h-64 overflow-auto text-[11px] whitespace-pre-wrap">{details}</pre>
          <div className="mt-3 flex gap-2">
            <button type="button" className="btn btn-primary btn-sm" onClick={this.reset}>
              {t('error.retry')}
            </button>
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => void navigator.clipboard?.writeText(details).catch(() => undefined)}
            >
              {t('error.copy')}
            </button>
          </div>
        </div>
      </div>
    )
  }
}
