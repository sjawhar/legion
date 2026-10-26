#!/usr/bin/env bash
# The Claude Code backend's live proof under Legion's Kubernetes runtime, on a real cluster. A real
# Go daemon on the devbox, whose legion.yaml puts the implementer on Claude Code
# (`worker_backends: {implementer: claude}`) and whose runtime is `kubernetes`, drives one scratch
# Dispatch issue: a real Oh My Pi architect and planner take it to implementing in the issue's pod,
# and the implementer then runs in that pod's own container as `legion claude-bridge -- claude -p …`
# — the Claude Code its pod's start script installed at launch from the image's pin (the image
# carries the pin and no Claude Code), the image's `@sjawhar/claude-envoy` plugin as its MCP server
# and its Legion plugin, its `mcp.json` written by the launcher into the generation's private
# directory, and its `CLAUDE_CONFIG_DIR` a directory of the role's state home on the issue's volume.
# The proof holds, in this order, that the Claude Code implementer's pod launches and its claim
# registers that pod's Claude Code transcript as its session; that the container's command line
# holds `claude-bridge --` and `--session-id <id>`, and runs the Claude Code the generation's start
# script installed into its own directory on the issue's volume — the pinned version, with no
# /usr/local/bin/claude in the image; that the endless phase reminder reaches it when a turn
# of its ends with its phase still open; that it completes its phase through the `legion` MCP tool's
# handoff_complete with a pushed `.legion/<issue>/` handoff commit; and that, the pod deleted, its
# replacement installs its own generation's copy, drops the deleted one's, and resumes the same
# Claude Code session (`--resume <id>`, the transcript's earlier bytes unchanged on the issue's
# volume, and an earlier marker quoted back out of its context).
#
# It is the tmux proof's rig (scripts/e2e/claude-backend-tmux.sh) on the cluster proof's cluster
# (scripts/e2e/stage4a-sandbox-runtime.sh): scratch Postgres, NATS, Envoy listener and Dispatch from
# lib/rig.sh, `admission_cap: 1`, `gates.design: off`, the smoke repository sjawhar/legion-smoke
# where the implementer pushes a branch and opens no pull request — and, for the runtime, an
# existing cluster named by LEGION_E2E_RUNTIME_CONTEXT, the namespace `legion`, and
# LEGION_E2E_IMAGE pinned by digest. It is the first rig that runs `runtime: kubernetes`, so what a
# tmux proof reads from /proc and a pane's own files this one reads with `kubectl exec` and
# `kubectl get` against the issue's pod.
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
# run's five services on the devbox, as Stage 4a's does:
#   - `aws-private-ip`: the pods reach the devbox at its private address (read from instance
#     metadata), so the five services bind that address on five ports the devbox's security group
#     admits from the Legion nodes, which is five of the six that range holds (the README's
#     port-admission note).
#   - `bridge`: for a cluster whose pods cannot reach the devbox at all (no tailnet, no routable
#     private address — GKE's shape). The five services bind the devbox's loopback, and
#     lib/bridge-tunnel.sh stands up a scratch pod+Service of the run's own in the namespace
#     (bridge-<the run's label>) whose reverse tunnel forwards each of the five ports back to it;
#     the pods dial the Service's cluster-DNS name. The daemon and the pods read one legion.yaml,
#     so its URLs name that Service, which the devbox cannot resolve: the daemon alone runs in a
#     private mount namespace whose /etc/hosts is the devbox's plus `127.0.0.1 <that name>`. Root
#     makes it (`sudo -n unshare --mount`, since the devbox refuses unprivileged user namespaces)
#     and drops back to the devbox user and its groups (`setpriv`) before the daemon runs, handing
#     it the environment it would have had (through an owner-only file, never an argument); nothing
#     outside that process changes, which prerequisites proves before the run creates anything. This
#     mode needs passwordless sudo.
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
# - LEGION_E2E_STORAGE_CLASS (required) is the issue volume's StorageClass: the cluster has no
#   default to fall back on, and the implementer's resume is a read of that volume.
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
# - LEGION_E2E_DAEMON_PORT, LEGION_E2E_WORKER_STREAM_PORT, LEGION_E2E_DISPATCH_PORT,
#   LEGION_E2E_ENVOY_PORT and LEGION_E2E_NATS_PORT (all required, all distinct, in both modes) are
#   the ports the daemon's API, its worker stream, the scratch Dispatch, the scratch Envoy listener
#   and the scratch NATS bind on the devbox: its private address under aws-private-ip, its loopback
#   under bridge. Every one of the five is dialled from inside a pod — the launchers dial the worker
#   stream, each agent's claim registration dials LEGION_DAEMON_URL, and each agent dials
#   DISPATCH_URL, ENVOY_URL and ENVOY_NATS_URL — so each must be, under aws-private-ip, a port the
#   devbox's security group admits from the Legion nodes, and under bridge a free port on the
#   devbox, which the bridge forwards. There is no default: a default would pick a port another
#   lane holds, and the repository carries no deployment's own.
# - CLAUDE_SANDBOX_E2E_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) holds the
#   transcript of the run, the daemon's log, the legion.yaml and pod fixture the run wrote, the
#   implementer's Claude Code transcript, and the namespace snapshots.
#
# Legion's role Apps resolve as the tmux proof's do: the daemon runs `secrets
# LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -- …` and `secrets GH_REVIEW_APP_PRIVATE_KEY_B64 -- …` as
# each App's private_key_command, never this shell. The proof human is the devbox's ordinary gh
# acting as sjawhar-agent[bot] (require_proof_human), so run it from the operator's own Oh My Pi
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
operator_route=$root/deploy/kubernetes/operator-route
route_configmap=legion-operator-route-$run_label
providers_secret=legion-$run_label-providers
# bridge_name is deterministic from the run's label alone (lib/bridge-tunnel.sh's pod and Service),
# so the cluster-DNS host pods dial under LEGION_E2E_POD_REACHABILITY=bridge is known before the
# bridge itself is ever stood up.
bridge_name=bridge-$run_label
pg_container="legion-e2eccs-pg-$$"
nats_container="legion-e2eccs-nats-$$"
daemon_log=$evidence/logs/daemon.log
# host is the name the pods dial the run's five services by, and bind_host the address those
# services bind and the devbox itself dials them on, both set in prerequisites: under aws-private-ip
# both are the devbox's private address, read from instance metadata; under bridge host is the
# bridge Service's cluster-DNS name and bind_host loopback. legion.yaml carries host alone — under
# bridge the daemon reads it through its private /etc/hosts — and pod_dispatch_url is Dispatch at
# host, as the pods are handed it.
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
pod_exec() {
  local pod=$1 role=$2
  shift 2
  op exec "$pod" -c "$role" -- "$@"
}
# implementer_pod prints the pod the implementer's claim names now, which is its Sandbox's own name
# and so the same word across the deletion step: a replacement pod is a new uid under that name,
# which is why every check of the replacement compares uids and never names.
implementer_pod() { claim_sandbox "$issue" implementer; }
# agent_pid_sh is the POSIX sh that finds a role's agent in its container, Oh My Pi's or Claude
# Code's: the process whose argv[0] is omp or claude and that runs under the role's `legion
# worker-shim` — directly for Oh My Pi, under `legion claude-bridge` for Claude Code, which is why
# the walk climbs rather than reading one parent. It exits 1 naming both pids when two such
# processes run and saying so when none does; otherwise it leaves the pid in $pid.
# shellcheck disable=SC2016  # expanded by the pod's shell
agent_pid_sh='
    found=
    for d in /proc/[0-9]*; do
      argv0=$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 1p)
      case "${argv0##*/}" in omp | claude) ;; *) continue ;; esac
      up=$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)
      shimmed=
      hops=0
      while [ -n "$up" ] && [ "$up" -gt 1 ] && [ "$hops" -lt 4 ]; do
        if tr "\0" " " 2>/dev/null <"/proc/$up/cmdline" | grep -q "worker-shim "; then shimmed=1; break; fi
        up=$(sed -n "s/^PPid:[[:space:]]*//p" "/proc/$up/status" 2>/dev/null)
        hops=$((hops + 1))
      done
      [ -n "$shimmed" ] || continue
      if [ -n "$found" ]; then
        echo "more than one agent under a legion worker-shim: pids ${found#/proc/} and ${d#/proc/}" >&2
        exit 1
      fi
      found=$d
    done
    if [ -z "$found" ]; then
      echo "no Oh My Pi or Claude Code under a legion worker-shim" >&2
      exit 1
    fi
    pid=${found#/proc/}'
# claude_pid_sh is agent_pid_sh for the Claude Code lane alone, and it names the bridge too: $pid is
# the pod's Claude Code and $bridge its parent, the `legion claude-bridge` the launcher's agent
# command runs it from. A container whose agent is not Claude Code under the bridge exits 1.
# shellcheck disable=SC2016  # expanded by the pod's shell
claude_pid_sh='
    found=
    for d in /proc/[0-9]*; do
      argv0=$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 1p)
      [ "${argv0##*/}" = claude ] || continue
      up=$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)
      [ -n "$up" ] || continue
      tr "\0" " " 2>/dev/null <"/proc/$up/cmdline" | grep -q " claude-bridge " || continue
      if [ -n "$found" ]; then
        echo "more than one Claude Code under legion claude-bridge: pids ${found#/proc/} and ${d#/proc/}" >&2
        exit 1
      fi
      found=$d
      bridge=$up
    done
    if [ -z "$found" ]; then
      echo "no Claude Code under legion claude-bridge" >&2
      exit 1
    fi
    pid=${found#/proc/}'
# pod_env POD ROLE prints the environment of ROLE's agent in POD, one NAME=value a line, read from
# /proc in the role's container. The container's spec is not where a role's environment is: it
# carries what the kubelet resolves, and every other variable reaches the role in its launcher's
# start command.
pod_env() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$1" "$2" sh -c "$agent_pid_sh"'
    tr "\0" "\n" <"/proc/$pid/environ"'
}
# claude_argv POD WHICH prints, one argument a line, the argv of the implementer container's Claude
# Code (`agent`) or of the `legion claude-bridge` that runs it (`bridge`).
claude_argv() {
  local which=$2
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$1" implementer sh -c "$claude_pid_sh"'
    case "$1" in bridge) read_from=$bridge ;; *) read_from=$pid ;; esac
    tr "\0" "\n" <"/proc/$read_from/cmdline"' sh "$which"
}
# claude_code_root is where the implementer's start script installs Claude Code, one directory per
# generation, under its state home on the issue's volume (claudeCodeRoot,
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

