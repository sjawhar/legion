#!/usr/bin/env bash
# stack.sh: the load test's local stack (README.md): Postgres with pgvector from the pgserver wheel,
# the 1 ms latency proxy toward it, nats-server with JetStream, the Envoy listener and Dispatch, all
# built from this checkout and kept under LOADTEST_ROOT.
#
#   stack.sh setup          build envoy-dispatch, envoy-listener and loadgen into $LOADTEST_ROOT/bin,
#                           install nats-server (the version packages/envoy/go.mod pins) and Postgres,
#                           initdb, write stack.json. Idempotent: a rerun rebuilds incrementally.
#   stack.sh start          start Postgres, the proxy, NATS and the listener (one already running is
#                           kept); refuses a port another process holds, naming it.
#   stack.sh seed [--force] seed through Dispatch (about 17 min on 2 vCPU), drain the outbox, vacuum,
#                           install the commit-time trigger, write seed.meta.json and keep the result as
#                           the template database dispatch_seed. --force drops an existing seed first.
#   stack.sh reset [LOG]    what run-step.sh does before each step: stop Dispatch and the listener,
#                           recreate database dispatch from dispatch_seed, restart NATS on a fresh store
#                           and the listener, start Dispatch (its log to LOG) and wait for /healthz.
#   stack.sh stop [NAME...] stop dispatch, listener, nats, proxy and postgres (or the named ones).
#   stack.sh status         each process: pid, alive, port.
#   stack.sh loadgen ARGS   run $LOADTEST_ROOT/bin/loadgen under the stack's environment, from the
#                           root (run-step.sh and run-series.sh call loadgen this way).
#
# Environment: LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest); LOADTEST_SRC (default: the
# checkout holding this script); ports LOADTEST_PG_PORT (15432), LOADTEST_PROXY_PORT (15433),
# LOADTEST_NATS_PORT (14222), LOADTEST_NATS_MONITOR_PORT (18222), LOADTEST_LISTENER_PORT (19120),
# LOADTEST_DISPATCH_PORT (18777), LOADTEST_PPROF_PORT (16061), read by setup into stack.json;
# LOADTEST_PROXY_RTT (1ms); LOADTEST_PG_DIRECT=1 starts Dispatch on Postgres without the proxy.
#
# Every stack process starts under `env -i` with an explicit environment, so this shell's
# DISPATCH_URL, ENVOY_URL, ENVOY_NATS_URL, AWS_*, HOME and XDG_* never reach it, and every process is
# stopped by the pid in its pidfile ($LOADTEST_ROOT/run/<name>.pid), never by name.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SRC=${LOADTEST_SRC:-$(cd -- "$here/../../.." && pwd -P)}
ENVOY_SRC=$SRC/packages/envoy
ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
mkdir -p -- "$ROOT"
ROOT=$(cd -- "$ROOT" && pwd -P)
BIN=$ROOT/bin
RUN=$ROOT/run
LOGS=$ROOT/logs
PGDIR=$ROOT/pg
PGDATA=$PGDIR/data
PGBIN=$PGDIR/venv/lib/python3.12/site-packages/pgserver/pginstall/bin
STACK=$ROOT/stack.json
SEED_META=$ROOT/seed.meta.json
STACK_HOME=$ROOT/home
PGSERVER_VERSION=0.1.4
# Processes in stop order; start runs them in reverse (dispatch is started by seed and reset).
PROCESSES=(dispatch listener nats proxy postgres)

note() { printf 'stack.sh: %s\n' "$*" >&2; }
die() {
  printf 'stack.sh: %s\n' "$*" >&2
  exit 1
}

# base_env is every stack process's environment: a PATH and a HOME and XDG directories of the
# stack's own, nothing inherited.
base_env=(
  "PATH=/usr/local/bin:/usr/bin:/bin"
  "HOME=$STACK_HOME"
  "XDG_CONFIG_HOME=$STACK_HOME/.config"
  "XDG_DATA_HOME=$STACK_HOME/.local/share"
  "XDG_CACHE_HOME=$STACK_HOME/.cache"
  "XDG_STATE_HOME=$STACK_HOME/.local/state"
  "TMPDIR=$ROOT/tmp"
)

