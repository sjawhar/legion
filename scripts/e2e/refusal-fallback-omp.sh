#!/usr/bin/env bash
# The live proof that a Legion worker a provider refused stays off that model for the whole of the
# plugin's cooldown and goes back to it when the cooldown ends (dispatch://INFRA-956 R1, the check
# c-r1-chain). It is a script of its own because the window it measures is longer than any CI job
# holds: Oh My Pi's own floor for suppressing a model whose error it cannot classify is five
# minutes (packages/coding-agent/src/session/turn-recovery.ts:2029-2037 at the pinned tag), the
# plugin's cooldown here is ten, and ci.yaml's pi-legion job has timeout-minutes: 10, so the only
# way to see that nothing goes back at the five-minute mark and something does after the tenth is
# to spend the quarter of an hour.
#
# Three Legion panes run at the same time, each the root architect of an issue of its own, each one
# real `omp --mode rpc` child of the pinned Oh My Pi with this checkout's @sjawhar/pi-envoy and
# @sjawhar/pi-legion loaded and an OpenAI-shaped model stand-in of its own, under a profile whose
# chain steps from standin/strong-model to standin/weak-model with `retry.fallbackRevertPolicy:
# never`, the shape INFRA's operator overlay carries (deploy/kubernetes/operator-route/overlay.yml),
# and `retry.rateLimitWalksChain: true`, which Oh My Pi has off: on, a rate limit is admitted to the
# chain consult rather than deferred to another attempt on the same model.
# Each pane is driven about once a minute for about twelve and a half minutes — the stand-in answers
# every turn with a `bash sleep`, so one long turn asks again each minute — and the model of each
# request says where that session stood at that minute
# (packages/pi-legion/scripts/refusal-fallback-pane.ts, the driver this script runs with bun). The
# three differ in the one answer that refuses:
#   - filtered-clean: `finish_reason: content_filter` with no content, which Oh My Pi may replay, so
#     the host walks its own chain and the plugin arms the return from the host's own reason;
#   - filtered-after-text: the same after streamed text, which is replay-unsafe, so the host leaves
#     the session where it is and the plugin walks the chain itself;
#   - rate-limited: HTTP 429 with `retry-after: 60`, which is no content filter, so the host notes a
#     suppression of that model for the 60 s the provider asked for, the arming's cause is `other`,
#     and the return waits on that suppression, which is a different clock from the cooldown. Oh My
#     Pi spends its same-model retries first — `retry.maxRetries` is 10 and it waits out a
#     `retry-after` of 60 s each time — so this pane's chain walk is about ten minutes in and its
#     return about two minutes after that, inside the same span.
# What the run holds each pane to: the first request goes to the refused model and the next to the
# chain's next one; the arming the pane recorded has the cause and the due time that fallback calls
# for; no request at all goes to the refused model between the fifth minute and the end of the
# ten-minute cooldown, which is the window a restore of Oh My Pi's own would show in, since five
# minutes is its own floor for a suppression it cannot time; the first request after the cooldown
# goes to that model again; and the rate-limited pane goes back about three minutes after its
# fallback — the 60 s suppression plus the gap between readings — rather than waiting the cooldown
# out.
#
# Each expectation prints `== <name>`, what was measured and `ok <name>`, or `FAIL <name>: <why>` on
# stderr. Unlike a stage proof, a failed expectation does not stop the driver: the twelve minutes
# are already spent and every other measurement is evidence about the same run, so every
# expectation is reported and the run exits non-zero naming the first that failed.
#
# Run it from a checkout on a Linux host with bash and bun, as
#   LEGION_OMP_PATH=/opt/omp/bin/omp bash scripts/e2e/refusal-fallback-omp.sh
# LEGION_OMP_PATH is required and names the Oh My Pi every pane runs, an absolute executable: this
# proof launches real agents, and no pin is guessed for it. In a Legion worker pod that binary is
# the fleet's build rather than the branch's own fetch, and this script prints which build it ran.
# The driver needs this repository's node_modules. A whole run took 798 s measured in a Legion
# worker pod (omp/18.16.0), and a pane that never gets its model back holds one to about eighteen
# minutes. It reaches no network and starts no server of its own: each pane's model gateway, the
# stand-in daemon and the stand-in Envoy listener are loopback servers of the driver's, and the NATS
# address it hands the Envoy extension refuses every connection. REFUSAL_FALLBACK_EVIDENCE_DIR
# (default a fresh directory under TMPDIR, or /tmp when it is unset, kept and printed) holds the
# transcript and a copy of every pane's Oh My Pi transcript; each pane's scratch root is under
# TMPDIR too, since a pod's own disk is small and its TMPDIR names the issue's volume. It needs no
# PostgreSQL, no NATS server, no kube context and no tmux, so it runs in a Legion pod, on a devbox
# and on a runner holding the pinned binary alike.
set -euo pipefail

# The cooldown the panes run on (LEGION_REFUSAL_COOLDOWN_MS) and how long each is driven for: ten
# minutes, and twelve and a half, so the run reads both sides of the cooldown's end.
cooldown_ms=600000
drive_ms=750000

root=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "${LEGION_OMP_PATH:-}" ]; then
  echo "FAIL setup: LEGION_OMP_PATH is required: it names the Oh My Pi every pane of this proof runs (an absolute executable path). This proof launches real agents and guesses no build." >&2
  exit 1
fi
if [ ! -x "$LEGION_OMP_PATH" ]; then
  echo "FAIL setup: LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file" >&2
  exit 1
fi
export LEGION_OMP_PATH

started=$SECONDS
check=setup
check_started=$SECONDS

begin() {
  check=$1
  check_started=$SECONDS
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}

evidence=${REFUSAL_FALLBACK_EVIDENCE_DIR:-}
if [ -z "$evidence" ]; then
  evidence=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-refusal-fallback-evidence.XXXXXXXX")
else
  mkdir -p "$evidence"
fi

# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"

# cleanup reports how the run ended with rc: the script's own status for EXIT, 128 plus the
# signal's number for INT and TERM, so an interrupted run never reports success. The panes are the
# driver's own children and it stops them itself.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  if [ "$rc" -eq 0 ]; then
    echo "refusal fallback e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "refusal fallback e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
command -v bun >/dev/null || fail "bun is not on PATH: the driver is a bun program"
driver=$root/packages/pi-legion/scripts/refusal-fallback-pane.ts
[ -f "$driver" ] || fail "$driver is missing"
[ -d "$root/packages/pi-legion/node_modules/@legion/pi-shared" ] ||
  fail "packages/pi-legion/node_modules holds no @legion/pi-shared: run bun install in this checkout"
[ -d "$root/packages/pi-shared/node_modules" ] ||
  fail "packages/pi-shared/node_modules is missing: run bun install in this checkout"
omp_build=$("$LEGION_OMP_PATH" --version 2>&1) || fail "$LEGION_OMP_PATH --version failed: $omp_build"
note "Oh My Pi ${omp_build%%$'\n'*} at $LEGION_OMP_PATH"
note "cooldown ${cooldown_ms}ms, each pane driven for ${drive_ms}ms"
note "evidence directory $evidence"
pass

# The driver prints a section per expectation of its own, as verification-results.sh's does, so this
# check names the run rather than opening one.
check=runs
status=0
bun "$driver" "$cooldown_ms" "$drive_ms" "$evidence" || status=$?
case "$status" in
  0) ;;
  1) fail "an expectation failed (above)" ;;
  *) fail "the driver stopped before its expectations (status $status)" ;;
esac
