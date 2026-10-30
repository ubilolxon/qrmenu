import { useCallback, useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { get, patch } from '../lib/api'
import { openStream } from '../lib/sse'
import { formatMoney, t, timeAgo } from '../lib/format'
import type { Order, OrderStatus, WaiterCall } from '../lib/types'
import { ErrorBox } from '../components/ui'

const COLUMNS: { status: OrderStatus; title: string }[] = [
  { status: 'pending', title: 'New' },
  { status: 'accepted', title: 'Accepted' },
  { status: 'preparing', title: 'Cooking' },
  { status: 'ready', title: 'Ready' },
  { status: 'served', title: 'Served' },
]

const ACTION_LABEL: Record<OrderStatus, string> = {
  pending: 'Pending',
  accepted: 'Accept',
  preparing: 'Start cooking',
  ready: 'Ready',
  served: 'Served',
  closed: 'Close',
  cancelled: 'Cancel',
}

const CALL_LABEL = { waiter: 'Waiter', bill: 'Bill', help: 'Help' }

// Closed and cancelled orders leave the board, as on the server.
const isOpen = (o: Order) => o.status !== 'closed' && o.status !== 'cancelled'

export default function Orders() {
  const { restaurantID = '' } = useParams()
  const [orders, setOrders] = useState<Map<string, Order>>(new Map())
  const [calls, setCalls] = useState<Map<string, WaiterCall>>(new Map())
  const [connected, setConnected] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [, tick] = useState(0)

  // `ready` and `resync` both mean "load the board now": the hub keeps no
  // history, so refetching is the only honest recovery from a gap.
  const loadBoard = useCallback(async () => {
    try {
      const q = `?restaurant_id=${restaurantID}`
      const [o, c] = await Promise.all([
        get<{ orders: Order[] }>(`/api/v1/staff/orders${q}`),
        get<{ calls: WaiterCall[] }>(`/api/v1/staff/calls${q}`),
      ])
      setOrders(new Map(o.orders.map((x) => [x.id, x])))
      setCalls(new Map(c.calls.map((x) => [x.id, x])))
      setError(null)
    } catch (err) {
      setError(err)
    }
  }, [restaurantID])

  useEffect(() => {
    return openStream(
      restaurantID,
      (e) => {
        switch (e.type) {
          case 'ready':
          case 'resync':
            loadBoard()
            break
          case 'order.created':
          case 'order.updated':
            setOrders((cur) => upsert(cur, e.data as Order, isOpen))
            break
          case 'waiter.called':
          case 'waiter.updated':
            setCalls((cur) => upsert(cur, e.data as WaiterCall, (c) => c.status !== 'resolved'))
            break
        }
      },
      setConnected,
    )
  }, [restaurantID, loadBoard])

  // Re-render each minute so "5 min ago" stays true on a screen left open all shift.
  useEffect(() => {
    const id = setInterval(() => tick((n) => n + 1), 60_000)
    return () => clearInterval(id)
  }, [])

  async function move(order: Order, status: OrderStatus) {
    let reason: string | undefined
    if (status === 'cancelled') {
      const r = prompt('Why is this order cancelled?')
      if (r === null) return
      reason = r
    }
    try {
      const updated = await patch<Order>(`/api/v1/staff/orders/${order.id}/status`, { status, reason })
      setOrders((cur) => upsert(cur, updated, isOpen))
    } catch (err) {
      // 409: someone else moved it first. The stream will bring the truth; reload now to be sure.
      setError(err)
      loadBoard()
    }
  }

  async function answer(call: WaiterCall) {
    const status = call.status === 'pending' ? 'acknowledged' : 'resolved'
    try {
      const updated = await patch<WaiterCall>(`/api/v1/staff/calls/${call.id}/status`, { status })
      setCalls((cur) => upsert(cur, updated, (c) => c.status !== 'resolved'))
    } catch (err) {
      setError(err)
      loadBoard()
    }
  }

  const byStatus = (s: OrderStatus) =>
    [...orders.values()].filter((o) => o.status === s).sort((a, b) => a.placed_at.localeCompare(b.placed_at))

  return (
    <main className="page wide">
      <div className="row spread">
        <h1>Orders</h1>
        <span className={`status-dot ${connected ? 'live' : ''}`}>{connected ? 'Live' : 'Reconnecting…'}</span>
      </div>
      <ErrorBox error={error} />

      {calls.size > 0 && (
        <div className="calls">
          {[...calls.values()].map((call) => (
            <div key={call.id} className={`call ${call.status}`}>
              <strong>Table {call.table_label}</strong> · {CALL_LABEL[call.kind]}
              <span className="muted small"> {timeAgo(call.created_at)}</span>
              <button className="small" onClick={() => answer(call)}>
                {call.status === 'pending' ? 'On my way' : 'Done'}
              </button>
            </div>
          ))}
        </div>
      )}

      <div className="board">
        {COLUMNS.map((col) => {
          const list = byStatus(col.status)
          return (
            <section key={col.status} className="column">
              <h2>{col.title} <span className="muted">{list.length}</span></h2>
              {list.map((o) => <OrderCard key={o.id} order={o} onMove={(s) => move(o, s)} />)}
            </section>
          )
        })}
      </div>
    </main>
  )
}

function OrderCard({ order, onMove }: { order: Order; onMove: (s: OrderStatus) => void }) {
  // next_statuses is computed by the server, so only buttons that will work are shown.
  const forward = order.next_statuses.filter((s) => s !== 'cancelled')
  const canCancel = order.next_statuses.includes('cancelled')

  return (
    <article className={`order ${order.status}`}>
      <header className="row spread">
        <strong>#{order.code} · Table {order.table_label}</strong>
        <span className="muted small">{timeAgo(order.placed_at)}</span>
      </header>
      <ul>
        {order.lines.map((l) => (
          <li key={l.id}>
            <b>{l.qty}×</b> {t(l.name, order.locale)}
            {t(l.variant_name, order.locale) && <span className="muted"> ({t(l.variant_name, order.locale)})</span>}
            {l.modifiers.length > 0 && (
              <div className="muted small">+ {l.modifiers.map((m) => t(m.name, order.locale)).join(', ')}</div>
            )}
            {l.note && <div className="note">“{l.note}”</div>}
          </li>
        ))}
      </ul>
      {order.note && <div className="note">“{order.note}”</div>}
      <footer className="row spread">
        <span className="price">{formatMoney(order.total_minor, order.currency)}</span>
        <div className="row">
          {canCancel && <button className="ghost small danger" onClick={() => onMove('cancelled')}>Cancel</button>}
          {forward.map((s) => (
            <button key={s} className="primary small" onClick={() => onMove(s)}>{ACTION_LABEL[s]}</button>
          ))}
        </div>
      </footer>
    </article>
  )
}

function upsert<T extends { id: string }>(cur: Map<string, T>, item: T, keep: (x: T) => boolean) {
  const next = new Map(cur)
  if (keep(item)) next.set(item.id, item)
  else next.delete(item.id)
  return next
}
