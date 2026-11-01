#!/usr/bin/env bash
# The live proof that a pull request whose base branch requires no check and no workflow has no CI
# verdict: a failed check there leaves no `checksVerdict` on GET /legion/v1/state and sends nothing
# back, where it read green before; the same failure once the base requires the check is red and
# sends the work back; a passing required check is still green; and at an exhausted fix-attempt
# count a push onto a head whose base requires nothing leaves the counted attempt, so the next red
# once the base requires the check again sends the work back. The whole daemon is booted by the
# function `legion start` calls, over a real PostgreSQL and a real nats-server, and driven only
# through GitHub envelopes on the ENVOY_NOTIFICATIONS stream and its own reads of a GitHub REST
# stand-in whose base branches the driver switches; every step is read back from GET
# /legion/v1/state and from what the daemon published to the Envoy stand-in
# (packages/daemon/internal/daemon/no_required_checks_live_test.go, which this script builds with
# `-tags e2e`). Each check prints `== <name>`, what it observed and `ok <name>`; the first that
# fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go and awk, as
#   LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/no-required-checks.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none, so the tester can re-run it wherever one is. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. NO_REQUIRED_CHECKS_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. The daemon reads what each base branch requires
# every two minutes, and the proof waits for three such reads, so a run takes about seven minutes.
# The run around the driver is lib/live-daemon-proof.sh's, which merged-root-waits.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'no required checks' no-required-checks TestABaseThatRequiresNothingGivesNoVerdictLive \
  "${NO_REQUIRED_CHECKS_EVIDENCE_DIR:-}"
