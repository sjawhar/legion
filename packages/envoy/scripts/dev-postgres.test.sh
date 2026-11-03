#!/usr/bin/env bash
# packages/envoy/scripts/dev-postgres.test.sh
#
# Proves dev-postgres.sh's reuse rule without Docker: a fake `docker` on PATH answers the inspects
# the script makes from the scenario its environment describes, and records every invocation, so
# each case asserts both what the script printed and every docker command it ran. A stale
# `dispatch-pg` (any image but the pinned one, pgvector on PostgreSQL 18) is refused naming the
# container, both images and, in order, the commands that list its databases, count the tables in
# its dispatch database and their rows, dump them, check the dump's last comment, restore them into
# a new container on the pinned image, list and count them again there, and remove the dump, while
# the old container stays, stopped and renamed, its removal left to the person as the refusal's
# last line of text; the script itself never stops, renames, removes, starts or creates anything
# for it. The printed listing, dump, dump check, run and restore lines are replayed through bash to
# prove they paste as they read.
# Its `quote`, which prints those commands, is held to fixtures/shell-quote.json, the table
# packages/dispatch's shellQuote test reads too, so the two copies of the rule cannot drift apart.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly script="${project_root}/packages/envoy/scripts/dev-postgres.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
mkdir -p "${temporary_dir}/bin"

readonly pinned="mirror.gcr.io/pgvector/pgvector@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
readonly pinned_id="sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
# The image an older version of dev-postgres.sh pinned: pgvector on PostgreSQL 16.
readonly old_pin="pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"
readonly old_pin_id="sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"
readonly stale_id="sha256:eb4759788a2182f08257135e61a34f2cfc3c2914079f3465d64ee62350f4d081"
readonly url="DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable"

# The fake docker. FAKE_CONTAINER=absent makes `container inspect` fail; otherwise the container
# runs image FAKE_IMAGE_ID (created as FAKE_CONFIG_IMAGE) and is running when FAKE_RUNNING=true.
# The pinned image is present when FAKE_PINNED_PRESENT=true.
cat >"${temporary_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${FAKE_CALLS:?}"
case "$1 $2" in
  "container inspect")
    [ "$FAKE_CONTAINER" = present ] || { echo "Error: No such container: dispatch-pg" >&2; exit 1; }
    case "$*" in
      *"{{.Image}}"*) printf '%s\n' "$FAKE_IMAGE_ID" ;;
      *"{{.State.Running}}"*) printf '%s\n' "$FAKE_RUNNING" ;;
      *"{{.Config.Image}}"*) printf '%s\n' "$FAKE_CONFIG_IMAGE" ;;
      "container inspect dispatch-pg") printf '[{}]\n' ;;
      *) echo "fake docker: unexpected inspect: $*" >&2; exit 2 ;;
    esac
    ;;
  "image inspect")
    [ "$FAKE_PINNED_PRESENT" = true ] || { echo "Error: No such image" >&2; exit 1; }
    printf '%s\n' "${FAKE_PINNED_ID:?}"
    ;;
  "run -d"|"start dispatch-pg") ;;
  *) echo "fake docker: unexpected command: $*" >&2; exit 2 ;;
esac
EOF
chmod +x "${temporary_dir}/bin/docker"

failures=0
fail() {
  printf 'FAIL: %s\n' "$1"
  printf '  stdout: %s\n' "$(cat "$out")"
  printf '  stderr: %s\n' "$(cat "$err")"
  printf '  docker: %s\n' "$(tr '\n' ';' <"$calls")"
  failures=$((failures + 1))
}

out="${temporary_dir}/out"
err="${temporary_dir}/err"
calls="${temporary_dir}/calls"
status=0
# Runs dev-postgres.sh with the scenario `env` describes; sets $status.
run_case() {
  : >"$out"
  : >"$err"
  : >"$calls"
  status=0
  env PATH="${temporary_dir}/bin:${PATH}" FAKE_CALLS="$calls" FAKE_PINNED_ID="$pinned_id" \
    FAKE_CONTAINER=present FAKE_PINNED_PRESENT=true FAKE_RUNNING=true \
    FAKE_IMAGE_ID="$pinned_id" FAKE_CONFIG_IMAGE="$pinned" \
    "$@" bash "$script" >"$out" 2>"$err" || status=$?
}
# Fails the case unless the script ran no docker command that changes anything.
read_only() {
  ! grep -qE '^(run|start|stop|rm|rename|create) ' "$calls"
}