# ---- the runtime seam: a claim's session and its issue's workspace live in the issue's pod -------

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
# claim_session_text ISSUE ROLE prints the claim's session as the pod holds it.
claim_session_text() {
  local issue=$1 role=$2 file pod
  file=$(claim_session_file "$issue" "$role") || return 1
  pod=$(claim_sandbox "$issue" "$role") || return 1
  pod_exec "$pod" "$role" cat -- "$file"
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
    printf 'no readable agent environment in its %s container: %s\n' "$role" "$(tr '\n' ' ' <<<"$env" | cut -c1-300)"
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

# ---- the implementer's transcript, as the pod holds it -------------------------------------------

# transcript_size prints the transcript's size in bytes, and transcript_prefix_sha SIZE the sha256
# of its first SIZE bytes: the resume's own before-and-after, computed in the pod, since the file is
# on the issue's volume and no host of the run can open it.
transcript_size() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$(implementer_pod)" implementer sh -c 'wc -c <"$1"' sh "$transcript" | tr -d '[:space:]'
}
transcript_prefix_sha() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$(implementer_pod)" implementer sh -c 'head -c "$1" -- "$2" | sha256sum' sh "$1" "$transcript" |
    cut -d' ' -f1
}
# transcript_since OFFSET prints the transcript's bytes past OFFSET, the tail every predicate below
# judges: a poll reads the new entries alone, not a session that grows all run.
transcript_since() {
  # shellcheck disable=SC2016  # expanded by the pod's shell
  pod_exec "$(implementer_pod)" implementer sh -c 'tail -c "+$(($1 + 1))" -- "$2"' sh "$1" "$transcript"
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
    start_process listener env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
      -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-listener"
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
    start_process dispatch env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
      -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-dispatch"
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
# create_providers_secret is the one Secret of the run: the Anthropic key, under the name
# provider_keys hands the pods. It reaches kubectl from the run's 0600 copy of the key file, never
# an argument, and is never printed.
create_providers_secret() {
  op create secret generic "$providers_secret" --from-file=ANTHROPIC_API_KEY="$work/claude-model-key" \
    --dry-run=client -o yaml | op label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
    fail "the operator could not create Secret $providers_secret"
  note "[operator] Secret $providers_secret: ANTHROPIC_API_KEY (the run's own key, not printed), label legion.dev/project=$run_label"
}

# ---- the daemon ------------------------------------------------------------------------------------

# The settings under test are worker_backends and runtime.kubernetes; the design gate is off, so the
# architect's gate registration starts planning with no human approval. provider_keys names a key of
# the providers Secret here, not a secretsd key: under this runtime the daemon resolves none itself.
# It names no `runtime.kubernetes.plugins` pair: the pods load the image's own plugins, a locally
# built `legion` links no release pair, and nothing here relaunches a worker on another plugin
# contract, so the daemon's boot says `plugins: none` and the proof is unaffected.
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
github_apps:
  implement:
    app_id: "3202636"
    private_key_command: "secrets LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
  review:
    app_id: "3202653"
    private_key_command: "secrets GH_REVIEW_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${GH_REVIEW_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
runtime:
  kubernetes:
    namespace: $namespace
    image: $image
    storage_class: $storage_class
    issue_volume: 20Gi
    kubeconfig: $runtime_kubeconfig
    context: $runtime_context
    pod:
$(sed 's/^/      /' "$work/pod.yml")
EOF
  cp "$work/legion.yaml" "$evidence/legion.yaml"
}
daemon_answers_or_exited() { ! kill -0 "$daemon_pid" 2>/dev/null || curl -fsS "http://$bind_host:$port_daemon/healthz"; }
report_boot() { note "the daemon log's tail: $(tail -5 "$daemon_log" | cut -c1-300 | tr '\n' '|')"; }
# The daemon runs with the caller's environment less these, under either reachability.
daemon_env_unset=(-u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u NATS_DAEMON_NKEY_SEED -u NATS_DAEMON_NKEY_SEED_FILE
  -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -u GH_AGENT_APP_PRIVATE_KEY_B64
  -u GH_REVIEW_APP_PRIVATE_KEY_B64 -u ANTHROPIC_API_KEY)
