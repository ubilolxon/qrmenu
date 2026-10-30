# QR Menu SaaS — Architecture Plan (Go)

> **Note:** this is the original design plan, written before Phase 0. Some details changed
> during the build (for example, image uploads use presigned URLs plus a server-side
> verify step). The README describes what is actually implemented.

**Date:** 2026-08-16
**Stack decision:** Go API (JSON) + separate SPA · PostgreSQL · shared tables with `tenant_id`
**Day-one features:** multi-language menus, image uploads, table orders, billing/plans

---

## 1. Product shape (what we're actually building)

Three distinct audiences, three distinct API surfaces. Getting this separation right up front is
the single most important architectural decision — it drives auth, caching, and rate limiting.

| Surface | Audience | Auth | Traffic profile |
|---|---|---|---|
| **Public API** | Guest scanning a QR at a table | none (QR token) | 95% of requests, read-heavy, cacheable, spiky at meal times |
| **Admin API** | Restaurant owner/manager in the SPA | JWT | low volume, write-heavy, latency-tolerant |
| **Staff API** | Waiter/kitchen tablet | JWT | long-lived SSE connections, low volume |

The public surface must be treated like a CDN-fronted static asset that happens to be JSON.
It is the thing that will fall over on a Friday night, and it is the thing that sells the product
(a menu that loads in 300ms on bad restaurant wifi).

---

## 2. Tenancy model

**Two levels, and they are not the same thing.**

```
tenant  (the paying account — "Chaykhana Group LLC")
  └── restaurant (a branch — "Chaykhana, Chilanzar")
        └── tables, menus, orders
```

- **Billing is per tenant.** Plan limits count across all of a tenant's restaurants.
- **Content is per restaurant.** Menus, tables, and orders never cross a restaurant boundary.

Separating these on day one is cheap. Retrofitting a `tenants` table onto a schema where
`restaurant` *was* the account is a multi-week migration — chains are a core customer segment,
so assume them from the start.

### Isolation strategy: shared tables + `tenant_id`

The chosen model. One database, one schema, every row carries its owner. Simple to migrate,
simple to back up, scales to thousands of tenants on one Postgres box.

The only real risk is **a forgotten `WHERE tenant_id = ?`** leaking one restaurant's data into
another's response. Four layers of defence, in order of how much they actually help:

1. **Repository signatures make it unforgettable.** Every method takes the scope as its first
   argument. There is no `GetItem(ctx, id)` — only `GetItem(ctx, restaurantID, id)`. If a
   function can't be called without the tenant, it can't be called wrong.
2. **The scope comes from the token, never from the request body.** Middleware pulls
   `tenant_id`/`restaurant_id` out of the verified JWT and puts it in `context.Context`.
   A handler that reads a tenant ID from JSON is a bug — enforce it in code review.
3. **Every SQL query is written by hand (sqlc)**, so the predicate is visible in a `.sql` file
   and shows up in diffs. No ORM auto-scoping magic that silently stops applying.
4. **Postgres RLS as a backstop, added in Phase 7.** Once the app is stable, enable row-level
   security with `SET LOCAL app.tenant_id` per transaction. It costs a small amount of plumbing
   and turns "we leaked data" into "the query returned zero rows." Do it *after* the app works —
   RLS while you're still moving fast makes every bug harder to debug.

---

## 3. Technology choices

