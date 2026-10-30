import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { useAuth } from '../lib/auth'
import { ErrorBox, Field } from '../components/ui'

export function Login() {
  const { login } = useAuth()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await login(email, password)
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }

  return (
    <div className="auth">
      <form className="card" onSubmit={submit}>
        <h1>Sign in</h1>
        <Field label="Email">
          <input type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
        </Field>
        <Field label="Password">
          <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
        </Field>
        <ErrorBox error={error} />
        <button className="primary" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
        <p className="muted">No account? <Link to="/register">Create one</Link></p>
      </form>
    </div>
  )
}

export function Register() {
  const { register } = useAuth()
  const [form, setForm] = useState({ tenant_name: '', name: '', email: '', password: '' })
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value })

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await register(form)
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }

  return (
    <div className="auth">
      <form className="card" onSubmit={submit}>
        <h1>Create an account</h1>
        <Field label="Business name" hint="The account that pays — one account can own several restaurants.">
          <input value={form.tenant_name} onChange={set('tenant_name')} required />
        </Field>
        <Field label="Your name">
          <input value={form.name} onChange={set('name')} required />
        </Field>
        <Field label="Email">
          <input type="email" autoComplete="email" value={form.email} onChange={set('email')} required />
        </Field>
        <Field label="Password">
          <input type="password" autoComplete="new-password" value={form.password} onChange={set('password')} required />
        </Field>
        <ErrorBox error={error} />
        <button className="primary" disabled={busy}>{busy ? 'Creating…' : 'Create account'}</button>
        <p className="muted">Already have one? <Link to="/login">Sign in</Link></p>
      </form>
    </div>
  )
}
