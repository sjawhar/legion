#!/usr/bin/env bash
# The live proof that a phase move the daemon makes on its own — here a head that starts
# conflicting with its base — takes no role's turn over: the start it makes acts at once — the
# implementer is prompted while the tester is still working the turn its own assignment began, and
# that turn is neither ended nor asked after, since nothing at all is sent to the tester's
# connection — the tester is told its phase ended by one message on its own Envoy role topic, a
# completion reported from that old turn once the issue has handed testing out again is refused
# HANDOFF_STALE_ASSIGNMENT while the completion of the handoff the new assignment's own task calls
# for, at a commit of its own, is taken, and a phase-ended message to a role topic no live session
# holds finishes its outbox row instead of retrying for ever, with the start still acting. The
# whole daemon is booted by the function `legion start` calls, over a real PostgreSQL and a real
# nats-server, and driven only through GitHub envelopes on the ENVOY_NOTIFICATIONS stream, the
# daemon's own HTTP routes and its worker stream, which the driver speaks as `legion worker-shim`
# does; every step is read back from GET /legion/v1/state, from the operator's claim listing, from
# what the daemon published to the Envoy stand-in and from what it sent — or did not send — to a
# role's own connection (packages/daemon/internal/daemon/conflict_no_takeover_live_test.go, which
# this script builds with `-tags e2e`). Each check prints `== <name>`, what it observed and
# `ok <name>`; the first that fails ends the run non-zero, naming it.
#
# Run it from a checkout on a Linux host with go and awk, as
#   LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/conflict-no-takeover.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none, so the tester can re-run it wherever one is. The broker is the run's own: a
# nats-server from LEGION_E2E_NATS_BIN, or the pinned one this script installs with `go install`
# into a GOBIN of its own, started with JetStream on a port of the run's own and stopped on any
# exit. CONFLICT_NO_TAKEOVER_EVIDENCE_DIR (default a fresh directory under TMPDIR, or /tmp when it is
# unset, kept and printed) keeps the transcript, the nats-server's log and the daemon's own; the
# run's work directory, the driver binary among it, is under TMPDIR too, since a pod's own disk is
# small and its TMPDIR names the issue's volume. It needs no kube context, no tmux and no agent, so
# it runs in a Legion pod, in CI and on a devbox alike. The run around the driver is
# lib/live-daemon-proof.sh's, which child-adoption.sh makes too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'conflict no takeover' conflict-no-takeover TestAConflictStartsTheNextRoleAtOnceAndTakesNoTurnOverLive \
  "${CONFLICT_NO_TAKEOVER_EVIDENCE_DIR:-}"
