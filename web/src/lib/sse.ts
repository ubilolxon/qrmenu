import { getAccessToken, refreshSession } from './api'

export interface StreamEvent {
  type: string
  data: unknown
}

// Reads the staff stream with fetch() rather than EventSource: the stream
// authenticates with an Authorization header, and EventSource cannot send one.
// (A token in the query string would land in every access log.)
//
// Reconnects forever with backoff until the returned stop() is called. The
// server opens every connection with `ready`, so the caller reloads the board
// on that and needs no other recovery logic.
export function openStream(
  restaurantID: string,
  onEvent: (e: StreamEvent) => void,
  onStatus: (connected: boolean) => void,
): () => void {
  const controller = new AbortController()
  let stopped = false
  let delay = 1000

  const run = async () => {
    while (!stopped) {
      try {
        const res = await fetch(`/api/v1/staff/stream?restaurant_id=${restaurantID}`, {
          headers: { Authorization: `Bearer ${getAccessToken() ?? ''}`, Accept: 'text/event-stream' },
          signal: controller.signal,
        })
        if (res.status === 401) {
          // The access token expired mid-shift; refresh and reconnect.
          if (!(await refreshSession())) return
          continue
        }
        if (!res.ok || !res.body) throw new Error(`stream ${res.status}`)

        onStatus(true)
        delay = 1000
        await readEvents(res.body, onEvent)
      } catch (err) {
        if (stopped) return
        console.warn('staff stream dropped', err)
      }
      onStatus(false)
      await new Promise((r) => setTimeout(r, delay))
      delay = Math.min(delay * 2, 15000)
    }
  }
  run()

  return () => {
    stopped = true
    controller.abort()
  }
}

async function readEvents(body: ReadableStream<Uint8Array>, onEvent: (e: StreamEvent) => void) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) return
    // stream: true keeps a multi-byte character split across chunks intact.
    buffer += decoder.decode(value, { stream: true })
    // Events are separated by a blank line.
    let end: number
    while ((end = buffer.indexOf('\n\n')) !== -1) {
      const block = buffer.slice(0, end)
      buffer = buffer.slice(end + 2)
      const event = parseBlock(block)
      if (event) onEvent(event)
    }
  }
}

function parseBlock(block: string): StreamEvent | null {
  let type = 'message'
  const data: string[] = []
  for (const line of block.split('\n')) {
    if (line.startsWith(':')) continue // heartbeat comment
    const colon = line.indexOf(':')
    const field = colon === -1 ? line : line.slice(0, colon)
    const value = colon === -1 ? '' : line.slice(colon + 1).replace(/^ /, '')
    if (field === 'event') type = value
    else if (field === 'data') data.push(value)
  }
  if (data.length === 0) return null
  try {
    return { type, data: JSON.parse(data.join('\n')) }
  } catch {
    return null
  }
}
