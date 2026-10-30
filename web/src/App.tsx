import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from './lib/auth'
import { Login, Register } from './pages/Auth'
import Restaurants from './pages/Restaurants'
import RestaurantLayout from './pages/RestaurantLayout'
import MenuEditor from './pages/MenuEditor'
import Tables from './pages/Tables'
import Orders from './pages/Orders'

export default function App() {
  const { state } = useAuth()

  if (state.status === 'loading') return <div className="center muted">Loading…</div>

  if (state.status === 'out') {
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/register" element={<Register />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    )
  }

  // Waiters and kitchen staff have no use for the menu editor; their home is the board.
  const staffOnly = state.user.role === 'waiter' || state.user.role === 'kitchen'

  return (
    <Routes>
      <Route path="/" element={<Restaurants />} />
      <Route path="/r/:restaurantID" element={<RestaurantLayout />}>
        <Route index element={<Navigate to={staffOnly ? 'orders' : 'menu'} replace />} />
        <Route path="menu" element={<MenuEditor />} />
        <Route path="tables" element={<Tables />} />
        <Route path="orders" element={<Orders />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