run_case FAKE_CONTAINER=absent
if [ "$status" -ne 0 ] || [ "$(cat "$out")" != "$url" ] || ! grep -qxF "run -d --name dispatch-pg --cpus 2 --memory 2g -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 $pinned" "$calls"; then
  fail "no dispatch-pg: created from the pinned image"
else
  printf 'PASS: no dispatch-pg: created from the pinned image, URL printed\n'
fi

run_case
if [ "$status" -ne 0 ] || [ "$(cat "$out")" != "$url" ] || ! read_only; then
  fail "a running dispatch-pg on the pinned image: reused as it is"
else
  printf 'PASS: a running dispatch-pg on the pinned image: reused, nothing run or started\n'
fi

run_case FAKE_RUNNING=false
if [ "$status" -ne 0 ] || [ "$(cat "$out")" != "$url" ] || ! grep -qxF "start dispatch-pg" "$calls" || grep -q '^run ' "$calls"; then
  fail "a stopped dispatch-pg on the pinned image: started"
else
  printf 'PASS: a stopped dispatch-pg on the pinned image: started, not recreated\n'
fi

# The stale container this script's previous pin made: pgvector on PostgreSQL 16. Its dump goes
# under a TMPDIR with a space in it, so the printed redirects must quote their targets to paste.
readonly backup="dispatch-pg-${old_pin_id:7:12}"
readonly spaced_tmp="${temporary_dir}/tmp dir"
mkdir -p "$spaced_tmp"
readonly dump="${spaced_tmp}/${backup}.sql"
readonly list_databases="docker exec dispatch-pg psql -U postgres -Atc 'select datname from pg_database where not datistemplate order by 1'"
readonly count_dispatch="docker exec dispatch-pg psql -U postgres -d dispatch -Atc 'select count(*), coalesce(sum((xpath('\''/row/c/text()'\'', query_to_xml(format('\''select count(*) as c from %I.%I'\'', schemaname, tablename), false, true, '\'''\'')))[1]::text::bigint), 0) from pg_tables where schemaname = '\''public'\'''"
readonly dump_command="docker exec dispatch-pg pg_dumpall -U postgres --clean --if-exists > '$dump'"
readonly trailer_command="grep -c -x -- '-- PostgreSQL database cluster dump complete' '$dump'"
readonly wait_ready="docker exec dispatch-pg pg_isready -h 127.0.0.1 -U postgres"
# The dump-and-restore sequence the refusal prints, every line in this order. The listing and the
# count of tables and rows run twice, against the old container before the dump and the new one
# after the restore; the dump's last comment is checked before the old container stops.
readonly sequence=(
  "$list_databases"
  "$count_dispatch"
  "$dump_command"
  "$trailer_command"
  "docker stop dispatch-pg"
  "docker rename dispatch-pg $backup"
  "docker run -d --name dispatch-pg --cpus 2 --memory 2g -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 $pinned"
  "$wait_ready"
  "docker exec -i dispatch-pg psql -h 127.0.0.1 -U postgres -q -f - < '$dump'"
  "$list_databases"
  "$count_dispatch"
  "docker exec dispatch-pg psql -U postgres -Atc 'select 1 from pg_available_extensions where name = '\''vector'\'''"
  "docker exec dispatch-pg psql -U postgres -Atc 'show server_version_num'"
  "rm '$dump'"
)
# The line number of the refusal's first command line "dev-postgres:   <$1>" after line $2 (0 when
# omitted), or nothing: a command the sequence prints twice is found after the line before it.
line_of() {
  grep -nxF -- "dev-postgres:   $1" "$err" | cut -d: -f1 | awk -v after="${2:-0}" '$1 > after { print; exit }' || true
}
# Fails unless every line of `sequence` is printed, each after the one before it.
in_order() {
  local previous=0 line want
  for want in "${sequence[@]}"; do
    line="$(line_of "$want" "$previous")"
    if [ -z "$line" ]; then
      printf '  missing or out of order: %s\n' "$want"
      return 1
    fi
    previous="$line"
  done
}

