#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.test.sh
#
# Proves dev-broker.sh's per-instance database and port isolation without a real Postgres, real
# network binds, or a real broker binary: fakes `docker`, `go`, and `curl` on PATH the same way
# e2e-local.test.sh fakes `docker` — recording every docker/curl invocation to a call-log file and
# answering it without touching anything real — plus a fake `go build -o <path> ...` that installs
# a tiny stub script at <path> instead of compiling (dev-broker.sh's own isolation logic lives
# entirely in the shell driver, never in the Go binaries it starts, so a stub proves the driver's
# behavior without depending on cmd/agent-secrets-devrelay or cmd/broker compiling or running for
# real). The broker stub logs "broker listening addr=127.0.0.1:<its own PID>" exactly like the
# real cmd/broker does once it binds (AGENTC-833) — using its own PID as a stand-in for the
# kernel-assigned port a real Listen would report, since two concurrently running stub processes
# always have distinct PIDs.
#
# Runs two instances of dev-broker.sh concurrently — the actual scenario
# BROKER_LISTEN_ADDR=127.0.0.1:0 exists to make safe — each with its own docker/curl call-log
# files, and asserts that: (a) each instance issues its own "create database dev_broker_<suffix>"
# call, never the literal "dispatch"; (b) the two instances mint different database names; (c)
# each instance's own EXIT-trap cleanup drops exactly the database it created, before its process
# ever exits; (d) each instance's exported AGENT_SECRETS_URL, and the address it curls for its own
# readiness check, both name the exact port its own broker logged as bound — never a different,
# stale, or script-precomputed value, the exact property that closes the AGENTC-833 race where a
# port collision let a losing instance report "ready" while a different, already-running
# instance's broker actually answered; and (e) the two concurrent instances bind two different
# ports.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-broker.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
mkdir -p "$fake_bin_dir"

declare -a instance_pid=()
cleanup() {
  # Best-effort: if an assertion fails mid-test, don't leave a dev-broker.sh instance (and its
  # backgrounded fake broker) running.
  for pid in "${instance_pid[@]}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake docker: reports dispatch-pg already running (skips the dev-postgres.sh boot path),
# answers every "exec ... pg_isready"/"exec ... psql -c ..." with success, and records the exact
# command line of every invocation to the caller's own DEV_BROKER_TEST_DOCKER_CALL_FILE (each
# instance below gets its own, so two concurrent instances' calls never interleave in one file).
# ---
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
# compiling. The client stubs are never run by dev-broker.sh (it only prints their directory); the
# broker stub logs a "broker listening addr=..." line exactly like the real cmd/broker does once
# it binds (AGENTC-833) — using its own PID as a stand-in for the kernel-assigned port a real
# Listen would report, since two concurrently running stub processes always have distinct PIDs —
# and then stays alive as the backgrounded "server" process dev-broker.sh waits on and kills
# during cleanup. ---
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
  */agent-secrets|*/agent-secrets-helper|*/agent-secrets-devrelay)
    cat >"$out" <<'DEVRELAY'
#!/usr/bin/env bash
exit 0
DEVRELAY
    ;;
  */broker)
    cat >"$out" <<'BROKER'
