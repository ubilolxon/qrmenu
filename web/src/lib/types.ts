// Response shapes of the Go admin and staff APIs. These mirror the structs in
// internal/http/handler and internal/domain/order_view.go — change both together.

// A translated field: {"uz": "Osh", "ru": "Плов"}.
export type Text = Record<string, string>

export type Role = 'owner' | 'manager' | 'waiter' | 'kitchen'

export interface User {
  id: string
  email: string
  name: string
  role: Role
  is_active: boolean
  last_login_at: string | null
  created_at: string
}

export interface Session {
  access_token: string
  token_type: 'Bearer'
  expires_at: string
  user: User
}

export interface Restaurant {
  id: string
  slug: string
  name: Text
  description: Text
  currency: string
  timezone: string
  default_locale: string
  locales: string[]
  phone: string | null
  address: string | null
  logo_image_id: string | null
  cover_image_id: string | null
  menu_version: number
  is_active: boolean
}

export interface Menu {
  id: string
  name: Text
  description: Text
  is_active: boolean
  sort_order: number
  available_from_min: number | null
  available_to_min: number | null
  days: number[]
}

export interface Category {
  id: string
  menu_id: string
  name: Text
  description: Text
  is_active: boolean
  sort_order: number
}

export interface Item {
  id: string
  category_id: string
  name: Text
  description: Text
  price_minor: number
  is_available: boolean
  sort_order: number
  tags: string[]
  allergens: string[]
  calories: number | null
  prep_minutes: number | null
  image_id: string | null
}

export interface Image {
  id: string
  status: string
  content_type: string
  byte_size: number
  width: number | null
  height: number | null
  urls: Record<string, string>
  error: string | null
}

export interface ImageUpload {
  image: Image
  upload_url: string
  upload_with_content_type: string
  expires_at: string
  complete_url: string
}

export interface Table {
  id: string
  label: string
  qr_token: string
  menu_url: string
  qr_png_url: string
  qr_svg_url: string
  qr_card_url: string
  seats: number | null
  sort_order: number
  is_active: boolean
}

export type OrderStatus =
  | 'pending'
  | 'accepted'
  | 'preparing'
  | 'ready'
  | 'served'
  | 'closed'
  | 'cancelled'

export interface OrderLine {
  id: string
  name: Text
  variant_name: Text
  modifiers: { id: string; name: Text; price_delta_minor: number }[]
  qty: number
  unit_price_minor: number
  line_total_minor: number
  note: string | null
}

export interface Order {
  id: string
  code: string
  status: OrderStatus
  next_statuses: OrderStatus[]
  table_id: string | null
  table_label: string
  locale: string
  note: string | null
  currency: string
  total_minor: number
  lines: OrderLine[]
  placed_at: string
}

export type CallStatus = 'pending' | 'acknowledged' | 'resolved'

export interface WaiterCall {
  id: string
  table_label: string
  kind: 'waiter' | 'bill' | 'help'
  status: CallStatus
  note: string | null
  created_at: string
}
