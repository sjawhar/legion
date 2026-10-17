#!/usr/bin/env bash
# Keeps the devbox Dispatch on the newest main commit with a published image.
set -euo pipefail

POLL="${POLL:-300}"
REPO="${REPO:-/home/ubuntu/.worktrees/legion/dn-an}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-envoy-dispatch}"
COMPOSE_DIR="${COMPOSE_DIR:-$REPO/packages/envoy/deploy/compose}"
BACKUPS="${BACKUPS:-/home/ubuntu/tmp/dispatch-backups}"
IMAGE="${IMAGE:-ghcr.io/sjawhar/legion/envoy}"
COMPOSE_FILE="$COMPOSE_DIR/dispatch.compose.yml"
COMPOSE_ENV="$COMPOSE_DIR/.env"
# pg_dump N dumps servers <= N; the production database is Aurora PostgreSQL 16.
PG_DUMP_IMAGE="${PG_DUMP_IMAGE:-postgres:16.11-alpine}"

case "$#" in
  0)
    once=false
    ;;
  1)
    if [[ "$1" != "--once" ]]; then
      printf 'usage: %s [--once]\n' "$0" >&2
      exit 2
    fi
    once=true
    ;;
  *)
    printf 'usage: %s [--once]\n' "$0" >&2
    exit 2
    ;;
esac

mkdir -p "$BACKUPS"

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

compose() {
  local duration="$1"
  shift
  timeout "$duration" docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
}

compose_with_tag() {
  local tag="$1"
  local duration="$2"
  shift 2
  env ENVOY_IMAGE_TAG="$tag" timeout "$duration" docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
}

env_value() {
  sed -nE "s/^$1=(.*)$/\1/p" "$COMPOSE_ENV"
}

deployed_tag() {
  env_value ENVOY_IMAGE_TAG
}

set_env_value() {
  if grep -q "^$1=" "$COMPOSE_ENV"; then
    sed -i -E "s|^$1=.*|$1=$2|" "$COMPOSE_ENV"
  else
    printf '%s=%s\n' "$1" "$2" >>"$COMPOSE_ENV"
  fi
}

