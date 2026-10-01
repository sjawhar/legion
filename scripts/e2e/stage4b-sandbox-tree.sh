#!/usr/bin/env bash
# Stage 4b's gate for the Go coordinator: the Go daemon drives real issue trees on the Agent Sandbox
# runtime, in the production cluster's namespace `legion`, against production Dispatch, the
# production Envoy listener and production NATS, in the disposable Dispatch project LEGSMOKE and the
# smoke repository sjawhar/legion-smoke. The daemon runs on the devbox under the Legion daemon's
# restricted identity; its pods run the worker image under test and dial its worker stream on the
# devbox's private address. Operator steps (exec into a pod, a Secret's hash, the namespace list,
# the controls' pods) use the admin context. It is the production-EKS driver LEGION-206
# Requirement 12 asks for.
#
# Three roots are set todo under admission_cap 2. Tree 1 runs the whole workflow with real agents to
# `done`, lingers, and closes. Tree 2 runs through its planner beside tree 1's implementer, on its own
# node, carrying the repository-configuration fixture, and is then moved to backlog. Tree 3 is
# admitted when tree 2 leaves the line, supplies the held phase the controller checkpoint needs, and
# is taken out from an operator shell. Tree 4 is admitted once tree 3 has left, supplies a planner
# killed mid-turn and an implementer killed until it is held, and is taken out the same way. Each
# checkpoint prints `== <name>`, what it observed with the source revision, the image digest and the
# plugin version recorded once in `run.json`, and `CHECK <name>: PASS`. The first that fails ends the
# run non-zero with `CHECK <name>: FAIL`, naming it; a checkpoint that cannot run prints
# `CHECK <name>: BLOCKED`, naming the command that failed and the record it checked.
#
# The proof human's GitHub writes (the merge, the teardown's closes and branch deletes, and the
# fixture push) are the devbox gh's and its git credential helper's, acting as the sjawhar-agent
# App: run the driver from the operator's own Oh My Pi session, not a Legion pane, with no personal
# GH_TOKEN in its environment. `prerequisites` refuses to start otherwise (require_proof_human,
# lib/workflow.sh).
#
# Inputs:
# - LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
#   ~/.kube/legion-daemon-production) name the restricted identity the daemon runs as.
# - LEGION_E2E_OPERATOR_CONTEXT (default production) names the admin context.
# - LEGION_E2E_IMAGE (required) is the worker image, by digest.
# - LEGION_E2E_MODEL_GATEWAY_URL (required) is the model gateway's Anthropic endpoint, the route the
#   operator fixture's models.yml and the controller's profile name (lib/model-gateway-url.sh).
# - LEGION_E2E_MODEL_GATEWAY_AUDIENCE (required) is the audience the operator's model gateway accepts
#   on a worker's projected ServiceAccount token, substituted into the run's copy of the operator
#   route's pod.yml and asserted on every pod.
# - LEGION_E2E_DISPATCH_URL, LEGION_E2E_ENVOY_URL and LEGION_E2E_NATS_URL (required) are production
#   Dispatch, the production Envoy listener and production NATS, by the operator's fully-qualified
#   names for them: an https:// URL, an http(s):// URL and a nats://host:port, none with a path. The
#   repository carries none of them, and the run never prints them.
# - LEGION_E2E_DISPATCH_TOKEN_SECRET_ID and LEGION_E2E_ENVOY_TOKEN_SECRET_ID (required) are the
#   Secrets Manager ids of the production Dispatch agents' bearer and the production Envoy listener's
#   API token. The repository carries neither.
# - STAGE4B_UNTIL=<checkpoint> stops after that checkpoint. A run with it set is a development run,
#   never the proof, and never prints PASS.
# - STAGE4B_SKIP_CONTROLLER=1, in a development run only, runs none of `controller`'s checks and only
#   takes tree 3 out, so a checkpoint after it runs while the controller's own defect is unfixed.
# - STAGE4B_DESIGN_GATE=root-issues, in a development run that stops at spec-posted or before it,
#   arms the design gate (`gates.design: root-issues`) and runs tree 1 alone: its root architect
#   settles its spec's decision blocks with a human, requests approval with a summary and registers
#   the gate, and spec-posted waits up to 12 hours for a human to approve the spec in Dispatch.
# - STAGE4B_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) holds the transcript, the
#   daemon log, the pod watch, every agent transcript, and the negative controls.
#
# The production bearers (the two Secrets Manager ids above) are read with the devbox admin role
# into 0600 files under the run's scratch directory. They are never printed, never in an argv (curl
# reads them from header files), and never in the evidence. One run at a time: the project, the NATS
# durable consumer names, ports 13372/13373 and the namespace label are shared, so the run takes a
# lock and refuses to start while another holds it, or while LEGSMOKE has pods, Sandboxes or claims
# it did not create.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2e4b.$$.XXXXXXXX")
evidence=${STAGE4B_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e4b-evidence.XXXXXXXX)}
# Refused before anything is written into the evidence directory, the transcript's tee started or
# any trap set, so it prints the run's verdict line itself (lib/model-gateway-unserved.sh --fresh).
if ! reason=$(bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --fresh "$evidence"); then
  echo "CHECK setup: FAIL: $reason"
  echo "stage 4b e2e: FAIL (check setup)"
  rmdir "$work"
  exit 1
fi
mkdir -p "$evidence/logs" "$evidence/transcripts" "$evidence/pods" "$evidence/controls"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
# fd 7 keeps the transcript for cleanup: a signal runs the EXIT trap under the redirections of the
# command it interrupted, whose output may be /dev/null or an evidence file.
exec 7>&1

namespace=legion
operator=${LEGION_E2E_OPERATOR_CONTEXT:-production}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-$HOME/.kube/legion-daemon-production}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
image=${LEGION_E2E_IMAGE:-}
dispatch_token_secret_id=${LEGION_E2E_DISPATCH_TOKEN_SECRET_ID:-}
envoy_token_secret_id=${LEGION_E2E_ENVOY_TOKEN_SECRET_ID:-}
until=${STAGE4B_UNTIL:-}
skip_controller=${STAGE4B_SKIP_CONTROLLER:-}
design_gate=${STAGE4B_DESIGN_GATE:-}
# The Dispatch project key (the workflow's) and its token (the pods' label, the claims' prefix).
project=LEGSMOKE
run_label=legsmoke
label_exact=legsmoke
repo=sjawhar/legion-smoke
dispatch_base=${LEGION_E2E_DISPATCH_URL:-}
# The proof human writes with the agents' bearer, so it names a session of its own: one that holds
# no claim, whose status write on a live root the daemon therefore sets back, as it does any outside
# session's. The run takes a tree out with `legion status` (take_out), the daemon's own write.
dispatch_actor=legion-e2e4b-proof-human-$$
envoy_url=${LEGION_E2E_ENVOY_URL:-}
nats_url=${LEGION_E2E_NATS_URL:-}
# The operator's pod configuration (deploy/kubernetes/operator-route): the model route, overlay,
# ServiceAccount and projected token every pod carries. Legion holds none of it. The run creates its
# own copy of the ConfigMap it mounts, labelled with the run's label, which the teardown
# deletes with the rest.
operator_route=$root/deploy/kubernetes/operator-route
route_configmap=legion-operator-route-$run_label
# The providers Secret the runtime names for the project (ProvidersSecretName), which the run creates
# only when the operator's environment names a NATS nkey seed and lib/namespace-rig.sh deletes.
providers_secret=legion-$run_label-providers
# operator-close's tree, which no workflow issue backs: the run's own, named for the run, and its
# worker's issue, a child in the same project. Both are issue keys (PROJECT-NUMBER), which the
# daemon's spawn requires; the trailing digit keeps the child apart from the root.
optree="S4BOP-$$"
opchild="S4BOP-${$}1"
# The rigs' own pair, beside the production daemon's 13370/13371: the devbox admits both pairs from
# the Legion nodes, so a run never waits for the production daemon to stop.
port_daemon=13372
port_worker_stream=13373
stream=ENVOY_NOTIFICATIONS
# One path for every run on the devbox, whatever its environment names as its state directory.
lock=$HOME/.local/state/legion/e2e/stage4b.lock
record=$work/sandboxes
pg_container=legion-e2e4b-pg-$$
profile=legion-e2e4b-$$-$(date +%s)
state=$work/state
# The HOME the controller's Oh My Pi runs under, so its profile lives in the work directory
# (make_omp_home, lib/omp-home.sh).
omp_home=$work/omp-home
profile_agent=$omp_home/.omp/profiles/$profile/agent
daemon_log=$evidence/logs/daemon.log
check=setup
TZ=UTC printf -v check_started '%(%FT%TZ)T' -1 # when the current check began (lib/model-gateway-unserved.sh)
ok=
was_blocked= # set by blocked: the run stopped on a prerequisite, so it did not run, and did not fail
torn_down=
snapshotted=
compared=
locked=
timeout_hook=
daemon_pid=
watch_pid=
leaks_pid=
events_pid=
sampler_pid=
interests_pid=
shape_pid=
controller_session=
host=
# The production services' hosts, which scrub keeps out of what the run prints (set in prerequisites).
service_hosts=()
pin=
tree1=
tree2=
tree3=
tree4=
pr_number=
smoke_file=
prod_baseline=
audited=
fixture_branch=
pair_recorded=
pair_session=

begin() {
  check=$1
  TZ=UTC printf -v check_started '%(%FT%TZ)T' -1
  echo "== $check"
}
note() { echo "   $*"; }
# pass ends a development run once its STAGE4B_UNTIL checkpoint has passed.
pass() {
  echo "CHECK $check: PASS"
  [ -z "$daemon_pid" ] || interests_sample "$check"
  until_reached
}
# skipped names a checkpoint a development run skipped, so no transcript reads it as passed.
skipped() {
  echo "CHECK $check: SKIPPED ($*)"
  until_reached
}
until_reached() {
  [ "$until" != "$check" ] || {
    ok=1
    echo "stage 4b e2e: development run until $until finished (not the proof)"
    exit 0
  }
}
fail() {
  echo "CHECK $check: FAIL: $*"
  exit 1
}
blocked() {
  echo "CHECK $check: BLOCKED: $*"
  was_blocked=1
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/namespace-rig.sh
. "$root/scripts/e2e/lib/namespace-rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"
# shellcheck source-path=SCRIPTDIR source=lib/stage-role-prompts.sh
. "$root/scripts/e2e/lib/stage-role-prompts.sh"


rk() { timeout --foreground 300 kubectl --kubeconfig "$runtime_kubeconfig" --context "$runtime_context" "$@"; }

# ---- the runtime seam: a claim's process is a pod, its session and workspace on the tree volume ----

# claim_view ISSUE ROLE prints the claim as the daemon's state projects it.
claim_view() {
  daemon_state | jq -ce --arg issue "$1" --arg role "$2" \
    'if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end'
}
claim_sandbox() { claim_view "$1" "$2" | jq -er '.locator.sandbox.name'; }
# claim_moved_or_held ISSUE ROLE UID: the daemon holds ISSUE, or ROLE's claim on it names a pod other
# than UID - its reaction to the end of pod UID.
claim_moved_or_held() {
  daemon_state | jq -e --arg issue "$1" --arg role "$2" --arg uid "$3" '.issues[$issue] |
    .phase == "held" or (((if $role == "architect" then .architect else .workers[$role].claim end).locator.incarnation // "") as $n | $n != "" and $n != $uid)' >/dev/null
}
claim_pod_uid() { claim_view "$1" "$2" | jq -er '.locator.incarnation'; }
# A phase worker stays live from its role's first assignment until its issue closes. record_resident
# ISSUE ROLE keeps the session and pod uid ROLE's claim registered with, once; resident_kept ISSUE
# ROLE is that claim still live (ready, working or idle) on the same session in the same pod.
record_resident() {
  local kept="$work/resident-$1-$2.json"
  [ -s "$kept" ] && return 0
  claim_view "$1" "$2" | jq -ce '{session, incarnation: .locator.incarnation} | select((.session // "") != "" and (.incarnation // "") != "")' >"$kept"
}
resident_kept() {
  claim_view "$1" "$2" | jq -e --slurpfile first "$work/resident-$1-$2.json" \
    '(.state | IN("ready", "working", "idle")) and .session == $first[0].session and .locator.incarnation == $first[0].incarnation' >/dev/null
}
# resident_lost ISSUE ROLE says how ROLE's claim differs from the session and pod it first had.
resident_lost() {
  printf 'now %s, first %s' "$(claim_view "$1" "$2" | jq -c '{state, session, incarnation: .locator.incarnation}')" "$(cat "$work/resident-$1-$2.json")"
}
# claims_cli ARGS... is `legion claims` from the operator shell, over the operator bearer.
claims_cli() { "$work/legion" claims "$@" --config "$work/legion.yaml" --operator-token-file "$work/operator-token"; }
# take_out ISSUE moves the tree ISSUE roots to backlog from the operator shell, over the operator
# bearer, and waits for Dispatch to show it and for the tree's pods to be gone.
take_out() {
  local issue=$1 out
  out=$("$work/legion" status "$issue" backlog --operator-token-file "$work/operator-token" --config "$work/legion.yaml" 2>&1) ||
    fail "legion status $issue backlog from the operator shell: $out"
  until_true 120 "Dispatch to show $issue in backlog" dispatch_status_is "$issue" backlog
  until_true 600 "$issue's pods to be gone" sh -c \
    "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get pods -l 'legion.dev/project=$run_label,legion.dev/tree=$issue' -o name) && [ -z \"\$out\" ]"
}
# status_set_back ISSUE WRITTEN: Dispatch no longer shows the status WRITTEN on ISSUE, and shows the
# status the daemon records for it: the daemon set its own status back over the write.
status_set_back() {
  local status
  status=$(dispatch_get "issues/$1" | jq -er .status) || return 1
  [ "$status" != "$2" ] && daemon_state | jq -e --arg issue "$1" --arg status "$status" '.issues[$issue].status == $status' >/dev/null
}
# set_back_by_daemon FILE: in the issue events FILE, the newest backlog write is the proof human's,
# and the status write after it is the daemon's own.
set_back_by_daemon() {
  jq -e --arg human "$dispatch_actor" --arg daemon "legion-daemon:$project" '
    [.[] | select(.type | IN("issue.updated", "issue.closed"))] | sort_by(.seq)
    | (map(select(.payload.status == "backlog")) | last) as $write
    | $write != null and $write.actor.id == $human
      and ([.[] | select(.seq > $write.seq and .payload.status != "backlog")] | first | .actor.id) == $daemon
  ' "$1" >/dev/null
}
# tree_pod TREE prints a Running pod of the tree, whose worker container mounts the tree volume.
tree_pod() {
  op get pods -l "legion.dev/project=$run_label,legion.dev/tree=$1" --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null | grep .
}
issue_tree() { daemon_state | jq -er --arg issue "$1" '.issues[$issue].tree // $issue'; }
pod_exec() {
  local pod=$1
  shift
  op exec "$pod" -c worker -- "$@"
}

# ---- the review pair: the reviewer's two thermonuclear task dispatches, as its session shows them ----

pair_agents="thermonuclear-deep-review thermonuclear-code-quality"
# pair_dispatch AGENT reads a reviewer session on stdin and prints what the session holds for AGENT:
# every task call naming it, the tool result of each call (text, isError, details), the ids its
# results name for AGENT (details.progress), and every task-result block naming AGENT the session
# received, however it arrived: an async-result delivery, or a hub wait or jobs snapshot that
# recovered it first. A delivery is that block alone, never the rest of a snapshot, which carries
# other jobs' output. Nothing else is summarised, so the evidence keeps each failure's text.
pair_dispatch() {
  jq -R -s -c --arg agent "$1" '[split("\n")[] | fromjson?] as $e
    | [$e[] | select(.type == "message" and .message.role == "assistant") | .message.content[]?
        | select(.type == "toolCall" and .name == "task" and (.arguments | tostring | contains($agent)))] as $calls
    | ($calls | map(.id)) as $ids
    | [$e[] | select(.type == "message" and .message.role == "toolResult" and (.message.toolCallId as $i | $ids | index($i)))
        | .message | {toolCallId, isError, text: ([.content[]? | select(.type == "text") | .text] | join("\n")), details}] as $results
    | {agent: $agent,
       calls: [$calls[] | {id, arguments}],
       results: $results,
       ids: ([$results[].details.progress[]? | select(.agent == $agent) | .id] | unique),
       deliveries: [$e[]
         | (if .type == "custom_message" and .customType == "async-result" then {timestamp, via: "async-result", text: (.content | tostring)}
            elif .type == "message" and .message.role == "toolResult" and .message.toolName == "hub"
              then {timestamp, via: "hub", text: ([.message.content[]? | select(.type == "text") | .text] | join("\n"))}
            else empty end)
         | . as $d
         | ($d.text | [scan("<task-result [^>]*agent=\"" + $agent + "\"[^>]*>[\\s\\S]*?</task-result>")])[]
         | {timestamp: $d.timestamp, via: $d.via, content: .}]}'
}
# pair_text prints the reviewer's session, read from the tree volume with no call to the daemon, so
# the teardown can still read it after a signal to the run's process group has ended the daemon.
pair_text() {
  local pod
  [ -n "$pair_session" ] || return 1
  pod=$(tree_pod "$tree1") || return 1
  pod_exec "$pod" cat -- "$pair_session"
}
# pair_settled: the reviewer dispatched both agents and each dispatch has an outcome: a refused call,
# a task-result block naming the agent, or the subagent's own session ending in a yield.
pair_settled() {
  local text agent ids id pod
  text=$(pair_text) || return 1
  pod=$(tree_pod "$tree1") || return 1
  for agent in $pair_agents; do
    ids=$(pair_dispatch "$agent" <<<"$text" | jq -r '
      if (.calls | length) == 0 then "none"
      elif any(.results[]; .isError == true) or (.deliveries | length) > 0 then "settled"
      else .ids[] end') || return 1
    case "$ids" in
      none | "") return 1 ;;
      settled) continue ;;
    esac
    for id in $ids; do
      pod_exec "$pod" grep -q '"toolName":"yield"' -- "${pair_session%.jsonl}/$id.jsonl" && continue 2
    done
    return 1
  done
}
# record_pair keeps the reviewer's session, the sessions of the subagents it started, and each
# agent's dispatch under $evidence/review-pair, once.
record_pair() {
  local pod text agent
  [ -z "$pair_recorded" ] || return 0
  text=$(pair_text) || return 1
  pod=$(tree_pod "$tree1") || return 1
  mkdir -p "$evidence/review-pair"
  printf '%s\n' "$text" >"$evidence/review-pair/reviewer.jsonl"
  op exec "$pod" -c worker -- tar -C "$(dirname "$pair_session")" -cf - "$(basename "$pair_session" .jsonl)" 2>/dev/null |
    tar -C "$evidence/review-pair" -xf - 2>/dev/null || true
  for agent in $pair_agents; do
    pair_dispatch "$agent" <<<"$text" >"$evidence/review-pair/$agent.json"
  done
  printf '%s\n' "$(basename "$pair_session" .jsonl)" >"$evidence/review-pair/session-stem"
  pair_recorded=1
}
claim_session_file() {
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -er --arg issue "$1" --arg role "$2" \
      '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}
claim_session_text() {
  local file pod
  file=$(claim_session_file "$1" "$2") || return 1
  pod=$(tree_pod "$(issue_tree "$1")") || return 1
  pod_exec "$pod" cat -- "$file"
}
workspace_jj() {
  local issue=$1 pod
  shift
  pod=$(tree_pod "$(issue_tree "$issue")") || return 1
  pod_exec "$pod" jj -R "/legion/workspaces/$repo/${issue,,}" "$@"
}
# assert_claim_endpoints ISSUE ROLE: the claim's pod names production's services,
# and its Oh My Pi has no Anthropic key: a turn off that route, or a pod reaching another rig, would
# not be this proof.
assert_claim_endpoints() {
  local pod mismatch
  pod=$(claim_sandbox "$1" "$2") || fail "$2 on $1 has no Sandbox locator"
  if mismatch=$(pod_endpoint_mismatch "$pod"); then
    fail "ABORT: $2 pod $pod on $1 has $mismatch"
  fi
}
pod_env() { op get pod "$1" -o json | jq -r '.spec.containers[] | select(.name == "worker") | .env[]? | select(.value != null) | "\(.name)=\(.value)"'; }
pod_endpoint_mismatch() {
  local pod=$1 env name want got
  env=$(pod_env "$pod") || {
    printf 'no readable spec\n'
    return 0
  }
  local source
  for name in DISPATCH_URL ENVOY_URL ENVOY_NATS_URL LEGION_DAEMON_URL; do
    case "$name" in
      DISPATCH_URL) want=$dispatch_base source=LEGION_E2E_DISPATCH_URL ;;
      ENVOY_URL) want=$envoy_url source=LEGION_E2E_ENVOY_URL ;;
      ENVOY_NATS_URL) want=$nats_url source=LEGION_E2E_NATS_URL ;;
      LEGION_DAEMON_URL) want="http://$host:$port_daemon" source= ;;
    esac
    got=$(sed -n "s/^$name=//p" <<<"$env")
    if [ "$got" != "$want" ]; then
      # A production service's address is never printed: the mismatch names the run's input.
      if [ -n "$source" ]; then
        printf '%s %s\n' "$name" "$([ -n "$got" ] && echo "differs from $source" || echo "unset, want $source")"
      else
        printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"
      fi
      return 0
    fi
  done
  if grep -q '^ANTHROPIC_API_KEY=' <<<"$env"; then
    printf 'ANTHROPIC_API_KEY in its spec\n'
    return 0
  fi
  return 1
}

# ---- production access ---------------------------------------------------------------------------

