package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	// Registers the "pgx" database/sql driver used for migrations.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ubilolxon/qrmenu/internal/config"
)

// Migrations are embedded so the deployed binary carries its own schema and no
// separate migration image or mounted volume is needed in production.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs the given goose command ("up", "down", "status", "version",
// "up-to", ...) against the configured database.
//
// goose speaks database/sql, so this opens a short-lived stdlib connection
// rather than reusing the pgx pool.
func Migrate(ctx context.Context, cfg config.DB, command string, args ...string) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	if err := goose.RunContext(ctx, command, db, "migrations", args...); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}