| Concern | Choice | Why |
|---|---|---|
| HTTP router | `chi/v5` | stdlib-compatible `http.Handler`, real sub-routers and middleware groups. Go 1.22 `ServeMux` is fine but you'll rebuild chi's middleware chain by hand. |
| DB driver | `pgx/v5` (native, not `database/sql`) | Faster, real Postgres types (jsonb, arrays, `numeric`), proper connection pool stats. |
| Queries | **sqlc** | You write SQL, it generates typed Go. No ORM surprises, no N+1 by accident, queries are reviewable. Avoid GORM — the tenant-scoping risk above gets much worse with implicit query building. |
| Migrations | **goose** | Plain `.sql` up/down files, embeddable via `embed.FS`, no separate binary needed in prod. |
| Validation | `go-playground/validator/v10` | Struct-tag validation on DTOs. |
| Auth | JWT (access) + rotating refresh tokens in DB | See §6. |
| QR codes | `yeqown/go-qrcode/v2` | Supports logo embedding and colors — branded QR is a paid-tier feature. `skip2/go-qrcode` if you only ever need plain black-and-white. |
| Images | `disintegration/imaging` + `chai2010/webp` | Pure Go, no cgo, no libvips dependency in the container. |
| Object storage | `minio-go/v7` | S3-compatible client. MinIO in dev, any S3 provider in prod — same code. |
| Cache | Redis (`redis/go-redis/v9`) | Public menu cache + rate limiting. Optional in Phase 0–2; add at Phase 3. |
| Realtime | **SSE**, not WebSocket | Kitchen/waiter screens only need server→client. SSE is plain HTTP, auto-reconnects natively, survives proxies, and needs no extra library. |
| Logging | `log/slog` (stdlib) | Structured JSON in prod, text in dev. No third-party logger needed in 2026. |
| Config | env vars via `caarlos0/env` or hand-rolled | 12-factor. No config files in the image. |
| Testing | `testcontainers-go` + real Postgres | Do not mock the database. Integration tests against a real PG container catch the tenant-scoping bugs that unit tests never will. |

---

## 4. Data model

### Money
Every price is `bigint` **minor units** (`price_minor`) plus an ISO currency code on the restaurant.
Never `float`. `12.30 USD` is `1230`. Formatting is the SPA's job.

### Multi-language: JSONB per field

```sql
name        jsonb NOT NULL,   -- {"uz": "Osh", "ru": "Плов", "en": "Plov"}
description jsonb
```

**Why JSONB over a `translations` table:** adding a language is a data change, not a schema
change; the whole menu loads in one query with no joins; and the public menu is served from
cache anyway so the "can't index it well" objection barely applies.

**The tradeoff, stated honestly:** cross-language full-text search is awkward
(`jsonb_each_text` + a GIN index on a generated tsvector column), and there's nothing stopping
a bad write from producing `{"ru": null}`. Mitigate with a `i18n.Text` Go type that validates on
marshal and implements fallback: requested locale → restaurant default locale → first non-empty.

Each restaurant stores `default_locale text` and `locales text[]` so the SPA knows which language
tabs to render in the editor.

### Tables (abbreviated — full DDL goes in `internal/postgres/migrations/`)

```
tenants           id, name, owner_user_id, status, created_at
users             id, tenant_id, email, password_hash, role, last_login_at
                  role ∈ (owner, manager, waiter, kitchen)

restaurants       id, tenant_id, slug UNIQUE, name jsonb, logo_image_id,
                  currency, timezone, default_locale, locales[],
                  theme jsonb, menu_version int, is_active

restaurant_tables id, restaurant_id, label, qr_token UNIQUE, seats, is_active
menus             id, restaurant_id, name jsonb, is_active, available_from, available_to, days[]
categories        id, restaurant_id, menu_id, name jsonb, image_id, sort_order
items             id, restaurant_id, category_id, name jsonb, description jsonb,
                  price_minor, image_id, is_available, sort_order,
                  tags[], allergens[], calories, prep_minutes

item_variants     id, item_id, name jsonb, price_delta_minor, sort_order
modifier_groups   id, item_id, name jsonb, min_select, max_select, is_required
modifiers         id, group_id, name jsonb, price_delta_minor

images            id, tenant_id, storage_key, mime, width, height,
                  variants jsonb, blurhash, bytes

orders            id, restaurant_id, table_id, code, status, guest_session_id,
                  subtotal_minor, service_fee_minor, total_minor, note,
                  placed_at, accepted_at, closed_at
order_items       id, order_id, item_id, variant_id, qty, unit_price_minor,
                  modifiers jsonb, name_snapshot jsonb
waiter_calls      id, restaurant_id, table_id, kind, status, created_at

plans             code PK, name, price_minor, currency, interval, limits jsonb
subscriptions     id, tenant_id, plan_code, status, current_period_end,
                  provider, provider_ref, cancel_at_period_end
invoices          id, tenant_id, subscription_id, amount_minor, status, paid_at
```

