#!/usr/bin/env bash
# packages/envoy/scripts/dev-stack.test.sh
#
# Proves dev-stack.sh's per-run isolation, its port reads and its stop, without a real Postgres,
# real network binds or a real server binary: it puts a fake `go`, `psql`, `curl` and `python3` on
# PATH — each recording what it was asked for to the instance's own call file and answering it
# without touching anything real — and the fake `go build -o <dir>/ ./cmd/<pkg> ...` installs a
# stub at <dir>/<pkg> instead of compiling. dev-stack.sh's own isolation, port and stop logic
# lives entirely in the shell driver and never in the Go binaries it starts, so a stub proves the
# driver's behaviour without depending on cmd/dispatch, cmd/broker or cmd/listener compiling.
#
# Each stub logs the line the real process logs once it is listening — the issuer's
# "issuer listening url=", the broker's "broker listening addr=", natsd's "Listening for client
# connections on" and "Server is ready", the listener's JSON "envoy-listener listening" and
# Dispatch's msg="dispatch: listening" addr= — with its own PID standing in for the port a real
# Listen would report, since two concurrently running stubs always have distinct PIDs, and then
# stays alive until it is signalled, as the server it stands in for does. So a test that reads a
# port out of dev-stack.sh's banner and finds the stub's PID has proved that dev-stack.sh took
# that port from the process's own report and from nowhere else.
#
# It runs two instances concurrently — the scenario a port and a database of each run's own exist
# to make safe — and asserts that: (a) every process of a run is started and the port the banner
# names for it is the one that process reported; (b) the Dispatch port keeper's held port is what
# the broker's machine-login URL and Dispatch's own server URL name, which is why a keeper is
# there at all; (c) "dev-stack: ready." is printed exactly once, with the export block after it;
# (d) the two instances mint different database names, different ports and different work
# directories, and neither names the other's; (e) the one trap stops every process it started and
# drops exactly the two databases it created, on a normal exit, on INT, and on a failure part way
# through, which leaves nothing running; and (f) a run given no database URL refuses with exit 2,
# naming all three sources.
set -euo pipefail
# Monitor mode, for the INT case alone: a background command of a shell without job control
# inherits SIGINT ignored, and a shell cannot trap a signal that was ignored when it started, so
# an instance started with & from this script could not run its own INT trap at all. With -m each
# instance is a process group of its own with the default dispositions, so the INT this test
# sends reaches dev-stack.sh's trap exactly as a terminal's Ctrl-C does.
set -m

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-stack.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/dev-stack-test.XXXXXX")"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
mkdir -p "$fake_bin_dir"

# The Postgres server every instance below is told to use. Nothing connects to it: the fake psql
# answers every call.
readonly admin_url='postgres://tester:s3cret@127.0.0.1:5999/postgres?sslmode=disable'

