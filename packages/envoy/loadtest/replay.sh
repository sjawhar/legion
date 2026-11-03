# shellcheck shell=bash
# replay.sh: the LINUX replay's own stack.sh commands (README.md), sourced by stack.sh, which
# dispatches them and documents them in its header. Not executable.
#
# It holds replay-seed-patch-check and replay-seed (the seed build at REPLAY_COMMIT, the work
# Dispatch still owes, the dump and its parts), replay-manifest, replay-get (the manifest's parts,
# the bundle, the restored seed template), replay-prepare and its prepared templates, and the
# `reset --replay` that resets database dispatch from one (cmd_reset_replay). Its functions call
# stack.sh's own helpers (sql, spawn, start_dispatch, build_dispatch_at, loadgen) and common.sh's
# (die, note, sha256_of, joined_sha256, and the part download and bundle verify that cluster.sh
# shares with it: replay_fetch_parts, replay_get_bundle), which bash resolves when called, so stack.sh
# sources it before it defines them; its top-level constants read $here and $ROOT, which stack.sh
# sets before the source line. $here, $ROOT, $BIN, $LOGS, $PGBIN, base_env and BUILT_TREE (set by
# build_dispatch_at) are stack.sh's (SC2154).
# shellcheck disable=SC2154

# The LINUX replay's seed: Dispatch at production's commit on 2026-10-26 (newest migration 0114,
# no cmd/dispatch/pprof.go), with the allocation-free len16 patch unless --stock, on its own database.
REPLAY_COMMIT=35efd326b66aea9ee2476396dabab7f7685fdbf3
REPLAY_MIGRATION=114
REPLAY_PATCH=$here/replay-seed-len16.patch
REPLAY_DB=dispatch_replay_build
# The replay's inputs (replay-get) and templates (replay-prepare, reset --replay): the manifest
# that names the bundle's and the seed dump's parts, the restored seed's template database and how
# many prepared templates are kept. The manifest names no Dispatch issue: the parts are file
# artifacts of an issue the operator names in LOADTEST_REPLAY_ISSUE, which replay-get requires.
REPLAY_MANIFEST=$here/replay-2026-10-26.json
REPLAY_DIR=$ROOT/replay
REPLAY_SEED_DB=dispatch_replay_seed
REPLAY_KEEP=3
BUNDLE_META=bundle.meta.json

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
    keys=$(abs_file --keys-file "$keys")
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

# ---- the LINUX replay's inputs and templates ---------------------------------------------------------

# now_ns: the time now, UTC, to the nanosecond, so last_used_at orders prepared templates.
now_ns() { date -u +%FT%T.%NZ; }

