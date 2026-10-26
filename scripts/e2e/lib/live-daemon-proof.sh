# shellcheck shell=bash
# The run a whole-daemon live proof makes around its driver: merged-root-waits.sh and
# no-required-checks.sh differ only in what their driver proves, so both are this run. Sourced by
# the entry script, never run. The entry script sets `set -euo pipefail` and root, the checkout it
# sources this file from, and calls live_daemon_proof as its last command.
#
# live_daemon_proof LABEL SLUG TEST EVIDENCE runs four checks, each printing `== <check>`, what it
# observed and `ok <check> (<s>s)`; the first that fails prints `FAIL <check>: <why>` and ends the
# run non-zero. prerequisites wants LEGION_TEST_PG_DSN, the PostgreSQL the daemon migrates and runs
# on, which nothing here starts or prints, go and awk, and a nats-server: LEGION_E2E_NATS_BIN's, or
# the pinned one installed with `go install` into a GOBIN of the run's own. broker starts that
# nats-server with JetStream on a port the kernel picks. driver builds the daemon package's
# `-tags e2e` test binary. proof runs the one test TEST against them and holds that it passed.
#
# SLUG names the run's own files: the work directory legion-e2e-SLUG.<pid>.* under TMPDIR (or /tmp
# when it is unset), which holds the broker's store and the driver binary SLUG.test and is removed
# on any exit, and the evidence directory legion-e2e-SLUG-evidence.* beside it when EVIDENCE, the
# directory the caller named, is empty. The evidence directory is kept and printed; it holds the
# transcript (lib/transcript.sh), the nats-server's log, the daemon's own, which the driver writes
# through LEGION_E2E_DAEMON_LOG, and the driver's output. On any exit the nats-server is stopped and
# the run prints `LABEL e2e: PASS in <n>s (evidence: <dir>)`, or `LABEL e2e: FAIL at <check> after
# <n>s (evidence: <dir>)` on stderr.

# shellcheck source-path=SCRIPTDIR source=transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"

# The nats-server the run installs when the caller names none: the 2.10 line the daemon's own NATS
# tests pin (packages/daemon/internal/testnats/nats.go).
nats_version=v2.10.22

# The run's state, global rather than live_daemon_proof's own because cleanup reads it from the
# EXIT trap, after live_daemon_proof has returned.
proof_label=''
evidence=''
work=''
driver=''
nats_pid=''
port_nats=''
check=setup
started=''
check_started=''

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [ -n "$nats_pid" ] && kill -0 "$nats_pid" 2>/dev/null; then
    kill -TERM "$nats_pid" 2>/dev/null || true
    wait "$nats_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "$proof_label e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "$proof_label e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}

live_daemon_proof() {
  local slug=$2 test_name=$3 tool nats_bin
  proof_label=$1
  evidence=${4:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug-evidence.XXXXXXXX")}
  mkdir -p "$evidence"
  transcript_to "$evidence/transcript.log"

  work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug.$$.XXXXXXXX")
  driver=$work/$slug.test
  started=$SECONDS
  check_started=$SECONDS
  trap cleanup EXIT

  begin prerequisites
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    fail "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in go awk; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  note "go $(go version | awk '{print $3}'), store LEGION_TEST_PG_DSN (set; not printed)"
  nats_bin=${LEGION_E2E_NATS_BIN:-}
  if [ -z "$nats_bin" ]; then
    note "LEGION_E2E_NATS_BIN names none; installing nats-server $nats_version into this run's own GOBIN"
    GOBIN="$work/bin" go install "github.com/nats-io/nats-server/v2@$nats_version" ||
      fail "could not install nats-server $nats_version; name a binary in LEGION_E2E_NATS_BIN instead"
    nats_bin=$work/bin/nats-server
  fi
  [ -x "$nats_bin" ] || fail "no executable nats-server at $nats_bin"
  note "nats-server $("$nats_bin" --version)"
  pass

  begin broker
  mkdir -p "$work/nats-store"
  # Both logs are this run's, whatever an evidence directory a caller named already holds: the port
  # below is read back out of the broker's.
  : >"$evidence/nats.log"
  : >"$evidence/daemon.log"
  # The port is the kernel's own pick (`-p -1`), read back from the line the server logs once it is
  # listening: the server binds it before anything else can take it, which no free-port pick can
  # promise, and a pod with no `ss` runs the proof all the same.
  "$nats_bin" -js -a 127.0.0.1 -p -1 -sd "$work/nats-store" >>"$evidence/nats.log" 2>&1 &
  nats_pid=$!
  for _ in $(seq 1 120); do
    grep -q 'Server is ready' "$evidence/nats.log" && break
    kill -0 "$nats_pid" 2>/dev/null || fail "the nats-server exited at startup; see $evidence/nats.log"
    sleep 0.5
  done
  grep -q 'Server is ready' "$evidence/nats.log" || fail "the nats-server was not ready within 60s; see $evidence/nats.log"
  port_nats=$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "$evidence/nats.log")
  [ -n "$port_nats" ] || fail "the nats-server logged no client port; see $evidence/nats.log"
  note "JetStream on nats://127.0.0.1:$port_nats, store $work/nats-store, log $evidence/nats.log"
  pass

  begin driver
  go -C "$root/packages/daemon" test -c -tags e2e -o "$driver" ./internal/daemon ||
    fail "the driver did not build"
  note "built $(basename "$driver") from $root/packages/daemon with -tags e2e"
  pass

  begin proof
  # The driver's steps print their own `== <name>` / `ok <name>` pair between this check's, so the
  # run's checks and the proof's read alike. The daemon's own log goes to the evidence directory
  # rather than into them.
  LEGION_E2E_NATS_URL="nats://127.0.0.1:$port_nats" \
    LEGION_E2E_DAEMON_LOG="$evidence/daemon.log" \
    TMPDIR="$work" \
    "$driver" -test.count=1 -test.timeout=20m -test.v \
    -test.run "^$test_name\$" 2>&1 | tee "$evidence/proof.log" ||
    fail "the proof's driver failed; see $evidence/proof.log and $evidence/daemon.log"
  # A driver that skipped, or whose -test.run matched nothing, exits 0 having proved nothing.
  grep -q "^--- PASS: $test_name" "$evidence/proof.log" ||
    fail "the proof's driver ran no step: it skipped, or -test.run matched no test; see $evidence/proof.log"
  pass
}
