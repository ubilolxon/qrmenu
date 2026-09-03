# qrmenu

QR code menu SaaS for restaurants. Go API (JSON) + separate SPA, PostgreSQL,
shared-table multi-tenancy.

Full architecture and build plan: `~/work/summary_files/qr-menu-saas-architecture.md`

## Quick start

```bash
cp .env.example .env
make up          # postgres :5433, redis :6379, minio :9000 (console :9001)
make migrate     # apply schema
make run         # API on :8080
```

```bash
curl localhost:8080/healthz   # liveness — no dependencies touched
curl localhost:8080/readyz    # readiness — reports postgres and redis
```

`make help` lists everything else.

## Layout

```
cmd/api          HTTP server
cmd/migrate      goose migration runner
internal/config  environment → validated Config, resolved once at boot
internal/domain  pure business types and rules — imports no infrastructure
internal/postgres  pool, embedded migrations, sqlc output, repositories
internal/http    router, middleware, handlers, DTOs, public page templates
internal/cache   Redis (public menu cache; never on a write path)
internal/money   minor-unit formatting; owns the zero-decimal currency list
internal/qr      QR rendering — PNG, SVG, printable table tent
internal/imaging decode, validate and resize uploads — the only untrusted input
internal/storage object storage (S3/MinIO); the only package that knows a bucket exists
internal/i18n    locale negotiation and fallback for translated fields
internal/realtime  the SSE hub — one goroutine-safe registry per restaurant
internal/ratelimit Redis token bucket for the unauthenticated write endpoints
internal/billing   payment-provider abstraction; `payme/` is the Merchant API
web/             SPA (separate build)
deploy/          Dockerfile, docker-compose
```

The rule that keeps this navigable: **dependencies point inward.** `internal/domain`
imports nothing from `postgres`, `http`, or any vendor SDK. When domain code needs
persistence it declares an interface and `postgres` implements it.

## Health endpoints

`/healthz` is liveness and deliberately touches nothing — a database outage is
never fixed by restarting the API, so it must not cause a restart loop.

`/readyz` is readiness. Postgres failing returns 503 and pulls the instance out
of the load balancer. Redis failing returns 200 with `"status": "degraded"`,
because a cache miss falls through to Postgres and a Redis blip should not take
down a healthy fleet.

## Multi-tenancy

Two levels: a **tenant** is the paying account, a **restaurant** is one of its
branches. Billing counts across a tenant; menus, tables and orders belong to a
restaurant.

Isolation is shared tables with a tenant column. The one failure mode that
matters is a query missing its scope, so:

1. Every repository method takes the scope as its **first argument** — there is
   no `GetItem(ctx, id)`, only `GetItem(ctx, restaurantID, id)`.
2. The scope comes from the verified JWT via middleware, **never** from a
   request body or query parameter.
3. Queries are hand-written SQL (sqlc), so the predicate is visible in review.
4. Postgres RLS is added in Phase 7 as a backstop.

## API

Four surfaces: public (unauthenticated, cached), admin (`/api/v1/admin`, JWT),
staff (`/api/v1/staff`, JWT, long-lived SSE), and `/webhooks` — payment-provider
callbacks, outside `/api` so they are easy to exempt from CORS and rate limiting,
and authenticated by each provider's own scheme rather than by JWT.

### Public

Guest pages live **outside** `/api`, because these URLs get printed on cards and
a person may have to type one.

| Method | Path | Returns |
|---|---|---|
| GET HEAD | `/m/{slug}` | the restaurant's menu as HTML |
| GET HEAD | `/t/{token}` | the same menu, with table context |
| GET HEAD | `/m/{slug}/qr.png` `qr.svg` `qr.card.svg` | QR for the restaurant link |
| GET HEAD | `/t/{token}/qr.png` `qr.svg` `qr.card.svg` | QR for one table |
| GET HEAD | `/api/v1/public/m/{slug}` | the menu as JSON |
| GET HEAD | `/api/v1/public/t/{token}` | the menu as JSON, with table context |
| POST | `/api/v1/public/t/{token}/orders` | place an order for that table |
| GET | `/api/v1/public/t/{token}/orders` | this guest's orders here |
| POST | `/api/v1/public/t/{token}/call-waiter` | `{"kind":"waiter"\|"bill"\|"help"}` |
| GET | `/api/v1/public/orders/{orderID}` | one order, for the guest who placed it |
| GET HEAD | `/api/v1/public/plans` | the pricing page: public plans, and which payment methods this deployment has credentials for |

