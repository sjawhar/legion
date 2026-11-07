#!/usr/bin/env bash
# The live proof that a provider error keeps a worker's task and that the operator can retry what
# it leaves (dispatch://INFRA-956 R2 and R3). The whole daemon is booted by the function `legion
# start` calls, over a real PostgreSQL and a real nats-server, and three real `omp --mode rpc`
# panes run under it — the real `legion worker-shim`, this checkout's `legion`, and this checkout's
# @sjawhar/pi-envoy and @sjawhar/pi-legion in an Oh My Pi profile of the run's own — against three
# HTTP model stand-ins of the driver's own. A 529 keeps the task on the claim and the daemon sends
# it to the same session again after 30 s, 60 s and 120 s; the sixth turn in a row tells the
# project's controller, once, and holds nothing; an OpenAI `finish_reason: content_filter` with no
# fallback model left carries no HTTP status at all, so the task goes again after 30 s, holding the
# issue for nobody; an HTTP 400, a client error that is none of 401, 403, 408 and 429, fails the claim and
# holds its issue for its architect; and `legion claims stop`,
# `legion claims retry --fresh-session` and `legion claims list` — the built binary against that
# daemon — drive and read the claims the first and the fourth leave
# (packages/daemon/internal/daemon/provider_error_retry_live_test.go, which this script builds with
# `-tags e2e`, and packages/daemon/internal/daemon/localruntime_live_test.go, the runtime that
# starts a pane with no tmux). Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# R3's other two operations — the compaction `legion claims compact` and the controller's
# `compact` run, and the fresh session the controller's `fresh_session` runs — are
# scripts/e2e/claim-compaction.sh's, a proof of the same shape with four panes of its own: the
# two runs would otherwise share a wall clock.
#
# The whole run took 317 s measured in a Legion worker pod (omp/18.16.0), the driver 300 s of it:
# the ladder's first three gaps are waited out where they are real (30 s, 60 s and 120 s of wall
# clock between the daemon's sends), its fourth and fifth are driven on the daemon's own clock, and
# three panes each start Oh My Pi once. The ladder is measured from the daemon's own first send of
# the task the planner's claim holds from the moment it boots, which is what that phase's start
# left pending on it; the other two panes answer a warm-up task of their own before the step that
# drives them (the driver's seed says why each claim holds one).
#
# Run it from a checkout on a Linux host with go, awk, bash, tar and bun, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/provider-error-retry.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's fetch, and the driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned
# one this script installs with `go install` into a GOBIN of its own, started with JetStream on a
# port of the run's own and stopped on any exit. PROVIDER_ERROR_RETRY_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's own and the driver's output; the run's work directory, the driver
# binary among it, is under TMPDIR too, since a pod's own disk is small and its TMPDIR names the
# issue's volume. The driver builds `legion` and packs the two plugins from this checkout itself,
# so it needs bun and the repository's node_modules. It needs no kube context and no tmux — the
# panes are child process groups of the driver — so it runs in a Legion pod and on a devbox alike.
# The run around the driver is lib/live-daemon-proof.sh's, which ci-red-no-takeover.sh and
# merged-root-waits.sh make too.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "${LEGION_OMP_PATH:-}" ]; then
  echo "FAIL setup: LEGION_OMP_PATH is required: it names the Oh My Pi every pane of this proof runs (an absolute executable path). This proof launches real agents and guesses no build." >&2
  exit 1
fi
if [ ! -x "$LEGION_OMP_PATH" ]; then
  echo "FAIL setup: LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file" >&2
  exit 1
fi
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
live_daemon_proof 'provider error retry' provider-error-retry \
  TestAProviderErrorKeepsTheTaskAndTheOperatorRetriesWhatItLeavesLive \
  "${PROVIDER_ERROR_RETRY_EVIDENCE_DIR:-}"
