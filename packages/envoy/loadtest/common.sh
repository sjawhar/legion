# shellcheck shell=bash
# common.sh: what run-step.sh, run-series.sh and stack.sh share (README.md). They source it. Not
# executable.
#
# die MESSAGE prints MESSAGE, prefixed with the script's name, and exits 1. note MESSAGE prints
# MESSAGE with the same prefix and goes on. usage prints the script's header comment, from its
# second line to its `set -euo` line, and exits 2. abs_file FLAG FILE prints FILE's absolute path
# and dies, naming FLAG, when FILE is not a file. sha256_of FILE prints FILE's sha256, and
# joined_sha256 FILE... the sha256 of the files' concatenation (stack.sh's builds, replay.sh's
# parts).
#
# resolve_paths sets SRC to LOADTEST_SRC (default: the checkout holding this file) and ROOT to
# LOADTEST_ROOT (default ${TMPDIR:-/tmp}/dispatch-loadtest, created), both as physical paths, and
# exports them as LOADTEST_SRC and LOADTEST_ROOT. It sets DISPATCH_SRC, the checkout envoy-dispatch
# is built from, to LOADTEST_DISPATCH_SRC (default SRC).
#
# stack_ports ROOT: the stack's ports for ROOT, "KEY PORT" a line. The defaults follow the root's
# physical path (port_block, default_port): a root always gets the same block of 8 ports, and two
# roots get two blocks out of 200, so they share every port about 1 pair of roots in 200 (the block
# is a hash modulo 200, not a per-root reservation). Each LOADTEST_*_PORT (PORT_VAR) overrides its
# own port. So two Legion roles of one pod, each with its own LOADTEST_ROOT, almost always run a
# stack each with no port variable set; when their blocks do collide, check_port refuses the second
# stack and the roles set the LOADTEST_*_PORT variables (or pick another root). stack.sh setup
# records the ports in ROOT/stack.json, and `stack.sh status` prints them.
#
# lock_root ROOT: one run at a time per root (README.md, "One run at a time"). It takes an
# exclusive, non-blocking flock on ROOT/.run.lock and keeps it on file descriptor 9. The lock lasts
# until every process that holds that descriptor has exited: the holder and the children that
# inherit it. Once locked, the holder empties the lock file, then writes its pid, its PID namespace,
# command, start time and a random token into it with one printf, and exports the token as
# LOADTEST_LOCK_TOKEN.
#
# A holder's own child (run-series.sh's run-step.sh, a run's stack.sh) goes on without locking when
# its LOADTEST_LOCK_TOKEN equals the file's token and its descriptor 9 is the inherited locked one:
# it keeps that descriptor and sets LOCK_INHERITED=true. So a series step, and the loadgen run that
# it starts, keep the root when run-series.sh is killed. Any other caller that sees a live holder is
# refused at once; while it sees none, it tries the lock for up to 3 s, and is admitted when the
# lock is freed in that time. It tries on descriptor 8 and moves the lock to 9 only once it has it,
# so a waiting caller never holds descriptor 9 on the file. A refused caller exits 1 before it
# touches the stack, naming the root and the holder: the record's pid, once the file holds a
# complete record whose pid runs in this PID namespace; or else the processes here that still hold
# descriptor 9 on the file (the children of a holder that has exited); or else, when this PID
# namespace shows neither (a holder in another PID namespace, such as another Legion role in this
# pod on a shared root), the pid, start time and command that the lock file records for that run,
# and that the lock file must stay. The record's pid is trusted only when the record's PID
# namespace is this one. stack.sh closes descriptor 9 in every stack process that it starts, so no
# stack process keeps the lock.
#
# prepare_stack runs stack.sh setup (an incremental build of SRC, and of DISPATCH_SRC's
# envoy-dispatch), start and seed, each of which does nothing when its work is already done, and the
# migration check: a Dispatch build that lacks a migration the seed holds is refused with exit 1,
# and one that holds migrations the seed lacks is noted. Then it sets BUILD to `stack.sh build-info`
# ("<commit> <dirty>"). run-step.sh calls it before a step that is not part of a series;
# run-series.sh calls it once, before its first step.