run_case TMPDIR="$spaced_tmp" FAKE_IMAGE_ID="$old_pin_id" FAKE_CONFIG_IMAGE="$old_pin"
for want in \
  "dev-postgres: dispatch-pg runs $old_pin (image $old_pin_id), not the pinned $pinned, which is pgvector on PostgreSQL 18, the version Dispatch's tests and dev databases run." \
  "Keep the output of this" \
  "It counts the dispatch database alone; the check after the dump covers every database." \
  "Stop anything that writes to this container (a Dispatch server, a test run) before these counts and the dump" \
  "This prints 1 when pg_dumpall finished every database; if it prints 0 (a failed, cut or empty dump), stop here: the old container is still running and untouched." \
  "It must list every database the old container's listing printed" \
  "the dispatch database alone. It must print the same two numbers as the old container did" \
  "(a dump that failed or stopped partway, or a write to the old container's dispatch database after its count)" \
  "until it reports accepting connections" \
  "over TCP" \
  'role "postgres" already exists' \
  "current user cannot be dropped" \
  "print 1 and 18xxxx once the new container serves the restored data" \
  "The old one can come back: once it is renamed" \
  "\`docker rm -f -v dispatch-pg\` removes the new one" \
  "\`docker rename $backup dispatch-pg\` and \`docker start dispatch-pg\` put the old one back"; do
  if ! grep -qF -- "$want" "$err"; then
    fail "a running dispatch-pg on the old pin: the refusal says \"$want\""
  fi
done
if [ "$status" -ne 1 ] || [ -s "$out" ] || ! read_only || grep -qF "no pgvector" "$err" \
  || [ -n "$(line_of "docker start dispatch-pg")" ] || [ "$(grep -cxF -- "dev-postgres:   $wait_ready" "$err")" -ne 1 ] \
  || ! in_order; then
  fail "a running dispatch-pg on the old pin: refused (exit 1, no URL, nothing changed), the dump-and-restore sequence in order with no start and one readiness wait, no claim it lacks pgvector"
else
  printf 'PASS: a running dispatch-pg on the old pin: refused naming both images and the dump-and-restore sequence in order, nothing changed\n'
fi

# No printed command removes the old container: it stays, stopped, as $backup, and the refusal's
# last line leaves its removal to the person, as text, for once Dispatch works on the new container.
if grep -qE "^dev-postgres:   docker rm .*${backup}" "$err" \
  || [ "$(tail -n1 "$err")" != "dev-postgres: The old container stays, stopped, as $backup; once Dispatch works on the new container, \`docker rm -v $backup\` deletes it and its data." ]; then
  fail "a running dispatch-pg on the old pin: no printed command removes $backup, and the last line leaves its removal to the person as text"
else
  printf 'PASS: a running dispatch-pg on the old pin: no printed command removes the old container, the last line leaves its removal to the person as text\n'
fi

