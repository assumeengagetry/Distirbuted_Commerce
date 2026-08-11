#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  printf 'usage: %s BUF SQLC\n' "$0" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
buf="$1"
sqlc="$2"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

mkdir -p "$temporary/api" "$temporary/internal/database"
cp "$root/buf.yaml" "$root/buf.gen.yaml" "$root/sqlc.yaml" "$temporary/"
ln -s "$root/api/proto" "$temporary/api/proto"
ln -s "$root/bin" "$temporary/bin"
ln -s "$root/db" "$temporary/db"

(
	cd "$temporary"
	"$buf" generate
	"$sqlc" generate
)

/usr/bin/diff -ru "$root/internal/genproto" "$temporary/internal/genproto"
/usr/bin/diff -ru "$root/internal/database/sqlc" "$temporary/internal/database/sqlc"