#!/usr/bin/env bash
if [[ $# -ne 0 ]]; then
  echo "fake broker: unexpected arguments: $*" >&2
  exit 2
fi
echo "broker listening addr=127.0.0.1:$$" >&2
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
# (which names the port dev-broker.sh read from its own broker's log) to the caller's own
# DEV_BROKER_TEST_CURL_CALL_FILE and answer success at once, so the readiness loop never actually
# waits on or opens a socket. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_BROKER_TEST_CURL_CALL_FILE:?}"
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# Sets the global instance_pid array (never wrapped in a subshell: the EXIT trap's safety-net
# kill must see the same variable this assignment updates).
run_instance() {
  local which="$1" log_file="$2" docker_call_file="$3" curl_call_file="$4"
  : >"$docker_call_file"
  : >"$curl_call_file"
  PATH="${fake_bin_dir}:${PATH}" \
    DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_call_file" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_call_file" \
    bash "$driver" >"$log_file" 2>&1 &
  instance_pid[which]=$!
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
  local docker_call_file="$1"
  # `|| true`: under this script's own set -o pipefail, grep -o finding nothing (the exact
  # regression this function exists to detect, e.g. dev-broker.sh reverted to the shared literal
  # "dispatch" database) exits 1, and since head/awk on empty input both exit 0, that 1 becomes
  # the whole pipeline's status. Force success here so an absent match reaches the caller as an
  # empty string, which it already checks for explicitly.
  grep -o 'create database dev_broker_[a-z0-9]\+' "$docker_call_file" | head -n1 | awk '{print $3}' || true
}

logged_port() {
  # The fake broker's own "broker listening addr=127.0.0.1:<port>" line, landing in the
  # instance's combined stdout/stderr log exactly as dev-broker.sh's own tee writes it.
  local log_file="$1"
  grep -o 'addr=127\.0\.0\.1:[0-9]\+' "$log_file" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true
}

curled_port() {
  # See created_database's own comment: `|| true` keeps a no-match here from tripping this
  # script's own errexit before the caller's own emptiness check can print its diagnostic.
  local curl_call_file="$1"
  grep -o 'http://127\.0\.0\.1:[0-9]\+/healthz' "$curl_call_file" | head -n1 | sed -E 's#.*:([0-9]+)/healthz#\1#' || true
}

exported_port() {
  # dev-broker.sh's own printed "export AGENT_SECRETS_URL=http://127.0.0.1:<port>" line: the
  # value a human or agent-secrets would actually be told to use.
  local log_file="$1"
  grep -o 'AGENT_SECRETS_URL=http://127\.0\.0\.1:[0-9]\+' "$log_file" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true
}

# Sets the global "db"/"port" variables to the database and port this instance used; exits the
# whole test on any assertion failure.
db=""
port=""
assert_instance_isolated() {
  local n="$1" label="$2" log_file="$3" docker_call_file="$4" curl_call_file="$5"
  if ! wait_for_ready "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label never reached ready" >&2
    exit 1
  fi

  db="$(created_database "$docker_call_file")"
  if [[ -z "$db" ]]; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label never issued a create database call" >&2
    exit 1
  fi
  if [[ "$db" == "dispatch" ]]; then
    echo "FAIL: $label created the shared dispatch database instead of an isolated one" >&2
    exit 1
  fi
  printf 'PASS: %s creates isolated database %s\n' "$label" "$db"

  local bound curled exported
  bound="$(logged_port "$log_file")"
  if [[ -z "$bound" ]]; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker never logged its bound address" >&2
    exit 1
  fi
  curled="$(curled_port "$curl_call_file")"
  if [[ -z "$curled" ]]; then
    cat "$curl_call_file" >&2
    echo "FAIL: $label never issued a healthz curl call" >&2
    exit 1
  fi
  if [[ "$curled" != "$bound" ]]; then
    echo "FAIL: $label curled port $curled for readiness, but its own broker logged binding $bound" >&2
    exit 1
  fi
  exported="$(exported_port "$log_file")"
  if [[ -z "$exported" ]]; then
    cat "$log_file" >&2
    echo "FAIL: $label never printed an AGENT_SECRETS_URL export" >&2
    exit 1
  fi
  if [[ "$exported" != "$bound" ]]; then
    echo "FAIL: $label exported port $exported, but its own broker logged binding $bound" >&2
    exit 1
  fi
  printf 'PASS: %s exports the exact port its own broker logged as bound (%s)\n' "$label" "$bound"
  port="$bound"

  local pid="${instance_pid[$n]}"
  kill -TERM "$pid"
  wait "$pid" 2>/dev/null || true
  instance_pid[n]=""

  if ! grep -qxF "exec dispatch-pg psql -U postgres -v ON_ERROR_STOP=1 -q -c drop database if exists ${db};" "$docker_call_file"; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label's cleanup never dropped its own database ($db)" >&2
    exit 1
  fi
  printf 'PASS: %s drops exactly the database it created on exit\n' "$label"
}

log1="${temporary_dir}/instance1.log"
log2="${temporary_dir}/instance2.log"
docker_calls1="${temporary_dir}/docker-calls-1"
docker_calls2="${temporary_dir}/docker-calls-2"
curl_calls1="${temporary_dir}/curl-calls-1"
curl_calls2="${temporary_dir}/curl-calls-2"

# Both instances start before either is waited on: this is the actual scenario
# BROKER_LISTEN_ADDR=127.0.0.1:0 exists to make safe (AGENTC-833), not two sequential runs that
# never actually contend for anything.
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
run_instance 2 "$log2" "$docker_calls2" "$curl_calls2"

assert_instance_isolated 1 'instance 1' "$log1" "$docker_calls1" "$curl_calls1"
db1="$db"
port1="$port"

assert_instance_isolated 2 'instance 2' "$log2" "$docker_calls2" "$curl_calls2"
db2="$db"
port2="$port"

if [[ "$db1" == "$db2" ]]; then
  echo "FAIL: instance 1 ($db1) and instance 2 ($db2) minted the same database name" >&2
  exit 1
fi
printf 'PASS: two concurrent instances of dev-broker.sh mint two different databases (%s, %s), never the shared "dispatch" database\n' "$db1" "$db2"

if [[ "$port1" == "$port2" ]]; then
  echo "FAIL: instance 1 (port $port1) and instance 2 (port $port2) bound the same port" >&2
  exit 1
fi
printf 'PASS: two concurrent instances of dev-broker.sh bind two different ports (%s, %s)\n' "$port1" "$port2"