# schema_version prints the highest migration the database has applied, the `schema_version`
# Dispatch's /healthz reports, read through the image the backup runs and with DATABASE_URL passed
# by name as the backup passes it.
schema_version() {
  local version
  # shellcheck disable=SC2016 # $DATABASE_URL expands in the container's shell, from its environment
  version=$(DATABASE_URL="$1" timeout 60 docker run --rm --network host -e DATABASE_URL "$PG_DUMP_IMAGE" \
    sh -c 'psql -X -At -v ON_ERROR_STOP=1 -c "select coalesce(max(version), 0) from schema_migrations" "$DATABASE_URL"') ||
    return 1
  [[ "$version" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "$version"
}

newest_published_main() {
  local sha
  jj -R "$REPO" git fetch >/dev/null 2>&1
  while IFS= read -r sha; do
    if timeout 60 docker manifest inspect "$IMAGE:$sha" >/dev/null 2>&1; then
      printf '%s\n' "$sha"
      return 0
    fi
  done < <(jj -R "$REPO" log -r 'ancestors(main@origin, 30)' --no-graph -T 'commit_id ++ "\n"')
  return 1
}

prune_backups() {
  local -a dumps=()
  local dump
  local i
  while IFS= read -r -d '' dump; do
    dumps+=("$dump")
  done < <(find "$BACKUPS" -maxdepth 1 -type f -name 'pre-*.dump' -printf '%T@:%p\0' | sort -znr)
  for ((i = 10; i < ${#dumps[@]}; i++)); do
    rm -f -- "${dumps[i]#*:}"
  done
}

oauth_redirect_uri() {
  local origin="$1"
  local port="$2"
  local host="${origin#http://}"
  host="${host#https://}"
  curl -s -o /dev/null -w '%{redirect_url}' -H "Host: $host" "http://127.0.0.1:$port/auth/start" |
    python3 -c 'import sys, urllib.parse as u; query = u.parse_qs(u.urlparse(sys.stdin.read()).query); print(query.get("redirect_uri", [""])[0])'
}

wait_for_health() {
  local port="$1"
  local attempt
  for ((attempt = 0; attempt < 30; attempt++)); do
    sleep 3
    if curl -sf --max-time 3 "http://127.0.0.1:$port/healthz" >/dev/null; then
      return 0
    fi
  done
  return 1
}

rollback() {
  local previous="$1"
  local dispatchPort="$2"
  sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$previous|" "$COMPOSE_ENV"
  if ! compose 300 up -d --no-deps dispatch; then
    log "rollback compose failed for $previous"
    return 1
  fi
  if ! wait_for_health "$dispatchPort"; then
    log "rollback to $previous did not become healthy within 90s"
    return 1
  fi
}

# roll_back_or_stop answers a deploy of $sha that failed. It restores $previous only while the
# schema is still the one $previous runs against. A failed image may have migrated before it failed,
# and an image older than its database's schema can write what the newer release fails to read
# (packages/envoy/cmd/dispatch/README.md, "Database migrations"), so once the schema has moved, or
# cannot be read, it stops $sha instead and fails, leaving the recovery to an operator, forward.
roll_back_or_stop() {
  local sha="$1"
  local previous="$2"
  local baseline="$3"
  local databaseUrl="$4"
  local dispatchPort="$5"
  local now
  if ! now=$(schema_version "$databaseUrl"); then
    compose 120 stop dispatch || log "could not stop $sha"
    log "could not read the schema version after $sha failed, so refusing to roll back to $previous, which runs against schema $baseline: $sha is stopped, and forward recovery is required"
    return 1
  fi
  if ((now > baseline)); then
    compose 120 stop dispatch || log "could not stop $sha"
    log "refusing to roll back to $previous: it runs against schema $baseline, and $sha moved the schema to $now before it failed. $sha is stopped, and forward recovery is required: deploy an image that fixes it, or start $sha by hand once its failure is understood. Never start $previous against schema $now"
    return 1
  fi
  log "rolling back to $previous (schema $now)"
  rollback "$previous" "$dispatchPort"
}

deploy() {
  local sha="$1"
  local previous
  local databaseUrl
  local dispatchPort
  local before
  local recorded
  local baseline
  local dump
  local origin
  local redirect
  local expected

  previous=$(deployed_tag)
  origin=$(env_value DISPATCH_SERVER_URL)
  if [[ -z "$origin" ]]; then
    log "DISPATCH_SERVER_URL is missing from $COMPOSE_ENV; not deploying $sha"
    return 1
  fi
  databaseUrl=$(env_value DATABASE_URL)
  if [[ -z "$databaseUrl" ]]; then
    log "DATABASE_URL is missing from $COMPOSE_ENV; not deploying $sha"
    return 1
  fi
  dispatchPort=$(env_value DISPATCH_PORT)
  dispatchPort="${dispatchPort:-8766}"
  if ! before=$(schema_version "$databaseUrl"); then
    log "could not read the schema version from DATABASE_URL; not deploying $sha"
    return 1
  fi
  # ENVOY_IMAGE_SCHEMA is <tag>:<version>, the schema version the deployed tag was running against
  # when the deployer first tried to replace it. Kept while that tag stays deployed, it is what a
  # rollback to it needs: a retry after a failed deploy that migrated starts from the moved schema.
  recorded=$(env_value ENVOY_IMAGE_SCHEMA)
  if [[ -n "$recorded" && "${recorded%:*}" == "$previous" ]]; then
    baseline="${recorded##*:}"
    if [[ ! "$baseline" =~ ^[0-9]+$ ]]; then
      log "ENVOY_IMAGE_SCHEMA in $COMPOSE_ENV is '$recorded', not <tag>:<version>; not deploying $sha"
      return 1
    fi
  else
    baseline="$before"
    if ! set_env_value ENVOY_IMAGE_SCHEMA "$previous:$baseline"; then
      log "could not record the schema version $previous runs against; not deploying $sha"
      return 1
    fi
  fi
  dump="$BACKUPS/pre-${sha:0:12}-$(date -u +%Y%m%dT%H%M%SZ).dump"
  # `-e DATABASE_URL` with no value: docker reads it from this process's environment, so the
  # password never appears in argv (/proc/<pid>/cmdline is world-readable on a shared host).
  if ! DATABASE_URL="$databaseUrl" docker run --rm --network host -e DATABASE_URL "$PG_DUMP_IMAGE" sh -c 'pg_dump -Fc "$DATABASE_URL"' >"$dump"; then
    log "backup failed; not deploying $sha"
    rm -f "$dump"
    return 1
  fi
  prune_backups
  if ! compose_with_tag "$sha" 900 pull -q dispatch || ! compose_with_tag "$sha" 300 up -d --no-deps dispatch; then
    log "compose failed for $sha"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$databaseUrl" "$dispatchPort"
    return 1
  fi
  if ! wait_for_health "$dispatchPort"; then
    log "deployed $sha but /healthz did not answer within 90s"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$databaseUrl" "$dispatchPort"
    return 1
  fi
  redirect=$(oauth_redirect_uri "$origin" "$dispatchPort")
  expected="$origin/auth/callback"
  if [[ "$redirect" != "$expected" ]]; then
    log "deployed $sha but the OAuth redirect_uri is '$redirect', expected '$expected' - login would break"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$databaseUrl" "$dispatchPort"
    return 1
  fi
  if ! sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$sha|" "$COMPOSE_ENV"; then
    log "could not persist deployed tag $sha"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$databaseUrl" "$dispatchPort"
    return 1
  fi
  log "deployed $sha ($(jj -R "$REPO" log -r "$sha" --no-graph -T 'description.first_line()' | cut -c1-90)); login origin $origin verified; backup $dump"
}

run_once() {
  local sha
  if sha=$(newest_published_main); then
    if [[ "$sha" != "$(deployed_tag)" ]]; then
      log "main has a newer published image: $sha (deployed $(deployed_tag))"
      deploy "$sha"
    fi
  else
    log "no published image among the last 30 main commits"
  fi
}

if [[ "$once" == true ]]; then
  run_once
  exit 0
fi

log "autodeploy started; deployed=$(deployed_tag)"
while true; do
  run_once
  sleep "$POLL"
done
