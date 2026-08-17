SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

GO ?= go
PODMAN ?= podman
SYSTEMCTL ?= systemctl --user
USER_BINARY := bin/user-service
ORDER_BINARY := bin/order-service
PAYMENT_BINARY := bin/payment-service
WORKER_BINARY := bin/job-worker
JOB_ADMIN_BINARY := bin/job-admin
MIGRATE_VERSION := v4.18.3
MIGRATE_BIN := $(CURDIR)/bin/migrate-$(MIGRATE_VERSION)
MIGRATE_PACKAGE := github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
BUF_VERSION := v1.72.0
BUF_BIN := $(CURDIR)/bin/buf-$(BUF_VERSION)
BUF_PACKAGE := github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_BIN := $(CURDIR)/bin/protoc-gen-go-$(PROTOC_GEN_GO_VERSION)
PROTOC_GEN_GO_PACKAGE := google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
PROTOC_GEN_GO_GRPC_VERSION := v1.6.0
PROTOC_GEN_GO_GRPC_BIN := $(CURDIR)/bin/protoc-gen-go-grpc-$(PROTOC_GEN_GO_GRPC_VERSION)
PROTOC_GEN_GO_GRPC_PACKAGE := google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
SQLC_VERSION := v1.31.1
SQLC_BIN := $(CURDIR)/bin/sqlc-$(SQLC_VERSION)
SQLC_PACKAGE := github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
GOVULNCHECK_VERSION := v1.6.0
GOVULNCHECK_BIN := $(CURDIR)/bin/govulncheck-$(GOVULNCHECK_VERSION)
GOVULNCHECK_PACKAGE := golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
SECRETS_DIR := $(CURDIR)/.secrets
QUADLET_FILES := \
	deploy/quadlet/commerce-postgres.volume \
	deploy/quadlet/commerce-redis.volume \
	deploy/quadlet/commerce-postgres.container \
	deploy/quadlet/commerce-redis.container

.PHONY: help init build run run-user run-order run-payment run-worker jobs-failed jobs-retry jobs-delete fmt fmt-check vet test test-race test-integration check release-check migrate-install \
	sqlc sqlc-vet secrets quadlet-check quadlet-install infra-up infra-down infra-status infra-logs \
	migrate-up migrate-down migrate-version migrate-create require-env require-database-secret require-redis-secret require-user-secrets require-infra-secrets \
	proto-tools proto-format proto-lint proto-generate proto-check mod-check vulncheck test-database

help:
	@printf '%s\n' \
		'init              Create .env and random local secret files' \
		'build             Build service, worker, and job-admin binaries' \
		'run               Run user-service with values from .env' \
		'run-order         Run order-service on 127.0.0.1:8082' \
		'run-payment       Run payment-service on 127.0.0.1:8083' \
		'run-worker        Run the Asynq maintenance worker' \
		'jobs-failed       List archived tasks; pass optional PAGE=<n>' \
		'jobs-retry        Retry one archived task; pass ID=<task-id>' \
		'jobs-delete       Delete one archived task; pass ID=<task-id>' \
		'fmt               Format every Go source file' \
		'fmt-check         Fail when any Go source file is unformatted' \
		'vet               Run go vet' \
		'test              Run unit and HTTP tests' \
		'test-race         Run tests with the race detector' \
		'test-integration  Recreate the isolated database and run integration tests' \
		'test-database     Create the isolated local test database when absent' \
		'check             Run static checks, tests, race tests, and build' \
		'release-check     Add integration, vulnerability, module, and generation gates' \
		'sqlc              Generate typed database code' \
		'proto-generate     Generate Go protobuf and gRPC code' \
		'proto-lint         Lint protobuf contracts with pinned Buf' \
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
	$(GO) build -trimpath -o $(PAYMENT_BINARY) ./cmd/payment-service
	$(GO) build -trimpath -o $(WORKER_BINARY) ./cmd/job-worker
	$(GO) build -trimpath -o $(JOB_ADMIN_BINARY) ./cmd/job-admin

run: run-user

