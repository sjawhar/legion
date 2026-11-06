#!/usr/bin/env bash
# packages/envoy/scripts/dev-airlock.test.sh
#
# Proves what dev-airlock.sh's own shell driver decides, with no real Postgres, no real network
# bind and no Go build: fakes `psql`, `curl` and `go` on PATH, where the fake `go build -o
# <path> ./cmd/<pkg>` installs a stub at <path> rather than compiling. The stubs stand in for
# every process the stack starts - dev-oidc-issuer, the broker, agent-secrets and
# dev-airlock-stands in its three modes - and each records what it was given, so the assertions
# below read the stack's own decisions rather than a broker's behaviour. The broker's own
# airlock lives in Go and has its own rows; what lives in this script is which processes are
# started, with which settings, behind which address, and what is stopped at the end.
#
# dev-airlock.sh runs scripts/dev-broker.sh for Postgres, the issuer and the binaries, so that
# script runs here too, against the same fakes and under DEV_BROKER_POSTGRES_URL (its no-Docker
# branch, the one a Legion pod takes). `jq` is real: the stack reads the stands' JSON report
# with it, and a fake would prove nothing about that reading.
#
# It asserts, one PASS line each: that the run's work directory, and dev-broker.sh's own, are
# under TMPDIR; that two broker processes are started, each with the whole BROKER_AIRLOCK_*
# block and both on one database; that one front stands over the two API ports and one over the
# two proxy ports, and that both brokers name those fronts as their public and proxy URLs; that
# --keep-values, --no-kind and --idle-seconds reach the brokers' environment and --one-broker
# starts one; that the pod session's enrolment id is written beside its key where
# AGENT_SECRETS_KEY_DIR names it; that a usage error exits 2; and that the cleanup stops every
# process the run started and drops the database it created.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-airlock.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
# The TMPDIR each run is given: every directory the stack makes must be under it.
readonly run_tmpdir="${temporary_dir}/tmp"
mkdir -p "$fake_bin_dir" "$run_tmpdir"

driver_pid=""
cleanup() {
  if [[ -n "$driver_pid" ]] && kill -0 "$driver_pid" 2>/dev/null; then
    kill -TERM "$driver_pid" 2>/dev/null || true
    wait "$driver_pid" 2>/dev/null || true
  fi
  # A stub that outlived its run would hold a port and a process slot for the next one.
  if [[ -f "${temporary_dir}/pids" ]]; then
    while read -r pid; do
      kill -KILL "$pid" 2>/dev/null || true
    done <"${temporary_dir}/pids"
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake psql: records every call to DEV_AIRLOCK_TEST_PSQL_CALLS, which both dev-broker.sh's
# and dev-airlock.sh's admin calls land in, and answers `select current_database()` with the
# database the URL in its first argument connects to, as libpq does. ---
cat >"${fake_bin_dir}/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"${DEV_AIRLOCK_TEST_PSQL_CALLS:?}"
if [[ "$*" == *"select current_database()"* ]]; then
  url=$1
  path=${url%%\?*}
  printf '%s\n' "${path##*/}"
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/psql"

# --- Fake curl: dev-broker.sh registers its exchange pod and polls its own /healthz, and
# dev-airlock.sh polls the API front's. Record each URL and answer at once, so no readiness loop
# ever opens a socket. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_AIRLOCK_TEST_CURL_CALLS:?}"
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# --- Fake go: "go build -o <path> ./cmd/<pkg>" installs a stub at <path>. Each stub that stays
# alive writes its own PID to DEV_AIRLOCK_TEST_PIDS first, which is how the cleanup assertion
# below knows what the run started. ---
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
  */agent-secrets-helper|*/agent-secrets-devrelay)
    cat >"$out" <<'NOTHING'
#!/usr/bin/env bash
exit 0
NOTHING
    ;;
  */agent-secrets)
    # Only `keygen --out <dir>` is ever run by the stack: it makes the session key and prints
    # its thumbprint, which the enrolment sends.
    cat >"$out" <<'AGENTSECRETS'
#!/usr/bin/env bash
if [[ "${1:-}" != keygen ]]; then
  echo "fake agent-secrets: unexpected form $*" >&2
  exit 2