cmd_replay_manifest() {
  local usage="usage: stack.sh replay-manifest --bundle-pack FILE --bundle DIR [--seed-dir DIR] [--part-version N] > MANIFEST"
  local pack="" bundle="" seed_dir="" part_version=1
  while [ $# -gt 0 ]; do
    case $1 in
      --bundle-pack | --bundle | --seed-dir | --part-version)
        if [ $# -lt 2 ] || [ -z "$2" ]; then die "$1 needs a value; $usage"; fi
        case $1 in
          --bundle-pack) pack=$2 ;;
          --bundle) bundle=$2 ;;
          --seed-dir) seed_dir=$2 ;;
          --part-version) part_version=$2 ;;
        esac
        shift 2
        ;;
      *) die "unknown argument '$1'; $usage" ;;
    esac
  done
  [[ $part_version =~ ^[1-9][0-9]*$ ]] || die "--part-version must be a positive integer, not '$part_version'; $usage"
  { [ -n "$pack" ] && [ -n "$bundle" ]; } || die "--bundle-pack and --bundle are required; $usage"
  [ -f "$pack" ] || die "no replay-pack output at $pack"
  [ -f "$bundle/$BUNDLE_META" ] || die "no $BUNDLE_META in $bundle"
  local seed_args=(--argjson seed '[]')
  if [ -n "$seed_dir" ]; then
    [ -f "$seed_dir/seed.meta.json" ] || die "no seed.meta.json in $seed_dir"
    seed_args=(--slurpfile seed "$seed_dir/seed.meta.json")
  fi
  # A part's slug is its file name lower-cased with every character outside [a-z0-9] a '-'; its
  # version is --part-version (default 1: each part uploaded once; a part uploaded again under the
  # same name is a later version of the same artifact). The manifest names no Dispatch issue, so a
  # part is {file, slug, version, bytes, sha256} alone and replay-get takes the issue from
  # LOADTEST_REPLAY_ISSUE. The seed check's *_list fields are dropped: a passing check has none,
  # and they would name production issues.
  # shellcheck disable=SC2016
  jq -n --argjson version "$part_version" --slurpfile pack "$pack" --slurpfile meta "$bundle/$BUNDLE_META" "${seed_args[@]}" '
    def part: (.file | ascii_downcase | gsub("[^a-z0-9]"; "-")) as $s |
      {file, slug: $s, version: $version, bytes, sha256};
    $meta[0] as $m | $pack[0] as $p | $seed[0] as $s |
    {schema: 1, project: $m.project, window: {from: $m.at, to: $m.until},
     bundle: {tar_zst_sha256: $p.tar_zst_sha256, bytes: $p.bytes, fetched_at: $m.fetched_at,
              counts: $m.counts, parts: [$p.parts[] | part]},
     seed: (if $s == null then null else
       {dump_sha256: $s.dump.sha256, bytes: $s.dump.bytes, compression: $s.compression.chosen,
        migration: $s.migration, dispatch_commit: $s.dispatch_commit, dispatch_tree: $s.dispatch_tree,
        patch: $s.patch, patch_sha256: $s.patch_sha256, go_version: $s.go_version,
        postgres_version: $s.postgres_version, seed_seconds: $s.seed_seconds, keys_file: $s.keys_file,
        counts: $s.counts, check: ($s.check | with_entries(select(.key | endswith("_list") | not))),
        parts: [$s.parts[] | part]} end)}'
}

