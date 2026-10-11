#!/usr/bin/env bash
# packages/envoy/scripts/dev-postgres.test.sh
#
# Proves dev-postgres.sh's reuse rule without Docker: a fake `docker` on PATH answers the inspects
# the script makes from the scenario its environment describes, and records every invocation, so
# each case asserts both what the script printed and every docker command it ran. A stale
# `dispatch-pg` (any image but the pinned pgvector one) is refused naming the container, both
# images and the commands that move its data volume onto the pinned image, and the script itself
# never stops, renames, removes, starts or creates anything for it. Its `quote`, which prints those
# commands, is held to fixtures/shell-quote.json, the table packages/dispatch's shellQuote test
# reads too, so the two copies of the rule cannot drift apart.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly script="${project_root}/packages/envoy/scripts/dev-postgres.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
trap 'rm -rf "$temporary_dir"' EXIT
mkdir -p "${temporary_dir}/bin"

readonly pinned="pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"
readonly pinned_id="sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"
readonly stale_id="sha256:eb4759788a2182f08257135e61a34f2cfc3c2914079f3465d64ee62350f4d081"
readonly url="DATABASE_URL=postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable"

# The fake docker. FAKE_CONTAINER=absent makes `container inspect` fail; otherwise the container
# runs image FAKE_IMAGE_ID (created as FAKE_CONFIG_IMAGE), is running when FAKE_RUNNING=true,
# carries PG_MAJOR=FAKE_PG_MAJOR, and mounts FAKE_MOUNT ("<type>|<name>|<source>", the fields the
# script's template prints) on the data directory. The pinned image is present when
# FAKE_PINNED_PRESENT=true.
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
      *".Config.Env"*) printf 'POSTGRES_PASSWORD=dispatch\nPG_MAJOR=%s\n\n' "$FAKE_PG_MAJOR" ;;
      *".Mounts"*) printf '%s\n' "$FAKE_MOUNT" ;;
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
    FAKE_CONTAINER=present FAKE_PINNED_PRESENT=true FAKE_RUNNING=true FAKE_PG_MAJOR=16 \
    FAKE_IMAGE_ID="$pinned_id" FAKE_CONFIG_IMAGE="$pinned" \
    FAKE_MOUNT="volume|6383303ba5ac|/var/lib/docker/volumes/6383303ba5ac/_data" \
    "$@" bash "$script" >"$out" 2>"$err" || status=$?
}
# Fails the case unless the script ran no docker command that changes anything.
read_only() {
  ! grep -qE '^(run|start|stop|rm|rename|create) ' "$calls"
}

run_case FAKE_CONTAINER=absent
if [ "$status" -ne 0 ] || [ "$(cat "$out")" != "$url" ] || ! grep -qxF "run -d --name dispatch-pg -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 $pinned" "$calls"; then
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

# The stale container this script's older version made: postgres:16 on an anonymous volume.
run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16
backup="dispatch-pg-${stale_id:7:12}"
for want in \
  "dispatch-pg runs postgres:16 (image $stale_id), not the pinned $pinned, so it has no pgvector" \
  "  docker stop dispatch-pg" \
  "  docker rename dispatch-pg $backup" \
  "  docker run -d --name dispatch-pg -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch -p 127.0.0.1:55432:5432 -v 6383303ba5ac:/var/lib/postgresql/data $pinned" \
  "  docker rm $backup"; do
  if ! grep -qF -- "$want" "$err"; then
    fail "a stale dispatch-pg: the refusal names \"$want\""
  fi
done
if [ "$status" -ne 1 ] || [ -s "$out" ] || ! read_only; then
  fail "a stale dispatch-pg: refused (exit 1, no URL, nothing changed)"
else
  printf 'PASS: a stale dispatch-pg: refused naming both images and the volume move, nothing changed\n'
fi

# The stale image when the pinned one has never been pulled here: still refused.
run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16 FAKE_PINNED_PRESENT=false
if [ "$status" -ne 1 ] || [ -s "$out" ] || ! read_only || ! grep -qF "not the pinned $pinned" "$err"; then
  fail "a stale dispatch-pg with no pinned image pulled: refused"
else
  printf 'PASS: a stale dispatch-pg with the pinned image never pulled: refused, nothing changed\n'
fi

run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16 FAKE_MOUNT="bind||/srv/dispatch-pg"
if [ "$status" -ne 1 ] || ! read_only || ! grep -qF -- "-v /srv/dispatch-pg:/var/lib/postgresql/data $pinned" "$err"; then
  fail "a stale dispatch-pg on a bind mount: the move mounts the host path"
else
  printf 'PASS: a stale dispatch-pg on a bind mount: the move mounts its host path\n'
fi

# A bind mount whose host path holds a space: the printed move pastes as one command, its -v
# argument one word. The `docker run` line is replayed through bash with `docker` as a function
# that prints its arguments one per line.
run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16 FAKE_MOUNT="bind||/srv/My Data"
printed_run="$(sed -n 's/^dev-postgres:   \(docker run .*\)$/\1/p' "$err")"
replayed="$(bash -c "docker() { printf '%s\n' \"\$@\"; }; $printed_run")"
if [ "$status" -ne 1 ] || ! read_only || [ -z "$printed_run" ] \
  || ! grep -qxF -- "/srv/My Data:/var/lib/postgresql/data" <<<"$replayed"; then
  fail "a stale dispatch-pg on a bind mount with a space: the printed move pastes whole"
  printf '  printed: %s\n  replayed: %s\n' "$printed_run" "$(tr '\n' '|' <<<"$replayed")"
else
  printf 'PASS: a stale dispatch-pg on a bind mount with a space: the printed move pastes as one command\n'
fi

run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:16 FAKE_MOUNT=""
if [ "$status" -ne 1 ] || ! read_only || ! grep -qF "is on no volume" "$err" || grep -qF "docker rename" "$err"; then
  fail "a stale dispatch-pg with no data volume: refused with no move"
else
  printf 'PASS: a stale dispatch-pg with no data volume: refused, offering a dump and no move\n'
fi

run_case FAKE_IMAGE_ID="$stale_id" FAKE_CONFIG_IMAGE=postgres:15 FAKE_PG_MAJOR=15
if [ "$status" -ne 1 ] || ! read_only || ! grep -qF "holds PostgreSQL 15" "$err" || grep -qF "docker rename" "$err"; then
  fail "a stale dispatch-pg on another PostgreSQL major: refused with no move"
else
  printf 'PASS: a stale dispatch-pg on PostgreSQL 15: refused, offering a dump and no move\n'
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

if [ "$failures" -ne 0 ]; then
  printf '%d case(s) failed\n' "$failures"
  exit 1
fi
