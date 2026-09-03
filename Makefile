COMPOSE := docker compose -f deploy/docker-compose.yml
BIN     := bin

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- local dependencies -----------------------------------------------------

.PHONY: up
up: ## Start Postgres, Redis and MinIO
	$(COMPOSE) up -d
	@echo "postgres :5433  redis :6379  minio :9000 (console :9001)"

.PHONY: down
down: ## Stop dependencies (keeps data)
	$(COMPOSE) down

.PHONY: reset
reset: ## Stop dependencies and DELETE all local data
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail dependency logs
	$(COMPOSE) logs -f

## --- database ---------------------------------------------------------------

.PHONY: migrate
migrate: ## Apply all pending migrations
	go run ./cmd/migrate up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	go run ./cmd/migrate down

.PHONY: migrate-status
migrate-status: ## Show migration status
	go run ./cmd/migrate status

.PHONY: migration
migration: ## Create a migration: make migration name=add_tenants
	@test -n "$(name)" || (echo "usage: make migration name=add_tenants" && exit 1)
	go run ./cmd/migrate create $(name) sql

.PHONY: sqlc
sqlc: ## Regenerate typed query code from internal/postgres/queries
	go tool sqlc generate

.PHONY: psql
psql: ## Open a psql shell against the dev database
	$(COMPOSE) exec postgres psql -U qrmenu -d qrmenu

## --- application ------------------------------------------------------------

.PHONY: run
run: ## Run the API against local dependencies
	go run ./cmd/api

.PHONY: build
build: ## Build binaries into ./bin
	@mkdir -p $(BIN)
	go build -trimpath -o $(BIN)/api ./cmd/api
	go build -trimpath -o $(BIN)/migrate ./cmd/migrate

.PHONY: test
test: ## Run all tests with the race detector
	go test -race ./...

.PHONY: lint
lint: ## Vet and format-check
	go vet ./...
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run: gofmt -w ." && exit 1)

.PHONY: tidy
tidy: ## Tidy and verify modules
	go mod tidy
	go mod verify

.PHONY: check
check: lint test ## Everything CI runs

.PHONY: db-role
db-role: ## Create the unprivileged application role (run after migrate)
	@test -n "$(password)" || (echo "usage: make db-role password=secret" && exit 1)
	psql "$(DATABASE_URL)" -v app_password="'$(password)'" -f deploy/postgres/app-role.sql

.PHONY: load
load: ## Load-test the public menu: make load url=http://localhost:8080/api/v1/public/t/TOKEN
	@test -n "$(url)" || (echo "usage: make load url=http://…/api/v1/public/t/TOKEN" && exit 1)
	go run ./cmd/loadtest -url "$(url)" -c 100 -clients 20000 -rps 2000 -d 30s
