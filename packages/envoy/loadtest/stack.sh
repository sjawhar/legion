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
#                           kept); refuses a port another process holds, naming it when this PID
#                           namespace can see it.
#   stack.sh seed [--force] seed through Dispatch, then its production-shaped references (37 to 50 min
#                           on 2 vCPU in all), drain the outbox, vacuum, install the commit-time trigger,
#                           write seed.meta.json and keep the result as the template database
#                           dispatch_seed. --force drops an existing seed first.
#   stack.sh reset [LOG]    what run-step.sh does before each step: stop Dispatch and the listener,
#                           recreate database dispatch from dispatch_seed, restart NATS on a fresh store
#                           and the listener, start Dispatch (its log to LOG) and wait for /healthz.
#   stack.sh reset --replay [LOG]
#                           the same from this build's prepared replay template (replay-prepare;
#                           refused without one), and before Dispatch starts, `loadgen replay-checksum
#                           -db dispatch` into $LOADTEST_ROOT/replay/last-reset.json.
#   stack.sh stop [NAME...] stop dispatch, listener, nats, proxy and postgres (or the named ones).
#   stack.sh status         the stack's ports and the stack.json that records them; each process:
#                           pid, alive, port.
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
#   stack.sh replay-manifest --bundle-pack FILE --bundle DIR [--seed-dir DIR] [--part-version N] > MANIFEST
#                           a replay manifest (schema 1, replay-2026-10-26.json) from replay-pack's
#                           JSON, the bundle's bundle.meta.json and replay-seed's seed.meta.json
#                           (without --seed-dir, "seed": null); every part's version is N (default
#                           1). No lock, no stack.
#   stack.sh replay-get [--manifest FILE] [--from-dir DIR]
#                           the manifest's parts (default replay-2026-10-26.json) under
#                           $LOADTEST_ROOT/replay/parts, each kept when its sha256 matches, else
#                           copied from DIR or downloaded from DISPATCH_URL with DISPATCH_TOKEN_FILE's
#                           token, and refused on a sha256 mismatch; the bundle unpacked into
#                           replay/bundle and verified file by file; the seed dump restored once into
#                           the template dispatch_replay_seed (replay/seed-restored.json). Needs
#                           Postgres running when the manifest names a seed.
#   stack.sh replay-prepare this build's replay template dispatch_replay_<key> (key: the first 16 hex
#                           of bin/envoy-dispatch.json's dispatch_binary_sha256): from
#                           dispatch_replay_seed, migrated by this build's Dispatch, its block-index
#                           backfill done, quiet, vacuumed (replay/prepared/<key>.json). Reused once
#                           made; the 3 most recently used are kept.
#
# A Dispatch built from another commit (build_dispatch_at) is built in a `git archive` export of that
# commit's packages/envoy under $LOADTEST_ROOT/dispatch-src/<commit>[-<patch sha256, 12 hex>], with
# GOWORK=off GOTOOLCHAIN=local; a tree without cmd/dispatch/pprof.go gets zz_loadtest_pprof.go.txt as
# cmd/dispatch/zz_loadtest_pprof.go, which serves pprof on LOADTEST_DISPATCH_PPROF_PORT. Every
# Dispatch binary has a <binary>.json beside it: dispatch_commit, dispatch_tree, build_dirty, patch,
# patch_sha256, dispatch_pprof_overlay, dispatch_binary_sha256, go_version, newest_migration.
#
# Environment: LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest); LOADTEST_SRC (default: the
# checkout holding this script); the ports, which follow LOADTEST_ROOT (common.sh: the root's block
# k of the path's sha256 modulo 200 gives postgres 15000+10k, proxy 15001+10k, nats 15002+10k,
# nats_monitor 15003+10k, listener 15004+10k, dispatch 15005+10k, pprof 15006+10k), so two roots on
# one machine hold the same 7 ports about 1 pair in 200 and otherwise none, and a collision is
# refused by start, not shared; LOADTEST_PG_PORT, LOADTEST_PROXY_PORT, LOADTEST_NATS_PORT, LOADTEST_NATS_MONITOR_PORT,
# LOADTEST_LISTENER_PORT, LOADTEST_DISPATCH_PORT and LOADTEST_PPROF_PORT each override their own
# port, read by setup into stack.json (`stack.sh status` prints them); LOADTEST_PROXY_RTT (1ms);
# LOADTEST_PG_DIRECT=1 starts Dispatch on Postgres without the proxy;
# LOADTEST_DISPATCH_COMMIT (setup, above); LOADTEST_GOMAXPROCS, Dispatch's GOMAXPROCS (default: nproc).
#
# Every stack process starts under `env -i` with an explicit environment, so this shell's
# DISPATCH_URL, ENVOY_URL, ENVOY_NATS_URL, AWS_*, HOME and XDG_* never reach it, and every process is
# stopped by the pid in its pidfile ($LOADTEST_ROOT/run/<name>.pid), never by name. replay-get alone
# reads DISPATCH_URL and DISPATCH_TOKEN_FILE, in this shell, for its downloads.
#
# setup, start, seed, reset, stop, replay-seed-patch-check, replay-seed, replay-get and
# replay-prepare take the root's run lock (common.sh) for their run, and are refused while another
# run holds it; run-step.sh's and run-series.sh's own calls pass. status, build-info, loadgen and
# replay-manifest take no lock.
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
PPROF_OVERLAY=$here/zz_loadtest_pprof.go.txt
# The binaries a dispatch process may run: the stack's own and the replay seed's two builds.
DISPATCH_BINARIES=("$BIN/envoy-dispatch" "$BIN/dispatch-seed" "$BIN/dispatch-seed-stock")
# Processes in stop order, and in start order (dispatch is started by seed and reset).
PROCESSES=(dispatch listener nats proxy postgres)
START_ORDER=(postgres proxy nats listener dispatch)
# The stack.json port keys (PORT_KEYS), each one's environment variable (PORT_VAR) and the default
# ports of this root, which follow LOADTEST_ROOT, are common.sh's (stack_ports).