# private_hosts_run PIDFILE COMMAND... runs COMMAND as the daemon runs under bridge: in a private
# mount namespace root makes (the devbox refuses unprivileged user namespaces) whose /etc/hosts is
# $work/hosts — the devbox's own plus `127.0.0.1 $host` — as the devbox user with that user's
# groups, and with the environment the daemon has under aws-private-ip, the caller's less
# daemon_env_unset. sudo resets the environment, so it travels NUL-separated in an owner-only file,
# never on an argv (any user can read another process's /proc/<pid>/cmdline): the namespace's root
# bash reads the file's well-formed NAME=value entries (an exported bash function's name is not
# one), removes it, clears its own environment and exports those entries alone, which setpriv keeps.
# The paths and ids reach that bash as arguments, never as script text. sudo forks once; from there
# every link execs — unshare (no --fork), bash, setpriv — so the pid the bash writes to PIDFILE is
# COMMAND's own. It execs sudo itself, so it is called in a subshell: a command substitution or a
# background job.
# shellcheck disable=SC2016  # expanded by the namespace's bash
private_hosts_sh='
  set -eu
  env_file=$1 hosts=$2 pid_file=$3 uid=$4 gid=$5
  shift 5
  entries=()
  while IFS= read -r -d "" entry; do
    if [[ $entry == *=* && ${entry%%=*} =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then entries+=("$entry"); fi
  done <"$env_file"
  rm -f "$env_file"
  mount --bind "$hosts" /etc/hosts
  setpriv=$(command -v setpriv)
  for name in $(compgen -e); do unset "$name" 2>/dev/null || true; done
  # A name bash holds read-only (UID, SHELLOPTS) cannot be imported, and no daemon reads one.
  for entry in "${entries[@]}"; do export "$entry" 2>/dev/null || true; done
  echo "$$" >"$pid_file"
  exec "$setpriv" --reuid="$uid" --regid="$gid" --init-groups -- "$@"'
private_hosts_run() {
  local pid_file=$1
  shift
  (umask 077 && env "${daemon_env_unset[@]}" -0 >"$work/private-hosts-env") &&
    exec sudo -n unshare --mount --propagation private -- bash -c "$private_hosts_sh" bash \
      "$work/private-hosts-env" "$work/hosts" "$pid_file" "$(id -u)" "$(id -g)" "$@" </dev/null
}
# daemon_started_or_gone LAUNCHER: the daemon's pid file names a process this user can signal — its
# private mount namespace has dropped to the devbox user and execed the daemon — or LAUNCHER, the
# background sudo, has exited. sudo runs as root, so its own end is read from /proc, not kill -0.
daemon_started_or_gone() {
  local pid
  pid=$(cat "$work/daemon.pid" 2>/dev/null) && [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && return 0
  [ ! -e "/proc/$1" ]
}
# kill_harness is lib/bridge-tunnel.sh's: its watchdog stops the daemon before it ends the run. The
# watchdog forked before the daemon started, so its own daemon_pid is empty and the daemon's pid
# file names it. The watchdog runs under errexit, so nothing here may fail.
kill_harness() {
  local pid=$daemon_pid
  if [ -s "$work/daemon.pid" ]; then pid=$(cat "$work/daemon.pid" 2>/dev/null) || pid=; fi
  if [ -n "$pid" ] && signalable "$pid" 2>/dev/null; then kill -TERM "$pid" 2>/dev/null || true; fi
  return 0
}
start_daemon() {
  local launcher
  # The daemon's own two ports alone: the run's NATS, Envoy listener and Dispatch hold the other
  # three by now.
  ports_free "$port_daemon" "$port_worker_stream"
  if [ "$pod_reachability" = bridge ]; then
    private_hosts_run "$work/daemon.pid" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 7>&- &
    launcher=$!
    until_true 30 "the daemon's private mount namespace to start it as $(id -un)" daemon_started_or_gone "$launcher"
    daemon_pid=$(cat "$work/daemon.pid" 2>/dev/null) || daemon_pid=
    case "$daemon_pid" in
      '' | *[!0-9]*)
        fail "the daemon's private mount namespace exited before it started the daemon: $(tail -3 "$daemon_log" | cut -c1-300 | tr '\n' '|')"
        ;;
    esac
  else
    env "${daemon_env_unset[@]}" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 7>&- &
    daemon_pid=$!
  fi
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

# collect_transcripts copies the implementer's Claude Code transcript out of its pod while the pod
# still exists; a run that failed before any pod had one has nothing to copy.
collect_transcripts() {
  local pod
  [ -n "$transcript" ] || return 0
  pod=$(implementer_pod 2>/dev/null) || return 0
  pod_exec "$pod" implementer cat -- "$transcript" >"$evidence/transcripts/implementer.jsonl" 2>/dev/null || true
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
  # The daemon's environment file is its private mount namespace's to remove; a start that failed
  # before it read the file leaves it here.
  rm -f "$work/claude-model-key" "$work/claude-key-header" "$work/private-hosts-env"
  # The daemon goes first: a live one would launch the Sandboxes the teardown is deleting.
  collect_transcripts
  stop_pid "$daemon_pid"
  # The bridge next, before the sweep below kills its chisel client, which its watchdog would read as
  # a dead tunnel; a no-op when bridge_start never ran.
  bridge_stop
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1 || true
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
  fail "LEGION_E2E_STORAGE_CLASS is unset: name the issue volume's StorageClass (the cluster has no default to fall back on)"
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
  "LEGION_E2E_DISPATCH_PORT:$port_dispatch" "LEGION_E2E_ENVOY_PORT:$port_listener" "LEGION_E2E_NATS_PORT:$port_nats"; do
  case "${named#*:}" in
    '') fail "${named%%:*} is unset: name $port_kind, since every pod dials it" ;;
    *[!0-9]*) fail "${named%%:*} must be a port number, not '${named#*:}'" ;;
  esac
done
[ "$(printf '%s\n' "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats" | sort -u | wc -l)" = 5 ] ||
  fail "the five ports must differ: daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats"
tools=(go docker jq curl ss kubectl bun secrets gh)
# Under bridge the daemon runs in a root-made private mount namespace (private_hosts_run), proven below.
[ "$pod_reachability" != bridge ] || tools+=(sudo setpriv unshare getent sha256sum)
for tool in "${tools[@]}"; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
refuse_leftovers legion-e2eccs
ports_free "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats"
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
for key in LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64; do
  status=$(secrets get "$key" --no-request 2>&1) || fail "secrets get $key does not resolve: $status"
  jq -e --arg key "$key" '.key == $key and .tier == "agent"' <<<"$status" >/dev/null 2>&1 ||
    fail "secrets get $key reports $status, want the agent tier"
  note "secrets get $key: $status"
done
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
    # namespace. The daemon dials it too, through its private /etc/hosts, which this proves before
    # the run creates anything: the wrapper runs its command as the devbox user under the pid it
    # records, the name resolves to loopback inside it and nowhere outside it, and the devbox's own
    # /etc/hosts is unchanged.
    bind_host=127.0.0.1
    host="$bridge_name.$namespace.svc.cluster.local"
    sudo_said=$(sudo -n true 2>&1) ||
      fail "passwordless sudo is required under LEGION_E2E_POD_REACHABILITY=bridge: the daemon alone runs in a private mount namespace whose /etc/hosts resolves $host, which only root can make on a devbox that refuses unprivileged user namespaces; sudo -n true said: $sudo_said"
    hosts_sha=$(sha256sum /etc/hosts | cut -d' ' -f1)
    { cat /etc/hosts && printf '\n127.0.0.1 %s\n' "$host"; } >"$work/hosts"
    # shellcheck disable=SC2016  # expanded by the probe's shell
    inside=$(private_hosts_run "$work/probe.pid" sh -c 'echo "$$"; id -u; id -g; getent hosts "$1"' sh "$host" 2>&1) ||
      fail "the daemon's private mount namespace could not run its probe: $(tr '\n' ' ' <<<"$inside" | cut -c1-300)"
    probe_pid=$(cat "$work/probe.pid" 2>/dev/null) || probe_pid=
    rm -f "$work/probe.pid"
    [ "$(sed -n 1p <<<"$inside")" = "$probe_pid" ] ||
      fail "the daemon's private mount namespace recorded pid ${probe_pid:-<none>}, but its command ran as pid $(sed -n 1p <<<"$inside"): a link of the wrapper forks, so the pid it records for the daemon would not be the daemon's"
    [ "$(sed -n 2,3p <<<"$inside" | tr '\n' ' ')" = "$(id -u) $(id -g) " ] ||
      fail "the daemon's private mount namespace ran its command as uid and gid $(sed -n 2,3p <<<"$inside" | tr '\n' ' '), want $(id -u) $(id -g), the devbox user's own"
    inside_hosts=$(sed -n 4p <<<"$inside")
    awk -v host="$host" '$1 == "127.0.0.1" { for (i = 2; i <= NF; i++) if ($i == host) found = 1 } END { exit !found }' <<<"$inside_hosts" ||
      fail "inside the daemon's private mount namespace, getent hosts $host printed '$inside_hosts', want 127.0.0.1"
    outside_status=0
    outside=$(getent hosts "$host") || outside_status=$?
    [ "$outside_status" != 0 ] ||
      fail "$host resolves outside the daemon's private mount namespace ($outside): the name must be the daemon's alone"
    [ "$(sha256sum /etc/hosts | cut -d' ' -f1)" = "$hosts_sha" ] ||
      fail "the devbox's /etc/hosts changed across the private mount namespace's probe: sha256 $hosts_sha before, $(sha256sum /etc/hosts | cut -d' ' -f1) after"
    note "the daemon's private mount namespace (sudo -n unshare --mount, setpriv): its probe ran as pid $probe_pid, the pid it recorded, uid $(id -u) gid $(id -g); inside it, getent hosts $host: $inside_hosts"
    note "outside it, getent hosts $host resolves nothing (exit $outside_status); the devbox's /etc/hosts sha256 $hosts_sha before and after"
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
  note "pod reachability aws-private-ip: the devbox's private address $host: daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats, each $port_kind"
else
  note "pod reachability bridge: the devbox's loopback $bind_host, advertised to pods as $host (the bridge's Service): daemon $port_daemon, worker stream $port_worker_stream, Dispatch $port_dispatch, Envoy $port_listener, NATS $port_nats, each $port_kind"
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
(cd "$root/packages/daemon" && go build -o "$work/legion" ./cmd/legion)
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
  bridge_start "$port_daemon" "$port_worker_stream" "$port_dispatch" "$port_listener" "$port_nats"
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
read_app_logins
probe=$(log_lines "sandbox runtime: the worker image passed its probe" | tail -1)
[ -n "$probe" ] || fail "the daemon booted with no image probe pass logged; see $daemon_log"
note "image probe: $(jq -c '{image, model, sandbox}' <<<"$probe")"
if [ "$pod_reachability" = aws-private-ip ]; then
  note "the Go daemon answers http://$bind_host:$port_daemon/healthz and hosts its worker stream on tcp://$host:$port_worker_stream"
else
  note "the Go daemon (pid $daemon_pid, in its private mount namespace) answers http://$bind_host:$port_daemon/healthz and hosts its worker stream on the devbox, advertised to pods as http://$host:$port_daemon and tcp://$host:$port_worker_stream"
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
send_agent "$issue" planner "Claude backend sandbox proof planning operation: write the required plan handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$issue" implementing
assert_handoff_committer "$issue" planner planning 0
pass

# Check 1. The implementer's pod launches and its claim registers a Claude Code transcript as its
# session: the file Claude Code records under the CLAUDE_CONFIG_DIR the launch places on the issue's
# volume, under the role's own state home, which the container's Claude Code says it was told. That
# is read from the agent's own environment (pod_env), never the pod's spec: the runtime sets only the
# kubelet-resolved variables there, and carries CLAUDE_CONFIG_DIR, like every plain NAME=value, in
# the launcher's start command (launchEnvironment in packages/daemon/internal/runtime/sandbox).
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
  fail "pod $pod has no readable agent environment in its implementer container: $(tr '\n' ' ' <<<"$agent_env" | cut -c1-300)"
config_dir=$(sed -n 's/^CLAUDE_CONFIG_DIR=//p' <<<"$agent_env")
[ "$config_dir" = /legion/scratch/state/implementer/claude ] ||
  fail "the implementer's Claude Code is told CLAUDE_CONFIG_DIR=${config_dir:-<unset>}, want /legion/scratch/state/implementer/claude, a directory of its role's state home on the issue's volume"
case "$transcript" in
  "$config_dir/projects/"*"/$session.jsonl") ;;
  *) fail "the implementer registered session file $transcript, want $config_dir/projects/<cwd>/$session.jsonl" ;;
