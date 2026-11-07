#!/usr/bin/env bash
# The live proof of the two operations an operator and a project's controller have over a claim
# whose provider keeps refusing its session (dispatch://INFRA-956 R3): `legion claims compact` and
# the controller's `compact`, and `legion claims retry --fresh-session` and the controller's
# `fresh_session`, each driven through both of its doors. The whole daemon is booted by the
# function `legion start` calls, over a real PostgreSQL and a real nats-server, and four real `omp
# --mode rpc` panes run under it — the real `legion worker-shim`, this checkout's `legion`, and
# this checkout's @sjawhar/pi-envoy and @sjawhar/pi-legion in an Oh My Pi profile of the run's own
# — against four HTTP model stand-ins of the driver's own, each of which answers a turn's request
# and a compaction's summary request apart.
#
# Each pane runs the overlays a deployment renders: the operator's own
# (`runtime.tmux.pi_config_files`), which turns `retry.enabled` off and sets
# `compaction.keepRecentTokens` to 1, then the claim's own (`role_config_files`), which names the
# one model its stand-in serves — a model that reads images. Nothing names a compaction method, so
# every pane runs Oh My Pi's own default order, which puts the image-based `snapcompact` above
# every summary method: the only thing that keeps a compaction off it is the custom instructions
# the daemon sends, and the proof reads the compaction entry back out of the pane's own session
# file to show the summary is text.
#
# After a compaction the worker keeps its session, the task it owes goes once and only after the
# compaction, and a root architect that owes nothing is sent nothing; a compaction whose endpoint
# holds the summary past `worker_rpc_timeout_seconds` and past 30 s still returns its real answer,
# and a `fresh_session` asked for while it runs is refused; a refused summary and an already
# compacted session are reported to the caller with the owed task sent again all the same; six
# turns in a row in a provider error tell the project's controller, and a compaction that ran puts
# that count back to zero so six more tell it a second time; a suspended, a stopped, a failed and
# a Claude Code worker, a worker in a turn and role `controller` are each refused in their own
# words through both doors; and a fresh session starts the phase worker on its issue's current
# phase task and the root architect on its catch-up
# (packages/daemon/internal/daemon/claim_compaction_live_test.go, which this script builds with
# `-tags e2e`, and packages/daemon/internal/daemon/localruntime_live_test.go, the runtime that
# starts a pane with no tmux). Each check prints `== <name>`, what it observed and `ok <name>`; the
# first that fails ends the run non-zero, naming it.
#
# The whole run took 186 s measured in a Legion worker pod (omp/18.16.0) with a warm Go build
# cache, the driver 138 s of it; a cold cache adds the driver's own build. Four panes each start
# Oh My Pi once, and each of the three phase workers answers the task its phase's start left
# pending on its claim — which is what keeps admission's repair of a slotted worker off these
# claims, as the driver's seed says —
# and the one wait is the held summary: Oh My Pi writes a split turn's summary in two passes and
# the stand-in holds each for 35 s, so that compaction answers after about 70 s. Nothing else
# waits on a clock: the ladder's own gaps are provider-error-retry.sh's subject and this proof
# never waits one out, since a newer task takes the place of the one a wait stands for.
#
# Run it from a checkout on a Linux host with go, awk, bash, tar and bun, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp \
#     LEGION_TEST_PG_DSN=postgres://<user>@<host>:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/claim-compaction.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's fetch, and the driver prints which build it ran.
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on — CI's
# service, a devbox container and a pod's own embedded PostgreSQL all satisfy it, and this script
# starts none. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN, or the pinned
# one this script installs with `go install` into a GOBIN of its own, started with JetStream on a
# port of the run's own and stopped on any exit. CLAIM_COMPACTION_EVIDENCE_DIR (default a fresh
# directory under TMPDIR, or /tmp when it is unset, kept and printed) keeps the transcript, the
# nats-server's log, the daemon's own and the driver's output; the run's work directory, the driver
# binary among it, is under TMPDIR too, since a pod's own disk is small and its TMPDIR names the
# issue's volume. The driver builds `legion` and packs the two plugins from this checkout itself,
# so it needs bun and the repository's node_modules. It needs no kube context, no Claude Code and
# no tmux — the panes are child process groups of the driver — so it runs in a Legion pod and on a
# devbox alike. The run around the driver is lib/live-daemon-proof.sh's, which
# provider-error-retry.sh and ci-red-no-takeover.sh make too.
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
live_daemon_proof 'claim compaction' claim-compaction \
  TestTheOperatorAndTheControllerCompactAClaimsSessionAndStartItFreshLive \
  "${CLAIM_COMPACTION_EVIDENCE_DIR:-}"
