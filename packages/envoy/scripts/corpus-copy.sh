#!/usr/bin/env bash
# Restores the newest nightly Dispatch dump into a local Postgres container and prints its
# DATABASE_URL, so a search change can be measured on the real corpus (cmd/dispatch/README.md,
# "Search"). The dump is production data: it stays on this machine, the downloaded file is deleted
# once it is restored, and the container is the copy (`docker rm -f -v <container>` deletes it with its data volume).
# Re-running while the container holds the database skips the download and the restore.
set -euo pipefail

if (($# != 0)); then
  printf 'usage: DISPATCH_BACKUP_BUCKET=<bucket> %s\n' "$0" >&2
  exit 2
fi
# The bucket is the deployment's, so it is an input and never a literal here.
readonly bucket="${DISPATCH_BACKUP_BUCKET:?DISPATCH_BACKUP_BUCKET must name the bucket the nightly Dispatch dumps land in}"
readonly container="${DISPATCH_CORPUS_CONTAINER:-dispatch-corpus-pg}"
readonly port="${DISPATCH_CORPUS_PORT:-55433}"
readonly database="${DISPATCH_CORPUS_DATABASE:-dispatch_corpus}"
readonly workdir="${DISPATCH_CORPUS_DIR:-${TMPDIR:-/tmp}}"

for command in aws docker psql pg_restore; do
  command -v "$command" >/dev/null || {
    printf '%s is required\n' "$command" >&2
    exit 1
  }
done

readonly admin_url="postgres://postgres:dispatch@127.0.0.1:${port}/postgres?sslmode=disable"
readonly database_url="postgres://postgres:dispatch@127.0.0.1:${port}/${database}?sslmode=disable"

if [[ "$(docker container inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" == true ]] &&
  [[ "$(psql --quiet --tuples-only --no-align "$admin_url" -c "select 1 from pg_database where datname = '${database}'" 2>/dev/null)" == 1 ]]; then
  printf '%s already holds %s; remove it with docker rm -f -v %s to restore a newer dump\n' "$container" "$database" "$container" >&2
  printf 'DATABASE_URL=%s\n' "$database_url"
  exit 0
fi

# Each dump is named for the UTC second it was taken (dispatch/dispatch_<time>.dump), so the
# names sort in the order the dumps were taken.
newest="$(aws s3 ls "s3://${bucket}/dispatch/" | awk '$4 ~ /\.dump$/ { print $4 }' | sort | tail -n 1)"
if [[ -z "$newest" ]]; then
  printf 'no dump under s3://%s/dispatch/\n' "$bucket" >&2
  exit 1
fi
readonly newest
readonly dump="${workdir}/${newest}"
trap 'rm -f "$dump"' EXIT
printf 'fetching %s\n' "$newest" >&2
aws s3 cp --only-show-errors "s3://${bucket}/dispatch/${newest}" "$dump"

if ! docker container inspect "$container" >/dev/null 2>&1; then
  # Two CPUs and 2 GiB, as every container on a shared devbox carries limits; the corpus lives on
  # the container's volume, outside that memory.
  docker run -d --name "$container" --cpus 2 --memory 2g -e POSTGRES_PASSWORD=dispatch -p "127.0.0.1:${port}:5432" mirror.gcr.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 >/dev/null
else
  docker start "$container" >/dev/null
fi
for _ in $(seq 1 120); do
  if psql --quiet "$admin_url" -c 'select 1' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
psql --quiet "$admin_url" -c 'select 1' >/dev/null

DISPATCH_ADMIN_URL="$admin_url" bash "$(dirname "$0")/restore-dispatch-dump.sh" "$dump" "$database" >/dev/null
printf 'restored %s into %s; the downloaded dump is deleted\n' "$newest" "$container" >&2
printf 'DATABASE_URL=%s\n' "$database_url"