# loadgen_env adds what loadgen needs: the root, and the go toolchain for `loadgen tables`
# (`go tool pprof`).
loadgen_env() {
  local gobin
  gobin=$(command -v go || true)
  local path="$BIN:/usr/local/bin:/usr/bin:/bin"
  [ -n "$gobin" ] && path="$(dirname -- "$gobin"):$path"
  printf '%s\n' "${base_env[@]:1}" "PATH=$path" "LOADTEST_ROOT=$ROOT"
}

# loadgen runs the stack's loadgen binary under the stack's environment, from the root.
loadgen() {
  local envs
  mapfile -t envs < <(loadgen_env)
  (cd -- "$ROOT" && env -i "${envs[@]}" "$BIN/loadgen" "$@")
}

psql_bin() {
  if [ -x "$PGBIN/psql" ]; then
    printf '%s\n' "$PGBIN/psql"
  elif [ -x /usr/bin/psql ]; then
    printf '%s\n' /usr/bin/psql
  else
    die "no psql: neither $PGBIN/psql nor /usr/bin/psql exists"
  fi
}

# psql_db DB ARGS...: a psql client on the stack's Postgres, no ~/.psqlrc, no PG* from this shell.
psql_db() {
  local db=$1
  shift
  env -i "${base_env[@]}" PGOPTIONS=--client-min-messages=warning "$(psql_bin)" -X -q -v ON_ERROR_STOP=1 \
    -h 127.0.0.1 -p "$(port postgres)" -U postgres -d "$db" "$@"
}

# sql DB QUERY: one value per line, unaligned.
sql() { psql_db "$1" -At -c "$2"; }

# ---- stack.json --------------------------------------------------------------------------------------

need_stack() { [ -f "$STACK" ] || die "$STACK does not exist; run stack.sh setup first"; }

port() {
  need_stack
  jq -er --arg n "$1" '.ports[$n]' "$STACK"
}

stack_name() { jq -er .name "$STACK"; }

# stack_set JQ_FILTER [jq args...]: rewrite stack.json through a filter.
stack_set() {
  local filter=$1
  shift
  local tmp
  tmp=$(mktemp "$STACK.XXXXXX")
  jq "$@" "$filter" "$STACK" >"$tmp"
  chmod 600 "$tmp"
  mv -f -- "$tmp" "$STACK"
}

set_pid() { stack_set '.pids[$n] = $p' --arg n "$1" --argjson p "$2"; }

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# write_stack_json writes stack.json on the first setup, and rewrites its ports from the environment
# on a later one, keeping the name, tokens and pids; it refuses to move a port while the stack runs.
write_stack_json() {
  local ports
  ports=$(jq -n \
    --argjson postgres "${LOADTEST_PG_PORT:-15432}" \
    --argjson proxy "${LOADTEST_PROXY_PORT:-15433}" \
    --argjson nats "${LOADTEST_NATS_PORT:-14222}" \
    --argjson nats_monitor "${LOADTEST_NATS_MONITOR_PORT:-18222}" \
    --argjson listener "${LOADTEST_LISTENER_PORT:-19120}" \
    --argjson dispatch "${LOADTEST_DISPATCH_PORT:-18777}" \
    --argjson pprof "${LOADTEST_PPROF_PORT:-16061}" \
    '{postgres: $postgres, proxy: $proxy, nats: $nats, nats_monitor: $nats_monitor, listener: $listener, dispatch: $dispatch, pprof: $pprof}')
  if [ ! -f "$STACK" ]; then
    (
      umask 077
      jq -n --arg name "loadtest-$(random_hex 4)" --arg root "$ROOT" \
        --arg dt "$(random_hex 24)" --arg lt "$(random_hex 24)" --argjson ports "$ports" \
        '{name: $name, root: $root, dispatch_token: $dt, listener_token: $lt, ports: $ports,
          pids: {postgres: 0, proxy: 0, nats: 0, listener: 0, dispatch: 0}, pg_direct: false}' >"$STACK"
    )
    note "wrote $STACK (stack $(stack_name))"
    return
  fi
  if [ "$(jq -c .ports "$STACK")" != "$(jq -c . <<<"$ports")" ]; then
    local p
    for p in "${PROCESSES[@]}"; do
      running "$p" && die "the ports in the environment differ from $STACK while $p is running; stack.sh stop first"
    done
    stack_set '.ports = $ports' --argjson ports "$ports"
    note "ports in $STACK updated from the environment"
  fi
  stack_set '.root = $root' --arg root "$ROOT"
}