# The LINUX replay's commands (replay-seed-patch-check, replay-seed, replay-manifest,
# replay-get, replay-prepare and `reset --replay`), their constants and their helpers.
# shellcheck source=packages/envoy/loadtest/replay.sh
source "$here/replay.sh"

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

# psql_db DB ARGS...: a psql client on the stack's Postgres, no ~/.psqlrc, no PG* from this shell.
psql_db() {
  local db=$1
  shift
  env -i "${base_env[@]}" PGOPTIONS=--client-min-messages=warning "$(pg_tool psql)" -X -q -v ON_ERROR_STOP=1 \
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

# json_set FILE JQ_FILTER [jq args...]: rewrite a JSON file through a filter.
json_set() {
  local file=$1 filter=$2
  shift 2
  local tmp
  tmp=$(mktemp "$file.XXXXXX")
  jq "$@" "$filter" "$file" >"$tmp"
  mv -f -- "$tmp" "$file"
}

# stack_set JQ_FILTER [jq args...]: rewrite stack.json through a filter, owner-only.
stack_set() {
  json_set "$STACK" "$@"
  chmod 600 "$STACK"
}

set_pid() { stack_set '.pids[$n] = $p' --arg n "$1" --argjson p "$2"; }

random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# write_stack_json writes stack.json on the first setup, with this root's ports (stack_ports: the
# defaults that follow LOADTEST_ROOT, each one overridden by its LOADTEST_*_PORT). A later setup
# keeps the ports the file records and moves only those whose variable this environment sets, so a
# rerun with no port variable set keeps the stack's ports; it refuses to move a port while the
# stack runs. The name, tokens and pids are kept.
write_stack_json() {
  local ports k p var args=()
  if [ ! -f "$STACK" ]; then
    while read -r k p; do args+=(--argjson "$k" "$p"); done < <(stack_ports "$ROOT")
    ports=$(jq -n "${args[@]}" '$ARGS.named')
    (
      umask 077
      jq -n --arg name "loadtest-$(random_hex 4)" --arg root "$ROOT" \
        --arg dt "$(random_hex 24)" --arg lt "$(random_hex 24)" --argjson ports "$ports" \
        '{name: $name, root: $root, dispatch_token: $dt, listener_token: $lt, ports: $ports,
          pids: {postgres: 0, proxy: 0, nats: 0, listener: 0, dispatch: 0}, pg_direct: false}' >"$STACK"
    )
    note "wrote $STACK (stack $(stack_name), ports $(jq -c .ports "$STACK"))"
    return
  fi
  local -A recorded=()
  while read -r k p; do recorded[$k]=$p; done < <(jq -r '(.ports // {}) | to_entries[] | "\(.key) \(.value)"' "$STACK")
  while read -r k p; do
    var=${PORT_VAR[$k]}
    args+=(--argjson "$k" "${!var:-${recorded[$k]:-$p}}")
  done < <(stack_ports "$ROOT")
  ports=$(jq -n "${args[@]}" '$ARGS.named')
  if [ "$(jq -c .ports "$STACK")" != "$(jq -c . <<<"$ports")" ]; then
    for k in "${PROCESSES[@]}"; do
      running "$k" && die "the port variables in the environment move ports of $STACK while $k is running; stack.sh stop first"
    done
    stack_set '.ports = $ports' --argjson ports "$ports"
    note "ports in $STACK updated from the environment: $(jq -c . <<<"$ports")"
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

# port_holder PORT: "pid N (command)" of each process of this PID namespace that holds a socket
# listening on PORT, joined with ", ". It reads the listening sockets' inodes from /proc/net/tcp
# and /proc/net/tcp6 and looks for them among the descriptors in /proc/<pid>/fd. A listener of
# another PID namespace (another Legion role of this pod, which shares the network namespace) has
# its socket in /proc/net/tcp and no process here, so the result is empty.
port_holder() {
  local hex want=" " file inode dir fd target cmd out=""
  printf -v hex '%04X' "$1"
  for file in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$file" ] || continue
    while read -r _ local_address _ state _ _ _ _ _ inode _; do
      [ "$state" = 0A ] && [ "${local_address##*:}" = "$hex" ] && want+="$inode "
    done <"$file"
  done
  [ "$want" = " " ] && return 0
  for dir in /proc/[0-9]*; do
    for fd in "$dir"/fd/*; do
      target=$(readlink -- "$fd" 2>/dev/null) || continue
      [[ $target == socket:\[*\] ]] || continue
      inode=${target#socket:\[}
      [[ $want == *" ${inode%\]} "* ]] || continue
      cmd=$(tr '\0' ' ' <"$dir/cmdline" 2>/dev/null) || continue
      out+="${out:+, }pid ${dir#/proc/} (${cmd% })"
      break
    done
  done
  [ -z "$out" ] || printf '%s\n' "$out"
}

# check_port NAME PORT_KEY: refuse a port another process holds, naming it when this PID namespace
# can see it, and saying that it cannot when it cannot. A port this namespace cannot see is most
# often another Legion role of the pod whose LOADTEST_ROOT hashed into this root's block of ports
# (common.sh: a block out of 200, so about 1 pair of roots in 200 collides), and then all 7 ports
# are the same, so the refusal names the block and both remedies. It says that only of a port this
# root's block still sets: a port stack.json recorded under another block, or one a LOADTEST_*_PORT
# overrides, is held by whoever holds it, which this root's block says nothing about.
check_port() {
  local name=$1 key=$2 p holders block
  p=$(port "$key")
  port_open "$p" || return 0
  holders=$(port_holder "$p")
  [ -n "$holders" ] &&
    die "port $p ($key, for $name) is already in use by $holders, which is not this stack's; stop it and rerun stack.sh start, or set ${PORT_VAR[$key]} to a port no one holds and rerun stack.sh setup with it set, then stack.sh start (start reads the ports stack.json records, so a port variable takes effect only through setup)"
  block=$(port_block "$LOADTEST_ROOT")
  [ "$p" = "$(default_port "$key" "$block")" ] ||
    die "port $p ($key, for $name) is already in use, and no process of this PID namespace holds it: another Legion role of this pod, or another PID namespace, may hold it, in the network namespace they share. This port is not the default of this root's port block $block (stack.json records it, or ${PORT_VAR[$key]} sets it), so no other root's block of ports explains it. Stop whatever holds it and rerun stack.sh start, or set ${PORT_VAR[$key]} to a port no one holds and rerun stack.sh setup with it set, then stack.sh start (start reads the ports stack.json records, so a port variable takes effect only through setup)"
  die "port $p ($key, for $name) is already in use, and no process of this PID namespace holds it: another Legion role of this pod, or another PID namespace, may hold it, in the network namespace they share. This root is in port block $block of $PORT_BLOCKS (the block is the hash of $LOADTEST_ROOT modulo $PORT_BLOCKS), and a second root in block $block holds these same 7 ports. Stop that stack from the role that started it and rerun stack.sh start; or set the LOADTEST_*_PORT variables (${PORT_VAR[$key]} for this one) to ports no one holds and rerun stack.sh setup with them set, then stack.sh start (start reads the ports stack.json records, so a port variable takes effect only through setup); or move this stack to another LOADTEST_ROOT, which picks another block with probability $((PORT_BLOCKS - 1))/$PORT_BLOCKS, and run stack.sh setup and stack.sh start there"
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

# start_dispatch LOG [BINARY [DB [SECONDS]]]: BINARY (default $BIN/envoy-dispatch) on database DB
# (default dispatch), through the latency proxy unless LOADTEST_PG_DIRECT=1; waits up to SECONDS
# (default 120) for /healthz to report ok and NATS.
start_dispatch() {
  local log=$1 binary=${2:-$BIN/envoy-dispatch} db=${3:-dispatch} healthz_seconds=${4:-120}
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
  wait_for "Dispatch /healthz to report ok and NATS" "$healthz_seconds" dispatch "$log" dispatch_healthy
  note "dispatch pid $(pid_of dispatch) ($binary, database $db, GOMAXPROCS $procs) on 127.0.0.1:$(port dispatch), pprof $(port pprof), postgres via port $pg_port; log $log"
}

# ---- builds ------------------------------------------------------------------------------------------

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
  else
    note "building envoy-dispatch from $ENVOY_SRC into $BIN"
    (cd -- "$ENVOY_SRC" && go build -o "$BIN/envoy-dispatch" ./cmd/dispatch)
  fi
  note "building envoy-listener and loadgen from $ENVOY_SRC into $BIN"
  (cd -- "$ENVOY_SRC" &&
    go build -o "$BIN/envoy-listener" ./cmd/listener &&
    go build -o "$BIN/loadgen" ./loadtest)
  if [ -z "$commit" ]; then
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
    # installed on its own, as scripts/e2e/dispatch-session-settings.sh does: with the toolchain's
    # own go, under its own GOROOT. mise's go shim replaces a caller's GOBIN when go.set_gobin is
    # true, and, on mise before 2026.9.0, in any shell `mise activate` set up. Separately, an
    # activated shell exports mise's Go's GOROOT, whose compiler a newer toolchain's go refuses.
    local goroot
    goroot=$(go env GOROOT)
    GOBIN=$BIN GOROOT=$goroot "$goroot/bin/go" install "github.com/nats-io/nats-server/v2@$nats_version"
  fi
  [ -x "$BIN/nats-server" ] || die "no executable nats-server in $BIN after go install"
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
  # This drops the template, seed.meta.json and seed.json below, and reaches seed-refs only after
  # the 14 to 27 minutes of loadgen seed. A loadgen built from a checkout older than the references
  # (README.md, "Before and after across commits") knows no seed-refs and would exit 2 there, with
  # the old seed already gone, so it is asked first.
  loadgen seed-refs -h >/dev/null 2>&1 ||
    die "the loadgen at $BIN was built from a checkout that has no seed-refs; seed with a loadgen that has it, then run the older build on that seed"
  local started=$SECONDS
  stop_one dispatch
  sql postgres 'drop database if exists dispatch' >/dev/null
  sql postgres 'drop database if exists dispatch_seed' >/dev/null
  rm -f -- "$SEED_META" "$ROOT/seed.json"
  sql postgres 'create database dispatch' >/dev/null
  trap 'stop_one dispatch' EXIT
  start_dispatch "$LOGS/dispatch-seed.log"

  note "seeding through Dispatch (14 to 27 min on 2 vCPU); log $LOGS/seed.log"
  loadgen seed -out "$ROOT/seed.json" 2>&1 | tee "$LOGS/seed.log" >&2

  note "writing production-shaped references (about 23 min on 2 vCPU); log $LOGS/seed-refs.log"
  loadgen seed-refs 2>&1 | tee "$LOGS/seed-refs.log" >&2

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

  # seed-refs writes under idempotency keys, so Dispatch keeps one request_keys row per write. They
  # protect retries during the seed only. Left in the template, Dispatch's sweep of keys older than
  # a day (24 h) would delete them inside the window of every step on a seed older than that, and
  # not on a younger one. A build older than migration 0137 has no such table.
  if [ "$(sql dispatch "select to_regclass('public.request_keys') is not null")" = t ]; then
    note "deleting the seed's idempotency keys (request_keys)"
    sql dispatch 'delete from request_keys' >/dev/null
    [ "$(sql dispatch 'select count(*) from request_keys')" = 0 ] ||
      die "request_keys still holds rows after the delete; the template would carry them"
  fi
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
      'ref_targets', (select count(*) from artifacts where name like 'ref-target-%'),
      'ref_target_versions', (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.name like 'ref-target-%'),
      'ref_sources', (select count(*) from artifacts where name = 'references.md'),
      'artifact_refs', (select count(*) from refs where to_kind = 'artifact'),
      'events', (select count(*) from events))")
  # The seed is made by the Dispatch in $BIN, which is the checkout's build only when setup ran
  # without LOADTEST_DISPATCH_COMMIT, so the record names both: build_commit is the checkout's,
  # dispatch_commit and dispatch_build the binary's.
  [ -f "$BIN/envoy-dispatch.json" ] || die "no Dispatch build record at $BIN/envoy-dispatch.json; run stack.sh setup"
  jq -n --argjson migration "$migration" --arg commit "$(build_commit)" --argjson dirty "$(build_dirty)" \
    --argjson counts "$counts" --arg at "$(date -u +%FT%TZ)" --arg pg "$("$PGBIN/postgres" --version)" \
    --argjson seconds "$((SECONDS - started))" --slurpfile build "$BIN/envoy-dispatch.json" \
    '$build[0] as $b |
     {migration: $migration, build_commit: $commit, build_dirty: $dirty,
      dispatch_commit: $b.dispatch_commit, dispatch_binary_sha256: $b.dispatch_binary_sha256,
      dispatch_build: $b, created_at: $at,
      seed_seconds: $seconds, postgres_version: $pg, counts: $counts}' >"$SEED_META"
  sql postgres 'alter database dispatch rename to dispatch_seed' >/dev/null
  note "seed done in $((SECONDS - started)) s: template database dispatch_seed; $(jq -c 'del(.dispatch_build)' "$SEED_META")"
}

cmd_reset() {
  if [ "${1:-}" = --replay ]; then
    cmd_reset_replay "${@:2}"
    return
  fi
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

# cmd_status prints the stack's ports, which follow LOADTEST_ROOT, and the file that records them,
# so two roots' stacks are told apart by their ports.
cmd_status() {
  need_stack
  printf 'stack %s, root %s\nports     %s (%s)\n' "$(stack_name)" "$ROOT" "$(jq -c .ports "$STACK")" "$STACK"
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

case ${1:-} in
  setup | start | seed | reset | stop | replay-seed-patch-check | replay-seed | replay-get | replay-prepare) lock_root "$ROOT" ;;
esac
case ${1:-} in
  setup) cmd_setup ;;
  start) cmd_start ;;
  seed) cmd_seed "${@:2}" ;;
  reset) cmd_reset "${@:2}" ;;
  stop) cmd_stop "${@:2}" ;;
  replay-seed-patch-check) cmd_replay_seed_patch_check "${@:2}" ;;
  replay-seed) cmd_replay_seed "${@:2}" ;;
  replay-manifest) cmd_replay_manifest "${@:2}" ;;
  replay-get) cmd_replay_get "${@:2}" ;;
  replay-prepare) cmd_replay_prepare "${@:2}" ;;
  status) cmd_status ;;
  build-info) cmd_build_info ;;
  loadgen) loadgen "${@:2}" ;;
  *) usage ;;
esac
