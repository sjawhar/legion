#!/usr/bin/env bash
# stack.sh: the load test's local stack (README.md): Postgres with pgvector from the pgserver wheel,
# the 1 ms latency proxy toward it, nats-server with JetStream, the Envoy listener and Dispatch, all
# built from this checkout and kept under LOADTEST_ROOT.
#
#   stack.sh setup          build envoy-dispatch, envoy-listener and loadgen into $LOADTEST_ROOT/bin,
#                           install nats-server (the version packages/envoy/go.mod pins) and Postgres,
#                           initdb, write stack.json. Idempotent: a rerun rebuilds incrementally.
#                           Writes bin/envoy-dispatch.json, the build's record (below). With
#                           LOADTEST_DISPATCH_COMMIT=<rev> envoy-dispatch alone is built from that
#                           commit instead (a rev that names no commit is refused before any build).
#   stack.sh start          start Postgres, the proxy, NATS and the listener (one already running is
#                           kept); refuses a port another process holds, naming it.
#   stack.sh seed [--force] seed through Dispatch (18 to 27 min on 2 vCPU), drain the outbox, vacuum,
#                           install the commit-time trigger, write seed.meta.json and keep the result as
#                           the template database dispatch_seed. --force drops an existing seed first.
#   stack.sh reset [LOG]    what run-step.sh does before each step: stop Dispatch and the listener,
#                           recreate database dispatch from dispatch_seed, restart NATS on a fresh store
#                           and the listener, start Dispatch (its log to LOG) and wait for /healthz.
#   stack.sh stop [NAME...] stop dispatch, listener, nats, proxy and postgres (or the named ones).
#   stack.sh status         each process: pid, alive, port.
#   stack.sh build-info     print this checkout's commit and whether its tree has uncommitted changes
#                           ("<commit> <true|false>"; run-step.sh records them in meta.json).
#   stack.sh loadgen ARGS   run $LOADTEST_ROOT/bin/loadgen under the stack's environment, from the
#                           root (run-step.sh and run-series.sh call loadgen this way).
#   stack.sh replay-seed-patch-check BUNDLE_DIR
#                           build Dispatch at the replay seed's commit with replay-seed-len16.patch
#                           into bin/dispatch-seed and run the patch's tests there (TestLen16Seed),
#                           with every *.md under BUNDLE_DIR as the corpus; exits with their status.
#   stack.sh replay-seed --bundle DIR [--keys-file FILE] [--stock] [--resume] [--out DIR] [-- ARGS]
#                           the LINUX replay's seed: build Dispatch at 35efd326 (production's commit on
#                           2026-10-26, migration 0114) with replay-seed-len16.patch (--stock: without)
#                           into bin/dispatch-seed[-stock]; on database dispatch_replay_build (dropped
#                           and recreated, or kept with --resume) start it while Postgres, the proxy,
#                           NATS and the listener run, run `loadgen replay-seed -bundle DIR -db
#                           dispatch_replay_build [-keys-file FILE] ARGS`, wait for Dispatch to owe no
#                           work and go idle, stop it, vacuum, pg_dump the database (the smallest of
#                           gzip:9 and the zstd levels this pg_dump accepts), split it into 24 MiB
#                           parts linux-replay-seed-0114.dump.partNN under DIR of --out (default
#                           $LOADTEST_ROOT/replay-seed) and write seed.meta.json there.
#
# A Dispatch built from another commit (build_dispatch_at) is built in a `git archive` export of that
# commit's packages/envoy under $LOADTEST_ROOT/dispatch-src/<commit>[-<patch sha256, 12 hex>], with
# GOWORK=off GOTOOLCHAIN=local; a tree without cmd/dispatch/pprof.go gets zz_loadtest_pprof.go.txt as
# cmd/dispatch/zz_loadtest_pprof.go, which serves pprof on LOADTEST_DISPATCH_PPROF_PORT. Every
# Dispatch binary has a <binary>.json beside it: dispatch_commit, dispatch_tree, build_dirty, patch,
# patch_sha256, dispatch_pprof_overlay, dispatch_binary_sha256, go_version, newest_migration.
#
# Environment: LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest); LOADTEST_SRC (default: the
# checkout holding this script); ports LOADTEST_PG_PORT (15432), LOADTEST_PROXY_PORT (15433),
# LOADTEST_NATS_PORT (14222), LOADTEST_NATS_MONITOR_PORT (18222), LOADTEST_LISTENER_PORT (19120),
# LOADTEST_DISPATCH_PORT (18777), LOADTEST_PPROF_PORT (16061), read by setup into stack.json;
# LOADTEST_PROXY_RTT (1ms); LOADTEST_PG_DIRECT=1 starts Dispatch on Postgres without the proxy;
# LOADTEST_DISPATCH_COMMIT (setup, above); LOADTEST_GOMAXPROCS, Dispatch's GOMAXPROCS (default: nproc).
#
# Every stack process starts under `env -i` with an explicit environment, so this shell's
# DISPATCH_URL, ENVOY_URL, ENVOY_NATS_URL, AWS_*, HOME and XDG_* never reach it, and every process is
# stopped by the pid in its pidfile ($LOADTEST_ROOT/run/<name>.pid), never by name.
#
# setup, start, seed, reset, stop, replay-seed-patch-check and replay-seed take the root's run lock
# (common.sh) for their run, and are refused while another run holds it; run-step.sh's and
# run-series.sh's own calls pass. status, build-info and loadgen take no lock.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/common.sh
source "$here/common.sh"
resolve_paths
ENVOY_SRC=$SRC/packages/envoy
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
# The LINUX replay's seed: Dispatch at production's commit on 2026-10-26 (newest migration 0114,
# no cmd/dispatch/pprof.go), with the allocation-free len16 patch unless --stock, on its own database.
REPLAY_COMMIT=35efd326b66aea9ee2476396dabab7f7685fdbf3
REPLAY_MIGRATION=114
REPLAY_PATCH=$here/replay-seed-len16.patch
REPLAY_DB=dispatch_replay_build
PPROF_OVERLAY=$here/zz_loadtest_pprof.go.txt
# The binaries a dispatch process may run: the stack's own and the replay seed's two builds.
DISPATCH_BINARIES=("$BIN/envoy-dispatch" "$BIN/dispatch-seed" "$BIN/dispatch-seed-stock")
# Processes in stop order, and in start order (dispatch is started by seed and reset).
PROCESSES=(dispatch listener nats proxy postgres)
START_ORDER=(postgres proxy nats listener dispatch)
# The stack.json port keys in their order, each one's environment variable and its default.
PORT_KEYS=(postgres proxy nats nats_monitor listener dispatch pprof)
declare -A PORT_VAR=(
  [postgres]=LOADTEST_PG_PORT [proxy]=LOADTEST_PROXY_PORT [nats]=LOADTEST_NATS_PORT
  [nats_monitor]=LOADTEST_NATS_MONITOR_PORT [listener]=LOADTEST_LISTENER_PORT
  [dispatch]=LOADTEST_DISPATCH_PORT [pprof]=LOADTEST_PPROF_PORT
)
declare -A PORT_DEFAULT=(
  [postgres]=15432 [proxy]=15433 [nats]=14222 [nats_monitor]=18222 [listener]=19120
  [dispatch]=18777 [pprof]=16061
)

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
  local ports k var args=()
  for k in "${PORT_KEYS[@]}"; do
    var=${PORT_VAR[$k]}
    args+=(--argjson "$k" "${!var:-${PORT_DEFAULT[$k]}}")
  done
  ports=$(jq -n "${args[@]}" '$ARGS.named')
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