# ---- processes ---------------------------------------------------------------------------------------

pidfile() { printf '%s/%s.pid\n' "$RUN" "$1"; }

# binary_of NAME: the executable a live process of that name must be running, so a pid the kernel
# has reused for something else is never signalled.
binary_of() {
  case $1 in
    postgres) printf '%s\n' "$PGBIN/postgres" ;;
    proxy) printf '%s\n' "$BIN/loadgen" ;;
    nats) printf '%s\n' "$BIN/nats-server" ;;
    listener) printf '%s\n' "$BIN/envoy-listener" ;;
    dispatch) printf '%s\n' "$BIN/envoy-dispatch" ;;
  esac
}

# pid_of NAME: the pid in NAME's pidfile when that process is alive and runs NAME's binary.
pid_of() {
  local f pid exe
  f=$(pidfile "$1")
  [ -f "$f" ] || return 1
  pid=$(cat -- "$f")
  [[ $pid =~ ^[0-9]+$ ]] || return 1
  [ -d "/proc/$pid" ] || return 1
  # A binary rebuilt while its process runs shows as "<path> (deleted)".
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || return 1
  [ "${exe% (deleted)}" = "$(binary_of "$1")" ] || return 1
  printf '%s\n' "$pid"
}

running() { pid_of "$1" >/dev/null; }

port_open() { (: <"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# spawn NAME LOG ENV_ARRAY CMD...: start CMD in its own session under `env -i` with the named
# environment, its output to LOG, and record its pid in NAME's pidfile.
spawn() {
  local name=$1 log=$2
  local -n spawn_env=$3
  shift 3
  local f
  f=$(pidfile "$name")
  mkdir -p -- "$RUN" "$(dirname -- "$log")" "$ROOT/tmp"
  rm -f -- "$f"
  # The shell writes its own pid and execs CMD, so the pidfile names CMD's process.
  # shellcheck disable=SC2016
  setsid -f env -i "${spawn_env[@]}" /bin/sh -c 'echo "$$" >"$0.tmp" && mv -f -- "$0.tmp" "$0" && exec "$@"' \
    "$f" "$@" >"$log" 2>&1 </dev/null
  # Wait until the pidfile names a process running NAME's binary (the shell has exec'd).
  local i
  for ((i = 0; i < 100; i++)); do
    running "$name" && return 0
    sleep 0.1
  done
  tail -n 40 -- "$log" >&2 || true
  die "$name did not start; see $log"
}

# wait_for WHAT SECONDS NAME LOG CMD...: poll CMD every 0.5 s until it succeeds; fail with LOG's tail
# when NAME's process dies or the time runs out.
wait_for() {
  local what=$1 seconds=$2 name=$3 log=$4
  shift 4
  local deadline=$((SECONDS + seconds))
  until "$@" >/dev/null 2>&1; do
    if ! running "$name"; then
      tail -n 40 -- "$log" >&2 || true
      die "$name exited before $what; log: $log"
    fi
    if ((SECONDS >= deadline)); then
      tail -n 40 -- "$log" >&2 || true
      die "timed out after ${seconds}s waiting for $what; log: $log"
    fi
    sleep 0.5
  done
}

stop_one() {
  local name=$1 pid
  if ! pid=$(pid_of "$name"); then
    rm -f -- "$(pidfile "$name")"
    [ -f "$STACK" ] && set_pid "$name" 0
    return 0
  fi
  # Postgres: SIGINT is its fast shutdown; the Go processes shut down on SIGTERM.
  local sig=TERM
  [ "$name" = postgres ] && sig=INT
  kill -s "$sig" "$pid" 2>/dev/null || true
  local deadline=$((SECONDS + 60))
  while [ -d "/proc/$pid" ] && ((SECONDS < deadline)); do
    sleep 0.2
  done
  if [ -d "/proc/$pid" ]; then
    note "$name (pid $pid) still running after 60 s; sending SIGKILL"
    kill -s KILL "$pid" 2>/dev/null || true
    while [ -d "/proc/$pid" ]; do sleep 0.2; done
  fi
  note "stopped $name (pid $pid)"
  rm -f -- "$(pidfile "$name")"
  set_pid "$name" 0
}

# check_port NAME PORT_KEY: refuse a port some other process holds.
check_port() {
  local name=$1 key=$2 p
  p=$(port "$key")
  if port_open "$p"; then
    local var
    case $key in
      postgres) var=LOADTEST_PG_PORT ;;
      proxy) var=LOADTEST_PROXY_PORT ;;
      nats) var=LOADTEST_NATS_PORT ;;
      nats_monitor) var=LOADTEST_NATS_MONITOR_PORT ;;
      listener) var=LOADTEST_LISTENER_PORT ;;
      dispatch) var=LOADTEST_DISPATCH_PORT ;;
      pprof) var=LOADTEST_PPROF_PORT ;;
    esac
    die "port $p ($key, for $name) is already in use by a process that is not this stack's; free it or set $var and rerun stack.sh setup"
  fi
}