die() {
  printf '%s: %s\n' "${0##*/}" "$*" >&2
  exit 1
}

note() { printf '%s: %s\n' "${0##*/}" "$*" >&2; }

# abs_file FLAG FILE: FILE's absolute path, refused when it is not a file.
abs_file() {
  [ -f "$2" ] || die "$1 names $2, which is not a file"
  printf '%s/%s\n' "$(cd -- "$(dirname -- "$2")" && pwd -P)" "$(basename -- "$2")"
}

# sha256_of FILE: FILE's sha256, the hex alone.
sha256_of() { sha256sum -- "$1" | cut -d' ' -f1; }

# joined_sha256 FILE...: the sha256 of the files' concatenation.
joined_sha256() { cat -- "$@" | sha256sum | cut -d' ' -f1; }

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

resolve_paths() {
  SRC=${LOADTEST_SRC:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd -P)}
  ROOT=${LOADTEST_ROOT:-${TMPDIR:-/tmp}/dispatch-loadtest}
  mkdir -p -- "$ROOT"
  ROOT=$(cd -- "$ROOT" && pwd -P)
  export LOADTEST_SRC=$SRC LOADTEST_ROOT=$ROOT
  DISPATCH_SRC=${LOADTEST_DISPATCH_SRC:-$SRC}
}

# ---- the stack's ports ---------------------------------------------------------------------------

# The stack's default ports follow LOADTEST_ROOT, so two roots on one machine (two Legion roles of
# one pod, each with its own root and its own PID namespace) almost always run side by side with no
# port variable set: each root hashes to one of PORT_BLOCKS blocks, so two roots land in one block
# about 1 pair in 200, and then every port of the two stacks is the same one (check_port refuses
# the second stack; the remedy is the LOADTEST_*_PORT variables). PORT_KEYS names stack.json's
# ports, PORT_VAR each one's environment variable and PORT_OFFSET its place in a root's block of
# ports.
PORT_KEYS=(postgres proxy nats nats_monitor listener dispatch pprof envoy_proxy)
declare -A PORT_VAR=(
  [postgres]=LOADTEST_PG_PORT [proxy]=LOADTEST_PROXY_PORT [nats]=LOADTEST_NATS_PORT
  [nats_monitor]=LOADTEST_NATS_MONITOR_PORT [listener]=LOADTEST_LISTENER_PORT
  [dispatch]=LOADTEST_DISPATCH_PORT [pprof]=LOADTEST_PPROF_PORT [envoy_proxy]=LOADTEST_ENVOY_PROXY_PORT
)
declare -A PORT_OFFSET=(
  [postgres]=0 [proxy]=1 [nats]=2 [nats_monitor]=3 [listener]=4 [dispatch]=5 [pprof]=6
  [envoy_proxy]=7
)
PORT_BASE=15000
PORT_STRIDE=10
PORT_BLOCKS=200

# port_block ROOT: ROOT's block of ports, an index k in [0, PORT_BLOCKS): the first 8 hex digits of
# the sha256 of ROOT's physical path, modulo PORT_BLOCKS. The same root always gets the same block.
port_block() {
  local hex
  hex=$(printf '%s' "$1" | sha256sum | cut -c1-8)
  printf '%s\n' "$((16#$hex % PORT_BLOCKS))"
}

# default_port KEY BLOCK: the default port of KEY in block BLOCK:
#
#   PORT_BASE + BLOCK * PORT_STRIDE + PORT_OFFSET[KEY] = 15000 + 10k + 0..7
#
# The 8 offsets differ and all are below the stride, so no two ports of a block and no two ports of
# two different blocks ever meet (two roots in one block hold the same 8 ports);
# every port is between 15000 and 16997, inside 1024-65535 and below the ephemeral range.
default_port() { printf '%s\n' "$((PORT_BASE + $2 * PORT_STRIDE + PORT_OFFSET[$1]))"; }

