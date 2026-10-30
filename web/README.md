# qrmenu admin

The restaurant-side SPA: React + TypeScript + Vite, TanStack Query for API state.

```bash
npm install
npm run dev        # http://localhost:5173, proxies /api, /m/*, /t/* to the Go API on :8080
npm run build      # type-check + production bundle in dist/
```

Point the proxy elsewhere with `QRMENU_API=http://host:port npm run dev`.

## Screens

- **Sign in / register** — registering creates the tenant and its owner.
- **Restaurants** — list and create (owner only).
- **Menu** — menus, categories and dishes; translations per restaurant locale;
  one-tap sold-out toggle; photo upload.
- **Tables & QR** — per-table QR codes, printable cards, token rotation (owner only).
- **Orders** — live board over the staff SSE stream, waiter and bill calls.

## Decisions

- **Access token in memory, refresh token in an HttpOnly cookie.** The SPA never
  sees the refresh token. The dev proxy keeps everything same-origin, which the
  `SameSite=Strict` cookie requires.
- **Refresh is single-flight** (`src/lib/api.ts`). Refresh tokens rotate and a
  replayed one revokes the whole session family, so two concurrent refreshes —
  React StrictMode alone causes one — must share one request.
- **The order stream uses `fetch()`, not `EventSource`** (`src/lib/sse.ts`),
  because the stream authenticates with an `Authorization` header. On `ready`
  and `resync` the board is reloaded; the server keeps no history to replay.
- **Prices are entered in major units** and converted with the same
  zero-decimal currency list as `internal/money` (UZS has no tiyin).
- **Buttons come from `next_statuses`** on each order, so the board only offers
  moves the server will accept.

## Known gaps

- Waiters and kitchen staff cannot list restaurants (`GET /restaurants` is
  owner/manager only), so they open the board from a direct `/r/{id}/orders` link.
- Not built yet: variants and modifiers, reordering, restaurant settings,
  staff management, billing pages.