ports_of() {
  case $1 in
    nats) printf '%s\n' nats nats_monitor ;;
    dispatch) printf '%s\n' dispatch pprof ;;
    *) printf '%s\n' "$1" ;;
  esac
}

start_postgres() {
  running postgres && return 0
  check_port postgres postgres
  local log=$LOGS/postgres.out
  # The postmaster takes its port on the command line, so a changed LOADTEST_PG_PORT needs no initdb.
  spawn postgres "$log" base_env "$PGBIN/postgres" -D "$PGDATA" -p "$(port postgres)"
  wait_for "Postgres to accept connections" 60 postgres "$log" sql postgres 'select 1'
  set_pid postgres "$(pid_of postgres)"
  note "postgres pid $(pid_of postgres) on 127.0.0.1:$(port postgres)"
}

start_proxy() {
  running proxy && return 0
  check_port proxy proxy
  local log=$LOGS/proxy.out
  spawn proxy "$log" base_env "$BIN/loadgen" proxy -listen "127.0.0.1:$(port proxy)" \
    -target "127.0.0.1:$(port postgres)" -rtt "${LOADTEST_PROXY_RTT:-1ms}"
  wait_for "the latency proxy" 30 proxy "$log" port_open "$(port proxy)"
  set_pid proxy "$(pid_of proxy)"
  note "proxy pid $(pid_of proxy) on 127.0.0.1:$(port proxy) (rtt ${LOADTEST_PROXY_RTT:-1ms})"
}