declare -a instance_pid=()
cleanup() {
  # Best-effort: if an assertion fails mid-test, leave no dev-stack.sh instance, and no stub it
  # started, running.
  local pid
  for pid in "${instance_pid[@]}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  pkill -f "${temporary_dir}/dev-stack\." 2>/dev/null || true
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake psql: records every call to the instance's own DEV_STACK_TEST_PSQL_CALL_FILE (each
# instance gets its own, so two concurrent instances' calls never interleave in one file), and
# answers `select current_database()` with the database the URL in its first argument connects to,
# as libpq does: a dbname= in the query string wins over the URL's path. ---
cat >"${fake_bin_dir}/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"${DEV_STACK_TEST_PSQL_CALL_FILE:?}"
if [[ "$*" == *"select current_database()"* ]]; then
  url=$1 query=""
  [[ "$url" == *\?* ]] && query=${url#*\?}
  if [[ "&$query" =~ \&dbname=([^&]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
  else
    path=${url%%\?*}
    printf '%s\n' "${path##*/}"
  fi
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/psql"

# --- Fake curl: dev-stack.sh curls its own broker's health and pod-session route, its own
# listener's health and its own Dispatch's route index; record the exact command line (which names
# the port dev-stack.sh read from each process's own log) to the instance's own
# DEV_STACK_TEST_CURL_CALL_FILE and answer success at once, so no readiness loop ever opens a
# socket. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_STACK_TEST_CURL_CALL_FILE:?}"
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# --- Fake python3, which dev-stack.sh runs only as scripts/e2e/lib/held-port.py: `keep` writes
# its own PID to the port file as the port it holds (two concurrent keepers have distinct PIDs, as
# two kernel-chosen ports are distinct) and stays alive, writing that file last as the real keeper
# does, which writes it only once it is serving; `exec` drops its own options and executes the
# command after `--`, as the real one does once it has put the held socket on descriptor 3. No
# socket is opened either way. ---
cat >"${fake_bin_dir}/python3" <<'EOF'
#!/usr/bin/env bash
script="${1:-}"
case "$script" in
  */held-port.py) shift ;;
  *)
    echo "fake python3: unexpected script $script" >&2
    exit 2
    ;;
esac
mode="${1:-}"
shift || true
case "$mode" in
  keep)
    port_file=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --port-file)
          port_file="$2"
          shift 2
          ;;
        --socket | --host)
          shift 2
          ;;
        *)
          echo "fake held-port keep: unexpected argument $1" >&2
          exit 2
          ;;
      esac
    done
    [[ -n "$port_file" ]] || exit 2
    sleeper=""
    trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM INT
    printf '%s\n' "$$" >"$port_file"
    sleep 999999 &
    sleeper=$!
    wait "$sleeper"
    ;;
  exec)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --)
          shift
          break
          ;;
        --from | --host | --port-file | --fd-env | --env)
          shift 2
          ;;
        *)
          echo "fake held-port exec: unexpected argument $1" >&2
          exit 2
          ;;
      esac
    done
    [[ $# -gt 0 ]] || exit 2
    exec "$@"
    ;;
  *)
    echo "fake held-port: unexpected mode $mode" >&2
    exit 2
    ;;
esac
EOF
chmod +x "${fake_bin_dir}/python3"

# --- Fake go: "go build -o <dir>/ ./cmd/<pkg> ..." installs one stub per package instead of
# compiling. Every server stub reports its own PID as its port and stays alive until it is
# signalled, exiting 0 on SIGTERM as a server stopped by its supervisor does, so a stub the test
# stops drives dev-stack.sh's normal exit. With DEV_STACK_TEST_BROKER_FAILS set at build time the
# broker stub instead exits 1 without reporting an address, which is a failure part way through a
# start; it is baked in rather than read from the environment because dev-stack.sh starts every
# process with env -i, so nothing of this test's environment reaches one. The client stubs
# (agent-secrets, agent-secrets-helper) are never run by dev-stack.sh, which only puts their
# directory on PATH. ---
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
out=""
prev=""
packages=()
for arg in "$@"; do
  if [[ "$prev" == "-o" ]]; then
    out="$arg"
  fi
  case "$arg" in ./*) packages+=("$arg") ;; esac
  prev="$arg"
done
if [[ -z "$out" || ${#packages[@]} -eq 0 ]]; then
  echo "fake go: want build -o <dir>/ ./pkg..." >&2
  exit 1
fi
mkdir -p "$out"
for package in "${packages[@]}"; do
  name="${package##*/}"
  target="${out%/}/$name"
  case "$name" in
    dev-oidc-issuer)
      cat >"$target" <<'ISSUER'
#!/usr/bin/env bash
code_clients="" device_clients="" sign_in_as=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token)
      account_and_file="${2#*=}"
      printf 'fake-token-for-%s\n' "${account_and_file%%=*}" >"${account_and_file#*=}"
      ;;
    --code-flow) code_clients="$code_clients ${2%%=*}:$(stat -c %a "${2#*=}")" ;;
    --device-flow) device_clients="$device_clients ${2%%=*}:$(stat -c %a "${2#*=}")" ;;
    --sign-in-as) sign_in_as="$2" ;;
    *)
      echo "fake dev-oidc-issuer: unexpected argument $1" >&2
      exit 2
      ;;
  esac
  shift 2 || exit 2
done
echo "fake issuer: code-flow$code_clients device-flow$device_clients sign-in-as=$sign_in_as" >&2
echo "dev-oidc-issuer: issuer listening url=http://127.0.0.1:$$" >&2
ISSUER
      ;;
    broker)
      cat >"$target" <<'BROKER'
