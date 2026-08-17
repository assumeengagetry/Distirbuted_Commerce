#!/usr/bin/env bash

set -euo pipefail

readonly project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly secrets_dir="${project_root}/.secrets"
readonly postgres_password_file="${secrets_dir}/postgres_password"
readonly redis_password_file="${secrets_dir}/redis_password"
readonly paseto_key_file="${secrets_dir}/paseto_v4_local_key"

umask 077
mkdir -p "${secrets_dir}"
chmod 700 "${secrets_dir}"

generate_secret() {
	local destination="$1"
	if [[ ! -s "${destination}" ]]; then
		openssl rand -hex 32 >"${destination}"
	fi
	chmod 600 "${destination}"
}

generate_secret "${postgres_password_file}"
generate_secret "${redis_password_file}"
generate_secret "${paseto_key_file}"

postgres_password="$(<"${postgres_password_file}")"
redis_password="$(<"${redis_password_file}")"
paseto_key="$(<"${paseto_key_file}")"
if [[ ! "${postgres_password}" =~ ^[[:alnum:]]+$ ]] || [[ ! "${redis_password}" =~ ^[[:alnum:]]+$ ]]; then
	printf 'Local password files must contain only letters and digits.\n' >&2
	exit 1
fi
if [[ ! "${paseto_key}" =~ ^[[:xdigit:]]{64}$ ]]; then
	printf 'The PASETO key must contain exactly 64 hexadecimal characters.\n' >&2
	exit 1
fi

printf '%s' "${postgres_password}" >"${postgres_password_file}"
printf '%s' "${redis_password}" >"${redis_password_file}"
printf '%s' "${paseto_key}" >"${paseto_key_file}"
chmod 600 "${postgres_password_file}" "${redis_password_file}" "${paseto_key_file}"

{
	printf '127.0.0.1:5432:postgres:commerce:%s\n' "${postgres_password}"
	printf '127.0.0.1:5432:commerce:commerce:%s\n' "${postgres_password}"
	printf '127.0.0.1:5432:commerce_test:commerce:%s\n' "${postgres_password}"
} >"${secrets_dir}/pgpass"
chmod 600 "${secrets_dir}/pgpass"

{
	printf 'appendonly yes\n'
	printf 'appendfsync everysec\n'
	printf 'maxmemory 192mb\n'
	printf 'maxmemory-policy noeviction\n'
	printf 'dir /data\n'
	printf 'user default off\n'
	printf 'user commerce on >%s ~* &* +@all\n' "${redis_password}"
} >"${secrets_dir}/redis.conf"
chmod 600 "${secrets_dir}/redis.conf"

if [[ ! -f "${project_root}/.env" ]]; then
	install -m 600 "${project_root}/.env.example" "${project_root}/.env"
fi
chmod 600 "${project_root}/.env"

printf 'Local environment initialized without printing secret values.\n'
