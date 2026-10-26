#!/usr/bin/env bash
# The Claude Code backend's live proof under Legion's Kubernetes runtime, on a real cluster. A real
# Go daemon on the devbox, whose legion.yaml puts the implementer on Claude Code
# (`worker_backends: {implementer: claude}`) and whose runtime is `kubernetes`, drives one scratch
# Dispatch issue: a real Oh My Pi architect and planner take it to implementing in the issue's pod,
# and the implementer then runs in that pod's one `roles` container, in namespaces of its own
# (`legion launcher role-init`), as `legion claude-bridge -- claude -p …`
# — the Claude Code its pod's start script installed at launch from the image's pin (the image
# carries the pin and no Claude Code), the image's `@sjawhar/claude-envoy` plugin as its MCP server
# and its Legion plugin, its `mcp.json` written by the launcher into the generation's private
# directory, and its `CLAUDE_CONFIG_DIR` a directory of the role's state home on the pod's own disk,
# which dies with the pod (docs/kubernetes.md, "The disk dies with its pod").
# The proof holds, in this order, that the Claude Code implementer's pod launches and its claim
# registers that pod's Claude Code transcript as its session; that the implementer's command line
# holds `claude-bridge --` and `--session-id <id>`, and runs the Claude Code the generation's start
# script installed into its own directory on the pod's disk — the pinned version, with no
# /usr/local/bin/claude in the image; that the endless phase reminder reaches it when a turn
# of its ends with its phase still open; that it completes its phase through the `legion` MCP tool's
# handoff_complete with a pushed `.legion/<issue>/` handoff commit; and that, the pod deleted and
# its disk with it, the transcript is a row of the run's session table (omp_session_files, where the
# pod's launcher saved it: docs/kubernetes.md, "Where a Claude Code session lives across pods") whose
# earlier bytes are unchanged, the replacement restores it from that row, installs its own
# generation's copy and resumes the same Claude Code session (`--resume <id>`, the restored file's
# earlier bytes unchanged, and an earlier marker quoted back out of its context), and the same pod's
# architect and planner come back in the replacement at a new generation and register there.
#
# It is the tmux proof's rig (scripts/e2e/claude-backend-tmux.sh) on the cluster proof's cluster
# (scripts/e2e/stage4a-sandbox-runtime.sh): scratch Postgres, NATS, Envoy listener and Dispatch from
# lib/rig.sh, `admission_cap: 1`, `gates.design: off`, the smoke repository sjawhar/legion-smoke
# where the implementer pushes a branch and opens no pull request — and, for the runtime, an
# existing cluster named by LEGION_E2E_RUNTIME_CONTEXT, the namespace `legion`, LEGION_E2E_IMAGE
# pinned by digest, and a scratch session database of the run's own (`session_store: postgres`),
# which every pod of the run keeps its sessions in, the Claude Code implementer's transcript among
# them. It is the first rig that runs `runtime: kubernetes`, so what a tmux proof reads from /proc
# and a pane's own files this one reads with `kubectl exec` and `kubectl get` against the issue's
# pod — a role's processes and its own view of the pod inside that role's namespaces of the `roles`
# container (lib/role-namespaces.sh), the files of the pod's disk in the container itself — and the
# session table with `psql` in the session database's container.
#
# It pins the branch's own published worker image by digest. A pull-request run of
# .github/workflows/worker-image.yaml publishes `ghcr.io/sjawhar/legion-worker:sha-<the first 12 hex
# of the built commit>` and records that tag's digest, so the image must be built from the branch
# under test — the branch carrying this repository's Claude Code lane, the Claude Code pin and the
# `@sjawhar/claude-envoy` plugin in the image — before this proof can run, and LEGION_E2E_IMAGE
# names that published image by its digest, never by a tag. Iteration may run the same script body
# against a throwaway kind cluster with Agent Sandbox and gVisor (lib/kind-sandbox.sh, the pattern
# of burstable-kind.sh, controller-sweep-kind.sh and share-volumes-kind.sh), which loads the same
# image by digest into the node's containerd; the recorded run is the one against the cluster.
#
# Devbox only, and one run at a time. LEGION_E2E_POD_REACHABILITY picks how the pods reach the
# run's six services on the devbox, as Stage 4a's does:
#   - `aws-private-ip`: the pods reach the devbox at its private address (read from instance
#     metadata), so the six services bind that address on six ports the devbox's security group
#     admits from the Legion nodes, which is all six that range holds (the README's port-admission
#     note).
#   - `bridge`: for a cluster whose pods cannot reach the devbox at all (no tailnet, no routable
#     private address — GKE's shape). The six services bind the devbox's loopback, and
#     lib/bridge-tunnel.sh stands up a scratch pod+Service of the run's own in the namespace
#     (bridge-<the run's label>) whose reverse tunnel forwards each of the six ports back to it;
#     the pods dial the Service's cluster-DNS name. The daemon and the pods read one legion.yaml,
#     so its URLs name that Service, which the devbox cannot resolve: the daemon alone runs in a
#     container on the devbox's own network (`docker run --network host --add-host <that
#     name>:127.0.0.1`), a static build of it in a minimal image pinned by digest, as the devbox
#     user, with every file legion.yaml names mounted at its own path and none of the caller's
#     environment but what the runtime kubeconfig's auth plugin and the App key runner read, passed
#     by name. Production runs this daemon in a pod, so a static binary in a container is close to
#     how operators run it. Prerequisites proves that container before the run creates anything.
#     This mode needs docker, which both modes need anyway, and no root.
# Nothing is defaulted: every port, the reachability, the cluster, the image and the model route are
# the operator's to name, and an unset one is refused by name before the run creates anything.
#
# Inputs:
# - LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
#   ~/.kube/legion-daemon-production) name the restricted identity the daemon's runtime runs as;
#   LEGION_E2E_OPERATOR_CONTEXT (default production) the admin context this script's own reads of
#   the namespace, the pods and the teardown use.
# - LEGION_E2E_IMAGE (required) is the branch's worker image, by digest (…@sha256:…); a tag is
#   refused.
# - LEGION_E2E_STORAGE_CLASS (required) is the StorageClass of the issue pod's disk, the pod's own
#   generic ephemeral volume (runtime.kubernetes.storage_class): the cluster has no default to fall
#   back on.
# - LEGION_E2E_CLAUDE_KEY_FILE (required) names a file holding the Anthropic API key the Claude Code
#   implementer reaches its model with. The run copies it once (0600) and creates the providers
#   Secret the runtime names for the project with it, which `provider_keys: {ANTHROPIC_API_KEY:
#   ANTHROPIC_API_KEY}` hands the pod: the key travels as a mounted Secret the shim exports into the
#   agent's environment, never in the pod's spec, since runtime.kubernetes.pod.env refuses a
#   credential-shaped name.
# - LEGION_E2E_CLAUDE_MODEL (required) is the model id that pod's Claude Code runs, carried as the
#   non-credential ANTHROPIC_MODEL in runtime.kubernetes.pod.env.
# - LEGION_E2E_CLAUDE_BASE_URL (optional) is the Anthropic endpoint it calls, carried as the
#   non-credential ANTHROPIC_BASE_URL in the same pod.env, for a devbox whose route to Anthropic is
#   a gateway. Unset, the pod calls Anthropic's own endpoint, which is where the key's preflight
#   reads too. Whichever it is, the key in LEGION_E2E_CLAUDE_KEY_FILE must be one that endpoint
#   accepts; the preflight below holds it to that before anything is created.
# - LEGION_E2E_MODEL_ROUTE (required, `gateway` or `key-file`) is the route the pod's *Oh My Pi*
#   roles take, the architect and the planner, which is the operator route's own
#   (deploy/kubernetes/operator-route) as Stage 4a takes it: under `gateway`,
#   {pod,models}.yml with LEGION_E2E_MODEL_GATEWAY_URL (required) as the models' baseUrl and
#   LEGION_E2E_MODEL_GATEWAY_AUDIENCE (required) as the audience of the projected ServiceAccount
#   token that keys it; under `key-file`, {pod,models}-key-file.yml, which carry no projected token
#   and read the same providers Secret's ANTHROPIC_API_KEY that the Claude lane uses.
# - LEGION_E2E_POD_REACHABILITY (required, `aws-private-ip` or `bridge`, above) is how the pods
#   reach the devbox's services.
# - LEGION_E2E_DAEMON_CONTAINER_MOUNTS (optional, bridge only) is a colon-separated list of absolute
#   host paths the daemon's container mounts read-only at their own paths, for what the runtime
#   kubeconfig's auth plugin or the App key runner reads beyond its own binary (a gcloud
#   configuration directory, say). An entry that is not an existing absolute path, or that holds a
#   comma, is refused by name with the other inputs.
# - LEGION_E2E_DAEMON_PORT, LEGION_E2E_WORKER_STREAM_PORT, LEGION_E2E_DISPATCH_PORT,
#   LEGION_E2E_ENVOY_PORT, LEGION_E2E_NATS_PORT and LEGION_E2E_SESSION_DB_PORT (all required, all
#   distinct, in both modes) are the ports the daemon's API, its worker stream, the scratch
#   Dispatch, the scratch Envoy listener, the scratch NATS and the scratch session database bind on
#   the devbox: its private address under aws-private-ip, its loopback under bridge. Every one of
#   the six is dialled from inside a pod — the launchers dial the worker stream, each agent's claim
#   registration dials LEGION_DAEMON_URL, each agent dials DISPATCH_URL, ENVOY_URL and
#   ENVOY_NATS_URL, and every Oh My Pi agent and the Claude Code implementer's launcher keep their
#   sessions in the session database — so each must be, under aws-private-ip, a port the
#   devbox's security group admits from the Legion nodes, and under bridge a free port on the
#   devbox, which the bridge forwards. There is no default: a default would pick a port another
#   lane holds, and the repository carries no deployment's own.
# - LEGION_E2E_IMPLEMENT_APP_ID, LEGION_E2E_IMPLEMENT_APP_KEY_NAME, LEGION_E2E_REVIEW_APP_ID and
#   LEGION_E2E_REVIEW_APP_KEY_NAME (optional) name the two role Apps the run acts as: each App's
#   numeric id and the secret-store key the `secrets` CLI resolves to its private key
#   (lib/github-apps.sh). Each defaults to this deployment's own, so a run here sets none.
# - CLAUDE_SANDBOX_E2E_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) holds the
#   transcript of the run, the daemon's log, the legion.yaml and pod fixture the run wrote, the
#   implementer's Claude Code transcript as the session table holds it, and the namespace snapshots.
#
# Legion's role Apps resolve as the tmux proof's do (lib/github-apps.sh): the daemon runs each
# App's private_key_command, `secrets <key name> -- …`, never this shell, and prerequisites proves
# each key is its id's App against GitHub's GET /app (lib/app-logins.ts) before the run writes
# anything; legion.yaml's github_apps is the block proved. The proof human is the devbox's ordinary
# gh acting as sjawhar-agent[bot] (require_proof_human), so run it from the operator's own Oh My Pi
# session, not a Legion pane, with no personal GH_TOKEN in its environment.
#
# Everything the run creates in the namespace carries its own project label (legion.dev/project,
# the run's Dispatch project lowercased, which is the daemon's own claim token for it), and
# lib/namespace-rig.sh owns it: the teardown deletes everything labelled with that exact label —
# never by label existence — and namespace-clean compares the run's own objects with the snapshot
# taken before it, so another system's objects in the shared namespace cannot fail it. The bridge's
# own pod, Service and auth Secret (bridge mode only) carry the same label and are torn down by
# lib/bridge-tunnel.sh before that comparison runs. Nothing outside `legion` is touched.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2eccs.$$.XXXXXXXX")
evidence=${CLAUDE_SANDBOX_E2E_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2eccs-evidence.XXXXXXXX)}
mkdir -p "$evidence/logs" "$evidence/transcripts"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# fd 7 keeps the transcript for cleanup: a signal runs the EXIT trap under the redirections of the
# command it interrupted, whose output may be /dev/null or an evidence file.
exec 7>&1