run-user: require-env require-user-secrets
	@set -a; source ./.env; set +a; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export PASETO_V4_LOCAL_KEY="$$(<'$(SECRETS_DIR)/paseto_v4_local_key')"; \
	export QUEUE_REDIS_ADDR="$${QUEUE_REDIS_ADDR-127.0.0.1:6379}"; \
	if [[ -n "$$QUEUE_REDIS_ADDR" ]]; then test -s '$(SECRETS_DIR)/redis_password' || { printf 'Run make init first.\n' >&2; exit 1; }; export QUEUE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; else unset QUEUE_REDIS_PASSWORD; fi; \
	export GRPC_ADDR="$${GRPC_ADDR:-127.0.0.1:9091}"; \
	exec $(GO) run ./cmd/user-service

run-order: require-env require-database-secret
	@set -a; source ./.env; set +a; \
	export SERVICE_NAME='order-service'; \
	export HTTP_ADDR="$${ORDER_HTTP_ADDR:-127.0.0.1:8082}"; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export IDENTITY_GRPC_TARGET="$${IDENTITY_GRPC_TARGET:-127.0.0.1:9091}"; \
	export CACHE_REDIS_ADDR="$${CACHE_REDIS_ADDR-127.0.0.1:6379}"; \
	if [[ -n "$$CACHE_REDIS_ADDR" ]]; then test -s '$(SECRETS_DIR)/redis_password' || { printf 'Run make init first.\n' >&2; exit 1; }; export CACHE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; else unset CACHE_REDIS_PASSWORD; fi; \
	unset PASETO_V4_LOCAL_KEY; \
	exec $(GO) run ./cmd/order-service

run-payment: require-env require-database-secret
	@set -a; source ./.env; set +a; \
	export SERVICE_NAME='payment-service'; \
	export HTTP_ADDR="$${PAYMENT_HTTP_ADDR:-127.0.0.1:8083}"; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export IDENTITY_GRPC_TARGET="$${IDENTITY_GRPC_TARGET:-127.0.0.1:9091}"; \
	unset PASETO_V4_LOCAL_KEY; \
	exec $(GO) run ./cmd/payment-service

run-worker: require-env require-database-secret require-redis-secret
	@set -a; source ./.env; set +a; \
	export SERVICE_NAME='job-worker'; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export QUEUE_REDIS_ADDR="$${QUEUE_REDIS_ADDR:-127.0.0.1:6379}"; \
	export QUEUE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; \
	unset PASETO_V4_LOCAL_KEY; \
	exec $(GO) run ./cmd/job-worker

jobs-failed: export JOB_PAGE := $(PAGE)
jobs-failed: require-env require-redis-secret
	@set -a; source ./.env; set +a; \
	export SERVICE_NAME='job-worker'; \
	export QUEUE_REDIS_ADDR="$${QUEUE_REDIS_ADDR:-127.0.0.1:6379}"; \
	export QUEUE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; \
	unset PASETO_V4_LOCAL_KEY; \
	if [[ -n "$$JOB_PAGE" ]]; then [[ "$$JOB_PAGE" =~ ^[1-9][0-9]*$$ ]] || { printf 'PAGE must be a positive integer.\n' >&2; exit 1; }; $(GO) run ./cmd/job-admin list "$$JOB_PAGE"; else $(GO) run ./cmd/job-admin list; fi

jobs-retry: export JOB_ID := $(ID)
jobs-retry: require-env require-redis-secret
	@set -a; source ./.env; set +a; \
	[[ "$$JOB_ID" =~ ^[a-zA-Z0-9:_-]{1,128}$$ ]] || { printf 'ID is invalid.\n' >&2; exit 1; }; \
	export SERVICE_NAME='job-worker'; \
	export QUEUE_REDIS_ADDR="$${QUEUE_REDIS_ADDR:-127.0.0.1:6379}"; \
	export QUEUE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; \
	unset PASETO_V4_LOCAL_KEY; \
	$(GO) run ./cmd/job-admin retry "$$JOB_ID"

jobs-delete: export JOB_ID := $(ID)
jobs-delete: require-env require-redis-secret
	@set -a; source ./.env; set +a; \
	[[ "$$JOB_ID" =~ ^[a-zA-Z0-9:_-]{1,128}$$ ]] || { printf 'ID is invalid.\n' >&2; exit 1; }; \
	export SERVICE_NAME='job-worker'; \
	export QUEUE_REDIS_ADDR="$${QUEUE_REDIS_ADDR:-127.0.0.1:6379}"; \
	export QUEUE_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; \
	unset PASETO_V4_LOCAL_KEY; \
	$(GO) run ./cmd/job-admin delete "$$JOB_ID"

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

