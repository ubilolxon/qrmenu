import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { del, get, inRestaurant, patch, post } from '../lib/api'
import { useUser } from '../lib/auth'
import type { Table } from '../lib/types'
import { ErrorBox, Toggle } from '../components/ui'
import { useRestaurant } from './RestaurantLayout'

// The API builds QR URLs from PUBLIC_BASE_URL (http://localhost:8080 in dev).
// Pointing <img> at the same path on this origin goes through the Vite proxy
// instead, so the page works whatever host the API is configured with.
const sameOrigin = (url: string) => new URL(url).pathname

export default function Tables() {
  const restaurant = useRestaurant()
  const rid = restaurant.id
  const tables = useQuery({
    queryKey: ['tables', rid],
    queryFn: () => get<{ tables: Table[] }>(inRestaurant(rid, '/tables')).then((r) => r.tables),
  })

  return (
    <main className="page">
      <div className="row spread">
        <h1>Tables & QR codes</h1>
        <a className="button" href={`/m/${restaurant.slug}/qr.card.svg`} target="_blank" rel="noreferrer">
          Restaurant QR (no table) ↗
        </a>
      </div>
      <p className="muted">
        Each table gets its own QR code, so an order placed from it lands on the board with the table name.
        Print the card version and stand it on the table.
      </p>
      <ErrorBox error={tables.error} />
      <NewTable restaurantID={rid} />
      {tables.data?.length === 0 && <div className="empty">No tables yet.</div>}
      <div className="grid">
        {tables.data?.map((table) => <TableCard key={table.id} restaurantID={rid} table={table} />)}
      </div>
    </main>
  )
}

function NewTable({ restaurantID }: { restaurantID: string }) {
  const qc = useQueryClient()
  const [label, setLabel] = useState('')
  const [seats, setSeats] = useState('')
  const create = useMutation({
    mutationFn: () =>
      post(inRestaurant(restaurantID, '/tables'), { label: label.trim(), seats: seats ? Number(seats) : null }),
    onSuccess: () => {
      setLabel('')
      setSeats('')
      qc.invalidateQueries({ queryKey: ['tables', restaurantID] })
    },
  })

  function submit(e: FormEvent) {
    e.preventDefault()
    create.mutate()
  }

  return (
    <form className="card subtle row inline-form" onSubmit={submit}>
      <input placeholder="Table label, e.g. 7 or Terrace 2" value={label} onChange={(e) => setLabel(e.target.value)} required />
      <input placeholder="Seats" type="number" min={1} value={seats} onChange={(e) => setSeats(e.target.value)} className="narrow-input" />
      <button disabled={create.isPending}>Add table</button>
      <ErrorBox error={create.error} />
    </form>
  )
}

function TableCard({ restaurantID, table }: { restaurantID: string; table: Table }) {
  const user = useUser()
  const qc = useQueryClient()
  const refresh = () => qc.invalidateQueries({ queryKey: ['tables', restaurantID] })
  const path = inRestaurant(restaurantID, `/tables/${table.id}`)

  const update = useMutation({ mutationFn: (body: object) => patch(path, body), onSuccess: refresh })
  const remove = useMutation({ mutationFn: () => del(path), onSuccess: refresh })
  const rotate = useMutation({ mutationFn: () => post(`${path}/rotate-token`), onSuccess: refresh })

  return (
    <div className={`card table-card ${table.is_active ? '' : 'inactive'}`}>
      <div className="row spread">
        <h3>Table {table.label}</h3>
        {table.seats && <span className="muted">{table.seats} seats</span>}
      </div>
      <img src={`${sameOrigin(table.qr_svg_url)}`} alt={`QR code for table ${table.label}`} className="qr" />
      <div className="row wrap">
        <a href={sameOrigin(table.qr_card_url)} target="_blank" rel="noreferrer">Print card</a>
        <a href={`${sameOrigin(table.qr_png_url)}?size=1024`} download={`table-${table.label}.png`}>PNG</a>
        <a href={sameOrigin(table.menu_url)} target="_blank" rel="noreferrer">Open menu</a>
      </div>
      <div className="row spread">
        <Toggle checked={table.is_active} label="Active" onChange={(is_active) => update.mutate({ is_active })} />
        <div className="row">
          {user.role === 'owner' && (
            <button
              className="ghost small"
              title="Issue a new code. Every printed card for this table stops working."
              onClick={() => confirm('New QR code? Printed cards for this table will stop working.') && rotate.mutate()}
            >
              New code
            </button>
          )}
          <button className="danger ghost small" onClick={() => confirm(`Delete table ${table.label}?`) && remove.mutate()}>
            Delete
          </button>
        </div>
      </div>
      <ErrorBox error={update.error ?? remove.error ?? rotate.error} />
    </div>
  )
}
