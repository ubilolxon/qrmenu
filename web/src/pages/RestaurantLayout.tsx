import { NavLink, Navigate, Outlet, useLocation, useOutletContext, useParams } from 'react-router-dom'
import { useUser } from '../lib/auth'
import { t } from '../lib/format'
import type { Restaurant } from '../lib/types'
import Header from '../components/Header'
import { ErrorBox } from '../components/ui'
import { useRestaurants } from './Restaurants'

export default function RestaurantLayout() {
  const user = useUser()
  const canEdit = user.role === 'owner' || user.role === 'manager'
  return canEdit ? <ManagerLayout /> : <StaffLayout />
}

function ManagerLayout() {
  const { restaurantID } = useParams()
  const { data, error } = useRestaurants()
  const restaurant = data?.find((r) => r.id === restaurantID)

  return (
    <>
      <Header>
        {restaurant && (
          <nav className="tabs">
            <strong>{t(restaurant.name, restaurant.default_locale)}</strong>
            <NavLink to="menu">Menu</NavLink>
            <NavLink to="tables">Tables & QR</NavLink>
            <NavLink to="orders">Orders</NavLink>
            <a href={`/m/${restaurant.slug}`} target="_blank" rel="noreferrer">Public menu ↗</a>
          </nav>
        )}
      </Header>
      <ErrorBox error={error} />
      {data && !restaurant && <div className="page empty">Restaurant not found.</div>}
      {restaurant && <Outlet context={restaurant} />}
    </>
  )
}

// Waiters and kitchen staff cannot read restaurant settings (GET /restaurants
// is owner/manager only), so they reach the board through a direct link and
// get no restaurant context. The board needs none: every order carries its own
// currency and locale.
function StaffLayout() {
  const { pathname } = useLocation()
  if (!pathname.endsWith('/orders')) return <Navigate to="orders" replace />
  return (
    <>
      <Header>
        <nav className="tabs"><strong>Orders</strong></nav>
      </Header>
      <Outlet context={null} />
    </>
  )
}

// Only valid under ManagerLayout — the menu and table pages.
export function useRestaurant() {
  return useOutletContext<Restaurant>()
}
