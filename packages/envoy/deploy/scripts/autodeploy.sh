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
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-${COMPOSE_PROJECT}-postgres-1}"

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

deploy() {
  local sha="$1"
  local previous
  local postgresPort
  local dispatchPort
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
  postgresPort=$(env_value DISPATCH_PG_PORT)
  postgresPort="${postgresPort:-55432}"
  dispatchPort=$(env_value DISPATCH_PORT)
  dispatchPort="${dispatchPort:-8766}"
  dump="$BACKUPS/pre-${sha:0:12}-$(date -u +%Y%m%dT%H%M%SZ).dump"
  if ! docker exec -e "PORT=$postgresPort" "$POSTGRES_CONTAINER" sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h 127.0.0.1 -p "$PORT" -U postgres -Fc dispatch' >"$dump"; then
    log "backup failed; not deploying $sha"
    rm -f "$dump"
    return 1
  fi
  prune_backups
  if ! compose_with_tag "$sha" 900 pull -q dispatch || ! compose_with_tag "$sha" 300 up -d --no-deps dispatch; then
    log "compose failed for $sha; rolling back to $previous"
    rollback "$previous" "$dispatchPort"
    return 1
  fi
  if ! wait_for_health "$dispatchPort"; then
    log "deployed $sha but /healthz did not answer within 90s; rolling back to $previous"
    rollback "$previous" "$dispatchPort"
    return 1
  fi
  redirect=$(oauth_redirect_uri "$origin" "$dispatchPort")
  expected="$origin/auth/callback"
  if [[ "$redirect" != "$expected" ]]; then
    log "deployed $sha but the OAuth redirect_uri is '$redirect', expected '$expected' - login would break; rolling back to $previous"
    rollback "$previous" "$dispatchPort"
    return 1
  fi
  if ! sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$sha|" "$COMPOSE_ENV"; then
    log "could not persist deployed tag $sha; rolling back to $previous"
    rollback "$previous" "$dispatchPort"
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
