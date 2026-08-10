SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

GO ?= go
PODMAN ?= podman
SYSTEMCTL ?= systemctl --user
USER_BINARY := bin/user-service
ORDER_BINARY := bin/order-service
MIGRATE_VERSION := v4.18.3
MIGRATE_BIN := $(CURDIR)/bin/migrate-$(MIGRATE_VERSION)
MIGRATE_PACKAGE := github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
SECRETS_DIR := $(CURDIR)/.secrets
QUADLET_FILES := \
	deploy/quadlet/commerce-postgres.volume \
	deploy/quadlet/commerce-redis.volume \
	deploy/quadlet/commerce-postgres.container \
	deploy/quadlet/commerce-redis.container

.PHONY: help init build run run-user run-order fmt fmt-check vet test test-race test-integration check migrate-install \
	sqlc sqlc-vet secrets quadlet-check quadlet-install infra-up infra-down infra-status infra-logs \
	migrate-up migrate-down migrate-version migrate-create require-env require-secrets

help:
	@printf '%s\n' \
		'init              Create .env and random local secret files' \
		'build             Build user-service and order-service binaries' \
		'run               Run user-service with values from .env' \
		'run-order         Run order-service on 127.0.0.1:8082' \
		'fmt               Format every Go source file' \
		'fmt-check         Fail when any Go source file is unformatted' \
		'vet               Run go vet' \
		'test              Run unit and HTTP tests' \
		'test-race         Run tests with the race detector' \
		'test-integration  Run PostgreSQL integration tests' \
		'check             Run static checks, tests, race tests, and build' \
		'sqlc              Generate typed database code' \
		'migrate-install   Build the pinned golang-migrate CLI' \
		'infra-up          Recreate and start rootless Podman Quadlets' \
		'infra-down        Stop development infrastructure' \
		'infra-status      Show Quadlet service status' \
		'infra-logs        Follow PostgreSQL and Redis journal logs' \
		'migrate-up        Apply all database migrations' \
		'migrate-down      Revert one database migration' \
		'migrate-create    Create a migration; pass NAME=<name>'

init:
	bash scripts/init-local-env.sh

build:
	@mkdir -p bin
	$(GO) build -trimpath -o $(USER_BINARY) ./cmd/user-service
	$(GO) build -trimpath -o $(ORDER_BINARY) ./cmd/order-service

run: run-user

run-user: require-env require-secrets
	@set -a; source ./.env; set +a; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export PASETO_V4_LOCAL_KEY="$$(<'$(SECRETS_DIR)/paseto_v4_local_key')"; \
	exec $(GO) run ./cmd/user-service

run-order: require-env require-secrets
	@set -a; source ./.env; set +a; \
	export SERVICE_NAME='order-service'; \
	export HTTP_ADDR="$${ORDER_HTTP_ADDR:-127.0.0.1:8082}"; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export PASETO_V4_LOCAL_KEY="$$(<'$(SECRETS_DIR)/paseto_v4_local_key')"; \
	exec $(GO) run ./cmd/order-service

fmt:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; gofmt -w $$files

fmt-check:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*')"; \
	unformatted="$$(gofmt -l $$files)"; \
	test -z "$$unformatted" || { printf 'Unformatted files:\n%s\n' "$$unformatted"; exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-integration: require-env require-secrets migrate-up
	@set -a; source ./.env; set +a; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	TEST_DATABASE_URL="$$DATABASE_URL" $(GO) test -count=1 -tags=integration ./...

check: sqlc fmt-check vet test test-race build sqlc-vet quadlet-check

sqlc:
	sqlc generate

sqlc-vet:
	sqlc vet

migrate-install: $(MIGRATE_BIN)

$(MIGRATE_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install -tags postgres '$(MIGRATE_PACKAGE)'
	mv '$(CURDIR)/bin/migrate' '$(MIGRATE_BIN)'

require-env:
	@test -f .env || { printf 'Run make init first.\n' >&2; exit 1; }

require-secrets:
	@test -s '$(SECRETS_DIR)/postgres_password' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/redis_password' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/pgpass' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/redis.conf' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/paseto_v4_local_key' || { printf 'Run make init first.\n' >&2; exit 1; }

secrets: require-secrets
	@$(PODMAN) secret create --replace commerce-postgres-password '$(SECRETS_DIR)/postgres_password' >/dev/null
	@$(PODMAN) secret create --replace commerce-redis-password '$(SECRETS_DIR)/redis_password' >/dev/null
	@$(PODMAN) secret create --replace commerce-redis-config '$(SECRETS_DIR)/redis.conf' >/dev/null

quadlet-check:
	@QUADLET_UNIT_DIRS='$(CURDIR)/deploy/quadlet' \
		/usr/lib/systemd/user-generators/podman-user-generator -user -dryrun >/dev/null

quadlet-install: quadlet-check
	$(PODMAN) quadlet install --replace $(QUADLET_FILES)
	$(SYSTEMCTL) daemon-reload

infra-up: require-secrets quadlet-install
	@$(SYSTEMCTL) stop commerce-postgres.service commerce-redis.service >/dev/null 2>&1 || true
	@$(MAKE) --no-print-directory secrets
	$(SYSTEMCTL) start commerce-postgres.service commerce-redis.service

infra-down:
	$(SYSTEMCTL) stop commerce-postgres.service commerce-redis.service

infra-status:
	$(SYSTEMCTL) --no-pager --full status commerce-postgres.service commerce-redis.service

infra-logs:
	journalctl --user --follow -u commerce-postgres.service -u commerce-redis.service

migrate-up: require-env require-secrets $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" up

migrate-down: require-env require-secrets $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" down 1

migrate-version: require-env require-secrets $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" version

migrate-create: $(MIGRATE_BIN)
	@test -n "$(NAME)" || { printf 'NAME is required, for example: make migrate-create NAME=create_users\n' >&2; exit 1; }
	$(MIGRATE_BIN) create -ext sql -dir db/migrations -seq '$(NAME)'
