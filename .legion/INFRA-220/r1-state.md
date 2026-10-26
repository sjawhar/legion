# INFRA-220 R1 state, recorded 2026-10-26 18:02Z before the 18:12Z park

The implementer's phase is open: implement.json is not written, because R1 (both devbox proofs at
the code head) has not passed. This file is the state of R1 and the commands given to the devbox
lane; it is a process note, removed with .legion/INFRA-220/ by retro.

## State

- Code head: d053ee2804405d58c8349d99a2031f18411ae0e1. Image (no image input changed since
  8241bd7c): ghcr.io/sjawhar/legion-worker@sha256:1e8ac4e6a9b7bcafff95ff45c7f771d3c6b4f187ae078d2e3b5b9e108a3e944f,
  Worker Image run 40348772700, revision label 8241bd7caddbdda1f9e6de2532a660c916dcb043.
- Runner: Proofs2229, the INFRA lead's lane, session 01a13c57-d826-7242-80fe-5f0670188b11 (named
  by the controller, 16:05Z).
- Anthropic key: broker LEGION_ANTHROPIC_API_KEY, live since ~16:55Z (ADC, session
  01a16ba6-2ee0-73c2-8221-d239fda0ef76; theorem-labs/agent-infra#421), owner shared, agent tier.
- Review App key GH_REVIEW_APP_PRIVATE_KEY_B64 (App 5341243): human tier since 16:22Z, one 2 h
  grant per proof, approved by Sami; no agent approves it. Implement App 5341240:
  LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64, agent tier. Both through LEGION_E2E_APP_KEY_RUNNER=$(mise which agent-secrets).
- gcloud: Sami's login on the devbox works since 16:17:22Z; the sandbox run's runtime identity is
  the operator kubeconfig and context (no restricted token may be minted out of band).
- Postgres tests: the 13 TestAClaude* passed on the devbox at b2bf8bb2 (14:45-14:46Z, exit 0, no SKIP).
- Runs so far, all with the transcript on INFRA-220:
  - 8241bd7c negative controls: both as wanted (infra220-8241bd7c-negctl1-tmux-transcript-log,
    infra220-8241bd7c-negctl2-sandbox-transcript-log).
  - 8241bd7c tmux: FAIL at claude-implementer-handoff, a spent gh rate limit read as a missing
    commit (fixed in 08138f1e, smoke_commit_files). infra220-8241bd7c-tmux-transcript-log.
  - 08138f1e tmux: FAIL at stop-hook-reminds-the-open-phase; the implementer completed its phase
    from the daemon's task before the stall instruction (fixed in d053ee28: the proof's
    instructions name the phase task, and the check fails by name on that order).
    infra220-08138f1e-tmux-transcript-log.
  - 08138f1e sandbox: running at 17:58Z; result not yet received.
- Next: Proofs2229 runs both proofs at d053ee28, tmux first, then the sandbox proof; the
  implementer fixes any harness or daemon defect in #2186, and completes only when both pass.

## The commands given to the lane (updated to d053ee28)

# INFRA-220 R1: the two devbox proofs at sjawhar/legion#2186

Head: d053ee2804405d58c8349d99a2031f18411ae0e1 (branch legion/INFRA-220). Run from a clean checkout
at exactly that commit, the tmux proof first and then the sandbox proof, one at a time. Print no key.

## 0. Shared inputs (one shell)

```sh
cd <clean checkout at d053ee28>
export LEGION_E2E_APP_KEY_RUNNER=$(mise which agent-secrets)   # any runner the devbox runs; under bridge it runs on the devbox, never in the daemon's container, so it need not be static
export LEGION_E2E_IMPLEMENT_APP_ID=5341240 LEGION_E2E_IMPLEMENT_APP_KEY_NAME=LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64
export LEGION_E2E_REVIEW_APP_ID=5341243   LEGION_E2E_REVIEW_APP_KEY_NAME=GH_REVIEW_APP_PRIVATE_KEY_B64
export LEGION_E2E_MODEL_ROUTE=key-file
# The Anthropic key: ADC's broker declaration, read once into a 0600 file on tmpfs, removed after both runs.
# LEGION_ANTHROPIC_API_KEY is the name ADC declares. Record this use on INFRA-220 (which key, when).
key=$XDG_RUNTIME_DIR/infra220-anthropic-key
"$LEGION_E2E_APP_KEY_RUNNER" LEGION_ANTHROPIC_API_KEY --reason 'INFRA-220 live proofs' -- \
  sh -c 'umask 077; printf %s "$LEGION_ANTHROPIC_API_KEY" > "$1"' sh "$key"
export LEGION_E2E_CLAUDE_KEY_FILE=$key LEGION_E2E_ANTHROPIC_KEY_FILE=$key
```

