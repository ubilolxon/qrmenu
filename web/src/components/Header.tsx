import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useAuth, useUser } from '../lib/auth'

export default function Header({ children }: { children?: ReactNode }) {
  const { logout } = useAuth()
  const user = useUser()
  return (
    <header className="topbar">
      <Link to="/" className="brand">qrmenu</Link>
      <div className="topbar-middle">{children}</div>
      <span className="muted">{user.name} · {user.role}</span>
      <button className="ghost" onClick={logout}>Sign out</button>
    </header>
  )
}
