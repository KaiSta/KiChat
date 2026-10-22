import { type ReactNode } from 'react'

/**
 * Translate a key, falling back to text the backend supplied. translate() echoes
 * the key back when it has no entry, and that echo is the signal: a preset or a
 * template variable the backend knows but this build of the dictionary does not
 * still reads as something rather than as "preset.my-id".
 */
export function translatedOr(t: (key: string) => string, key: string, fallback: string): string {
  const translated = t(key)
  return translated === key ? fallback : translated
}

export function Section({ title, children, hint }: { title: string; children: ReactNode; hint?: string }) {
  return (
    <section className="card p-4">
      <h2 className="mb-1 text-[14px] font-semibold">{title}</h2>
      {hint && <p className="faint mb-3 text-[12px]">{hint}</p>}
      <div className={hint ? '' : 'mt-3'}>{children}</div>
    </section>
  )
}

export function Field({
  label,
  children,
  hint,
  labelledBy,
}: {
  label: string
  children: ReactNode
  hint?: string
  /**
   * Set when the control inside must not sit in a `<label>`: the model picker is a
   * combobox whose rows are clickable, and a click inside a label forwards a second
   * activation to the labelled field. The visible label is linked by
   * `aria-labelledby` instead.
   */
  labelledBy?: string
}) {
  if (labelledBy) {
    return (
      <div className="mb-3 block">
        <span
          id={labelledBy}
          className="mb-1 block text-[12px]"
          style={{ color: 'var(--text-dim)' }}
        >
          {label}
        </span>
        {children}
        {hint && <span className="faint mt-1 block text-[11px]">{hint}</span>}
      </div>
    )
  }
  return (
    <label className="mb-3 block">
      <span className="mb-1 block text-[12px]" style={{ color: 'var(--text-dim)' }}>
        {label}
      </span>
      {children}
      {hint && <span className="faint mt-1 block text-[11px]">{hint}</span>}
    </label>
  )
}

/* -------------------------------------------------------------------------
 * General
 * ---------------------------------------------------------------------- */

