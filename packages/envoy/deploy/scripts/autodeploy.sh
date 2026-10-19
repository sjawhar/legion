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

# database_url_split reads DATABASE_URL into db_url, the URL without its password, and db_password,
# that password percent-decoded. The database clients below get db_url as their argument and the
# password as PGPASSWORD, an environment variable, so no process's arguments carry a credential:
# a container's processes are the host's, and /proc/<pid>/cmdline is world-readable on a shared
# host. It accepts exactly one shape and fails on every other, printing nothing:
#
#   postgres[ql]://<user>[:<password>]@<host>[:<port>]/<database>[?<key>=<value>[&<key>=<value>]…]
#
#   - <user>, <database> and each <value>: ASCII letters, digits, `.`, `_` and `-`.
#   - <password>: ASCII letters, digits, `.`, `_`, `~` and `-`, and %XX escapes other than %00,
#     whose decoded bytes are valid UTF-8. Any other character (@ : / ? # [ ], %, a space,
#     non-ASCII) has to be percent-encoded.
#   - <host>: dot-separated labels of ASCII letters, digits and hyphens, none empty and none
#     starting or ending with a hyphen. That is a host name or an IPv4 address; no brackets, no
#     second host, no socket path.
#   - <port>: 1 to 65535.
#   - <key>: sslmode, connect_timeout or application_name.
#
# In that shape every reading of the URL, the deployer's, libpq's and pgx's, puts the password in
# the same place, and nowhere else. LC_ALL=C makes the ranges ASCII whatever the caller's locale:
# under en_US.UTF-8, [a-z] and [0-9] also match letters and digits outside ASCII, and under a
# double-byte locale such as GBK one character can hide an @.
database_url_split() {
  local LC_ALL=C
  local value='[A-Za-z0-9._-]+'
  local key='(sslmode|connect_timeout|application_name)'
  local escape='%(0[1-9A-Fa-f]|[1-9A-Fa-f][0-9A-Fa-f])'
  local label='[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?'
  local port='([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])'
  local grammar="^(postgres|postgresql)://$value(:([A-Za-z0-9._~-]|$escape)+)?@$label(\\.$label)*(:$port)?/$value(\\?$key=$value(&$key=$value)*)?\$"
  local userinfo
  local password=""
  if [[ ! "$1" =~ $grammar ]]; then
    return 1
  fi
  # An accepted URL holds exactly one @, so the user and password are before it, and the rest of
  # the URL, the clients' part, is after it.
  userinfo="${1#*://}"
  userinfo="${userinfo%%@*}"
  if [[ "$userinfo" == *:* ]]; then
    password="${userinfo#*:}"
  fi
  printf -v db_password '%b' "${password//%/\\x}"
  # Docker rewrites an environment value that is not valid UTF-8, so such a password would reach
  # the clients as different bytes than libpq or pgx read from the URL. The check reads it from a
  # pipe, never an argument.
  if ! printf '%s' "$db_password" | iconv -f UTF-8 -t UTF-8 >/dev/null 2>&1; then
    return 1
  fi
  db_url="${1%%://*}://${userinfo%%:*}@${1#*@}"
}

# pg_client runs shell command $2 in the pg_dump image, bounded at $1 seconds (0: unbounded), with
# the connection in its environment: DATABASE_URL (db_url) and PGPASSWORD (db_password), each passed
# to docker by name, so docker reads the value from this process's environment. --init runs the
# command under an init that passes on the signal docker forwards when `timeout` fires; as PID 1
# the shell would ignore it, and the container would outlive the bound.
pg_client() {
  PGPASSWORD="$db_password" DATABASE_URL="$db_url" timeout "$1" \
    docker run --init --rm --network host -e PGPASSWORD -e DATABASE_URL "$PG_DUMP_IMAGE" sh -c "$2"
}