Two details that are easy to miss and expensive to add later:

- **`order_items` snapshots price and name.** When the owner raises the price of Plov tomorrow,
  yesterday's order must still show what the guest actually paid. Never join historical orders to
  live `items` for price or name.
- **`restaurants.menu_version`** is an integer bumped on *any* menu write. It's the cache key
  suffix — see §7. This is far more reliable than trying to invalidate individual keys.

### QR tokens
The QR encodes `https://menu.example.com/t/{qr_token}`, where `qr_token` is a random 12-char
base32 string — **not** the table's numeric ID. Sequential IDs let anyone enumerate every table
in every restaurant on the platform and place orders against them.

---

## 5. Project layout

```
qrmenu/
├── cmd/
│   ├── api/main.go          # HTTP server
│   ├── worker/main.go       # image processing, billing renewals, cleanup
│   └── migrate/main.go      # goose runner
├── internal/
│   ├── config/              # env → Config struct, validated at boot
│   ├── domain/              # pure types + business rules, zero infra imports
│   │   ├── menu/  order/  tenant/  billing/
│   ├── postgres/
│   │   ├── migrations/      # goose .sql files (embedded)
│   │   ├── queries/         # sqlc .sql sources
│   │   ├── gen/             # sqlc output — do not edit
│   │   └── *_repo.go        # repo impls satisfying domain interfaces
│   ├── http/
│   │   ├── router.go
│   │   ├── handler/         # public/, admin/, staff/, webhook/
│   │   ├── middleware/      # auth, tenant scope, ratelimit, recover, reqid, cors
│   │   └── dto/             # request/response shapes — never expose domain structs
│   ├── auth/                # jwt, argon2id, refresh rotation
│   ├── storage/             # Storage interface + s3 impl
│   ├── imaging/             # resize/encode pipeline
│   ├── qr/
│   ├── billing/             # Provider interface + stripe/, payme/, manual/
│   ├── realtime/            # SSE hub
│   └── i18n/                # i18n.Text type + fallback
├── web/                     # SPA lives here (separate build)
├── deploy/                  # Dockerfile, compose, nginx, systemd
└── Makefile
```

**The rule that keeps this clean:** `internal/domain` imports nothing from `internal/postgres`,
`internal/http`, or any third-party SDK. Dependencies point inward. If `domain/order` needs to
save something, it declares an interface and `postgres` implements it. This is what makes the
order-pricing logic testable without a database.

---

## 6. Auth