## 1. Negative controls (each: the FAIL line, exit 1, nothing created)

```sh
env -u LEGION_E2E_MODEL_ROUTE CLAUDE_E2E_EVIDENCE_DIR=$(mktemp -d) bash scripts/e2e/claude-backend-tmux.sh; echo exit=$?
# want: FAIL prerequisites: LEGION_E2E_MODEL_ROUTE must be gateway or key-file, not ''
<the sandbox command of step 3 with LEGION_E2E_POD_REACHABILITY=nope>; echo exit=$?
# want: FAIL prerequisites: LEGION_E2E_POD_REACHABILITY must be aws-private-ip or bridge, not 'nope': …
```

## 2. The tmux proof

```sh
CLAUDE_E2E_EVIDENCE_DIR=<fresh dir, not existing yet> bash scripts/e2e/claude-backend-tmux.sh; echo exit=$?
```

Want `claude backend e2e: PASS`, exit 0, and the prerequisites note `GET /app answers each App's
configured id: {"implement":"legion-implementer[bot]","review":"legion-reviewer[bot]"}`.

## 3. The sandbox proof

```sh
LEGION_E2E_OPERATOR_CONTEXT=<operator GKE context> \
LEGION_E2E_RUNTIME_KUBECONFIG=<the operator kubeconfig> \
LEGION_E2E_RUNTIME_CONTEXT=<the same operator context> \
LEGION_E2E_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:1e8ac4e6a9b7bcafff95ff45c7f771d3c6b4f187ae078d2e3b5b9e108a3e944f \
LEGION_E2E_STORAGE_CLASS=standard-rwo \
LEGION_E2E_CLAUDE_MODEL=claude-sonnet-5 \
LEGION_E2E_POD_REACHABILITY=bridge \
LEGION_E2E_DAEMON_PORT=13380 LEGION_E2E_WORKER_STREAM_PORT=13381 LEGION_E2E_DISPATCH_PORT=13382 \
LEGION_E2E_ENVOY_PORT=13383 LEGION_E2E_NATS_PORT=13384 LEGION_E2E_SESSION_DB_PORT=13385 \
CLAUDE_SANDBOX_E2E_EVIDENCE_DIR=<fresh dir> \
  bash scripts/e2e/claude-backend-sandbox.sh; echo exit=$?
```

Check the six ports are free first (`ss -ltn`). If the auth plugin needs a path beyond its binary
and the variables passed by name, add `LEGION_E2E_DAEMON_CONTAINER_MOUNTS=<abs path>[:<abs path>…]`.
The probe notes print what the daemon's container sees. Under bridge, prerequisites resolves both
App keys on the devbox into owner-only files under `$XDG_RUNTIME_DIR` (set, the user's own, 0700,
a tmpfs), so the review key's grant is asked for there; cleanup removes the files.

Want `claude backend sandbox e2e: PASS`, exit 0 after cleanup; the planner's time printed beside its
2700 s budget; the table row and the restored prefix asserted at the pod deletion; the architect's and
planner's new generations ready in the new pod; namespace-before.txt and namespace-after.txt
identical. The runtime identity is the operator's: the run proves the runtime, the Claude lane and the
session table on the real cluster, not that the daemon's restricted RBAC is enough.

## 4. After both runs

```sh
rm -f "$key"
```

For each run, upload transcript.log (and, for the sandbox run, namespace-before.txt and
namespace-after.txt) to INFRA-220 with `dispatch artifact`, and send the implementer (session
01a17575-698b-7763-8976-425ab8792b84) the exit status, the PASS or FAIL line and the artifact
references. On a failure, also send the last 40 lines of the transcript and of logs/daemon.log.