# The listing, dump, dump check, run and restore lines replayed as printed, in a directory of their
# own, with `docker` a function that prints its arguments one per line and then its stdin: the
# listing's query must arrive as one word, the dump's `>` must create the file at $dump, the dump
# check must read that file for its whole pattern (a dump holding no such comment, so 0), the run
# must pass the pinned image's flags word for word, and the restore's `<` must feed that same file
# back.
printed() {
  grep -F -- "dev-postgres:   $1" "$err" | head -n1 | cut -c17- || true
}
# A line that fails to replay prints its error, which no expected answer holds. `docker` is called
# only through the eval, which shellcheck cannot see.
replay() {
  # shellcheck disable=SC2317
  (cd "${temporary_dir}/replay" && docker() { printf '%s\n' "$@"; cat; } && eval "$1") 2>&1 || true
}
mkdir -p "${temporary_dir}/replay"
printed_list="$(printed "docker exec dispatch-pg psql -U postgres -Atc 'select datname ")"
printed_dump="$(printed "docker exec dispatch-pg pg_dumpall ")"
printed_trailer="$(printed "grep -c -x -- ")"
printed_run="$(printed "docker run ")"
printed_restore="$(printed "docker exec -i dispatch-pg psql ")"
rm -f "$dump"
replayed_list="$(replay "$printed_list" </dev/null)"
replayed_dump="$(replay "$printed_dump" </dev/null)"
replayed_trailer="$(replay "$printed_trailer" </dev/null)"
replayed_run="$(replay "$printed_run" </dev/null)"
replayed_restore="$(replay "$printed_restore" </dev/null)"
want_list="$(printf '%s\n' exec dispatch-pg psql -U postgres -Atc 'select datname from pg_database where not datistemplate order by 1')"
want_dump="$(printf '%s\n' exec dispatch-pg pg_dumpall -U postgres --clean --if-exists)"
want_run="$(printf '%s\n' run -d --name dispatch-pg --cpus 2 --memory 2g -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 "$pinned")"
want_restore="$(printf '%s\n' exec -i dispatch-pg psql -h 127.0.0.1 -U postgres -q -f - "$want_dump")"
if [ -z "$printed_list" ] || [ -z "$printed_dump" ] || [ -z "$printed_trailer" ] || [ -z "$printed_run" ] \
  || [ -z "$printed_restore" ] || [ "$replayed_list" != "$want_list" ] || [ -n "$replayed_dump" ] \
  || [ "$(cat "$dump" 2>/dev/null)" != "$want_dump" ] || [ "$replayed_trailer" != 0 ] \
  || [ "$replayed_run" != "$want_run" ] || [ "$replayed_restore" != "$want_restore" ]; then
  fail "the printed listing, dump, dump check, run and restore lines paste as they read"
  printf '  list: %s\n  replayed: %s\n' "$printed_list" "$(tr '\n' '|' <<<"$replayed_list")"
  printf '  dump: %s\n  replayed: %s\n  dump file: %s\n' "$printed_dump" "$(tr '\n' '|' <<<"$replayed_dump")" "$(tr '\n' '|' <"$dump" 2>/dev/null || true)"
  printf '  dump check: %s\n  replayed: %s\n' "$printed_trailer" "$(tr '\n' '|' <<<"$replayed_trailer")"
  printf '  run: %s\n  replayed: %s\n' "$printed_run" "$(tr '\n' '|' <<<"$replayed_run")"
  printf '  restore: %s\n  replayed: %s\n' "$printed_restore" "$(tr '\n' '|' <<<"$replayed_restore")"
else
  printf 'PASS: the printed listing, dump, dump check, run and restore lines paste as they read: the query is one word, the dump lands at its quoted path, the check reads it, the restore reads it back\n'
fi

# A stopped stale container: the sequence starts it first, since pg_dumpall needs a running
# server, then waits for it to accept connections before the listing and the dump, since a server
# still starting refuses them and the dump's redirect would leave an empty file; the script itself
# still starts nothing.
run_case TMPDIR="$spaced_tmp" FAKE_IMAGE_ID="$old_pin_id" FAKE_CONFIG_IMAGE="$old_pin" FAKE_RUNNING=false
start_line="$(line_of "docker start dispatch-pg")"
ready_line="$(line_of "$wait_ready" "${start_line:-0}")"
list_line="$(line_of "$list_databases")"
dump_line="$(line_of "$dump_command")"
if [ "$status" -ne 1 ] || [ -s "$out" ] || ! read_only || [ -z "$start_line" ] || [ -z "$ready_line" ] \
  || [ -z "$list_line" ] || [ -z "$dump_line" ] || [ "$ready_line" -ge "$list_line" ] \
  || ! grep -qF "a dump taken before then is refused and leaves an empty file" "$err" || ! in_order; then
  fail "a stopped dispatch-pg on the old pin: refused, the sequence starting it and waiting for it before the listing and the dump, nothing changed"
else
  printf 'PASS: a stopped dispatch-pg on the old pin: refused, the sequence starts it, waits for it, then lists and dumps, nothing changed\n'
fi

