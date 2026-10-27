#!/usr/bin/env bash
# packages/envoy/scripts/dev-postgres.sh
#
# The local dev Postgres, `dispatch-pg` on 127.0.0.1:55432: created from the pinned image, pgvector
# on PostgreSQL 18, when absent, started when stopped, reused when running. Prints the
# DATABASE_URL it serves.
#
# The pinned image is pgvector on PostgreSQL 18, the version Dispatch's tests and dev databases
# run; Dispatch's migrations need its `vector` extension. A `dispatch-pg` running any other image
# (an older version of this script created it from postgres:16, then from pgvector on PostgreSQL
# 16) is refused, naming both images and the commands that dump every database in it, check the
# dump finished, and restore them into a new container on the pinned image, which works from any
# image or major and needs no volume. This script never starts, stops, renames, removes or
# recreates a stale container itself: the container can hold other work's databases, and moving it
# is its owner's call.
set -euo pipefail

container="dispatch-pg"
image="mirror.gcr.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
# Two CPUs and 2 GiB, as every container on a shared devbox carries limits; a dev database's
# working set fits, and its data stays on the volume, outside that memory.
run_flags=(-d --name "$container" --cpus 2 --memory 2g -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432)
database_url="postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable"

refuse() {
  printf 'dev-postgres: %s\n' "$@" >&2
  exit 1
}

# Each argument as one shell word, joined by spaces: a word a shell would split or expand (a dump
# path under a TMPDIR with a space in it) is single-quoted, so a printed command pastes as it reads.
# The same rule as shellQuote in packages/dispatch/e2e/local-instance-support.ts; both tests hold
# it to fixtures/shell-quote.json.
quote() {
  local words=() word
  for word in "$@"; do
    if [[ "$word" =~ ^[A-Za-z0-9_@%+=:,./-]+$ ]]; then
      words+=("$word")
    else
      words+=("'${word//\'/\'\\\'\'}'")
    fi
  done
  printf '%s' "${words[*]}"
}

if ! docker container inspect "$container" >/dev/null 2>&1; then
  docker run "${run_flags[@]}" "$image" >/dev/null
  printf '%s\n' "DATABASE_URL=$database_url"
  exit 0
fi

# The container's image by id, compared with the pinned image's id: the reference a container was
# created with can name the same image several ways. A pinned image absent here cannot be the one
# the container runs, since Docker keeps every image a container uses.
container_image_id="$(docker container inspect -f '{{.Image}}' "$container")"
pinned_image_id="$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null || true)"
running="$(docker container inspect -f '{{.State.Running}}' "$container")"
if [ "$container_image_id" = "$pinned_image_id" ]; then
  if [ "$running" != "true" ]; then
    docker start "$container" >/dev/null
  fi
  printf '%s\n' "DATABASE_URL=$database_url"
  exit 0
fi

container_image="$(docker container inspect -f '{{.Config.Image}}' "$container")"
backup="$container-$(printf '%s' "${container_image_id#sha256:}" | cut -c1-12)"
dump="${TMPDIR:-/tmp}/$backup.sql"
wait_ready=(docker exec "$container" pg_isready -h 127.0.0.1 -U postgres)
steps=()
# pg_dumpall reads from a running server: a stopped container is started first, as it is, and
# waited for, since a server still starting refuses the dump and the redirect leaves an empty file.
if [ "$running" != "true" ]; then
  steps+=(
    "  $(quote docker start "$container")"
    "Repeat this until it reports accepting connections; a dump taken before then is refused and leaves an empty file:"
    "  $(quote "${wait_ready[@]}")"
  )
fi
# The same listing runs against the old container before the dump and the new one after the
# restore, so the two outputs compare line for line. The same count runs on both too, of the tables
# in the dispatch database (the one POSTGRES_DB in run_flags names) and of the rows they hold,
# printed as <tables>|<rows>: after an empty dump restores nothing, the new container still lists
# postgres and dispatch, which it creates itself, but its dispatch database holds no table, and
# after a dump that stopped once its schema was written, it holds every table and none of their rows.
# The count covers the dispatch database alone; the whole dump is checked by its last comment,
# which pg_dumpall writes only once it has dumped every database, before the old container stops.
list_databases=(docker exec "$container" psql -U postgres -Atc 'select datname from pg_database where not datistemplate order by 1')
count_dispatch=(docker exec "$container" psql -U postgres -d dispatch -Atc "select count(*), coalesce(sum((xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %I.%I', schemaname, tablename), false, true, '')))[1]::text::bigint), 0) from pg_tables where schemaname = 'public'")
steps+=(
  "Keep the output of this: the databases the old container holds, every one of which the new container must list after the restore."
  "  $(quote "${list_databases[@]}")"
  "Keep the output of this too: the number of tables in the old container's dispatch database and the rows they hold. It counts the dispatch database alone; the check after the dump covers every database. Stop anything that writes to this container (a Dispatch server, a test run) before these counts and the dump, or the counts after the restore cannot match them."
  "  $(quote "${count_dispatch[@]}")"
  "Then dump them:"
  "  $(quote docker exec "$container" pg_dumpall -U postgres --clean --if-exists) > $(quote "$dump")"
  "This prints 1 when pg_dumpall finished every database; if it prints 0 (a failed, cut or empty dump), stop here: the old container is still running and untouched."
  "  $(quote grep -c -x -- '-- PostgreSQL database cluster dump complete' "$dump")"
  "Then move the old container aside:"
  "  $(quote docker stop "$container")"
  "  $(quote docker rename "$container" "$backup")"
  "  $(quote docker run "${run_flags[@]}" "$image")"
  "Repeat this until it reports accepting connections. It asks over TCP because the new container's first-start setup server listens only on its socket, and stops before the server that keeps running starts:"
  "  $(quote "${wait_ready[@]}")"
  "Then restore the dump. psql reports two errors, which are expected: role \"postgres\" already exists, and current user cannot be dropped."
  "  $(quote docker exec -i "$container" psql -h 127.0.0.1 -U postgres -q -f -) < $(quote "$dump")"
  "This lists the new container's databases. It must list every database the old container's listing printed; if one is missing, the dump is incomplete, so stop here and keep the old container and the dump:"
  "  $(quote "${list_databases[@]}")"
  "This counts the tables in the new container's dispatch database and the rows they hold, the dispatch database alone. It must print the same two numbers as the old container did; if either differs (a dump that failed or stopped partway, or a write to the old container's dispatch database after its count), stop here and keep the old container and the dump:"
  "  $(quote "${count_dispatch[@]}")"
  "These print 1 and 18xxxx once the new container serves the restored data:"
  "  $(quote docker exec "$container" psql -U postgres -Atc "select 1 from pg_available_extensions where name = 'vector'")"
  "  $(quote docker exec "$container" psql -U postgres -Atc 'show server_version_num')"
  "Once the checks match, the dump can go:"
  "  $(quote rm "$dump")"
)
refuse "$container runs $container_image (image $container_image_id), not the pinned $image, which is pgvector on PostgreSQL 18, the version Dispatch's tests and dev databases run." \
  "To move every database in it to a new $container on the pinned image, from any image or PostgreSQL major and with no volume needed, run these in order; the old container stays, stopped, as $backup:" \
  "${steps[@]}" \
  "The old one can come back: once it is renamed, \`$(quote docker rm -f -v "$container")\` removes the new one, with its anonymous data volume, if you created it, then \`$(quote docker rename "$backup" "$container")\` and \`$(quote docker start "$container")\` put the old one back." \
  "The old container stays, stopped, as $backup; once Dispatch works on the new container, \`$(quote docker rm -v "$backup")\` deletes it and its data."