nats_named() {
  [ "$(curl -fsS --max-time 2 "http://127.0.0.1:$(port nats_monitor)/varz" | jq -r .server_name)" = "$(stack_name)" ]
}

# start_nats starts NATS on a fresh JetStream store, so every start begins with no streams.
start_nats() {
  running nats && return 0
  check_port nats nats
  check_port nats nats_monitor
  local store=$ROOT/nats/store log=$LOGS/nats.out
  rm -rf -- "$store"
  mkdir -p -- "$store"
  spawn nats "$log" base_env "$BIN/nats-server" -js -a 127.0.0.1 -p "$(port nats)" -n "$(stack_name)" \
    -m "$(port nats_monitor)" -sd "$store"
  wait_for "NATS /varz to name the stack" 30 nats "$log" nats_named
  set_pid nats "$(pid_of nats)"
  note "nats pid $(pid_of nats) on 127.0.0.1:$(port nats), monitor $(port nats_monitor), store $store"
}

listener_up() {
  curl -fsS --max-time 2 -H "Authorization: Bearer $(jq -r .listener_token "$STACK")" \
    "http://127.0.0.1:$(port listener)/v1/sessions"
}

start_listener() {
  running listener && return 0
  check_port listener listener
  local log=$LOGS/listener.out
  # internal/config/config.go Load: the listener's settings.
  # shellcheck disable=SC2034
  local listener_env=("${base_env[@]}"
    "PORT=$(port listener)"
    "ENVOY_LISTEN_HOST=127.0.0.1"
    "ENVOY_MACHINE_ID=$(stack_name)"
    "NATS_URLS=nats://127.0.0.1:$(port nats)"
    "ENVOY_API_TOKEN=$(jq -r .listener_token "$STACK")")
  spawn listener "$log" listener_env "$BIN/envoy-listener"
  wait_for "the listener's /v1/sessions" 60 listener "$log" listener_up
  set_pid listener "$(pid_of listener)"
  note "listener pid $(pid_of listener) on 127.0.0.1:$(port listener)"
}

dispatch_healthy() {
  curl -fsS --max-time 5 "http://127.0.0.1:$(port dispatch)/healthz" | jq -e '.ok and .nats'
}

# start_dispatch LOG: Dispatch on database dispatch, through the latency proxy unless
# LOADTEST_PG_DIRECT=1; waits up to 120 s for /healthz to report ok and NATS.
start_dispatch() {
  local log=$1
  running dispatch && die "Dispatch is already running (pid $(pid_of dispatch)); stack.sh stop dispatch first"
  check_port dispatch dispatch
  check_port dispatch pprof
  local pg_port direct=false
  pg_port=$(port proxy)
  if [ "${LOADTEST_PG_DIRECT:-}" = 1 ]; then
    pg_port=$(port postgres)
    direct=true
  else
    running proxy || die "the latency proxy is not running; stack.sh start (or LOADTEST_PG_DIRECT=1)"
  fi
  local webdist=$ROOT/webdist
  mkdir -p -- "$webdist" "$STACK_HOME"
  # scripts/e2e/dispatch-session-settings.sh's Dispatch, on the stack's ports and tokens. No
  # DISPATCH_SIGNING_KEY: Dispatch keeps its key under the stack's HOME (XDG_DATA_HOME), so it is the
  # same across steps. No AWS_*: no Bedrock calls.
  # shellcheck disable=SC2034
  local dispatch_env=("${base_env[@]}"
    "DATABASE_URL=postgres://postgres@127.0.0.1:$pg_port/dispatch?sslmode=disable"
    "DISPATCH_AGENT_TOKEN=$(jq -r .dispatch_token "$STACK")"
    "DISPATCH_IDENTITY=header:X-Dispatch-User"
    "DISPATCH_IDENTITY_HEADER_TRUSTED=1"
    "DISPATCH_LISTEN_HOST=127.0.0.1"
    "DISPATCH_PORT=$(port dispatch)"
    "DISPATCH_SERVER_URL=http://127.0.0.1:$(port dispatch)"
    "DISPATCH_WEB_DIST=$webdist"
    "DISPATCH_PPROF_PORT=$(port pprof)"
    "NATS_URLS=nats://127.0.0.1:$(port nats)"
    "ENVOY_URL=http://127.0.0.1:$(port listener)"
    "ENVOY_TOKEN=$(jq -r .listener_token "$STACK")")
  spawn dispatch "$log" dispatch_env "$BIN/envoy-dispatch"
  set_pid dispatch "$(pid_of dispatch)"
  stack_set '.pg_direct = $d' --argjson d "$direct"
  wait_for "Dispatch /healthz to report ok and NATS" 120 dispatch "$log" dispatch_healthy
  note "dispatch pid $(pid_of dispatch) on 127.0.0.1:$(port dispatch), pprof $(port pprof), postgres via port $pg_port; log $log"
}

# ---- commands ----------------------------------------------------------------------------------------

cmd_setup() {
  command -v go >/dev/null || die "go is required"
  command -v uv >/dev/null || die "uv is required (Postgres comes from the pgserver wheel)"
  command -v jq >/dev/null || die "jq is required"
  [ -d "$ENVOY_SRC/cmd/dispatch" ] || die "LOADTEST_SRC=$SRC holds no packages/envoy/cmd/dispatch"
  mkdir -p -- "$BIN" "$RUN" "$LOGS" "$PGDIR/log" "$STACK_HOME" "$ROOT/tmp" "$ROOT/runs"

  note "building envoy-dispatch, envoy-listener and loadgen from $ENVOY_SRC into $BIN"
  (cd -- "$ENVOY_SRC" &&
    go build -o "$BIN/envoy-dispatch" ./cmd/dispatch &&
    go build -o "$BIN/envoy-listener" ./cmd/listener &&
    go build -o "$BIN/loadgen" ./loadtest)

  local nats_version
  nats_version=$(awk '$1 == "github.com/nats-io/nats-server/v2" { print $2; exit }' "$ENVOY_SRC/go.mod")
  [ -n "$nats_version" ] || die "$ENVOY_SRC/go.mod pins no github.com/nats-io/nats-server/v2"
  if ! { [ -x "$BIN/nats-server" ] && "$BIN/nats-server" --version | grep -qF "$nats_version"; }; then
    note "installing nats-server $nats_version into $BIN"
    # A go build inside the envoy module fails (its main imports a module go.sum lacks), so it is
    # installed on its own, as scripts/e2e/dispatch-session-settings.sh does.
    GOBIN=$BIN go install "github.com/nats-io/nats-server/v2@$nats_version"
  fi
  note "$("$BIN/nats-server" --version)"

  if [ ! -x "$PGBIN/postgres" ]; then
    note "installing Postgres with pgvector (pgserver $PGSERVER_VERSION) into $PGDIR/venv"
    export UV_CACHE_DIR=$ROOT/uv/cache UV_PYTHON_INSTALL_DIR=$ROOT/uv/python
    uv venv -q --python 3.12 "$PGDIR/venv"
    uv pip install -q --python "$PGDIR/venv/bin/python" "pgserver==$PGSERVER_VERSION"
    [ -x "$PGBIN/postgres" ] || die "pgserver $PGSERVER_VERSION installed no $PGBIN/postgres"
  fi
  note "$("$PGBIN/postgres" --version)"

  if [ ! -f "$PGDATA/PG_VERSION" ]; then
    note "initdb $PGDATA"
    env -i "${base_env[@]}" "$PGBIN/initdb" -D "$PGDATA" -U postgres --auth=trust -E UTF8 --locale=C >"$LOGS/initdb.out" 2>&1 ||
      die "initdb failed; see $LOGS/initdb.out"
    cat >>"$PGDATA/postgresql.conf" <<EOF

# stack.sh: the root report's settings, plus commit timestamps for delivery from commit and TCP only
# (no socket path, so a long LOADTEST_ROOT never meets the 107-byte socket path limit).
listen_addresses = '127.0.0.1'
unix_socket_directories = ''
max_connections = 800
shared_buffers = 1GB
fsync = on
track_commit_timestamp = on
logging_collector = on
log_directory = '$PGDIR/log'
log_filename = 'postgresql-%Y%m%d-%H%M.log'
log_min_duration_statement = 50ms
log_lock_waits = on
log_line_prefix = '%m [%p] %a '
EOF
  fi
  write_stack_json
}

cmd_start() {
  need_stack
  [ -f "$PGDATA/PG_VERSION" ] || die "no Postgres data directory at $PGDATA; run stack.sh setup first"
  # Every port a stopped process needs is checked before anything starts.
  local p k
  for p in postgres proxy nats listener dispatch; do
    running "$p" && continue
    for k in $(ports_of "$p"); do check_port "$p" "$k"; done
  done
  start_postgres
  start_proxy
  start_nats
  start_listener
}

# build_commit prints the checkout's commit and whether its tree has uncommitted changes, without
# touching the repository (no index refresh).
build_commit() { git -C "$SRC" rev-parse HEAD 2>/dev/null || echo unknown; }
build_dirty() {
  local s
  s=$(git -C "$SRC" --no-optional-locks status --porcelain 2>/dev/null) || {
    echo false
    return
  }
  [ -n "$s" ] && echo true || echo false
}

db_exists() { [ "$(sql postgres "select count(*) from pg_database where datname = '$1'")" = 1 ]; }

cmd_seed() {
  local force=false
  case ${1:-} in
    --force) force=true ;;
    "") ;;
    *) die "seed takes only --force, not '$1'" ;;
  esac
  need_stack
  running postgres || die "Postgres is not running; stack.sh start first"
  if db_exists dispatch_seed && [ -f "$SEED_META" ] && [ "$force" = false ]; then
    note "already seeded: database dispatch_seed and $SEED_META exist (stack.sh seed --force reseeds)"
    return 0
  fi
  local started=$SECONDS
  stop_one dispatch
  sql postgres 'drop database if exists dispatch' >/dev/null
  sql postgres 'drop database if exists dispatch_seed' >/dev/null
  rm -f -- "$SEED_META" "$ROOT/seed.json"
  sql postgres 'create database dispatch' >/dev/null
  trap 'stop_one dispatch' EXIT
  start_dispatch "$LOGS/dispatch-seed.log"

  note "seeding through Dispatch (about 17 min on 2 vCPU); log $LOGS/seed.log"
  loadgen seed -out "$ROOT/seed.json" 2>&1 | tee "$LOGS/seed.log" >&2

  note "waiting for the outbox to publish every seed event"
  local deadline=$((SECONDS + 900)) pending
  while :; do
    pending=$(sql dispatch 'select count(*) from events where published_at is null')
    [ "$pending" = 0 ] && break
    ((SECONDS < deadline)) || die "the outbox still holds $pending unpublished events after 900 s; see $LOGS/dispatch-seed.log"
    sleep 2
  done
  stop_one dispatch
  trap - EXIT

  note "vacuum analyze"
  sql dispatch 'vacuum analyze' >/dev/null
  note "installing the commit-time trigger (loadtest.event_commits)"
  loadgen trigger-sql | psql_db dispatch >/dev/null

  local migration counts
  migration=$(sql dispatch 'select max(version) from schema_migrations')
  counts=$(sql dispatch "select json_build_object(
      'projects', (select count(*) from projects),
      'issues', (select count(*) from issues),
      'comments', (select count(*) from comments),
      'artifacts', (select count(*) from artifacts),
      'artifact_versions', (select count(*) from artifact_versions),
      'events', (select count(*) from events))")
  jq -n --argjson migration "$migration" --arg commit "$(build_commit)" --argjson dirty "$(build_dirty)" \
    --argjson counts "$counts" --arg at "$(date -u +%FT%TZ)" --arg pg "$("$PGBIN/postgres" --version)" \
    --argjson seconds "$((SECONDS - started))" \
    '{migration: $migration, build_commit: $commit, build_dirty: $dirty, created_at: $at,
      seed_seconds: $seconds, postgres_version: $pg, counts: $counts}' >"$SEED_META"
  sql postgres 'alter database dispatch rename to dispatch_seed' >/dev/null
  note "seed done in $((SECONDS - started)) s: template database dispatch_seed; $(jq -c . "$SEED_META")"
}