# The container an older version made from postgres:16, when the pinned image has never been
# pulled here: still refused, with the same sequence.
run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16 FAKE_PINNED_PRESENT=false
if [ "$status" -ne 1 ] || [ -s "$out" ] || ! read_only \
  || ! grep -qF "dispatch-pg runs postgres:16 (image $stale_id), not the pinned $pinned, which is pgvector on PostgreSQL 18" "$err" \
  || ! grep -qxF "dev-postgres:   docker rename dispatch-pg dispatch-pg-${stale_id:7:12}" "$err"; then
  fail "a postgres:16 dispatch-pg with no pinned image pulled: refused"
else
  printf 'PASS: a postgres:16 dispatch-pg with the pinned image never pulled: refused with the dump-and-restore sequence, nothing changed\n'
fi

# dev-postgres.sh's own `quote`, taken from the script as it ships (it runs on load, so it is not
# sourced), against the shared table: each word quotes to the table's value, and that value reads
# back in bash as exactly the one word.
eval "$(sed -n '/^quote() {$/,/^}$/p' "$script")"
if ! declare -F quote >/dev/null; then
  printf 'FAIL: no quote() function in %s\n' "$script"
  failures=$((failures + 1))
else
  table_failures=0
  table_cases=0
  while IFS= read -r word && IFS= read -r quoted; do
    table_cases=$((table_cases + 1))
    got="$(quote "$word")"
    eval "set -- $got"
    if [ "$got" != "$quoted" ] || [ "$#" -ne 1 ] || [ "$1" != "$word" ]; then
      printf 'FAIL: quote %q gave %s, the table says %s\n' "$word" "$got" "$quoted"
      table_failures=$((table_failures + 1))
    fi
  done < <(jq -r '.[] | .word, .quoted' "${project_root}/packages/envoy/scripts/fixtures/shell-quote.json")
  if [ "$table_cases" -eq 0 ]; then
    printf 'FAIL: fixtures/shell-quote.json holds no case\n'
    failures=$((failures + 1))
  elif [ "$table_failures" -ne 0 ]; then
    failures=$((failures + 1))
  else
    printf 'PASS: quote matches fixtures/shell-quote.json on all %d words, each reading back whole\n' "$table_cases"
  fi
fi

# The checks the refusal prints are what a person reads to decide the move worked before they remove
# the old container, the only other copy of the data, once the printed move ends. A dump that fails
# (the printed redirect then leaves an empty file) restores nothing and psql reports no error, and
# one that stops once its schema is written restores every table and no row, so the dump check must
# print 0 for both before the old container stops, and the listing and count after the restore must
# print something different after either than after a whole one, where they print what they
# printed before the dump; otherwise the checks pass a move that lost every row, or fail a good
# one. Every printed line is replayed as printed on two real PostgreSQL clusters, a fake docker
# mapping `dispatch-pg` to the old cluster until the rename and to the new one, which holds the
# database POSTGRES_DB names, from `docker run` on. Skipped, saying so, where no PostgreSQL server
# binaries are found, except under GitHub Actions, whose ubuntu runners ship them under
# /usr/lib/postgresql, where their absence fails.
pg_bindir=""
if command -v initdb >/dev/null 2>&1; then
  pg_bindir="$(dirname "$(command -v initdb)")"
else
  while IFS= read -r candidate; do
    if [ -x "${candidate}/initdb" ]; then
      pg_bindir="$candidate"
      break
    fi
  done < <(printf '%s\n' /usr/lib/postgresql/*/bin | sort -V -r)
fi
if [ -z "$pg_bindir" ] && [ "${GITHUB_ACTIONS:-}" = true ]; then
  printf 'FAIL: the move replay found no PostgreSQL server binaries (no initdb on PATH or under /usr/lib/postgresql/*/bin), which GitHub Actions ubuntu runners ship\n'
  failures=$((failures + 1))
elif [ -z "$pg_bindir" ]; then
  printf 'SKIP: the move replay on real clusters (no PostgreSQL server binaries: no initdb on PATH or under /usr/lib/postgresql/*/bin)\n'
else
  mkdir -p "${temporary_dir}/bin-pg"
  cat >"${temporary_dir}/bin-pg/docker" <<'EOF'