**Admin/staff SPA:**
- Access token: JWT, 15 min, contains `user_id`, `tenant_id`, `role`. Held in memory by the SPA.
- Refresh token: opaque 32-byte random, stored **hashed** in `refresh_tokens`, 30 days,
  `HttpOnly; Secure; SameSite=Strict` cookie. **Rotated on every use** — if an old token is
  replayed, revoke the whole family (that's a stolen-token signal).
- Passwords: **argon2id** (`golang.org/x/crypto/argon2`), not bcrypt.

**Guests (public menu):** no login. A `guest_session_id` cookie ties a guest to their own orders
so they can see status. Ordering is rate-limited per `qr_token` + IP.

**Roles:** `owner` (billing + everything), `manager` (menu + orders, no billing),
`waiter` (orders only), `kitchen` (order status only). Enforce with a
`RequireRole(...)` middleware, and check it in the handler layer — never only in the SPA.

---

## 7. The public menu path (the hot path)

```
GET /api/v1/public/t/{qr_token}     → restaurant + table + full menu, one response
GET /api/v1/public/m/{slug}         → same, without table context (takeaway / browse)
```

One request returns everything: restaurant branding, locales, categories, items, variants,
modifiers, image URLs. No waterfall of requests from a phone on 3G.

**Caching, three layers:**

1. **Redis:** key `menu:{restaurant_id}:{locale}:v{menu_version}`, value = the fully rendered
   JSON bytes. Because the version is *in the key*, a menu write just bumps the counter — no
   invalidation logic, no stale-key hunting, and old keys expire on their own TTL.
2. **ETag + `Cache-Control: public, max-age=60, stale-while-revalidate=300`** so repeat scans at
   the same table hit the browser cache.
3. **CDN** in front, keyed on the same URL. At this point your origin sees almost no traffic.

Serve `?locale=ru` as a separate cache entry rather than sending all languages and filtering
client-side — a 4-language menu with descriptions is 4× the bytes for no benefit.

---

## 8. Images

Upload goes **through the API**, not via presigned direct-to-S3. You need to validate the file,
strip EXIF (guest-facing photos with GPS metadata is a real privacy problem), enforce plan
storage limits, and generate derivatives. Presigned uploads skip all of that.

Pipeline, run in `cmd/worker` off a queue (or inline for MVP — it's ~200ms):

1. Accept ≤8MB, sniff real content type from magic bytes (**not** the `Content-Type` header or
   the filename), allow JPEG/PNG/WebP/HEIC only.
2. Decode, auto-orient from EXIF, then **discard all metadata**.
3. Resize to `thumb 200w`, `card 600w`, `full 1200w`. Never upscale.
4. Encode WebP (quality 82) + JPEG fallback.
5. Compute a blurhash for the SPA's placeholder — cheap, and it's the difference between a menu
   that feels fast and one that flashes grey boxes.
6. Store under `tenants/{tenant_id}/images/{image_id}/{variant}.{ext}`; record variants in
   `images.variants` jsonb.

Serve via CDN with a 1-year cache — the key contains the immutable image ID, so files are never
overwritten, only replaced by a new ID.

---

## 9. Table orders

**Flow:** guest scans → menu → cart (client-side) → `POST /public/t/{qr_token}/orders` →
order lands on the kitchen/waiter screen over SSE.

```
POST /api/v1/public/t/{qr_token}/orders
POST /api/v1/public/t/{qr_token}/call-waiter
GET  /api/v1/public/orders/{id}          # guest polls own order status
GET  /api/v1/staff/stream                # SSE: order.created, order.updated, waiter.called
PATCH /api/v1/staff/orders/{id}/status
```

**Server-side price calculation, always.** The client sends item IDs, variant IDs, modifier IDs
and quantities — never prices. The server looks up current prices, verifies each item belongs to
this restaurant and `is_available`, validates modifier group min/max, computes the total, and
snapshots it into `order_items`. Trusting a client-sent total is how you get ₹0 orders.

**Status machine:** `pending → accepted → preparing → ready → served → closed`, plus
`cancelled` from any pre-`served` state. Encode the legal transitions in
`domain/order` as a map and reject everything else — this is where sloppy code produces orders
that go backwards.

**Idempotency:** the guest's phone will double-tap and lose signal. Require an
`Idempotency-Key` header on order creation, keep it unique per table for 10 minutes.

**SSE hub:** one goroutine-safe registry of `map[restaurantID][]chan Event`. Each connection gets
a buffered channel; **drop events on a full buffer rather than blocking the publisher** — one
frozen tablet must not stall the kitchen. Send a heartbeat comment every 20s to defeat proxy
timeouts. If you later run more than one API instance, publish through Redis pub/sub so an order
created on instance A reaches a tablet connected to instance B.

---

## 10. Billing & plans

```go
type Limits struct {
    Restaurants   int   // -1 = unlimited
    MenuItems     int
    StaffUsers    int
    StorageMB     int
    Locales       int
    OrdersEnabled bool
    CustomBrand   bool
    RemoveWatermark bool
}
```

Stored as `plans.limits` jsonb, so pricing experiments don't need a deploy.

Enforcement lives in the **service layer**, not the handler, and is checked on the *write path*
only: `if err := s.plan.CanAddItem(ctx, tenantID); err != nil`. Never enforce limits on reads —
if someone downgrades, their existing menu must keep serving guests. Downgrade should make
things read-only, never break the QR code a customer is scanning right now.

**Payment provider abstraction:**

```go
type Provider interface {
    Checkout(ctx, tenant, plan) (redirectURL string, err error)
    HandleWebhook(ctx, r *http.Request) (Event, error)
    Cancel(ctx, subscriptionRef string) error
}
```

Three implementations: `manual` (mark paid from admin — ship with this in the MVP),
`payme`/`click` (Uzbekistan), `stripe` (international). Webhooks must be signature-verified and
**idempotent by provider event ID** — every provider retries, and a double-applied renewal is a
support ticket.

A `worker` cron checks daily for `current_period_end < now()` and moves subscriptions to
`past_due` → `suspended` after a grace period.

---

## 11. Cross-cutting

**Observability**
- `slog` JSON to stdout, with `request_id`, `tenant_id`, `user_id` on every line.
- `/healthz` (liveness, no deps) and `/readyz` (pings DB + Redis) — keep them separate or a
  blipping Redis will make Kubernetes kill healthy pods.
- Prometheus: request duration by route/status, DB pool saturation, cache hit ratio, SSE
  connection count. Cache hit ratio on the public menu is your single best health signal.

**Rate limiting** (Redis token bucket): public menu 60/min per IP; order creation 10/min per
`qr_token`; login 5/min per IP **and** per email — the per-email limit is what stops credential
stuffing.

**Errors:** one `APIError{code, message, details}` shape everywhere. `code` is a stable machine
string (`ITEM_NOT_AVAILABLE`), `message` is human text the SPA can show. Never leak Postgres
errors to the client.

**Graceful shutdown:** `signal.NotifyContext` → `srv.Shutdown(ctx)` with a 30s deadline, close
SSE channels first so tablets reconnect cleanly.

**Testing:** integration tests against a real Postgres via testcontainers, with one dedicated
suite that asserts *every* repository method refuses to return another tenant's rows. That suite
is the highest-value test file in the project.

---

## 12. Build order

| Phase | Deliverable | Notes |
|---|---|---|
| **0** | Repo skeleton, config, Docker compose (PG + MinIO + Redis), goose migrations, `/healthz`, Makefile, CI | Nothing interesting, but it makes every later phase fast |
| **1** | Tenants, users, auth (register/login/refresh), roles, restaurants CRUD | The tenant-scoping test suite starts here |
| **2** | Menus, categories, items, variants, modifiers + `i18n.Text` | Admin API complete for menu editing |
| **3** | Public menu endpoint, Redis cache + `menu_version`, ETag, QR generation & download (PNG/SVG/PDF sheet) | **First demoable milestone — a real scannable menu** |
| **4** | Image upload pipeline, MinIO/S3, variants, blurhash | |
| **5** | Orders, cart validation, status machine, SSE staff stream, waiter calls | |
| **6** | Plans, limits enforcement, `manual` provider, then Payme/Stripe + webhooks | |
| **7** | RLS backstop, rate limiting, metrics, structured logs, load test, deploy | |

Phase 3 is the one to aim at. A working QR that opens a real menu on a phone is what you show
people — everything before it is plumbing, everything after it is features.

---

## 13. Open questions to settle before Phase 6

- **Payment provider:** Uzbekistan (Payme/Click) or international (Stripe)? Changes Phase 6
  entirely, but nothing before it.
- **Custom domains** per restaurant (`menu.chaykhana.uz`)? If yes, plan for ACME/Let's Encrypt
  automation and a `domains` table early — it affects the routing layer.
- **Offline-capable menu** (service worker / PWA)? Worth it in venues with bad wifi; it's an SPA
  concern but the API must send cache-friendly, versioned responses for it to work.
- **Analytics** for owners (scans per table, top items, peak hours)? Cheap to add if you log scan
  events from day one; expensive to backfill later. Recommend logging scans in Phase 3 even if
  nothing reads them yet.
