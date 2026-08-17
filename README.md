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
internal/i18n    locale negotiation and fallback for translated fields
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

Three surfaces: public (unauthenticated, cached), admin (`/api/v1/admin`, JWT),
and staff (Phase 5).

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

Tables, also under `/restaurants/{restaurantID}`:

| Method | Path | Notes |
|---|---|---|
| GET POST | `/tables` | |
| GET PATCH DELETE | `/tables/{tableID}` | |
| POST | `/tables/{tableID}/rotate-token` | **owner only** — breaks printed codes |

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
invalidation commit together or not at all.

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

## Status

- **Phase 0** — skeleton, config, migrations, health, compose, CI.
- **Phase 1** — tenants, users, argon2id passwords, JWT + rotating refresh
  tokens, roles, restaurants CRUD, tenant-isolation suite.
- **Phase 2** — menus, categories, items, variants, modifier groups and
  modifiers; bulk reorder; availability toggle; translated fields throughout.
- **Phase 3** — tables and QR tokens; public menu as HTML and JSON in uz/ru/en;
  QR as PNG, SVG and a printable table tent; Redis cache keyed on `menu_version`;
  ETag/304. This is the first part you can demo on a phone.
- **Phase 4 next** — image uploads (S3-compatible, presigned PUT).
