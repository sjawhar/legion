#!/usr/bin/env bash
set -euo pipefail

if (($# < 1 || $# > 2)); then
  printf 'usage: %s <dump-file> [database-name]\n' "$0" >&2
  exit 2
fi

for command in psql pg_restore; do
  command -v "$command" >/dev/null || {
    printf '%s is required\n' "$command" >&2
    exit 1
  }
done

readonly dump_file="$1"
if [[ ! -f "$dump_file" ]]; then
  printf 'dump file not found: %s\n' "$dump_file" >&2
  exit 1
fi

if (($# == 2)); then
  database_name="$2"
else
  basename="$(basename "$dump_file")"
  database_name="dispatch_copy_${basename//[^a-z0-9]/_}"
fi

if [[ ! "$database_name" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]] || ((${#database_name} > 63)); then
  printf 'database name must be a PostgreSQL identifier no longer than 63 characters: %s\n' "$database_name" >&2
  exit 2
fi
readonly database_name

readonly admin_url="${DISPATCH_ADMIN_URL:-postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable}"
case "$admin_url" in
  */postgres\?*) database_url="${admin_url%/postgres\?*}/${database_name}?${admin_url#*\?}" ;;
  */postgres) database_url="${admin_url%/postgres}/${database_name}" ;;
  *)
    printf 'DISPATCH_ADMIN_URL must name the postgres database: %s\n' "$admin_url" >&2
    exit 2
    ;;
esac
readonly database_url

psql --quiet --set=ON_ERROR_STOP=1 "$admin_url" \
  -c "drop database if exists \"${database_name}\" with (force)" \
  -c "create database \"${database_name}\""
pg_restore --no-owner --no-privileges --dbname="$database_url" "$dump_file"
printf 'DATABASE_URL=%s\n' "$database_url"