ok=
check=setup
# The run's Dispatch project, and from it the namespace label every object of the run carries: the
# daemon's own claim token for the project (claim.ProjectToken), which for a key of [A-Z][A-Z0-9]*
# is the key lowercased.
project="CCS$(( ($$ + $(date +%s)) % 10000000 ))"
project=${project:0:10}
run_label=${project,,}
label_exact=$run_label
namespace=legion
repo="sjawhar/legion-smoke"
state="$work/state"
record="$work/sandboxes"
operator=${LEGION_E2E_OPERATOR_CONTEXT:-production}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-$HOME/.kube/legion-daemon-production}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
image=${LEGION_E2E_IMAGE:-}
storage_class=${LEGION_E2E_STORAGE_CLASS:-}
pod_reachability=${LEGION_E2E_POD_REACHABILITY:-}
model_route=${LEGION_E2E_MODEL_ROUTE:-}
claude_key_file=${LEGION_E2E_CLAUDE_KEY_FILE:-}
claude_model=${LEGION_E2E_CLAUDE_MODEL:-}
claude_base_url=${LEGION_E2E_CLAUDE_BASE_URL:-}
port_daemon=${LEGION_E2E_DAEMON_PORT:-}
port_worker_stream=${LEGION_E2E_WORKER_STREAM_PORT:-}
port_dispatch=${LEGION_E2E_DISPATCH_PORT:-}
port_listener=${LEGION_E2E_ENVOY_PORT:-}
port_nats=${LEGION_E2E_NATS_PORT:-}
port_session_db=${LEGION_E2E_SESSION_DB_PORT:-}
daemon_container_mounts=${LEGION_E2E_DAEMON_CONTAINER_MOUNTS:-}
operator_route=$root/deploy/kubernetes/operator-route
route_configmap=legion-operator-route-$run_label
providers_secret=legion-$run_label-providers
# bridge_name is deterministic from the run's label alone (lib/bridge-tunnel.sh's pod and Service),
# so the cluster-DNS host pods dial under LEGION_E2E_POD_REACHABILITY=bridge is known before the
# bridge itself is ever stood up.
bridge_name=bridge-$run_label
pg_container="legion-e2eccs-pg-$$"
nats_container="legion-e2eccs-nats-$$"
# The run's session database: its container, the image (the pinned Postgres the repository's CI
# tests against, as Stage 4a's and Stage 4b's), and the providers Secret key its URL reaches every
# pod under (runtime.kubernetes.session_dsn_secret).
session_db_container="legion-e2eccs-db-$$"
session_db_image=pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a
session_dsn_key=SESSION_DSN
daemon_log=$evidence/logs/daemon.log
# The daemon's container under bridge (daemon_run_args): its name, whose prefix and trailing pid
# refuse_leftovers reads as it reads the other containers', and its image, busybox pinned by digest
# and pulled through the mirror (AGENTS.md). The daemon is a static binary and needs nothing of an
# image; the image is there for the `sh`, `tr` and `base64` each App's private_key_command runs
# (lib/github-apps.sh's app_key_command).
daemon_container="legion-e2eccs-daemon-$$"
daemon_image=mirror.gcr.io/library/busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
# How long a planner the driver has told to plan gets to finish, as Stage 4b's plan_seconds: before
# its handoff it runs its two model-backed plan checks, the gap analyst and up to three plan-review
# rounds (each a subagent call), so planning takes a whole loop of its own (wait_for_phase). The
# route is the operator route's own, so the planner runs the models production runs.
plan_seconds=2700
# host is the name the pods dial the run's six services by, and bind_host the address those
# services bind and the devbox itself dials them on, both set in prerequisites: under aws-private-ip
# both are the devbox's private address, read from instance metadata; under bridge host is the
# bridge Service's cluster-DNS name and bind_host loopback. legion.yaml carries host alone — under
# bridge the daemon reads it through its container's /etc/hosts (daemon_run_args) — and
# pod_dispatch_url is Dispatch at host, as the pods are handed it.
host=
bind_host=
pod_dispatch_url=
gateway=
gateway_audience=
daemon_pid=
dispatch_pid=
listener_pid=
port_pg=
dispatch_base=
# The proof human's writes carry no session actor: the scratch Dispatch reads the human header.
dispatch_actor=
proof_human_user=smoke
issue=
pr_number=
smoke_file=
transcript=
stall_offset=
timeout_hook=
snapshotted=
torn_down=
compared=

begin() { check=$1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/namespace-rig.sh
. "$root/scripts/e2e/lib/namespace-rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/bridge-tunnel.sh
. "$root/scripts/e2e/lib/bridge-tunnel.sh"
# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"
# shellcheck source-path=SCRIPTDIR source=lib/session-db.sh
. "$root/scripts/e2e/lib/session-db.sh"
# shellcheck source-path=SCRIPTDIR source=lib/role-namespaces.sh
. "$root/scripts/e2e/lib/role-namespaces.sh"
# Nothing the run starts inherits the operator's production Dispatch bearer.
unset DISPATCH_TOKEN

# ---- the issue's pod ------------------------------------------------------------------------------

# claim_view ISSUE ROLE prints the claim as the daemon's state projects it, and claim_sandbox its
# Sandbox, whose one pod carries that name.
claim_view() {
  daemon_state | jq -ce --arg issue "$1" --arg role "$2" \
    'if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end'
}
claim_sandbox() { claim_view "$1" "$2" | jq -er '.locator.sandbox.name'; }
claim_pod_uid() { claim_view "$1" "$2" | jq -er '.locator.sandbox.podUid'; }
# pod_exec POD ROLE COMMAND… runs COMMAND as ROLE's agent sees POD, as stage4b-sandbox-tree.sh's
# does (role_enter_sh): an issue pod has one container, `roles`, whose launcher runs every role in
# namespaces of its own, so COMMAND enters ROLE's role-init's namespaces and then its worker-shim's
# user namespace, and sees ROLE's /proc (its own processes alone), its private directory and the
# image's paths. It needs ROLE's generation running.
pod_exec() {
  local pod=$1 role=$2
  shift 2
  op exec "$pod" -c roles -- sh -c "$role_enter_sh" role-enter "$role" "$@"
}
# roles_exec POD COMMAND… runs COMMAND in POD's `roles` container itself, outside every role's
# namespaces: the pod's disk at /legion, which the container and every role see at the same path,
# so a file of a role's state home reads there whether or not the role's generation runs.
roles_exec() {
  local pod=$1
  shift
  op exec "$pod" -c roles -- "$@"
}
# implementer_pod prints the pod the implementer's claim names now, which is its Sandbox's own name
# and so the same word across the deletion step: a replacement pod is a new uid under that name,
# which is why every check of the replacement compares uids and never names.
implementer_pod() { claim_sandbox "$issue" implementer; }
# agent_pid_sh is the POSIX sh that finds a role's agent in its namespaces (pod_exec), Oh My Pi's or
# Claude Code's, by its direct parent alone, as stage4b-sandbox-tree.sh's does: an omp whose parent
# is the role's `legion worker-shim`, or a claude whose parent is the `legion claude-bridge` the shim
# runs, each parent known by its argv[1], never by a later argv word naming it. A process further
# down is never the agent: Oh My Pi starts its own helpers under its own name
# (`omp __omp_worker_daemon_broker`, a child of the architect's omp), which a walk up to the shim
# would count as a second agent. It exits 1 naming both pids when two such processes run and saying
# so when none does; otherwise it leaves the pid in $pid and the /proc it read in $proc:
# `${proc_root:-/proc}`, the role's own unless a test points proc_root at a fake tree.
# shellcheck disable=SC2016  # expanded by the pod's shell
agent_pid_sh='
    proc=${proc_root:-/proc}
    found=
    for d in "$proc"/[0-9]*; do
      argv0=$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 1p)
      case "${argv0##*/}" in
        omp) runner=worker-shim ;;
        claude) runner=claude-bridge ;;
        *) continue ;;
      esac
      parent=$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)
      [ -n "$parent" ] || continue
      [ "$(tr "\0" "\n" 2>/dev/null <"$proc/$parent/cmdline" | sed -n 2p)" = "$runner" ] || continue
      if [ -n "$found" ]; then
        echo "more than one agent under a legion worker-shim: pids $found and ${d##*/}" >&2
        exit 1
      fi
      found=${d##*/}
    done
    if [ -z "$found" ]; then
      echo "no Oh My Pi or Claude Code under a legion worker-shim" >&2
      exit 1
    fi
    pid=$found'
# claude_pid_sh is agent_pid_sh for the Claude Code lane alone, and it names the bridge too: $pid is
# the role's Claude Code and $bridge its parent, the `legion claude-bridge` (argv[1] claude-bridge)
# the launcher's agent command runs it from, both read from $proc. A role whose agent is not Claude
# Code under the bridge exits 1.
# shellcheck disable=SC2016  # expanded by the pod's shell
claude_pid_sh='
    proc=${proc_root:-/proc}
    found=
    for d in "$proc"/[0-9]*; do
      argv0=$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 1p)
      [ "${argv0##*/}" = claude ] || continue
      up=$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)
      [ -n "$up" ] || continue
      [ "$(tr "\0" "\n" 2>/dev/null <"$proc/$up/cmdline" | sed -n 2p)" = claude-bridge ] || continue
      if [ -n "$found" ]; then
        echo "more than one Claude Code under legion claude-bridge: pids $found and ${d##*/}" >&2
        exit 1
      fi
      found=${d##*/}
      bridge=$up
    done
    if [ -z "$found" ]; then
      echo "no Claude Code under legion claude-bridge" >&2
      exit 1
    fi
    pid=$found'
# pod_env POD ROLE prints the environment of ROLE's agent in POD, one NAME=value a line, read from
# the /proc of the role's namespaces. The container's spec is not where a role's environment is: it
# carries only what the kubelet resolves, and every other variable reaches the role in its
# launcher's start command.
pod_env() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$1" "$2" sh -c "$agent_pid_sh"'
    tr "\0" "\n" <"$proc/$pid/environ"'
}
# claude_argv POD WHICH prints, one argument a line, the argv of the implementer's Claude Code
# (`agent`) or of the `legion claude-bridge` that runs it (`bridge`), read in the implementer's
# namespaces.
claude_argv() {
  local which=$2
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$1" implementer sh -c "$claude_pid_sh"'
    case "$1" in bridge) read_from=$bridge ;; *) read_from=$pid ;; esac
    tr "\0" "\n" <"$proc/$read_from/cmdline"' sh "$which"
}
# claude_code_root is where the implementer's start script installs Claude Code, one directory per
# generation, under its state home on the pod's disk (claudeCodeRoot,
# packages/daemon/internal/runtime/sandbox/plugins.go), and claude_code_pin the pin in the image it
# installs from (claudeCodePin, names.go). claude_code_binary GENERATION prints the binary that
# generation's install leaves and its agent command names (claudeCodeBinary): the linux-x64
# package's own executable, never the npm package's placeholder `bin`.
claude_code_root=/legion/scratch/state/implementer/legion-claude-code
claude_code_pin=/opt/legion/claude-code/package.json
claude_code_binary() { printf '%s/g%s/node_modules/@anthropic-ai/claude-code-linux-x64/claude\n' "$claude_code_root" "$1"; }
# argv_has_pair ARGV FLAG VALUE: ARGV (one argument per line) holds FLAG immediately followed by VALUE.
argv_has_pair() { awk -v flag="$2" -v value="$3" 'prev == flag && $0 == value { found = 1 } { prev = $0 } END { exit !found }' <<<"$1"; }
# argv_value ARGV FLAG prints the argument after FLAG, or nothing when ARGV does not carry it.
argv_value() { awk -v flag="$2" 'prev == flag { print; exit } { prev = $0 }' <<<"$1"; }