test-integration: require-env require-database-secret require-redis-secret $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	export TEST_REDIS_ADDR="$${TEST_REDIS_ADDR:-127.0.0.1:6379}"; \
	export TEST_REDIS_USERNAME="$${TEST_REDIS_USERNAME:-commerce}"; \
	export TEST_REDIS_PASSWORD="$$(<'$(SECRETS_DIR)/redis_password')"; \
	export TEST_REDIS_DB="$${TEST_REDIS_DB:-15}"; \
	test -n "$${TEST_DATABASE_URL:-}" || { printf 'TEST_DATABASE_URL must point to an isolated test database.\n' >&2; exit 1; }; \
	cache_redis_db="$${CACHE_REDIS_DB:-0}"; queue_redis_db="$${QUEUE_REDIS_DB:-1}"; \
	[[ "$$TEST_REDIS_DB" =~ ^[0-9]+$$ && "$$cache_redis_db" =~ ^[0-9]+$$ && "$$queue_redis_db" =~ ^[0-9]+$$ ]] || { printf 'Redis database numbers must be decimal integers.\n' >&2; exit 1; }; \
	test_redis_db=$$((10#$$TEST_REDIS_DB)); cache_redis_db=$$((10#$$cache_redis_db)); queue_redis_db=$$((10#$$queue_redis_db)); \
	(( test_redis_db >= 2 && test_redis_db <= 15 && cache_redis_db >= 0 && cache_redis_db <= 15 && queue_redis_db >= 0 && queue_redis_db <= 15 )) || { printf 'Redis database numbers are outside the allowed range.\n' >&2; exit 1; }; \
	(( test_redis_db != cache_redis_db && test_redis_db != queue_redis_db )) || { printf 'TEST_REDIS_DB must differ from cache and queue Redis databases.\n' >&2; exit 1; }; \
	app_env="$${APP_ENV:-local}"; app_env="$${app_env,,}"; app_env="$${app_env#"$${app_env%%[![:space:]]*}"}"; app_env="$${app_env%"$${app_env##*[![:space:]]}"}"; \
	test "$$app_env" != 'production' || { printf 'Integration tests must not run with APP_ENV=production.\n' >&2; exit 1; }; \
	bash scripts/manage-test-database.sh reset "$$DATABASE_URL" "$$TEST_DATABASE_URL"; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$TEST_DATABASE_URL" up; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$TEST_DATABASE_URL" down -all; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$TEST_DATABASE_URL" up; \
	TEST_DATABASE_URL="$$TEST_DATABASE_URL" $(GO) test -count=1 -p=1 -tags=integration ./...

check: proto-check fmt-check vet test test-race build sqlc-vet quadlet-check

release-check:
	@$(MAKE) --no-print-directory check
	@$(MAKE) --no-print-directory test-integration
	@$(MAKE) --no-print-directory vulncheck
	@$(MAKE) --no-print-directory mod-check

sqlc: $(SQLC_BIN)
	$(SQLC_BIN) generate

sqlc-vet: $(SQLC_BIN)
	$(SQLC_BIN) vet

proto-tools: $(BUF_BIN) $(PROTOC_GEN_GO_BIN) $(PROTOC_GEN_GO_GRPC_BIN)

proto-format: $(BUF_BIN)
	$(BUF_BIN) format -w

proto-lint: $(BUF_BIN)
	$(BUF_BIN) lint

proto-generate: proto-tools proto-lint
	$(BUF_BIN) generate

proto-check: proto-tools $(SQLC_BIN) proto-lint
	$(BUF_BIN) breaking --against api/proto-baseline
	bash scripts/check-generated.sh '$(BUF_BIN)' '$(SQLC_BIN)'

mod-check:
	$(GO) mod tidy -diff

vulncheck: $(GOVULNCHECK_BIN)
	$(GOVULNCHECK_BIN) ./...

$(BUF_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install '$(BUF_PACKAGE)'
	mv '$(CURDIR)/bin/buf' '$(BUF_BIN)'

$(PROTOC_GEN_GO_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install '$(PROTOC_GEN_GO_PACKAGE)'
	mv '$(CURDIR)/bin/protoc-gen-go' '$(PROTOC_GEN_GO_BIN)'

$(PROTOC_GEN_GO_GRPC_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install '$(PROTOC_GEN_GO_GRPC_PACKAGE)'
	mv '$(CURDIR)/bin/protoc-gen-go-grpc' '$(PROTOC_GEN_GO_GRPC_BIN)'

$(SQLC_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install '$(SQLC_PACKAGE)'
	mv '$(CURDIR)/bin/sqlc' '$(SQLC_BIN)'

$(GOVULNCHECK_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install '$(GOVULNCHECK_PACKAGE)'
	mv '$(CURDIR)/bin/govulncheck' '$(GOVULNCHECK_BIN)'

migrate-install: $(MIGRATE_BIN)

$(MIGRATE_BIN):
	@mkdir -p bin
	GOBIN='$(CURDIR)/bin' $(GO) install -tags postgres '$(MIGRATE_PACKAGE)'
	mv '$(CURDIR)/bin/migrate' '$(MIGRATE_BIN)'

require-env:
	@test -f .env || { printf 'Run make init first.\n' >&2; exit 1; }

require-database-secret:
	@test -s '$(SECRETS_DIR)/pgpass' || { printf 'Run make init first.\n' >&2; exit 1; }

require-redis-secret:
	@test -s '$(SECRETS_DIR)/redis_password' || { printf 'Run make init first.\n' >&2; exit 1; }

require-user-secrets: require-database-secret
	@test -s '$(SECRETS_DIR)/paseto_v4_local_key' || { printf 'Run make init first.\n' >&2; exit 1; }

require-infra-secrets:
	@test -s '$(SECRETS_DIR)/postgres_password' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/redis_password' || { printf 'Run make init first.\n' >&2; exit 1; }
	@test -s '$(SECRETS_DIR)/redis.conf' || { printf 'Run make init first.\n' >&2; exit 1; }

secrets: require-infra-secrets
	@$(PODMAN) secret create --replace commerce-postgres-password '$(SECRETS_DIR)/postgres_password' >/dev/null
	@$(PODMAN) secret create --replace commerce-redis-password '$(SECRETS_DIR)/redis_password' >/dev/null
	@$(PODMAN) secret create --replace commerce-redis-config '$(SECRETS_DIR)/redis.conf' >/dev/null

quadlet-check:
	@QUADLET_UNIT_DIRS='$(CURDIR)/deploy/quadlet' \
		/usr/lib/systemd/user-generators/podman-user-generator -user -dryrun >/dev/null

quadlet-install: quadlet-check
	$(PODMAN) quadlet install --replace $(QUADLET_FILES)
	$(SYSTEMCTL) daemon-reload

infra-up: require-infra-secrets quadlet-install
	@$(SYSTEMCTL) stop commerce-postgres.service commerce-redis.service >/dev/null 2>&1 || true
	@$(MAKE) --no-print-directory secrets
	$(SYSTEMCTL) start commerce-postgres.service commerce-redis.service
	@$(MAKE) --no-print-directory test-database

test-database: require-env require-database-secret
	@set -a; source ./.env; set +a; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	test -n "$${TEST_DATABASE_URL:-}" || { printf 'TEST_DATABASE_URL is required.\n' >&2; exit 1; }; \
	bash scripts/manage-test-database.sh ensure "$$DATABASE_URL" "$$TEST_DATABASE_URL"

infra-down:
	$(SYSTEMCTL) stop commerce-postgres.service commerce-redis.service

infra-status:
	$(SYSTEMCTL) --no-pager --full status commerce-postgres.service commerce-redis.service

infra-logs:
	journalctl --user --follow -u commerce-postgres.service -u commerce-redis.service

migrate-up: require-env require-database-secret $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" up

migrate-down: require-env require-database-secret $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" down 1

migrate-version: require-env require-database-secret $(MIGRATE_BIN)
	@set -a; source ./.env; set +a; \
	test -n "$${DATABASE_URL:-}" || { printf 'DATABASE_URL is required.\n' >&2; exit 1; }; \
	export PGPASSFILE='$(SECRETS_DIR)/pgpass'; \
	$(MIGRATE_BIN) -path=db/migrations -database "$$DATABASE_URL" version

migrate-create: $(MIGRATE_BIN)
	@test -n "$(NAME)" || { printf 'NAME is required, for example: make migrate-create NAME=create_users\n' >&2; exit 1; }
	$(MIGRATE_BIN) create -ext sql -dir db/migrations -seq '$(NAME)'
