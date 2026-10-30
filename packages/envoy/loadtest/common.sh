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
# exports them as LOADTEST_SRC and LOADTEST_ROOT.
#
# stack_ports ROOT: the stack's ports for ROOT, "KEY PORT" a line. The defaults follow the root's
# physical path (port_block, default_port): a root always gets the same block of 7 ports, and two
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
# prepare_stack runs stack.sh setup (an incremental build of SRC), start and seed, each of which
# does nothing when its work is already done, and the migration check: a build whose highest
# migration is older than the seed's is refused with exit 1. Then it sets BUILD to
# `stack.sh build-info` ("<commit> <dirty>"). run-step.sh calls it before a step that is not part of
# a series; run-series.sh calls it once, before its first step.
#
# The replay's inputs (README.md, The replay): stack.sh replay-get and cluster.sh replay-prepare
# both download and verify the manifest's parts with replay_fetch_parts and replay_get_bundle
# below. The manifest names no issue; LOADTEST_REPLAY_ISSUE names the one the parts are file
# artifacts of, and nothing is read or downloaded without it.

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
}

# ---- the stack's ports ---------------------------------------------------------------------------

# The stack's default ports follow LOADTEST_ROOT, so two roots on one machine (two Legion roles of
# one pod, each with its own root and its own PID namespace) almost always run side by side with no
# port variable set: each root hashes to one of PORT_BLOCKS blocks, so two roots land in one block
# about 1 pair in 200, and then every port of the two stacks is the same one (check_port refuses
# the second stack; the remedy is the LOADTEST_*_PORT variables). PORT_KEYS names stack.json's
# ports, PORT_VAR each one's environment variable and PORT_OFFSET its place in a root's block of
# ports.
PORT_KEYS=(postgres proxy nats nats_monitor listener dispatch pprof)
declare -A PORT_VAR=(
  [postgres]=LOADTEST_PG_PORT [proxy]=LOADTEST_PROXY_PORT [nats]=LOADTEST_NATS_PORT
  [nats_monitor]=LOADTEST_NATS_MONITOR_PORT [listener]=LOADTEST_LISTENER_PORT
  [dispatch]=LOADTEST_DISPATCH_PORT [pprof]=LOADTEST_PPROF_PORT
)
declare -A PORT_OFFSET=(
  [postgres]=0 [proxy]=1 [nats]=2 [nats_monitor]=3 [listener]=4 [dispatch]=5 [pprof]=6
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
#   PORT_BASE + BLOCK * PORT_STRIDE + PORT_OFFSET[KEY] = 15000 + 10k + 0..6
#
# The 7 offsets differ and all are below the stride, so no two ports of a block and no two ports of
# two different blocks ever meet (two roots in one block hold the same 7 ports);
# every port is between 15000 and 16996, inside 1024-65535 and below the ephemeral range.
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

prepare_stack() {
  local stack
  stack=$(dirname -- "${BASH_SOURCE[0]}")/stack.sh
  "$stack" setup
  "$stack" start
  "$stack" seed
  # A build cannot start on a seed that a newer build migrated: compare the highest migration of the
  # Dispatch that setup built (bin/envoy-dispatch.json, which names LOADTEST_DISPATCH_COMMIT's tree
  # when that is set, and the checkout's otherwise) with the seed's level. Without that record, the
  # checkout's highest migration file stands for it.
  local build_json=$ROOT/bin/envoy-dispatch.json build_level build_what seed_level
  if [ -f "$build_json" ]; then
    build_level=$(jq -er .newest_migration "$build_json")
    build_what="Dispatch $(jq -r .dispatch_commit "$build_json")"
  else
    local migrations=$SRC/packages/envoy/internal/dispatch/store/migrations build_file
    build_file=$(cd -- "$migrations" && printf '%s\n' [0-9]*.up.sql | sort -n | tail -n1)
    build_level=$((10#${build_file%%_*}))
    build_what=$migrations/$build_file
  fi
  seed_level=$(jq -er .migration "$ROOT/seed.meta.json")
  if ((build_level < seed_level)); then
    die "this build's highest migration is $build_level ($build_what), older than the seed's $seed_level (seed made by $(jq -r .build_commit "$ROOT/seed.meta.json")); reseed with this build (stack.sh seed --force) or run a newer one"
  fi
  if ((build_level > seed_level)); then
    note "the seed is at migration $seed_level and this build ($build_what) at $build_level. Dispatch migrates each step's copy forward at start, and any backfill those migrations start runs inside the step; reseed with this build (stack.sh seed --force) to measure it alone"
  fi
  BUILD=$("$stack" build-info)
}

# random_hex N: N random bytes in hex.
random_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# SEED_COUNTS_SQL: the seed's counts that seed.meta.json records, one JSON object, the same for
# stack.sh seed and cluster.sh seed, so the record tells a seed with the reference documents
# (loadgen seed-refs) from one without them.
SEED_COUNTS_SQL="select json_build_object(
  'projects', (select count(*) from projects),
  'issues', (select count(*) from issues),
  'comments', (select count(*) from comments),
  'artifacts', (select count(*) from artifacts),
  'artifact_versions', (select count(*) from artifact_versions),
  'ref_targets', (select count(*) from artifacts where name like 'ref-target-%'),
  'ref_target_versions', (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id where a.name like 'ref-target-%'),
  'ref_sources', (select count(*) from artifacts where name = 'references.md'),
  'artifact_refs', (select count(*) from refs where to_kind = 'artifact'),
  'events', (select count(*) from events))"
export SEED_COUNTS_SQL

# ---- the replay's inputs ------------------------------------------------------------------------------

# sha256_of FILE: FILE's sha256, in hex.
sha256_of() { sha256sum -- "$1" | cut -d' ' -f1; }

# joined_sha256 FILE...: the sha256 of the files' concatenation.
joined_sha256() { cat -- "$@" | sha256sum | cut -d' ' -f1; }

# replay_download OUT ISSUE SLUG VERSION: one version of a Dispatch file artifact into OUT, with
# the caller's DISPATCH_URL and the bearer token in DISPATCH_TOKEN_FILE. The scripts run in the
# caller's environment (stack.sh starts only its stack processes under env -i), and the token
# reaches curl on its standard input, never on a command line or in the script's output.
replay_download() {
  [ -n "${DISPATCH_URL:-}" ] || die "DISPATCH_URL is not set: set it and DISPATCH_TOKEN_FILE to download the replay's parts, or pass --from-dir"
  [ -r "${DISPATCH_TOKEN_FILE:-}" ] || die "DISPATCH_TOKEN_FILE does not name a readable file: set it to download the replay's parts, or pass --from-dir"
  local token
  token=$(<"$DISPATCH_TOKEN_FILE")
  token=${token//[$'\r\n\t ']/}
  note "downloading dispatch://$2/artifact/$3 version $4"
  printf 'Authorization: Bearer %s\n' "$token" |
    curl -q -fsSL --retry 3 --max-time 900 -H @- -o "$1" "${DISPATCH_URL%/}/api/v1/issues/$2/artifacts/$3/versions/$4"
}

# replay_fetch_part ENTRY ISSUE FROM_DIR PARTS_DIR: put a manifest part (its JSON) at
# PARTS_DIR/<file>: kept when its sha256 matches, else copied from FROM_DIR (when not empty) or
# downloaded, and refused, deleted, when its sha256 differs from the manifest's.
replay_fetch_part() {
  local entry=$1 issue=$2 from_dir=$3 parts_dir=$4 file slug version want
  file=$(jq -er .file <<<"$entry")
  slug=$(jq -er .slug <<<"$entry")
  version=$(jq -er .version <<<"$entry")
  want=$(jq -er .sha256 <<<"$entry")
  [[ $file =~ ^[A-Za-z0-9._-]+$ ]] || die "the manifest's part file '$file' is not a plain file name"
  local dst=$parts_dir/$file
  if [ -f "$dst" ] && [ "$(sha256_of "$dst")" = "$want" ]; then
    note "part $file: kept (sha256 matches)"
    return 0
  fi
  local tmp=$dst.tmp
  rm -f -- "$tmp"
  if [ -n "$from_dir" ]; then
    [ -f "$from_dir/$file" ] || die "part $file: not in --from-dir $from_dir"
    cp -- "$from_dir/$file" "$tmp"
  elif ! replay_download "$tmp" "$issue" "$slug" "$version"; then
    rm -f -- "$tmp"
    die "part $file: the download of dispatch://$issue/artifact/$slug version $version failed"
  fi
  local got
  got=$(sha256_of "$tmp")
  if [ "$got" != "$want" ]; then
    rm -f -- "$tmp" "$dst"
    die "part $file: sha256 $got, the manifest says $want; deleted it"
  fi
  mv -f -- "$tmp" "$dst"
  note "part $file: $(stat -c %s -- "$dst") bytes, sha256 matches"
}

# replay_fetch_parts MANIFEST WHICH PARTS_DIR FROM_DIR: every part of the manifest's WHICH (bundle
# or seed) at PARTS_DIR/<file> (replay_fetch_part), then the joined parts' sha256 against WHICH's
# tar_zst_sha256 (bundle) or dump_sha256 (seed). The parts' paths, in order, go into REPLAY_PARTS.
# The parts are file artifacts of the issue LOADTEST_REPLAY_ISSUE names, which is required.
replay_fetch_parts() {
  local manifest=$1 which=$2 parts_dir=$3 from_dir=$4 key issue entry want got
  case $which in
    bundle) key=tar_zst_sha256 ;;
    seed) key=dump_sha256 ;;
    *) die "replay_fetch_parts: no part list $which" ;;
  esac
  # The manifest names no issue: the operator names it, and nothing is read or downloaded without
  # it (stack.sh replay-get, cluster.sh replay-prepare).
  [ -n "${LOADTEST_REPLAY_ISSUE:-}" ] ||
    die "LOADTEST_REPLAY_ISSUE is not set: set it to the issue the replay's parts are file artifacts of (the manifest names none)"
  issue=$LOADTEST_REPLAY_ISSUE
  mkdir -p -- "$parts_dir"
  REPLAY_PARTS=()
  while IFS= read -r entry; do
    replay_fetch_part "$entry" "$issue" "$from_dir" "$parts_dir"
    REPLAY_PARTS+=("$parts_dir/$(jq -r .file <<<"$entry")")
  done < <(jq -c --arg w "$which" '.[$w].parts[]' "$manifest")
  ((${#REPLAY_PARTS[@]} > 0)) || die "$manifest lists no $which parts"
  want=$(jq -er --arg w "$which" --arg k "$key" '.[$w][$k]' "$manifest")
  got=$(joined_sha256 "${REPLAY_PARTS[@]}")
  [ "$got" = "$want" ] || die "the joined $which parts' sha256 is $got, the manifest's $key $want"
}

# replay_get_bundle MANIFEST DIR FROM_DIR LOADGEN: the bundle's parts under DIR/parts
# (replay_fetch_parts), unpacked with `LOADGEN replay-unpack` into DIR/bundle (not again while
# DIR/bundle.unpacked.json names the same parts and the bundle's bundle.meta.json), then checked
# file by file with `LOADGEN replay-verify-bundle`, whose line goes into REPLAY_BUNDLE_VERIFIED.
# LOADGEN is the caller's command (a function) that runs its loadgen.
replay_get_bundle() {
  local manifest=$1 dir=$2 from_dir=$3 loadgen_cmd=$4 want
  replay_fetch_parts "$manifest" bundle "$dir/parts" "$from_dir"
  want=$(jq -er .bundle.tar_zst_sha256 "$manifest")
  local bundle=$dir/bundle marker=$dir/bundle.unpacked.json meta=bundle.meta.json
  if [ -f "$marker" ] && [ "$(jq -r .tar_zst_sha256 "$marker")" = "$want" ] && [ -f "$bundle/$meta" ] &&
    [ "$(jq -r .bundle_meta_sha256 "$marker")" = "$(sha256_of "$bundle/$meta")" ]; then
    note "bundle at $bundle is already unpacked from these parts; kept"
  else
    rm -f -- "$marker"
    rm -rf -- "$bundle.tmp"
    note "unpacking the bundle into $bundle"
    "$loadgen_cmd" replay-unpack -out "$bundle.tmp" "${REPLAY_PARTS[@]}" || die "loadgen replay-unpack of the bundle's parts failed"
    rm -rf -- "$bundle"
    mv -- "$bundle.tmp" "$bundle"
    jq -n --arg tar "$want" --arg meta "$(sha256_of "$bundle/$meta")" \
      '{tar_zst_sha256: $tar, bundle_meta_sha256: $meta}' >"$marker"
  fi
  REPLAY_BUNDLE_VERIFIED=$("$loadgen_cmd" replay-verify-bundle -dir "$bundle") || die "the bundle at $bundle does not verify (above)"
  note "bundle verified: $REPLAY_BUNDLE_VERIFIED"
}