#!/usr/bin/env bash
echo "fake broker: database=${BROKER_DATABASE_URL:-} issuer=${BROKER_K8S_OIDC_ISSUER:-} callers=${BROKER_API_ACCOUNTS:-}" >&2
echo "fake broker: machine-login=${BROKER_MACHINE_LOGIN_URL:-} sign-in-client=${BROKER_SIGNIN_CLIENT_ID:-} exchange=${BROKER_EXCHANGE_ACCOUNTS:-}" >&2
BROKER
      if [[ -n "${DEV_STACK_TEST_BROKER_FAILS:-}" ]]; then
        cat >>"$target" <<'REFUSES'
echo "fake broker: refusing to start" >&2
exit 1
REFUSES
      else
        cat >>"$target" <<'LISTENS'
echo "broker listening addr=127.0.0.1:$$" >&2
LISTENS
      fi
      ;;
    natsd)
      cat >"$target" <<'NATSD'
#!/usr/bin/env bash
echo "[INF] Starting nats-server" >&2
echo "[INF] Listening for client connections on 127.0.0.1:$$" >&2
echo "[INF] Server is ready" >&2
NATSD
      ;;
    listener)
      cat >"$target" <<'LISTENER'
#!/usr/bin/env bash
printf '{"level":"INFO","msg":"listener API auth: none","nats":"%s"}\n' "${NATS_URLS:-}" >&2
printf '{"level":"INFO","msg":"envoy-listener listening","addr":"127.0.0.1:%s"}\n' "$$" >&2
LISTENER
      ;;
    dispatch)
      cat >"$target" <<'DISPATCH'
#!/usr/bin/env bash
echo "fake dispatch: database=${DATABASE_URL:-} server-url=${DISPATCH_SERVER_URL:-} listen-fd=${DISPATCH_LISTEN_FD:-} envoy=${ENVOY_URL:-} broker=${DISPATCH_AGENT_SECRETS_URL:-} nats=${NATS_URLS:-} home=${HOME:-}" >&2
echo "time=2026-01-01T00:00:00.000Z level=INFO msg=\"dispatch: listening\" addr=127.0.0.1:$$" >&2
DISPATCH
      ;;
    agent-secrets | agent-secrets-helper)
      cat >"$target" <<'CLIENT'
#!/usr/bin/env bash
exit 0
CLIENT
      chmod +x "$target"
      continue
      ;;
    *)
      echo "fake go: unhandled build target $package" >&2
      exit 1
      ;;
  esac
  # Every server stub ends the same way: stay alive as the backgrounded process dev-stack.sh waits
  # on, and end at 0 when it is stopped, killing the sleep so nothing of the stub outlives it.
  cat >>"$target" <<'IDLE'
sleeper=""
trap 'kill "$sleeper" 2>/dev/null; exit 0' TERM INT
sleep 999999 &
sleeper=$!
wait "$sleeper"
IDLE
  chmod +x "$target"
done
EOF
chmod +x "${fake_bin_dir}/go"

# Sets the global instance_pid array (never wrapped in a subshell: the EXIT trap's safety-net kill
# must see the same variable this assignment updates). The log and the call files are emptied
# here, before the fork, not only by the background job's own redirect: a reused log still holds
# the previous run's "dev-stack: ready." line until that job runs, and wait_for_ready would read
# it. TMPDIR is this test's own directory, so each run's work directory lives under it and this
# test's cleanup removes both. The run is given no LEGION_TEST_PG_DSN and no DATABASE_URL, so
# nothing of the machine it runs on decides which server it is pointed at.
run_instance() {
  local which="$1" log_file="$2" psql_call_file="$3" curl_call_file="$4"
  : >"$log_file"
  : >"$psql_call_file"
  : >"$curl_call_file"
  env -u LEGION_TEST_PG_DSN -u DATABASE_URL \
    PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" \
    DEV_STACK_TEST_PSQL_CALL_FILE="$psql_call_file" DEV_STACK_TEST_CURL_CALL_FILE="$curl_call_file" \
    DEV_STACK_TEST_BROKER_FAILS="${DEV_STACK_TEST_BROKER_FAILS:-}" \
    bash "$driver" --database-url "$admin_url" >"$log_file" 2>&1 &
  instance_pid[which]=$!
}