Item photos arrive in the payload as three sizes plus an intrinsic width, height
and average colour. The page uses the last three to reserve the right box before
the image loads: on a menu of forty dishes, photos arriving one by one otherwise
push the whole list around while someone is reading it.

`?locale=uz|ru|en` overrides the `Accept-Language` header. `?size=` on a QR
endpoint is pixels, clamped to 128–2048 (default 512) — rendering is CPU work,
so an unbounded size would be a cheap way to load the server.

The HTML is plain HTML with inline CSS and **no JavaScript**. A guest on
restaurant wifi wants the menu now; it also works with JS off, reads correctly in
a screen reader, and follows `prefers-color-scheme`.

Responses carry an `ETag`, honour `If-None-Match` with a 304, and set
`Cache-Control: public, max-age=…, stale-while-revalidate=…` and
`Vary: Accept-Language`. Server-side, a rendered menu is cached in Redis under
`menu:{restaurantID}:{locale}:v{menuVersion}` — see the menu data model below for
why the version is a key *suffix* rather than something to invalidate. Every
cache failure path is treated as a miss, never an error.

`qr.card.svg` is a printable 420×560 table tent: the code plus the restaurant and
table names, as a standalone SVG a print shop can use directly. Vector, because a
PNG blown up to A4 goes soft.

Table QR tokens are 14 characters of Crockford base32 (no I, L, O or U, so they
survive being read aloud or copied by hand) and are unique platform-wide, which
is why `/t/{token}` needs no restaurant in the URL. Rotating a token is
owner-only: it invalidates every code already printed for that table.

### Admin

| Method | Path | Auth | Roles |
|---|---|---|---|
| POST | `/auth/register` | — | creates a tenant and its first owner |
| POST | `/auth/login` | — | |
| POST | `/auth/refresh` | refresh cookie | rotates the token |
| POST | `/auth/logout` | refresh cookie | revokes the session family |
| GET | `/me` | bearer | any |
| GET | `/restaurants` | bearer | owner, manager |
| POST | `/restaurants` | bearer | owner |
| GET | `/restaurants/{id}` | bearer | owner, manager |
| PATCH | `/restaurants/{id}` | bearer | owner, manager |
| DELETE | `/restaurants/{id}` | bearer | owner |
| GET | `/billing` | bearer | **owner** |
| POST | `/billing/subscribe` | bearer | **owner** |
| POST | `/billing/cancel` | bearer | **owner** |
| GET | `/billing/invoices` | bearer | **owner** |
| POST | `/billing/invoices/{id}/pay` | bearer | **owner** |
| POST | `/billing/invoices/{id}/void` | bearer | **owner** |

Billing is owner-only, and that is the whole role rule: someone who runs the menu
does not thereby get to change what the account is paying, or mark its bills
settled.

Menu tree, all under `/restaurants/{restaurantID}` and all requiring owner or
manager, except where noted.

| Method | Path | Notes |
|---|---|---|
| GET POST | `/menus` | |
| PUT | `/menus/reorder` | array order is the new order |
| GET PATCH DELETE | `/menus/{menuID}` | |
| GET POST | `/menus/{menuID}/categories` | |
| PUT | `/categories/reorder` | |
| GET PATCH DELETE | `/categories/{categoryID}` | |
| GET POST | `/categories/{categoryID}/items` | |
| PUT | `/items/reorder` | |
| GET PATCH DELETE | `/items/{itemID}` | |
| PUT | `/items/{itemID}/availability` | **also kitchen and waiter** |
| GET POST | `/items/{itemID}/variants` | |
| PATCH DELETE | `/variants/{variantID}` | |
| GET POST | `/items/{itemID}/modifier-groups` | |
| PATCH DELETE | `/modifier-groups/{groupID}` | |
| GET POST | `/modifier-groups/{groupID}/modifiers` | |
| PATCH DELETE | `/modifiers/{modifierID}` | |