#!/usr/bin/env bash
set -u
rig="${RIG:?}"
bin="${PG_BINDIR:?}"
cluster_of() { cat "${rig}/names/$1" 2>/dev/null; }
start_cluster() {
  "${bin}/pg_ctl" -D "${rig}/$1/data" -l "${rig}/$1/log" -w -o "-k ${rig}/$1 -c listen_addresses=" start >/dev/null
}
case "$1" in
  exec)
    shift
    stdin=false
    if [ "$1" = -i ]; then stdin=true; shift; fi
    cluster="$(cluster_of "$1")" || { echo "Error: No such container: $1" >&2; exit 1; }
    shift
    command="$1"
    shift
    args=()
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -h ]; then shift 2; continue; fi
      args+=("$1")
      shift
    done
    if [ "$command" = pg_dumpall ]; then
      case "${FAKE_DUMP:?}" in
        empty)
          echo "pg_dumpall: error: connection to server failed" >&2
          exit 1
          ;;
        # A dump that stops partway (killed, or its redirect's disk full) once it has written the
        # schema: every CREATE TABLE and no row, the data sections being the dump's last and largest.
        cut)
          "${bin}/${command}" -h "${rig}/${cluster}" "${args[@]}" </dev/null | sed '/^COPY public\./,$d'
          echo "pg_dumpall: error: could not write to output file: No space left on device" >&2
          exit 1
          ;;
        whole) ;;
        *) echo "fake docker: unknown FAKE_DUMP: $FAKE_DUMP" >&2; exit 2 ;;
      esac
    fi
    if "$stdin"; then
      exec "${bin}/${command}" -h "${rig}/${cluster}" "${args[@]}"
    fi
    exec "${bin}/${command}" -h "${rig}/${cluster}" "${args[@]}" </dev/null
    ;;
  stop) "${bin}/pg_ctl" -D "${rig}/$(cluster_of "$2")/data" -m fast -w stop >/dev/null && echo "$2" ;;
  rename) mv "${rig}/names/$2" "${rig}/names/$3" ;;
  run)
    name=""
    database=""
    previous=""
    for arg in "$@"; do
      if [ "$previous" = --name ]; then name="$arg"; fi
      case "$arg" in POSTGRES_DB=*) database="${arg#POSTGRES_DB=}" ;; esac
      previous="$arg"
    done
    "${bin}/initdb" -D "${rig}/new/data" -U postgres -A trust -E UTF8 --locale=C >/dev/null
    start_cluster new
    "${bin}/psql" -h "${rig}/new" -U postgres -qc "create database ${database}"
    echo new >"${rig}/names/${name}"
    echo fake-container-id
    ;;
  start) start_cluster "$(cluster_of "$2")" && echo "$2" ;;
  *) echo "fake docker: unexpected command: $*" >&2; exit 2 ;;
