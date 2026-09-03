-- Creates the role the application connects as, and grants it exactly what it
-- needs and nothing more.
--
-- This is separate from the migrations on purpose. A role is cluster-level
-- state, not schema state: it is shared by every database on the server, it
-- carries a password that has no business being in a version-controlled .sql
-- file, and rolling a migration back should not delete the account the service
-- logs in with.
--
-- **Why it matters that this role is not the owner.** Row-level security is
-- ignored outright by a superuser or any role with BYPASSRLS, and it is ignored
-- by a table's owner unless the table is set to FORCE (migration 00008 does set
-- it). The standard Postgres image creates POSTGRES_USER as a superuser — so
-- the obvious way to run this service is the way that silently disables its
-- last line of defence, with pg_policies still listing every policy. The API
-- checks at boot and refuses to start outside dev; this script is the fix.
--
-- Run it as the database owner, after migrations:
--
--   psql "$DATABASE_URL" -v app_password="'…'" -f deploy/postgres/app-role.sql
--
-- Then point the application at the same database as qrmenu_app.

\set ON_ERROR_STOP on

-- +--------------------------------------------------------------------------+
-- | The role                                                                 |
-- +--------------------------------------------------------------------------+

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'qrmenu_app') THEN
        CREATE ROLE qrmenu_app LOGIN;
    END IF;
END;
$$;

-- NOSUPERUSER and NOBYPASSRLS are stated rather than assumed: this script may
-- be re-run against a role someone has since granted more to.
ALTER ROLE qrmenu_app NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;

\if :{?app_password}
ALTER ROLE qrmenu_app PASSWORD :'app_password';
\endif

-- +--------------------------------------------------------------------------+
-- | Grants                                                                   |
-- +--------------------------------------------------------------------------+

GRANT USAGE ON SCHEMA public TO qrmenu_app;
-- app.current_tenant() is called by every policy, so the role must be able to
-- reach the schema holding it.
GRANT USAGE ON SCHEMA app TO qrmenu_app;

-- Data only. No CREATE, no TRUNCATE, no REFERENCES: the application reads and
-- writes rows, and everything structural is a migration run by the owner.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO qrmenu_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO qrmenu_app;

-- The migration that adds the next table runs as the owner, and without this
-- the application would be unable to see it until someone remembered to re-run
-- the grant above. That is a deploy that passes CI and fails in production.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO qrmenu_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO qrmenu_app;

-- goose's own bookkeeping table is written by the migration runner, which
-- connects as the owner. The application never touches it.
REVOKE ALL ON TABLE goose_db_version FROM qrmenu_app;