Marking a dish sold out is the one menu write that happens mid-service, so
kitchen and waiting staff can do it without being able to change anything else.

Images, also under `/restaurants/{restaurantID}`:

| Method | Path | Notes |
|---|---|---|
| GET POST | `/images` | POST reserves an upload, returns a presigned URL |
| GET | `/images/limits` | size, type and dimension rules, for client-side checks |
| GET DELETE | `/images/{imageID}` | |
| POST | `/images/{imageID}/complete` | verify the bytes and publish |

Attach an image by PATCHing `image_id` onto an item, or `logo_image_id` /
`cover_image_id` onto the restaurant. `clear_image`, `clear_logo` and
`clear_cover` remove one.

Tables, also under `/restaurants/{restaurantID}`:

| Method | Path | Notes |
|---|---|---|
| GET POST | `/tables` | |
| GET PATCH DELETE | `/tables/{tableID}` | |
| POST | `/tables/{tableID}/rotate-token` | **owner only** — breaks printed codes |

### Staff

Waiter and kitchen devices. All four roles reach these routes — the kitchen has
to see orders and the waiting staff have to answer calls — but what each role may
*do* is narrower, and that is enforced per status rather than per route.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/staff/stream?restaurant_id=` | SSE; held open for a shift |
| GET | `/api/v1/staff/orders?restaurant_id=` | the open board, oldest first |
| PATCH | `/api/v1/staff/orders/{orderID}/status` | `{"status":"…","reason":"…"}` |
| GET | `/api/v1/staff/calls?restaurant_id=` | live waiter calls |
| PATCH | `/api/v1/staff/calls/{callID}/status` | `pending → acknowledged → resolved` |

**The stream authenticates with the `Authorization` header, so a browser's
built-in `EventSource` cannot be used** — it cannot set headers. Read it with
`fetch()` and a `ReadableStream`. Passing the token as a query parameter would
write a live credential into every access log, proxy log and `Referer` between
the server and the tablet, and those logs outlive the token.

Events: `ready` (on connect), `resync` (this connection lost events),
`order.created`, `order.updated`, `waiter.called`, `waiter.updated`. Order and
call payloads are byte-for-byte the shape the REST endpoints return, so a client
has one type to handle rather than two.

`ready` and `resync` both mean **load the board now**. The hub keeps no history,
so `Last-Event-ID` cannot be honoured and is ignored; refetching is the only
honest recovery, and making it the answer to both means one code path. Event
`id`s are strictly increasing, so a gap is detectable. A comment heartbeat goes
out every 20s to stop proxies closing an idle connection.

## Table orders

**Every amount is computed by the server.** The client sends item, variant and
modifier ids plus quantities — and nothing else. Unknown fields are rejected
outright, so a request carrying `price_minor` is a 400 rather than something to
be quietly ignored. Trusting a client-sent total is how you get a zero-som order.

Before pricing, each line is checked against the live menu: the dish exists in
*this* restaurant, is in stock, its category is active, and its menu is inside
its serving window right now. That last one matters because the public menu is
deliberately time-independent and cached — a guest's cart may be minutes old and
the breakfast menu may have closed while they were reading it.

The restaurant's clock and its business day both come from Postgres in a single
query, not from Go. Postgres carries the full tzdata; a scratch container running
the API may carry none, and a missing zone in Go silently degrades to UTC — which
would roll the daily order numbers over at 05:00 in Tashkent.

**Snapshots.** An order records the item name (all translations), the variant,
every modifier with its price, the table label and the currency. None of it is
re-derived from the live menu, so repricing a dish tomorrow — or deleting it —
leaves yesterday's receipt intact. `item_id` and `variant_id` are kept for
reporting and go null when the dish is deleted; nothing rendered depends on them.

**Status machine**, as a table in `domain/order.go`:

```
pending → accepted → preparing → ready → served → closed
        ↘ cancelled, from any state before served