wait_for_ready() {
  local log_file="$1"
  for _ in $(seq 1 150); do
    grep -q 'dev-stack: ready\.' "$log_file" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# Waits up to 15 s for instance <n> to exit by itself and sets the global "exited_status" to its
# exit status. A run meant to stop that is still running then went on serving: it is killed and
# "exited_status" stays 0, which each caller reads as the failure, so the wait is bounded and
# reports that case rather than hanging.
exited_status=0
wait_for_exit() {
  local n="$1"
  local pid="${instance_pid[$n]}"
  exited_status=0
  for _ in $(seq 1 150); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  else
    wait "$pid" || exited_status=$?
  fi
  instance_pid[n]=""
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Each reader below ends in `|| true`: under this script's own pipefail a grep that matches
# nothing — the exact regression each caller checks for — exits 1, and head, awk and sed on empty
# input exit 0, so that 1 would become the pipeline's status and trip errexit before the caller's
# own emptiness check could print its diagnostic.
work_dir_of() { grep -o 'dev-stack: workdir .*' "$1" | head -n1 | awk '{print $3}' || true; }
created_databases() { grep -o 'create database dev_stack_[a-z0-9_]*' "$1" | awk '{print $3}' | sort | tr '\n' ' ' || true; }
dropped_databases() { grep -o 'drop database if exists dev_stack_[a-z0-9_]*' "$1" | awk '{print $5}' | sort | tr '\n' ' ' || true; }
# The port dev-stack.sh printed for one process, from its own "dev-stack: <name> <url>" line.
banner_port_of() { grep -o "dev-stack: $2 [a-z]*://127\.0\.0\.1:[0-9]*" "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
# The port a process itself reported, from the log file dev-stack.sh gave that process alone.
issuer_log_port() { grep -o 'issuer listening url=http://127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
broker_log_port() { grep -o 'broker listening addr=127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
nats_log_port() { grep -o 'Listening for client connections on 127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
listener_log_port() { grep -o '"addr":"127\.0\.0\.1:[0-9]*"' "$1" | head -n1 | sed -E 's/.*:([0-9]+)"$/\1/' || true; }
dispatch_log_port() { grep -o 'msg="dispatch: listening" addr=127\.0\.0\.1:[0-9]*' "$1" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true; }
# The value of one "export NAME=..." line of the banner.
exported() { grep -o "export $2=.*" "$1" | head -n1 | cut -d= -f2- || true; }

# The globals assert_instance_ready sets for the caller: the run's work directory, the suffix its
# names are made from, the ports every process of it reported, and the port its Dispatch keeper
# held.
work_dir=""
run_suffix=""
kept_port=""
declare -A process_port=()
assert_instance_ready() {
  local label="$1" log_file="$2" psql_call_file="$3" curl_call_file="$4"
  if ! wait_for_ready "$log_file"; then
    cat "$log_file" >&2
    fail "$label never reached ready"
  fi
  work_dir="$(work_dir_of "$log_file")"
  [[ -n "$work_dir" && -d "$work_dir" ]] || fail "$label named no work directory"
  run_suffix="$(printf '%s' "${work_dir##*.}" | tr '[:upper:]' '[:lower:]')"
  process_port=()

  # (a) Every process is started, and the port dev-stack.sh printed for it is the port that
  # process itself reported in its own log, never one the driver chose.
  local entry name reader port banner
  for entry in "dev-oidc-issuer:issuer_log_port:dev-oidc-issuer.log" \
    "broker:broker_log_port:broker.log" \
    "natsd:nats_log_port:natsd.log" \
    "listener:listener_log_port:listener.log" \
    "dispatch:dispatch_log_port:dispatch.log"; do
    name="${entry%%:*}"
    reader="${entry#*:}"
    reader="${reader%%:*}"
    port="$("$reader" "${work_dir}/logs/${entry##*:}")"
    [[ -n "$port" ]] || fail "$label: $name reported no port in its own log"
    kill -0 "$port" 2>/dev/null || fail "$label: $name's reported port $port is no live process of this run"
    banner="$(banner_port_of "$log_file" "$name")"
    [[ "$banner" == "$port" ]] || fail "$label: the banner names port '$banner' for $name, which reported $port"
    process_port["$name"]="$port"
  done

  # (b) The keeper's held port is the one the broker was told to send a machine login to and the
  # one Dispatch serves under: the reason a keeper holds Dispatch's port before either starts.
  kept_port="$(cat "${work_dir}/dispatch-port")"
  [[ -n "$kept_port" ]] || fail "$label: the Dispatch port keeper wrote no port"
  grep -q "machine-login=http://127\.0\.0\.1:${kept_port}/credentials/machine" "${work_dir}/logs/broker.log" ||
    fail "$label: the broker was not told Dispatch's machine-login page at the kept port $kept_port"
  grep -q "server-url=http://127\.0\.0\.1:${kept_port} " "${work_dir}/logs/dispatch.log" ||
    fail "$label: Dispatch's server URL does not name the kept port $kept_port"
  grep -q 'listen-fd=3' "${work_dir}/logs/dispatch.log" ||
    fail "$label: Dispatch was not given the keeper's socket (DISPATCH_LISTEN_FD)"

  # (c) One "ready." line, and the exports after it, each naming the port its own process reported.
  local ready_lines
  ready_lines="$(grep -c 'dev-stack: ready\.' "$log_file" || true)"
  [[ "$ready_lines" -eq 1 ]] || fail "$label printed 'dev-stack: ready.' $ready_lines times"
  awk '/dev-stack: ready\./ { seen = 1 } /^  export AGENT_SECRETS_URL=/ { if (!seen) exit 1 }' "$log_file" ||
    fail "$label printed its exports before it was ready"
  local exported_value
  exported_value="$(exported "$log_file" AGENT_SECRETS_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[broker]}" ]] ||
    fail "$label exported AGENT_SECRETS_URL=$exported_value, not the broker's own port"
  exported_value="$(exported "$log_file" DISPATCH_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[dispatch]}" ]] ||
    fail "$label exported DISPATCH_URL=$exported_value, not Dispatch's own port"
  exported_value="$(exported "$log_file" ENVOY_URL)"
  [[ "$exported_value" == "http://127.0.0.1:${process_port[listener]}" ]] ||
    fail "$label exported ENVOY_URL=$exported_value, not the listener's own port"
  exported_value="$(exported "$log_file" NATS_URLS)"
  [[ "$exported_value" == "nats://127.0.0.1:${process_port[natsd]}" ]] ||
    fail "$label exported NATS_URLS=$exported_value, not natsd's own port"
  exported_value="$(exported "$log_file" PATH)"
  [[ "$exported_value" == "${work_dir}/bin:\$PATH" ]] ||
    fail "$label exported PATH=$exported_value, not this run's binaries first"
  exported_value="$(exported "$log_file" AGENT_SECRETS_ACCOUNT_TOKEN_FILE)"
  [[ -s "$exported_value" ]] ||
    fail "$label exported AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$exported_value, which its issuer never wrote"

  # Each readiness check this run made named its own process's port, so no check of it could have
  # been answered by another run's server.
  grep -qxF -e "-sf http://127.0.0.1:${process_port[broker]}/healthz" "$curl_call_file" ||
    fail "$label did not check its own broker's health at port ${process_port[broker]}"
  grep -qxF -e "-sf http://127.0.0.1:${process_port[listener]}/healthz" "$curl_call_file" ||
    fail "$label did not check its own listener's health at port ${process_port[listener]}"
  grep -qxF -e "-sf http://127.0.0.1:${process_port[dispatch]}/api/v1" "$curl_call_file" ||
    fail "$label did not read its own Dispatch's route index at port ${process_port[dispatch]}"

  # Two databases of this run's own, named for it, and no other; each server pointed at its own.
  local created
  created="$(created_databases "$psql_call_file")"
  [[ "$created" == "dev_stack_broker_${run_suffix} dev_stack_dispatch_${run_suffix} " ]] ||
    fail "$label created '$created', not one broker and one Dispatch database of its own"
  grep -q "database=postgres://tester:s3cret@127.0.0.1:5999/dev_stack_broker_${run_suffix}" "${work_dir}/logs/broker.log" ||
    fail "$label did not point its broker at its own database"
  grep -q "database=postgres://tester:s3cret@127.0.0.1:5999/dev_stack_dispatch_${run_suffix}" "${work_dir}/logs/dispatch.log" ||
    fail "$label did not point Dispatch at its own database"
}

# Every process of the run is gone, by the PID each reported as its port, and the keeper with them.
assert_everything_stopped() {
  local label="$1" pid
  for pid in "${process_port[@]}" "$kept_port"; do
    [[ -n "$pid" ]] || continue
    if kill -0 "$pid" 2>/dev/null; then
      fail "$label left process $pid running"
    fi
  done
}

assert_databases_dropped() {
  local label="$1" psql_call_file="$2" suffix="$3" dropped
  dropped="$(dropped_databases "$psql_call_file")"
  [[ "$dropped" == "dev_stack_broker_${suffix} dev_stack_dispatch_${suffix} " ]] ||
    fail "$label dropped '$dropped', not exactly the two databases it created"
}

log1="${temporary_dir}/instance1.log"
log2="${temporary_dir}/instance2.log"
psql_calls1="${temporary_dir}/psql-calls-1"
psql_calls2="${temporary_dir}/psql-calls-2"
curl_calls1="${temporary_dir}/curl-calls-1"
curl_calls2="${temporary_dir}/curl-calls-2"

# Both instances start before either is waited on: this is the scenario a port and a database of
# each run's own exist to make safe, not two sequential runs that never contend for anything.
run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
run_instance 2 "$log2" "$psql_calls2" "$curl_calls2"

assert_instance_ready 'instance 1' "$log1" "$psql_calls1" "$curl_calls1"
work_dir1="$work_dir"
suffix1="$run_suffix"
kept_port1="$kept_port"
declare -A port1=()
for name in "${!process_port[@]}"; do port1["$name"]="${process_port[$name]}"; done
printf "PASS: instance 1 started every process and took each port from that process's own report (issuer %s, broker %s, natsd %s, listener %s, dispatch %s; Dispatch keeper %s)\n" \
  "${port1[dev-oidc-issuer]}" "${port1[broker]}" "${port1[natsd]}" "${port1[listener]}" "${port1[dispatch]}" "$kept_port1"

assert_instance_ready 'instance 2' "$log2" "$psql_calls2" "$curl_calls2"
work_dir2="$work_dir"
suffix2="$run_suffix"
kept_port2="$kept_port"
declare -A port2=()
for name in "${!process_port[@]}"; do port2["$name"]="${process_port[$name]}"; done
printf "PASS: instance 2 started every process and took each port from that process's own report (issuer %s, broker %s, natsd %s, listener %s, dispatch %s; Dispatch keeper %s)\n" \
  "${port2[dev-oidc-issuer]}" "${port2[broker]}" "${port2[natsd]}" "${port2[listener]}" "${port2[dispatch]}" "$kept_port2"

# (d) Two concurrent runs share nothing: not a work directory, not a port, not a database.
[[ "$work_dir1" != "$work_dir2" ]] || fail "both instances used the work directory $work_dir1"
if grep -q "$work_dir2" "$log1"; then
  fail "instance 1 named instance 2's work directory"
fi
if grep -q "$work_dir1" "$log2"; then
  fail "instance 2 named instance 1's work directory"
fi
for name in dev-oidc-issuer broker natsd listener dispatch; do
  [[ "${port1[$name]}" != "${port2[$name]}" ]] ||
    fail "both instances' $name reported port ${port1[$name]}"
done
[[ "$kept_port1" != "$kept_port2" ]] || fail "both instances held the Dispatch port $kept_port1"
[[ "$suffix1" != "$suffix2" ]] || fail "both instances named their databases dev_stack_*_${suffix1}"
printf 'PASS: two concurrent instances used two work directories, two sets of ports and two sets of databases (%s| %s)\n' \
  "$(created_databases "$psql_calls1")" "$(created_databases "$psql_calls2")"

# (e1) A normal exit: one process of the stack stops by itself, so the run stops the rest, drops
# both of its databases and exits with that process's status.
kill -TERM "${port1[natsd]}"
wait_for_exit 1
[[ "$exited_status" -eq 0 ]] || fail "instance 1 exited $exited_status after a process of it stopped cleanly"
grep -q 'dev-stack: a process of this stack stopped (status 0)' "$log1" ||
  fail "instance 1 did not report the process that stopped"
process_port=()
for name in "${!port1[@]}"; do process_port["$name"]="${port1[$name]}"; done
kept_port="$kept_port1"
assert_everything_stopped 'instance 1'
assert_databases_dropped 'instance 1' "$psql_calls1" "$suffix1"
printf 'PASS: a normal exit stopped every process instance 1 started and dropped both its databases\n'

# (e2) An interrupt: the same stop, with 130, the status a run interrupted by Ctrl-C reports.
kill -INT "${instance_pid[2]}"
wait_for_exit 2
[[ "$exited_status" -eq 130 ]] || fail "instance 2 exited $exited_status on INT, not 130"
process_port=()
for name in "${!port2[@]}"; do process_port["$name"]="${port2[$name]}"; done
kept_port="$kept_port2"
assert_everything_stopped 'instance 2'
assert_databases_dropped 'instance 2' "$psql_calls2" "$suffix2"
printf 'PASS: INT stopped every process instance 2 started, dropped both its databases and exited 130\n'

# (e3) A failure part way through, before ready: the broker refuses to start, so the run fails,
# and the processes it had already started are stopped and its databases dropped all the same.
DEV_STACK_TEST_BROKER_FAILS=1 run_instance 1 "$log1" "$psql_calls1" "$curl_calls1"
wait_for_exit 1
[[ "$exited_status" -ne 0 ]] || fail "a run whose broker refused to start reported success"
if grep -q 'dev-stack: ready\.' "$log1"; then
  fail "a run whose broker refused to start reported ready"
fi
failed_work_dir="$(work_dir_of "$log1")"
[[ -n "$failed_work_dir" ]] || fail "the failed run named no work directory"
process_port=()
process_port[dev-oidc-issuer]="$(issuer_log_port "${failed_work_dir}/logs/dev-oidc-issuer.log")"
[[ -n "${process_port[dev-oidc-issuer]}" ]] || fail "the failed run started no issuer to stop"
kept_port="$(cat "${failed_work_dir}/dispatch-port")"
assert_everything_stopped 'the failed run'
assert_databases_dropped 'the failed run' "$psql_calls1" "$(printf '%s' "${failed_work_dir##*.}" | tr '[:upper:]' '[:lower:]')"
for name in natsd listener dispatch; do
  [[ ! -s "${failed_work_dir}/logs/${name}.log" ]] ||
    fail "the failed run started $name after its broker had refused"
done
printf 'PASS: a broker that refuses to start leaves nothing running: the issuer and the keeper are stopped and both databases dropped\n'

# (f) No database URL at all: the refusal names all three sources and exits 2, before anything is
# created.
refusal="${temporary_dir}/refusal.log"
refusal_status=0
env -u LEGION_TEST_PG_DSN -u DATABASE_URL PATH="${fake_bin_dir}:${PATH}" TMPDIR="$temporary_dir" \
  DEV_STACK_TEST_PSQL_CALL_FILE="${temporary_dir}/psql-calls-refusal" \
  DEV_STACK_TEST_CURL_CALL_FILE="${temporary_dir}/curl-calls-refusal" \
  bash "$driver" >"$refusal" 2>&1 || refusal_status=$?
[[ "$refusal_status" -eq 2 ]] || fail "a run with no database URL exited $refusal_status, not 2"
for source in --database-url LEGION_TEST_PG_DSN DATABASE_URL scripts/dev-postgres.sh; do
  grep -q -- "$source" "$refusal" || fail "the refusal does not name $source"
done
[[ ! -e "${temporary_dir}/psql-calls-refusal" ]] || fail "a run with no database URL still called psql"
printf 'PASS: a run with no database URL names --database-url, LEGION_TEST_PG_DSN, DATABASE_URL and dev-postgres.sh, and exits 2\n'

printf 'ALL PASS: dev-stack.test.sh\n'
