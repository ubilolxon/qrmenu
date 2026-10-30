import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { onUnauthenticated, post, refreshSession, setAccessToken } from './api'
import type { Session, User } from './types'

type State = { status: 'loading' } | { status: 'out' } | { status: 'in'; user: User }

interface Auth {
  state: State
  login: (email: string, password: string) => Promise<void>
  register: (input: { tenant_name: string; name: string; email: string; password: string }) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<Auth | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<State>({ status: 'loading' })
  const queryClient = useQueryClient()

  const signedIn = useCallback((session: Session) => {
    setAccessToken(session.access_token)
    setState({ status: 'in', user: session.user })
  }, [])

  const signedOut = useCallback(() => {
    setAccessToken(null)
    queryClient.clear()
    setState({ status: 'out' })
  }, [queryClient])

  // A page load has no access token; the refresh cookie, if any, is the session.
  useEffect(() => {
    onUnauthenticated(signedOut)
    refreshSession().then((s) => (s ? signedIn(s) : setState({ status: 'out' })))
  }, [signedIn, signedOut])

  const value: Auth = {
    state,
    login: async (email, password) => signedIn(await post<Session>('/api/v1/admin/auth/login', { email, password })),
    register: async (input) => signedIn(await post<Session>('/api/v1/admin/auth/register', input)),
    logout: async () => {
      try {
        await post('/api/v1/admin/auth/logout')
      } finally {
        signedOut()
      }
    },
  }

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth outside AuthProvider')
  return ctx
}

export function useUser(): User {
  const { state } = useAuth()
  if (state.status !== 'in') throw new Error('useUser outside a signed-in route')
  return state.user
}
