#!/usr/bin/env bash
# The live proof that a labelled root still queued in admission, which a person then makes a child
# of a live tree, joins that tree and runs there (INFRA-252): admission's sweep adopts it once its
# recorded parent is in the tree, an architect's release_children adopts it from todo, from backlog
# and from the state INFRA-214 reached before it answers, a todo root under an unreleased child of
# the tree waits in no line, and a child recorded in another tree is refused by name. The whole
# daemon is booted by the function `legion start` calls, over a real PostgreSQL and a real
# nats-server, and driven only through the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream and
# its own HTTP routes; every step is read back from GET /legion/v1/state, the park route, what the
# daemon wrote to the Dispatch stand-in, and the daemon's own record of each issue's tree
# (packages/daemon/internal/daemon/child_adoption_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that
# fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go and awk, as
#   LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/child-adoption.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none, so the tester can re-run it wherever one is. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. CHILD_ADOPTION_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume.
set -euo pipefail

# The nats-server the run installs when the caller names none: the 2.10 line the daemon's own NATS
# tests pin (packages/daemon/internal/testnats/nats.go).
nats_version=v2.10.22

root=$(cd "$(dirname "$0")/../.." && pwd)
evidence=${CHILD_ADOPTION_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-child-adoption-evidence.XXXXXXXX")}
mkdir -p "$evidence"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-child-adoption.$$.XXXXXXXX")
driver=$work/child-adoption.test
nats_pid=''
port_nats=''
check=setup
started=$SECONDS
check_started=$SECONDS

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
    echo "child adoption e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "child adoption e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}
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
# The seven steps print their own `== <name>` / `ok <name>` pair between this check's, so the run's
# checks and the proof's read alike. The daemon's own log goes to the evidence directory rather
# than into them.
LEGION_E2E_NATS_URL="nats://127.0.0.1:$port_nats" \
  LEGION_E2E_DAEMON_LOG="$evidence/daemon.log" \
  TMPDIR="$work" \
  "$driver" -test.count=1 -test.timeout=20m -test.v \
  -test.run '^TestAQueuedRootUnderALiveTreeJoinsItLive$' 2>&1 | tee "$evidence/proof.log" ||
  fail "the proof's driver failed; see $evidence/proof.log and $evidence/daemon.log"
# A driver that skipped, or whose -test.run matched nothing, exits 0 having proved nothing.
grep -q '^--- PASS: TestAQueuedRootUnderALiveTreeJoinsItLive' "$evidence/proof.log" ||
  fail "the proof's driver ran no step: it skipped, or -test.run matched no test; see $evidence/proof.log"
pass