# ---- the runtime seam: a claim's session is a row of the session table, its workspace in the pod -

claim_session_file() {
  claims_list | jq -er --arg issue "$1" --arg role "$2" \
    '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}
# claim_generation ISSUE ROLE prints the claim's generation as `legion claims list` carries it: the
# supervisor's count of the claim's launches, one more for every try, written before the try starts.
# The daemon state's ClaimView carries none, which is why it is read here and not from claim_view.
claim_generation() {
  claims_list | jq -er --arg token "$(claim_token "$1" "$2")" '.claims[] | select(.token == $token) | .generation'
}
# claim_relaunched ISSUE ROLE GENERATION: the claim's generation has passed GENERATION, so the
# supervisor has started a later try of it.
claim_relaunched() { [ "$(claim_generation "$1" "$2")" -gt "$3" ]; }
# claim_session_text ISSUE ROLE prints the claim's session whole, as the session database holds it:
# an Oh My Pi role keeps no session file in its pod (session_store postgres), and the session file
# its claim records is the session's key in the table. The Claude Code implementer's transcript is
# a file in the pod too, which the checks of its own read there (transcript_since, below).
claim_session_text() {
  local file
  file=$(claim_session_file "$1" "$2") || return 1
  session_text "$file"
}
workspace_jj() {
  local issue=$1 pod
  shift
  pod=$(claim_sandbox "$issue" architect) || return 1
  pod_exec "$pod" architect jj -R "/legion/workspaces/$repo/${issue,,}" "$@"
}
# assert_claim_endpoints ISSUE ROLE: the claim's agent names the run's own scratch services and the
# run's own daemon, so a turn that reached another rig — production Dispatch, another run's
# listener — is never read as this proof's.
assert_claim_endpoints() {
  local issue=$1 role=$2 pod mismatch
  pod=$(claim_sandbox "$issue" "$role") || fail "$role on $issue has no Sandbox locator"
  if mismatch=$(pod_endpoint_mismatch "$pod" "$role"); then
    fail "ABORT: $role pod $pod on $issue has $mismatch"
  fi
}
# pod_endpoint_mismatch POD ROLE prints the first endpoint ROLE's agent does not carry, and nothing
# when it carries every one of them.
pod_endpoint_mismatch() {
  local pod=$1 role=$2 env name want got
  env=$(pod_env "$pod" "$role" 2>&1) || {
    printf 'no readable agent environment in its %s namespaces: %s\n' "$role" "$(tr '\n' ' ' <<<"$env" | cut -c1-300)"
    return 0
  }
  for name in DISPATCH_URL ENVOY_URL ENVOY_NATS_URL LEGION_DAEMON_URL; do
    case "$name" in
      DISPATCH_URL) want="http://$host:$port_dispatch" ;;
      ENVOY_URL) want="http://$host:$port_listener" ;;
      ENVOY_NATS_URL) want="nats://$host:$port_nats" ;;
      LEGION_DAEMON_URL) want="http://$host:$port_daemon" ;;
    esac
    got=$(sed -n "s/^$name=//p" <<<"$env")
    if [ "$got" != "$want" ]; then
      printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"
      return 0
    fi
  done
  return 1
}

# ---- the session table: every pod's sessions are rows of the run's session database -------------

# Every pod of the run keeps its sessions in the run's own scratch Postgres
# (runtime.kubernetes.session_store: postgres), in Oh My Pi's table omp_session_files and its parts
# omp_session_files_parts, keyed by the path the session's file would have had
# (internal/ompsessions). Each Oh My Pi role's session is only there; the Claude Code implementer's
# transcript and its phase reminder's record are files in its pod that the role's launcher saves
# there as rows keyed by their absolute paths, and restores from there in a replacement pod before
# Claude Code starts (internal/launcher's claude.go; docs/kubernetes.md, "Where a Claude Code
# session lives across pods"). Every read goes through the database's own container
# (session_db_container) with lib/session-db.sh's readers, with no client on the devbox, as Stage
# 4b's do.

# pod_uid_gone POD UID: no pod named POD has uid UID any more — it is gone, or a replacement holds
# its name. A read the API server refuses is not an answer, so it is never taken for gone.
pod_uid_gone() {
  local uid
  uid=$(op get pod "$1" --ignore-not-found -o jsonpath='{.metadata.uid}') || return 1
  [ "$uid" != "$2" ]
}

# ---- the implementer's transcript, as the pod holds it -------------------------------------------

