#!/usr/bin/env bash
# The live proof that the daemon keeps a tree's order: an acceptance is refused while any issue
# beneath the signing one is in the workflow, and a merged root's close waits for the rest of its
# tree instead of lingering it mid-turn. The whole daemon is booted by the function `legion start`
# calls, over a real PostgreSQL and a real nats-server, and driven only through the GitHub and
# Dispatch envelopes of the ENVOY_NOTIFICATIONS stream and its own HTTP routes; every step is read
# back from GET /legion/v1/state and from what the daemon posted to the Dispatch and Envoy
# stand-ins (packages/daemon/internal/daemon/merged_root_waits_live_test.go, which this script
# builds with `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go and awk, as
#   LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/merged-root-waits.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none, so the tester can re-run it wherever one is. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. MERGED_ROOT_WAITS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. The run around the driver is
# lib/live-daemon-proof.sh's, which no-required-checks.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'merged root waits' merged-root-waits TestAMergedRootsCloseWaitsForTheRestOfItsTreeLive \
  "${MERGED_ROOT_WAITS_EVIDENCE_DIR:-}"