esac
[ "$(pod_exec "$pod" implementer sh -c 'wc -c <"$1"' sh "$transcript" | tr -d '[:space:]')" -gt 0 ] ||
  fail "the implementer's transcript $transcript is missing or empty in pod $pod"
note "implementer claim $token: generation $generation, pod $pod (uid $pod_uid), Claude Code session $session"
note "its transcript is $transcript, on the issue's volume under CLAUDE_CONFIG_DIR=$config_dir"
pass

# Check 2. The container's command line is the Claude Code launch: `legion claude-bridge --` running
# the Claude Code this generation's start script installed on this generation's session, with the
# MCP document the launcher wrote into the generation's private directory as its only
# `--mcp-config`. The image carries no Claude Code, only its pin, so the fetch has to have happened
# for the worker to run at all, and this holds that it is the fetch the proof expects: the bridge
# names, and the process under it is, the binary under this generation's own directory of the
# implementer's install root on the issue's volume (the claim's generation, which Check 1 read),
# never /usr/local/bin/claude, which the image does not carry; and that binary, run in the pod,
# prints the version the image's pin names, which is what the start script held it to before it
# execed.
begin claude-pod-runs-the-bridge
bridge_argv=$(claude_argv "$pod" bridge) || fail "the implementer container runs no Claude Code under legion claude-bridge"
argv_has_pair "$bridge_argv" claude-bridge -- ||
  fail "the implementer container's command line does not run claude-bridge --: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-300)"
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
pod_exec "$pod" implementer test -f "$mcp" || fail "the launcher file $mcp the launch reads its MCP document from is not in the pod"
note "pod $pod, container implementer: $(tr '\n' ' ' <<<"$bridge_argv" | cut -c1-200)"
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
  stop_hook_reminded "$stall_offset"
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
files=$(timeout 60 gh api "repos/$repo/commits/$commit" --jq '.files[].filename') ||
  fail "the handoff commit $commit the implementer reported is not on $repo"
