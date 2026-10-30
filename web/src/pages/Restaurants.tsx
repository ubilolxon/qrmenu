import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { admin, get, post } from '../lib/api'
import { useUser } from '../lib/auth'
import { LOCALE_NAMES, t } from '../lib/format'
import type { Restaurant } from '../lib/types'
import Header from '../components/Header'
import { ErrorBox, Field } from '../components/ui'

export function useRestaurants() {
  return useQuery({
    queryKey: ['restaurants'],
    queryFn: () => get<{ restaurants: Restaurant[] }>(admin('/restaurants')).then((r) => r.restaurants),
  })
}

export default function Restaurants() {
  const user = useUser()
  if (user.role === 'waiter' || user.role === 'kitchen') return <StaffHome />
  return <RestaurantList />
}

function StaffHome() {
  return (
    <>
      <Header />
      <main className="page narrow">
        <div className="empty">
          Open the order board from the link your manager shared — it looks like <code>/r/…/orders</code>.
        </div>
      </main>
    </>
  )
}

function RestaurantList() {
  const user = useUser()
  const { data, error, isPending } = useRestaurants()
  const [creating, setCreating] = useState(false)

  return (
    <>
      <Header />
      <main className="page narrow">
        <div className="row spread">
          <h1>Restaurants</h1>
          {user.role === 'owner' && !creating && (
            <button className="primary" onClick={() => setCreating(true)}>New restaurant</button>
          )}
        </div>
        <ErrorBox error={error} />
        {creating && <CreateRestaurant onDone={() => setCreating(false)} />}
        {isPending && <p className="muted">Loading…</p>}
        {data?.length === 0 && !creating && (
          <div className="empty">No restaurants yet. Create one to start building a menu.</div>
        )}
        <ul className="list">
          {data?.map((r) => (
            <li key={r.id}>
              <Link to={`/r/${r.id}`} className="list-link">
                <strong>{t(r.name, r.default_locale)}</strong>
                <span className="muted">/m/{r.slug} · {r.currency} · {r.locales.join(', ')}</span>
                {!r.is_active && <span className="tag warn">inactive</span>}
              </Link>
            </li>
          ))}
        </ul>
      </main>
    </>
  )
}

function CreateRestaurant({ onDone }: { onDone: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [currency, setCurrency] = useState('UZS')
  const [locales, setLocales] = useState<string[]>(['uz'])
  const defaultLocale = locales[0] ?? 'uz'

  const create = useMutation({
    mutationFn: () =>
      post<Restaurant>(admin('/restaurants'), {
        slug,
        name: { [defaultLocale]: name },
        currency,
        default_locale: defaultLocale,
        locales,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restaurants'] })
      onDone()
    },
  })

  function submit(e: FormEvent) {
    e.preventDefault()
    create.mutate()
  }

  function toggleLocale(loc: string) {
    setLocales((cur) => (cur.includes(loc) ? cur.filter((l) => l !== loc) : [...cur, loc]))
  }

  return (
    <form className="card" onSubmit={submit}>
      <h2>New restaurant</h2>
      <Field label={`Name (${defaultLocale})`}>
        <input value={name} onChange={(e) => setName(e.target.value)} required />
      </Field>
      <Field label="Slug" hint="Used in the public link: /m/your-slug">
        <input value={slug} onChange={(e) => setSlug(e.target.value.toLowerCase())} pattern="[a-z0-9]+(-[a-z0-9]+)*" required />
      </Field>
      <Field label="Currency">
        <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
          <option>UZS</option>
          <option>USD</option>
          <option>EUR</option>
          <option>KZT</option>
        </select>
      </Field>
      <div className="field">
        <span className="field-label">Languages (first one is the default)</span>
        <div className="row">
          {Object.entries(LOCALE_NAMES).map(([loc, label]) => (
            <label key={loc} className="toggle">
              <input type="checkbox" checked={locales.includes(loc)} onChange={() => toggleLocale(loc)} />
              <span>{label}</span>
            </label>
          ))}
        </div>
      </div>
      <ErrorBox error={create.error} />
      <div className="row">
        <button className="primary" disabled={create.isPending || locales.length === 0}>Create</button>
        <button type="button" className="ghost" onClick={onDone}>Cancel</button>
      </div>
    </form>
  )
}