```

Once food is on the table, cancelling it is a refund, not a status change.
Three checks apply to every move: the tenant owns the order, the role may set
that status, and the move is legal from where the order is. The last is then
re-applied in the `UPDATE`'s `WHERE` clause, so two waiters tapping at the same
moment produce one update and one 409 rather than both succeeding.

**Idempotency.** Send `Idempotency-Key` on order creation; a replay within 10
minutes returns the original order with **200** instead of creating a second one
with 201. The same distinction applies to the waiter-call button: pressing it
while a call is already live returns that call with 200, and only one live call
per table per kind can exist — five presses must not put five rows on the staff
screen.

**Guest identity** is an opaque 32-byte cookie (`qrmenu_guest`, HttpOnly,
SameSite=Lax), minted on the first order rather than on the menu page. It is the
only credential in the flow, so a mismatch reads as 404 rather than 403 — a 403
would confirm the order exists. It appears in no response body.

Order creation and waiter calls are rate limited per **table** (10/min and 5/min)
rather than per IP: everyone in the restaurant shares one NAT address, so an IP
limit would let the first table's rush silence the room. The limiter fails
**open** — Redis is a cache here as everywhere else, and a restaurant unable to
take any orders because a cache is down is worse than the spam it prevents.

## Plans and billing

A plan is pricing configuration, not code: the limits live in `plans.limits` as
jsonb, so a new lever or a bespoke deal for one customer is a row change rather
than a deploy. `free`, `basic` and `pro` ship in the billing migration, because a
running system with no `free` plan has no valid state for a new tenant —
registration would fail on the foreign key.

**The rule everything else follows: limits are enforced on the write path only.**
A tenant who downgrades, stops paying, or gets suspended keeps serving the menu a
guest is scanning right now. The lever is that they cannot add anything new — not
that the QR code stuck to their table goes dark. Nothing on the public menu path
consults a plan at all.

Enforced today, counted across the whole tenant rather than one restaurant:

| Lever | Checked when |
|---|---|
| `restaurants` | creating a restaurant |
| `menu_items` | creating an item |
| `tables` | creating a table |
| `locales` | creating a restaurant, or PATCHing its locale list |
| `storage_mb` | reserving an image upload |
| `orders_enabled` | a guest placing an order or calling a waiter |
| `staff_users` | advertised on the plan; enforced when staff invites land |

A refusal is **402** with `code: "PLAN_LIMIT"` and the numbers in `details`
(`resource`, `limit`, `current`, `plan_code`), so the SPA can render "3 of 3 used"
next to an upgrade button instead of parsing a sentence.

The guest ordering path is the exception. A diner has no plan, no way to upgrade
one, and no interest in why, so that refusal is rewritten as a plain **403 — "This
table is not taking orders right now."** The real reason goes to the log, where
the operator can find it. A 402 addressed to whoever owes the money is never
addressed to the guest.

Two concurrent creates can both read the same count and both pass, leaving a
tenant one row over their limit. That is deliberate: serialising every write
behind a lock on the subscription row costs far more than the outcome it prevents.

**Subscription states**

```
trialing ──► active ──► past_due ──► suspended
    │           │          │  ▲          │
    │           │          │  └── grace ─┘
    └───────────┴──────────┴──────────────► cancelled