# transcript_size prints the transcript's size in bytes, and transcript_prefix_sha SIZE the sha256
# of its first SIZE bytes: the resume's own before-and-after in the pod, computed there, since the
# file is on the pod's own disk and no host of the run can open it. The pod's disk is mounted at
# /legion in the `roles` container and seen at that same path in the implementer's namespaces, so
# the transcript's path is read in the container itself (roles_exec), as stage4b-sandbox-tree.sh
# reads a role's files, whether or not the implementer's generation runs then. The table's copy of
# the same bytes is read from the devbox (session_text).
transcript_size() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  roles_exec "$(implementer_pod)" sh -c 'wc -c <"$1"' sh "$transcript" | tr -d '[:space:]'
}
transcript_prefix_sha() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  roles_exec "$(implementer_pod)" sh -c 'head -c "$1" -- "$2" | sha256sum' sh "$1" "$transcript" |
    cut -d' ' -f1
}
# transcript_since OFFSET prints the transcript's bytes past OFFSET, the tail every predicate below
# judges: a poll reads the new entries alone, not a session that grows all run.
transcript_since() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  roles_exec "$(implementer_pod)" sh -c 'tail -c "+$(($1 + 1))" -- "$2"' sh "$1" "$transcript"
}
# The opening of what a worker whose turn ended with its phase still open is told (REMINDER in
# packages/pi-shared/src/phase-stall.ts), which the pod's `legion` MCP server answers its `Stop`
# hook with. A fragment of that paragraph, not the whole of it, so a later rewording of the rest
# does not break this proof silently; this sentence is in no other message a worker is given.
stall_reminder_needle="Your turn ended with your Legion phase still open"
# claude_entries OFFSET prints one line per transcript entry written past OFFSET bytes: `assistant`
# for an assistant message, and `user <its text>` for a user one — its content, a string or the text
# of its blocks, every run of whitespace squeezed to one space. One line is one entry, so the
# predicates below decide on one message rather than on a line of one.
claude_entries() {
  transcript_since "$1" | jq -R -r '
    fromjson?
    | if .type == "assistant" then "assistant"
      elif .type == "user" then
        "user " + ((.message.content // "")
          | if type == "string" then . else [.[]? | select(.type == "text") | .text] | join(" ") end
          | gsub("\\s+"; " "))
      else empty end'
}
# stop_hook_reminded OFFSET: past OFFSET bytes the session holds Claude Code's own `Stop hook
# feedback`, carrying the reminder — the `{"decision":"block","reason":…}` the pod's legion MCP
# server answered the `Stop` hook with (packages/claude-envoy/src/legion-phase-hook.ts), which
# Claude Code turns into a user turn. Both in the one entry: the feedback is what makes it the
# hook's own reminder rather than any other line written into the session.
# reminded_or_completed OFFSET is the stall check's wait: the reminder reached the implementer
# (stop_hook_reminded), or the daemon recorded its handoff first, which leaves no open phase to
# remind it of. The check then tells the two apart, so a worker that did its phase from the task that
# started it, against the proof's instructions, is a named failure and never a timeout.
reminded_or_completed() { stop_hook_reminded "$1" || handoff_recorded; }
stop_hook_reminded() {
  local entries
  entries=$(claude_entries "$1") || return 1
  awk -v needle="$stall_reminder_needle" \
    'index($0, "Stop hook feedback") && index($0, needle) { found = 1 } END { exit !found }' <<<"$entries"
}
# reminded_turn_continued OFFSET: an assistant message follows that feedback, so the block carried
# the turn on in the same session rather than ending it.
reminded_turn_continued() {
  local entries
  entries=$(claude_entries "$1") || return 1
  awk -v needle="$stall_reminder_needle" '
    index($0, "Stop hook feedback") && index($0, needle) { reminded = 1; next }
    reminded && $0 == "assistant" { continued = 1 }
    END { exit !continued }' <<<"$entries"
}
# report_stall_entries is that step's timeout_hook: what the transcript held when the wait gave up.
report_stall_entries() {
  note "the implementer's transcript since the stall instruction: $(claude_entries "$stall_offset" | cut -c1-200 | tr '\n' '|')"
}
# legion_handoff_in: the transcript holds a call of the `legion` MCP tool whose input names
# handoff_complete, and a tool result for it that is not an error.
legion_handoff_in() {
  transcript_since 0 | jq -R -s -e '
    [split("\n")[] | fromjson? | select(.message.content | type == "array") | .message.content[]] as $blocks
    | [$blocks[] | select(.type == "tool_use" and (.name | endswith("__legion")) and (.input | tostring | contains("handoff_complete"))) | .id] as $calls
    | any($blocks[]; .type == "tool_result" and (.tool_use_id as $id | $calls | index($id)) and (.is_error != true))' >/dev/null
}
# resumed_reply_names OFFSET MARKER: an assistant text written past OFFSET bytes quotes MARKER.
resumed_reply_names() {
  local text
  text=$(transcript_since "$1" | jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text')
  grep -Fq -- "$2" <<<"$text"
}
handoff_recorded() { [ -n "$(role_handoff "$issue" implementer implementing)" ]; }

# ---- the scratch services ------------------------------------------------------------------------

# Each binds bind_host (the devbox's private address, or its loopback under bridge) on the port the
# operator named, which prerequisites held free: a service that cannot take it has lost it to
# another process since, and the run says so rather than picking another, since only an admitted or
# forwarded port is one a pod can dial.
start_listener() {
  local offset
  offset=$(log_size listener)
  ENVOY_API_TOKEN="$(cat "$work/envoy-token")" PORT="$port_listener" ENVOY_LISTEN_HOST="$bind_host" \
    ENVOY_MACHINE_ID="legion-e2eccs-$$" NATS_URLS="nats://$bind_host:$port_nats" \
    start_process listener env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT "${app_key_unsets[@]}" "$work/envoy-listener"
  await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
    curl -fsS -H "@$work/envoy-auth-header" "http://$bind_host:$port_listener/v1/sessions" ||
    fail "the Envoy listener could not take port $port_listener; see $evidence/logs/listener.log"
}

start_dispatch() {
  local offset
  offset=$(log_size dispatch)
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  # HOME and XDG_CONFIG_HOME are the scratch home, so the server reads no envoy.json of the operator's.
  DATABASE_URL="postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/dispatch?sslmode=disable" \
    DISPATCH_AGENT_TOKEN="$(cat "$work/dispatch-token")" ENVOY_TOKEN="$(cat "$work/envoy-token")" \
    HOME="$work/dispatch-home" XDG_CONFIG_HOME="$work/dispatch-home/.config" \
    DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
    DISPATCH_LISTEN_HOST="$bind_host" DISPATCH_PORT="$port_dispatch" \
    DISPATCH_SERVER_URL="$pod_dispatch_url" NATS_URLS="nats://$bind_host:$port_nats" \
    ENVOY_URL="http://$bind_host:$port_listener" \
    start_process dispatch env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT "${app_key_unsets[@]}" "$work/envoy-dispatch"
  dispatch_base="http://$bind_host:$port_dispatch"
  await_start dispatch "$dispatch_pid" "$offset" 60 "the scratch Dispatch server" \
    curl -fsS "$dispatch_base/api/v1" ||
    fail "the scratch Dispatch could not take port $port_dispatch; see $evidence/logs/dispatch.log"
}

# ---- the operator's route and the run's own Secret ------------------------------------------------

# write_pod_fixture writes the run's copy of the operator route's pod block: the chosen route's
# fixture, its ConfigMap reference pointed at the run's own copy, the gateway's audience in place of
# the placeholder under `gateway`, and the Claude lane's two non-credential variables added to its
# env. The key itself is in none of it: a credential-shaped name in pod.env is refused by the
# daemon, and provider_keys carries it instead.
write_pod_fixture() {
  local source=$operator_route/pod.yml placeholder='${MODEL_TOKEN_AUDIENCE}' pod added
  [ "$model_route" = gateway ] || source=$operator_route/pod-key-file.yml
  pod=$(<"$source")
  if [ "$model_route" = gateway ]; then
    pod=${pod//"$placeholder"/"$gateway_audience"}
  fi
  added=$(printf '  ANTHROPIC_MODEL: "%s"' "$claude_model")
  [ -z "$claude_base_url" ] || added=$(printf '%s\n  ANTHROPIC_BASE_URL: "%s"' "$added" "$claude_base_url")
  awk -v add="$added" '{ print } $0 == "env:" { print add }' <<<"$pod" |
    sed "s/name: legion-operator-route\$/name: $route_configmap/" >"$work/pod.yml"
  grep -qx "  ANTHROPIC_MODEL: \"$claude_model\"" "$work/pod.yml" ||
    fail "$source has no env: block to carry the Claude lane's ANTHROPIC_MODEL"
  grep -qF "name: $route_configmap" "$work/pod.yml" || fail "$source mounts no ConfigMap legion-operator-route"
  if [ "$model_route" = gateway ]; then
    grep -qF "audience: \"$gateway_audience\"" "$work/pod.yml" ||
      fail "$source has no token audience $placeholder to fill with the gateway's"
  fi
  cp "$work/pod.yml" "$evidence/pod.yml"
}
# create_route_configmap is the operator's step before any pod runs: the chosen route's models.yml,
# the gateway's base URL in place of the placeholder under `gateway`, and the route's overlay.yml.
create_route_configmap() {
  local placeholder='${MODEL_BASE_URL}' models
  if [ "$model_route" = gateway ]; then
    models=$(<"$operator_route/models.yml")
    printf '%s\n' "${models//"$placeholder"/"$gateway"}" >"$work/models.yml"
    grep -qFx "    baseUrl: $gateway" "$work/models.yml" ||
      fail "the operator route's models.yml has no baseUrl $placeholder to point at the gateway"
  else
    cp "$operator_route/models-key-file.yml" "$work/models.yml"
  fi
  op create configmap "$route_configmap" --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$operator_route/overlay.yml" \
    --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
    fail "the operator could not create ConfigMap $route_configmap"
  note "[operator] ConfigMap $route_configmap: the $model_route route's models.yml and the route's overlay.yml, label legion.dev/project=$run_label"
}
# create_providers_secret is the one Secret of the run, under the name the runtime gives the
# project's providers Secret: the Anthropic key, under the name provider_keys hands the pods, and the
# session database's pod-facing URL, under the key session_dsn_secret names, which every pod keeps
# its sessions through. Each reaches kubectl from the run's 0600 file, never an argument, and
# neither is printed.
create_providers_secret() {
  [ -s "$work/session-dsn" ] || fail "the session database's URL file $work/session-dsn is missing: start the session database before the providers Secret"
  op create secret generic "$providers_secret" --from-file=ANTHROPIC_API_KEY="$work/claude-model-key" \
    --from-file="$session_dsn_key=$work/session-dsn" \
    --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
    fail "the operator could not create Secret $providers_secret"
  note "[operator] Secret $providers_secret: ANTHROPIC_API_KEY (the run's own key, not printed) and $session_dsn_key (the scratch session database's URL, not printed), label legion.dev/project=$run_label"
}

# ---- the daemon ------------------------------------------------------------------------------------

# The settings under test are worker_backends and runtime.kubernetes; the design gate is off, so the
# architect's gate registration starts planning with no human approval. provider_keys names a key of
# the providers Secret here, not a secretsd key: under this runtime the daemon resolves none itself.
# The session store is the run's scratch Postgres (session_store postgres, its URL the providers
# Secret's session_dsn_secret key), which the loader requires: an issue pod's disk dies with the
# pod, so no session may live on it. github_apps is the block check_github_apps proved, as written
# (lib/github-apps.sh). It names no `runtime.kubernetes.plugins` pair: the pods load the image's
# own plugins, a locally built `legion` links no release pair, and nothing here relaunches a worker
# on another plugin contract, so the daemon's boot says `plugins: none` and the proof is unaffected.
write_legion_config() {
  cat >"$work/legion.yaml" <<EOF
project: $project
bind: 0.0.0.0
advertise_host: $host
port: $port_daemon
worker_stream_port: $port_worker_stream
daemon_url: http://$host:$port_daemon
postgres_dsn: postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/legion?sslmode=disable
state_dir: $state
operator_token_file: $work/operator-token
envoy_url: http://$host:$port_listener
envoy_token_file: $work/envoy-token
nats_urls:
  - nats://$host:$port_nats
dispatch_url: $pod_dispatch_url
dispatch_token_file: $work/dispatch-token
# The project's repository is Dispatch's mapping (the PUT below makes $repo its default), not a key of
# legion.yaml.
projects:
  $project: {}
gates:
  design: off
admission_cap: 1
instructions: $work/instructions.md
worker_backends:
  implementer: claude
provider_keys: {ANTHROPIC_API_KEY: ANTHROPIC_API_KEY}
$(<"$work/github-apps.yaml")
runtime:
  kubernetes:
    namespace: $namespace
    image: $image
    storage_class: $storage_class
    issue_volume: 20Gi
    session_store: postgres
    session_dsn_secret: $session_dsn_key
    kubeconfig: $runtime_kubeconfig
    context: $runtime_context
    pod:
$(sed 's/^/      /' "$work/pod.yml")
EOF
  cp "$work/legion.yaml" "$evidence/legion.yaml"
}
daemon_answers_or_exited() { ! kill -0 "$daemon_pid" 2>/dev/null || curl -fsS "http://$bind_host:$port_daemon/healthz"; }
report_boot() { note "the daemon log's tail: $(tail -5 "$daemon_log" | cut -c1-300 | tr '\n' '|')"; }
# Under aws-private-ip the daemon runs with the caller's environment less these, and less every App
# key (app_key_unsets), which prerequisites adds once require_app_inputs has read the Apps. Under
# bridge its container carries none of the caller's environment but the names daemon_run_args
# passes, so none of these can reach it.
daemon_env_unset=(-u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u NATS_DAEMON_NKEY_SEED -u NATS_DAEMON_NKEY_SEED_FILE
  -u GH_PUBLIC_REPO_PAT -u ANTHROPIC_API_KEY)
# mountable PATH: PATH can be bind-mounted into the daemon's container at its own path — absolute,
# existing, and holding no ',' (the separator of docker's --mount) and no ':' (the separator of
# LEGION_E2E_DAEMON_CONTAINER_MOUNTS and of PATH).
mountable() {
  case "$1" in /*) ;; *) return 1 ;; esac
  case "$1" in *[:,]*) return 1 ;; esac
  [ -e "$1" ]
}
# daemon_mounts holds the host paths the daemon's container mounts read-only at their own paths,
# and daemon_path its PATH, both filled by prerequisites under bridge; daemon_mount_add PATH adds
# PATH to daemon_mounts once, since docker refuses two mounts at one path.
daemon_mounts=()
daemon_path=/bin
daemon_mount_add() {
  local mount
  for mount in "${daemon_mounts[@]}"; do [ "$mount" != "$1" ] || return 0; done
  daemon_mounts+=("$1")
}
# daemon_run_args sets daemon_run to every argument of the daemon's `docker run` under bridge, up to
# and including its image, and daemon_passed to the names of the caller's variables it passes.
# prerequisites' probe and start_daemon both run that one array, so the probe proves the very
# container the daemon gets. It shares the devbox's network, so the services it dials on loopback
# and the API and worker stream it binds are the devbox's own, and --add-host maps host, the bridge
# Service's name legion.yaml carries, to loopback in its own /etc/hosts alone. It runs as the devbox
# user, so what it writes under $work is that user's. --init puts docker's reaper at its pid 1,
# which hands the daemon the TERM docker delivers and reaps whatever a key command or an auth
# plugin run leaves orphaned, which the daemon at pid 1 would leave as zombies. $work is mounted
# read-write at its own path — the static binary, legion.yaml, its state_dir and token files, every
# path legion.yaml names but the kubeconfig — and each path of daemon_mounts read-only at its own;
# --mount refuses a source that does not exist rather than making an empty directory of it. Its
# environment is HOME, a directory of $work's, PATH, and the caller's variables the auth plugin and
# the key runner read, passed by name alone (`-e NAME`, whose value docker takes from its own
# environment), so no value is on an argv and nothing else of the caller's — the App keys, the NATS
# seeds, ANTHROPIC_API_KEY — enters it.
daemon_run=()
daemon_passed=()
daemon_run_args() {
  local mount name
  daemon_run=(--rm --name "$daemon_container" --network host --add-host "$host:127.0.0.1"
    --user "$(id -u):$(id -g)" --init --workdir "$work"
    --mount "type=bind,source=$work,target=$work")
  for mount in "${daemon_mounts[@]}"; do
    daemon_run+=(--mount "type=bind,source=$mount,target=$mount,readonly")
  done
  daemon_run+=(-e "HOME=$work/daemon-home" -e "PATH=$daemon_path")
  daemon_passed=()
  while IFS= read -r name; do
    case "$name" in
      AGENT_SECRETS_* | CLOUDSDK_* | GOOGLE_APPLICATION_CREDENTIALS | KUBECACHEDIR)
        daemon_run+=(-e "$name")
        daemon_passed+=("$name")
        ;;
    esac
  done < <(compgen -e)
  daemon_run+=("$daemon_image")
}
# prove_daemon_container is prerequisites' proof of the daemon's container under bridge, before the
# run creates anything. It resolves what the container mounts beside $work, refusing by name each
# path it cannot mount: the runtime kubeconfig; the auth plugin that kubeconfig's context's user
# execs, if it names one, and the App key runner, each a regular executable found as the daemon
# finds it (by PATH for a bare name, as client-go and the key command's shell look it up); the
# devbox's /etc/ssl/certs, for the daemon's HTTPS calls to GitHub; the file
# GOOGLE_APPLICATION_CREDENTIALS names, if it names one; and LEGION_E2E_DAEMON_CONTAINER_MOUNTS. The
# container's PATH is the image's /bin, so its `sh`, `tr` and `base64` win, then the two binaries'
# directories. A probe then runs with the daemon's own arguments (daemon_run_args) and must print
# the devbox user's uid and gid, an /etc/hosts line mapping host to 127.0.0.1, and the runner and
# the auth plugin resolved by name at the paths the devbox resolves them to.
prove_daemon_container() {
  local runner= plugin= plugin_word gac mount bin inside got_uid got_gid got_hosts got_runner got_plugin plugin_said
  mountable "$runtime_kubeconfig" ||
    fail "LEGION_E2E_RUNTIME_KUBECONFIG names $runtime_kubeconfig, which the daemon's container cannot mount at its own path: it must be an existing absolute path holding no ',' or ':'"
  runner=$(command -v "$app_key_runner") && [ -f "$runner" ] && [ -x "$runner" ] && mountable "$runner" ||
    fail "the App key runner $app_key_runner (LEGION_E2E_APP_KEY_RUNNER) resolves to '${runner:-nothing}', not a regular executable the daemon's container can mount at its own path (absolute, holding no ',' or ':')"
  plugin_word=$(kubectl config view --raw --minify --kubeconfig "$runtime_kubeconfig" --context "$runtime_context" \
    -o jsonpath='{.users[0].user.exec.command}') ||
    fail "kubectl cannot read context $runtime_context's user from the runtime kubeconfig $runtime_kubeconfig"
  case "$plugin_word" in
    '') ;;
    */*) plugin=$plugin_word ;;
    *) plugin=$(command -v "$plugin_word") || plugin= ;;
  esac
  if [ -n "$plugin_word" ]; then
    [ -n "$plugin" ] && [ -f "$plugin" ] && [ -x "$plugin" ] && mountable "$plugin" ||
      fail "context $runtime_context's user in $runtime_kubeconfig execs the auth plugin $plugin_word, which resolves to '${plugin:-nothing}', not a regular executable the daemon's container can mount at its own path (absolute, holding no ',' or ':')"
  fi
  gac=${GOOGLE_APPLICATION_CREDENTIALS:-}
  if [ -f "$gac" ]; then
    mountable "$gac" ||
      fail "GOOGLE_APPLICATION_CREDENTIALS names $gac, which the daemon's container cannot mount at its own path: it must be absolute and hold no ',' or ':'"
  else
    gac=
  fi
  daemon_mounts=()
  for mount in "$runtime_kubeconfig" "$plugin" "$runner" /etc/ssl/certs "$gac" "${daemon_extra_mounts[@]}"; do
    [ -z "$mount" ] || daemon_mount_add "$mount"
  done
  # A variable the container is passed by name whose value is a path (the key runner's key directory,
  # the gcloud config) is useless inside without that path, so each one naming an existing absolute
  # path is mounted read-only at it too.
  local name value
  while IFS= read -r name; do
    case "$name" in AGENT_SECRETS_* | CLOUDSDK_* | KUBECACHEDIR) ;; *) continue ;; esac
    value=${!name}
    case "$value" in /*) ;; *) continue ;; esac
    [ -e "$value" ] || continue
    mountable "$value" ||
      fail "$name names $value, which the daemon's container cannot mount at its own path: it must hold no ',' or ':'"
    daemon_mount_add "$value"
  done < <(compgen -e)
  daemon_path=/bin
  for bin in "$runner" "$plugin"; do
    [ -n "$bin" ] || continue
    case ":$daemon_path:" in
      *":$(dirname "$bin"):"*) ;;
      *) daemon_path+=":$(dirname "$bin")" ;;
    esac
  done
  mkdir -p "$work/daemon-home"
  chmod 0700 "$work/daemon-home"
  daemon_run_args
  # shellcheck disable=SC2016  # expanded by the probe's shell
  inside=$(docker run "${daemon_run[@]}" sh -c '
    printf "uid %s\n" "$(id -u)"
    printf "gid %s\n" "$(id -g)"
    printf "hosts %s\n" "$(awk -v host="$1" "!/^#/ { for (i = 2; i <= NF; i++) if (\$i == host) { print; exit } }" /etc/hosts)"
    printf "runner %s\n" "$(command -v "$2")"
    "$2" --help >/dev/null 2>&1; printf "runner-exec %s\n" "$?"
    if [ -n "$3" ]; then
      printf "plugin %s\n" "$(command -v "$3")"
      "$3" --help >/dev/null 2>&1; printf "plugin-exec %s\n" "$?"
    fi' sh "$host" "$app_key_runner" "$plugin_word" 2>&1) ||
    fail "the daemon's container ($daemon_image) could not run its probe: $(tr '\n' ' ' <<<"$inside" | cut -c1-300)"
  got_uid=$(sed -n 's/^uid //p' <<<"$inside")
  got_gid=$(sed -n 's/^gid //p' <<<"$inside")
  got_hosts=$(sed -n 's/^hosts //p' <<<"$inside")
  got_runner=$(sed -n 's/^runner //p' <<<"$inside")
  got_plugin=$(sed -n 's/^plugin //p' <<<"$inside")
  # Each binary is started once with --help: busybox holds no dynamic loader and no interpreter, so a
  # dynamically linked binary or a script (a version manager's shim) cannot be executed there, which
  # the shell reports as 126 or 127, whatever the binary would answer --help with.
  case "$(sed -n 's/^runner-exec //p' <<<"$inside")" in
    126 | 127 | '')
      fail "the App key runner $runner cannot be executed in the daemon's container ($daemon_image): it is dynamically linked or a script; name a static binary by its path in LEGION_E2E_APP_KEY_RUNNER (a version manager's shim resolves elsewhere: \`mise which agent-secrets\`, say)" ;;
  esac
  if [ -n "$plugin" ]; then
    case "$(sed -n 's/^plugin-exec //p' <<<"$inside")" in
      126 | 127 | '')
        fail "the auth plugin $plugin cannot be executed in the daemon's container ($daemon_image): it is dynamically linked or a script; point the runtime kubeconfig's exec command at a static binary" ;;
    esac
  fi
  [ "$got_uid $got_gid" = "$(id -u) $(id -g)" ] ||
    fail "the daemon's container ran its probe as uid ${got_uid:-<none>} gid ${got_gid:-<none>}, want $(id -u) $(id -g), the devbox user's own"
  awk -v host="$host" '$1 == "127.0.0.1" { for (i = 2; i <= NF; i++) if ($i == host) found = 1 } END { exit !found }' <<<"$got_hosts" ||
    fail "the daemon's container's /etc/hosts names $host as '$got_hosts', want 127.0.0.1 (--add-host)"
  [ "$got_runner" = "$runner" ] ||
    fail "inside the daemon's container the App key runner $app_key_runner resolves to '${got_runner:-nothing}', want $runner, the devbox's"
  plugin_said="the runtime kubeconfig's user execs no auth plugin"
  if [ -n "$plugin" ]; then
    [ "$got_plugin" = "$plugin" ] ||
      fail "inside the daemon's container the auth plugin $plugin_word resolves to '${got_plugin:-nothing}', want $plugin, the devbox's"
    plugin_said="the auth plugin $plugin_word is $got_plugin"
  fi
  note "the daemon's container ($daemon_image, docker run --network host --add-host $host:127.0.0.1 --init): its probe ran as uid $got_uid gid $got_gid, the devbox user's; its /etc/hosts: $got_hosts"
  note "inside it the App key runner $app_key_runner is $got_runner, and $plugin_said; its PATH is $daemon_path"
  note "mounted at their own paths: $work read-write; read-only ${daemon_mounts[*]}"
  note "passed by name from the caller's environment: ${daemon_passed[*]:-nothing}"
}
# kill_harness is lib/bridge-tunnel.sh's: its watchdog stops the daemon before it ends the run. The
# watchdog forked before the daemon started, so its own daemon_pid is empty; the container's name
# was fixed before then, and docker signals it whoever asks. The watchdog runs under errexit, so
# nothing here may fail.
kill_harness() {
  if ! docker kill --signal TERM "$daemon_container" >/dev/null 2>&1 &&
    [ -n "$daemon_pid" ] && signalable "$daemon_pid" 2>/dev/null; then
    kill -TERM "$daemon_pid" 2>/dev/null || true
  fi
  return 0
}
start_daemon() {
  # The daemon's own two ports alone: the run's NATS, Envoy listener, Dispatch and session database
  # hold the other four by now.
  ports_free "$port_daemon" "$port_worker_stream"
  # Under bridge daemon_pid is the docker client, attached, so the container's output reaches the
  # log as the daemon's own does, the client exits with the container, and its default --sig-proxy
  # hands the container the TERM stop_pid sends; cleanup removes the container after it.
  if [ "$pod_reachability" = bridge ]; then
    docker run "${daemon_run[@]}" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 7>&- &
  else
    env "${daemon_env_unset[@]}" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 7>&- &
  fi
  daemon_pid=$!
  timeout_hook=report_boot
  # The boot runs the worker image's probe pod, so the bound is the cluster's, not a local process's.
  until_true 900 "the Go daemon to boot and answer /healthz" daemon_answers_or_exited
  timeout_hook=
  kill -0 "$daemon_pid" 2>/dev/null ||
    fail "the Go daemon exited before it answered /healthz: $(tail -3 "$daemon_log" | cut -c1-300 | tr '\n' '|')"
}
# ports_free PORT... fails naming the first PORT something on the devbox already listens on.
ports_free() {
  local port
  for port in "$@"; do
    [ -z "$(ss -Hltn "sport = :$port")" ] || fail "port $port is taken on the devbox: $(ss -Hltnp "sport = :$port")"
  done
}
log_lines() { jq -R -c --arg m "$1" 'fromjson? | select(.msg == $m)' "$daemon_log"; }

# ---- the run's own cleanup -------------------------------------------------------------------------

# collect_transcripts copies the implementer's Claude Code transcript out of the session table, where
# its pod's launcher saved it: the table outlives every pod of the run, the one the resume check
# deleted included, and holds the transcript up to the last line a save committed. A run that failed
# before the implementer registered one has nothing to copy, and a copy that fails leaves no file.
collect_transcripts() {
  [ -n "$transcript" ] || return 0
  session_text "$transcript" >"$evidence/transcripts/implementer.jsonl" 2>/dev/null ||
    rm -f "$evidence/transcripts/implementer.jsonl"
}
# remove_run_branch closes any pull request of the run's project on the smoke repository and deletes
# the issue's branch, which the implementer pushed: a passing run opens no pull request, so neither
# outlives it. It makes no gh call before the proof human is established.
remove_run_branch() {
  [ -n "$proof_human" ] && [ -n "$issue" ] || return 0
  close_run_pull_requests >&2 || printf "some of this run's pull requests may still be open on %s (above)\n" "$repo" >&2
  [ "$(smoke_branch_state "legion/$issue" 2>/dev/null)" = present ] || return 0
  if timeout 60 gh api -X DELETE "repos/$repo/git/refs/heads/legion/$issue" >/dev/null 2>&1; then
    printf 'deleted %s branch legion/%s\n' "$repo" "$issue" >&2
  else
    printf 'could not delete %s branch legion/%s\n' "$repo" "$issue" >&2
  fi
}

cleanup() {
  local status=$? p
  # A second signal must not cut the teardown short, and a closed output must not end it.
  trap '' HUP INT TERM PIPE
  exec >&7 2>&7
  set +e
  # Teardown is best effort: a failing command here is a warning about the teardown, never a check's
  # FAIL line, and the run's exit status stays the one the checks set.
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  rm -f "$work/claude-model-key" "$work/claude-key-header"
  # The daemon goes first: a live one would launch the Sandboxes the teardown is deleting.
  collect_transcripts
  stop_pid "$daemon_pid"
  # Under bridge that stopped the daemon's docker client; a client stop_pid had to KILL leaves its
  # container running, so the container is removed here too, before the teardown deletes the
  # Sandboxes. Nothing to remove under aws-private-ip. A driver killed outright leaves it for the
  # next run's refuse_leftovers, which names it.
  docker rm -f "$daemon_container" >/dev/null 2>&1 || true
  # The bridge next, before the sweep below kills its chisel client, which its watchdog would read as
  # a dead tunnel; a no-op when bridge_start never ran.
  bridge_stop
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$pg_container" "$nats_container" "$session_db_container" >/dev/null 2>&1 || true
  [ -z "$snapshotted" ] || teardown
  remove_run_branch
  # namespace_clean fails loudly when the namespace differs from its snapshot, and its `fail` is an
  # exit: in a subshell, so the lines below it still run and the run keeps the status it earned.
  if [ -z "$compared" ] && [ -n "$snapshotted" ]; then (namespace_clean) || status=1; fi
  if [ -n "$ok" ]; then
    rm -rf "$work"
  else
    printf "the run's scratch workspace is %s\n" "$work" >&2
    printf 'claude backend sandbox e2e: FAIL (check %s)\n' "$check" >&2
  fi
  printf "the run's evidence is %s\n" "$evidence" >&2
  exit "$status"
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# The operator's inputs come first, before the run reads a tool, a key or the cluster: an unset one
# is a run that was never going to happen, and it is refused by its own name.
begin prerequisites
[ -n "$runtime_context" ] ||
  fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the runtime must run as the Legion daemon's restricted identity, never the operator's"
case "$image" in
  *@sha256:*) ;;
  *) fail "LEGION_E2E_IMAGE must be the branch's worker image pinned by digest (…@sha256:…), not '$image': a pull-request run of .github/workflows/worker-image.yaml publishes ghcr.io/sjawhar/legion-worker:sha-<first 12 hex of the commit> and records its digest" ;;
esac
[ -n "$storage_class" ] ||
  fail "LEGION_E2E_STORAGE_CLASS is unset: name the StorageClass of the issue pod's disk (the cluster has no default to fall back on)"
[ -n "$claude_key_file" ] ||
  fail "LEGION_E2E_CLAUDE_KEY_FILE is unset: name a file holding the Anthropic API key the Claude Code implementer reaches its model with"
[ -f "$claude_key_file" ] && [ -r "$claude_key_file" ] ||
  fail "LEGION_E2E_CLAUDE_KEY_FILE names $claude_key_file, which is not a readable file"
[ -n "$claude_model" ] ||
  fail "LEGION_E2E_CLAUDE_MODEL is unset: name the model id the pod's Claude Code runs, which travels as ANTHROPIC_MODEL in runtime.kubernetes.pod.env"
case "$claude_base_url" in
  '' | https://?*) ;;
  *) fail "LEGION_E2E_CLAUDE_BASE_URL must be an https:// URL or unset (Anthropic's own endpoint), not '$claude_base_url'" ;;
esac
case "$model_route" in
  gateway)
    gateway=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
      fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway URL the operator route's models.yml can name (the reason is above)"
    gateway_audience=$(bash "$root/scripts/e2e/lib/model-gateway-audience.sh") ||
      fail "LEGION_E2E_MODEL_GATEWAY_AUDIENCE is not a token audience the operator route's pod.yml can carry (the reason is above)"
    ;;
  key-file) ;;
  *) fail "LEGION_E2E_MODEL_ROUTE must be gateway or key-file, not '$model_route': it is the route the pod's Oh My Pi architect and planner take" ;;
esac
case "$pod_reachability" in
  aws-private-ip) port_kind="a port the devbox's security group admits from the Legion nodes (scripts/e2e/README.md's port-admission note)" ;;
  bridge) port_kind="a free port on the devbox, which the bridge forwards" ;;
  *) fail "LEGION_E2E_POD_REACHABILITY must be aws-private-ip or bridge, not '$pod_reachability': it is how the pods reach the run's services on the devbox, at its private address or through a bridge of the run's own" ;;
esac
for named in "LEGION_E2E_DAEMON_PORT:$port_daemon" "LEGION_E2E_WORKER_STREAM_PORT:$port_worker_stream" \
  "LEGION_E2E_DISPATCH_PORT:$port_dispatch" "LEGION_E2E_ENVOY_PORT:$port_listener" "LEGION_E2E_NATS_PORT:$port_nats" \
  "LEGION_E2E_SESSION_DB_PORT:$port_session_db"; do
  case "${named#*:}" in
    '') fail "${named%%:*} is unset: name $port_kind, since every pod dials it" ;;
    *[!0-9]*) fail "${named%%:*} must be a port number, not '${named#*:}'" ;;
  esac
done
[ "$(printf '%s\n' "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats" "$port_session_db" | sort -u | wc -l)" = 6 ] ||
  fail "the six ports must differ: daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats, session database $port_session_db"
# The daemon's container's extra mounts, under bridge alone (prove_daemon_container): each entry
# must be one docker can mount at its own path.
daemon_extra_mounts=()
if [ "$pod_reachability" = bridge ] && [ -n "$daemon_container_mounts" ]; then
  IFS=: read -r -a daemon_extra_mounts <<<"$daemon_container_mounts"
  for mount in "${daemon_extra_mounts[@]}"; do
    mountable "$mount" ||
      fail "LEGION_E2E_DAEMON_CONTAINER_MOUNTS names '$mount', which is not an existing absolute path holding no ',': each of its colon-separated entries is a host path the daemon's container mounts read-only at its own path"
  done
fi
# The run's two role Apps, this deployment's own unless the App inputs name another's
# (lib/github-apps.sh); a malformed input is refused here by name, never printed. No process the run
# starts inherits an App key: the listener and Dispatch run less app_key_unsets, the daemon under
# aws-private-ip less daemon_env_unset, which takes the same list here, and the daemon's container
# under bridge carries none of the caller's environment but the names daemon_run_args passes.
require_app_inputs implement review
daemon_env_unset+=("${app_key_unsets[@]}")
# sha256sum holds the session table's copy of the transcript to the pod's (pod-deleted-resumes-…).
tools=(go docker jq curl ss kubectl bun secrets gh sha256sum)
# getent holds the bridge Service's name to resolving nowhere on the devbox (prerequisites, below).
[ "$pod_reachability" != bridge ] || tools+=(getent)
for tool in "${tools[@]}"; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
refuse_leftovers legion-e2eccs
ports_free "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats" "$port_session_db"
mkdir -p "$state"
chmod 0700 "$state"
(umask 077 && tr -d '[:space:]' <"$claude_key_file" >"$work/claude-model-key")
[ -s "$work/claude-model-key" ] || fail "LEGION_E2E_CLAUDE_KEY_FILE ($claude_key_file) holds no key"
# The key's preflight, against the endpoint the pod's Claude Code will call: it lists its models to
# a key it accepts. The header goes by file, so the key is in no argv.
(umask 077 && printf 'x-api-key: %s\n' "$(cat "$work/claude-model-key")" >"$work/claude-key-header")
code=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -H "@$work/claude-key-header" -H 'anthropic-version: 2023-06-01' \
  "${claude_base_url:-https://api.anthropic.com}/v1/models") || code="unreachable (curl exited $?)"
rm -f "$work/claude-key-header"
[ "$code" = 200 ] ||
  fail "the model endpoint ${claude_base_url:-https://api.anthropic.com} refuses the key in LEGION_E2E_CLAUDE_KEY_FILE ($claude_key_file): GET /v1/models answered $code"
note "the Claude Code implementer's key from $claude_key_file: ${claude_base_url:-https://api.anthropic.com} accepts it, and it runs $claude_model"
# Legion's two role Apps proved against GitHub before the run writes anything (lib/github-apps.sh):
# each App's key command runs as the daemon runs it, and GET /app under the key's JWT must answer
# the App's id and the write permission its role needs. legion.yaml's github_apps is the block
# proved, and every check that names an App compares with the login read here.
write_github_apps implement review
read_app_logins
require_proof_human
gh repo view "$repo" --json name >/dev/null || fail "the devbox's ordinary gh cannot read $repo"
case "$pod_reachability" in
  aws-private-ip)
    imds=$(curl -sf -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60') ||
      fail "instance metadata is unreachable; the run's services bind the devbox's private address, read from it, since every pod dials them there"
    host=$(curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4) ||
      fail "instance metadata has no local-ipv4"
    unset imds
    bind_host=$host
    ;;
  bridge)
    # The devbox binds loopback; the reverse-tunnel bridge (begin bridge, below) is what makes host —
    # the bridge's own ClusterIP Service, deterministic from bridge_name — dialable from inside the
    # namespace. The daemon dials it too, from its container, whose own /etc/hosts maps it to
    # loopback: the name must resolve nowhere on the devbox itself, and prove_daemon_container
    # proves the container before the run creates anything.
    bind_host=127.0.0.1
    host="$bridge_name.$namespace.svc.cluster.local"
    outside_status=0
    outside=$(getent hosts "$host") || outside_status=$?
    [ "$outside_status" != 0 ] ||
      fail "$host resolves on the devbox ($outside): the name must be the daemon's container's alone, which maps it to loopback"
    note "on the devbox, getent hosts $host resolves nothing (exit $outside_status)"
    prove_daemon_container
    ;;
esac
pod_dispatch_url="http://$host:$port_dispatch"
op get namespace "$namespace" -o name >/dev/null || fail "the operator context $operator cannot read namespace $namespace"
timeout --foreground 120 kubectl --kubeconfig "$runtime_kubeconfig" --context "$runtime_context" -n "$namespace" get sandboxes -o name >/dev/null ||
  fail "the runtime identity (context $runtime_context in $runtime_kubeconfig) cannot list the Sandboxes of namespace $namespace; is Agent Sandbox installed and the identity bound?"
leftover=$(op get sandboxes,pods,pvc,configmaps,secrets -l "legion.dev/project=$run_label" -o name 2>&1) ||
  fail "the operator context cannot list namespace $namespace: $leftover"
[ -z "$leftover" ] ||
  fail "namespace $namespace already holds objects labelled legion.dev/project=$run_label: $(tr '\n' ' ' <<<"$leftover")"
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root") || fail "lib/built-from.sh could not read the source revision"
while IFS= read -r line; do note "$line"; done <<<"$built"
note "project $project (label legion.dev/project=$run_label), namespace $namespace, image $image, storage class $storage_class"
note "runtime identity: context $runtime_context in $runtime_kubeconfig; operator: context $operator"
if [ "$pod_reachability" = aws-private-ip ]; then
  note "pod reachability aws-private-ip: the devbox's private address $host: daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats, session database $port_session_db, each $port_kind"
else
  note "pod reachability bridge: the devbox's loopback $bind_host, advertised to pods as $host (the bridge's Service): daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats, session database $port_session_db, each $port_kind"
fi
pass

begin rig
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
  printf 'X-Dispatch-User: %s\n' "$proof_human_user" >"$work/dispatch-human-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
chmod 0600 "$work"/*token "$work"/*-header "$work/postgres-password"
# The daemon is built static (CGO_ENABLED=0), so under bridge it runs in its container's minimal
# image (daemon_run_args); the devbox runs the same binary under aws-private-ip and for the run's own
# `legion` calls.
(cd "$root/packages/daemon" && CGO_ENABLED=0 go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/envoy-dispatch" ./cmd/dispatch)
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion" "$work/envoy-listener" "$work/envoy-dispatch") ||
  fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"
# Postgres is the daemon's and Dispatch's alone, so it stays on loopback: no pod reads it.
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=dispatch \
  -p "127.0.0.1::5432" pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
[ -n "$port_pg" ] || fail "docker assigned Postgres no host port"
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d dispatch
docker exec "$pg_container" createdb -U legion legion
# NATS binds bind_host on the run's port: every pod's agent dials it there, directly or through the
# bridge.
docker run -d --name "$nats_container" -p "$bind_host:$port_nats:4222" nats:2.10 -js >/dev/null
until_true 60 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
# The run's session database, a second throwaway Postgres: every pod keeps its sessions in it
# (session_store postgres, which the daemon requires, since an issue pod's disk dies with the pod),
# the Claude Code implementer's transcript among them, so it binds bind_host on the run's port and
# every pod dials it at host, directly or through the bridge. It is its own container and its own
# credential, never the daemon's database above, since its URL reaches every pod through the
# providers Secret. Its password is the run's own and reaches docker through an owner-only env
# file, never an argument; the pod-facing URL is written only to an owner-only file, which the
# providers Secret reads, and is never printed. The run's own reads go through the container
# (session_psql), so the devbox needs no URL of it.
session_db_password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
(
  umask 077
  printf 'POSTGRES_USER=legion\nPOSTGRES_DB=sessions\nPOSTGRES_PASSWORD=%s\n' "$session_db_password" >"$work/session-db.env"
  printf 'postgres://legion:%s@%s:%s/sessions?sslmode=disable\n' "$session_db_password" "$host" "$port_session_db" >"$work/session-dsn"
)
unset session_db_password
docker run -d --name "$session_db_container" --cpus 1 --mount type=tmpfs,destination=/var/lib/postgresql/data \
  --env-file "$work/session-db.env" -p "$bind_host:$port_session_db:5432" "$session_db_image" >/dev/null ||
  fail "docker could not start the scratch session database $session_db_container on $bind_host:$port_session_db"
until_true 60 "the session database to accept TCP connections" docker exec "$session_db_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d sessions
note "container $session_db_container: database sessions on $bind_host:$port_session_db ($session_db_image), which every pod reads at $host:$port_session_db through the providers Secret's $session_dsn_key (the URL is in an owner-only file, not printed)"
start_listener
start_dispatch
dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Claude backend sandbox proof $project" '{key:$key,name:$name}')" >/dev/null
dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project:$project,default:true}')" >/dev/null
cat >"$work/instructions.md" <<'EOF'
# Claude Code sandbox backend proof instructions

This is a throwaway workflow proof. A tree's root architect starts its tree from the daemon's
`catch-up` notice as its role says: it writes the spec in the issue's own primary document and
registers the gate, then waits. Apart from that, do not act until a human Dispatch message targeted
at your own session gives the next exact proof operation; it arrives in your session as a message to
you. Any other notice is not an instruction. Follow your instruction precisely, use the Go-daemon
Legion tools and handoffs, and do not create work outside the issue's smoke branch.

The task that starts your phase (`Continue … Phase: <phase>`) is such a notice too: in this proof it
is never your instruction. Answer it with one line of plain prose saying you are standing by for
the proof's instruction, call no tool, and do not do the phase's work or call `handoff_complete`
until a Dispatch message from the proof asks you to. A reminder that your phase is still open is
answered the same way, with one line and no tool, until that message comes.
EOF
if [ "$pod_reachability" = aws-private-ip ]; then
  note "project $project, scratch Dispatch $dispatch_base, Envoy listener http://$bind_host:$port_listener, NATS nats://$bind_host:$port_nats"
else
  note "project $project, scratch Dispatch $dispatch_base, Envoy listener http://$bind_host:$port_listener, NATS nats://$bind_host:$port_nats (devbox loopback), advertised to pods as $pod_dispatch_url, http://$host:$port_listener and nats://$host:$port_nats"
fi
pass

begin snapshot
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
: >"$record"
note "[operator] $(wc -l <"$evidence/namespace-before.txt") of the run's objects in $namespace before it starts (labelled with project $run_label, or unlabelled and named for it)"
pass

if [ "$pod_reachability" = bridge ]; then
  begin bridge
  bridge_start "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats" "$port_session_db"
  pass
fi

begin operator-route
write_pod_fixture
create_route_configmap
create_providers_secret
note "the pods' Oh My Pi route is the $model_route fixture; the Claude lane's ANTHROPIC_MODEL$([ -n "$claude_base_url" ] && echo " and ANTHROPIC_BASE_URL") in runtime.kubernetes.pod.env, its key in provider_keys"
pass

begin daemon
write_legion_config
start_daemon
probe=$(log_lines "sandbox runtime: the worker image passed its probe" | tail -1)
[ -n "$probe" ] || fail "the daemon booted with no image probe pass logged; see $daemon_log"
note "image probe: $(jq -c '{image, model, sandbox}' <<<"$probe")"
if [ "$pod_reachability" = aws-private-ip ]; then
  note "the Go daemon answers http://$bind_host:$port_daemon/healthz and hosts its worker stream on tcp://$host:$port_worker_stream"
else
  note "the Go daemon (container $daemon_container, its docker client pid $daemon_pid) answers http://$bind_host:$port_daemon/healthz and hosts its worker stream on the devbox's network, advertised to pods as http://$host:$port_daemon and tcp://$host:$port_worker_stream"
fi
pass

begin admission
issue=$(new_issue "Claude Code sandbox implementer proof")
set_status "$issue" todo
until_true 180 "$issue to be admitted" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg issue '$issue' '.admission.active == [\$issue]' >/dev/null"
pass

begin architect-registers-the-gate
architect_registers_gate "$issue" "the proof's"
wait_for_phase "$issue" planning
wait_for_worker "$issue" planner
note "the design gate is off: the architect's registration started planning, both roles in issue pod $(claim_sandbox "$issue" architect)"
pass

begin planner-handoff
plan_started=$SECONDS
send_agent "$issue" planner "Claude backend sandbox proof planning operation: write the required plan handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$issue" implementing "$plan_seconds"
note "the planner took $((SECONDS - plan_started))s from its instruction to implementing, of a ${plan_seconds}s budget"
assert_handoff_committer "$issue" planner planning 0
pass

# Check 1. The implementer's pod launches and its claim registers a Claude Code transcript as its
# session: the file Claude Code records under the CLAUDE_CONFIG_DIR the launch places on the pod's
# disk, under the role's own state home, which the implementer's Claude Code says it was told. That
# is read from the agent's own environment (pod_env), never the pod's spec: the runtime sets only the
# kubelet-resolved variables there, and carries CLAUDE_CONFIG_DIR, like every plain NAME=value, in
# the launcher's start command (launchEnvironment in packages/daemon/internal/runtime/sandbox), so it
# is read in the implementer's own namespaces of the pod's `roles` container (pod_exec). The
# claim registers the transcript only once the launcher's first save of it has committed, so the
# session table already holds its row, keyed by its absolute path.
begin claude-implementer-registers
wait_for_worker "$issue" implementer
token=$(claim_token "$issue" implementer)
claim=$(claim_view "$issue" implementer) || fail "no implementer claim $token in the daemon's state"
session=$(jq -r .session <<<"$claim")
transcript=$(claim_session_file "$issue" implementer) || fail "the implementer's claim records no session file"
pod=$(implementer_pod) || fail "the implementer's claim has no Sandbox locator"
pod_uid=$(claim_pod_uid "$issue" implementer)
generation=$(claim_generation "$issue" implementer) || fail "legion claims list has no generation for the implementer's claim $token"
agent_env=$(pod_env "$pod" implementer 2>&1) ||
  fail "pod $pod has no readable agent environment in its implementer's namespaces: $(tr '\n' ' ' <<<"$agent_env" | cut -c1-300)"
config_dir=$(sed -n 's/^CLAUDE_CONFIG_DIR=//p' <<<"$agent_env")
[ "$config_dir" = /legion/scratch/state/implementer/claude ] ||
  fail "the implementer's Claude Code is told CLAUDE_CONFIG_DIR=${config_dir:-<unset>}, want /legion/scratch/state/implementer/claude, a directory of its role's state home on the pod's disk"
case "$transcript" in
  "$config_dir/projects/"*"/$session.jsonl") ;;
  *) fail "the implementer registered session file $transcript, want $config_dir/projects/<cwd>/$session.jsonl" ;;
esac
[ "$(transcript_size)" -gt 0 ] ||
  fail "the implementer's transcript $transcript is missing or empty in pod $pod"
registered_len=$(session_byte_len "$transcript") || fail "the session table gives no byte_len for the registered transcript $transcript (the reason is above)"
note "implementer claim $token: generation $generation, pod $pod (uid $pod_uid), Claude Code session $session"
note "its transcript is $transcript, on the pod's disk under CLAUDE_CONFIG_DIR=$config_dir, and a row of the session table (byte_len $registered_len)"
pass

# Check 2. The implementer's command line is the Claude Code launch: `legion claude-bridge --` running
# the Claude Code this generation's start script installed on this generation's session, with the
# MCP document the launcher wrote into the generation's private directory as its only
# `--mcp-config`. The image carries no Claude Code, only its pin, so the fetch has to have happened
# for the worker to run at all, and this holds that it is the fetch the proof expects: the bridge
# names, and the process under it is, the binary under this generation's own directory of the
# implementer's install root on the pod's disk (the claim's generation, which Check 1 read),
# never /usr/local/bin/claude, which the image does not carry; and that binary, run in the
# implementer's namespaces,
# prints the version the image's pin names, which is what the start script held it to before it
# execed.
begin claude-pod-runs-the-bridge
bridge_argv=$(claude_argv "$pod" bridge) || fail "the implementer's namespaces run no Claude Code under legion claude-bridge"
argv_has_pair "$bridge_argv" claude-bridge -- ||
  fail "the implementer's command line does not run claude-bridge --: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-300)"
argv_has_pair "$bridge_argv" --session-id "$session" ||
  fail "the implementer's claude was not started with --session-id $session: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-300)"
installed=$(claude_code_binary "$generation")
[ "$(argv_value "$bridge_argv" --)" = "$installed" ] ||
  fail "the bridge runs $(argv_value "$bridge_argv" --), not the Claude Code generation $generation's start script installed, $installed"
agent_argv=$(claude_argv "$pod" agent)
[ "$(sed -n 1p <<<"$agent_argv")" = "$installed" ] ||
  fail "the process under the bridge is $(sed -n 1p <<<"$agent_argv"), not the Claude Code generation $generation's start script installed, $installed"
if pod_exec "$pod" implementer test -e /usr/local/bin/claude; then
  fail "pod $pod's image carries /usr/local/bin/claude; the worker image carries Claude Code's pin and no Claude Code"
fi
pinned=$(pod_exec "$pod" implementer cat "$claude_code_pin" | jq -er '.dependencies["@anthropic-ai/claude-code"]') ||
  fail "pod $pod carries no Claude Code pin at $claude_code_pin naming @anthropic-ai/claude-code"
installed_version=$(pod_exec "$pod" implementer "$installed" --version) ||
  fail "the installed Claude Code $installed does not run in pod $pod: $installed_version"
[ "${installed_version%% *}" = "$pinned" ] ||
  fail "the installed Claude Code $installed prints --version '$installed_version'; the image's pin names $pinned"
mcp=$(argv_value "$bridge_argv" --mcp-config)
[ -n "$mcp" ] || fail "the Claude Code launch carries no --mcp-config: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-300)"
pod_exec "$pod" implementer test -f "$mcp" || fail "the launcher file $mcp the launch reads its MCP document from is not in the implementer's view of the pod"
note "pod $pod, the implementer's namespaces in container roles: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-200)"
note "its Claude Code is $installed, installed at launch from $claude_code_pin: --version $installed_version, the pin's $pinned"
note "its MCP document is the launcher file $mcp, written into this generation's private directory"
pass

# Check 3. The endless phase reminder on the Claude Code backend (INFRA-220), which runs while the
# implementer still holds its phase, before Check 4 completes it, as the tmux proof runs it. The
# phase is open since the daemon's start task, which reached the session as an assignment
# (packages/claude-envoy/src/legion-phase-hook.ts), and nothing has completed it. So a turn of the
# implementer's that ends now settles its `Stop` hook: the pod's `legion` MCP server answers
# `{"decision":"block","reason":<the reminder>}`, and Claude Code carries that reason into the
# session as `Stop hook feedback` and one more turn of the same session. The instruction is what
# makes the turn settle that way: it arrives as an Envoy delivery, which puts an open phase's run of
# reminders back at zero but never reopens a closed one (the `inbound-event` case of
# packages/pi-shared/src/phase-stall.ts), so it must come while the phase is still open, and the
# reminder this settle earns is the hook's own continuation rather than a later timer's; it forbids
# the `legion` tool, so the phase is still open when the turn ends; and it forbids a WAITING line,
# which would buy a quiet wait in place of the block. The run moves no LEGION_STALL_* number: every
# wait of this step is on that first reminder, which the machine hands back to the hook as the turn
# settles. The reminded turn's own settle earns the stall's second reminder, which the server arms
# on the default five-minute timer instead; the next step's instruction usually arrives inside that,
# and were it not to, that reminder is the behaviour under test and the worker answers it as it
# answered this one.
begin stop-hook-reminds-the-open-phase
stall_offset=$(transcript_size)
send_agent "$issue" implementer "Claude backend sandbox proof stall operation: reply with one line of plain prose saying you are standing by for your implementation instruction, and end your turn there. Change nothing, do not call the legion tool in this turn, call no other tool either, and do not begin any line with WAITING. Legion will then remind you that your phase is not complete: answer that reminder with one more line of plain prose saying the proof will instruct you next, under these same rules — no legion tool, no other tool, no WAITING line."
timeout_hook=report_stall_entries
until_true 900 "the implementer's turn to end with its phase open and the reminder to reach it as Stop hook feedback" \
  reminded_or_completed "$stall_offset"
stop_hook_reminded "$stall_offset" ||
  fail "the implementer completed its phase (handoff $(role_handoff "$issue" implementer implementing)) before the proof's stall instruction left it a turn ending with the phase open: it did the work of the task that started its phase, against the proof's instructions, so there was no open phase for the Stop hook to remind"
until_true 600 "the blocked turn to be continued in the same session" reminded_turn_continued "$stall_offset"
timeout_hook=
until_true 300 "the reminded implementer's turn to end" issue_worker_state "$issue" implementer idle
note "the implementer ended a turn with implementing still open: Claude Code carried its Stop hook's block back as Stop hook feedback naming \"$stall_reminder_needle\", and took one more turn on it"
pass

# Check 4. The phase is completed through the `legion` MCP tool, and the handoff commit is on the
# smoke repository under .legion/<issue>/, pushed from the pod's own workspace by the implementer's
# own App.
begin claude-implementer-handoff
marker="claude-sandbox-marker-$run_label-$RANDOM$RANDOM"
send_agent "$issue" implementer "Claude backend sandbox proof implementation operation ($marker): make the smallest one-file change this issue's spec describes in your $repo workspace, commit it on legion/$issue, write and commit the required implementation handoff, push the branch to origin, then call the legion tool's handoff_complete with a concise summary. Do not open a pull request and do not merge."
until_true 1200 "the daemon to record the implementer's handoff_complete" handoff_recorded
commit=$(role_handoff "$issue" implementer implementing)
assert_handoff_committer "$issue" implementer implementing 0
files=$(smoke_commit_files "$commit") || exit 1
grep -q "^\.legion/$issue/" <<<"$files" || fail "the handoff commit $commit changes nothing under .legion/$issue/: $(tr '\n' ' ' <<<"$files")"
legion_handoff_in || fail "the transcript $transcript holds no successful legion handoff_complete call"
until_true 300 "the implementer's turn to end" issue_worker_state "$issue" implementer idle
note "handoff_complete through mcp__legion__legion recorded commit $commit, pushed to $repo under .legion/$issue/"
pass

# Check 5. The pod is deleted, and its disk with it, the transcript's file included
# (docs/kubernetes.md, "The disk dies with its pod"): the transcript lives on as a row of the
# session table, and the replacement resumes the same Claude Code session from that row. Before the
# deletion the transcript's size and the sha256 of those bytes are read in the pod. Once no pod
# holds the deleted one's uid — so its launcher's last save, made as its Claude Code stops, is
# behind it — and before the replacement's turn, the table is read from the devbox: the row's
# byte_len is at least that size, and the first that many bytes of the row as Oh My Pi reads it
# (session_text) have that sha256. The replacement's launcher writes the row back to the
# transcript's path before its Claude Code starts, so the restored file's first bytes, read in the
# new pod, have it too. A replacement launcher already running when the table is read only appends
# to the row, never rewrites it, so the read holds either way. The claim keeps its session and
# session file, the implementer's new generation runs `--resume <id>`, and the resumed session answers out of its
# own context with a marker only its implementation instruction carried. The relaunch is the
# claim's generation in `legion claims list` passing the one it held when the pod was deleted, read
# just before the deletion along with that pod's uid, so the wait and the uid check both name the
# process the run deleted. The replacement fetched Claude Code again at its own launch: its bridge
# runs the binary under the new generation's directory, and the deleted generation's install went
# with the deleted pod's disk. The same pod ran the issue's architect and planner, Oh My Pi roles
# whose sessions are rows of the same table: each runs a later generation in the replacement pod
# and registers there, so an Oh My Pi that loads its session index from the table with the Claude
# Code rows in it still starts.
begin pod-deleted-resumes-the-same-claude-session
size=$(transcript_size)
prefix=$(transcript_prefix_sha "$size")
turns=$(transcript_since 0 | jq -R -s '[split("\n")[] | fromjson? | select(.type == "user" or .type == "assistant")] | length')
generation=$(claim_generation "$issue" implementer) || fail "legion claims list has no generation for the implementer's claim $token"
pod_uid=$(claim_pod_uid "$issue" implementer) || fail "the implementer's claim records no pod uid"
architect_generation=$(claim_generation "$issue" architect) || fail "legion claims list has no generation for the architect's claim $(claim_token "$issue" architect)"
planner_generation=$(claim_generation "$issue" planner) || fail "legion claims list has no generation for the planner's claim $(claim_token "$issue" planner)"
op delete pod "$pod" --wait=false >/dev/null || fail "the operator could not delete pod $pod"
until_true 600 "the deleted pod $pod (uid $pod_uid) to be gone" pod_uid_gone "$pod" "$pod_uid"
session_text "$transcript" >"$work/table-transcript" ||
  fail "the session table does not give the implementer's transcript $transcript once pod $pod is gone (the reason is above)"
held_len=$(session_byte_len "$transcript") || fail "the session table gives no byte_len for the held transcript $transcript (the reason is above)"
[ "$held_len" -ge "$size" ] ||
  fail "the session table's row for $transcript has byte_len $held_len, short of the $size bytes the transcript held in pod $pod before its deletion"
held_size=$(wc -c <"$work/table-transcript")
[ "$held_size" -ge "$size" ] ||
  fail "the session table's row for $transcript and its parts come to $held_size bytes, short of the $size bytes the transcript held in pod $pod before its deletion"
[ "$(head -c "$size" "$work/table-transcript" | sha256sum | cut -d' ' -f1)" = "$prefix" ] ||
  fail "the first $size bytes of the session table's row for $transcript are not the transcript's in pod $pod before its deletion (sha256 $prefix)"
note "pod $pod (uid $pod_uid) is gone; the session table's row for $transcript has byte_len $held_len, and its first $size bytes are the transcript's from before the deletion (sha256 $prefix)"
until_true 900 "the implementer to relaunch in a replacement pod" claim_relaunched "$issue" implementer "$generation"
until_true 600 "the relaunched implementer to register" issue_worker_live "$issue" implementer
claim=$(claim_view "$issue" implementer)
[ "$(jq -r .session <<<"$claim")" = "$session" ] ||
  fail "the relaunch registered session $(jq -r .session <<<"$claim"), want $session"
[ "$(claim_session_file "$issue" implementer)" = "$transcript" ] ||
  fail "the relaunch registered session file $(claim_session_file "$issue" implementer), want $transcript"
new_pod=$(implementer_pod)
new_uid=$(claim_pod_uid "$issue" implementer)
[ "$new_uid" != "$pod_uid" ] || fail "the implementer came back in pod uid $new_uid, the one the run deleted"
bridge_argv=$(claude_argv "$new_pod" bridge) || fail "the replacement pod runs no Claude Code under legion claude-bridge"
argv_has_pair "$bridge_argv" --resume "$session" ||
  fail "the relaunched claude was not started with --resume $session: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-300)"
new_generation=$(claim_generation "$issue" implementer) || fail "legion claims list has no generation for the relaunched claim $token"
reinstalled=$(claude_code_binary "$new_generation")
[ "$(argv_value "$bridge_argv" --)" = "$reinstalled" ] ||
  fail "the relaunched bridge runs $(argv_value "$bridge_argv" --), not the Claude Code generation $new_generation's start script installed, $reinstalled"
pod_exec "$new_pod" implementer test -x "$reinstalled" || fail "the replacement pod holds no executable Claude Code at $reinstalled"
if pod_exec "$new_pod" implementer test -e "$claude_code_root/g$generation"; then
  fail "the deleted generation $generation's Claude Code install $claude_code_root/g$generation is in the replacement pod, whose disk is its own"
fi
[ "$(transcript_prefix_sha "$size")" = "$prefix" ] ||
  fail "the transcript the replacement pod $new_pod restored at $transcript does not begin with the $size bytes the table and the deleted pod held (sha256 $prefix)"
for named in "architect:$architect_generation" "planner:$planner_generation"; do
  role=${named%%:*}
  until_true 900 "the $role to relaunch past generation ${named#*:}" claim_relaunched "$issue" "$role" "${named#*:}"
  until_true 600 "the relaunched $role to register" issue_worker_live "$issue" "$role"
  role_uid=$(claim_pod_uid "$issue" "$role") || fail "the relaunched $role's claim records no pod uid"
  [ "$role_uid" = "$new_uid" ] || fail "the relaunched $role runs in pod uid $role_uid, not the replacement pod's $new_uid"
  note "the $role registered at generation $(claim_generation "$issue" "$role") (was ${named#*:}) in the replacement pod (uid $new_uid)"
done
send_agent "$issue" implementer "Claude backend sandbox proof after a relaunch: reply with one line starting WAITING that quotes the proof marker your implementation instruction in this session carried. Change nothing and call no tool."
until_true 600 "the resumed implementer to quote its implementation instruction's marker" resumed_reply_names "$size" "$marker"
note "deleted pod $pod (uid $pod_uid, generation $generation): generation $new_generation runs $reinstalled --resume $session in pod $new_pod (uid $new_uid), generation $generation's install gone with the deleted pod's disk"
note "the transcript kept its $turns earlier turns ($size bytes, unchanged in the session table and in the file the replacement restored from it) and the resumed session quoted $marker"
pass

ok=1
echo "claude backend sandbox e2e: PASS"
