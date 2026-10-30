#!/usr/bin/env bash
# Idempotent Postgres harness for this issue pod (INFRA-484).
# Builds a rootless PostgreSQL 17 under /legion/scratch/pg (the issue's volume) from Debian
# packages, starts it on 127.0.0.1:5433, and creates the role/database LEGION_TEST_PG_DSN names.
# PATH and LD_LIBRARY_PATH are exported here only, around this script's own commands.
#   DSN: postgres://legion:legion@127.0.0.1:5433/legion
set -euo pipefail

ROOT=/legion/scratch/pg
DEBS="$ROOT/debs"
PREFIX="$ROOT/root"
DATA="$ROOT/data"
SOCK="$ROOT/sock"
PORT=5433

export PATH="$PREFIX/usr/lib/postgresql/17/bin:$PATH"
export LD_LIBRARY_PATH="$PREFIX/usr/lib/x86_64-linux-gnu:$PREFIX/usr/lib/postgresql/17/lib"

mkdir -p "$DEBS" "$PREFIX" "$SOCK"

base=https://deb.debian.org/debian/pool/main
for f in \
  p/postgresql-17/libpq5_17.11-0+deb13u1_amd64.deb \
  p/postgresql-17/postgresql-17_17.11-0+deb13u1_amd64.deb \
  p/postgresql-17/postgresql-client-17_17.11-0+deb13u1_amd64.deb \
  i/icu/libicu76_76.1-4+b1_amd64.deb; do
  name=${f##*/}
  [ -s "$DEBS/$name" ] || curl -fsSL -o "$DEBS/$name" "$base/$f"
done

if [ ! -x "$PREFIX/usr/lib/postgresql/17/bin/postgres" ]; then
  for deb in "$DEBS"/*.deb; do dpkg-deb -x "$deb" "$PREFIX"; done
fi

if [ ! -s "$DATA/PG_VERSION" ]; then
  rm -rf "$DATA"
  initdb -D "$DATA" -U postgres --locale=C --encoding=UTF8 >/dev/null
fi

if ! pg_ctl -D "$DATA" status >/dev/null 2>&1; then
  pg_ctl -D "$DATA" -l "$ROOT/server.log" \
    -o "-k $SOCK -h 127.0.0.1 -p $PORT" -w start >/dev/null
fi

psql -h 127.0.0.1 -p "$PORT" -U postgres -d postgres -v ON_ERROR_STOP=1 -q <<'SQL'
select 'create role legion login superuser createdb password ''legion'''
 where not exists (select 1 from pg_roles where rolname = 'legion') \gexec
select 'create database legion owner legion'
 where not exists (select 1 from pg_database where datname = 'legion') \gexec
SQL

echo "ready: postgres://legion:legion@127.0.0.1:$PORT/legion"