grep -q "^\.legion/$issue/" <<<"$files" || fail "the handoff commit $commit changes nothing under .legion/$issue/: $(tr '\n' ' ' <<<"$files")"
legion_handoff_in || fail "the transcript $transcript holds no successful legion handoff_complete call"
until_true 300 "the implementer's turn to end" issue_worker_state "$issue" implementer idle
note "handoff_complete through mcp__legion__legion recorded commit $commit, pushed to $repo under .legion/$issue/"
pass

# Check 5. The pod is deleted and the replacement resumes the same Claude Code session: the claim
# keeps its session and session file, the new container runs `--resume <id>`, the transcript's
# earlier bytes are unchanged on the issue's volume, and the resumed session answers out of its own
# context with a marker only its implementation instruction carried. The relaunch is the claim's
# generation in `legion claims list` passing the one it held when the pod was deleted, read just
# before the deletion along with that pod's uid, so the wait and the uid check both name the
# process the run deleted. The replacement fetched Claude Code again at its own launch: its bridge
# runs the binary under the new generation's directory, and the deleted generation's install, which
# the new start script drops before it installs, is gone from the issue's volume.
begin pod-deleted-resumes-the-same-claude-session
size=$(transcript_size)
prefix=$(transcript_prefix_sha "$size")
turns=$(transcript_since 0 | jq -R -s '[split("\n")[] | fromjson? | select(.type == "user" or .type == "assistant")] | length')
generation=$(claim_generation "$issue" implementer) || fail "legion claims list has no generation for the implementer's claim $token"
pod_uid=$(claim_pod_uid "$issue" implementer) || fail "the implementer's claim records no pod uid"
op delete pod "$pod" --wait=false >/dev/null || fail "the operator could not delete pod $pod"
until_true 900 "the implementer to relaunch in a replacement pod" sh -c \
  "'$work/legion' claims list --json --config '$work/legion.yaml' --operator-token-file '$work/operator-token' |
    jq -e --arg token '$token' --argjson gen '$generation' '.claims[] | select(.token == \$token) | .generation > \$gen' >/dev/null"
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
  fail "the deleted generation $generation's Claude Code install $claude_code_root/g$generation survived the relaunch on the issue's volume"
fi
[ "$(transcript_prefix_sha "$size")" = "$prefix" ] || fail "the transcript's first $size bytes changed across the relaunch"
send_agent "$issue" implementer "Claude backend sandbox proof after a relaunch: reply with one line starting WAITING that quotes the proof marker your implementation instruction in this session carried. Change nothing and call no tool."
until_true 600 "the resumed implementer to quote its implementation instruction's marker" resumed_reply_names "$size" "$marker"
note "deleted pod $pod (uid $pod_uid, generation $generation): generation $new_generation runs $reinstalled --resume $session in pod $new_pod (uid $new_uid), generation $generation's install dropped"
note "the transcript kept its $turns earlier turns ($size bytes unchanged on the issue's volume) and the resumed session quoted $marker"
pass

ok=1
echo "claude backend sandbox e2e: PASS"