# stack_ports ROOT: ROOT's ports, "KEY PORT" a line in PORT_KEYS' order: the block's default port,
# or the value of the key's PORT_VAR when this environment sets it. stack.sh setup records them in
# ROOT/stack.json, which `stack.sh status` prints.
stack_ports() {
  local block key var
  block=$(port_block "$1")
  for key in "${PORT_KEYS[@]}"; do
    var=${PORT_VAR[$key]}
    printf '%s %s\n' "$key" "${!var:-$(default_port "$key" "$block")}"
  done
}

# lock_field NAME RECORD: the value of NAME= in a lock record.
lock_field() { sed -n "s/^$1=//p" <<<"$2"; }

# lock_pidns: this process's PID namespace ("pid:[N]"), empty when /proc does not show it.
lock_pidns() { readlink /proc/self/ns/pid 2>/dev/null || true; }

# lock_pid_alive PID: PID is a running process, not a zombie.
lock_pid_alive() {
  local stat
  [[ $1 =~ ^[0-9]+$ ]] || return 1
  { read -r stat <"/proc/$1/stat"; } 2>/dev/null || return 1
  [[ ${stat##*) } != Z* ]]
}

# lock_record_ns RECORD: where RECORD was written: "here" (this PID namespace), "elsewhere" (another
# one) or "unknown" (the record names no PID namespace).
lock_record_ns() {
  local ns
  ns=$(lock_field pidns "$1")
  if [ -z "$ns" ]; then
    echo unknown
  elif [ "$ns" = "$(lock_pidns)" ]; then
    echo here
  else
    echo elsewhere
  fi
}

# lock_record_runs RECORD: RECORD was written in this PID namespace and its pid runs. A pid of
# another PID namespace names an unrelated process here, or none.
lock_record_runs() {
  [ "$(lock_record_ns "$1")" = here ] && lock_pid_alive "$(lock_field pid "$1")"
}

# lock_holders FILE: "pid N (command)" of each process whose descriptor 9 is open on FILE, joined
# with ", ".
lock_holders() {
  local file=$1 dir cmd out=""
  for dir in /proc/[0-9]*; do
    [ "$dir/fd/9" -ef "$file" ] 2>/dev/null || continue
    cmd=$(tr '\0' ' ' <"$dir/cmdline" 2>/dev/null) || continue
    out+="${out:+, }pid ${dir#/proc/} (${cmd% })"
  done
  [ -z "$out" ] || printf '%s\n' "$out"
}

# lock_take FILE: this process has just locked FILE on descriptor 9; write its record.
lock_take() {
  local file=$1 command token
  # Empty the file before anything else, so that a refused caller never reads the previous
  # holder's record, still complete in the file, as this one's.
  : >"$file"
  command=$(tr '\0' ' ' <"/proc/$$/cmdline")
  token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  printf 'pid=%s\npidns=%s\ncommand=%s\nstarted=%s\ntoken=%s\n' "$$" "$(lock_pidns)" "${command% }" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$token" >"$file"
  export LOADTEST_LOCK_TOKEN=$token
}

lock_root() {
  local root=$1
  local file=$root/.run.lock
  command -v flock >/dev/null || die "flock (util-linux) is required"
  mkdir -p -- "$root"
  LOCK_INHERITED=false
  # A holder's own child keeps the descriptor 9 that it inherited, which holds the lock: opening the
  # file again on descriptor 9 would replace it, and the child would hold nothing. flock on the
  # holder's own open file description succeeds; on any other description, while the holder runs, it
  # fails.
  if [ -n "${LOADTEST_LOCK_TOKEN:-}" ] && [ /dev/fd/9 -ef "$file" ] &&
    [ "$(lock_field token "$(cat -- "$file" 2>/dev/null)")" = "$LOADTEST_LOCK_TOKEN" ] && flock -n 9; then
    LOCK_INHERITED=true
    return 0
  fi
  exec 9>&-
  # Try the lock for up to 3 s, on descriptor 8: lock_holders lists the processes with descriptor 9
  # on the file, and a waiting caller is none of them. While it fails, read the holder's record,
  # which the holder writes just after it locks, and stop at a complete record whose pid runs in this
  # PID namespace. A complete record whose pid does not is a holder whose children still hold the
  # lock (stop when lock_holders names them), one that has just ended (the next try takes the lock),
  # or one in another PID namespace, which this one never sees.
  local record="" holders="" runs=false i
  for ((i = 0; i < 30; i++)); do
    ((i == 0)) || sleep 0.1
    exec 8<>"$file"
    if flock -n 8; then
      # The duplicate shares the locked open file description, so descriptor 9 keeps the lock.
      exec 9<&8 8<&-
      lock_take "$file"
      return 0
    fi
    exec 8<&-
    record=$(cat -- "$file" 2>/dev/null || true)
    if [[ $(lock_field token "$record") =~ ^[0-9a-f]{32}$ ]]; then
      if lock_record_runs "$record"; then
        runs=true
        break
      fi
      holders=$(lock_holders "$file")
      [ -n "$holders" ] && break
    fi
  done
  local me=${0##*/} pid ns run
  pid=$(lock_field pid "$record")
  ns=$(lock_record_ns "$record")
  # A pid of another PID namespace is not written as "pid N": here, pid N is an unrelated process.
  if [ "$ns" = elsewhere ]; then
    run="pid=$pid in PID namespace $(lock_field pidns "$record")"
  else
    run="pid $pid"
  fi
  run+=", started $(lock_field started "$record"): $(lock_field command "$record")"
  $runs || [ -n "$holders" ] || holders=$(lock_holders "$file")
  if $runs; then
    printf '%s: LOADTEST_ROOT %s is in use by another run (%s)\n' "$me" "$root" "$run" >&2
  elif [ -n "$holders" ]; then
    local taker="left no record in the lock file"
    if [ -n "$pid" ]; then
      case $ns in
        here) taker="($run) has exited" ;;
        elsewhere) taker="($run) wrote its record in another PID namespace" ;;
        *) taker="($run) wrote no PID namespace in its record, so its pid is not trusted" ;;
      esac
    fi
    printf '%s: LOADTEST_ROOT %s is in use by another run: %s holds its lock; the run that took it %s\n' \
      "$me" "$root" "$holders" "$taker" >&2
  else
    # The lock is held, yet this PID namespace shows no holder: neither the record's pid nor a
    # process with descriptor 9 on the file.
    local named="the lock file holds no record of that run"
    if [ -n "$pid" ]; then
      case $ns in
        here) named="the lock file names that run as $run, which has exited" ;;
        elsewhere) named="the lock file names that run as $run, written in another PID namespace" ;;
        *) named="the lock file names that run as $run, in a record without its PID namespace" ;;
      esac
    fi
    printf '%s: LOADTEST_ROOT %s is locked by a process that is not visible in this PID namespace, such as a run in another PID namespace (another Legion role in this pod on a shared LOADTEST_ROOT); %s. Do not remove %s: that run still holds it, and a run that locked a new lock file would reset the stack under it\n' \
      "$me" "$root" "$named" "$file" >&2
  fi
  die "one run at a time per root ($file): wait for that run to end, or use another LOADTEST_ROOT"
}