```

`past_due` stays writable — it is the grace window, and locking an account the
moment a card expires punishes the restaurant for a bank's timing. Suspension
comes after, and freezes the tenant's own writes only.

Every tenant has exactly one non-cancelled subscription, created at registration
on the free plan and held by a partial unique index. Making "no subscription"
impossible removes the branch that would otherwise have to be written — and got
wrong — at every limit check.

A daily sweep runs in every instance (the statements only ever move a row out of
the state they select on, so two instances sweeping at once do the work once
between them). It runs at startup too, or a deployment that was down over a
weekend would leave every lapsed subscription active until the next tick.

**Paying**

`POST /billing/subscribe` raises an invoice and returns a checkout. It changes no
plan — the plan moves when the money arrives, and the only things allowed to say
that has happened are a provider callback verified against that provider's own
credentials, or an owner marking the invoice paid by hand.

Invoices snapshot the plan code, amount and currency, for the same reason order
lines snapshot a dish name: what the customer was charged has to survive the next
price change. Numbers are `INV-{year}-{seq}`, allocated as max+1 and made safe by
a unique index, with a retry on collision — the same shape as the daily order
number.

An unchanged plan renews by extending the *current period end*, so a payment that
lands two days late does not shorten the month the customer bought. Any other
plan starts today: someone upgrading mid-month is paying to get the bigger plan
now, not at the end of the one they are leaving. Downgrading to free takes effect
immediately and raises no invoice.

**Providers**

```go
type Provider interface {
    Name() domain.PaymentProvider
    Checkout(ctx, req CheckoutRequest) (CheckoutResult, error)
    Webhook() http.Handler
}
```

A provider talks to a `billing.Ledger`, which the service layer implements, so
adding one means writing a file in `internal/billing` rather than threading a new
dependency through the application. Only providers this deployment has
credentials for are registered, so a missing secret means the method is not
offered rather than offered and broken — and its webhook path 404s instead of
accepting callbacks it cannot verify.

`manual` is payment by human: a transfer that cleared, cash, a comped account. It
ships in the MVP and stays afterwards, because every payment system eventually
needs an override, and having one from the start means the first customer can be
onboarded before any integration is finished. It has no webhook by design —
settlement goes through the authenticated admin API so the payment carries which
person recorded it.

`payme` implements the Uzbek Merchant API at `POST /webhooks/payme`. Three things
about it are worth knowing before touching that file:

- **It is not a webhook.** Payme calls one endpoint with JSON-RPC and asks about a
  transaction it names by id. Every method is a lookup by that id, so idempotency
  is structural: `CreateTransaction` repeated returns the existing transaction,
  and `PerformTransaction` repeated returns the existing settlement without
  renewing anything. That last one is the failure the whole design exists to
  prevent, and it has a test named after it.
- **Amounts are in tiyin.** `internal/money` treats UZS as zero-decimal because
  tiyin are defunct in every shop in the country, but Payme's API is denominated
  in them anyway — so an amount crossing that boundary is multiplied by 100.
  Getting it wrong is a hundredfold mischarge, hence `payme.TiyinPerSom`.
- **Every reply is HTTP 200.** JSON-RPC carries its own error channel and Payme
  reads the body, not the status; a 4xx is read as a transport failure and
  retried forever.

Authentication is HTTP Basic with the fixed username `Paycom`, compared in
constant time on both halves. `click` and `stripe` are named in the schema and
the provider enum but are not implemented.

## Image uploads

Uploading is two steps, and the split is the point.

**Step one** POSTs the content type and size to `/images`. The API writes a
`pending` row, derives a storage key from its id, and returns a presigned PUT
URL. **Step two** is the client uploading the bytes *straight to object storage*
— the API never carries them. A restaurant adding fifty dish photos would
otherwise tie up an API worker for each one, over connections that are slow
precisely because they are phones.

**Step three** POSTs to `/complete`. Only now does the server touch the file: it
fetches the object, decodes it, generates the three sizes, and flips the row to
`ready`.

That decode is not a formality. **A presigned PUT enforces nothing about
content.** Anyone holding a URL signed for `image/jpeg` can upload HTML, and the
bucket is served publicly — so publishing on the strength of a declared content
type would be stored XSS on our own origin. The only way to know a file is an
image is to decode it, and the limits are enforced against what the decoder
actually finds:

- 10 MiB per file, and 40 million pixels *decoded*. The second is not redundant:
  a 20 000 × 20 000 PNG is a few hundred KB on disk and 1.6 GB as a raster, so
  the dimension check is read from the header before the full decode allocates.
- JPEG, PNG, WebP and GIF. **SVG is refused** — it is a document, it can carry
  script, and it would be served from a public bucket.
- At least 100 px on a side, which rejects an icon picked by mistake.

Derivatives are 320 / 800 / 1600 px wide, never upscaled. They come out as JPEG
unless the source has genuinely translucent pixels, in which case PNG — a logo
with a cut-out background must not come back with black corners, and a
photograph must not be paid for at PNG size.

EXIF orientation is applied at decode. A phone does not rotate pixels when you
turn it sideways; it stores a tag and leaves the raster alone. Ignoring that tag
is why uploaded food photos come out on their side, and it has to be corrected
*before* the dimensions are recorded, since orientations 5–8 swap the axes.

A presigned upload that is never completed leaves a pending row and an orphaned
object. Both are swept hourly, after a two-hour grace period comfortably longer
than the 15-minute URL lifetime.

The bucket must be readable by whoever serves the images — a CDN in front of it
in production, anonymous GET locally. `deploy/docker-compose.yml` provisions
that; a bucket the application creates itself at startup gets the same read-only
policy, and an existing bucket's policy is never touched.

## Menu data model

Every table in the tree carries `restaurant_id`, even though it could be reached
by following parent links. That keeps scoping to a single predicate instead of a
four-table join, and lets Phase 3 load a whole menu in a handful of indexed
scans.

The usual risk of that denormalisation — the copy drifting from the parent —
cannot happen here. Each child references its parent with a **composite foreign
key** on `(parent_id, restaurant_id)`, so Postgres itself refuses to let a
category belong to a menu from a different restaurant. The redundancy is
enforced, not merely intended.

Prices are `bigint` minor units; the currency lives on the restaurant.

**UZS is treated as a zero-decimal currency**, so `price_minor` holds whole som.
This is a deliberate divergence from ISO 4217, which assigns UZS an exponent of
2 — but tiyin have been out of circulation for years and nothing is priced in
them. Following the standard would put two zeroes on every price that nobody
types, reads or charges. Read `price_minor` as "the smallest unit this currency
is actually used in", not always the ISO minor unit. The rule lives in one place,
`internal/money`, and is pinned by a test: changing it back would silently
multiply every price on every menu by 100.

Menu availability windows are minutes from midnight, and a window
may wrap midnight — a 22:00–02:00 late menu has `from > to`, and the hours after
midnight belong to the day the window opened.

Any menu write bumps `restaurants.menu_version` **in the same transaction**.
That counter is the public cache key suffix, so a write and its cache
invalidation commit together or not at all. Editing the restaurant itself bumps
it too — its name, phone, address, theme, logo and cover are all part of the
rendered menu, so a change to any of them has to supersede the cached copy.

Images attach through a composite foreign key on `(image_id, restaurant_id)`,
the same trick the menu tree uses, so one restaurant cannot reference another's
photo. The `ON DELETE SET NULL` carries an explicit column list: on a composite
key an unqualified `SET NULL` nulls *every* column in it, which would mean
trying to null `items.restaurant_id` — and, on the restaurant side, the primary
key itself.

## Auth

Access tokens are JWTs valid for 15 minutes, held in memory by the SPA. Refresh
tokens are opaque 32-byte random strings stored only as a SHA-256, delivered in
an `HttpOnly; Secure; SameSite=Strict` cookie scoped to `/api/v1/admin/auth`.
Passwords use argon2id with the parameters encoded in each hash, so they can be
raised later without invalidating existing passwords.

Refresh tokens **rotate**: using one consumes it and issues a replacement in the
same family. Presenting an already-consumed token means it leaked, so the entire
family is revoked — the attacker and the legitimate client are both logged out.
A forced re-login beats a persistent intruder.

Login returns the same error for an unknown address and a wrong password, and
spends the same CPU either way, so the endpoint cannot be used to discover which
addresses are registered.

## Testing

```bash
make test                                   # unit tests only
DATABASE_URL=... go test ./internal/service  # + tenant-isolation suite
```

`internal/service/tenant_isolation_test.go` is the file to keep honest. It runs
against a real Postgres and asserts that one tenant cannot read, modify, or
delete another's rows through any service method — and that the refusal is
"not found" rather than "forbidden", since a 403 would confirm the ID exists.
Fakes cannot catch a missing `WHERE tenant_id`; only a real database can.
**Every new tenant-scoped method needs a case there.**

`internal/http/handler/public_test.go` renders the real template. `html/template`
resolves almost everything at *render* time — a field that does not exist, or a
func-typed field the template tries to call, parses cleanly and then aborts
mid-document, leaving a page that just stops after the heading. Nothing catches
that at compile time, so the test executes the template and checks the output
still ends in `</html>`. **Any change to `menu.gohtml` or the view model should
be covered there**, or the failure mode is a truncated page nobody notices.

`internal/domain/plan_test.go` pins the limit arithmetic without a plan table:
that an absent jsonb key reads as "not allowed" rather than "unlimited", that the
check is inclusive so a tenant holding exactly their limit may not add another,
and that a suspended account is refused whatever room its plan has.

`internal/billing/payme/payme_test.go` drives the real JSON-RPC endpoint over
HTTP against a fake ledger that *counts settlements*. The assertion that matters
is `TestPerformTwiceSettlesOnce`: a duplicated callback must leave that counter
at one. It also pins the tiyin conversion by sending the stored amount unconverted
and requiring a refusal — the hundredfold undercharge, caught in a test rather
than in a reconciliation.

`internal/service/billing_test.go` runs the whole paid path against a real
Postgres, because neither half of a limit check can be faked: the count has to be
a real count and the limits have to be the ones the migration seeded. It also
holds the two rules the billing design is built around:
`TestSuspensionFreezesWritesButNotReads`, and `TestGuestNeverSeesTheUpsell` —
which asserts the diner's refusal mentions no plan, no price and no upgrade.

`internal/domain/order_test.go` pins the pricing rules and the status machine
without a database, because they are pure functions of (menu, cart) by design —
that is what `domain.Catalog` exists for. `internal/realtime/hub_test.go` covers
what a hub gets wrong: a send landing on a closed channel, a wedged subscriber
blocking the publisher, and events crossing between restaurants. Run it with
`-race`, which is how CI runs it.

## Status

- **Phase 0** — skeleton, config, migrations, health, compose, CI.
- **Phase 1** — tenants, users, argon2id passwords, JWT + rotating refresh
  tokens, roles, restaurants CRUD, tenant-isolation suite.
- **Phase 2** — menus, categories, items, variants, modifier groups and
  modifiers; bulk reorder; availability toggle; translated fields throughout.
- **Phase 3** — tables and QR tokens; public menu as HTML and JSON in uz/ru/en;
  QR as PNG, SVG and a printable table tent; Redis cache keyed on `menu_version`;
  ETag/304. This is the first part you can demo on a phone.
- **Phase 4** — image uploads: presigned PUT to object storage, server-side
  validation and resizing, EXIF orientation, photos on the public menu.
- **Phase 5** — table orders: server-side pricing, cart validation against the
  live menu, snapshotted line items, daily order numbers, idempotent placement,
  the status machine, waiter calls, and the staff SSE stream.
- **Phase 6** — plans, limits enforced on the write path, subscriptions and
  invoices, the `manual` provider, the Payme Merchant API, and the daily expiry
  sweep.
- **Phase 7 next** — admin SPA, RLS backstop, metrics, load test, deploy.