# drop_prepared_templates: drop every prepared template (dispatch_replay_<16 hex>) and its record.
drop_prepared_templates() {
  local db
  for db in $(sql postgres "select datname from pg_database where datname ~ '^dispatch_replay_[0-9a-f]{16}\$'"); do
    sql postgres "drop database if exists $db" >/dev/null
    note "dropped prepared template $db"
  done
  rm -f -- "$REPLAY_DIR"/prepared/*.json
}

# restore_replay_seed MANIFEST DUMP_PARTS...: restore the joined dump once into $REPLAY_SEED_DB and
# write seed-restored.json; the same dump sha256 restored before is kept, another one drops the old
# restore and every prepared template first.
restore_replay_seed() {
  local manifest=$1
  shift
  local restored=$REPLAY_DIR/seed-restored.json want
  want=$(jq -er .seed.dump_sha256 "$manifest")
  if [ -f "$restored" ] && [ "$(jq -r .seed_dump_sha256 "$restored")" = "$want" ] && db_exists "$REPLAY_SEED_DB"; then
    note "seed dump $want is already restored into $REPLAY_SEED_DB; kept"
    return 0
  fi
  if [ -f "$restored" ] || db_exists "$REPLAY_SEED_DB"; then
    note "$REPLAY_SEED_DB holds another seed dump or an unfinished restore: dropping it and every prepared template"
    drop_prepared_templates
  fi
  rm -f -- "$restored"
  sql postgres "drop database if exists $REPLAY_SEED_DB" >/dev/null
  sql postgres "create database $REPLAY_SEED_DB" >/dev/null
  local dump=$ROOT/tmp/replay-seed.dump log=$LOGS/replay-restore.log started=$SECONDS
  mkdir -p -- "$ROOT/tmp"
  cat -- "$@" >"$dump"
  note "pg_restore -j 2 of seed dump $want into $REPLAY_SEED_DB; log $log"
  env -i "${base_env[@]}" "$(pg_tool pg_restore)" --no-owner --no-privileges -j 2 -h 127.0.0.1 \
    -p "$(port postgres)" -U postgres -d "$REPLAY_SEED_DB" "$dump" >"$log" 2>&1 || {
    rm -f -- "$dump"
    tail -n 20 -- "$log" >&2 || true
    die "pg_restore of the replay seed failed; see $log"
  }
  rm -f -- "$dump"
  local seconds=$((SECONDS - started)) migration
  migration=$(sql "$REPLAY_SEED_DB" 'select max(version) from schema_migrations')
  [ "$migration" = "$(jq -r .seed.migration "$manifest")" ] ||
    die "the restored replay seed is at migration $migration, the manifest says $(jq -r .seed.migration "$manifest")"
  jq -n --arg sha "$want" --argjson migration "$migration" --argjson seconds "$seconds" \
    --arg pg "$("$PGBIN/postgres" --version)" --arg at "$(date -u +%FT%TZ)" \
    --argjson passed "$(jq '.seed.check.passed == true' "$manifest")" \
    --arg commit "$(jq -r .seed.dispatch_commit "$manifest")" \
    '{seed_dump_sha256: $sha, migration: $migration, restore_seconds: $seconds, postgres_version: $pg,
      restored_at: $at, check_passed: $passed, dispatch_commit: $commit}' >"$restored"
  note "restored the replay seed into $REPLAY_SEED_DB in $seconds s (migration $migration)"
}

cmd_replay_get() {
  local usage="usage: stack.sh replay-get [--manifest FILE] [--from-dir DIR]"
  local manifest=$REPLAY_MANIFEST from_dir=""
  while [ $# -gt 0 ]; do
    case $1 in
      --manifest | --from-dir)
        if [ $# -lt 2 ] || [ -z "$2" ]; then die "$1 needs a value; $usage"; fi
        case $1 in
          --manifest) manifest=$2 ;;
          --from-dir) from_dir=$2 ;;
        esac
        shift 2
        ;;
      *) die "unknown argument '$1'; $usage" ;;
    esac
  done
  # The manifest names no Dispatch issue: the operator names the issue its parts are file
  # artifacts of, and replay-get refuses before it reads or downloads anything without it.
  [ -n "${LOADTEST_REPLAY_ISSUE:-}" ] ||
    die "LOADTEST_REPLAY_ISSUE is not set: set it to the issue the replay's parts are file artifacts of (the manifest names none)"
  [ -f "$manifest" ] || die "no replay manifest at $manifest"
  [ "$(jq -r .schema "$manifest")" = 1 ] || die "$manifest is not a schema 1 replay manifest"
  [ -z "$from_dir" ] || [ -d "$from_dir" ] || die "--from-dir $from_dir is not a directory"
  need_stack
  [ -x "$BIN/loadgen" ] || die "no $BIN/loadgen; run stack.sh setup first"
  local has_seed
  has_seed=$(jq '.seed != null' "$manifest")
  if [ "$has_seed" = true ]; then
    running postgres || die "Postgres is not running; stack.sh start first (replay-get restores the seed into it)"
  fi
  local started=$SECONDS
  replay_get_bundle "$manifest" "$REPLAY_DIR" "$from_dir" loadgen
  if [ "$has_seed" != true ]; then
    note "the manifest names no seed (seed is null): nothing restored"
    note "replay-get done in $((SECONDS - started)) s"
    return 0
  fi
  replay_fetch_parts "$manifest" seed "$REPLAY_DIR/parts" "$from_dir"
  restore_replay_seed "$manifest" "${REPLAY_PARTS[@]}"
  note "replay-get done in $((SECONDS - started)) s"
}

# build_key: the first 16 hex of bin/envoy-dispatch.json's dispatch_binary_sha256, the build's
# prepared template key. Sets BUILD_COMMIT and BUILD_MIGRATION too.
build_key() {
  local build=$BIN/envoy-dispatch.json sha
  [ -f "$build" ] || die "no $build; run stack.sh setup first"
  sha=$(jq -er .dispatch_binary_sha256 "$build")
  BUILD_KEY=${sha:0:16}
  [[ $BUILD_KEY =~ ^[0-9a-f]{16}$ ]] || die "$build's dispatch_binary_sha256 '$sha' is not a sha256"
  BUILD_COMMIT=$(jq -r .dispatch_commit "$build")
  BUILD_MIGRATION=$(jq -er .newest_migration "$build")
}

# touch_prepared RECORD: refresh a prepared template's last_used_at.
# shellcheck disable=SC2016
touch_prepared() { json_set "$1" '.last_used_at = $t' --arg t "$(now_ns)"; }

# prune_prepared: keep the REPLAY_KEEP most recently used prepared templates; drop the rest, each
# database with its record.
prune_prepared() {
  local files=() f i db
  mapfile -t files < <(for f in "$REPLAY_DIR"/prepared/*.json; do
    [ -e "$f" ] && printf '%s\t%s\n' "$(jq -r .last_used_at "$f")" "$f"
  done | sort -r | cut -f2)
  for ((i = REPLAY_KEEP; i < ${#files[@]}; i++)); do
    f=${files[i]}
    db=dispatch_replay_$(basename -- "$f" .json)
    sql postgres "drop database if exists $db" >/dev/null
    rm -f -- "$f"
    note "dropped the least recently used prepared template $db"
  done
}

cmd_replay_prepare() {
  [ $# -eq 0 ] || die "usage: stack.sh replay-prepare"
  need_stack
  running postgres || die "Postgres is not running; stack.sh start first"
  build_key
  local restored=$REPLAY_DIR/seed-restored.json
  { [ -f "$restored" ] && db_exists "$REPLAY_SEED_DB"; } ||
    die "no restored replay seed (database $REPLAY_SEED_DB and $restored); run stack.sh replay-get first"
  local seed_migration seed_sha
  seed_migration=$(jq -er .migration "$restored")
  seed_sha=$(jq -er .seed_dump_sha256 "$restored")
  ((BUILD_MIGRATION >= seed_migration)) ||
    die "this build (envoy-dispatch from $BUILD_COMMIT) has newest migration $BUILD_MIGRATION, older than the replay seed's $seed_migration; build a newer Dispatch (LOADTEST_DISPATCH_COMMIT=<rev> stack.sh setup)"
  [ "$(jq -r .check_passed "$restored")" = true ] ||
    die "the restored replay seed's check did not pass (the manifest's seed.check.passed is not true); refusing to prepare it"
  local db=dispatch_replay_$BUILD_KEY rec=$REPLAY_DIR/prepared/$BUILD_KEY.json
  if [ -f "$rec" ] && db_exists "$db" && [ "$(jq -r .seed_dump_sha256 "$rec")" = "$seed_sha" ]; then
    touch_prepared "$rec"
    note "prepared template $db for this build (envoy-dispatch from $BUILD_COMMIT) exists; reused"
    return 0
  fi
  local p
  for p in nats listener; do [ -x "$(binary_of "$p")" ] || die "no $(binary_of "$p"); run stack.sh setup first"; done
  local started=$SECONDS log=$LOGS/dispatch-replay-prepare-$BUILD_KEY.log
  mkdir -p -- "$REPLAY_DIR/prepared"
  rm -f -- "$rec"
  stop_one dispatch
  stop_one listener
  sql postgres "drop database if exists $db" >/dev/null
  sql postgres "create database $db template $REPLAY_SEED_DB" >/dev/null
  stop_one nats
  start_nats
  start_listener
  trap 'stop_one dispatch' EXIT
  # Dispatch migrates the copy forward before it serves /healthz; the template is not measured, so
  # Dispatch reaches Postgres directly.
  note "preparing $db: envoy-dispatch from $BUILD_COMMIT (migration $BUILD_MIGRATION) on the seed (migration $seed_migration); log $log"
  LOADTEST_PG_DIRECT=1 start_dispatch "$log" "$BIN/envoy-dispatch" "$db" 1800
  local backfill_started=$SECONDS
  if has_table "$db" document_block_versions && has_table "$db" block_grammar_owed; then
    note "waiting for Dispatch's block-index backfill in $db (at most 3600 s)"
    local deadline=$((SECONDS + 3600)) logged=$SECONDS pending
    while :; do
      pending=$(sql "$db" 'select (select count(*) from document_block_versions where version is null) + (select count(*) from block_grammar_owed)')
      [ "$pending" = 0 ] && break
      running dispatch || die "Dispatch exited while the block index still owed $pending entries; see $log"
      ((SECONDS < deadline)) || die "Dispatch still owes $pending block-index entries after 3600 s; see $log"
      if ((SECONDS - logged >= 30)); then
        note "block-index backfill: $pending entries owed after $((SECONDS - backfill_started)) s"
        logged=$SECONDS
      fi
      sleep 2
    done
  fi
  local backfill_seconds=$((SECONDS - backfill_started))
  note "block-index backfill done after $backfill_seconds s"
  wait_quiet "$db" "$log"
  stop_one dispatch
  trap - EXIT
  note "vacuum analyze $db"
  sql "$db" 'vacuum analyze' >/dev/null
  local migration_after at
  migration_after=$(sql "$db" 'select max(version) from schema_migrations')
  at=$(now_ns)
  jq -n --arg key "$BUILD_KEY" --arg commit "$BUILD_COMMIT" \
    --arg binary_sha "$(jq -r .dispatch_binary_sha256 "$BIN/envoy-dispatch.json")" --arg seed "$seed_sha" \
    --argjson before "$seed_migration" --argjson after "$migration_after" \
    --argjson seconds "$((SECONDS - started))" --argjson backfill "$backfill_seconds" \
    --argjson quiet "$QUIET_SECONDS" --arg at "$at" \
    '{key: $key, dispatch_commit: $commit, dispatch_binary_sha256: $binary_sha, seed_dump_sha256: $seed,
      migration_before: $before, migration_after: $after, prepare_seconds: $seconds,
      backfill_wait_seconds: $backfill, quiet_wait_seconds: $quiet, created_at: $at, last_used_at: $at}' >"$rec"
  note "prepared template $db in $((SECONDS - started)) s: $(jq -c . "$rec")"
  prune_prepared
}

# block_index_owed DB: the block-index entries Dispatch owes in DB (document_block_versions with a
# null version and block_grammar_owed), 0 when the tables do not exist.
block_index_owed() {
  local n=0
  if has_table "$1" document_block_versions; then
    n=$((n + $(sql "$1" 'select count(*) from document_block_versions where version is null')))
  fi
  if has_table "$1" block_grammar_owed; then
    n=$((n + $(sql "$1" 'select count(*) from block_grammar_owed')))
  fi
  printf '%s\n' "$n"
}

cmd_reset_replay() {
  local log=${1:-$LOGS/dispatch.log}
  need_stack
  running postgres || die "Postgres is not running; stack.sh start first"
  build_key
  local db=dispatch_replay_$BUILD_KEY rec=$REPLAY_DIR/prepared/$BUILD_KEY.json restored=$REPLAY_DIR/seed-restored.json
  { [ -f "$rec" ] && db_exists "$db"; } ||
    die "no prepared replay template for this build (envoy-dispatch from $BUILD_COMMIT, key $BUILD_KEY); run stack.sh replay-prepare"
  local seed_sha
  seed_sha=$(jq -r .seed_dump_sha256 "$rec")
  { [ -f "$restored" ] && [ "$(jq -r .seed_dump_sha256 "$restored")" = "$seed_sha" ]; } ||
    die "prepared template $db was made from seed dump $seed_sha, not the restored one; run stack.sh replay-prepare"
  local last=$REPLAY_DIR/last-reset.json
  rm -f -- "$last"
  stop_one dispatch
  stop_one listener
  sql postgres 'drop database if exists dispatch' >/dev/null
  sql postgres "create database dispatch template $db" >/dev/null
  local checksum owed migration
  checksum=$(loadgen replay-checksum -db dispatch) || die "loadgen replay-checksum -db dispatch failed"
  owed=$(block_index_owed dispatch)
  migration=$(sql dispatch 'select max(version) from schema_migrations')
  touch_prepared "$rec"
  jq -n --arg template "$db" --arg key "$BUILD_KEY" --arg seed "$seed_sha" --argjson checksum "$checksum" \
    --argjson migration "$migration" --slurpfile prepared "$rec" --argjson owed "$owed" \
    --arg at "$(date -u +%FT%TZ)" \
    '{template: $template, key: $key, seed_dump_sha256: $seed, checksum: $checksum, migration: $migration,
      prepared: $prepared[0], backfill_empty: ($owed == 0), reset_at: $at}' >"$last"
  note "database dispatch from $db: checksum $(jq -c .checksum "$last"), block index owes $owed"
  stop_one nats
  start_nats
  start_listener
  start_dispatch "$log"
}
