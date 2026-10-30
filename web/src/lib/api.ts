import type { Session } from './types'

// The access token lives in memory only. The refresh token is an HttpOnly
// cookie the browser sends to /api/v1/admin/auth/* — JavaScript never sees it.
let accessToken: string | null = null
let onSessionLost: () => void = () => {}

export function setAccessToken(token: string | null) {
  accessToken = token
}

export function getAccessToken() {
  return accessToken
}

export function onUnauthenticated(fn: () => void) {
  onSessionLost = fn
}

export interface FieldError {
  field: string
  message: string
}

// Mirrors the server's {"error": {code, message, details}} envelope.
export class ApiError extends Error {
  status: number
  code: string
  details: unknown

  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message)
    this.status = status
    this.code = code
    this.details = details
  }

  // Validation failures list their fields; everything else is one sentence.
  get fieldErrors(): FieldError[] {
    return this.code === 'VALIDATION_FAILED' && Array.isArray(this.details)
      ? (this.details as FieldError[])
      : []
  }
}

export function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    const fields = err.fieldErrors
    if (fields.length > 0) return fields.map((f) => `${f.field}: ${f.message}`).join('; ')
    return err.message
  }
  if (err instanceof Error) return err.message
  return 'Something went wrong.'
}

const AUTH = '/api/v1/admin/auth'

// Refresh is single-flight. Refresh tokens rotate on every use and a replayed
// one revokes the whole session family, so two concurrent refreshes (React
// StrictMode mounts effects twice; two queries can 401 at once) must share
// one request rather than race each other into a logout.
let refreshing: Promise<Session | null> | null = null

export function refreshSession(): Promise<Session | null> {
  refreshing ??= (async () => {
    try {
      const res = await fetch(`${AUTH}/refresh`, { method: 'POST', credentials: 'same-origin' })
      if (!res.ok) {
        accessToken = null
        return null
      }
      const session = (await res.json()) as Session
      accessToken = session.access_token
      return session
    } finally {
      refreshing = null
    }
  })()
  return refreshing
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await send(method, path, body)

  // One retry after a refresh: the access token lasts 15 minutes, so a 401 on
  // an ordinary call usually just means it expired.
  if (res.status === 401 && !path.startsWith(AUTH)) {
    const session = await refreshSession()
    if (!session) {
      onSessionLost()
      throw await toError(res)
    }
    return parse<T>(await send(method, path, body))
  }
  return parse<T>(res)
}

function send(method: string, path: string, body?: unknown) {
  const headers: Record<string, string> = {}
  if (accessToken) headers.Authorization = `Bearer ${accessToken}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  return fetch(path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  })
}

async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) throw await toError(res)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

async function toError(res: Response): Promise<ApiError> {
  try {
    const { error } = await res.json()
    return new ApiError(res.status, error.code, error.message, error.details)
  } catch {
    return new ApiError(res.status, 'HTTP_' + res.status, res.statusText || 'Request failed.')
  }
}

export const get = <T>(path: string) => api<T>('GET', path)
export const post = <T>(path: string, body?: unknown) => api<T>('POST', path, body ?? {})
export const patch = <T>(path: string, body: unknown) => api<T>('PATCH', path, body)
export const put = <T>(path: string, body: unknown) => api<T>('PUT', path, body)
export const del = (path: string) => api<void>('DELETE', path)

export const admin = (path: string) => `/api/v1/admin${path}`
export const inRestaurant = (restaurantID: string, path = '') =>
  admin(`/restaurants/${restaurantID}${path}`)