# binary_of NAME: the executables a live process of that name may be running, one a line, so a pid
# the kernel has reused for something else is never signalled. A dispatch process runs the stack's
# envoy-dispatch or one of the replay seed's builds.
binary_of() {
  case $1 in
    postgres) printf '%s\n' "$PGBIN/postgres" ;;
    proxy) printf '%s\n' "$BIN/loadgen" ;;
    nats) printf '%s\n' "$BIN/nats-server" ;;
    listener) printf '%s\n' "$BIN/envoy-listener" ;;
    dispatch) printf '%s\n' "${DISPATCH_BINARIES[@]}" ;;
  esac
}

# pid_of NAME: the pid in NAME's pidfile when that process is alive and runs one of NAME's binaries.
pid_of() {
  local f pid exe
  f=$(pidfile "$1")
  [ -f "$f" ] || return 1
  pid=$(cat -- "$f")
  [[ $pid =~ ^[0-9]+$ ]] || return 1
  [ -d "/proc/$pid" ] || return 1
  # A binary rebuilt while its process runs shows as "<path> (deleted)".
  exe=$(readlink "/proc/$pid/exe" 2>/dev/null) || return 1
  binary_of "$1" | grep -qxF -- "${exe% (deleted)}" || return 1
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
  # The shell writes its own pid and execs CMD, so the pidfile names CMD's process. Descriptor 9,
  # the run lock (common.sh), is closed, so a stack process never holds the lock.
  # shellcheck disable=SC2016
  setsid -f env -i "${spawn_env[@]}" /bin/sh -c 'echo "$$" >"$0.tmp" && mv -f -- "$0.tmp" "$0" && exec "$@"' \
    "$f" "$@" >"$log" 2>&1 </dev/null 9>&-
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
    die "port $p ($key, for $name) is already in use by a process that is not this stack's; free it or set ${PORT_VAR[$key]} and rerun stack.sh setup"
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

# gomaxprocs: LOADTEST_GOMAXPROCS (default: nproc), refused unless a positive integer.
gomaxprocs() {
  local n=${LOADTEST_GOMAXPROCS:-$(nproc)}
  [[ $n =~ ^[1-9][0-9]*$ ]] || die "LOADTEST_GOMAXPROCS='$n' is not a positive integer"
  printf '%s\n' "$n"
}

# start_dispatch LOG [BINARY [DB]]: BINARY (default $BIN/envoy-dispatch) on database DB (default
# dispatch), through the latency proxy unless LOADTEST_PG_DIRECT=1; waits up to 120 s for /healthz to
# report ok and NATS.
start_dispatch() {
  local log=$1 binary=${2:-$BIN/envoy-dispatch} db=${3:-dispatch}
  [ -x "$binary" ] || die "no Dispatch binary at $binary; run stack.sh setup first"
  local procs
  procs=$(gomaxprocs) || exit 1
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
  # A build with a pprof.go of its own reads DISPATCH_PPROF_PORT; one without it carries stack.sh's
  # overlay (build_dispatch_at), which reads LOADTEST_DISPATCH_PPROF_PORT.
  local pprof_var=DISPATCH_PPROF_PORT
  if [ -f "$binary.json" ] && [ "$(jq -r .dispatch_pprof_overlay "$binary.json")" = true ]; then
    pprof_var=LOADTEST_DISPATCH_PPROF_PORT
  fi
  local webdist=$ROOT/webdist
  mkdir -p -- "$webdist" "$STACK_HOME"
  # scripts/e2e/dispatch-session-settings.sh's Dispatch, on the stack's ports and tokens. No
  # DISPATCH_SIGNING_KEY: Dispatch keeps its key under the stack's HOME (XDG_DATA_HOME), so it is the
  # same across steps. No AWS_*: no Bedrock calls. Of the settings below, the only one 35efd326 (the
  # replay seed's build) lacks is DISPATCH_PPROF_PORT: it has no pprof.go, so the overlay's
  # LOADTEST_DISPATCH_PPROF_PORT stands in for it.
  # shellcheck disable=SC2034
  local dispatch_env=("${base_env[@]}"
    "DATABASE_URL=postgres://postgres@127.0.0.1:$pg_port/$db?sslmode=disable"
    "DISPATCH_AGENT_TOKEN=$(jq -r .dispatch_token "$STACK")"
    "DISPATCH_IDENTITY=header:X-Dispatch-User"
    "DISPATCH_IDENTITY_HEADER_TRUSTED=1"
    "DISPATCH_LISTEN_HOST=127.0.0.1"
    "DISPATCH_PORT=$(port dispatch)"
    "DISPATCH_SERVER_URL=http://127.0.0.1:$(port dispatch)"
    "DISPATCH_WEB_DIST=$webdist"
    "$pprof_var=$(port pprof)"
    "GOMAXPROCS=$procs"
    "NATS_URLS=nats://127.0.0.1:$(port nats)"
    "ENVOY_URL=http://127.0.0.1:$(port listener)"
    "ENVOY_TOKEN=$(jq -r .listener_token "$STACK")")
  spawn dispatch "$log" dispatch_env "$binary"
  set_pid dispatch "$(pid_of dispatch)"
  stack_set '.pg_direct = $d' --argjson d "$direct"
  wait_for "Dispatch /healthz to report ok and NATS" 120 dispatch "$log" dispatch_healthy
  note "dispatch pid $(pid_of dispatch) ($binary, database $db, GOMAXPROCS $procs) on 127.0.0.1:$(port dispatch), pprof $(port pprof), postgres via port $pg_port; log $log"
}

# ---- builds ------------------------------------------------------------------------------------------

sha256_of() { sha256sum -- "$1" | cut -d' ' -f1; }

# resolve_commit REV: REV's full commit id in the checkout; refused, naming REV, when it names none.
resolve_commit() {
  git -C "$SRC" rev-parse --verify --quiet "$1^{commit}" ||
    die "'$1' names no commit in $SRC"
}

# newest_migration ENVOY_DIR: the highest NNNN prefix of a migration file in that tree, an integer.
newest_migration() {
  local file
  file=$(cd -- "$1/internal/dispatch/store/migrations" && printf '%s\n' [0-9]*.sql | sort -n | tail -n1)
  printf '%s\n' "$((10#${file%%_*}))"
}

# write_build_json BINARY ENVOY_DIR COMMIT TREE DIRTY PATCH PATCH_SHA256 OVERLAY: BINARY.json, the
# build's record (an empty TREE, PATCH or PATCH_SHA256 is null).
write_build_json() {
  local binary=$1 dir=$2
  jq -n --arg commit "$3" --arg tree "$4" --argjson dirty "$5" --arg patch "$6" --arg patch_sha "$7" \
    --argjson overlay "$8" --arg binary_sha "$(sha256_of "$binary")" \
    --arg go "$(go version "$binary" | awk '{ print $NF }')" --argjson migration "$(newest_migration "$dir")" \
    'def orNull: if . == "" then null else . end;
     {dispatch_commit: $commit, dispatch_tree: ($tree | orNull), build_dirty: $dirty,
      patch: ($patch | orNull), patch_sha256: ($patch_sha | orNull), dispatch_pprof_overlay: $overlay,
      dispatch_binary_sha256: $binary_sha, go_version: $go, newest_migration: $migration}' >"$binary.json"
}

# build_dispatch_at REV OUT [PATCH]: build cmd/dispatch of commit REV (PATCH applied) into OUT, and
# write OUT.json. The tree is a `git archive` export of REV's packages/envoy under
# $ROOT/dispatch-src/<commit>[-<first 12 hex of PATCH's sha256>], reused once marked .complete; a
# tree without cmd/dispatch/pprof.go gets the pprof overlay. Sets BUILT_TREE to its packages/envoy.
build_dispatch_at() {
  local rev=$1 out=$2 patch=${3:-} sha
  sha=$(resolve_commit "$rev") || exit 1
  local key=$sha patch_sha="" patch_name=""
  if [ -n "$patch" ]; then
    [ -f "$patch" ] || die "no patch at $patch"
    patch_sha=$(sha256_of "$patch")
    patch_name=$(basename -- "$patch")
    key+=-${patch_sha:0:12}
  fi
  local srcroot=$ROOT/dispatch-src
  local tree=$srcroot/$key
  if [ ! -f "$tree/.complete" ]; then
    note "exporting packages/envoy at $sha${patch:+ with $patch_name} into $tree"
    rm -rf -- "$tree"
    mkdir -p -- "$tree"
    git -C "$SRC" archive "$sha" packages/envoy | tar -x -C "$tree"
    if [ -n "$patch" ]; then
      # The export is no repository: the ceiling keeps git apply from finding one above it.
      (cd -- "$tree" && GIT_CEILING_DIRECTORIES=$srcroot git apply -p1 "$patch") ||
        die "$patch does not apply to $sha"
    fi
    : >"$tree/.complete"
  fi
  BUILT_TREE=$tree/packages/envoy
  local overlay=false
  if [ ! -f "$BUILT_TREE/cmd/dispatch/pprof.go" ]; then
    cp -f -- "$PPROF_OVERLAY" "$BUILT_TREE/cmd/dispatch/zz_loadtest_pprof.go"
    overlay=true
  fi
  note "building $out from $sha${patch:+ + $patch_name} (pprof overlay: $overlay)"
  (cd -- "$BUILT_TREE" && GOWORK=off GOTOOLCHAIN=local go build -o "$out" ./cmd/dispatch)
  write_build_json "$out" "$BUILT_TREE" "$sha" "$(git -C "$SRC" rev-parse "$sha:packages/envoy")" false \
    "$patch_name" "$patch_sha" "$overlay"
}

# ---- commands ----------------------------------------------------------------------------------------

cmd_setup() {
  local commit=""
  if [ -n "${LOADTEST_DISPATCH_COMMIT:-}" ]; then
    commit=$(resolve_commit "$LOADTEST_DISPATCH_COMMIT") || exit 1
  fi
  command -v go >/dev/null || die "go is required"
  command -v uv >/dev/null || die "uv is required (Postgres comes from the pgserver wheel)"
  command -v jq >/dev/null || die "jq is required"
  [ -d "$ENVOY_SRC/cmd/dispatch" ] || die "LOADTEST_SRC=$SRC holds no packages/envoy/cmd/dispatch"
  mkdir -p -- "$BIN" "$RUN" "$LOGS" "$PGDIR/log" "$STACK_HOME" "$ROOT/tmp" "$ROOT/runs"

  if [ -n "$commit" ]; then
    build_dispatch_at "$commit" "$BIN/envoy-dispatch"
    note "building envoy-listener and loadgen from $ENVOY_SRC into $BIN"
    (cd -- "$ENVOY_SRC" &&
      go build -o "$BIN/envoy-listener" ./cmd/listener &&
      go build -o "$BIN/loadgen" ./loadtest)
  else
    note "building envoy-dispatch, envoy-listener and loadgen from $ENVOY_SRC into $BIN"
    (cd -- "$ENVOY_SRC" &&
      go build -o "$BIN/envoy-dispatch" ./cmd/dispatch &&
      go build -o "$BIN/envoy-listener" ./cmd/listener &&
      go build -o "$BIN/loadgen" ./loadtest)
    local dirty tree=""
    dirty=$(build_dirty)
    [ "$dirty" = true ] || tree=$(git -C "$SRC" rev-parse HEAD:packages/envoy 2>/dev/null || true)
    write_build_json "$BIN/envoy-dispatch" "$ENVOY_SRC" "$(build_commit)" "$tree" "$dirty" "" "" false
  fi

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
  for p in "${START_ORDER[@]}"; do
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

cmd_build_info() { printf '%s %s\n' "$(build_commit)" "$(build_dirty)"; }

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

  note "seeding through Dispatch (18 to 27 min on 2 vCPU); log $LOGS/seed.log"
  loadgen seed -out "$ROOT/seed.json" 2>&1 | tee "$LOGS/seed.log" >&2

  note "waiting for the outbox to publish every seed event"
  local deadline=$((SECONDS + 900)) pending
  while :; do
    pending=$(sql dispatch 'select count(*) from events where published_at is null')
    [ "$pending" = 0 ] && break
    ((SECONDS < deadline)) || die "the outbox still holds $pending unpublished events after 900 s; see $LOGS/dispatch-seed.log"
    sleep 2
  done
  # Dispatch's block-index backfill (internal/dispatch/docs/block_index.go, migrations 0121-0126)
  # indexes 20 owed documents a tick in the background. A seed that still owes it would hand every
  # step about 100 s of that backfill, inside its window; production in steady state owes none.
  # The tables do not exist in a build older than 0121, which owes nothing; Postgres resolves every
  # table a statement names before it runs, so the existence check is its own query.
  if [ "$(sql dispatch "select to_regclass('public.document_block_versions') is not null and to_regclass('public.block_grammar_owed') is not null")" = t ]; then
    note "waiting for Dispatch's block-index backfill to finish"
    deadline=$((SECONDS + 900))
    while :; do
      pending=$(sql dispatch 'select (select count(*) from document_block_versions where version is null) + (select count(*) from block_grammar_owed)')
      [ "$pending" = 0 ] && break
      ((SECONDS < deadline)) || die "Dispatch still owes $pending block-index entries after 900 s; see $LOGS/dispatch-seed.log"
      sleep 2
    done
  fi
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

# ---- the LINUX replay's seed -------------------------------------------------------------------------

cmd_replay_seed_patch_check() {
  [ $# -eq 1 ] || die "usage: stack.sh replay-seed-patch-check BUNDLE_DIR"
  [ -d "$1" ] || die "no bundle directory at $1"
  local bundle
  bundle=$(cd -- "$1" && pwd -P)
  command -v go >/dev/null || die "go is required"
  mkdir -p -- "$BIN" "$LOGS"
  build_dispatch_at "$REPLAY_COMMIT" "$BIN/dispatch-seed" "$REPLAY_PATCH"
  local log=$LOGS/replay-seed-patch-check.log status=0
  note "go test ./internal/dispatch/pmdoc -run TestLen16Seed in $BUILT_TREE, corpus $bundle; log $log"
  (cd -- "$BUILT_TREE" && GOWORK=off GOTOOLCHAIN=local LOADTEST_LEN16_CORPUS=$bundle \
    go test ./internal/dispatch/pmdoc -run 'TestLen16Seed' -count=1 -v) >"$log" 2>&1 || status=$?
  grep -E 'len16 corpus:|^(--- |ok |FAIL|PASS)' -- "$log" || true
  ((status == 0)) || die "the len16 patch's tests failed (exit $status); see $log"
  note "the len16 patch's tests pass"
}

# has_table DB TABLE: TABLE exists in DB. Postgres resolves every table a statement names before it
# runs, so the existence check is a query of its own.
has_table() { [ "$(sql "$1" "select to_regclass('public.$2') is not null")" = t ]; }

# owe WHAT COUNT: owed_work's tally.
owe() {
  OWED=$((OWED + $2))
  (($2 == 0)) || OWED_WHAT+="${OWED_WHAT:+, }$2 $1"
}

# owed_work DB: sets OWED to the number of rows of work Dispatch still owes in DB and OWED_WHAT to
# what they are, each table read only when it exists. At 0114 the settle timer is in memory, but every
# durable document update writes a doc_settlements_pending row and settlement deletes it.
owed_work() {
  local db=$1 n
  OWED=0
  OWED_WHAT=""
  if has_table "$db" events; then
    owe "unpublished events" "$(sql "$db" 'select count(*) from events where published_at is null')"
  fi
  if has_table "$db" comment_deliveries; then
    owe "pending comment deliveries" "$(sql "$db" "select count(*) from comment_deliveries where state = 'pending'")"
  fi
  if has_table "$db" message_deliveries; then
    owe "pending message deliveries" "$(sql "$db" "select count(*) from message_deliveries where state = 'pending'")"
  fi
  if has_table "$db" doc_settlements_pending; then
    owe "pending document settlements" "$(sql "$db" 'select count(*) from doc_settlements_pending')"
  fi
  if has_table "$db" document_block_versions; then
    n=$(sql "$db" 'select count(*) from document_block_versions where version is null')
    owe "unversioned document blocks" "$n"
  fi
  if has_table "$db" block_grammar_owed; then
    owe "block-grammar entries owed" "$(sql "$db" 'select count(*) from block_grammar_owed')"
  fi
}

# cpu_ticks PID: the process's utime + stime, in clock ticks.
cpu_ticks() {
  local stat fields
  stat=$(cat -- "/proc/$1/stat") || return 1
  read -ra fields <<<"${stat##*) }"
  # After "pid (comm) ", utime and stime are the 12th and 13th fields.
  printf '%s\n' "$((fields[11] + fields[12]))"
}

# wait_quiet DB LOG: wait until Dispatch owes no work in DB (within 1800 s), then until it uses less
# than 0.05 cores over 30 s (within 900 s), then read the owed work again. Sets QUIET_SECONDS.
wait_quiet() {
  local db=$1 log=$2 started=$SECONDS deadline
  note "waiting for Dispatch to owe no work in $db"
  deadline=$((SECONDS + 1800))
  while :; do
    owed_work "$db"
    ((OWED == 0)) && break
    running dispatch || die "Dispatch exited while it still owed $OWED_WHAT; see $log"
    ((SECONDS < deadline)) || die "Dispatch still owes $OWED_WHAT after 1800 s; see $log"
    sleep 2
  done
  note "Dispatch owes no work; waiting for it to idle below 0.05 cores over 30 s"
  local clk pid before after
  clk=$(getconf CLK_TCK)
  deadline=$((SECONDS + 900))
  while :; do
    pid=$(pid_of dispatch) || die "Dispatch exited before it went idle; see $log"
    before=$(cpu_ticks "$pid")
    sleep 30
    after=$(cpu_ticks "$pid") || die "Dispatch exited before it went idle; see $log"
    # Below 0.05 cores over 30 s: fewer than 1.5 s of CPU, 2 * ticks < 3 * CLK_TCK.
    if ((2 * (after - before) < 3 * clk)); then
      break
    fi
    ((SECONDS < deadline)) || die "Dispatch still uses $(((after - before) * 100 / clk / 30))% of a core after 900 s; see $log"
    note "Dispatch used $(((after - before) * 100 / clk / 30))% of a core over 30 s; waiting"
  done
  owed_work "$db"
  ((OWED == 0)) || die "Dispatch went idle yet owes $OWED_WHAT; see $log"
  QUIET_SECONDS=$((SECONDS - started))
  note "Dispatch is quiet after $QUIET_SECONDS s"
}

# pg_tool NAME: the Postgres client NAME of the stack's own Postgres (pgserver), else the system's.
pg_tool() {
  if [ -x "$PGBIN/$1" ]; then
    printf '%s\n' "$PGBIN/$1"
  elif [ -x "/usr/bin/$1" ]; then
    printf '%s\n' "/usr/bin/$1"
  else
    die "no $1: neither $PGBIN/$1 nor /usr/bin/$1 exists"
  fi
}

cmd_replay_seed() {
  local usage="usage: stack.sh replay-seed --bundle DIR [--keys-file FILE] [--stock] [--resume] [--out DIR] [-- LOADGEN_ARGS]"
  local bundle="" keys="" stock=false resume=false out=$ROOT/replay-seed extra=()
  while [ $# -gt 0 ]; do
    case $1 in
      --bundle | --keys-file | --out)
        if [ $# -lt 2 ] || [ -z "$2" ]; then die "$1 needs a value; $usage"; fi
        case $1 in
          --bundle) bundle=$2 ;;
          --keys-file) keys=$2 ;;
          --out) out=$2 ;;
        esac
        shift 2
        ;;
      --stock) stock=true && shift ;;
      --resume) resume=true && shift ;;
      --)
        extra=("${@:2}")
        break
        ;;
      *) die "unknown argument '$1'; $usage" ;;
    esac
  done
  [ -n "$bundle" ] || die "--bundle is required; $usage"
  [ -d "$bundle" ] || die "no bundle directory at $bundle"
  bundle=$(cd -- "$bundle" && pwd -P)
  if [ -n "$keys" ]; then
    [ -f "$keys" ] || die "no keys file at $keys"
    keys=$(cd -- "$(dirname -- "$keys")" && pwd -P)/$(basename -- "$keys")
  fi
  mkdir -p -- "$out"
  out=$(cd -- "$out" && pwd -P)
  need_stack
  local p
  for p in postgres nats listener; do
    running "$p" || die "$p is not running; stack.sh start first"
  done
  running dispatch && die "Dispatch is already running (pid $(pid_of dispatch)); stack.sh stop dispatch first"
  [ -x "$BIN/loadgen" ] || die "no $BIN/loadgen; run stack.sh setup first"
  gomaxprocs >/dev/null
  if [ "$resume" = true ]; then
    db_exists "$REPLAY_DB" || die "--resume: database $REPLAY_DB does not exist; run without --resume"
  fi

  local binary=$BIN/dispatch-seed patch=$REPLAY_PATCH
  if [ "$stock" = true ]; then
    binary=$BIN/dispatch-seed-stock
    patch=""
  fi
  build_dispatch_at "$REPLAY_COMMIT" "$binary" "$patch"

  if [ "$resume" = true ]; then
    note "resuming on database $REPLAY_DB"
  else
    sql postgres "drop database if exists $REPLAY_DB" >/dev/null
    sql postgres "create database $REPLAY_DB" >/dev/null
  fi
  local dispatch_log=$LOGS/dispatch-replay-seed.log started=$SECONDS
  trap 'stop_one dispatch' EXIT
  start_dispatch "$dispatch_log" "$binary" "$REPLAY_DB"

  note "replaying the bundle's state into $REPLAY_DB; log $LOGS/replay-seed.log"
  local args=(replay-seed -bundle "$bundle" -db "$REPLAY_DB")
  [ -z "$keys" ] || args+=(-keys-file "$keys")
  loadgen "${args[@]}" "${extra[@]}" 2>&1 | tee "$LOGS/replay-seed.log" >&2
  local seed_seconds=$((SECONDS - started))

  wait_quiet "$REPLAY_DB" "$dispatch_log"
  stop_one dispatch
  trap - EXIT

  note "vacuum analyze"
  sql "$REPLAY_DB" 'vacuum analyze' >/dev/null
  local migration
  migration=$(sql "$REPLAY_DB" 'select max(version) from schema_migrations')
  [ "$migration" = "$REPLAY_MIGRATION" ] ||
    die "database $REPLAY_DB is at migration '$migration', not $REPLAY_MIGRATION; refusing to dump it"

  # The seed's check (loadgen replay-check-seed) with the bundle and keys file it seeded: no dump
  # unless it passed. Its file goes beside the dump as seed-check.json and into seed.meta.json.
  local check_file=$ROOT/tmp/seed-check.json check_args=(replay-check-seed -bundle "$bundle" -db "$REPLAY_DB")
  [ -z "$keys" ] || check_args+=(-keys-file "$keys")
  rm -f -- "$check_file"
  note "replay-check-seed; log $LOGS/replay-check-seed.log"
  loadgen "${check_args[@]}" -out "$check_file" >"$LOGS/replay-check-seed.log" 2>&1 || true
  [ -f "$check_file" ] || die "replay-check-seed wrote no result; see $LOGS/replay-check-seed.log"
  [ "$(jq -r .passed "$check_file")" = true ] ||
    die "replay-check-seed did not pass ($(jq -c '{differences, version_size_mismatches, anchors_outside_logged_block}' "$check_file")); refusing to dump; see $LOGS/replay-check-seed.log"
  cp -- "$check_file" "$out/seed-check.json"
  note "replay-check-seed passed: $(jq -c '{documents, anchor_rules, sign_flipped}' "$check_file")"

  # pg_dump's custom format under each compression; pgserver's 16.2 is built without zstd and
  # refuses those, which is logged and skipped. The smallest is kept.
  local name dump_log=$LOGS/replay-seed-dump.log candidates=() spec file best="" best_bytes=0 best_spec="" i=0
  name=linux-replay-seed-$(printf '%04d' "$REPLAY_MIGRATION")
  local pg_dump compression='[]'
  pg_dump=$(pg_tool pg_dump)
  : >"$dump_log"
  for spec in gzip:9 zstd:19 zstd:level=19,long; do
    file=$ROOT/tmp/$name.dump.$i
    i=$((i + 1))
    note "pg_dump -Fc -Z $spec"
    if env -i "${base_env[@]}" "$pg_dump" -h 127.0.0.1 -p "$(port postgres)" -U postgres -d "$REPLAY_DB" \
      -Fc -Z "$spec" -f "$file" >>"$dump_log" 2>&1; then
      local bytes
      bytes=$(stat -c %s -- "$file")
      compression=$(jq -c --arg s "$spec" --argjson b "$bytes" '. + [{compression: $s, bytes: $b}]' <<<"$compression")
      candidates+=("$file")
      if [ -z "$best" ] || ((bytes < best_bytes)); then
        best=$file best_bytes=$bytes best_spec=$spec
      fi
    else
      local why
      why=$(tail -n 1 -- "$dump_log")
      note "pg_dump -Z $spec refused, skipped: $why"
      compression=$(jq -c --arg s "$spec" --arg r "$why" '. + [{compression: $s, refused: $r}]' <<<"$compression")
      rm -f -- "$file"
    fi
  done
  [ -n "$best" ] || die "pg_dump refused every compression; see $dump_log"

  local dump_sha parts='[]' part
  dump_sha=$(sha256_of "$best")
  rm -f -- "$out/$name.dump.part"*
  split -b 24M -d -a 2 -- "$best" "$out/$name.dump.part"
  for part in "$out/$name.dump.part"*; do
    parts=$(jq -c --arg f "$(basename -- "$part")" --argjson b "$(stat -c %s -- "$part")" \
      --arg s "$(sha256_of "$part")" '. + [{file: $f, bytes: $b, sha256: $s}]' <<<"$parts")
  done
  rm -f -- "${candidates[@]}"

  local counts
  counts=$(sql "$REPLAY_DB" "select json_build_object(
      'projects', (select count(*) from projects),
      'issues', (select count(*) from issues),
      'artifacts', (select count(*) from artifacts),
      'artifact_versions', (select count(*) from artifact_versions),
      'comments', (select count(*) from comments),
      'asks', (select count(*) from asks),
      'events', (select count(*) from events))")
  jq -n --argjson migration "$migration" --slurpfile build "$binary.json" \
    --arg pg "$("$PGBIN/postgres" --version)" --arg at "$(date -u +%FT%TZ)" \
    --argjson seed_seconds "$seed_seconds" --argjson quiet "$QUIET_SECONDS" \
    --arg chosen "$best_spec" --argjson candidates "$compression" \
    --argjson bytes "$best_bytes" --arg sha "$dump_sha" --argjson parts "$parts" --argjson counts "$counts" \
    --arg keys "$([ -z "$keys" ] || basename -- "$keys")" --slurpfile check "$check_file" \
    '$build[0] as $b |
     {migration: $migration, dispatch_commit: $b.dispatch_commit, dispatch_tree: $b.dispatch_tree,
      dispatch_binary_sha256: $b.dispatch_binary_sha256, patch: $b.patch, patch_sha256: $b.patch_sha256,
      go_version: $b.go_version, postgres_version: $pg, created_at: $at, seed_seconds: $seed_seconds,
      quiet_wait_seconds: $quiet, compression: {chosen: $chosen, candidates: $candidates},
      dump: {bytes: $bytes, sha256: $sha}, parts: $parts, counts: $counts,
      keys_file: (if $keys == "" then null else $keys end), check: $check[0]}' >"$out/seed.meta.json"
  note "replay seed done: $(jq '.parts | length' "$out/seed.meta.json") parts of $best_bytes bytes ($best_spec) in $out; $(jq -c .counts "$out/seed.meta.json")"
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
  for p in "${START_ORDER[@]}"; do
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
  setup | start | seed | reset | stop | replay-seed-patch-check | replay-seed) lock_root "$ROOT" stack.sh ;;
esac
case ${1:-} in
  setup) cmd_setup ;;
  start) cmd_start ;;
  seed) cmd_seed "${@:2}" ;;
  reset) cmd_reset "${@:2}" ;;
  stop) cmd_stop "${@:2}" ;;
  replay-seed-patch-check) cmd_replay_seed_patch_check "${@:2}" ;;
  replay-seed) cmd_replay_seed "${@:2}" ;;
  status) cmd_status ;;
  build-info) cmd_build_info ;;
  loadgen) loadgen "${@:2}" ;;
  *) usage ;;
esac
