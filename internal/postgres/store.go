package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ubilolxon/qrmenu/internal/domain"
	"github.com/ubilolxon/qrmenu/internal/postgres/gen"
)

// Store is the database entry point. It embeds the generated Queries so
// services call q.GetUser(...) directly, and adds transaction support, which
// sqlc deliberately leaves to the caller.
type Store struct {
	*gen.Queries
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Queries: gen.New(pool), pool: pool}
}

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a transaction, rolling back on error or panic.
//
// The rollback is deferred rather than written on each error path: a panic
// mid-transaction would otherwise hold the connection until the pool reaped it,
// with the transaction still open and its locks still held.
func (s *Store) InTx(ctx context.Context, fn func(q *gen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op that returns
		// pgx.ErrTxClosed, so it is safe to always defer.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(s.Queries.WithTx(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Postgres error codes worth naming.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
)

// MapError translates driver errors into the domain sentinels the HTTP layer
// knows how to render. Everything unrecognised is returned unchanged so it
// surfaces as a 500 and gets logged, rather than being quietly downgraded to a
// friendly-looking 400.
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return domain.Errorf(domain.ErrConflict, "%s", constraintLabel(pgErr.ConstraintName))
		case pgForeignKeyViolation:
			return domain.Errorf(domain.ErrNotFound, "referenced record does not exist")
		case pgCheckViolation:
			return &domain.ValidationError{Fields: []domain.FieldError{{
				Field:   constraintField(pgErr.ConstraintName),
				Message: "violates a database constraint",
			}}}
		}
	}
	return err
}

// constraintLabel turns an index name into something a user can act on. The
// database is the only place a uniqueness race can be detected, so this path is
// reached even when the service checked availability first.
func constraintLabel(constraint string) string {
	switch constraint {
	case "users_email_uniq":
		return "email address is already registered"
	case "restaurants_slug_uniq":
		return "slug is already taken"
	case "refresh_tokens_hash_uniq":
		return "token already exists"
	default:
		return "record already exists"
	}
}

func constraintField(constraint string) string {
	switch constraint {
	case "restaurants_default_locale_listed":
		return "default_locale"
	case "restaurants_locales_not_empty":
		return "locales"
	default:
		return "_"
	}
}