fi
dir=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == --out ]]; then dir="$2"; shift; fi
  shift
done
mkdir -p "$dir"
printf 'a fake session key\n' >"$dir/key.pem"
printf 'the-session-key-thumbprint\n'
AGENTSECRETS
    ;;
  */dev-oidc-issuer)
    # Writes each --token file and logs the line dev-airlock.sh reads each account, the
    # broker's audience and the pod's uid out of, then its own URL, as the real one does.
    cat >"$out" <<'ISSUER'
#!/usr/bin/env bash
printf '%s\n' "$$" >>"${DEV_AIRLOCK_TEST_PIDS:?}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token)
      spec="$2"
      audience="${spec%%=*}"
      rest="${spec#*=}"
      account="${rest%%=*}"
      file="${rest#*=}"
      printf 'fake-token-for-%s\n' "$account" >"$file"
      echo "dev-oidc-issuer: $file holds $account's token for audience $audience (pod pod-uid-of-${account##*:})" >&2
      ;;
    --code-flow|--sign-in-as) ;;
    *)
      echo "fake dev-oidc-issuer: unexpected argument $1" >&2
      exit 2
      ;;
  esac
  shift 2 || exit 2
done
echo "dev-oidc-issuer: issuer listening url=http://127.0.0.1:$$" >&2
exec sleep 999999
ISSUER
    ;;
  */broker)
    # Logs the whole of what it was configured with, then the two lines the stack reads its
    # addresses from: the API port always, the proxy port only with a rules file, exactly as
    # the real broker binds a proxy port only for a deployment that has one.
    cat >"$out" <<'BROKER'
#!/usr/bin/env bash
printf '%s\n' "$$" >>"${DEV_AIRLOCK_TEST_PIDS:?}"
echo "fake broker: pid=$$ db=${BROKER_DATABASE_URL:-} public=${BROKER_PUBLIC_URL:-} services=${BROKER_SERVICES:-} logins=${BROKER_SERVICE_LOGINS:-} fake_secrets=${BROKER_FAKE_SECRETS_FILE:-} azure_secret=${BROKER_AZURE_LOGIN_SECRET:-}" >&2
echo "fake broker airlock: pid=$$ rules=${BROKER_AIRLOCK_RULES_FILE:-} proxy_url=${BROKER_AIRLOCK_PROXY_URL:-} ca_cert=${BROKER_AIRLOCK_CA_CERT_FILE:-} ca_key=${BROKER_AIRLOCK_CA_KEY_FILE:-} listen=${BROKER_AIRLOCK_LISTEN_ADDR:-} idle=${BROKER_AIRLOCK_IDLE_SECONDS:-} keep_values=${BROKER_AIRLOCK_TEST_KEEP_VALUES:-} no_kind=${BROKER_AIRLOCK_TEST_NO_KIND:-} github=${BROKER_AIRLOCK_TEST_GITHUB_API_BASE:-} google=${BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL:-} upstream_ca=${BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE:-} dial_allow=[${BROKER_AIRLOCK_TEST_DIAL_ALLOW:-}] resolve=[${BROKER_AIRLOCK_TEST_RESOLVE:-}]" >&2
echo "broker listening addr=127.0.0.1:$$" >&2
if [[ -n "${BROKER_AIRLOCK_RULES_FILE:-}" ]]; then
  echo "airlock proxy listening addr=127.0.0.1:1$$" >&2
fi
exec sleep 999999
BROKER
    ;;
  */dev-airlock-stands)
    # The three modes the stack drives: the stands, which write the files and the JSON report
    # it reads; the front, which reports the address it bound; and the enrolment, which
    # answers an enrollment id and records what it was asked to enrol.
    cat >"$out" <<'STANDS'