read_bearers() {
  (umask 077 &&
    aws secretsmanager get-secret-value --secret-id "$dispatch_token_secret_id" --query SecretString --output text >"$work/dispatch-token" &&
    aws secretsmanager get-secret-value --secret-id "$envoy_token_secret_id" --query SecretString --output text >"$work/envoy-token" &&
    printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
    cp "$work/dispatch-auth-header" "$work/dispatch-human-header" &&
    printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
  [ -s "$work/dispatch-token" ] && [ -s "$work/envoy-token" ] || fail "Secrets Manager returned an empty bearer"
}
# scrub replaces each production service's host with the variable that names it: the run prints
# none of them, and a tool's error (a refused connection, an unresolved name) may carry one.
scrub() {
  local args=() i names=(LEGION_E2E_DISPATCH_URL LEGION_E2E_ENVOY_URL LEGION_E2E_NATS_URL LEGION_E2E_MODEL_GATEWAY_URL) h
  for i in "${!service_hosts[@]}"; do
    h=${service_hosts[$i]%%:*}
    [ -n "$h" ] && args+=(-e "s#${h//./\\.}#<${names[$i]}>#g")
  done
  if [ "${#args[@]}" -eq 0 ]; then cat; else sed "${args[@]}"; fi
}
nats_stream() { bun "$root/scripts/e2e/lib/nats-stream.ts" "$@" 2> >(scrub >&2); }

# ---- the daemon ----------------------------------------------------------------------------------

write_legion_config() {
  cat >"$work/legion.yaml" <<EOF
project: $project
bind: $host
port: $port_daemon
worker_stream_port: $port_worker_stream
daemon_url: http://$host:$port_daemon
postgres_dsn: postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/legion?sslmode=disable
state_dir: $state
operator_token_file: $work/operator-token
envoy_url: $envoy_url
envoy_token_file: $work/envoy-token
nats_urls:
  - $nats_url
dispatch_url: $dispatch_base
dispatch_token_file: $work/dispatch-token
projects:
  $project: { repo: $repo }
gates:
  design: "${design_gate:-off}"
admission_cap: 2
linger_hours: 0.3
controller_wake_interval_seconds: 60
instructions: $work/instructions.md
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
    storage_class: gp2
    kubeconfig: $runtime_kubeconfig
    context: $runtime_context
    pod:
EOF
  # The operator route's pod, its token audience the operator's and its ConfigMap reference pointed
  # at the run's own copy.
  render_operator_pod
  sed -e 's/^/      /' -e "s/name: legion-operator-route\$/name: $route_configmap/" "$work/pod.yml" >>"$work/legion.yaml"
  grep -qF "name: $route_configmap" "$work/legion.yaml" || fail "the operator route's pod.yml mounts no ConfigMap legion-operator-route"
}
# render_operator_pod writes the run's copy of the operator route's pod.yml, the gateway's audience
# in place of its placeholder.
render_operator_pod() {
  # shellcheck disable=SC2016  # the operator route's literal placeholder, not an expansion
  local placeholder='${MODEL_TOKEN_AUDIENCE}' pod
  pod=$(<"$operator_route/pod.yml")
  printf '%s\n' "${pod//"$placeholder"/"$gateway_audience"}" >"$work/pod.yml"
  grep -qF "audience: \"$gateway_audience\"" "$work/pod.yml" || fail "the operator route's pod.yml has no token audience $placeholder to fill with the gateway's"
}
# create_route_configmap is the operator's step before any pod runs: the operator route's models.yml and
# overlay.yml in the ConfigMap the run's pods mount.
create_route_configmap() {
  # shellcheck disable=SC2016  # the operator route's literal placeholder, not an expansion
  local placeholder='${MODEL_BASE_URL}' models
  models=$(<"$operator_route/models.yml")
  printf '%s\n' "${models//"$placeholder"/"$gateway"}" >"$work/models.yml"
  grep -qFx "    baseUrl: $gateway" "$work/models.yml" || fail "the operator route's models.yml has no baseUrl $placeholder to point at the gateway"
  op create configmap "$route_configmap" --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$operator_route/overlay.yml" \
    --dry-run=client -o yaml | kubectl label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
    fail "the operator could not create ConfigMap $route_configmap"
  note "[operator] ConfigMap $route_configmap: models.yml (baseUrl from LEGION_E2E_MODEL_GATEWAY_URL) and overlay.yml from $operator_route, label legion.dev/project=$run_label"
}
# create_providers_secret is the operator's step for the NATS nkey seed: a daemon with a seed (the
# operator's NATS_NKEY_SEED_FILE, else NATS_NKEY_SEED, which the daemon inherits) points every pod
# and the image probe at the providers Secret's NATS_NKEY_SEED key, so the run puts the same seed
# there, from a 0600 file, never an argument. With neither set there is no seed and no Secret.
create_providers_secret() {
  local seed_file=$work/providers-nats-seed
  if [ -n "${NATS_NKEY_SEED_FILE+set}" ]; then
    (umask 077 && tr -d '[:space:]' <"$NATS_NKEY_SEED_FILE" >"$seed_file") || fail "NATS_NKEY_SEED_FILE names $NATS_NKEY_SEED_FILE, which could not be read"
  elif [ -n "${NATS_NKEY_SEED+set}" ]; then
    (umask 077 && printf '%s' "$NATS_NKEY_SEED" | tr -d '[:space:]' >"$seed_file")
  else
    note "[operator] no NATS nkey seed in the environment: no providers Secret"
    return 0
  fi
  [ -s "$seed_file" ] || fail "the operator's NATS nkey seed is empty"
  op create secret generic "$providers_secret" --from-file=NATS_NKEY_SEED="$seed_file" \
    --dry-run=client -o yaml | kubectl label --local -f - "legion.dev/project=$run_label" -o yaml | op create -f - >/dev/null ||
    fail "the operator could not create Secret $providers_secret"
  rm -f "$seed_file"
  note "[operator] Secret $providers_secret: NATS_NKEY_SEED from the operator's seed, label legion.dev/project=$run_label"
}
start_daemon() {
  env -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -u GH_AGENT_APP_PRIVATE_KEY_B64 \
    -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 9>&- 7>&- &
  daemon_pid=$!
  timeout_hook=report_boot
  # A daemon that exits (a refused image probe, a config it will not run) ends the wait at once.
  until_true 900 "the Go daemon to boot and answer /healthz" daemon_answers_or_exited
  timeout_hook=
  kill -0 "$daemon_pid" 2>/dev/null || fail "the Go daemon exited before it answered /healthz: $(tail -3 "$daemon_log" | cut -c1-300 | tr '\n' ' ')"
}
daemon_answers_or_exited() { ! kill -0 "$daemon_pid" 2>/dev/null || curl -fsS "http://$host:$port_daemon/healthz"; }
report_boot() { note "the daemon log's tail: $(tail -5 "$daemon_log" | cut -c1-300)"; }
log_lines() { jq -R -c --arg m "$1" 'fromjson? | select(.msg == $m)' "$daemon_log"; }

# ---- the pod watch (checkpoint pod-watch) ---------------------------------------------------------

# driver_action KIND UID: the driver itself ended pod UID; the watch's checker matches it.
driver_action() { printf '%s %s %s\n' "$1" "$2" "$(date -u +%FT%T.%3NZ)" >>"$evidence/driver-actions.txt"; }
# end_claim_pod ISSUE ROLE kill|delete: ends the pod ISSUE's ROLE claim runs on - `kill` signals its
# worker's PID 1, `delete` deletes the pod object - records the action, and returns only once that
# death is observable: the pod's Sandbox reports Finished for its current generation, the claim moved
# to another incarnation, or the issue is held. Sets `ended_pod_uid` and `ended_pod` for the
# caller's later assertions. A pod can be observably dead before the daemon has moved its claim, so
# a caller that then reads the daemon's reaction waits for that reaction itself.
# Finished is read as the daemon reads a condition (runtime/sandbox/types.go, condition): only when
# the controller wrote it for the Sandbox's current generation, since every relaunch bumps the
# generation and leaves the previous pod's Finished on the object. It is the one arm that says the
# pod died whether or not the daemon has reacted yet, so a kill that lands on a daemon that never
# reacts fails at the caller's wait for the daemon, not here.
# A claim takes its pod's uid the moment the pod is created, before any container of it runs, so a
# kill issued in that window reaches no worker container and ends nothing, and the run would read
# its own silence as a daemon that never reacted. `kill` therefore refuses a claim that has not
# registered from the pod it names; a site that means to end a pod before its worker registers
# passes `delete`, which is what lands on a pod that is still starting.
end_claim_pod() {
  local issue=$1 role=$2 method=$3 claim state bound exec_out
  claim=$(claim_view "$issue" "$role")
  ended_pod_uid=$(jq -r '.locator.incarnation // empty' <<<"$claim")
  ended_pod=$(jq -r '.locator.sandbox.name // empty' <<<"$claim")
  [ -n "$ended_pod_uid" ] && [ -n "$ended_pod" ] || fail "the $role claim of $issue names no pod to end: $claim"
  state=$(jq -r '.state // "none"' <<<"$claim")
  case $method in
    kill)
      case $state in
        ready | working | idle) ;;
        *) fail "the $role claim of $issue is $state on pod $ended_pod_uid, not registered from it: a kill would reach no worker container (delete it instead)" ;;
      esac
      driver_action "$method" "$ended_pod_uid"
      # Killing PID 1 ends the container the exec runs in, so kubectl's own exit says nothing either
      # way; it is recorded here, and the wait below is what says the kill landed.
      exec_out=$(op exec "$ended_pod" -c worker -- sh -c 'kill 1' 2>&1) ||
        note "the kill exec of $ended_pod answered: $(printf '%s' "$exec_out" | tr '\n' ' ' | cut -c1-200)"
      bound=300
      ;;
    delete)
      driver_action "$method" "$ended_pod_uid"
      op delete pod "$ended_pod" --wait=false >/dev/null
      # A deleted pod waits out its termination grace before the runtime launches its replacement.
      bound=600
      ;;
    *) fail "end_claim_pod: unknown method $method" ;;
  esac
  until_true "$bound" "the $method of pod $ended_pod_uid to land" pod_end_observed "$issue" "$role" "$ended_pod_uid" "$ended_pod"
}
# pod_end_observed ISSUE ROLE UID SANDBOX: the end of pod UID is observable (end_claim_pod).
pod_end_observed() {
  timeout 120 kubectl --context "$operator" -n "$namespace" get sandbox "$4" -o json |
    jq -e '.metadata.generation as $g | .status.conditions[]? | select(.type == "Finished" and .status == "True" and .observedGeneration == $g)' >/dev/null ||
    claim_moved_or_held "$1" "$2" "$3"
}
# relaunch_ends CLAIM UID... prints one line for each UID, a relaunch of CLAIM the driver ended:
# when the driver's end (its kill or its delete), the relaunch's registration (the first
# `api: claim registered` of CLAIM between its `supervise: launched` and its `supervise: process
# died`) and its death fell, in seconds after its launch, and whether the daemon charged the death
# as one with work outstanding
# (a `supervise: the agent died with work outstanding` of CLAIM after that death and before the
# next launch). A deleted pod that is still starting can run on, register and take its task before
# its containers are stopped, and a death is charged only once the agent is ready, which follows its
# registration (supervise/budgets.go). Its last line is `expect: REASON`, the hold the ends lead to:
# died (supervise/machine.go) charges a death first and fails on it at the limit, and a charged
# death follows a ready that reset the launch count, so the last death decides: charged is `deaths
# with work outstanding ran out`, uncharged `launch failures ran out`. It is `refused: WHY` instead
# for a death charged that never registered, which could have had no work, or a relaunch the log
# lacks.
relaunch_ends() {
  local claim=$1
  shift
  jq -n -r --arg c "$claim" --rawfile log "$daemon_log" --rawfile actions "$evidence/driver-actions.txt" '
    def secs: (.[0:19] + "Z" | fromdateiso8601) + (.[19:] | rtrimstr("Z") | if . == "" then 0 else "0" + . | tonumber end);
    def since($t): . - $t | . * 100 | round / 100 | tostring;
    ($log | split("\n") | map(fromjson? // empty | select(.claim == $c) | . + {t: (.time | secs)})) as $lines
    | ($lines | map(select(.msg == "supervise: launched") | .t)) as $launches
    | ($actions | split("\n") | map(split(" ") | select(length == 3 and (.[0] == "kill" or .[0] == "delete")) | {key: .[1], value: .[2]}) | from_entries) as $ends_issued
    | [$ARGS.positional[] as $u
        | ($lines | map(select(.msg == "supervise: launched" and .incarnation == $u)) | first) as $launch
        | ($lines | map(select(.msg == "supervise: process died" and .incarnation == $u)) | first) as $death
        | if $launch == null or $death == null then {u: $u, missing: (if $launch == null then "launch" else "death" end)}
          else $launch.t as $l | $death.t as $d
            | ([$launches[] | select(. > $d)] | min // infinite) as $next
            | {u: $u, l: $l, d: $d,
               r: ($lines | map(select(.msg == "api: claim registered" and .t > $l and .t < $d) | .t) | first),
               charged: ($lines | any(.msg == "supervise: the agent died with work outstanding" and .t >= $d and .t < $next)),
               ended: $ends_issued[$u]}
          end] as $ends
    | ($ends[] as $e | if $e.missing then "relaunch \($e.u[0:8]): the daemon log has no \($e.missing) of it"
        else "relaunch \($e.u[0:8]): end issued +\(if $e.ended == null then "(none recorded)" else ($e.ended | secs | since($e.l)) + " s" end), \(if $e.r == null then "never registered" else "registered +" + ($e.r | since($e.l)) + " s" end), died +\($e.d | since($e.l)) s, \(if $e.charged then "charged as a death with work outstanding" else "not charged (a launch failure)" end)" end),
      ( ($ends | map(select(.missing)) | first) as $gap
        | ($ends | map(select(.missing | not) | select(.charged and .r == null)) | first) as $bad
        | if $gap then "refused: the daemon log has no \($gap.missing) of relaunch \($gap.u)"
          elif $bad then "refused: relaunch \($bad.u) was charged as a death with work outstanding, but never registered"
          elif ($ends | length) == 0 then "refused: no relaunch was ended"
          elif ($ends | last | .charged) then "expect: deaths with work outstanding ran out"
          else "expect: launch failures ran out" end )
  ' --args "$@"
}
# pod_watch_verdict WATCH ACTIONS DAEMONLOG prints every pod of the run the node ended (Evicted, or
# a container OOMKilled), and every claim process the daemon found dead (`supervise: process died`,
# naming the pod uid as the incarnation) that no driver action ended, and exits 1 when there is any.
# A pod the daemon suspended, released or closed ends without either, so it needs no match. The
# resume that finds the tree volume lost dies by design (the runtime's detail begins "the tree volume
# was lost: "), and
# re-admission counts those itself. A pod the scheduler never placed is no unexplained death either:
# the daemon retires a pod still unscheduled at its boot deadline and relaunches the claim, so a
# death whose pod the watch saw `PodScheduled=False Unschedulable` and never `PodScheduled=True` (nor
# on a node) is accounted for (never_scheduled_deaths); a pod that was scheduled and then died is
# judged as before. The memory hog, labelled legion.dev/e2e-control=memory-hog, is excluded, and
# must have been seen OOMKilled.
pod_watch_verdict() {
  local watch=$1 actions=$2 log=$3
  jq -s -r --rawfile actions "$actions" --rawfile log "$log" --arg unscheduled "$(never_scheduled_deaths "$watch" "$log")" '
    ($actions | split("\n") | map(select(. != "") | split(" ")[1])) as $driver
    | ($unscheduled | split("\n") | map(select(. != ""))) as $retired
    | ($log | split("\n") | map(fromjson? // empty) | map(select(.msg == "supervise: process died"))) as $died
    | [ .[] | select(.object.kind == "Pod") | .object ] as $pods
    | ($pods | map(select(.metadata.labels["legion.dev/e2e-control"] == "memory-hog"))
       | any(.status.containerStatuses[]?.state.terminated.reason == "OOMKilled"
             or .status.containerStatuses[]?.lastState.terminated.reason == "OOMKilled")) as $hog
    | [ $pods[] | select(.metadata.labels["legion.dev/e2e-control"] == null)
        | . as $p
        | ($p.status.reason // "") as $podReason
        | [ ($p.status.initContainerStatuses[]?, $p.status.containerStatuses[]?) | (.state.terminated, .lastState.terminated) | select(. != null) ] as $terms
        | select($podReason == "Evicted" or any($terms[]; .reason == "OOMKilled"))
        | "\($p.metadata.name) uid \($p.metadata.uid): \($podReason) \([$terms[] | "\(.reason) exit \(.exitCode)"] | join(", "))" ]
      + [ $died[] | select((.detail // "") | startswith("the tree volume was lost: ") | not)
          | .incarnation as $i | select(($driver | index($i)) == null and ($retired | index($i)) == null)
          | "incarnation \($i) died with no driver action: observed \(.observed), \((.detail // "") | .[0:200])" ]
      | unique as $bad
    | if $hog then $bad[] else ("the memory hog was never seen OOMKilled", $bad[]) end
  ' "$watch" | tee "$work/pod-watch-verdict.txt"
  [ ! -s "$work/pod-watch-verdict.txt" ]
}
# never_scheduled_deaths WATCH DAEMONLOG prints, one a line, each incarnation the daemon found dead
# (`supervise: process died`) whose pod the watch saw `PodScheduled=False` with reason
# `Unschedulable` and never `PodScheduled=True` or bound to a node: a pod the daemon retired at its
# boot deadline because the scheduler never placed it. It reads the pod's own conditions, never the
# daemon's detail text.
never_scheduled_deaths() {
  local watch=$1 log=$2
  jq -s -r --rawfile log "$log" '
    ($log | split("\n") | map(fromjson? // empty) | map(select(.msg == "supervise: process died") | .incarnation)) as $died
    | [ .[] | select(.object.kind == "Pod") | .object ] as $pods
    | $died[] | . as $i
    | [ $pods[] | select(.metadata.uid == $i) ] as $seen
    | select(($seen | length) > 0
        and any($seen[]; any(.status.conditions[]?; .type == "PodScheduled" and .status == "False" and .reason == "Unschedulable"))
        and (any($seen[]; (.spec.nodeName // "") != "" or any(.status.conditions[]?; .type == "PodScheduled" and .status == "True")) | not))
    | $i
  ' "$watch" | sort -u
}
# watch_raw FILE KIND PATH writes every watch event of the API collection PATH (with its query) to
# FILE, one JSON object a line, for the rest of the run. kubectl's own watch ends, silently, when the
# API server closes it at its watch timeout (a full run's pod watch once stopped 56 minutes in), so
# this lists the collection, watches from the list's resourceVersion, and resumes from the last
# version it saw each time a watch ends; a version the server no longer holds (410 Gone) is listed
# again. Each list, end and resume is noted in the transcript. A list's items are recorded as ADDED
# events of KIND, as the watch reports them. Each watch asks the server to end it within 300 s, so a
# loop a killed driver left behind stops at its next pass rather than an hour later; a watch that
# delivered nothing is resumed after a pause, and a line that does not parse (a watch cut mid-line)
# ends that watch without being recorded, so FILE holds whole events only.
watch_raw() {
  local file=$1 kind=$2 path=$3 version='' list line type next events
  trap - EXIT ERR
  set +e
  while kill -0 "$$" 2>/dev/null; do
    if [ -z "$version" ]; then
      if ! list=$(kubectl --context "$operator" get --raw "$path" 2>>"$evidence/logs/$(basename "$file" .json).err"); then
        sleep 2
        continue
      fi
      version=$(jq -r '.metadata.resourceVersion' <<<"$list")
      jq -c --arg kind "$kind" '.items[] | {type: "ADDED", object: (. + {kind: $kind})}' <<<"$list" >>"$file"
      echo "   [watch] $(basename "$file"): listed at resourceVersion $version"
    fi
    events=0
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      # An ERROR carries the status code in place of a version; a line jq cannot read ends the watch.
      IFS=$'\t' read -r type next < <(jq -r 'if .type == "ERROR" then "ERROR\t\(.object.code // "")" else "\(.type)\t\(.object.metadata.resourceVersion // "")" end' <<<"$line" 2>/dev/null || echo TORN)
      events=$((events + 1))
      case "$type" in
        TORN)
          echo "   [watch] $(basename "$file"): a watch line did not parse; resuming at resourceVersion $version"
          break
          ;;
        ERROR)
          echo "   [watch] $(basename "$file"): the server ended the watch at resourceVersion $version with code $next$([ "$next" = 410 ] && echo '; listing again')"
          [ "$next" != 410 ] || version=
          break
          ;;
        BOOKMARK) version=$next ;;
        *)
          printf '%s\n' "$line" >>"$file"
          [ -z "$next" ] || version=$next
          ;;
      esac
    done < <(setpriv --pdeathsig KILL kubectl --context "$operator" get --raw "$path&watch=1&allowWatchBookmarks=true&timeoutSeconds=300&resourceVersion=$version" 2>>"$evidence/logs/$(basename "$file" .json).err")
    [ -z "$version" ] || echo "   [watch] $(basename "$file"): the watch ended; resuming at resourceVersion $version"
    [ "$events" -gt 0 ] || sleep 2
  done
}
start_pod_watch() {
  # fd 9, the run lock, stays out of every background child, so none can outlive the run holding it.
  # Each watch and loop also ends with the driver itself, however it ends: a killed driver runs no
  # cleanup, so a kubectl dies with it (setpriv --pdeathsig) and a loop stops at its next pass.
  : >"$evidence/pod-watch.json"
  watch_raw "$evidence/pod-watch.json" Pod "/api/v1/namespaces/$namespace/pods?labelSelector=legion.dev%2Fproject%3D$run_label" 9>&- 7>&- &
  watch_pid=$!
  : >"$evidence/node-events.json"
  watch_raw "$evidence/node-events.json" Event "/api/v1/events?fieldSelector=involvedObject.kind%3DNode" 9>&- 7>&- &
  events_pid=$!
  # Every value the run's Secrets hold, kept in memory by lib/secret-leaks.ts and judged against the
  # recorded pods at pod-shape; no value is printed or written.
  setpriv --pdeathsig KILL bun "$root/scripts/e2e/lib/secret-leaks.ts" "$operator" "$namespace" "legion.dev/project=$run_label" \
    "$evidence/pod-watch.json" "$work/secret-leaks.json" 2>>"$evidence/logs/secret-leaks.err" 9>&- 7>&- &
  leaks_pid=$!
  ( # Node memory for the nodes the run's pods are on, every 30 s (metrics-server).
    trap - EXIT ERR
    set +e
    while kill -0 "$$" 2>/dev/null; do
      for node in $(op get pods -l "legion.dev/project=$run_label" -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null | tr ' ' '\n' | sort -u); do
        printf '%s %s %s\n' "$(date -u +%FT%TZ)" "$node" "$(kubectl --context "$operator" top node "$node" --no-headers 2>&1 | tr -s ' ')"
        kubectl --context "$operator" get node "$node" -o json 2>/dev/null |
          jq -c --arg at "$(date -u +%FT%TZ)" '{at: $at, node: .metadata.name, pressure: [.status.conditions[] | select(.type | test("Pressure")) | select(.status == "True") | .type]}'
      done
      sleep 30
    done
  ) >>"$evidence/node-memory.txt" 2>&1 9>&- 7>&- &
  sampler_pid=$!
}

# start_memory_hog NODE is the pod watch's negative control: a pod the operator pins to a tree's
# node, labelled as the run's control, with its own 64Mi limit, which it allocates past. Only its
# own cgroup OOM-kills it, so no tree pod sees node pressure; pod_watch_verdict excludes it by its
# label and requires that it was seen OOMKilled.
start_memory_hog() {
  local node=$1 hog=$work/memory-hog.yaml
  cat >"$hog" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: legion-e2e4b-memory-hog-$$
  labels: { legion.dev/project: $run_label, legion.dev/e2e-control: memory-hog }
spec:
  restartPolicy: Never
  nodeName: $node
  automountServiceAccountToken: false
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  securityContext: { runAsNonRoot: true, runAsUser: 1000, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: hog
      image: $image
      command: [bun, -e, "const held = []; for (;;) held.push(new Uint8Array(8 << 20).fill(1))"]
      resources: { requests: { memory: 32Mi, cpu: 10m }, limits: { memory: 64Mi } }
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
EOF
  op apply -f "$hog" >/dev/null || blocked "the operator could not create the memory hog ($hog)"
}
hog_oomkilled() {
  op get pod "legion-e2e4b-memory-hog-$$" -o json |
    jq -e '[.status.containerStatuses[]?.state.terminated.reason, .status.containerStatuses[]?.lastState.terminated.reason] | index("OOMKilled")' >/dev/null
}

# ---- the pod-shape watcher (checkpoint pod-shape) --------------------------------------------------

# check_pod_shape SPEC prints each way the pod object SPEC departs from the shape every Sandbox pod
# has, or nothing, with its Secret's values as they are now: gVisor, the operator's ServiceAccount and its one projected token, the run's own copy of
# the operator's route ConfigMap mounted where the profile reads models.yml, the pool, the restricted
# security context, no token value in a container's environment, command or args, and 4b.6b's split
# provisioning (the provisioning token only in workspace-fetch, the feed read-only in workspace-init,
# no provision directory in the worker).
# shape_problems prints each way the pod object on stdin departs from that shape, or nothing. It
# judges the object alone, so a pod the watch recorded is judged after it is gone.
shape_problems() {
  jq -r --arg route "$route_configmap" --arg audience "$gateway_audience" '
    .spec as $s
    | (if $s.runtimeClassName != "gvisor" then "runtimeClassName \($s.runtimeClassName)" else empty end),
      (if $s.serviceAccountName != "legion-worker" then "serviceAccountName \($s.serviceAccountName)" else empty end),
      (if $s.automountServiceAccountToken != false then "automountServiceAccountToken \($s.automountServiceAccountToken)" else empty end),
      (if ([$s.volumes[] | select(.projected) | .projected.sources[] | select(.serviceAccountToken)] | length) != 1
        or ([$s.volumes[] | select(.projected) | .projected.sources[] | select(.serviceAccountToken) | .serviceAccountToken.audience] != [$audience])
        then "the projected token source of the operator pod is not the one token for LEGION_E2E_MODEL_GATEWAY_AUDIENCE" else empty end),
      ([$s.volumes[] | select(.configMap.name == $route) | .name] as $route_volumes
        | if ($route_volumes | length) != 1 then "no one volume of the route ConfigMap \($route)"
          elif ([$s.containers[] | select(.name == "worker") | .volumeMounts[]? | select(.name == $route_volumes[0] and .mountPath == "/home/legion/.omp/profiles/legion/agent/models.yml")] | length) != 1
          then "the worker does not mount models.yml of \($route) where the profile reads it" else empty end),
      (if $s.nodeSelector["legion.dev/pool"] != "legion" then "nodeSelector \($s.nodeSelector)" else empty end),
      (if ([$s.tolerations[]? | select(.key == "legion.dev/pool")] | length) == 0 then "no legion.dev/pool toleration" else empty end),
      # Pod Security "restricted": non-root and RuntimeDefault seccomp may be set on the pod; the
      # escalation and capability rules are set on each container.
      ([$s.initContainers[]?, $s.containers[]] | .[] as $c
        | (if $c.securityContext.allowPrivilegeEscalation != false
             or ($c.securityContext.runAsNonRoot // $s.securityContext.runAsNonRoot) != true
             or ($c.securityContext.seccompProfile.type // $s.securityContext.seccompProfile.type) != "RuntimeDefault"
             or ($c.securityContext.capabilities.drop // []) != ["ALL"]
            then "container \($c.name) is not restricted: \({container: $c.securityContext, pod: $s.securityContext} | tostring)" else empty end)),
      ([$s.initContainers[]? | select(.name != "workspace-fetch") | .volumeMounts[]? | select(.mountPath == "/var/run/legion/provision")] | if length > 0 then "the provision volume is mounted outside workspace-fetch" else empty end),
      ([$s.containers[] | select(.name == "worker") | .volumeMounts[]? | select(.mountPath == "/var/run/legion/provision")] | if length > 0 then "the worker mounts the provision volume" else empty end),
      ([$s.initContainers[]? | select(.name == "workspace-init") | .volumeMounts[]? | select(.name == "feed" and .readOnly != true)] | if length > 0 then "workspace-init mounts the feed writable" else empty end)
  '
}
check_pod_shape() {
  local spec=$1 tokens
  shape_problems <<<"$spec"
  tokens=$(op get secret "$(jq -r '.metadata.name' <<<"$spec")-boot" -o json 2>/dev/null | jq -r '.data // {} | .[] | @base64d') || tokens=
  if [ -n "$tokens" ]; then
    jq -r '[.spec.initContainers[]?, .spec.containers[]] | .[] | [.command[]?, .args[]?, (.env[]? | .value // empty)] | .[]' <<<"$spec" >"$work/shape-words"
    while IFS= read -r token; do
      [ -n "$token" ] || continue
      if grep -qF -- "$token" "$work/shape-words"; then echo "a Secret value is in a container's command, args or environment"; fi
    done <<<"$tokens"
  fi
  rm -f "$work/shape-words"
}
# pod_facts POD records, once a Sandbox pod runs, what the proof reports per pod: uname -r from
# inside, the init containers' timeline (workspace-fetch's duration and the wait before
# workspace-init), the pod argv, and the node's ephemeral-storage use.
pod_facts() {
  local pod=$1 node
  node=$(op get pod "$pod" -o jsonpath='{.spec.nodeName}')
  {
    printf 'uname=%s\n' "$(pod_exec "$pod" uname -r 2>&1)"
    op get pod "$pod" -o json | jq -c '{
      role: .metadata.labels["legion.dev/role"], tree: .metadata.labels["legion.dev/tree"], node: .spec.nodeName,
      init: [.status.initContainerStatuses[]? | {name, exit: .state.terminated.exitCode, started: .state.terminated.startedAt, finished: .state.terminated.finishedAt}],
      argv: [.spec.containers[] | select(.name == "worker") | .command[]?]}'
    kubectl --context "$operator" get --raw "/api/v1/nodes/$node/proxy/stats/summary" 2>/dev/null |
      jq -c '{node: .node.nodeName, ephemeralUsedBytes: .node.fs.usedBytes, ephemeralCapacityBytes: .node.fs.capacityBytes}'
  } >"$evidence/pods/$pod.$(op get pod "$pod" -o jsonpath='{.metadata.uid}').txt" 2>&1
}
# pod_shape_watcher checks each Sandbox pod of the run it finds Running, and records its facts: uname
# -r from inside and the init timeline, which only a live pod gives. A departure is recorded as the
# run's violation, and the next bounded wait aborts naming it. It is the early warning; pod-shape's
# verdict judges every pod from the pod watch's record, whether or not this reached it.
pod_shape_watcher() {
  local pod uid problems spec
  trap - EXIT ERR
  set +e
  while kill -0 "$$" 2>/dev/null; do
    while IFS=$'\t' read -r pod uid; do
      [ -n "$pod" ] || continue
      grep -qF " $uid " "$evidence/pods-checked.txt" 2>/dev/null && continue
      # A pod gone before it is read is the verdict's to judge, from the spec the pod watch recorded.
      spec=$(op get pod "$pod" -o json 2>/dev/null) || continue
      printf '%s\n' "$spec" >"$evidence/pods/$uid.json"
      problems=$(check_pod_shape "$spec")
      if [ -n "$problems" ]; then
        printf 'pod %s (uid %s): %s\n' "$pod" "$uid" "$(tr '\n' ';' <<<"$problems")" >"$evidence/pane-endpoint-violation.txt"
        return 0
      fi
      pod_facts "$pod"
      printf '%s %s %s\n' "$pod" " $uid " "$(date -u +%FT%T.%3NZ)" >>"$evidence/pods-checked.txt"
    done < <(op get pods -l "legion.dev/project=$run_label,!legion.dev/probe,!legion.dev/e2e-control" \
      --field-selector=status.phase=Running -o json 2>/dev/null |
      jq -r '.items[] | select(any(.status.containerStatuses[]?; .name == "worker" and .ready)) | [.metadata.name, .metadata.uid] | @tsv')
    sleep 3
  done
}
# pod_shape_verdict WATCH prints, for each Sandbox pod whose worker the watch ever saw ready (the
# image probe and the run's controls aside), each way the spec the watch last recorded for it while
# ready departs from the pod shape. It reads the record only, so a pod gone before any poll reached
# it is judged too.
pod_shape_verdict() {
  local watch=$1 uid spec problems
  while IFS=$'\t' read -r uid spec; do
    problems=$(shape_problems <<<"$spec")
    [ -z "$problems" ] || printf '%s: %s\n' "$uid" "$(tr '\n' ';' <<<"$problems")"
  done < <(jq -c 'select(.object.kind == "Pod") | .object
      | select(.metadata.labels["legion.dev/probe"] == null and .metadata.labels["legion.dev/e2e-control"] == null)
      | select(any(.status.containerStatuses[]?; .name == "worker" and .ready))' "$watch" |
    jq -s -r 'group_by(.metadata.uid)[] | last | "\(.metadata.uid)\t\(tojson)"')
}
# stream_missing WATCH prints each Sandbox pod uid the run knows from another source that the watch
# never recorded: a pod the shape watcher read, a pod the driver ended, and every incarnation the
# daemon launched. A watch that went silent partway through the run fails here, naming what it
# missed, rather than leaving later pods unjudged.
stream_missing() {
  local watch=$1
  comm -23 \
    <({ awk '{print $2}' "$evidence/pods-checked.txt" 2>/dev/null
        awk '{print $2}' "$evidence/driver-actions.txt" 2>/dev/null
        jq -R -r 'fromjson? | select(.msg == "supervise: launched") | .incarnation // empty' "$daemon_log"
      } | { grep -E '^[0-9a-f]{8}-' || true; } | sort -u) \
    <(jq -r 'select(.object.kind == "Pod") | .object.metadata.uid' "$watch" | sort -u)
}

# ---- the smoke repository's fixture (tree 2) --------------------------------------------------------

# push_fixture ISSUE: legion/ISSUE on the smoke repository, one commit on main carrying a
# repository's own configuration: AGENTS.md, an Oh My Pi extension and tool, two MCP servers under
# distinct names, an LSP config, a Codex tool, and a Claude plugin list. Each one, if the agent's
# process loads it, writes its own marker under /tmp/legion-fixture/ in the pod. The workspace of
# ISSUE is provisioned from that bookmark (internal/workspace createWorkspace), so tree 2's pods
# carry the fixture; tree 2 never opens a pull request, so nothing of it reaches the smoke main.
push_fixture() {
  local issue=$1 dir
  dir=$work/fixture
  fixture_branch="legion/$issue"
  gh repo clone "$repo" "$dir" -- --quiet --depth 1 --branch main >/dev/null 2>&1 || fail "clone $repo for the fixture"
  mkdir -p "$dir/.omp/extensions" "$dir/.omp/tools" "$dir/.omp/plugins" "$dir/.codex/tools"
  cat >"$dir/AGENTS.md" <<'EOF'
# Fixture instructions (Stage 4b proof)
Before anything else in this repository, run `mkdir -p /tmp/legion-fixture && touch /tmp/legion-fixture/agents-md`.
EOF
  cat >"$dir/.omp/extensions/fixture.ts" <<'EOF'
import { mkdirSync, writeFileSync } from "node:fs";
export default function fixtureExtension() {
  mkdirSync("/tmp/legion-fixture", { recursive: true });
  writeFileSync("/tmp/legion-fixture/omp-extension", "loaded\n");
}
EOF
  cat >"$dir/.omp/tools/fixture.ts" <<'EOF'
import { mkdirSync, writeFileSync } from "node:fs";
mkdirSync("/tmp/legion-fixture", { recursive: true });
writeFileSync("/tmp/legion-fixture/omp-tool", "imported\n");
export default { name: "fixture_marker", description: "fixture", parameters: { type: "object", properties: {} }, execute: async () => "ok" };
EOF
  cat >"$dir/.mcp.json" <<'EOF'
{ "mcpServers": { "fixture-root-mcp": { "command": "sh", "args": ["-c", "mkdir -p /tmp/legion-fixture && touch /tmp/legion-fixture/mcp-root && sleep 600"] } } }
EOF
  cat >"$dir/.omp/mcp.json" <<'EOF'
{ "mcpServers": { "fixture-omp-mcp": { "command": "sh", "args": ["-c", "mkdir -p /tmp/legion-fixture && touch /tmp/legion-fixture/mcp-omp && sleep 600"] } } }
EOF
  cat >"$dir/lsp.json" <<'EOF'
{ "servers": { "fixture-lsp": { "command": ["sh", "-c", "mkdir -p /tmp/legion-fixture && touch /tmp/legion-fixture/lsp && sleep 600"], "fileTypes": ["md"] } } }
EOF
  cat >"$dir/.codex/tools/fixture.ts" <<'EOF'
import { mkdirSync, writeFileSync } from "node:fs";
mkdirSync("/tmp/legion-fixture", { recursive: true });
writeFileSync("/tmp/legion-fixture/codex-tool", "imported\n");
export default { name: "fixture_codex_marker", description: "fixture", parameters: { type: "object", properties: {} }, execute: async () => "ok" };
EOF
  cat >"$dir/.omp/plugins/installed_plugins.json" <<'EOF'
{ "version": 2, "plugins": { "fixture-claude-plugin@fixture": [ { "scope": "project", "installPath": ".omp/plugins/fixture-claude-plugin" } ] } }
EOF
  mkdir -p "$dir/.omp/plugins/fixture-claude-plugin/.claude-plugin" "$dir/.omp/plugins/fixture-claude-plugin/commands"
  printf '{ "name": "fixture-claude-plugin", "version": "0.0.1" }\n' >"$dir/.omp/plugins/fixture-claude-plugin/.claude-plugin/plugin.json"
  cat >"$dir/.omp/plugins/fixture-claude-plugin/commands/fixture.md" <<'EOF'
---
description: fixture
---
Run `touch /tmp/legion-fixture/claude-plugin`.
EOF
  git -C "$dir" checkout -q -b "$fixture_branch"
  git -C "$dir" add -A
  git -C "$dir" -c user.name="stage4b proof" -c user.email="stage4b@legion.invalid" commit -qm "Stage 4b fixture: a repository's own configuration, each with a marker ($issue)"
  git -C "$dir" push -q origin "$fixture_branch" || fail "push the fixture branch $fixture_branch to $repo"
  note "pushed the repository-configuration fixture as $repo $fixture_branch ($(git -C "$dir" rev-parse --short HEAD))"
}
# assistant_said ISSUE ROLE TEXT: one of the claim's assistant turns carries TEXT, in its reply text
# or in a tool call's arguments: an agent answers a Dispatch message with dispatch_message, so the
# answer is a call's body. The instruction that asks for TEXT is a delivered message, not an
# assistant turn, so a plain search of the session would match it.
assistant_said() {
  local text
  text=$(claim_session_text "$1" "$2") || return 1
  jq -R -s -e --arg want "$3" '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant")
    | .message.content[]? | (if .type == "text" then .text elif .type == "toolCall" then (.arguments | tostring) else "" end)
    | select(contains($want))] | length > 0' <<<"$text" >/dev/null
}
fixture_markers() {
  local pod=$1
  pod_exec "$pod" sh -c 'ls /tmp/legion-fixture 2>/dev/null | sort | tr "\n" " "' 2>&1
}
# completion_verdict reads a phase worker's session (JSONL on stdin) and prints, for every
# assignment (the daemon's task, a user message) its worker answered with the legion tool's
# handoff_complete, the calls that got no result, how many results succeeded, and whether the phase
# stall recorded `closed` after the call that succeeded (the extension records it inside the call,
# before Oh My Pi writes the result); and how many phase-stall follow-ups came after the session's
# last successful completion. It is the 4b.13b acceptance's stall check
# (stage3-4b13b-acceptance.sh, pane-rule-phase-worker-and-stall) for a pod's session: a worker
# stopped while its handoff_complete call runs leaves that call with no result and no `closed`
# (LEGION-283), and a worker resumed from such a session may report the phase again.
completion_verdict() {
  jq -R -s -c --arg followup "Your turn ended with your Legion phase still open" '
    [split("\n")[] | fromjson?] | to_entries
    | [.[] | .key as $i | .value as $e
        | if $e.type == "message" and $e.message.role == "user" then {i: $i, kind: "assignment"}
          elif $e.type == "message" and $e.message.role == "assistant" then
            [$e.message.content[]? | select(.type == "toolCall" and .name == "legion" and .arguments.op == "handoff_complete") | .id] as $ids
            | if ($ids | length) > 0 then {i: $i, kind: "call", ids: $ids} else empty end
          elif $e.type == "message" and $e.message.role == "toolResult" and $e.message.toolName == "legion" then
            {i: $i, kind: "result", id: $e.message.toolCallId, ok: ($e.message.isError != true)}
          elif $e.type == "custom" and $e.customType == "legion-phase-stall" then {i: $i, kind: "stall", state: $e.data.state}
          elif (($e | tostring) | contains($followup)) then {i: $i, kind: "followup"}
          else empty end] as $t
    | [$t[] | select(.kind == "assignment") | .i] as $starts
    | [range(0; $starts | length) as $k | $starts[$k] as $from | ($starts[$k + 1] // ($t | map(.i) | max + 1)) as $to
        | [$t[] | select(.i >= $from and .i < $to)] as $seg
        | [$seg[] | select(.kind == "call") | .ids[]] as $calls
        | select(($calls | length) > 0)
        | [$seg[] | select(.kind == "result") | select(.id as $id | $calls | index($id) != null)] as $results
        | [$results[] | select(.ok)] as $ok
        | ([$seg[] | select(.kind == "call" and ($ok[0].id as $id | .ids | index($id) != null)) | .i] | first) as $okcall
        | {assignment: $from, calls: ($calls | length),
           unanswered: [$calls[] | select(. as $id | [$results[].id] | index($id) == null)],
           succeeded: ($ok | length),
           closed: (($ok | length) == 1 and $okcall != null and any($seg[]; .kind == "stall" and .state == "closed" and .i > $okcall))}] as $segments
    | ([$t[] | select(.kind == "result" and .ok) | .i] | last) as $last
    | {segments: $segments,
       followups_after: (if $last == null then null else [$t[] | select(.kind == "followup" and .i > $last)] | length end)}'
}
# completions_answered FILE: every assignment its worker answered with handoff_complete got a
# result for each call, exactly one success (the report, made once), and `closed` after the call
# that succeeded, and no phase-stall follow-up came after the last success.
completions_answered() {
  completion_verdict <"$1" | jq -e '(.segments | length) > 0
    and all(.segments[]; (.unanswered | length) == 0 and .succeeded == 1 and .closed)
    and .followups_after == 0' >/dev/null
}
# cut_at_completion_call FILE prints FILE's session up to and including its last assistant entry that
# calls handoff_complete: the transcript a suspension that stops the worker inside that call leaves.
cut_at_completion_call() {
  jq -R -s -r '[split("\n")[] | select(length > 0)] as $lines
    | ([$lines | to_entries[] | select(.value | fromjson? | .type == "message" and .message.role == "assistant"
        and any(.message.content[]?; .type == "toolCall" and .name == "legion" and .arguments.op == "handoff_complete")) | .key] | last) as $k
    | $lines[0:$k + 1][]' "$1"
}

# ---- teardown ------------------------------------------------------------------------------------

# The daemon's two durable consumers are named by the Dispatch project key (intake's
# dispatchConsumerName and githubConsumerName): legion-go-LEGSMOKE-dispatch and -github.
consumers_prefix="legion-go-$project-"
delete_consumers() {
  local name
  for name in "${consumers_prefix}dispatch" "${consumers_prefix}github"; do
    case "$(nats_stream delete "$nats_url" "$stream" "$name" 2>&1)" in
      deleted) note "deleted the run's durable consumer $name from production NATS" ;;
      absent) ;;
      *) note "could not delete the durable consumer $name from production NATS; it may remain" ;;
    esac
  done
  return 0
}
# remove_run_branches closes each pull request the run left open on the smoke repository and deletes
# each tree's branch legion/<tree> there (tree 2's is the fixture's): the run's own, which a run that
# stops before the proof human's merge would otherwise leave behind. It makes no gh call unless
# require_proof_human passed: a refused run's gh acts as someone else.
remove_run_branches() {
  local issue number
  [ -n "$proof_human" ] || return 0
  for issue in $tree1 $tree2 $tree3 $tree4; do
    number=$(timeout 60 gh -R "$repo" pr list --head "legion/$issue" --state open --json number --jq '.[0].number // empty' 2>/dev/null)
    if [ -n "$number" ]; then
      timeout 60 gh -R "$repo" pr close "$number" --comment "Closed by the Stage 4b run that opened it, at its teardown." >/dev/null 2>&1 &&
        note "closed the run's open pull request $repo#$number (legion/$issue)"
    fi
    timeout 60 gh api -X DELETE "repos/$repo/git/refs/heads/legion/$issue" >/dev/null 2>&1 && note "deleted the run's branch legion/$issue from $repo"
  done
  return 0
}
# collect_transcripts copies every Oh My Pi session the run held into $evidence/transcripts: each
# tree pod's, from its tree volume, and the operator's controller's, which lives in the run's own
# profile on this machine and goes with that profile at teardown (transcripts/controller).
collect_transcripts() {
  local pod
  for tree in $tree1 $tree2 $tree3 $tree4; do
    pod=$(tree_pod "$tree") || continue
    op exec "$pod" -c worker -- tar -C /home/legion/.omp/profiles/legion/agent/sessions -cf - . 2>/dev/null |
      tar -C "$evidence/transcripts" -xf - 2>/dev/null || true
  done
  if [ -d "$profile_agent/sessions" ]; then
    mkdir -p "$evidence/transcripts/controller"
    cp -R "$profile_agent/sessions/." "$evidence/transcripts/controller/"
  fi
}
cleanup() {
  local status=$? p teardown_failed=""
  # A second signal must not cut the teardown short, and a closed output must not end it.
  trap '' HUP INT TERM PIPE
  exec >&7 2>&7
  set +e
  # Teardown is best effort, and errexit off does not turn the ERR trap off: a cleanup command that
  # fails is a warning about the teardown, never a check's FAIL line, and the exit status stays the
  # one the checks set. Inside this EXIT trap BASH_COMMAND is still the command the trap interrupted,
  # so the warning names the line alone.
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  stop_tree "$shape_pid"
  [ -z "$tree1" ] || record_pair >/dev/null 2>&1
  stop_pid "$daemon_pid"
  collect_transcripts
  stop_tree "$watch_pid"
  stop_tree "$events_pid"
  stop_pid "$leaks_pid"
  stop_tree "$sampler_pid"
  stop_tree "$interests_pid"
  # The namespace label, the durable consumers and the project are shared by every Stage 4b run,
  # so a run that never passed prerequisites' ownership checks (the lock, the ports, no leftover
  # objects or consumers) owns none of them and removes nothing.
  if [ -n "$locked" ]; then
    # The teardown waits for the run's labelled pods to go but deletes no pod itself, so the run's
    # own control pods (the memory hog, the reachability pod) go first.
    op delete pod -l "legion.dev/project=$run_label,legion.dev/e2e-control" --ignore-not-found --wait=false >/dev/null 2>&1
    teardown
    if [ -z "$compared" ] && [ -n "$snapshotted" ]; then (namespace_clean) || teardown_failed+="${teardown_failed:+, }namespace-clean"; fi
    delete_consumers
    remove_run_branches
    if [ -z "$audited" ] && [ -n "$prod_baseline" ] && ! production_audit; then
      teardown_failed+="${teardown_failed:+, }production-audit"
      echo "CHECK production-audit: FAIL: $(audit_failure)"
    fi
  fi
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null; done
  docker rm -f "$pg_container" >/dev/null 2>&1
  rm -rf "$work"
  # The notes are for a checkpoint that failed itself: none once ok is set, after a blocked
  # checkpoint, or after a signal (129, 130, 143, as trapped below). Otherwise a failed teardown
  # check (a namespace left dirty, a write outside LEGSMOKE) names itself, and only a clean teardown
  # lets a blocked checkpoint end BLOCKED.
  if [ -z "$ok" ] && [ -z "$was_blocked" ] && [[ ! $status =~ ^(129|130|143)$ ]]; then
    bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --notes "$evidence/model-gateway" "$check_started" "$check" || true
    echo "stage 4b e2e: FAIL (check $check)"
  elif [ -n "$teardown_failed" ]; then
    echo "stage 4b e2e: FAIL (check $teardown_failed, in the teardown after check $check)"
  elif [ -n "$was_blocked" ]; then
    echo "stage 4b e2e: BLOCKED (check $check): the checkpoint could not run, so the run is no verdict on the change; the checkpoints before it stand"
  elif [ -z "$ok" ]; then
    echo "stage 4b e2e: FAIL (check $check)"
  fi
  echo "evidence: $evidence (transcript.log, logs/daemon.log, pod-watch.json, pods/, transcripts/, the namespace snapshots)"
  [ -z "$teardown_failed" ] || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the production audit (checkpoint production-audit) --------------------------------------------

# production_baseline opens the audit's window, once production Dispatch answers: before the daemon
# starts, so its boot writes fall inside, and to the nanosecond, so a write in the baseline's own
# second compares as the time it is.
production_baseline() {
  dispatch_get "issues?project=$project" >/dev/null || fail "read production Dispatch"
  prod_baseline=$(date -u +%FT%T.%NZ)
}
# production_audit fails on any production write the run made outside LEGSMOKE, and on any sampled
# interest of the run's sessions outside it.
production_audit() {
  local sessions actors
  audited=1
  sessions=$(jq -R -s -c 'split("\n") | map(fromjson? | select(.msg | IN("api: claim registered", "api: controller registered")) | .session) | unique' "$daemon_log")
  printf '%s\n' "$sessions" >"$evidence/run-sessions.json"
  # Production Dispatch is busy with other work while the run goes on, so an issue outside
  # LEGSMOKE updated since the baseline is the run's write only when one of its events names one
  # of the run's own writers: its agents' sessions, the daemon, and the proof human.
  actors=$(jq -c --arg daemon "legion-daemon:$project" --arg human "$dispatch_actor" '. + [$daemon, $human]' <<<"$sessions")
  touched_outside "$actors" >"$evidence/production-issues-touched-outside.json" ||
    printf '"the production issues outside %s could not be read"\n' "$project" >"$evidence/production-issues-touched-outside.json"
  # Envoy lists no roles, and a session's interests leave the listener with it, so the run samples
  # its sessions' interests every 5 s and at every checkpoint while its daemon runs. A registered
  # session with no sample the listener answered leaves the audit unable to vouch for it, and so do
  # three unanswered samples of one session in a row (interests_unanswered): either fails the audit.
  local sampled unvouched unanswered outside
  sampled=$(jq -s -c '[.[].session_id] | unique' "$evidence/interests.jsonl" 2>/dev/null) || sampled='[]'
  unvouched=$(jq -c --argjson sampled "$sampled" '[.[] | select(. as $s | $sampled | index($s) | not)]' <<<"$sessions")
  unanswered=$(interests_unanswered "$evidence/interests-outcomes.txt") || unanswered='["the interest sample outcomes could not be read"]'
  note_listener_restarts
  if [ "$unanswered" != "[]" ]; then
    printf '%s\n' "$unanswered" >"$evidence/production-interests-outside.json"
  elif [ "$unvouched" != "[]" ]; then
    jq -c '[{"registered sessions with no interest sample": .}]' <<<"$unvouched" >"$evidence/production-interests-outside.json"
  elif outside=$(interests_outside "$evidence/interests.jsonl"); then
    printf '%s\n' "$outside" >"$evidence/production-interests-outside.json"
  else
    printf '"the interest samples could not be read"\n' >"$evidence/production-interests-outside.json"
  fi
  audit_verdict "$evidence/production-issues-touched-outside.json" "$evidence/production-interests-outside.json"
}
# note_listener_restarts records every listener-restart episode in $evidence/listener-restarts.json
# and names, in a note, the release of the production listener whose run spans them, when one does.
listener_release_repo=sjawhar/legion
note_listener_restarts() {
  local episodes first last releases
  episodes=$(listener_restarts "$evidence/interests-outcomes.txt") || { note "the listener-restart episodes could not be read"; return 0; }
  printf '%s\n' "$episodes" >"$evidence/listener-restarts.json"
  [ "$episodes" != "[]" ] || return 0
  first=$(jq -r 'map(.start) | min' <<<"$episodes")
  last=$(jq -r 'map(.end // .start) | max' <<<"$episodes")
  releases=$(timeout 60 gh run list -R "$listener_release_repo" --workflow 'Release Envoy Listener' --limit 30 --json databaseId,headSha,createdAt,updatedAt 2>/dev/null |
    jq -r --arg at "$first" '[.[] | select(.createdAt <= $at and $at <= .updatedAt) | "run \(.databaseId) (\(.headSha[0:10]))"] | join(", ")') || releases=
  note "the Envoy listener restarted from $first to $last: $(jq -r 'length' <<<"$episodes") episodes over $(jq -r 'map(.session) | unique | length' <<<"$episodes") sessions, $(jq -r 'map(.samples) | add' <<<"$episodes") samples answered 503 service starting; release: ${releases:-none known}"
}
# event_time_def is the jq definition of an event's time, Dispatch's created_at, as seconds since the
# epoch with its fraction: as strings, "…:19.5Z" sorts before "…:19Z". An event without one, or a
# time that is not RFC3339 UTC, stops the read rather than counting as older than the baseline.
event_time_def='def ts: (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?Z$") // error("not an RFC3339 UTC time: \(.)")) | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | "0." + . | tonumber); def event_time: (.created_at // error("event \(.seq) of \(.issue_key) has no created_at")) | ts;'
# touched_outside ACTORS prints, as one JSON array, every event since the baseline on a production
# issue outside LEGSMOKE whose actor is one of ACTORS (a JSON array of actor ids), or fails when a
# read fails, so a Dispatch it could not read never passes as untouched.
touched_outside() {
  local actors=$1 keys key touched='[]'
  keys=$(dispatch_get "issues?updated_since=$prod_baseline" | jq -r --arg p "$project-" '.[].key | select(startswith($p) | not)') || return 1
  for key in $keys; do
    touched=$(dispatch_events "$key" | jq -c --argjson actors "$actors" --arg since "$prod_baseline" --arg key "$key" --argjson so_far "$touched" \
      "$event_time_def"' $so_far + [.[] | select(event_time >= ($since | ts) and (.actor.id as $id | $actors | index($id))) | {issue: $key, seq, type, actor: .actor.id, created_at}]') || return 1
  done
  printf '%s\n' "$touched"
}
# find_outside_writer sets outsider to the actor of one event since the baseline on a production
# issue outside LEGSMOKE, or to nothing when no such issue was updated: the collector's control on
# real data. Every event moves its issue's updated_at (the Dispatch broker), so an issue the listing
# returns has an event since the baseline, and a listing whose events all read as older fails.
find_outside_writer() {
  local keys key
  outsider=
  keys=$(dispatch_get "issues?updated_since=$prod_baseline" | jq -r --arg p "$project-" '.[].key | select(startswith($p) | not)') || fail "list production issues updated since $prod_baseline"
  [ -n "$keys" ] || return 0
  for key in $keys; do
    outsider=$(dispatch_events "$key" | jq -r --arg since "$prod_baseline" "$event_time_def"' [.[] | select(event_time >= ($since | ts)) | .actor.id | select(. != null)] | first // empty') || fail "read $key's events"
    [ -z "$outsider" ] || return 0
  done
  fail "$(wc -w <<<"$keys") issues outside $project were updated since $prod_baseline, and none of their events reads as since then"
}
# interests_sample LABEL appends the Envoy interests of every session the run has registered so far
# (its agents' claims and the operator's controller)
# to $evidence/interests.jsonl, each line labelled, and each attempt's outcome to
# $evidence/interests-outcomes.txt as "TIME SESSION ok|absent|error DETAIL". A session no longer
# registered answers 404 (absent). No answer, or any other, is an error, so an unreadable listener
# never reads as a clean sample.
interests_sample() {
  local session code now tmp=$work/interest.$BASHPID.json
  for session in $(jq -R -r 'fromjson? | select(.msg | IN("api: claim registered", "api: controller registered")) | .session' "$daemon_log" | sort -u); do
    now=$(date -u +%FT%T.%3NZ)
    if ! code=$(curl -sS --max-time 20 -o "$tmp" -w '%{http_code}' -H "@$work/envoy-auth-header" "$envoy_url/v1/interests/$session" 2>&1); then
      printf '%s %s error unreachable: %s\n' "$now" "$session" "$(tr '\n' ' ' <<<"$code" | scrub)" >>"$evidence/interests-outcomes.txt"
      continue
    fi
    case "$code" in
      200)
        jq -c --arg at "$1" --arg time "$now" '{at: $at, time: $time} + .' "$tmp" >>"$evidence/interests.jsonl"
        printf '%s %s ok\n' "$now" "$session" >>"$evidence/interests-outcomes.txt"
        ;;
      404) printf '%s %s absent\n' "$now" "$session" >>"$evidence/interests-outcomes.txt" ;;
      *) printf '%s %s error answered %s: %s\n' "$now" "$session" "$code" "$(head -c 200 "$tmp" | tr '\n' ' ')" >>"$evidence/interests-outcomes.txt" ;;
    esac
  done
}
# outcome_sessions_def is the jq definition of sessions: the sample outcomes of $evidence/
# interests-outcomes.txt (read raw, one string), grouped by session in the order sampled, each
# folded into its longest run of unanswered samples and its listener-restart episodes.
#
# A restart episode is the listener answering 503 {"error":"service starting"}, as it does while a
# release restarts it: a rolling deploy interleaves those 503s with answers from the task it
# replaces. An episode opens at a session's first such 503 and takes every such 503 of the session
# within restart_bound seconds of it; it ends at the session's first answered sample (ok or absent)
# after its last 503. A 503 later than the bound opens the next episode. The episode's 503s count
# toward no run of unanswered samples, and break none. Any other error (no answer, another code, a 503 with another body) counts: three in a row
# is a listener not answering for that session, which the audit cannot vouch past. The sampler
# leaves 5 s between a session's samples; one or two failures widen that gap to 10 or 15 s, a blip
# the evidence keeps.
# shellcheck disable=SC2016 # a jq program: its $ are jq's
outcome_sessions_def='def ts: (capture("^(?<s>[0-9-]+T[0-9:]+)(\\.(?<f>[0-9]+))?Z$") // error("not an RFC3339 UTC time: \(.)")) | ((.s + "Z") | fromdateiso8601) + ((.f // "0") | "0." + . | tonumber);
def sessions:
  split("\n") | map(select(. != "") | (capture("^(?<time>\\S+) (?<session>\\S+) (?<kind>ok|absent|error)(?: (?<detail>.*))?$") // error("not a sample outcome: \(.)")) | . + {t: (.time | ts)})
  | map(. + {restart: (.kind == "error" and ((.detail // "") | test("^answered 503: \\{\"error\":\"service starting\"\\}\\s*$")))})
  | group_by(.session)
  | map(reduce .[] as $s ({session: .[0].session, run: 0, worst: 0, episodes: []};
      if $s.restart then
        if .open and $s.t <= .open.st + $bound then .open.samples += 1 | .open.last = $s.time | .open.end = null | .open.seconds = null
        else (if .open then .episodes += [.open] else . end) | .open = {start: $s.time, st: $s.t, last: $s.time, samples: 1, end: null, seconds: null} end
      elif $s.kind == "error" then
        .run += 1 | (if .run == 1 then .from = $s.time else . end)
        | (if .run > .worst then .worst = .run | .wfrom = .from | .wto = $s.time | .wlast = "\($s.time) \($s.session) error \($s.detail // "")" else . end)
      else
        .run = 0 | (if .open and .open.end == null then .open.end = $s.time | .open.seconds = ($s.t - .open.st) else . end)
      end)
    | (if .open then .episodes += [.open] | .open = null else . end));'
# interests_unanswered OUTCOMES prints, as one JSON array, what leaves the audit unable to vouch for
# a session's samples: three unanswered in a row; a restart episode the session did not answer
# after within restart_bound seconds of its start; or a second episode within restart_span seconds
# of the first.
restart_bound=300
restart_span=600
interests_unanswered() {
  jq -R -s -c --argjson bound "$restart_bound" --argjson span "$restart_span" "$outcome_sessions_def"' [sessions[]
    | (select(.worst >= 3) | {"sessions the Envoy listener left unanswered for 3 samples in a row": {session, consecutive: .worst, from: .wfrom, to: .wto, last: .wlast}}),
      (.session as $session | .episodes[] | select(.end == null) | {"a listener restart the session never answered after": {session: $session, start, last, samples}}),
      (.session as $session | .episodes[] | select(.end != null and .seconds > $bound) | {"a listener restart longer than \($bound) s": {session: $session, start, end, seconds, samples}}),
      (.session as $session | [.episodes[] | .st] as $starts | range(1; $starts | length) | select($starts[.] - $starts[. - 1] < $span) | {"more than one listener restart within \($span) s": {session: $session, starts: [$starts[. - 1], $starts[.]]}})]' "$1"
}
# listener_restarts OUTCOMES prints, as one JSON array, every restart episode of every session:
# its start, end and sample count.
listener_restarts() {
  jq -R -s -c --argjson bound "$restart_bound" "$outcome_sessions_def"' [sessions[] | .session as $session | .episodes[] | {session: $session, start, end, samples, seconds}]' "$1"
}
# start_interests_sampler samples every 5 s while the daemon runs, besides every checkpoint's pass,
# so a session that registers and ends between two checkpoints is still seen.
start_interests_sampler() {
  : >>"$evidence/interests-outcomes.txt"
  (
    trap - EXIT ERR
    set +e
    while kill -0 "$$" 2>/dev/null; do
      interests_sample sampler
      sleep 5
    done
  ) >/dev/null 2>&1 9>&- 7>&- &
  interests_pid=$!
}
# interests_outside FILE prints, as one JSON array, every sampled topic in FILE outside the run: a
# topic of the run names LEGSMOKE, the project's own subject space (notifications.legion.legsmoke.),
# its repository's GitHub subjects (notifications.github.sjawhar.legion-smoke., where an agent
# follows its own pull request), operator-close's tree ($optree), a legion-legsmoke- role, or the
# session itself.
interests_outside() {
  jq -s -c --arg p "$project" --arg t "legion-$run_label-" --arg space "notifications.legion.$run_label." --arg repo "notifications.github.${repo/\//.}." --arg op "$optree" '[.[] | .session_id as $s | .topics[]?
    | select((contains($p) or contains($t) or startswith($space) or startswith($repo) or contains($op) or contains($s)) | not) | {session: $s, topic: .}] | unique' "$1"
}
audit_verdict() { [ "$(jq -c . "$1")" = "[]" ] && [ "$(jq -c . "$2")" = "[]" ]; }
# audit_failure is what a failed production audit says, in the checkpoint and in the teardown.
audit_failure() { echo "the run wrote outside LEGSMOKE or subscribed outside it: $evidence/production-issues-touched-outside.json, $evidence/production-interests-outside.json"; }

# ==== checkpoints ===================================================================================

begin prerequisites
for tool in go docker jq curl ss kubectl aws gh bun jj mise shellcheck secrets setpriv pgrep; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -n "$runtime_context" ] || fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the daemon runs as the Legion daemon's restricted identity, never the operator's"
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
case "$image" in *@sha256:*) ;; *) fail "LEGION_E2E_IMAGE must be the worker image by digest (…@sha256:…), not '$image'" ;; esac
# The production services, each a fully-qualified host (a bare alias resolves through whatever
# search domain the box or the pod has) and no path. A refusal names the variable, never its value.
fqdn='[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+'
[[ $dispatch_base =~ ^https://$fqdn(:[0-9]+)?$ ]] ||
  fail "LEGION_E2E_DISPATCH_URL is unset or not production Dispatch's https:// URL: a fully-qualified host, an optional port, no path"
[[ $envoy_url =~ ^https?://$fqdn(:[0-9]+)?$ ]] ||
  fail "LEGION_E2E_ENVOY_URL is unset or not the production Envoy listener's http(s):// URL: a fully-qualified host, an optional port, no path"
[[ $nats_url =~ ^nats://$fqdn:[0-9]+$ ]] ||
  fail "LEGION_E2E_NATS_URL is unset or not production NATS as nats://host:port with a fully-qualified host"
nats_host=${nats_url#nats://} && nats_port=${nats_host##*:} && nats_host=${nats_host%:*}
gateway=$(bash "$root/scripts/e2e/lib/model-gateway-url.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_URL is not a model gateway URL the operator route's models.yml can name (the reason is above)"
gateway_audience=$(bash "$root/scripts/e2e/lib/model-gateway-audience.sh") ||
  fail "LEGION_E2E_MODEL_GATEWAY_AUDIENCE is not a token audience the operator route's pod.yml can carry (the reason is above)"
[[ $dispatch_token_secret_id =~ ^[A-Za-z0-9/_+=.@:-]+$ ]] ||
  fail "LEGION_E2E_DISPATCH_TOKEN_SECRET_ID is unset or not a Secrets Manager secret id or ARN"
[[ $envoy_token_secret_id =~ ^[A-Za-z0-9/_+=.@:-]+$ ]] ||
  fail "LEGION_E2E_ENVOY_TOKEN_SECRET_ID is unset or not a Secrets Manager secret id or ARN"
# The gateway's health endpoint is at its origin.
gateway_origin=$(sed -E 's#^(https://[^/]+).*#\1#' <<<"$gateway")
service_hosts=("${dispatch_base#https://}" "${envoy_url#*://}" "$nats_host" "${gateway_origin#https://}")
require_proof_human
mkdir -p "$(dirname "$lock")"
exec 9>"$lock"
flock -n 9 || fail "another Stage 4b run holds $lock: one run at a time"
refuse_leftovers legion-e2e4b
for port in "$port_daemon" "$port_worker_stream"; do
  [ -z "$(ss -Hltn "sport = :$port")" ] || fail "port $port is taken on the devbox: $(ss -Hltnp "sport = :$port")"
done
imds=$(curl -sf -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60') ||
  fail "instance metadata is unreachable; the daemon binds the devbox's private address, read from it"
host=$(curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4) || fail "instance metadata has no local-ipv4"
unset imds
leftover=$(op get sandboxes,pods,pvc,configmaps -l "legion.dev/project=$run_label" -o name 2>&1) || fail "the operator context cannot list namespace $namespace: $leftover"
[ -z "$leftover" ] || fail "namespace $namespace already holds objects labelled legion.dev/project=$run_label, which another run left or owns: $(tr '\n' ' ' <<<"$leftover")"
stale_consumers=$(nats_stream consumers "$nats_url" "$stream" "legion-go-$project-") ||
  fail "production NATS $stream could not list its consumers"
[ -z "$stale_consumers" ] || fail "production NATS already holds durable consumers of $project, which another run left or owns: $(jq -r .name <<<"$stale_consumers" | tr '\n' ' ')"
# The run owns the namespace label, the consumers and the project only from here: a run refused
# above leaves another run's objects alone.
locked=1
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root") || fail "lib/built-from.sh could not read the source revision"
revision=$(sed -n 's/^source: //p' <<<"$built")
jq -n --arg revision "$revision" --arg image "$image" --arg plugin "$(jq -r '.name + "@" + .version' "$root/packages/pi-envoy/package.json")" \
  --arg started "$(date -u +%FT%TZ)" '{revision: $revision, image: $image, plugin: $plugin, started: $started}' >"$evidence/run.json"
note "source $revision; image $image; plugin $(jq -r .plugin "$evidence/run.json")"
note "daemon http://$host:$port_daemon, worker stream tcp://$host:$port_worker_stream; runtime identity context $runtime_context in $runtime_kubeconfig; operator context $operator"
if [ -n "$until" ]; then
  # A name no checkpoint has would run the whole proof as a development run.
  grep -qxF -e "begin $until" -e "begin \"$until\"" "$root/scripts/e2e/stage4b-sandbox-tree.sh" ||
    fail "STAGE4B_UNTIL=$until names no checkpoint of this driver"
  note "STAGE4B_UNTIL=$until: a development run, never the proof"
fi
if [ -n "$skip_controller" ]; then
  [ -n "$until" ] || fail "STAGE4B_SKIP_CONTROLLER is for a development run: set STAGE4B_UNTIL too"
  note "STAGE4B_SKIP_CONTROLLER=$skip_controller: controller's checks are skipped; it only takes tree 3 out"
fi
if [ -n "$design_gate" ]; then
  [ "$design_gate" = root-issues ] || fail "STAGE4B_DESIGN_GATE=$design_gate: the one policy it arms is root-issues"
  [ -n "$until" ] || fail "STAGE4B_DESIGN_GATE is for a development run: set STAGE4B_UNTIL too"
  # Only tree 1 exists under the switch, so a checkpoint past spec-posted, which drives trees 2 to
  # 4, cannot run.
  sed -n '1,/^begin spec-posted$/s/^begin //p' "$root/scripts/e2e/stage4b-sandbox-tree.sh" | grep -qxF "$until" ||
    fail "STAGE4B_DESIGN_GATE runs tree 1 alone, so STAGE4B_UNTIL must be spec-posted or a checkpoint before it, not $until"
  note "STAGE4B_DESIGN_GATE=$design_gate: the design gate is armed and tree 1 runs alone"
fi
read_bearers
pass

begin preflight
# The runtime identity is the restricted role, and nothing more: the assumed-role pattern Stage 4a's
# identity check uses (packages/daemon-go/internal/runtime/sandbox/live_install_test.go:52).
who=$(rk auth whoami -o json) || blocked "kubectl auth whoami under $runtime_context failed"
jq -e '.status.userInfo.username | test(":assumed-role/[A-Za-z0-9+=,.@_-]*legion-daemon/")' <<<"$who" >/dev/null ||
  fail "the runtime identity $(jq -r .status.userInfo.username <<<"$who") is not the assumed Legion daemon role"
jq -e '.status.userInfo.groups | index("legion-daemon")' <<<"$who" >/dev/null || fail "the runtime identity is not in group legion-daemon"
[ "$(rk auth can-i list secrets -n "$namespace" 2>/dev/null)" = no ] || fail "the runtime identity can list Secrets in $namespace"
note "[runtime] $(jq -r .status.userInfo.username <<<"$who"); list secrets -n $namespace: no"
kubectl --context "$operator" get crd sandboxes.agents.x-k8s.io -o name >/dev/null || fail "the Agent Sandbox CRD is not installed"
floor=$(kubectl --context "$operator" get nodepool legion -o json |
  jq -c '[.spec.template.spec.requirements[] | select(.key == "karpenter.k8s.aws/instance-cpu")]')
jq -e 'any(.[]; .operator == "Gt" and (.values | index("3")))' <<<"$floor" >/dev/null || fail "the legion NodePool has no instance-cpu Gt 3 floor: $floor"
note "[operator] CRD sandboxes.agents.x-k8s.io installed; NodePool legion floor $floor"
# LEGSMOKE's stale todo roots would be admitted at boot ahead of the run's own.
stale=$(dispatch_get "issues?project=$project&status=todo" | jq -r '.[] | select(.parent == null or .parent == "") | .key')
for key in $stale; do
  set_status "$key" backlog
  note "moved stale todo root $key to backlog"
done
[ -z "$(dispatch_get "issues?project=$project&status=todo" | jq -r '.[].key')" ] || fail "LEGSMOKE still holds todo issues"
# The configured stream carries both halves of the workflow's intake. Dispatch publishes every
# project's issue events under subjects that name the issue, so any project's shows the Dispatch
# half (LEGSMOKE's own age out between runs; admission is where the run's are seen); the GitHub
# half is the smoke repository's own.
for subject in "notifications.dispatch.issue.>" "notifications.github.sjawhar.legion-smoke.>"; do
  seen=$(nats_stream last "$nats_url" "$stream" "$subject" 20) ||
    blocked "production NATS $stream carries no message on $subject (bun scripts/e2e/lib/nats-stream.ts last \$LEGION_E2E_NATS_URL $stream '$subject'): the daemon's intake would miss that half of the workflow"
  note "production NATS $stream carries $subject: newest $(jq -c . <<<"$seen")"
done
# From a throwaway pod on the Legion pool, with the restricted pod shape: every service a Sandbox
# pod reaches.
reach=$work/reach.yaml
cat >"$reach" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: legion-e2e4b-reach-$$
  labels: { legion.dev/project: $run_label, legion.dev/e2e-control: reach }
spec:
  restartPolicy: Never
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  nodeSelector: { legion.dev/pool: legion }
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  securityContext: { runAsNonRoot: true, runAsUser: 1000, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: reach
      image: $image
      command: [bash, -c]
      args:
        - |
          # The image has no curl: bun answers the HTTP services, bash's /dev/tcp the NATS port. A
          # fresh node's first outbound connection can fail while the node settles, so each service
          # gets three tries, 5 s apart, and one that never answers is printed with every try's error.
          # Each line names its service, never its address, and an error's host becomes the service.
          for probe in dispatch=$dispatch_base/healthz listener=$envoy_url/healthz gateway=$gateway_origin/health; do
            service=\${probe%%=*}
            printf '%s ' "\$service"
            SERVICE="\$service" URL="\${probe#*=}" bun -e 'const host = new URL(process.env.URL).hostname; const errors = []; for (let attempt = 1; attempt <= 3; attempt++) { const r = await fetch(process.env.URL, { signal: AbortSignal.timeout(10000) }).catch((e) => e); if (r instanceof Response) { console.log(r.status); process.exit(0); } errors.push("attempt " + attempt + ": " + String(r && r.name) + ": " + String(r && r.message).split(host).join("<" + process.env.SERVICE + ">").replace(/\s+/g, " ")); if (attempt < 3) await Bun.sleep(5000); } console.log("unreachable (" + errors.join("; ") + ")");'
          done
          printf 'nats '

          errors=
          for attempt in 1 2 3; do
            answer=\$(timeout 5 bash -c 'exec 3<>/dev/tcp/$nats_host/$nats_port && head -c 4 <&3' 2>&1) && break
            errors="\$errors attempt \$attempt: \$(tr '\n' ' ' <<<"\${answer:-no answer}" | sed 's#$nats_host#<nats>#g');"
            answer=
            [ "\$attempt" = 3 ] || sleep 5
          done
          if [ -n "\$answer" ]; then printf '%s' "\$answer"; else printf 'unreachable (%s)' "\${errors# }"; fi
          echo
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
EOF
op apply -f "$reach" >/dev/null || blocked "the operator could not create the reachability pod ($reach)"
until_true 600 "the reachability pod to finish" sh -c "timeout 120 kubectl --context '$operator' -n '$namespace' get pod legion-e2e4b-reach-$$ -o jsonpath='{.status.phase}' | grep -qx 'Succeeded\|Failed'"
op logs "legion-e2e4b-reach-$$" >"$evidence/reach.txt" 2>&1
op delete pod "legion-e2e4b-reach-$$" --wait=false >/dev/null 2>&1
while read -r service answer; do
  case "$service" in
    dispatch) source=LEGION_E2E_DISPATCH_URL ;;
    listener) source=LEGION_E2E_ENVOY_URL ;;
    gateway) source=LEGION_E2E_MODEL_GATEWAY_URL ;;
    nats) source=LEGION_E2E_NATS_URL ;;
    *) fail "the reachability pod printed a line naming no service: $(scrub <<<"$service $answer")" ;;
  esac
  case "$answer" in 000 | unreachable* | "") fail "a pod on the Legion pool cannot reach $service ($source): $answer ($evidence/reach.txt)" ;; esac
  note "[pod] $service ($source) → $answer"
done <"$evidence/reach.txt"
pass

begin pod-watch
snapshot "$evidence/namespace-before.txt" || fail "the operator could not list namespace $namespace"
snapshotted=1
: >"$evidence/driver-actions.txt"
start_pod_watch
note "streaming the run's pods, the nodes' events, and node memory into $evidence (pod-watch.json, node-events.json, node-memory.txt)"
pass

begin boot
(cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion)
stage_role_prompts "$root" "$work"
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion") || fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"
# The build's source is the run's recorded source, or the tree changed between the two.
[ "$(sed -n 's/^source: //p' <<<"$built")" = "$(jq -r .revision "$evidence/run.json")" ] ||
  fail "the tree changed between prerequisites ($(jq -r .revision "$evidence/run.json")) and the build ($(sed -n 's/^source: //p' <<<"$built"))"
# The prompt bundle is deployed beside this binary, not read from the checkout it was built in.
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=legion \
  -p "127.0.0.1::5432" postgres:16 >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion
mkdir -p "$state"
cat >"$work/instructions.md" <<'EOF'
# Stage 4b proof instructions

This is a throwaway workflow proof on the disposable LEGSMOKE project. A tree's root architect
starts its tree from the daemon's `catch-up` notice as its role says: it writes the spec in the
issue's own primary document and registers the gate, then waits. Apart from that, do not act until
a targeted human Dispatch message gives the next exact proof operation. A phase worker's first
message is the daemon's task line (`Continue <title>. Issue: <key>. Phase: <phase>.`): it names your
phase and is not that message. Read what your role says to read, then reply WAITING and wait for
the targeted message. Follow that instruction precisely, use the Go-daemon Legion tools and
handoffs, and do not create work outside the issue's smoke branch.

## Scope of this proof

The controller hands Legion no issue itself: this proof admits only the issues its driver sets to
`todo`. LEGSMOKE holds earlier runs' roots, and a slot the proof frees stays for the tree the driver
admits next.
EOF
# The daily report is the one controller action that waits for no targeted message (daily-report).
report_title="Legion daily report ($work)"
cat >>"$work/instructions.md" <<EOF

## The controller's daily report

The controller's daily report is the one controller action that waits for no targeted message.
Post it as \`skill://legion-controller\`'s "Daily report" says, on the first turn a \`tick on $project\`
wake starts after your start turn has ended: never in your start turn, where a tick that arrives
while that turn still runs does not count, and once in this run. Its issue is titled
\`$report_title\`: find it with \`dispatch_search\`, and when there is none, create it once with
that title and park it in icebox, as the skill says for the default report issue.
EOF
write_legion_config
out=$("$work/legion" start --check-config --config "$work/legion.yaml" 2>&1) || fail "legion start --check-config refused the proof's config: $out"
note "$out"
create_route_configmap
create_providers_secret
production_baseline
start_daemon
start_interests_sampler
probe=$(log_lines "sandbox runtime: the worker image passed its probe" | tail -1)
[ -n "$probe" ] || fail "the daemon booted with no image probe pass logged"
note "image probe: $(jq -c '{image, model, sandbox}' <<<"$probe")"
[ ! -e "$state/workspaces" ] || fail "the daemon's state_dir has a workspaces/ directory under the Sandbox runtime"
probe_pod=$(jq -r '.sandbox' <<<"$probe")
kubectl --context "$operator" get events -n "$namespace" --field-selector "involvedObject.name=$probe_pod" -o json 2>/dev/null |
  jq -c '[.items[] | {reason, at: (.firstTimestamp // .eventTime), message: (.message | .[0:160])}]' >"$evidence/probe-timeline.json"
note "the first probe attempt's timeline (scheduling, node launch, image pull): $evidence/probe-timeline.json"
pass

begin admitted-issue-cap
if [ -n "$design_gate" ]; then
  # One root: each admitted root's architect requests approval on its own, so a second tree would
  # put a second approval in a human's Inbox. The cap check needs three roots, so it is skipped.
  # Tree 1's document leaves one choice to the human, so its architect has a decision block to
  # settle before it may request approval.
  gate_spec=$(printf '%s\n' "## Summary" "" \
    "A Legion smoke proof of the design gate. Make one tiny, concrete one-file change in \`$repo\`: add one new Markdown file holding a single line that names this issue." "" \
    "Where the file goes is the human's choice, and nobody has made it yet: under \`smoke/\` at the repository root, or under \`docs/smoke/\`." "" \
    "## Scope" "" \
    "A review of the pull request may ask for one more line appended to that same file; that is in scope. Nothing else changes.")
  tree1=$(new_issue "Stage 4b design gate tree 1: the spec's decision blocks, then approval ($work)" "" "$gate_spec")
  set_status "$tree1" todo
  until_true 300 "tree 1 admitted" sh -c \
    "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg a '$tree1' '.admission.active == [\$a]'"
else
  tree1=$(new_issue "Stage 4b proof tree 1: the whole workflow ($work)")
  tree2=$(new_issue "Stage 4b proof tree 2: planner beside tree 1, with the repository fixture ($work)")
  tree3=$(new_issue "Stage 4b proof tree 3: the held phase for the controller ($work)")
  push_fixture "$tree2"
  for issue in "$tree1" "$tree2" "$tree3"; do set_status "$issue" todo; done
  until_true 300 "two roots admitted and one waiting in rank order" sh -c \
    "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg a '$tree1' --arg b '$tree2' --arg c '$tree3' '.admission.cap == 2 and .admission.active == [\$a,\$b] and .admission.waiting == [\$c]'"
fi
state_file admission
note "active $(jq -c .admission.active "$evidence/admission.json"), waiting $(jq -c .admission.waiting "$evidence/admission.json")"
shape_pid=
pod_shape_watcher 9>&- 7>&- &
shape_pid=$!
if [ -n "$design_gate" ]; then skipped "STAGE4B_DESIGN_GATE: tree 1 alone, so the cap is not exercised"; else pass; fi

# drive_spec ISSUE: the architect registers the gate on its own (architect_registers_gate); with
# gates.design off the daemon approves the registered version itself and the issue moves to
# planning.
drive_spec() {
  local issue=$1
  architect_registers_gate "$issue" "the $issue"
  wait_for_phase "$issue" planning
}
gate_open() {
  daemon_state | jq -e --arg issue "$1" --arg artifact "$2" \
    '.issues[$issue].designGate as $g | $g.artifactId == $artifact and $g.approvedVersion != null and $g.approvedVersion == $g.currentVersion'
}
# drive_gated_spec ISSUE waits, up to 12 hours, for a human to answer the spec's decision blocks and
# approve the version the architect asked about, then checks what the architect did
# (lib/design-gate-verdict.jq): its approval request at the approved version carries a summary after
# "Approve <name> (version N)?", a human answered at least one of the spec's decision blocks, and no
# approval request it made on the spec, retracted ones included, named a version holding one open or
# came before a human answered one.
drive_gated_spec() {
  local issue=$1 artifact approved asks version verdict request early blocks
  local -a requested=()
  artifact=$(dispatch_get "issues/$issue" | jq -er .primary_artifact_id)
  wait_for_worker "$issue" architect
  until_true 300 "the $issue architect to be given its catch-up notice" notice_delivered "$issue" architect "$(notice_needle catch-up "$issue")"
  until_true 43200 "a human to approve the $issue spec in Dispatch, opening its design gate" gate_open "$issue" "$artifact"
  approved=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.approvedVersion')
  asks=$(dispatch_get "issues/$issue/asks")
  jq . <<<"$asks" >"$evidence/$issue-asks.json"
  # Each version of the spec an approval request named, as the human was asked to approve it.
  for version in $(jq -r --arg artifact "$artifact" '[.[] | select(.kind == "approval" and .approval.artifact_id == $artifact) | .approval.version] | unique | .[]' <<<"$asks"); do
    dispatch_get "artifacts/$artifact/versions/$version" >"$evidence/$issue-spec-v$version.json" ||
      fail "$issue: version $version of its spec could not be read"
    requested+=("$evidence/$issue-spec-v$version.json")
  done
  verdict=$(jq -c -s --arg artifact "$artifact" --argjson version "$approved" -f "$root/scripts/e2e/lib/design-gate-verdict.jq" "$evidence/$issue-asks.json" "${requested[@]}")
  printf '%s\n' "$verdict" >"$evidence/$issue-gate-verdict.json"
  request=$(jq -r '.request // empty' <<<"$verdict")
  [ -n "$request" ] || fail "$issue: no approval request names version $approved of its spec ($evidence/$issue-asks.json)"
  jq -e .summarized <<<"$verdict" >/dev/null || fail "$issue: the approval request carries no summary: $request"
  early=$(jq -c .early <<<"$verdict")
  [ "$early" = "[]" ] || fail "$issue: approval was requested before the spec's decision blocks were settled: $early"
  blocks=$(jq .blocks <<<"$verdict")
  [ "$blocks" -gt 0 ] || fail "$issue: a human answered none of the spec's decision blocks, so its open choice was never settled as one ($evidence/$issue-asks.json)"
  note "$issue: a human answered $blocks of the spec's decision blocks, and every approval request came after those answers on a version with none open"
  note "$issue: the approval request at version $approved asked: $request"
  wait_for_phase "$issue" planning
}

begin spec-posted
specs=("$tree1")
if [ -n "$design_gate" ]; then
  drive_gated_spec "$tree1"
else
  specs+=("$tree2")
  for issue in "${specs[@]}"; do drive_spec "$issue"; done
fi
for issue in "${specs[@]}"; do
  version=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.currentVersion')
  note "$issue: the architect posted its spec (version $version) and the daemon moved it to planning"
done
pass

begin tree-separation
wait_for_worker "$tree1" planner
record_resident "$tree1" planner || fail "tree 1's planner has no session and pod to keep: $(claim_view "$tree1" planner)"
send_agent "$tree1" planner "Stage 4b proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$tree1" implementing 900
wait_for_worker "$tree1" implementer
record_resident "$tree1" implementer || fail "tree 1's implementer has no session and pod to keep: $(claim_view "$tree1" implementer)"
wait_for_worker "$tree2" planner
node_of_tree() { op get pods -l "legion.dev/project=$run_label,legion.dev/tree=$1" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u; }
at=$(date -u +%FT%TZ)
nodes1=$(node_of_tree "$tree1")
nodes2=$(node_of_tree "$tree2")
[ "$(wc -l <<<"$nodes1")" = 1 ] && [ "$(wc -l <<<"$nodes2")" = 1 ] || fail "a tree spans nodes at $at: tree 1 $nodes1, tree 2 $nodes2"
[ "$nodes1" != "$nodes2" ] || fail "at $at tree 1 and tree 2 share node $nodes1"
note "at $at: tree 1 ($tree1, implementer running) on $nodes1; tree 2 ($tree2, planner running) on $nodes2"
start_memory_hog "$nodes1"
until_true 300 "the memory hog on $nodes1 to be OOMKilled" hog_oomkilled
op delete pod "legion-e2e4b-memory-hog-$$" --wait=false >/dev/null
note "the memory hog on tree 1's node $nodes1 was OOMKilled by its own 64Mi limit"
pass

begin repository-configuration
# Tree 2's pods carry the repository's own configuration (push_fixture). Each marker names a loading
# path the pod's agent loaded, as an agent in any checkout does. The markers live in the
# pod's own /tmp, which goes with the pod when its tree closes, and are read while the planner waits
# after its first turn, before it plans. The argv the pod ran
# its agent with is recorded beside them.
nonce="fixture-read-$RANDOM$RANDOM"
send_agent "$tree2" planner "Stage 4b proof repository-configuration operation: read this repository's README and AGENTS.md, then answer this message with the single word $nonce and wait for the next instruction. Do not write a handoff yet."
until_true 900 "tree 2's planner to answer $nonce" assistant_said "$tree2" planner "$nonce"
pod=$(claim_sandbox "$tree2" planner) || fail "tree 2's planner has no Sandbox"
# No marker proves nothing unless the fixture is in the workspace the agent runs in: a workspace
# provisioned from another branch reads the same.
fixture_dir=/legion/workspaces/$repo/${tree2,,}
present=$(pod_exec "$pod" sh -c "cd '$fixture_dir' && ls .omp/extensions/fixture.ts && grep -l legion-fixture AGENTS.md" 2>&1) ||
  fail "tree 2's workspace $fixture_dir does not carry the fixture of $fixture_branch: $present"
note "tree 2's workspace $fixture_dir carries the fixture: $(tr '\n' ' ' <<<"$present")"
markers=$(fixture_markers "$pod") || fail "the fixture markers could not be read in $pod: $markers"
printf '%s\n' "$markers" >"$evidence/fixture-markers.txt"
argv=$(op get pod "$pod" -o json | jq -c '[.spec.containers[] | select(.name == "worker") | .command[]?]')
note "tree 2 pod $pod: fixture markers [${markers:-none}]; agent argv $argv"
if grep -q -- '--no-extensions' <<<"$argv"; then note "the pod's agent runs with --no-extensions"; else note "the pod's agent runs without --no-extensions"; fi
send_agent "$tree2" planner "Stage 4b proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary."
wait_for_phase "$tree2" implementing 900
pass

begin issue-cap-moves
# The proof human's session holds no claim in tree 2, so its backlog on tree 2's live root is set
# back: Dispatch shows the daemon's own status again, written as legion-daemon:$project, tree 2's
# architect is told who wrote backlog, and tree 2 keeps its slot. Tree 2 then leaves the line
# through `legion status`, the daemon's own write: its slot frees and the waiting root takes it.
set_status "$tree2" backlog
until_true 300 "the daemon to set $tree2 back from backlog" status_set_back "$tree2" backlog
needle=$(notice_needle status-reasserted "$tree2")
until_true 300 "tree 2's architect to be told of the proof human's backlog" notice_delivered "$tree2" architect "$needle"
# head ends the pipeline early, which pipefail would report as a failure, hence `|| true`: an empty
# line fails the check below, naming it.
told=$(notice_line "$tree2" architect "$needle" | head -1 || true)
printf '%s\n' "$told" >"$evidence/notice-status-reasserted.jsonl"
grep -qF -- "$dispatch_actor" <<<"$told" || fail "tree 2's status-reasserted notice does not name the proof human $dispatch_actor: $told"
dispatch_events "$tree2" >"$evidence/tree2-events-set-back.json" || fail "tree 2's events could not be read"
# The control re-attributes every write after the proof human's backlog to the proof human.
jq --arg human "$dispatch_actor" '([.[] | select(.payload.status == "backlog") | .seq] | max) as $w | map(if .seq > $w then .actor.id = $human else . end)' \
  "$evidence/tree2-events-set-back.json" >"$evidence/tree2-events-set-back-negative.json"
expect_failure set-back-actor set_back_by_daemon "$evidence/tree2-events-set-back-negative.json"
set_back_by_daemon "$evidence/tree2-events-set-back.json" || fail "tree 2's backlog was not the proof human's, or the write after it not legion-daemon:$project's"
daemon_state | jq -e --arg b "$tree2" --arg c "$tree3" '(.admission.active | index($b)) != null and (.admission.waiting | index($c)) != null and .issues[$b].phase != "done"' >/dev/null ||
  fail "the proof human's backlog took tree 2 out: $(daemon_state | jq -c --arg b "$tree2" '{admission, phase: .issues[$b].phase}')"
note "the proof human's backlog on tree 2 was set back to $(dispatch_get "issues/$tree2" | jq -r .status) by legion-daemon:$project, its architect told, and tree 2 kept its slot"
take_out "$tree2"
until_true 300 "tree 3 to take tree 2's admission slot" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg c '$tree3' '(.admission.active | index(\$c)) != null'"
note "legion status moved tree 2 to backlog, its pods gone; tree 3 ($tree3) admitted"
pass

begin tree-moved
send_agent "$tree1" implementer "Stage 4b proof implementation operation: make the smallest one-file change described by this issue in your $repo workspace, commit it on legion/$tree1, open its pull request, record the required implementation proof and handoff, then call the legion tool's handoff_complete. Do not merge."
until_true 1200 "implementer pull request on legion/$tree1" sh -c \
  "timeout 60 gh -R '$repo' pr list --head 'legion/$tree1' --state open --json number | jq -e 'length == 1' >/dev/null"
pr_number=$(gh -R "$repo" pr list --head "legion/$tree1" --state open --json number --jq '.[0].number')
wait_for_phase "$tree1" testing 1200
assert_handoff_committer "$tree1" implementer implementing 0
smoke_file=$(pull_request_product_files)
wait_for_worker "$tree1" tester
record_resident "$tree1" tester || fail "tree 1's tester has no session and pod to keep: $(claim_view "$tree1" tester)"
# The adoption on top of a described commit: the tester's @ is a new empty change authored by the
# reviewer App, and the implementer's handoff commit keeps its author.
adoption=$(workspace_jj "$tree1" log -r '@|@-' --no-graph -T 'if(empty, "empty", "change") ++ "|" ++ author.name() ++ "\n"')
note "after the tester's adoption: $(tr '\n' ' ' <<<"$adoption")"
[ "$(sed -n 1p <<<"$adoption")" = "empty|legion-reviewer[bot]" ] || fail "the tester's adoption left @ as '$(sed -n 1p <<<"$adoption")', want a new empty change authored by legion-reviewer[bot]"
[ "$(sed -n 2p <<<"$adoption" | cut -d'|' -f2)" = "legion-implementer[bot]" ] || fail "the adoption rewrote the implementer's commit author: '$(sed -n 2p <<<"$adoption")'"
send_agent "$tree1" tester "Stage 4b proof test operation: inspect the implementer's actual one-file change and pull request #$pr_number, run a focused observable check, record the required test handoff with verdict pass, then call the legion tool's handoff_complete with verdict pass."
wait_for_phase "$tree1" reviewing 1200
assert_handoff_committer "$tree1" tester testing 0
wait_for_worker "$tree1" reviewer
record_resident "$tree1" reviewer || fail "tree 1's reviewer has no session and pod to keep: $(claim_view "$tree1" reviewer)"
# A round no review decides (LEGION-326): the reviewer comments instead of deciding, and completes.
# The daemon leaves the issue in reviewing and tells the architect, naming the head. The proof's
# instructions hold every agent until a targeted message gives its next operation, so the driver
# then tells the architect only to handle that notice as its role says, naming no topic, head or
# decision: the architect takes those from the notice, asks the reviewer for the decision over
# Envoy, and the reviewer's approval ends the round. The proof asks the reviewer nothing more, and
# counts only messages the architect sent after the reviewer's completion. An architect acting on
# the notice unprompted, as it must where no driver holds it, is not what this checkpoint proves
# (LEGION-413).
send_agent "$tree1" reviewer "Stage 4b proof review operation: review pull request #$pr_number in $repo as your role requires, running the deep and code-quality review passes your instructions name as task subagents. This round deliberately proves what the daemon does with a round no review decides: submit your review as legion-reviewer[bot] as a COMMENT review, never APPROVE or REQUEST_CHANGES, take the round's other steps in the order your role gives, and complete the reviewer handoff. Submit no other review until you are asked for the round's decision; when you are, your decision is APPROVE."
pair_session=$(claim_session_file "$tree1" reviewer) || fail "the reviewer on $tree1 has no session file"
until_true 1800 "the reviewer's two thermonuclear dispatches to reach an outcome" pair_settled
record_pair || fail "the reviewer's session and its review pair could not be recorded"
note "the review pair's dispatches are kept in $evidence/review-pair ($(jq -r -s 'map("\(.agent): \(.calls | length) calls, \(.results | length) results, \(.deliveries | length) deliveries") | join("; ")' "$evidence"/review-pair/thermonuclear-*.json))"
until_true 1800 "legion-reviewer[bot]'s COMMENT review of pull request #$pr_number" reviewer_commented
until_true 900 "the daemon to record the reviewer's completion of $tree1's round" reviewer_completed "$tree1"
# The ask baseline is read here, not before the reviewer is instructed: the notice is written in the
# completion's own transaction and the architect needs a model turn after it, so no real ask can
# precede this read, while a message the architect sent the reviewer earlier in the round (a reply to
# the reviewer's own round report) stays out of the count, the path and the saved lines.
asked_before=$(architect_messages "$tree1" reviewer)
# At the completion, before the architect can have asked anything: no approval yet, and the round
# still open.
early_approvals=$(review_app_reviews '.state == "APPROVED"' id) || fail "read the reviews on pull request #$pr_number"
[ -z "$early_approvals" ] || fail "the reviewer approved pull request #$pr_number before anyone asked it for the round's decision"
issue_phase "$tree1" reviewing >/dev/null || fail "$tree1 left reviewing on a round no review decided"
until_true 300 "the review-stuck notice on $tree1's architect" notice_delivered "$tree1" architect "$(notice_needle review-stuck "$tree1")"
stuck=$(notice_line "$tree1" architect "$(notice_needle review-stuck "$tree1")" | head -1 || true)
printf '%s\n' "$stuck" >"$evidence/notice-review-stuck.jsonl"
# The head the notice names is the one the completion left, which GitHub's current head can have
# moved past since: it must be a commit of the pull request, and the reviewer's completion wrote it.
stuck_head=$(grep -oE 'APPROVE of head [0-9a-f]{40}' <<<"$stuck" | head -1 | awk '{print $4}' || true)
[ -n "$stuck_head" ] || fail "the review-stuck notice on $tree1's architect names no head: $stuck"
pr_commits=$(gh api --paginate "repos/$repo/pulls/$pr_number/commits" --jq '.[].sha') || fail "read the commits of pull request #$pr_number"
grep -qx -- "$stuck_head" <<<"$pr_commits" || fail "the review-stuck notice names $stuck_head, which is no commit of pull request #$pr_number"
grep -qF -- "the reviewer's completion" <<<"$stuck" || fail "the review-stuck notice was not written by the reviewer's completion: $stuck"
note "the reviewer commented and completed; $tree1 stayed in reviewing and its architect was told review-stuck naming $stuck_head (kept in $evidence/notice-review-stuck.jsonl)"
# What the checkpoint proves is that a stuck round got unstuck. An ask the architect made on its own,
# before the driver sent its message, is the production behaviour, so it counts too, as the stronger
# pass, and the run notes it.
asked_unprompted=$(architect_messages "$tree1" reviewer)
send_agent "$tree1" architect "Stage 4b proof review-stuck operation: handle the daemon's review-stuck notice on $tree1 now, exactly as your role says to handle a review-stuck notice."
architect_asked() { [ "$(architect_messages "$tree1" reviewer)" -gt "$asked_before" ]; }
until_true 900 "$tree1's architect, sent the review-stuck operation, to ask the reviewer for the round's decision" architect_asked
notice_line "$tree1" reviewer "notifications.role.$(claim_token "$tree1" architect)" | tail -n +"$((asked_before + 1))" >"$evidence/architect-ask.jsonl"
[ "$asked_unprompted" -le "$asked_before" ] || note "the architect asked the reviewer before the driver's message"
until_true 1800 "legion-reviewer[bot] approval of pull request #$pr_number at its head" reviewer_approved_head
note "the architect asked the reviewer over Envoy (kept in $evidence/architect-ask.jsonl), and the reviewer's approval of the head ended the round"
until_true 900 "$tree1 to leave reviewing for retro" issue_phase_in "$tree1" retro merging
if issue_phase "$tree1" retro >/dev/null; then
  wait_for_worker "$tree1" implementer
  send_agent "$tree1" implementer "Stage 4b proof retro: write the required retro handoff for pull request #$pr_number and complete the phase. Do not change the approved implementation."
fi
wait_for_phase "$tree1" merging 1800
note "$tree1 moved planner → implementer → tester → reviewer → retro → merging with real agents; $repo#$pr_number changes $smoke_file"
pass

begin completion-closed
# Each phase worker of tree 1 stays live as its phase ends — the planner, the implementer
# (implementing, and retro when it ran), the tester and the reviewer. The completion a worker reports
# from inside its turn ends its assignment, not its process: the turn runs to its end and the worker
# goes idle in the pod and on the session it first registered with, where it stays until tree 1
# closes. Its live session answers every handoff_complete call, reports each assignment once, and
# records the phase stall `closed` after the call that succeeded: the 4b.13b acceptance's stall
# check (completion_verdict), which a stop inside the call fails. The implementer, handed retro in
# that same session, is where a session holding an unanswered report would report again.
resident_idle() { resident_kept "$1" "$2" && issue_worker_state "$1" "$2" idle; }
for role in planner implementer tester reviewer; do
  resident_kept "$tree1" "$role" || fail "$role on $tree1 did not stay live in the pod and session it first registered with after its phase: $(resident_lost "$tree1" "$role")"
  until_true 300 "$role on $tree1 to go idle in the pod and session it first registered with" resident_idle "$tree1" "$role"
  session_copy="$evidence/completion-$role.jsonl"
  claim_session_text "$tree1" "$role" >"$session_copy" || fail "$role on $tree1 has no readable session"
  completion_verdict <"$session_copy" >"$evidence/completion-$role-verdict.json"
  completions_answered "$session_copy" ||
    fail "$role on $tree1 left a phase completion unanswered, unclosed or repeated: $(cat "$evidence/completion-$role-verdict.json")"
  note "$role on $tree1, idle in its first pod $(cat "$work/resident-$tree1-$role.json"): $(jq -c '[.segments[] | {calls, succeeded, closed}]' "$evidence/completion-$role-verdict.json")"
done
# The control: the planner's session cut at its handoff_complete call, the transcript a stop inside
# the call leaves, is refused.
cut_at_completion_call "$evidence/completion-planner.jsonl" >"$evidence/completion-planner-cut-at-the-call.jsonl"
expect_failure completion-cut-at-the-call completions_answered "$evidence/completion-planner-cut-at-the-call.jsonl"
pass

begin resident-answer
# A role that finished its phase answers while another role's phase is the issue's: tree 1's planner,
# asked a question while tree 1 is in merging, replies in the session and pod it first registered
# with, and tree 1 stays in merging. Nothing relaunched the planner, and its answer moved no phase.
issue_phase "$tree1" merging >/dev/null || fail "$tree1 left merging before the planner was asked: $(daemon_state | jq -c --arg i "$tree1" '.issues[$i].phase')"
nonce="resident-answer-$RANDOM$RANDOM"
send_agent "$tree1" planner "Stage 4b proof resident-role question: your phase is finished and another role's phase is active, so change no file and call no legion operation. Reply to this message with the single word $nonce, then wait."
until_true 900 "tree 1's finished planner to answer $nonce" assistant_said "$tree1" planner "$nonce"
until_true 300 "tree 1's planner to go idle again after its answer" resident_idle "$tree1" planner
resident_kept "$tree1" planner || fail "answering relaunched or replaced tree 1's planner: $(resident_lost "$tree1" planner)"
issue_phase "$tree1" merging >/dev/null || fail "the planner's answer moved $tree1 out of merging: $(daemon_state | jq -c --arg i "$tree1" '.issues[$i].phase')"
note "tree 1's finished planner answered $nonce in its first pod and session $(cat "$work/resident-$tree1-planner.json"); $tree1 stayed in merging"
pass

begin review-pair
# The reviewer's two review passes are the image's thermonuclear agents, dispatched by name, and each
# must have run: one of its runs completed, by the task-result block the reviewer received (a
# delivery or a hub snapshot) or, with none, by its own session, which beside the reviewer's ends in
# an accepted yield, every turn on the review target. A missing agent is refused to the model
# as "Unknown agent", an agent whose declared model the pod cannot resolve fails "No model
# selected", and a model that substitutes the bundled reviewer still posts a verdict, which looks
# the same from outside. tree-moved recorded the dispatches (record_pair); each failure below
# quotes them.
stem=$(cat "$evidence/review-pair/session-stem")
# Both agents declare @review, which the operator's overlay maps. The task executor runs a subagent
# on its parent's model when the subagent's own does not resolve, silently, so each pair session's
# turns must all be on the fixture's review target.
review_target=$(sed -n 's/^  review: \([^:]*\).*/\1/p' "$operator_route/overlay.yml")
[ -n "$review_target" ] || fail "the operator fixture's overlay.yml maps no review role"
for agent in $pair_agents; do
  d=$evidence/review-pair/$agent.json
  [ "$(jq '.calls | length' "$d")" -gt 0 ] || fail "the reviewer dispatched no task naming $agent"
  # A refusal is in a task call's own result or in a run that did not complete; a completed review's
  # output may quote the same words.
  said=$(jq -r '[.results[].text, (.deliveries[].content | select(test("^<task-result [^>]*status=\"completed\"") | not))] | join("\n")' "$d")
  if grep -qF "Unknown agent \"$agent\"" <<<"$said"; then fail "the reviewer's task for $agent was refused: $(grep -F 'Unknown agent' <<<"$said" | head -1)"; fi
  if grep -qF 'No model selected' <<<"$said"; then fail "the reviewer's task for $agent did not run: $(grep -F 'No model selected' <<<"$said" | head -1)"; fi
  # A reviewer may run an agent more than once; one completed run is the pass, and with none the
  # failure quotes what the reviewer received. A run that sent no result (its session only) counts
  # when its own session ends in an accepted yield, below.
  id=$(jq -r --arg agent "$agent" '([.deliveries[].content | capture("<task-result id=\"(?<id>[^\"]+)\" agent=\"" + $agent + "\" status=\"completed\"")? | .id]
    + [.ids[] as $i | select([.deliveries[].content | select(contains("id=\"" + $i + "\""))] | length == 0) | $i]) | first // empty' "$d")
  [ -n "$id" ] || fail "the reviewer's task for $agent did not complete: $(jq -c '[.deliveries[] | .content[0:300]] + [.results[] | {isError, text: .text[0:300]}]' "$d")"
  sub=$evidence/review-pair/$stem/$id.jsonl
  [ -s "$sub" ] || fail "$agent's session $stem/$id.jsonl is not beside the reviewer's"
  jq -R -s -e '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "toolResult" and .message.toolName == "yield" and .message.isError == false)] | length > 0' "$sub" >/dev/null ||
    fail "$agent's session $id holds no accepted yield"
  models=$(jq -R -s -c '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant") | "\(.message.provider)/\(.message.model)"] | unique' "$sub")
  [ "$models" = "$(jq -cn --arg m "$review_target" '[$m]')" ] ||
    fail "$agent's session $id ran on $models, not only the fixture's review target $review_target"
  note "$agent ran as $id on $review_target, its session ending in an accepted yield; its result reached the reviewer by $(jq -r '[.deliveries[].via] | unique | if length == 0 then "no delivery or snapshot" else join(" and ") end' "$d")"
done
pass

begin first-turns
# Every role's first turn in its pod completed: with no model round trip in the image probe, a
# worker's first turn is where a misconfigured model surfaces, and a tree reaching done does not
# say every role got one.
for role in architect planner implementer tester reviewer; do
  text=$(claim_session_text "$tree1" "$role") || fail "$role on $tree1 has no readable session"
  jq -R -s -e '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant")] | first | .message.stopReason | IN("stop", "toolUse")' <<<"$text" >/dev/null ||
    fail "$role on $tree1: its first assistant turn did not complete"
done
note "architect, planner, implementer, tester and reviewer on $tree1 each completed a first turn in a pod"
pass

begin token-rotation
# A pod's projected operator token (pod.yml: expiration_seconds 3600) is renewed by the kubelet at
# 80 % of its life, 2880 s after it was issued, and a model turn after the renewal still runs on the
# gateway's aliases. The architect's pod is the longest-lived. A token whose issue time (iat) is
# later than the pod's start is a renewal: the pod's first token was issued as it started. By
# token-rotation the architect has usually run long enough for one, so the wait is often none.
pod=$(claim_sandbox "$tree1" architect)
pod_uid=$(op get pod "$pod" -o jsonpath='{.metadata.uid}')
pod_started=$(date -d "$(op get pod "$pod" -o jsonpath='{.status.startTime}')" +%s) || fail "the architect pod $pod has no start time"
# token_iat prints the issue time of the pod's token, from its payload alone, or fails: an exec that
# did not answer is never a time. The token itself is never printed.
token_iat() {
  local payload
  payload=$(pod_exec "$pod" cat /var/run/operator/token | cut -d. -f2 | tr '_-' '/+') || return 1
  case $((${#payload} % 4)) in 2) payload="$payload==" ;; 3) payload="$payload=" ;; esac
  base64 -d <<<"$payload" 2>/dev/null | jq -er '.iat | numbers'
}
token_renewed() {
  local iat
  iat=$(token_iat) || return 1
  [ "$iat" -gt $((pod_started + 60)) ]
}
# architect_pod_kept WHEN fails when the architect's pod is no longer the one whose token renewed:
# a replacement's first token is issued at its start, later than the old pod's, and is no renewal.
architect_pod_kept() {
  local now
  now=$(op get pod "$pod" -o jsonpath='{.metadata.uid}')
  [ "$now" = "$pod_uid" ] || fail "the architect's pod was replaced $1 ($pod_uid -> $now), so its token is another pod's first, not a renewal"
}
until_true 3600 "the architect pod's operator token to be renewed" token_renewed
architect_pod_kept "during the wait"
iat=$(token_iat) || fail "the architect pod $pod's operator token could not be read"
rotated=$(date -u +%FT%TZ)
note "the architect pod started at $(date -u -d "@$pod_started" +%FT%TZ); its token was issued at $(date -u -d "@$iat" +%FT%TZ), a renewal"
send_agent "$tree1" architect "Stage 4b proof: reply to this message with one short sentence, then wait."
# The poll runs in this shell: the session readers need the run's own variables, which a child sh
# would not have.
turn_after_rotation() {
  claim_session_text "$tree1" architect | jq -R -s -e --arg at "$rotated" '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant" and .timestamp > $at)] | any(.message.stopReason == "stop")' >/dev/null
}
until_true 600 "a completed architect turn after the rotation" turn_after_rotation
architect_pod_kept "before the turn after the rotation completed"
after=$(claim_session_text "$tree1" architect | jq -R -s -c --arg at "$rotated" '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant" and .timestamp > $at) | "\(.message.provider)/\(.message.model)"] | unique')
note "turns after the rotation were answered by $after"
jq -e 'all(.[]; test("^anthropic/claude-[a-z0-9.-]+-legion$"))' <<<"$after" >/dev/null || fail "a turn after the rotation left the gateway's aliases: $after"
pass

begin idle-resident
# A finished worker's Sandbox stays running while its issue is open: each role of tree 1 that
# finished its phase is still live in the pod it first registered with, that pod Running, and no
# Sandbox of tree 1 is Suspended. The tree volume is Bound.
for role in planner implementer tester reviewer; do
  resident_kept "$tree1" "$role" || fail "$role on $tree1 left the pod or session it first registered with while tree 1 is open: $(resident_lost "$tree1" "$role")"
  pod=$(claim_sandbox "$tree1" "$role") || fail "$role on $tree1 has no Sandbox"
  running=$(op get pod "$pod" -o jsonpath='{.metadata.uid} {.status.phase}') || fail "the operator could not read $role's pod $pod"
  [ "$running" = "$(jq -r .incarnation "$work/resident-$tree1-$role.json") Running" ] || fail "$role on $tree1 runs in pod $pod as '$running', want its first pod Running"
done
suspended=$(op get sandboxes -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o json | jq -c '[.items[] | select(.spec.operatingMode == "Suspended") | .metadata.name]')
[ "$suspended" = "[]" ] || fail "Sandboxes of $tree1 are Suspended while its issue is open: $suspended"
bound=$(op get pvc -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].status.phase}')
[ "$bound" = Bound ] || fail "the tree volume of $tree1 is '$bound', want Bound"
note "planner, implementer, tester and reviewer of $tree1 each run in the pod they first registered with; no Sandbox of $tree1 is Suspended; its tree volume Bound"
pass

begin kill-pod-resume
# merging launches the merger; its pod must be running before the kill reaches PID 1.
wait_for_worker "$tree1" merger
session=$(claim_view "$tree1" merger | jq -r .session)
end_claim_pod "$tree1" merger kill
pod=$ended_pod
uid=$ended_pod_uid
# The claim has no incarnation between the process's death and its relaunch, so moving off the
# old pod means a new incarnation, never an empty one.
until_true 600 "the merger to relaunch with a new pod" sh -c \
  "inc=\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree1' '.issues[\$i].workers.merger.claim.locator.incarnation // empty'); [ -n \"\$inc\" ] && [ \"\$inc\" != '$uid' ]"
new_uid=$(claim_pod_uid "$tree1" merger)
[ "$(claim_view "$tree1" merger | jq -r .session)" = "$session" ] || fail "the relaunched merger has another session"
resume=$(op get pod "$pod" -o json | jq -r '[.spec.containers[] | select(.name == "worker") | .command[]?] | map(select(startswith("--resume"))) | first // empty')
[ -n "$resume" ] || fail "the relaunched merger pod runs without --resume"
note "killed PID 1 of $pod (uid $uid); relaunched as uid $new_uid, same session $session, $resume"
pass

begin fence
# (a) A pod the controller recreates on its own is never adopted: the daemon suspends it, and the
# claim relaunches with a third uid.
# (b) needs the boot token of the generation (a) replaces, so it is read before the delete.
boot_token() { op get secret "$pod-boot" -o json | jq -er '.data.LEGION_BOOT_TOKEN // empty | @base64d' | grep .; }
old_token=$(boot_token) || fail "the merger's boot Secret $pod-boot has no LEGION_BOOT_TOKEN: the Sandbox runtime under test writes it"
(umask 077 && printf '%s' "$old_token" >"$work/old-boot-token")
end_claim_pod "$tree1" merger delete
uid=$ended_pod_uid
# The deleted pod's Sandbox reports Finished before the daemon moves the claim, so the helper can
# return with the claim still on it: the relaunch is waited for here.
until_true 600 "the merger's claim to move off pod $uid" sh -c \
  "inc=\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree1' '.issues[\$i].workers.merger.claim.locator.incarnation // empty'); [ -n \"\$inc\" ] && [ \"\$inc\" != '$uid' ]"
third=$(claim_pod_uid "$tree1" merger)
[ "$third" != "$new_uid" ] && [ "$third" != "$uid" ] || fail "the merger's third incarnation $third repeats an earlier uid"
grep -q '"msg":"supervise: dropped a stale event"' "$daemon_log" || note "no stale event was dropped in this run (the recreated pod's events arrived after the claim moved)"
# (b) Once the relaunch's token is in the Secret, the replaced generation's hello is refused, and
# refused as stale: the daemon logs why, as Stage 2 asserts, so a shim that failed for any other
# reason (a timeout, a refused connection, a bad flag) does not pass.
boot_rotated() {
  local now
  now=$(boot_token) || return 1
  [ "$now" != "$old_token" ]
}
until_true 600 "the merger's boot token to rotate" boot_rotated
refused_msg="worker-stream: rejected hello (stale worker generation)"
refused_before=$(log_lines "$refused_msg" | wc -l)
if out=$(timeout 60 "$work/legion" worker-shim --connect "tcp://$host:$port_worker_stream" --boot-token-file "$work/old-boot-token" -- true 2>&1); then
  fail "the worker stream accepted a hello with the previous generation's boot token"
fi
stale_refused() { [ "$(log_lines "$refused_msg" | wc -l)" -gt "$refused_before" ]; }
until_true 30 "the daemon to log the replaced generation's hello as stale" stale_refused
note "the replaced generation's hello was refused, and the daemon logged \"$refused_msg\"; the shim said: $(tail -1 <<<"$out" | cut -c1-200)"
pass

begin daemon-relaunch-count
kills=$(grep -c . "$evidence/driver-actions.txt")
relaunches=$(log_lines "supervise: launched" | jq -s --arg c "$(claim_token "$tree1" merger)" '[.[] | select(.claim == $c and .resumed == true)] | length')
note "the driver ended $kills pods; the daemon relaunched the merger $relaunches times"
[ "$relaunches" -ge "$kills" ] || fail "the daemon relaunched the merger $relaunches times for $kills kills"
pass

begin restart-mid-tree
before=$(claim_view "$tree1" merger | jq -c '{session, incarnation: .locator.incarnation}')
stop_pid "$daemon_pid"
daemon_pid=
start_daemon
until_true 300 "the merger to be re-adopted" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg i '$tree1' '.issues[\$i].workers.merger.claim.state | IN(\"ready\", \"working\", \"idle\")' >/dev/null"
after=$(claim_view "$tree1" merger | jq -c '{session, incarnation: .locator.incarnation}')
[ "$before" = "$after" ] || fail "the restart relaunched the merger: $before → $after"
note "the daemon restarted and re-adopted the merger as it was: $after"
pass

# launch_failure_limit is the daemon's default (3), which the run's legion.yaml leaves unset; it
# bounds a claim's launch failures and its deaths with work outstanding alike.
launch_failure_limit=3
begin controller
# `legion controller start` on the devbox registers with the Sandbox daemon; tree 3 supplies the
# held phase whose notice reaches it; `legion status … backlog` from the operator shell takes
# tree 3 out, and Dispatch shows it.
if [ -n "$skip_controller" ]; then
take_out "$tree3"
skipped "STAGE4B_SKIP_CONTROLLER: a development run; tree 3 was only taken out"
else
(cd "$root" && bun install --frozen-lockfile >/dev/null)
make_omp_home "$omp_home"
bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --home "$omp_home" --dest "$work/plugin" >/dev/null
bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$omp_home" --dest "$evidence/model-gateway" --cache-dir "$work/model-gateway-cache" >/dev/null ||
  blocked "the controller's model route could not be installed (lib/install-model-gateway.sh)"
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
cat >"$work/controller.yaml" <<EOF
project: $project
daemon_url: http://$host:$port_daemon
operator_token_file: $work/operator-token
envoy_url: $envoy_url
envoy_token_file: $work/envoy-token
nats_urls: [$nats_url]
dispatch_url: $dispatch_base
dispatch_token_file: $work/dispatch-token
instructions: $work/instructions.md
omp_invocation: mise x $pin -- omp
state_dir: $work/controller-state
EOF
tmux -L "legion-e2e4b-$$" new-session -d -s controller -x 200 -y 50 \
  "cd '$work' && HOME='$omp_home' OMP_PROFILE='$profile' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/logs/controller.stderr'; sleep 3600"
until_true 300 "controllerLocator in the state" sh -c "'$work/legion' state --json --config '$work/legion.yaml' | jq -e '.controllerLocator.sessionId != null' >/dev/null"
controller_session=$(daemon_state | jq -r .controllerLocator.sessionId)
note "controllerLocator $(daemon_state | jq -c .controllerLocator)"
# The controller's first turn starts itself (LEGION-392): nothing is ever typed into its pane, so
# its session's first user message is the start message `legion controller start` launches Oh My
# Pi with (controllerStartMessage, cmd/legion/controller.go), and the model answers it.
controller_started_itself() {
  local file
  for file in "$profile_agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    grep -m1 '"role":"user"' "$file" | grep -qF 'Legion controller start: follow skill://legion-controller' &&
      grep -q '"role":"assistant"' "$file" && return 0
  done
  return 1
}
until_true 300 "the controller's first turn to start itself, with nothing typed" controller_started_itself
note "the controller's first turn began from its start message, with nothing typed into its pane"
wait_for_worker "$tree3" architect
drive_spec "$tree3"
wait_for_worker "$tree3" planner
# The daemon holds a phase when one of its claim's budgets reaches launch_failure_limit (3)
# (supervise/budgets.go): launch failures, which restart at the agent's ready, or deaths after a
# ready while the claim has its task outstanding. Tree 3's planner finishes its task turn when the
# tree is admitted, so an end of it or of a relaunch leaves no task outstanding, and a relaunch that
# reaches ready restarts the launch count. So the hold is driven on tree 3's implementer, whose task
# is fresh: the planner is told to plan, and each implementer launch is killed once its agent is
# ready or in a turn with its task outstanding. Every such death is charged; the check below accepts
# either budget.
send_agent "$tree3" planner "Stage 4b proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$tree3" implementing 900
wait_for_worker "$tree3" implementer
killed=" "
kills=0
# held_ready_with_work: tree 3's implementer claim names a pod none of the ends took, its agent is
# ready or in a turn, and its task is outstanding, as `legion claims` shows it. It runs in this
# shell: the ended list is a here-string, which the sh of an `sh -c` (dash) refuses as a syntax
# error.
held_ready_with_work() {
  local claim inc
  claim=$(claims_cli list --json | jq -ce --arg t "$(claim_token "$tree3" implementer)" '.claims[] | select(.token == $t)') || return 1
  inc=$(jq -r '.locator.incarnation // empty' <<<"$claim")
  [ -n "$inc" ] && ! grep -qF " $inc " <<<"$killed" &&
    jq -e '(.state | IN("ready", "working", "idle")) and .pending != null' <<<"$claim" >/dev/null
}
until issue_phase "$tree3" held >/dev/null 2>&1; do
  [ "$kills" -lt 8 ] || fail "$tree3 was not held after $kills ended implementer launches"
  until_true 600 "$tree3's implementer ready with its task outstanding" held_ready_with_work
  state=$(claim_view "$tree3" implementer | jq -r '.state // "none"')
  end_claim_pod "$tree3" implementer kill
  uid=$ended_pod_uid
  killed="$killed$uid "
  kills=$((kills + 1))
  note "ended implementer launch $kills of $tree3 (uid $uid), its claim $state with its task outstanding"
  until_true 600 "$tree3 to be held or its implementer relaunched" claim_moved_or_held "$tree3" implementer "$uid"
done
note "$tree3 is held after $kills ended implementer launches"
# The hold is one of the implementer claim's own supervise budgets, launches or deaths with work
# outstanding, and no other path that also holds an issue (a prompt budget, the architect's
# escalation).
held_claim=$(claim_token "$tree3" implementer)
failed=$(log_lines "supervise: claim failed" | jq -c --arg c "$held_claim" 'select(.claim == $c)' | tail -1)
why=$(jq -r '.why // empty' <<<"${failed:-null}")
# The daemon's own record judges the reason: its claim-failed line carries the claim's budget
# counters, and the reason must name the one at launch_failure_limit, with the other below it, as
# died (supervise/machine.go) charges a death first.
case $why in
  "launch failures ran out") counter=launchFailures other=deaths ;;
  "deaths with work outstanding ran out") counter=deaths other=launchFailures ;;
  *) fail "$tree3's implementer claim $held_claim failed with '${why:-no logged failure}', not one of its launch or death budgets" ;;
esac
counts=$(jq -r '"launchFailures \(.launchFailures), deaths \(.deaths)"' <<<"$failed")
jq -e --arg c "$counter" --arg o "$other" --argjson n "$launch_failure_limit" '.[$c] >= $n and .[$o] < $n' <<<"$failed" >/dev/null ||
  fail "$tree3's implementer claim $held_claim failed with '$why', but the daemon's counters ($counts, bound $launch_failure_limit) do not name that budget"
# The first end takes the implementer's first launch; every later one is a relaunch, whose ends set
# the hold they lead to (relaunch_ends).
read -r -a ended <<<"$killed"
ends=$(relaunch_ends "$held_claim" "${ended[@]:1}") || fail "the ends of $tree3's implementer relaunches could not be read from the daemon log"
while IFS= read -r line; do note "$line"; done < <(sed '$d' <<<"$ends")
last=$(tail -1 <<<"$ends")
case $last in
  "expect: "*) expected=${last#expect: } ;;
  *) fail "$tree3's implementer relaunches: ${last#refused: }" ;;
esac
[ "$why" = "$expected" ] || fail "$tree3's implementer claim $held_claim failed with '$why', but its last relaunch's death leads to '$expected'"
note "the daemon failed $held_claim because $why ($counts, bound $launch_failure_limit)"
# The held notice reaches the controller: its session, on this machine, holds the Envoy delivery.
controller_notice() {
  local file
  for file in "$profile_agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    grep -F '"customType":"envoy-message"' "$file" | grep -qF "$(notice_needle held "$tree3")" && return 0
  done
  return 1
}
until_true 300 "the held notice for $tree3 to reach the controller session $controller_session" controller_notice
note "the controller session $controller_session received the held notice for $tree3"
# That session is under the run's own home, and the operator's profile root holds none of the
# controller's profile (make_omp_home, lib/omp-home.sh).
[ ! -e "$HOME/.omp/profiles/$profile" ] || fail "the run wrote the operator's profile root: $HOME/.omp/profiles/$profile exists"
note "the controller's session is under $profile_agent/sessions; $HOME/.omp/profiles/$profile does not exist"
interests_sample "$check"
before_status=$(dispatch_get "issues/$tree3" | jq -r .status)
out=$("$work/legion" status "$tree3" backlog --operator-token-file "$work/operator-token" --config "$work/legion.yaml" 2>&1) || fail "legion status $tree3 backlog from the operator shell: $out"
until_true 120 "Dispatch to show $tree3 in backlog" dispatch_status_is "$tree3" backlog
note "legion status $tree3 backlog from the operator shell, with the operator bearer: Dispatch moved $tree3 from $before_status to $(dispatch_get "issues/$tree3" | jq -r .status)"
until_true 600 "$tree3's pods to be gone" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get pods -l 'legion.dev/project=$run_label,legion.dev/tree=$tree3' -o name) && [ -z \"\$out\" ]"
# The controller's walk wakes (LEGION-392). controller_received NEEDLE: an Envoy delivery in the
# controller's session holds NEEDLE.
controller_received() {
  local file
  for file in "$profile_agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    grep -F '"customType":"envoy-message"' "$file" | grep -qF "$1" && return 0
  done
  return 1
}
# (1) The controller was launched with the daemon's own design gate policy (gates.design: off): the
# Oh My Pi running in the controller's directory carries the line in its --append-system-prompt.
# shellcheck disable=SC2016 # the backticks are the line's own text, never a substitution
policy_line='Design gate policy: `gates.design: off`.'
controller_launched_with_policy() {
  local proc
  for proc in /proc/[0-9]*; do
    [ "$(readlink "$proc/cwd" 2>/dev/null)" = "$work/controller-state/controller" ] || continue
    tr '\0' '\n' <"$proc/cmdline" 2>/dev/null | grep -qF "$policy_line" && return 0
  done
  return 1
}
controller_launched_with_policy || fail "no process in $work/controller-state/controller was launched with '$policy_line'"
note "the controller's Oh My Pi was launched with '$policy_line'"
# (2) With tree 3 out, a slot stands free (tree 1 holds the other), so an unlabelled leaf set to
# todo wakes the controller with `todo on <KEY>`.
free=$(daemon_state | jq -r '.admission.cap - (.admission.active | length) - (.admission.waiting | length)')
[ "$free" -gt 0 ] || fail "no admission slot stands free after $tree3 left ($(daemon_state | jq -c .admission)), so no walk wake can be sent"
walk_candidate=$(dispatch_human POST issues "$(jq -cn --arg project "$project" --arg title "Stage 4b proof walk candidate ($work)" '{project:$project,title:$title,force:true}')" | jq -er .key) ||
  fail "create the unlabelled walk candidate in $project"
set_status "$walk_candidate" todo
until_true 300 "'todo on $walk_candidate' to reach the controller session $controller_session" controller_received "$(notice_needle todo "$walk_candidate")"
note "the controller session received 'todo on $walk_candidate' with $free slot(s) free"
# (3) The daemon's periodic tick (controller_wake_interval_seconds: 60) reaches it too.
until_true 300 "'tick on $project' to reach the controller session $controller_session" controller_received "$(notice_needle tick "$project")"
note "the controller session received 'tick on $project'"
# (4) The walk takes nothing: this proof's scope line says the controller hands Legion no issue
# itself. The controller has a minute after the tick to act on either wake, and the candidate must
# still be unlabelled and unrecorded when it ends.
sleep 60
dispatch_get "issues/$walk_candidate" | jq -e '(.labels | map(ascii_downcase) | index("legion")) == null' >/dev/null ||
  fail "the controller labelled $walk_candidate legion, though the proof's scope says it hands Legion no issue"
daemon_state | jq -e --arg key "$walk_candidate" '.issues[$key] == null' >/dev/null ||
  fail "the daemon recorded $walk_candidate, though the controller was to take nothing"
set_status "$walk_candidate" "done"
note "the controller took nothing: $walk_candidate stayed unlabelled and unrecorded, and is now done"
# (5) A root architect claimed its root issue at its first catch-up: tree 3's root is claimed by a
# session that is not the controller's, and its events say so.
root_claim=$(dispatch_get "issues/$tree3" | jq -c '.claim')
jq -e --arg c "$controller_session" '.actor.kind == "session" and .actor.id != $c' <<<"$root_claim" >/dev/null ||
  fail "$tree3's root issue is not claimed by its architect's session: claim $root_claim"
dispatch_events "$tree3" | jq -e --argjson claim "$root_claim" 'any(.[]; .type == "issue.claimed" and .actor.id == $claim.actor.id)' >/dev/null ||
  fail "$tree3's events hold no issue.claimed by $(jq -r .actor.id <<<"$root_claim")"
note "$tree3's root issue is claimed by its architect session $(jq -r .actor.id <<<"$root_claim")"
pass
fi # the controller checks

begin daily-report
# The controller's daily report runs by rule, not on a human's word, so the proof's instructions
# exempt it from their wait for a targeted message and fit its day to this run: a report issue of
# this run's own (so the skill's "no report message from today" guard starts fresh), posted on the
# first turn a tick starts. With every slot taken, the tick is the quiet day's only wake.
if [ -n "$skip_controller" ]; then
  skipped "STAGE4B_SKIP_CONTROLLER: no controller ran"
else
# report_key STATUS prints the key of this run's report issue in STATUS, or nothing.
report_key() {
  dispatch_get "issues?project=$project&status=$1" | jq -r --arg t "$report_title" '[.[] | select(.title == $t)][0].key // empty'
}
report_in_icebox() { [ -n "$(report_key icebox)" ]; }
until_true 600 "the controller's report issue '$report_title' in $project, parked in icebox" report_in_icebox
report=$(report_key icebox)
dispatch_get "issues/$report" | jq -e '(.labels | map(ascii_downcase) | index("legion")) == null' >/dev/null ||
  fail "$report, the report issue, carries the legion label"
# The message is the controller's, names tree 1, which runs throughout, and the free slots, and
# fits the skill's 2,000 characters.
report_posted() {
  dispatch_events "$report" | jq -e --arg s "$controller_session" --arg tree "$tree1" \
    'any(.[]; .type == "message.created" and .actor.id == $s and (.payload.body | test("(^|[^0-9A-Za-z-])" + $tree + "($|[^0-9])")) and (.payload.body | test("slot"; "i")) and (.payload.body | length) <= 2000)' >/dev/null
}
until_true 300 "the controller's report message on $report" report_posted
# It was posted on a turn a tick started while the controller was idle, the quiet day's wake. A
# tick delivered while a turn still runs is steered into that turn (pi-envoy delivers every Envoy
# message as a steer), and Oh My Pi keeps an errored or aborted attempt in the transcript and
# retries in the same turn, so neither a turn's first terminal-looking message nor line order
# marks idleness. The anchor is the tick itself: some tick delivery before the first report call
# whose last preceding message entry is an assistant message with stopReason `stop`, a turn that
# had genuinely finished. A start turn that ends in an unretried error fails the check.
report_after_tick() {
  local file
  for file in "$profile_agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    jq -R -s -e --arg tick "summary: tick on $project" '
      [split("\n") | to_entries[] | {i: .key, raw: .value, m: (.value | fromjson? // null)}] as $lines
      | [$lines[] | select(.m.type? == "message" and .m.message.role? != "custom")] as $msgs
      | ([$lines[] | select(.raw | test("xd://dispatch_message|\"name\":\"dispatch_message\"")) | .i] | first) as $call
      | $call != null and any($lines[]; .i < $call and (.raw | contains($tick))
          and (.i as $t | ([$msgs[] | select(.i < $t)] | last) as $before
            | $before != null and $before.m.message.role == "assistant" and $before.m.message.stopReason == "stop"))
    ' "$file" >/dev/null && return 0
  done
  return 1
}
report_after_tick || fail "the controller's first report message was not posted on a turn a tick started while it was idle"
note "the controller parked $report ('$report_title') in icebox and posted its daily report there on a tick's turn"
pass
fi # the daily report

begin deaths-with-work
# A worker whose process dies after its agent is ready, while it has its task outstanding, gets the
# task back, and one that keeps dying before it completes a turn is held (supervise/budgets.go,
# Deaths). Tree 4 is admitted once tree 3 has left: its planner is killed once mid-turn and finishes
# the phase on the task sent again, and its implementer is killed after each ready, its task
# outstanding, until the daemon fails the claim.
tree4=$(new_issue "Stage 4b proof tree 4: deaths with work outstanding ($work)")
set_status "$tree4" todo
drive_spec "$tree4"
wait_for_worker "$tree4" planner
# claim_json ISSUE ROLE is the claim as `legion claims` shows it, its budgets and pending task included.
claim_json() { claims_cli list --json | jq -ce --arg t "$(claim_token "$1" "$2")" '.claims[] | select(.token == $t)'; }
# in_turn ISSUE ROLE: the claim's agent is running the turn of its task.
in_turn() { claim_json "$1" "$2" | jq -e '.state == "working" and .pending != null' >/dev/null; }
# (a) One kill mid-turn: the relaunch is sent its task again, told the turn was interrupted.
until_true 600 "$tree4's planner to be in the turn of its task" in_turn "$tree4" planner
end_claim_pod "$tree4" planner kill
planner_killed=$ended_pod_uid
note "killed $tree4's planner mid-turn (uid $planner_killed)"
interrupted_needle="Your previous turn on this task was interrupted when your process died."
planner_resent() { claim_session_text "$tree4" planner | grep -qF "$interrupted_needle"; }
until_true 600 "$tree4's planner to be sent its task again, told its turn was interrupted" planner_resent
send_agent "$tree4" planner "Stage 4b proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$tree4" implementing 900
note "$tree4's planner was sent its task again after the kill, told the turn was interrupted, and finished planning"
# (b) Kills after each ready, the task outstanding, until the claim fails.
wait_for_worker "$tree4" implementer
implementer4=$(claim_token "$tree4" implementer)
# ready_with_work: the implementer's claim names a pod no kill took, its agent is ready or in a
# turn, and its task is outstanding.
ready_with_work() {
  local claim inc
  claim=$(claim_json "$tree4" implementer) || return 1
  inc=$(jq -r '.locator.incarnation // empty' <<<"$claim")
  [ -n "$inc" ] && ! grep -qF " $inc " <<<"$work_killed" &&
    jq -e '(.state | IN("ready", "working", "idle")) and .pending != null' <<<"$claim" >/dev/null
}
work_killed=" "
work_kills=0
until issue_phase "$tree4" held >/dev/null 2>&1; do
  [ "$work_kills" -lt 5 ] || fail "$tree4 was not held after $work_kills implementer deaths with its task outstanding"
  until_true 600 "$tree4's implementer ready with its task outstanding" ready_with_work
  end_claim_pod "$tree4" implementer kill
  work_killed="$work_killed$ended_pod_uid "
  work_kills=$((work_kills + 1))
  note "killed $tree4's implementer with its task outstanding, $work_kills (uid $ended_pod_uid)"
  until_true 600 "$tree4 to be held or its implementer relaunched" claim_moved_or_held "$tree4" implementer "$ended_pod_uid"
done
[ "$work_kills" = "$launch_failure_limit" ] || fail "$tree4 was held after $work_kills implementer deaths, want launch_failure_limit ($launch_failure_limit)"
claim=$(claim_json "$tree4" implementer) || fail "legion claims shows no claim $implementer4"
jq -e --argjson n "$launch_failure_limit" '.state == "failed" and .budgets.deaths == $n' <<<"$claim" >/dev/null ||
  fail "$implementer4 reads $(jq -c '{state, budgets}' <<<"$claim"), want failed with budgets.deaths $launch_failure_limit"
why=$(log_lines "supervise: claim failed" | jq -r --arg c "$implementer4" 'select(.claim == $c) | .why' | tail -1)
[ "$why" = "deaths with work outstanding ran out" ] ||
  fail "$implementer4 failed with '${why:-no logged failure}', not 'deaths with work outstanding ran out'"
failed_at=$(log_lines "supervise: claim failed" | jq -r --arg c "$implementer4" 'select(.claim == $c) | .time' | tail -1)
sleep 30
relaunched=$(log_lines "supervise: launched" | jq -s --arg c "$implementer4" --arg at "$failed_at" '[.[] | select(.claim == $c and .time > $at)] | length')
[ "$relaunched" = 0 ] || fail "the daemon launched $implementer4 $relaunched times after failing it"
note "$tree4 is held after $work_kills implementer deaths with its task outstanding: $implementer4 failed because $why, budgets $(jq -c .budgets <<<"$claim"), and nothing relaunched it"
take_out "$tree4"
pass

begin "done"
wait_for_worker "$tree1" merger
send_agent "$tree1" merger "Stage 4b proof READY operation: verify pull request #$pr_number is ready to merge and call the legion tool's handoff_complete with ready true."
wait_for_phase "$tree1" awaiting_merge 900
# The hold is an open descriptor (hold_smoke_main): start no background child before
# release_smoke_main below, or it inherits the descriptor and holds the smoke main past this run's
# window. `9>&- 7>&-` does not close it: its number is allocated at runtime, not fixed.
hold_smoke_main
gh -R "$repo" pr merge "$pr_number" --squash --delete-branch
wait_for_phase "$tree1" production_check 600
if ! production_check_reported "$tree1" >/dev/null 2>&1; then
  wait_for_worker "$tree1" implementer
  send_agent "$tree1" implementer "Stage 4b proof production check: verify the merged smoke change through its repository surface, record the production-check handoff and required PR/Dispatch record, then complete the phase."
fi
until_true 900 "the implementer's production-check completion" production_check_reported "$tree1"
if issue_phase "$tree1" production_check >/dev/null 2>&1; then
  send_agent "$tree1" architect "Stage 4b proof sign-off: the implementer's production check for $tree1 is recorded; use the Go-daemon sign-off operation for $tree1 now."
fi
wait_for_phase "$tree1" "done" 900
until_true 120 "the daemon's done status on the Dispatch board" dispatch_status_is "$tree1" "done"
clean_smoke_main
release_smoke_main
note "$repo#$pr_number merged by the proof human; the production check and the sign-off closed $tree1"
# The close is what stops the roles the tree kept live: each claim of tree 1 is suspended, its
# session kept for a re-admission to resume.
tree1_roles_suspended() {
  local role
  for role in architect planner implementer tester reviewer merger; do issue_worker_state "$tree1" "$role" suspended || return 1; done
}
until_true 300 "the close to suspend every role of $tree1" tree1_roles_suspended
note "the close suspended the architect, planner, implementer, tester, reviewer and merger of $tree1"
# The daemon's done released the claim tree 1's architect took on its root issue (LEGION-392).
dispatch_events "$tree1" | jq -e 'any(.[]; .type == "issue.claimed" and .actor.kind == "session")' >/dev/null ||
  fail "$tree1's events hold no issue.claimed by its architect's session"
root_claim_released() { dispatch_get "issues/$1" | jq -e '.claim == null' >/dev/null; }
until_true 120 "$tree1's root claim to be released by its done" root_claim_released "$tree1"
note "$tree1's architect claimed its root issue, and the done released the claim"
pass

begin node-release
# Tree 1 lingers (linger_hours 0.3): its Sandboxes stay Suspended, its volume stays Bound, and no pod
# of the run is left on its node. The pool consolidates a node only once it is empty, so the node is
# gone after consolidateAfter unless another project's pod is now on it: a tree pod refuses only a
# node holding another tree's pod (the runtime's affinity, internal/runtime/sandbox/manifest.go), and
# the image probe carries no tree label, so a production daemon running beside the run can place a
# pod on the node tree 1 emptied. That pod keeps the node from the moment it is bound, Pending through
# its init containers included. Either case passes, and the note says which one it saw.
node=$(jq -r 'select(.object.kind == "Pod") | .object | select(.metadata.labels["legion.dev/tree"] == "'"$tree1"'") | .spec.nodeName // empty' "$evidence/pod-watch.json" | tail -1)
[ -n "$node" ] || fail "the pod watch saw no pod of tree 1 on a node, so there is no node whose release to wait for"
node_release=
node_released() {
  local out pods others
  out=$(timeout 120 kubectl --context "$operator" get node "$1" -o name --ignore-not-found) || return 1
  if [ -z "$out" ]; then
    node_release="node $1 is gone"
    return 0
  fi
  pods=$(timeout 120 kubectl --context "$operator" -n "$namespace" get pods --field-selector "spec.nodeName=$1" -o json) || return 1
  jq -e --arg run "$run_label" '[.items[] | select(.metadata.labels["legion.dev/project"] == $run and .status.phase != "Succeeded" and .status.phase != "Failed")] | length == 0' <<<"$pods" >/dev/null || return 1
  others=$(jq -r --arg run "$run_label" '[.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed") | (.metadata.labels["legion.dev/project"] // empty) as $p | select($p != $run) | "\($p)/\(.metadata.name) \(.status.phase)"] | join(", ")' <<<"$pods")
  [ -n "$others" ] || return 1
  node_release="node $1 stays, carrying no pod of the run while another project's pod is on it ($others)"
}
# A timed-out wait says what the node held: a pod of the run that never left, another project's pod,
# or nothing the pool ever deleted.
report_node_release() {
  local out
  if ! out=$(timeout 120 kubectl --context "$operator" get node "$node" -o name --ignore-not-found 2>&1); then
    note "node $node could not be read: $out"
    return
  fi
  if [ -z "$out" ]; then
    note "node $node no longer exists"
    return
  fi
  note "node $node still exists; its pods (namespace/name, project label, phase):"
  timeout 120 kubectl --context "$operator" get pods -A --field-selector "spec.nodeName=$node" -o json |
    jq -r '.items[] | "     \(.metadata.namespace)/\(.metadata.name) \(.metadata.labels["legion.dev/project"] // "-") \(.status.phase)"'
}
timeout_hook=report_node_release
until_true 1500 "tree 1's node $node to be released, or to carry no pod of the run while another project's pod is on it" node_released "$node"
timeout_hook=
modes=$(op get sandboxes -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].spec.operatingMode}')
bound=$(op get pvc -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].status.phase}')
if [ -z "$modes" ] || grep -qv Suspended <<<"$(tr ' ' '\n' <<<"$modes")"; then fail "tree 1's Sandboxes are '$modes' after its node was released ($node_release), want all Suspended"; fi
[ "$bound" = Bound ] || fail "tree 1's volume is '$bound' after its node was released ($node_release), want Bound"
note "at $(date -u +%FT%TZ) $node_release; tree 1's Sandboxes Suspended ($modes), its volume Bound"
pass

begin close
until_true 1500 "tree 1 to close at linger expiry" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get sandboxes,pvc -l 'legion.dev/project=$run_label,legion.dev/tree=$tree1' -o name) && [ -z \"\$out\" ]"
note "tree 1's Sandboxes and tree volume are deleted"
pass

begin re-admission
set_status "$tree1" todo
lost_msg="supervise: the tree volume was lost with the session; relaunching a fresh session"
lost_seen() { [ "$(log_lines "$lost_msg" | wc -l)" -ge 1 ]; }
until_true 900 "the re-admitted tree 1 to report its tree volume lost and relaunch a fresh architect" lost_seen
wait_for_worker "$tree1" architect
pod=$(tree_pod "$tree1")
recovered=$(pod_exec "$pod" cat "/legion/workspaces/$repo/${tree1,,}/.legion/workspace-recovered.json")
jq -e --arg b "legion/$tree1" 'tostring | contains($b)' <<<"$recovered" >/dev/null || fail "the recovery marker does not name legion/$tree1: $recovered"
lost=$(log_lines "$lost_msg" | wc -l)
[ "$lost" = 1 ] || fail "the daemon reported the tree volume lost $lost times, want exactly once"
note "the tree volume reported lost once, then a fresh session whose workspace holds .legion/workspace-recovered.json naming legion/$tree1"
pass

begin operator-close
# The operator's `legion claims close` on the Sandbox runtime. A workflow issue's tree is the
# workflow's to close: the close of re-admitted tree 1's live root is refused 409, and its claims,
# Sandboxes and pods are untouched. A tree no workflow issue backs, which the operator spawns here,
# closes with its worker live: the root and the worker are retired, and the tree's Sandboxes, pods
# and volume are gone.
tree_objects() {
  op get sandboxes,pods,pvc -l "legion.dev/project=$run_label,legion.dev/tree=$1" -o json |
    jq -c '[.items[] | {kind, name: .metadata.name, uid: .metadata.uid}] | sort_by(.kind, .name)'
}
root1=$(claim_token "$tree1" architect)
states1() { claims_cli list --json | jq -c --arg t "$tree1" '[.claims[] | select(.tree == $t) | {token, state, generation}] | sort_by(.token)'; }
objects_before=$(tree_objects "$tree1")
claims_before=$(states1)
if refusal=$(claims_cli close --claim "$root1" 2>&1 >/dev/null); then
  fail "the operator's close of workflow tree $tree1 through $root1 was accepted"
fi
case "$refusal" in
  *"409"*) ;;
  *) fail "the operator's close of workflow tree $tree1 was refused with '$refusal', not 409" ;;
esac
[ "$(tree_objects "$tree1")" = "$objects_before" ] || fail "the refused close changed tree 1's objects: $objects_before, then $(tree_objects "$tree1")"
[ "$(states1)" = "$claims_before" ] || fail "the refused close changed tree 1's claims: $claims_before, then $(states1)"
note "the operator's close of workflow tree $tree1 was refused ($refusal); its claims $claims_before and objects $objects_before are unchanged"
take_out "$tree1"
printf '%s\n' "You are a Stage 4b operator-close fixture, the root of a tree no workflow issue backs. Do nothing and wait." >"$work/op-architect.md"
printf '%s\n' "You are a Stage 4b operator-close fixture, a worker of that tree. Do nothing and wait." >"$work/op-worker.md"
op_root=$(claims_cli spawn --json --tree "$optree" --issue "$optree" --role architect --prompt-file "$work/op-architect.md" | jq -er .token) ||
  fail "the operator could not spawn the root of $optree"
op_worker=$(claims_cli spawn --json --tree "$optree" --issue "$opchild" --role implementer --prompt-file "$work/op-worker.md" | jq -er .token) ||
  fail "the operator could not spawn a worker of $optree"
claim_live() { claims_cli list --json | jq -e --arg t "$1" '.claims[] | select(.token == $t) | .state | IN("ready", "idle", "working")' >/dev/null; }
until_true 900 "$optree's root $op_root to be live" claim_live "$op_root"
until_true 900 "$optree's worker $op_worker to be live" claim_live "$op_worker"
objects=$(tree_objects "$optree")
jq -e 'map(select(.kind == "Pod")) | length == 2' <<<"$objects" >/dev/null || fail "$optree does not have its two pods before the close: $objects"
for uid in $(jq -r '.[] | select(.kind == "Pod") | .uid' <<<"$objects"); do driver_action close "$uid"; done
note "before the close, $optree has: $objects"
closed=$(claims_cli close --json --claim "$op_root") || fail "the operator's close of $optree through $op_root was refused: $closed"
jq -e '.state == "retired"' <<<"$closed" >/dev/null || fail "the close left $optree's root $(jq -c '{state}' <<<"$closed")"
retired() { claims_cli list --json | jq -e --arg t "$1" '.claims[] | select(.token == $t) | .state == "retired"' >/dev/null; }
retired "$op_worker" || fail "the close of $optree left its worker $op_worker $(claims_cli list --json | jq -c --arg t "$op_worker" '.claims[] | select(.token == $t) | {state}')"
until_true 600 "$optree's Sandboxes, pods and volume to be gone" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get sandboxes,pods,pvc -l 'legion.dev/project=$run_label,legion.dev/tree=$optree' -o name) && [ -z \"\$out\" ]"
note "the operator's close of $optree, with its worker $op_worker live, retired the root and the worker; afterwards $optree has: $(tree_objects "$optree")"
pass

begin pod-shape
missing=$(stream_missing "$evidence/pod-watch.json")
[ -z "$missing" ] || fail "the pod watch never recorded pods the run knows from other sources: $(tr '\n' ' ' <<<"$missing")"
bad=$(pod_shape_verdict "$evidence/pod-watch.json")
[ -z "$bad" ] || fail "Sandbox pods depart from the pod shape: $(tr '\n' ' ' <<<"$bad")"
judged=$(jq -r 'select(.object.kind == "Pod") | .object | select(.metadata.labels["legion.dev/probe"] == null and .metadata.labels["legion.dev/e2e-control"] == null)
  | select(any(.status.containerStatuses[]?; .name == "worker" and .ready)) | .metadata.uid' "$evidence/pod-watch.json" | sort -u | wc -l)
# The Secret-value check: the helper judges the recorded pods against every value it held, on TERM.
stop_pid "$leaks_pid"
leaks_pid=
[ -s "$work/secret-leaks.json" ] || fail "lib/secret-leaks.ts wrote no verdict ($evidence/logs/secret-leaks.err)"
jq -e '.leaks == [] and .unseen == [] and .unreadable == 0' "$work/secret-leaks.json" >/dev/null ||
  fail "Sandbox pods carry a value of their Sandbox's Secret, name a Secret the check never saw, or the pod watch holds unreadable lines: $(jq -c '{leaks, unseen, unreadable}' "$work/secret-leaks.json")"
note "$judged Sandbox pods judged from the pod watch's record, deleted ones included; every pod another source names is in it"
note "no pod's command, args or environment carries a value of its Sandbox's Secret: $(jq -r '"\(.pods) pods, \(.secrets) Secrets, \(.values) values held in memory, none printed"' "$work/secret-leaks.json")"
# Negative controls: a recorded pod with another runtime class, and a pod the watch never recorded.
jq -c 'select(.object.kind == "Pod" and any(.object.status.containerStatuses[]?; .name == "worker" and .ready))' "$evidence/pod-watch.json" | tail -1 |
  jq -c '.object.spec.runtimeClassName = "runc"' >"$work/wrong-shape.json"
cat "$evidence/pod-watch.json" "$work/wrong-shape.json" >"$evidence/controls/pod-watch-wrong-shape.json"
expect_failure pod-shape-wrong-runtime test -z "$(pod_shape_verdict "$evidence/controls/pod-watch-wrong-shape.json")"
cp "$evidence/driver-actions.txt" "$work/driver-actions.saved"
printf 'kill 00000000-e2e4-4b00-0000-000000000000 control\n' >>"$evidence/driver-actions.txt"
expect_failure pod-shape-unrecorded-pod test -z "$(stream_missing "$evidence/pod-watch.json")"
mv "$work/driver-actions.saved" "$evidence/driver-actions.txt"
unames=$(cat "$evidence"/pods/*.txt 2>/dev/null | sed -n 's/^uname=//p' | sort -u | tr '\n' ' ')
note "$(wc -l <"$evidence/pods-checked.txt") of them also read live by the shape watcher (gVisor uname -r: $unames)"
grep -h '"init"' "$evidence"/pods/*.txt | jq -s -c 'map({role, init: [.init[] | select(.name == "workspace-fetch") | {started, finished}]})' >"$evidence/workspace-fetch.json"
note "workspace-fetch per pod (started, finished) and node ephemeral-storage use: $evidence/workspace-fetch.json, $evidence/pods/"
pass

begin pod-watch-verdict
stop_tree "$shape_pid"
shape_pid=
missing=$(stream_missing "$evidence/pod-watch.json")
[ -z "$missing" ] || fail "the pod watch never recorded pods the run knows from other sources: $(tr '\n' ' ' <<<"$missing")"
if ! pod_watch_verdict "$evidence/pod-watch.json" "$evidence/driver-actions.txt" "$daemon_log"; then
  fail "the pod watch saw terminations the run cannot account for: $(tr '\n' ';' <"$work/pod-watch-verdict.txt")"
fi
retired=$(never_scheduled_deaths "$evidence/pod-watch.json" "$daemon_log")
[ -z "$retired" ] || note "accounted for: the daemon retired pods the scheduler never placed at their boot deadline and relaunched their claims: $(tr '\n' ' ' <<<"$retired")"
pressure=$(jq -R -c 'fromjson? | select(.pressure? and (.pressure | index("MemoryPressure")))' "$evidence/node-memory.txt")
[ -z "$pressure" ] || fail "a node of the run reported MemoryPressure: $(head -3 <<<"$pressure" | tr '\n' ' ')"
# grep -c exits 1 on a count of 0, which under errtrace would fire the ERR trap inside the
# substitution and print a check's FAIL in a passing run; `|| true` keeps the count.
note "no node of the run reported MemoryPressure ($(grep -c '"pressure"' "$evidence/node-memory.txt" || true) samples)"
jq -c 'select(.object.kind == "Pod") | .object' "$evidence/pod-watch.json" | tail -1 |
  jq -c '{kind: "Pod", object: (.status.containerStatuses[0].state = {terminated: {reason: "OOMKilled", exitCode: 137}})}' >"$work/injected.json"
cat "$evidence/pod-watch.json" "$work/injected.json" >"$evidence/controls/pod-watch-with-oom.json"
expect_failure pod-watch-synthetic-oom pod_watch_verdict "$evidence/controls/pod-watch-with-oom.json" "$evidence/driver-actions.txt" "$daemon_log"
{ cat "$daemon_log"; jq -cn '{msg: "supervise: process died", incarnation: "00000000-e2e4-control", observed: "gone", detail: "synthetic"}'; } >"$evidence/controls/daemon-log-with-death.log"
expect_failure pod-watch-synthetic-death pod_watch_verdict "$evidence/pod-watch.json" "$evidence/driver-actions.txt" "$evidence/controls/daemon-log-with-death.log"
# Controls for the unscheduled-retirement rule: a pod that was only ever Unschedulable and then died
# is accounted for, and the same death is unexplained once its pod was scheduled.
jq -cn '{kind: "Pod", object: {kind: "Pod", metadata: {name: "control-unscheduled", uid: "00000000-e2e4-unscheduled", labels: {}},
  spec: {}, status: {phase: "Pending", conditions: [{type: "PodScheduled", status: "False", reason: "Unschedulable"}]}}}' >"$work/unscheduled.json"
cat "$evidence/pod-watch.json" "$work/unscheduled.json" >"$evidence/controls/pod-watch-unscheduled.json"
{ cat "$daemon_log"; jq -cn '{msg: "supervise: process died", incarnation: "00000000-e2e4-unscheduled", observed: "gone", detail: "synthetic"}'; } >"$evidence/controls/daemon-log-unscheduled-death.log"
pod_watch_verdict "$evidence/controls/pod-watch-unscheduled.json" "$evidence/driver-actions.txt" "$evidence/controls/daemon-log-unscheduled-death.log" ||
  fail "the verdict counted a pod retired unscheduled as an unexplained death: $(tr '\n' ';' <"$work/pod-watch-verdict.txt")"
jq -c '.object.spec.nodeName = "control-node" | .object.status.conditions = [{type: "PodScheduled", status: "True"}]' "$work/unscheduled.json" >"$work/scheduled.json"
cat "$evidence/controls/pod-watch-unscheduled.json" "$work/scheduled.json" >"$evidence/controls/pod-watch-scheduled-then-died.json"
expect_failure pod-watch-scheduled-then-died pod_watch_verdict "$evidence/controls/pod-watch-scheduled-then-died.json" "$evidence/driver-actions.txt" "$evidence/controls/daemon-log-unscheduled-death.log"
pass

begin hygiene
stop_pid "$daemon_pid"
daemon_pid=
teardown
delete_consumers
left=$(nats_stream consumers "$nats_url" "$stream" "$consumers_prefix")
[ -z "$left" ] || fail "the run's durable consumers remain on production NATS: $left"
pass
# namespace_clean is a checkpoint of its own (lib/namespace-rig.sh), so it runs once hygiene has
# passed under its own name.
namespace_clean

begin production-audit
audit_verdict_ok=
if production_audit; then audit_verdict_ok=1; fi
[ -n "$audit_verdict_ok" ] || fail "$(audit_failure)"
printf '["AGENTC-1"]\n' >"$evidence/controls/audit-outside.json"
expect_failure production-audit-outside audit_verdict "$evidence/controls/audit-outside.json" "$evidence/production-interests-outside.json"
# The interest filter, on the run's own samples with one topic outside the run added for a session
# of the run, must find that topic.
control_session=$(jq -r '.[0] // "legion-e2e4b-control"' "$evidence/run-sessions.json")
{ cat "$evidence/interests.jsonl"; jq -cn --arg s "$control_session" '{at: "control", session_id: $s, topics: ["notifications.dispatch.issue.AGENTC-1"]}'; } >"$evidence/controls/interests-outside.jsonl"
caught=$(interests_outside "$evidence/controls/interests-outside.jsonl")
jq -e 'any(.[]; .topic == "notifications.dispatch.issue.AGENTC-1")' <<<"$caught" >/dev/null ||
  fail "the interest filter let an outside topic through: $caught"
note "the interest filter's control: an added topic outside the run (notifications.dispatch.issue.AGENTC-1) is caught"
# The unanswered-sample rule: a blip passes, a listener that stops answering fails, and so does a
# listener restart past its bound or a 503 of another body; a restart within the bound passes.
outcomes() { # outcomes START_SECOND OUTCOME…: one control sample per OUTCOME, 5 s apart
  local at=$1 outcome
  shift
  for outcome in "$@"; do
    printf '%s control %s\n' "$(date -u -d "@$at" +%FT%T.000Z)" "$outcome"
    at=$((at + 5))
  done
}
starting='error answered 503: {"error":"service starting"} '
outcomes 1790000000 ok 'error unreachable: timeout' ok 'error answered 503: busy' 'error answered 503: busy' absent \
  >"$evidence/controls/interests-outcomes-transient.txt"
outcomes 1790000000 ok 'error unreachable: timeout' 'error answered 503: busy' 'error unreachable: timeout' ok \
  >"$evidence/controls/interests-outcomes-sustained.txt"
outcomes 1790000000 ok "$starting" ok "$starting" "$starting" "$starting" ok >"$evidence/controls/interests-outcomes-restart.txt"
{ outcomes 1790000000 ok; for i in $(seq 0 61); do outcomes $((1790000005 + i * 5)) "$starting"; done; outcomes 1790000315 ok; } \
  >"$evidence/controls/interests-outcomes-long-restart.txt"
outcomes 1790000000 ok 'error answered 503: {"error":"overloaded"} ' 'error answered 503: {"error":"overloaded"} ' \
  'error answered 503: {"error":"overloaded"} ' ok >"$evidence/controls/interests-outcomes-other-503.txt"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-transient.txt")" = "[]" ] ||
  fail "two unanswered samples in a row failed the sample rule, which only three in a row do"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-sustained.txt" | jq length)" = 1 ] ||
  fail "three unanswered samples in a row passed the sample rule"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-restart.txt")" = "[]" ] ||
  fail "a listener restart answered within ${restart_bound} s failed the sample rule"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-long-restart.txt" | jq length)" -gt 0 ] ||
  fail "a listener restart of 310 s passed the sample rule"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-other-503.txt" | jq length)" = 1 ] ||
  fail "three 503s of another body in a row passed the sample rule"
note "the unanswered-sample rule's controls: two failures in a row pass, three fail; a restart within ${restart_bound} s passes, one of 310 s and three 503s of another body fail; this run had $(grep -c ' error ' "$evidence/interests-outcomes.txt" || true) unanswered samples of $(grep -c . "$evidence/interests-outcomes.txt" || true)"
# The collector itself, on real data: an actor that did write outside LEGSMOKE during the run,
# counted as one of the run's writers, must be found. Nothing is written for it.
find_outside_writer
if [ -z "$outsider" ]; then
  note "no one wrote to a production issue outside $project during the run, so the collector's real-data control had nothing to find"
else
  seen=$(touched_outside "$(jq -cn --arg a "$outsider" '[$a]')")
  printf '%s\n' "$seen" >"$evidence/controls/audit-real-outsider.json"
  [ "$(jq length <<<"$seen")" -gt 0 ] || fail "the collector did not find $outsider's writes outside $project, which it must"
  note "the collector's control: given $outsider (not the run's) as a writer, it finds $(jq length <<<"$seen") of its events outside $project"
fi
note "no write outside $project; every sampled interest of the run's sessions names $project, legion-$run_label-, or the session itself ($(wc -l <"$evidence/interests.jsonl" 2>/dev/null || echo 0) samples)"
pass

ok=1
echo "stage 4b e2e: PASS"