esac
EOF
  chmod +x "${temporary_dir}/bin-pg/docker"

  # Builds the old cluster (a dispatch database holding 25 rows), prints the refusal and replays
  # every line it prints, in order, with the dump $1 (whole, empty or cut) makes. Writes what the
  # dump check printed to move-$1.trailer, what the listing and the count printed against the old
  # container before the dump to move-$1.before and against the new one after the restore to
  # move-$1.after, and the rows the new container's issues table holds (none: no such table) to
  # move-$1.rows.
  replay_move() {
    local rig="${temporary_dir}/move-$1" line output restored=false cluster
    mkdir -p "${rig}/old" "${rig}/new" "${rig}/names"
    : >"${rig}.trailer"
    : >"${rig}.before"
    : >"${rig}.after"
    "${pg_bindir}/initdb" -D "${rig}/old/data" -U postgres -A trust -E UTF8 --locale=C >/dev/null
    "${pg_bindir}/pg_ctl" -D "${rig}/old/data" -l "${rig}/old/log" -w -o "-k ${rig}/old -c listen_addresses=" start >/dev/null
    "${pg_bindir}/psql" -h "${rig}/old" -U postgres -qc "create database dispatch"
    "${pg_bindir}/psql" -h "${rig}/old" -U postgres -d dispatch -qc \
      "create table issues (key text primary key); insert into issues select 'DEV-' || g from generate_series(1, 25) g"
    echo old >"${rig}/names/dispatch-pg"
    run_case TMPDIR="$rig" FAKE_IMAGE_ID="$old_pin_id" FAKE_CONFIG_IMAGE="$old_pin"
    while IFS= read -r line; do
      output="$(env PATH="${temporary_dir}/bin-pg:${PATH}" RIG="$rig" PG_BINDIR="$pg_bindir" \
        FAKE_DUMP="$1" bash -c "$line" 2>&1 </dev/null || true)"
      # The listing and the count are found by their command, not their query, so the replay judges
      # whatever query the script prints.
      case "$line" in
        "docker exec dispatch-pg psql -U postgres -Atc 'select datname "* | "docker exec dispatch-pg psql -U postgres -d dispatch -Atc "*)
          if "$restored"; then
            printf '%s\n' "$output" >>"${rig}.after"
          else
            printf '%s\n' "$output" >>"${rig}.before"
          fi
          ;;
        "grep -c -x -- "*) printf '%s\n' "$output" >>"${rig}.trailer" ;;
        "docker exec -i dispatch-pg psql -h 127.0.0.1 -U postgres -q -f - < "*) restored=true ;;
      esac
    done < <(sed -n 's/^dev-postgres:   //p' "$err")
    "${pg_bindir}/psql" -h "${rig}/new" -U postgres -d dispatch -Atc 'select count(*) from issues' \
      >"${rig}.rows" 2>/dev/null || echo none >"${rig}.rows"
    for cluster in old new; do
      "${pg_bindir}/pg_ctl" -D "${rig}/${cluster}/data" -m immediate -w stop >/dev/null 2>&1 || true
    done
  }
  # What a replay wrote, for a failure's report.
  show_move() {
    printf '  rows: %s\n  dump check: %s\n' "$(cat "$1.rows")" "$(tr '\n' '|' <"$1.trailer")"
    printf '  before the dump: %s\n  after the restore: %s\n' "$(tr '\n' '|' <"$1.before")" "$(tr '\n' '|' <"$1.after")"
  }

  whole="${temporary_dir}/move-whole"
  replay_move whole
  if [ "$(cat "${whole}.rows")" != 25 ] || [ "$(cat "${whole}.trailer")" != 1 ] || [ ! -s "${whole}.before" ] \
    || ! cmp -s "${whole}.before" "${whole}.after"; then
    printf 'FAIL: a whole dump: the move replay restores its 25 rows, the dump check prints 1, and the listing and count after the restore print what they printed before the dump\n'
    show_move "$whole"
    failures=$((failures + 1))
  else
    printf 'PASS: a whole dump: the move replay restores its 25 rows, the dump check prints 1, and the listing and count after the restore print what they printed before the dump\n'
  fi

  # A dump that lost every row, made as mode $1 (described as $2), whose restore leaves $3 in the
  # new container's issues table: the dump check prints 0 before the old container stops, and the
  # listing and count after the restore print something other than what they printed against the
  # old container before the dump.
  lost_move() {
    local result="${temporary_dir}/move-$1"
    replay_move "$1"
    if [ "$(cat "${result}.rows")" != "$3" ] || [ "$(cat "${result}.trailer")" != 0 ] || [ ! -s "${result}.before" ] \
      || [ ! -s "${result}.after" ] || cmp -s "${result}.after" "${result}.before"; then
      printf 'FAIL: %s: the dump check prints 0, and the listing and count after the restore differ from what they printed before the dump; else the checks pass a move that lost every row\n' "$2"
      show_move "$result"
      failures=$((failures + 1))
    else
      printf 'PASS: %s: the dump check prints 0, and the listing and count after the restore differ from what they printed before the dump\n' "$2"
    fi
  }
  lost_move empty "a dump that failed, leaving an empty file" none
  lost_move cut "a dump that stopped once its schema was written" 0
fi

if [ "$failures" -ne 0 ]; then
  printf '%d case(s) failed\n' "$failures"
  exit 1
fi
