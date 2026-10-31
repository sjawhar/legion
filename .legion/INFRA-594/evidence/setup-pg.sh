#!/usr/bin/env bash
# INFRA-594: rootless Postgres 17 + pgvector on port 5433, as
# docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-...-INFRA-42.md says.
# Everything under $TMPDIR/infra594. Idempotent: re-running starts a server that is down.
set -euo pipefail

TMPDIR="${TMPDIR:-/legion/scratch/tmp}"
BASE="$TMPDIR/infra594"
ROOT="$BASE/pgroot"
DATA="$BASE/pg"
SOCK="$BASE/pgsock"
DEBS="$BASE/debs"
mkdir -p "$ROOT" "$DEBS" "$SOCK"

fetch() { # fetch <pool-path> <glob>
  local dir="$1" pat="$2" name
  name=$(curl -s "https://deb.debian.org/debian/pool/main/$dir/" | grep -o "$pat" | sort -u | tail -1)
  [ -n "$name" ] || { echo "no match for $pat in $dir" >&2; exit 1; }
  [ -f "$DEBS/$name" ] || curl -sSL -o "$DEBS/$name" "https://deb.debian.org/debian/pool/main/$dir/$name"
  echo "$DEBS/$name"
}

if [ ! -x "$ROOT/usr/lib/postgresql/17/bin/initdb" ]; then
  for deb in \
    "$(fetch p/postgresql-17 'postgresql-17_[^"]*amd64\.deb')" \
    "$(fetch p/pgvector 'postgresql-17-pgvector_[^"]*amd64\.deb')" \
    "$(fetch i/icu 'libicu76_[^"]*amd64\.deb')" \
    "$(fetch p/postgresql-17 'postgresql-client-17_[^"]*amd64\.deb')"; do
    echo "unpacking $deb"
    dpkg-deb -x "$deb" "$ROOT"
  done
fi

export PATH="$ROOT/usr/lib/postgresql/17/bin:$PATH"
export LD_LIBRARY_PATH="$ROOT/usr/lib/x86_64-linux-gnu:$ROOT/usr/lib/postgresql/17/lib"

if [ ! -f "$DATA/PG_VERSION" ]; then
  rm -rf "$DATA"
  initdb -D "$DATA" -U postgres --locale=C --encoding=UTF8
fi

if ! pg_ctl -D "$DATA" status >/dev/null 2>&1; then
  pg_ctl -D "$DATA" -l "$BASE/pg.log" -o "-k $SOCK -h 127.0.0.1 -p 5433" -w start
fi

psql "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable" -v ON_ERROR_STOP=1 \
  -c 'create extension if not exists vector' -c 'select version()'
echo "DISPATCH_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable'"
