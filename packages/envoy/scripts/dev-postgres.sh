#!/usr/bin/env bash
# packages/envoy/scripts/dev-postgres.sh
#
# The local dev Postgres, `dispatch-pg` on 127.0.0.1:55432: created from the pinned pgvector image
# when absent, started when stopped, reused when running. Prints the DATABASE_URL it serves.
#
# Dispatch's migrations need the `vector` extension, which only the pinned image carries. A
# `dispatch-pg` running any other image (an older version of this script created it from
# postgres:16) is refused, naming both images and the commands that move its data volume onto the
# pinned image. This script never stops, removes or recreates an existing container itself: the
# container can hold other work's databases, and moving it is its owner's call.
set -euo pipefail

container="dispatch-pg"
image="pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"
# The PostgreSQL major version the pinned image runs: a data directory moves onto it only from 16.
image_major="16"
data_dir="/var/lib/postgresql/data"
run_flags=(-d --name "$container" -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432)
database_url="postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable"

refuse() {
  printf 'dev-postgres: %s\n' "$@" >&2
  exit 1
}

# Each argument as one shell word, joined by spaces: a word a shell would split or expand (a bind
# mount's host path with a space in it) is single-quoted, so a printed command pastes as it reads.
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
if [ "$container_image_id" = "$pinned_image_id" ]; then
  if [ "$(docker container inspect -f '{{.State.Running}}' "$container")" != "true" ]; then
    docker start "$container" >/dev/null
  fi
  printf '%s\n' "DATABASE_URL=$database_url"
  exit 0
fi

container_image="$(docker container inspect -f '{{.Config.Image}}' "$container")"
container_major="$(docker container inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" | sed -n 's/^PG_MAJOR=//p')"
# The mount that holds the data directory: a named or anonymous volume by its name, a bind mount
# by its host path. Either is what the new container mounts in the old one's place. The fields are
# split on `|`, never on whitespace, which would fold a bind mount's empty name into its path.
mount="$(docker container inspect -f "{{range .Mounts}}{{if eq .Destination \"$data_dir\"}}{{.Type}}|{{.Name}}|{{.Source}}{{end}}{{end}}" "$container")"
IFS='|' read -r mount_type mount_name mount_source <<<"$mount" || true
case "$mount_type" in
  volume) mount_spec="$mount_name:$data_dir" ;;
  bind) mount_spec="$mount_source:$data_dir" ;;
  *) mount_spec="" ;;
esac

stale="$container runs $container_image (image $container_image_id), not the pinned $image, so it has no pgvector, which Dispatch's migrations need."
if [ -z "$mount_spec" ]; then
  refuse "$stale" \
    "Its data directory $data_dir is on no volume, so moving to the pinned image would lose it. Dump what you need with \`docker exec $container pg_dumpall -U postgres\` before replacing the container."
fi
if [ "$container_major" != "$image_major" ]; then
  refuse "$stale" \
    "Its data volume ($mount_spec) holds PostgreSQL ${container_major:-of an unknown version}, and the pinned image runs PostgreSQL $image_major, which cannot open it. Dump what you need with \`docker exec $container pg_dumpall -U postgres\` before replacing the container."
fi
backup="$container-$(printf '%s' "${container_image_id#sha256:}" | cut -c1-12)"
refuse "$stale" \
  "To move it onto the pinned image and keep every database in it, run these in order; the old container stays, stopped, until the new one serves:" \
  "  $(quote docker stop "$container")" \
  "  $(quote docker rename "$container" "$backup")" \
  "  $(quote docker run "${run_flags[@]}" -v "$mount_spec" "$image")" \
  "  $(quote docker exec "$container" psql -U postgres -Atc "select 1 from pg_available_extensions where name = 'vector'")" \
  "  $(quote docker rm "$backup")" \
  "The psql line prints 1 once the new container serves; until then \`$(quote docker rename "$backup" "$container")\` and \`$(quote docker start "$container")\` put the old one back. The two images are built on different operating systems, so PostgreSQL may warn that a database's collation version does not match; \`REINDEX DATABASE <name>\` and then \`ALTER DATABASE <name> REFRESH COLLATION VERSION\` in that database clears it."
