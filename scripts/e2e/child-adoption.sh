#!/usr/bin/env bash
# The live proof that a labelled root still queued in admission, which a person then makes a child
# of a live tree, joins that tree and runs there (INFRA-252): admission's sweep adopts it once its
# recorded parent is in the tree, an architect's release_children adopts it from todo, from backlog
# and from the state INFRA-214 reached before it answers, a todo root under an unreleased child of
# the tree waits in no line, past a root of its own parked on its chain too, and a child recorded in
# another tree, the child of a root held under an unreleased child among them, is refused by name.
# The whole daemon is booted by the function `legion start` calls, over a real PostgreSQL and a
# real nats-server, and driven only through the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream
# and its own HTTP routes; every
# step of its eleven is read back from GET /legion/v1/state, the park route, what the daemon wrote
# to the Dispatch stand-in, and the daemon's own record of each issue's tree
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
# small and its TMPDIR names the issue's volume. The run around the driver is
# lib/live-daemon-proof.sh's, which merged-root-waits.sh and no-required-checks.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'child adoption' child-adoption TestAQueuedRootUnderALiveTreeJoinsItLive \
  "${CHILD_ADOPTION_EVIDENCE_DIR:-}"