# schema_version prints the highest migration the database has applied, the `schema_version`
# Dispatch's /healthz reports. It first takes the advisory lock Dispatch's migration runner holds
# for each migration (internal/dispatch/store/store.go, migrationLockKey), in a statement of its
# own, so a migration transaction still in flight, one a stopped image sent its commit for
# included, ends before the read takes its snapshot. psql's stderr is dropped, since its
# diagnostics can quote the connection; a failure prints nothing and returns psql's status, or 1
# for an answer that is no version.
schema_version() {
  local version
  # shellcheck disable=SC2016 # $DATABASE_URL expands in the container's shell, from its environment
  version=$(pg_client 60 'psql -X -q -At -v ON_ERROR_STOP=1 -c "set lock_timeout = 30000" -c "select pg_advisory_lock(8150001)" -c "select coalesce(max(version), 0) from schema_migrations" "$DATABASE_URL"' 2>/dev/null) ||
    return
  version="${version##*$'\n'}"
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
# (packages/envoy/cmd/dispatch/README.md, "Database migrations"). So it stops $sha first and
# confirms the stop, then reads the schema, so nothing $sha runs can move it after the read. Once the
# schema has moved, or the stop or the read fails, it starts nothing and fails, for an operator to
# recover forward.
roll_back_or_stop() {
  local sha="$1"
  local previous="$2"
  local baseline="$3"
  local dispatchPort="$4"
  local running
  local now
  local status
  local refusing="so refusing to roll back to $previous, which runs against schema $baseline, and forward recovery is required"
  if ! compose 120 stop dispatch; then
    log "could not stop $sha, $refusing"
    return 1
  fi
  if ! running=$(compose 60 ps --status running -q dispatch) || [[ -n "$running" ]]; then
    log "could not confirm $sha stopped, $refusing"
    return 1
  fi
  now=$(schema_version) || {
    status=$?
    log "could not read the schema version after stopping $sha (status $status), $refusing"
    return 1
  }
  if ((now > baseline)); then
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
  local db_url
  local db_password
  local dispatchPort
  local status
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
  if ! database_url_split "$databaseUrl"; then
    log "DATABASE_URL in $COMPOSE_ENV is not in the one form the deployer accepts, the only one in which it can keep the password off its database clients' process arguments; see packages/envoy/deploy/README.md, \"Auto-deployer\", for the form; not deploying $sha"
    return 1
  fi
  dispatchPort=$(env_value DISPATCH_PORT)
  dispatchPort="${dispatchPort:-8766}"
  before=$(schema_version) || {
    status=$?
    log "could not read the schema version (status $status); not deploying $sha"
    return 1
  }
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
  # shellcheck disable=SC2016 # $DATABASE_URL expands in the container's shell, from its environment
  if ! pg_client 0 'pg_dump -Fc "$DATABASE_URL"' >"$dump"; then
    log "backup failed; not deploying $sha"
    rm -f "$dump"
    return 1
  fi
  prune_backups
  if ! compose_with_tag "$sha" 900 pull -q dispatch; then
    log "could not pull $sha; $previous still serves"
    return 1
  fi
  if ! compose_with_tag "$sha" 300 up -d --no-deps dispatch; then
    log "compose up failed for $sha"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$dispatchPort"
    return 1
  fi
  if ! wait_for_health "$dispatchPort"; then
    log "deployed $sha but /healthz did not answer within 90s"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$dispatchPort"
    return 1
  fi
  redirect=$(oauth_redirect_uri "$origin" "$dispatchPort")
  expected="$origin/auth/callback"
  if [[ "$redirect" != "$expected" ]]; then
    log "deployed $sha but the OAuth redirect_uri is '$redirect', expected '$expected' - login would break"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$dispatchPort"
    return 1
  fi
  if ! sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$sha|" "$COMPOSE_ENV"; then
    log "could not persist deployed tag $sha"
    roll_back_or_stop "$sha" "$previous" "$baseline" "$dispatchPort"
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
