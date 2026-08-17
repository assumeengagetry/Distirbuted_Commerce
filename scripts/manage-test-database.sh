#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 3 ]] || [[ "$1" != "ensure" && "$1" != "reset" ]]; then
	printf 'usage: %s ensure|reset DATABASE_URL TEST_DATABASE_URL\n' "$0" >&2
	exit 2
fi

readonly mode="$1"
readonly application_url="$2"
readonly test_url="$3"
readonly ownership_marker="distributed-commerce:disposable-test-database:v1"

application_environment="${APP_ENV:-local}"
application_environment="${application_environment,,}"
application_environment="${application_environment#"${application_environment%%[![:space:]]*}"}"
application_environment="${application_environment%"${application_environment##*[![:space:]]}"}"
if [[ "$application_environment" == "production" ]]; then
	printf 'Test database management must not run with APP_ENV=production.\n' >&2
	exit 1
fi
if [[ -z "${PGPASSFILE:-}" ]]; then
	printf 'PGPASSFILE is required.\n' >&2
	exit 1
fi
if [[ "$test_url" != postgres://* && "$test_url" != postgresql://* ]]; then
	printf 'TEST_DATABASE_URL must be a PostgreSQL URL.\n' >&2
	exit 1
fi

test_url_without_query="${test_url%%\?*}"
test_database="${test_url_without_query##*/}"
if [[ ! "$test_database" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
	printf 'TEST_DATABASE_URL has an invalid database name.\n' >&2
	exit 1
fi

test_authority="${test_url_without_query%/*}"
test_query=""
if [[ "$test_url" == *\?* ]]; then
	test_query="?${test_url#*\?}"
fi
maintenance_url="${test_authority}/postgres${test_query}"

query_scalar() {
	psql "$1" --no-password --no-psqlrc --tuples-only --no-align --set ON_ERROR_STOP=1 --command "$2"
}

application_database="$(query_scalar "$application_url" "SELECT current_database()")"
if [[ "$test_database" != "${application_database}_test" ]]; then
	printf 'TEST_DATABASE_URL database name must be DATABASE_URL name plus _test.\n' >&2
	exit 1
fi

mark_test_database() {
	psql "$test_url" --no-password --no-psqlrc --quiet --set ON_ERROR_STOP=1 --command \
		"COMMENT ON DATABASE \"$test_database\" IS '$ownership_marker'" >/dev/null
}

if ! psql "$test_url" --no-password --no-psqlrc --quiet --command 'SELECT 1' >/dev/null 2>&1; then
	createdb --no-password --maintenance-db="$maintenance_url" "$test_database"
	mark_test_database
	created_test_database=true
else
	created_test_database=false
fi

database_identity() {
	query_scalar "$1" \
		"SELECT system_identifier::text || '|' || (SELECT oid::text FROM pg_database WHERE datname = current_database()) FROM pg_control_system()"
}

if [[ "$created_test_database" == false ]]; then
	test_marker="$(query_scalar "$test_url" "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = current_database()")"
	if [[ "$test_marker" != "$ownership_marker" ]]; then
		printf 'TEST_DATABASE_URL is not marked as a disposable project test database.\n' >&2
		exit 1
	fi
fi

application_identity="$(database_identity "$application_url")"
test_identity="$(database_identity "$test_url")"
if [[ "$application_identity" == "$test_identity" ]]; then
	printf 'TEST_DATABASE_URL resolves to the application database.\n' >&2
	exit 1
fi

if [[ "$mode" == "reset" ]]; then
	dropdb --no-password --force --maintenance-db="$maintenance_url" "$test_database"
	createdb --no-password --maintenance-db="$maintenance_url" "$test_database"
	mark_test_database
fi
