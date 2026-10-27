#!/usr/bin/env bash
# The live proof that a Legion phase's handoff is a record on its Dispatch issue (INFRA-177's A3,
# A6, A10 and the daemon half of A7, and HANDOFF_NOT_NEW). The driver
# (packages/daemon/internal/daemon/handoff_records_live_test.go, which this script builds with
# `-tags e2e`) builds and starts the real Dispatch server from this checkout's packages/envoy on a
# database of its own, boots the whole daemon with dispatch_url at it, and drives the real
# `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun, as each claim's registered
# session) and the daemon's POST /legion/v1/handoff/complete. Each check prints `== <name>`, what it
# observed and `ok <name>`; the first that fails ends the run non-zero, naming it:
#   - A6: a 40 KB body written with `dispatch handoff-write` reads back byte for byte with
#     `dispatch handoff-read`, and two writes are versions 1 and 2; control: a body of the cap plus
#     one byte (262145) is refused by the CLI, and by Dispatch itself, sent raw, 413 CAP_EXCEEDED;
#   - A3: the planner's completion naming a record another session wrote is refused
#     HANDOFF_SESSION_MISMATCH naming both sessions, and the issue stays in planning; control: after
#     the planner writes its own, the same completion moves the issue to implementing;
#   - A7 (the daemon's half): `legion handoffs import` (handoffimport.Run) writes a testing issue's
#     committed `.legion/<KEY>/*.json` as its records under the daemon's session and removes
#     `.legion/` from a GitHub stand-in's branch; the tester's completion naming the imported record
#     is refused HANDOFF_SESSION_MISMATCH; the tester reads it, writes it again under its session and
#     completes, and the reviewer the daemon starts is prompted with a task naming that record;
#   - HANDOFF_NOT_NEW: the implementer completes round 1, the tester fails it, and the implementer's
#     round-2 completion naming its round-1 record is refused; naming a new record moves it on;
#   - A10: with Dispatch up the first write succeeds; with the Dispatch process stopped
#     `handoff-write` exits non-zero naming the connection failure and the completion naming the
#     existing record is refused DISPATCH_READ_FAILED, the phase unchanged; Dispatch restarted on the
#     same database, the retried write succeeds, the completion is accepted, and GET
#     /api/v1/issues/<KEY>/handoffs (what Conversation lists) holds it.
#
# Run it from a checkout on a Linux host with go, bun (after `bun install` at the checkout's root)
# and awk, as
#   LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/handoff-records.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the
# driver also creates Dispatch's database on the same server (its role must be allowed to create
# and drop databases, and the server must offer the pgvector extension, which Dispatch's migrations
# create) and drops it on exit. This script starts no database. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one installed with `go install`.
# HANDOFF_RECORDS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is unset,
# kept and printed) keeps the transcript, the nats-server's log, the daemon's, Dispatch's
# (dispatch.log) and the driver's output. Nothing reaches github.com or a deployed Dispatch: the run
# is self-contained, so INFRA-480 runs it again on main as it is. A run takes two to four minutes,
# most of it building Dispatch and the driver. The run around the driver is
# lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
command -v bun >/dev/null || {
  echo "FAIL prerequisites: bun is not on PATH; the dispatch CLI the proof drives runs under it" >&2
  exit 1
}
[ -d "$root/node_modules" ] || {
  echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
  exit 1
}
# The driver binary runs from this directory, not the package's, so it is told the checkout it
# builds Dispatch from and runs the CLI out of.
export LEGION_E2E_REPO_ROOT="$root"
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'handoff records' handoff-records TestHandoffRecordsLive \
  "${HANDOFF_RECORDS_EVIDENCE_DIR:-}"
