import type { ReactNode } from 'react'
import { errorText } from '../lib/api'
import { LOCALE_NAMES } from '../lib/format'
import type { Text } from '../lib/types'

export function ErrorBox({ error }: { error: unknown }) {
  if (!error) return null
  return <div className="error" role="alert">{errorText(error)}</div>
}

export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  )
}

// One input per restaurant locale. The admin API always edits every
// translation at once, so the value is the whole {"uz": .., "ru": ..} object.
export function TextField({
  label,
  value,
  locales,
  onChange,
  multiline,
  required,
}: {
  label: string
  value: Text
  locales: string[]
  onChange: (v: Text) => void
  multiline?: boolean
  required?: string
}) {
  return (
    <fieldset className="i18n">
      <legend>{label}</legend>
      {locales.map((loc) => {
        const props = {
          value: value[loc] ?? '',
          placeholder: LOCALE_NAMES[loc] ?? loc,
          required: loc === required,
          onChange: (e: { target: { value: string } }) => onChange({ ...value, [loc]: e.target.value }),
        }
        return (
          <div key={loc} className="i18n-row">
            <span className="tag">{loc}</span>
            {multiline ? <textarea rows={2} {...props} /> : <input {...props} />}
          </div>
        )
      })}
    </fieldset>
  )
}

// Empty strings are dropped: the server stores a translation object, and
// {"ru": ""} is a translation that says nothing.
export function cleanText(text: Text): Text {
  return Object.fromEntries(Object.entries(text).map(([k, v]) => [k, v.trim()]).filter(([, v]) => v))
}

export function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (v: boolean) => void; label: string }) {
  return (
    <label className="toggle" title={label}>
      <input type="checkbox" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <span>{label}</span>
    </label>
  )
}
