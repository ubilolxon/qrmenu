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
internal/http    router, middleware, handlers, DTOs
internal/cache   Redis (public menu cache; never on a write path)
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

## Status

Phase 0 complete: skeleton, config, migrations, health, compose, CI.
Phase 1 next: tenants, users, auth, restaurants CRUD.