#!/usr/bin/env bash
case "${1:-}" in
  front)
    printf '%s\n' "$$" >>"${DEV_AIRLOCK_TEST_PIDS:?}"
    printf '%s\n' "front $*" >>"${DEV_AIRLOCK_TEST_STANDS_CALLS:?}"
    printf '{"address": "127.0.0.1:%s", "url": "http://127.0.0.1:%s"}\n' "$$" "$$"
    echo "dev-airlock-stands: front listening addr=127.0.0.1:$$" >&2
    exec sleep 999999
    ;;
  enroll-pod)
    printf '%s\n' "enroll-pod $*" >>"${DEV_AIRLOCK_TEST_STANDS_CALLS:?}"
    printf '{"credential_id": "the-launcher-credential", "enrollment_id": "the-pod-enrollment", "lease_expires_at": "2100-01-01T00:00:00Z"}\n'
    exit 0
    ;;
esac
printf '%s\n' "$$" >>"${DEV_AIRLOCK_TEST_PIDS:?}"
printf '%s\n' "stands $*" >>"${DEV_AIRLOCK_TEST_STANDS_CALLS:?}"
dir=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == --dir ]]; then dir="$arg"; fi
  prev="$arg"
done
mkdir -p "$dir"
printf 'a fake authority certificate\n' >"$dir/ca.crt"
printf 'a fake authority key\n' >"$dir/ca.key"
printf -- '-----BEGIN RSA PRIVATE KEY-----\na fake GitHub App key\n-----END RSA PRIVATE KEY-----\n' >"$dir/github-app.pem"
cat <<REPORT
{
  "dir": "$dir",
  "ca_cert_file": "$dir/ca.crt",
  "ca_key_file": "$dir/ca.key",
  "records_url": "http://127.0.0.1:$$",
  "aws_key_id": "AKIASTANDINKEY000000",
  "aws_secret_key": "the-stand-in-aws-secret-access-key",
  "github_app_id": "123456",
  "github_app_key_file": "$dir/github-app.pem",
  "stands": {
    "s": {"address": "127.0.0.1:1001", "url": "https://127.0.0.1:1001", "records_url": "http://127.0.0.1:$$/records/s"},
    "t": {"address": "127.0.0.1:1002", "url": "https://127.0.0.1:1002", "records_url": "http://127.0.0.1:$$/records/t"},
    "github": {"address": "127.0.0.1:1003", "url": "http://127.0.0.1:1003", "records_url": "http://127.0.0.1:$$/records/github"},
    "google": {"address": "127.0.0.1:1004", "url": "http://127.0.0.1:1004", "records_url": "http://127.0.0.1:$$/records/google"},
    "microsoft": {"address": "127.0.0.1:1005", "url": "https://127.0.0.1:1005", "records_url": "http://127.0.0.1:$$/records/microsoft"},
    "aws": {"address": "127.0.0.1:1006", "url": "https://127.0.0.1:1006", "records_url": "http://127.0.0.1:$$/records/aws"}
  }
}
REPORT
echo "dev-airlock-stands: stands listening records=http://127.0.0.1:$$" >&2
exec sleep 999999
STANDS
    ;;
  *)
    echo "fake go: unhandled build target $pkg" >&2
    exit 1
    ;;
esac
chmod +x "$out"
EOF
chmod +x "${fake_bin_dir}/go"

readonly admin_url='postgres://tester@127.0.0.1:5999/postgres?sslmode=disable'
readonly log="${temporary_dir}/run.log"
readonly psql_calls="${temporary_dir}/psql-calls"
readonly curl_calls="${temporary_dir}/curl-calls"
readonly stands_calls="${temporary_dir}/stands-calls"
readonly pids_file="${temporary_dir}/pids"

# run_stack starts dev-airlock.sh in the background with every call file emptied, and sets the
# global driver_pid (never in a subshell: the EXIT trap's kill must see this assignment).
run_stack() {
  : >"$log"
  : >"$psql_calls"
  : >"$curl_calls"
  : >"$stands_calls"
  : >"$pids_file"
  PATH="${fake_bin_dir}:${PATH}" TMPDIR="$run_tmpdir" DEV_BROKER_POSTGRES_URL="$admin_url" \
    DEV_AIRLOCK_TEST_PSQL_CALLS="$psql_calls" DEV_AIRLOCK_TEST_CURL_CALLS="$curl_calls" \
    DEV_AIRLOCK_TEST_STANDS_CALLS="$stands_calls" DEV_AIRLOCK_TEST_PIDS="$pids_file" \
    bash "$driver" "$@" >"$log" 2>&1 &
  driver_pid=$!
}