# build_migration_versions prints the version of each migration in directory $1, one per line: the
# leading digits of every [0-9]*.up.sql name, as the runners read them (internal/pgmigrate).
build_migration_versions() {
  local file
  for file in "$1"/[0-9]*.up.sql; do
    file=${file##*/}
    printf '%s\n' "$((10#${file%%_*}))"
  done
}

# seed_migration_check compares the migration versions $2 (one per line) of the build $3 describes
# with the set the seed's meta file $1 records; a non-empty $4 says $2 is only the build's highest.
# It dies when the seed holds a version this build lacks, since a newer build made that seed, and
# notes the versions this build holds and the seed lacks. It compares the sets, not the highest
# version of each: a migration whose version is below one the seed already holds, a numbered
# migration merged after a timestamped one, leaves the highest version the same on both sides.
seed_migration_check() {
  local meta=$1 build=$2 what=$3 only_highest=${4:-}
  local seed behind ahead seed_max build_max reseed
  reseed="reseed with this build (stack.sh seed --force)"
  if [[ -n "$only_highest" ]] || ! jq -e 'has("migrations")' "$meta" >/dev/null; then
    # A seed.meta.json written before the seed recorded its versions holds only the highest one, and
    # so does a build record written before setup recorded the build's, so only the highest can be
    # compared: neither can say which versions are below it.
    seed_max=$(jq -er .migration "$meta")
    build_max=$(printf '%s\n' "$build" | sort -n | tail -n1)
    if ((build_max < seed_max)); then
      die "this build's ($what) highest migration is $build_max, older than the seed's $seed_max (seed made by $(jq -r .build_commit "$meta")); $reseed or run a newer one"
    fi
    note "only the highest migrations are compared, the seed's $seed_max and this build's ($what) $build_max, since one of the two records only its highest; $reseed to compare the whole set"
    return 0
  fi
  seed=$(jq -r '.migrations[]' "$meta")
  behind=$(grep -Fxv -f <(printf '%s\n' "$build") <<<"$seed" || true)
  ahead=$(grep -Fxv -f <(printf '%s\n' "$seed") <<<"$build" || true)
  if [[ -n "$behind" ]]; then
    die "the seed holds $(wc -l <<<"$behind") migration(s) this build ($what) does not have, the highest $(sort -n <<<"$behind" | tail -n1), so a newer build made it (seed made by $(jq -r .build_commit "$meta")); $reseed or run a newer one"
  fi
  if [[ -n "$ahead" ]]; then
    note "this build ($what) holds $(wc -l <<<"$ahead") migration(s) the seed lacks, the highest $(sort -n <<<"$ahead" | tail -n1). Dispatch migrates each step's copy forward at start, and any backfill those migrations start runs inside the step; $reseed to measure it alone"
  fi
}

prepare_stack() {
  local stack
  stack=$(dirname -- "${BASH_SOURCE[0]}")/stack.sh
  "$stack" setup
  "$stack" start
  "$stack" seed
  # A build cannot start on a seed that a newer build migrated: compare the migrations of the
  # Dispatch that setup built (bin/envoy-dispatch.json, which names LOADTEST_DISPATCH_COMMIT's tree
  # when that is set, and the Dispatch checkout's otherwise) with the set the seed records. Without
  # that record, the Dispatch checkout's migration files stand for it.
  # A record written before setup recorded the build's versions holds only its highest one.
  local build_json=$ROOT/bin/envoy-dispatch.json build_versions build_what only_highest=""
  if [ -f "$build_json" ]; then
    build_what="Dispatch $(jq -r .dispatch_commit "$build_json")"
    if jq -e 'has("migrations")' "$build_json" >/dev/null; then
      build_versions=$(jq -r '.migrations[]' "$build_json")
    else
      build_versions=$(jq -er .newest_migration "$build_json")
      only_highest=1
    fi
  else
    build_versions=$(build_migration_versions "$DISPATCH_SRC/packages/envoy/internal/dispatch/store/migrations")
    build_what="the Dispatch checkout's migrations"
  fi
  seed_migration_check "$ROOT/seed.meta.json" "$build_versions" "$build_what" "$only_highest"
  BUILD=$("$stack" build-info)
}
