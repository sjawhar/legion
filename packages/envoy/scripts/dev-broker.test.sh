#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.test.sh
#
# Proves dev-broker.sh's per-instance database and port isolation without a real Postgres, real
# network binds, or a real broker binary: fakes `docker`, `go`, and `curl` on PATH the same way
# e2e-local.test.sh fakes `docker` — recording every docker/curl invocation to a call-log file and
# answering it without touching anything real — plus a fake `go build -o <path> ...` that installs
# a tiny stub script at <path> instead of compiling (dev-broker.sh's own isolation logic lives
# entirely in the shell driver, never in the Go binaries it starts, so a stub proves the driver's
# behavior without depending on cmd/agent-secrets-devkey or cmd/broker compiling or running for
# real). Runs the real driver twice, sequentially, and asserts from the call logs that each run (a)
# issues its own "create database dev_broker_<suffix>" call, never the literal "dispatch", (b) the
# two runs mint different database names, (c) each run's own EXIT-trap cleanup issues a matching
# "drop database" for exactly the name it created before the run's process ever exits, and (d) the
# two runs curl two different ports, never the old shared 13380 — the port collision that let one
# instance report "ready" while pointing at a different, already-running instance's broker.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-broker.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly docker_call_file="${temporary_dir}/docker-calls"
readonly curl_call_file="${temporary_dir}/curl-calls"
mkdir -p "$fake_bin_dir"
: >"$docker_call_file"
: >"$curl_call_file"

instance_pid=""
cleanup() {
  # Best-effort: if an assertion fails mid-test, don't leave a dev-broker.sh instance (and its
  # backgrounded fake broker) running.
  if [[ -n "$instance_pid" ]] && kill -0 "$instance_pid" 2>/dev/null; then
    kill -TERM "$instance_pid" 2>/dev/null || true
    wait "$instance_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake docker: reports dispatch-pg already running (skips the dev-postgres.sh boot path),
# answers every "exec ... pg_isready"/"exec ... psql -c ..." with success, and records the exact
# command line of every invocation. ---
cat >"${fake_bin_dir}/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_BROKER_TEST_DOCKER_CALL_FILE:?}"
if [[ "$1" == "ps" ]]; then
  printf 'dispatch-pg\n'
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/docker"

# --- Fake go: "go build -o <path> ./cmd/<pkg>" installs a stub executable at <path> instead of
# compiling. The devkey stub prints fixed but well-formed seed JSON; the broker stub answers
# -migrate-only immediately and otherwise just stays alive as the backgrounded "server" process
# dev-broker.sh waits on and kills during cleanup. ---
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
out=""
pkg=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == "-o" ]]; then
    out="$arg"
  fi
  pkg="$arg"
  prev="$arg"
done
case "$pkg" in
  */agent-secrets-devkey)
    cat >"$out" <<'DEVKEY'
#!/usr/bin/env bash
printf '%s\n' '{"credential_id":"fake-cred","aaguid":"00000000-0000-0000-0000-000000000000","challenge_nonce":"fake-nonce","registration":{"id":"fake"},"ca_pem":"-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"}'
DEVKEY
    ;;
  */broker)
    cat >"$out" <<'BROKER'
#!/usr/bin/env bash
if [[ "$1" == "-migrate-only" ]]; then
  echo "fake broker: migrated" >&2
  exit 0
fi
exec sleep 999999
BROKER
    ;;
  *)
    echo "fake go: unhandled build target $pkg" >&2
    exit 1
    ;;
esac
chmod +x "$out"
EOF
chmod +x "${fake_bin_dir}/go"

# --- Fake curl: dev-broker.sh only ever curls its own broker's /healthz; record the exact URL
# (which names the port dev-broker.sh picked for this instance) and answer success at once so the
# readiness loop never actually waits on or opens a socket. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_BROKER_TEST_CURL_CALL_FILE:?}"
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# Sets the global instance_pid (never wrapped in a subshell: the EXIT trap's safety-net kill must
# see the same variable this assignment updates).
run_instance() {
  local log_file="$1"
  DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_call_file" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_call_file" \
    PATH="${fake_bin_dir}:${PATH}" bash "$driver" >"$log_file" 2>&1 &
  instance_pid=$!
}

