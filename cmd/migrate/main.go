// Command migrate applies database migrations.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate status
//	go run ./cmd/migrate down
//	go run ./cmd/migrate create add_tenants sql
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/joho/godotenv"

	"github.com/ubilolxon/qrmenu/internal/config"
	"github.com/ubilolxon/qrmenu/internal/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Optional: absent in production, where config comes from the environment.
	_ = godotenv.Load()

	if len(os.Args) < 2 {
		return fmt.Errorf("usage: migrate <up|down|status|version|up-to|down-to|create|fix> [args]")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	command, args := os.Args[1], os.Args[2:]
	if err := postgres.Migrate(context.Background(), cfg.DB, command, args...); err != nil {
		return err
	}
	return nil
}
