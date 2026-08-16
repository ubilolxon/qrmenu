package config

import (
	"strings"
	"testing"
	"time"
)

const validDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", validDSN)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Env != "dev" {
		t.Errorf("Env = %q, want dev", cfg.Env)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 30s", cfg.HTTP.ShutdownTimeout)
	}
	if !cfg.IsDev() {
		t.Error("IsDev() = false, want true")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without DATABASE_URL, want error")
	} else if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error = %v, want it to name DATABASE_URL", err)
	}
}

// Load must report every problem at once; one-error-per-restart makes fixing a
// bad deploy needlessly slow.
func TestLoadReportsAllProblems(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("APP_ENV", "production") // not one of dev|staging|prod
	t.Setenv("DB_MAX_CONNS", "many")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error")
	}
	for _, want := range []string{"DATABASE_URL", "APP_ENV", "DB_MAX_CONNS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

// A wildcard origin plus the credentialed refresh cookie would let any site
// drive the admin API on a logged-in user's behalf.
func TestWildcardOriginRejectedOutsideDev(t *testing.T) {
	t.Setenv("DATABASE_URL", validDSN)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("HTTP_ALLOWED_ORIGINS", "https://app.example.com,*")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted '*' origin in prod, want error")
	}
}

func TestWildcardOriginAllowedInDev(t *testing.T) {
	t.Setenv("DATABASE_URL", validDSN)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("HTTP_ALLOWED_ORIGINS", "*")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() rejected '*' in dev: %v", err)
	}
}

func TestCSVTrimsAndDropsEmpty(t *testing.T) {
	t.Setenv("DATABASE_URL", validDSN)
	t.Setenv("HTTP_ALLOWED_ORIGINS", " http://a.test , ,http://b.test ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"http://a.test", "http://b.test"}
	if len(cfg.HTTP.AllowedOrigins) != len(want) {
		t.Fatalf("AllowedOrigins = %q, want %q", cfg.HTTP.AllowedOrigins, want)
	}
	for i, w := range want {
		if cfg.HTTP.AllowedOrigins[i] != w {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.HTTP.AllowedOrigins[i], w)
		}
	}
}