wait_for_ready() {
  local log_file="$1"
  for _ in $(seq 1 50); do
    grep -q 'dev-broker: ready\.' "$log_file" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

created_database() {
  # `|| true`: under this script's own set -o pipefail, grep -o finding nothing (the exact
  # regression this function exists to detect, e.g. dev-broker.sh reverted to the shared literal
  # "dispatch" database) exits 1, and since head/awk on empty input both exit 0, that 1 becomes
  # the whole pipeline's status. Assigning that failing command substitution to a variable
  # (`db="$(created_database)"` below) would trip this script's own errexit and abort before the
  # caller's own `[[ -z "$db" ]]` branch ever prints its FAIL diagnostic. Force success here so an
  # absent match reaches the caller as an empty string, which it already checks for explicitly.
  grep -o 'create database dev_broker_[a-z0-9]\+' "$docker_call_file" | head -n1 | awk '{print $3}' || true
}

used_port() {
  # See created_database's own comment: `|| true` keeps a no-match here from tripping this
  # script's own errexit before the caller's `[[ -z "$port" ]]` branch can print its diagnostic.
  grep -o 'http://127\.0\.0\.1:[0-9]\+/healthz' "$curl_call_file" | head -n1 | sed -E 's#.*:([0-9]+)/healthz#\1#' || true
}

# Sets the global "db"/"port" variables to the database and port this instance used; exits the
# whole test on any assertion failure.
db=""
port=""
assert_instance_isolated() {
  local label="$1" log_file="$2" other_db="${3:-}" other_port="${4:-}"
  run_instance "$log_file"
  if ! wait_for_ready "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label never reached ready" >&2
    exit 1
  fi
  db="$(created_database)"
  if [[ -z "$db" ]]; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label never issued a create database call" >&2
    exit 1
  fi
  if [[ "$db" == "dispatch" ]]; then
    echo "FAIL: $label created the shared dispatch database instead of an isolated one" >&2
    exit 1
  fi
  if [[ -n "$other_db" && "$db" == "$other_db" ]]; then
    echo "FAIL: $label minted the same database name as the previous instance ($db)" >&2
    exit 1
  fi

  port="$(used_port)"
  if [[ -z "$port" ]]; then
    cat "$curl_call_file" >&2
    echo "FAIL: $label never issued a healthz curl call" >&2
    exit 1
  fi
  if [[ "$port" == "13380" ]]; then
    echo "FAIL: $label used the old fixed shared port 13380 instead of a per-instance one" >&2
    exit 1
  fi
  if [[ -n "$other_port" && "$port" == "$other_port" ]]; then
    echo "FAIL: $label used the same port as the previous instance ($port)" >&2
    exit 1
  fi

  kill -TERM "$instance_pid"
  wait "$instance_pid" 2>/dev/null || true
  instance_pid=""

  if ! grep -qxF "exec dispatch-pg psql -U postgres -v ON_ERROR_STOP=1 -q -c drop database if exists ${db};" "$docker_call_file"; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label's cleanup never dropped its own database ($db)" >&2
    exit 1
  fi
  printf 'PASS: %s creates isolated database %s and drops exactly that database on exit\n' "$label" "$db"
}

log1="${temporary_dir}/instance1.log"
assert_instance_isolated 'instance 1' "$log1"
db1="$db"
port1="$port"

: >"$docker_call_file" # isolate instance 2's assertions from instance 1's own logged calls
: >"$curl_call_file"

log2="${temporary_dir}/instance2.log"
assert_instance_isolated 'instance 2' "$log2" "$db1" "$port1"
db2="$db"
port2="$port"

if [[ "$db1" == "$db2" ]]; then
  echo "FAIL: instance 1 ($db1) and instance 2 ($db2) minted the same database name" >&2
  exit 1
fi
printf 'PASS: two invocations of dev-broker.sh mint two different databases (%s, %s), never the shared "dispatch" database\n' "$db1" "$db2"

if [[ "$port1" == "$port2" ]]; then
  echo "FAIL: instance 1 (port $port1) and instance 2 (port $port2) used the same port" >&2
  exit 1
fi
printf 'PASS: two invocations of dev-broker.sh listen on two different ports (%s, %s), never the old shared 13380\n' "$port1" "$port2"