wait_for_ready() {
  for _ in $(seq 1 300); do
    grep -q '^dev-airlock: ready\.$' "$log" 2>/dev/null && return 0
    kill -0 "$driver_pid" 2>/dev/null || break
    sleep 0.1
  done
  cat "$log" >&2
  echo "FAIL: dev-airlock.sh never reached ready" >&2
  exit 1
}

# stop_stack sends the run the signal a Ctrl-C sends it and waits for it to go.
stop_stack() {
  kill -TERM "$driver_pid" 2>/dev/null || true
  for _ in $(seq 1 100); do
    kill -0 "$driver_pid" 2>/dev/null || break
    sleep 0.1
  done
  wait "$driver_pid" 2>/dev/null || true
  driver_pid=""
}

# exported reads one "export NAME=value" line of dev-airlock.sh's own banner: the last such
# line in the log, since dev-broker.sh's banner, printed first, exports some of the same names
# for its own broker.
exported() {
  grep -oE "export $1=[^[:space:]]+" "$log" | tail -n1 | cut -d= -f2- || true
}

# --- The default run: two brokers. ---
run_stack
wait_for_ready

airlock_dir="$(exported AIRLOCK_DIR)"
dev_broker_dir="$(exported DEV_BROKER_DIR)"
if [[ "$airlock_dir" != "$run_tmpdir"/* || "$dev_broker_dir" != "$run_tmpdir"/* ]]; then
  echo "FAIL: the work directories are $airlock_dir and $dev_broker_dir; want both under TMPDIR ($run_tmpdir)" >&2
  exit 1
fi
printf 'PASS: the run keeps its own work directory (%s) and dev-broker.sh'"'"'s (%s) under TMPDIR, never /tmp\n' \
  "$airlock_dir" "$dev_broker_dir"

# Each broker logs one "fake broker airlock:" line naming every BROKER_AIRLOCK_* setting it was
# started with. The airlock stack's brokers are the ones with a rules file; dev-broker.sh's own
# broker has none, which is what keeps it out of these counts.
mapfile -t airlock_lines < <(grep -o 'fake broker airlock: .*' "$log" | grep -v 'rules= ')
if [[ "${#airlock_lines[@]}" -ne 2 ]]; then
  cat "$log" >&2
  echo "FAIL: ${#airlock_lines[@]} broker(s) were started with an airlock; want 2" >&2
  exit 1
fi
rules_file="$(exported AIRLOCK_RULES_FILE)"
ca_cert="$(exported AIRLOCK_CA_CERT_FILE)"
ca_key="$(exported AIRLOCK_CA_KEY_FILE)"
proxy_front_url="$(exported AIRLOCK_PROXY_URL)"
api_front_url="$(exported AGENT_SECRETS_URL)"
for line in "${airlock_lines[@]}"; do
  for want in "rules=$rules_file" "proxy_url=$proxy_front_url" "ca_cert=$ca_cert" "ca_key=$ca_key" \
    "listen=127.0.0.1:0" "idle=300" "upstream_ca=$ca_cert" "keep_values=false" "no_kind=false"; do
    if [[ "$line" != *"$want"* ]]; then
      echo "FAIL: a broker was started without $want: $line" >&2
      exit 1
    fi
  done
  for want in 's.airlock.test:443' 't.airlock.test:443' 'microsoft.airlock.test:443' \
    'azure-resource.airlock.test:443' 'aws.airlock.test:443'; do
    if [[ "$line" != *"dial_allow="*"$want"* ]]; then
      echo "FAIL: a broker may not dial the stand-in at $want: $line" >&2
      exit 1
    fi
  done
  if [[ "$line" != *'resolve='*'s.airlock.test=127.0.0.2'* ]]; then
    echo "FAIL: a broker is not told where s.airlock.test is: $line" >&2
    exit 1
  fi
done
printf 'PASS: two broker processes are started, each with the whole BROKER_AIRLOCK_* block (rules, proxy URL, authority, idle limit, the stand-ins and the addresses it may dial)\n'

mapfile -t broker_lines < <(grep -o 'fake broker: .*' "$log")
airlock_dbs=()
for line in "${broker_lines[@]}"; do
  pid="$(printf '%s' "$line" | sed -E 's/.*pid=([0-9]+).*/\1/')"
  for airlock_line in "${airlock_lines[@]}"; do
    if [[ "$airlock_line" == *"pid=$pid "* ]]; then
      airlock_dbs+=("$(printf '%s' "$line" | sed -E 's/.*db=([^ ]+).*/\1/')")
      if [[ "$line" != *"public=$api_front_url "* ]]; then
        echo "FAIL: a broker's public URL is not the API front ($api_front_url): $line" >&2
        exit 1
      fi
    fi
  done
done
if [[ "${#airlock_dbs[@]}" -ne 2 || "${airlock_dbs[0]}" != "${airlock_dbs[1]}" ]]; then
  echo "FAIL: the airlock's brokers are on ${airlock_dbs[*]}; want one database for both" >&2
  exit 1
fi
if [[ "${airlock_dbs[0]}" != *"/dev_airlock_"* ]]; then
  echo "FAIL: the airlock's brokers are on ${airlock_dbs[0]}; want a dev_airlock_ database of this run's own" >&2
  exit 1
fi
printf 'PASS: both brokers read one database of this run'"'"'s own (%s) and answer as one public URL (%s)\n' \
  "${airlock_dbs[0]}" "$api_front_url"

# The two fronts: each bound before the brokers and given its backends afterwards, through the
# file it rereads. Each front's backends file must hold both brokers' own ports.
api_backends="$airlock_dir/api-backends"
proxy_backends="$airlock_dir/proxy-backends"
api_count="$(grep -c . "$api_backends" || true)"
proxy_count="$(grep -c . "$proxy_backends" || true)"
if [[ "$api_count" -ne 2 || "$proxy_count" -ne 2 ]]; then
  echo "FAIL: the fronts stand over $api_count API and $proxy_count proxy addresses; want 2 each" >&2
  cat "$api_backends" "$proxy_backends" >&2
  exit 1
fi
for line in "${airlock_lines[@]}"; do
  pid="$(printf '%s' "$line" | sed -E 's/.*pid=([0-9]+).*/\1/')"
  if ! grep -qxF "127.0.0.1:$pid" "$api_backends"; then
    echo "FAIL: broker $pid's API port is not behind the API front" >&2
    exit 1
  fi
  if ! grep -qxF "127.0.0.1:1$pid" "$proxy_backends"; then
    echo "FAIL: broker $pid's proxy port is not behind the proxy front" >&2
    exit 1
  fi
done
if ! grep -q "front .*--backends-file $api_backends" "$stands_calls" ||
  ! grep -q "front .*--backends-file $proxy_backends" "$stands_calls"; then
  cat "$stands_calls" >&2
  echo "FAIL: the two fronts were not started over those backends files" >&2
  exit 1
fi
printf 'PASS: one front stands over both brokers'"'"' API ports and one over both proxy ports\n'

# The enrolment: made against the API front, with the registrar's token and the pod's own, and
# its id written beside the session key AGENT_SECRETS_KEY_DIR names.
key_dir="$(exported AGENT_SECRETS_KEY_DIR)"
enroll_call="$(grep -m1 '^enroll-pod ' "$stands_calls" || true)"
for want in "--url $api_front_url" "--token-file $dev_broker_dir/registrar-token" \
  "--pod-token-file $dev_broker_dir/pod-token" "--pod-uid pod-uid-of-" "--thumbprint the-session-key-thumbprint"; do
  if [[ "$enroll_call" != *"$want"* ]]; then
    echo "FAIL: the pod enrolment was made without $want: $enroll_call" >&2
    exit 1
  fi
done
if [[ "$(cat "$key_dir/enrollment" 2>/dev/null)" != "the-pod-enrollment" || ! -s "$key_dir/key.pem" ]]; then
  echo "FAIL: $key_dir holds no key.pem and enrollment for the pod session" >&2
  exit 1
fi
printf 'PASS: a pod session is enrolled at the API front under the registrar account, and its id is written beside the key in AGENT_SECRETS_KEY_DIR (%s)\n' "$key_dir"

# The cleanup: every process this run started is gone, and the database it created is dropped.
mapfile -t started_pids <"$pids_file"
if [[ "${#started_pids[@]}" -lt 4 ]]; then
  echo "FAIL: the run started only ${#started_pids[@]} processes; want the issuer, three brokers, two fronts and the stands" >&2
  exit 1
fi
airlock_db="${airlock_dbs[0]##*/}"
airlock_db="${airlock_db%%\?*}"
stop_stack
still_running=()
for pid in "${started_pids[@]}"; do
  if kill -0 "$pid" 2>/dev/null; then
    still_running+=("$pid")
  fi
done
if [[ "${#still_running[@]}" -ne 0 ]]; then
  echo "FAIL: ${#still_running[@]} of the run's processes are still running after it stopped: ${still_running[*]}" >&2
  exit 1
fi
if ! grep -qF "drop database if exists ${airlock_db};" "$psql_calls"; then
  cat "$psql_calls" >&2
  echo "FAIL: the run did not drop the database it created ($airlock_db)" >&2
  exit 1
fi
printf 'PASS: stopping the run stops every one of the %d processes it started and drops the database it created (%s)\n' \
  "${#started_pids[@]}" "$airlock_db"

# --- The negative controls and the idle limit, which the checks turn on from the command line. ---
run_stack --keep-values --no-kind --idle-seconds 60 --one-broker
wait_for_ready
mapfile -t airlock_lines < <(grep -o 'fake broker airlock: .*' "$log" | grep -v 'rules= ')
if [[ "${#airlock_lines[@]}" -ne 1 ]]; then
  echo "FAIL: --one-broker started ${#airlock_lines[@]} brokers with an airlock; want 1" >&2
  exit 1
fi
for want in "keep_values=true" "no_kind=true" "idle=60"; do
  if [[ "${airlock_lines[0]}" != *"$want"* ]]; then
    echo "FAIL: the broker was started without $want: ${airlock_lines[0]}" >&2
    exit 1
  fi
done
stop_stack
printf 'PASS: --keep-values, --no-kind and --idle-seconds 60 reach the broker as BROKER_AIRLOCK_TEST_KEEP_VALUES, BROKER_AIRLOCK_TEST_NO_KIND and BROKER_AIRLOCK_IDLE_SECONDS, and --one-broker starts one\n'

# --- A usage error exits 2 and starts nothing. ---
for args in "--nope" "--idle-seconds" "--idle-seconds five" "--idle-seconds 5"; do
  : >"$psql_calls"
  status=0
  # shellcheck disable=SC2086
  PATH="${fake_bin_dir}:${PATH}" TMPDIR="$run_tmpdir" DEV_BROKER_POSTGRES_URL="$admin_url" \
    DEV_AIRLOCK_TEST_PSQL_CALLS="$psql_calls" DEV_AIRLOCK_TEST_CURL_CALLS="$curl_calls" \
    DEV_AIRLOCK_TEST_STANDS_CALLS="$stands_calls" DEV_AIRLOCK_TEST_PIDS="$pids_file" \
    bash "$driver" $args >"$log" 2>&1 || status=$?
  if [[ "$status" -ne 2 ]]; then
    cat "$log" >&2
    echo "FAIL: dev-airlock.sh $args exited $status; want 2" >&2
    exit 1
  fi
  if [[ -s "$psql_calls" ]]; then
    echo "FAIL: dev-airlock.sh $args created something before it refused" >&2
    exit 1
  fi
done
printf 'PASS: a usage error exits 2 and creates nothing (--nope, --idle-seconds with no number, with a word, and below the broker'"'"'s own bound)\n'

# --- --help prints the usage and exits 0. ---
help_status=0
PATH="${fake_bin_dir}:${PATH}" TMPDIR="$run_tmpdir" bash "$driver" --help >"$log" 2>&1 || help_status=$?
if [[ "$help_status" -ne 0 ]] || ! grep -q 'usage: dev-airlock.sh' "$log"; then
  cat "$log" >&2
  echo "FAIL: --help exited $help_status without printing the usage" >&2
  exit 1
fi
printf 'PASS: --help prints the usage and exits 0\n'