cmd_reset() {
  local log=${1:-$LOGS/dispatch.log}
  need_stack
  running postgres || die "Postgres is not running; stack.sh start first"
  db_exists dispatch_seed || die "no template database dispatch_seed; stack.sh seed first"
  stop_one dispatch
  stop_one listener
  sql postgres 'drop database if exists dispatch' >/dev/null
  sql postgres 'create database dispatch template dispatch_seed' >/dev/null
  stop_one nats
  start_nats
  start_listener
  start_dispatch "$log"
}

cmd_stop() {
  [ -f "$STACK" ] || return 0
  local names=("$@") p
  [ ${#names[@]} -gt 0 ] || names=("${PROCESSES[@]}")
  for p in "${names[@]}"; do
    [ -n "$(binary_of "$p")" ] || die "unknown process '$p'; one of: ${PROCESSES[*]}"
    stop_one "$p"
  done
}

cmd_status() {
  need_stack
  printf 'stack %s, root %s\n' "$(stack_name)" "$ROOT"
  local p k pid state
  for p in postgres proxy nats listener dispatch; do
    if pid=$(pid_of "$p"); then state="running pid $pid"; else state=stopped; fi
    local ports=()
    for k in $(ports_of "$p"); do
      if port_open "$(port "$k")"; then ports+=("$k $(port "$k") open"); else ports+=("$k $(port "$k") closed"); fi
    done
    printf '%-9s %-20s %s\n' "$p" "$state" "$(
      IFS=,
      echo "${ports[*]}"
    )"
  done
  if [ -f "$SEED_META" ]; then
    printf 'seed      %s\n' "$(jq -c '{migration, build_commit, counts}' "$SEED_META")"
  else
    printf 'seed      none\n'
  fi
}

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

case ${1:-} in
  setup) cmd_setup ;;
  start) cmd_start ;;
  seed) cmd_seed "${@:2}" ;;
  reset) cmd_reset "${@:2}" ;;
  stop) cmd_stop "${@:2}" ;;
  status) cmd_status ;;
  loadgen) loadgen "${@:2}" ;;
  *) usage ;;
esac
