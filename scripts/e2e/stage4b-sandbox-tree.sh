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
# is taken out from an operator shell. Each checkpoint prints `== <name>`, what it observed with the
# source revision, the image digest and the plugin version recorded once in `run.json`, and
# `CHECK <name>: PASS`. The first that fails ends the run non-zero with `CHECK <name>: FAIL`, naming
# it; a checkpoint that cannot run prints `CHECK <name>: BLOCKED`, naming the command that failed and
# the record it checked.
#
# Inputs:
# - LEGION_E2E_RUNTIME_CONTEXT (required) and LEGION_E2E_RUNTIME_KUBECONFIG (default
#   ~/.kube/legion-daemon-production) name the restricted identity the daemon runs as.
# - LEGION_E2E_OPERATOR_CONTEXT (default production) names the admin context.
# - LEGION_E2E_IMAGE (required) is the worker image, by digest.
# - STAGE4B_UNTIL=<checkpoint> stops after that checkpoint. A run with it set is a development run,
#   never the proof, and never prints PASS.
# - STAGE4B_EVIDENCE_DIR (default a fresh /tmp directory, kept and printed) holds the transcript, the
#   daemon log, the pod watch, every agent transcript, and the negative controls.
#
# The production bearers (Secrets Manager's production/dispatch/agent-token and
# production/envoy/api-token) are read with the devbox admin role into 0600 files under the run's
# scratch directory. They are never printed, never in an argv (curl reads them from header files),
# and never in the evidence. One run at a time: the project, the NATS durable consumer names, ports
# 13370/13371 and the namespace label are shared, so the run takes a lock and refuses to start while
# another holds it, or while LEGSMOKE has pods, Sandboxes or claims it did not create.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d /tmp/legion-e2e4b.XXXXXXXX)
evidence=${STAGE4B_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2e4b-evidence.XXXXXXXX)}
mkdir -p "$evidence/logs" "$evidence/transcripts" "$evidence/pods" "$evidence/controls"
# tee shares the driver's process group, so a signal to the group (Ctrl-C, a closed pane, timeout's
# TERM) would end it before cleanup writes, and cleanup's first write would die of SIGPIPE: tee
# ignores the signals the driver traps, and outlives the driver's last line.
exec > >(trap '' HUP INT TERM && exec tee -a "$evidence/transcript.log") 2>&1
# fd 7 keeps the transcript for cleanup: a signal runs the EXIT trap under the redirections of the
# command it interrupted, whose output may be /dev/null or an evidence file.
exec 7>&1

namespace=legion
operator=${LEGION_E2E_OPERATOR_CONTEXT:-production}
runtime_kubeconfig=${LEGION_E2E_RUNTIME_KUBECONFIG:-$HOME/.kube/legion-daemon-production}
runtime_context=${LEGION_E2E_RUNTIME_CONTEXT:-}
image=${LEGION_E2E_IMAGE:-}
until=${STAGE4B_UNTIL:-}
# The Dispatch project key (the workflow's) and its token (the pods' label, the claims' prefix).
project=LEGSMOKE
run_label=legsmoke
label_exact=legsmoke
repo=sjawhar/legion-smoke
dispatch_base=https://dispatch.internal.trajectorylabs.com
# The proof human writes with the agents' bearer, so it names a session of its own: one that holds
# no claim, whose status writes the workflow therefore reads as a human's.
dispatch_actor=legion-e2e4b-proof-human-$$
envoy_url=http://envoy-listener.internal.trajectorylabs.com:9020
nats_url=nats://nats.internal.trajectorylabs.com:4222
gateway_url=https://middleman.hawk.internal.trajectorylabs.com
port_daemon=13370
port_worker_stream=13371
stream=ENVOY_NOTIFICATIONS
# One path for every run on the devbox, whatever its environment names as its state directory.
lock=$HOME/.local/state/legion/e2e/stage4b.lock
record=$work/sandboxes
pg_container=legion-e2e4b-pg-$$
profile=legion-e2e4b-$$-$(date +%s)
state=$work/state
daemon_log=$evidence/logs/daemon.log
check=setup
ok=
torn_down=
snapshotted=
compared=
locked=
timeout_hook=
daemon_pid=
watch_pid=
events_pid=
sampler_pid=
interests_pid=
shape_pid=
controller_session=
host=
pin=
tree1=
tree2=
tree3=
pr_number=
smoke_file=
prod_baseline=
audited=
fixture_branch=
pair_recorded=

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
# pass ends a development run once its STAGE4B_UNTIL checkpoint has passed.
pass() {
  echo "CHECK $check: PASS"
  [ -z "$daemon_pid" ] || interests_sample "$check"
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
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/namespace-rig.sh
. "$root/scripts/e2e/lib/namespace-rig.sh"


rk() { timeout --foreground 300 kubectl --kubeconfig "$runtime_kubeconfig" --context "$runtime_context" "$@"; }

# ---- the runtime seam: a claim's process is a pod, its session and workspace on the tree volume ----

# claim_view ISSUE ROLE prints the claim as the daemon's state projects it.
claim_view() {
  daemon_state | jq -ce --arg issue "$1" --arg role "$2" \
    'if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end'
}
claim_sandbox() { claim_view "$1" "$2" | jq -er '.locator.sandbox.name'; }
claim_pod_uid() { claim_view "$1" "$2" | jq -er '.locator.incarnation'; }
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
# every task call naming it, the tool result of each call (text, isError, details), and every
# async-result delivery naming it. Nothing is summarised, so the evidence keeps each failure's text.
pair_dispatch() {
  jq -R -s -c --arg agent "$1" '[split("\n")[] | fromjson?] as $e
    | [$e[] | select(.type == "message" and .message.role == "assistant") | .message.content[]?
        | select(.type == "toolCall" and .name == "task" and (.arguments | tostring | contains($agent)))] as $calls
    | ($calls | map(.id)) as $ids
    | {agent: $agent,
       calls: [$calls[] | {id, arguments}],
       results: [$e[] | select(.type == "message" and .message.role == "toolResult" and (.message.toolCallId as $i | $ids | index($i)))
         | .message | {toolCallId, isError, text: ([.content[]? | select(.type == "text") | .text] | join("\n")), details}],
       deliveries: [$e[] | select(.type == "custom_message" and .customType == "async-result" and (.content | tostring | contains("agent=\"" + $agent + "\"")))
         | {timestamp, content: (.content | tostring)}]}'
}
# pair_settled: the reviewer dispatched both agents and each dispatch has an outcome: a refused call,
# a finished result naming the agent, or a delivered background result.
pair_settled() {
  local text agent
  text=$(claim_session_text "$tree1" reviewer) || return 1
  for agent in $pair_agents; do
    pair_dispatch "$agent" <<<"$text" | jq -e --arg agent "$agent" '(.calls | length) > 0 and (
      any(.results[]; .isError == true) or (.deliveries | length) > 0
      or any(.results[].details.results[]?; .agent == $agent))' >/dev/null || return 1
  done
}
# record_pair keeps the reviewer's session, the sessions of the subagents it started, and each
# agent's dispatch under $evidence/review-pair, once.
record_pair() {
  local file pod text agent
  [ -z "$pair_recorded" ] || return 0
  file=$(claim_session_file "$tree1" reviewer) || return 1
  pod=$(tree_pod "$tree1") || return 1
  mkdir -p "$evidence/review-pair"
  text=$(pod_exec "$pod" cat -- "$file") || return 1
  printf '%s\n' "$text" >"$evidence/review-pair/reviewer.jsonl"
  op exec "$pod" -c worker -- tar -C "$(dirname "$file")" -cf - "$(basename "$file" .jsonl)" 2>/dev/null |
    tar -C "$evidence/review-pair" -xf - 2>/dev/null || true
  for agent in $pair_agents; do
    pair_dispatch "$agent" <<<"$text" >"$evidence/review-pair/$agent.json"
  done
  printf '%s\n' "$(basename "$file" .jsonl)" >"$evidence/review-pair/session-stem"
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
# assert_claim_endpoints ISSUE ROLE: the claim's pod names production's services and the gateway,
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
  for name in DISPATCH_URL ENVOY_URL ENVOY_NATS_URL LEGION_MODEL_GATEWAY_URL LEGION_DAEMON_URL; do
    case "$name" in
      DISPATCH_URL) want=$dispatch_base ;;
      ENVOY_URL) want=$envoy_url ;;
      ENVOY_NATS_URL) want=$nats_url ;;
      LEGION_MODEL_GATEWAY_URL) want=$gateway_url ;;
      LEGION_DAEMON_URL) want="http://$host:$port_daemon" ;;
    esac
    got=$(sed -n "s/^$name=//p" <<<"$env")
    if [ "$got" != "$want" ]; then
      printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"
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
    aws secretsmanager get-secret-value --secret-id production/dispatch/agent-token --query SecretString --output text >"$work/dispatch-token" &&
    aws secretsmanager get-secret-value --secret-id production/envoy/api-token --query SecretString --output text >"$work/envoy-token" &&
    printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
    cp "$work/dispatch-auth-header" "$work/dispatch-human-header" &&
    printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
  [ -s "$work/dispatch-token" ] && [ -s "$work/envoy-token" ] || fail "Secrets Manager returned an empty bearer"
}
nats_stream() { bun "$root/scripts/e2e/lib/nats-stream.ts" "$@"; }

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
  design: "off"
admission_cap: 2
linger_hours: 0.3
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
    gateway:
      url: $gateway_url
      audience: middleman-legion
      service_account: legion-worker
      token_expiry_seconds: 600
EOF
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
# pod_watch_verdict WATCH ACTIONS DAEMONLOG prints every pod of the run the node ended (Evicted, or
# a container OOMKilled), and every claim process the daemon found dead (`supervise: process died`,
# naming the pod uid as the incarnation) that no driver action ended, and exits 1 when there is any.
# A pod the daemon suspended, released or closed ends without either, so it needs no match. The
# resume that finds the tree volume lost dies by design (its detail begins workspace-lost:), and
# re-admission counts those itself. The memory hog, labelled legion.dev/e2e-control=memory-hog, is
# excluded, and must have been seen OOMKilled.
pod_watch_verdict() {
  local watch=$1 actions=$2 log=$3
  jq -s -r --rawfile actions "$actions" --rawfile log "$log" '
    ($actions | split("\n") | map(select(. != "") | split(" ")[1])) as $driver
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
      + [ $died[] | select((.detail // "") | startswith("workspace-lost:") | not)
          | .incarnation as $i | select(($driver | index($i)) == null)
          | "incarnation \($i) died with no driver action: observed \(.observed), \((.detail // "") | .[0:200])" ]
      | unique as $bad
    | if $hog then $bad[] else ("the memory hog was never seen OOMKilled", $bad[]) end
  ' "$watch" | tee "$work/pod-watch-verdict.txt"
  [ ! -s "$work/pod-watch-verdict.txt" ]
}
start_pod_watch() {
  # kubectl itself, not a subshell around a function, so the recorded pid is what cleanup stops; and
  # fd 9, the run lock, stays out of every background child, so none can outlive the run holding it.
  kubectl --context "$operator" -n "$namespace" get pods -l "legion.dev/project=$run_label" -w -o json --output-watch-events \
    >"$evidence/pod-watch.json" 2>"$evidence/logs/pod-watch.err" 9>&- 7>&- &
  watch_pid=$!
  kubectl --context "$operator" get events -A -w -o json --field-selector involvedObject.kind=Node \
    >"$evidence/node-events.json" 2>"$evidence/logs/node-events.err" 9>&- 7>&- &
  events_pid=$!
  ( # Node memory for the nodes the run's pods are on, every 30 s (metrics-server).
    trap - EXIT ERR
    set +e
    while :; do
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

# check_pod_shape POD UID prints each way the pod departs from the shape every Sandbox pod has, or
# nothing: gVisor, the gateway's ServiceAccount and its one projected token, the pool, the restricted
# security context, no token value in a container's environment, command or args, and 4b.6b's split
# provisioning (the provisioning token only in workspace-fetch, the feed read-only in workspace-init,
# no provision directory in the worker).
check_pod_shape() {
  local pod=$1 spec tokens
  spec=$(op get pod "$pod" -o json) || {
    echo "the pod's spec could not be read"
    return
  }
  jq -r '
    .spec as $s
    | (if $s.runtimeClassName != "gvisor" then "runtimeClassName \($s.runtimeClassName)" else empty end),
      (if $s.serviceAccountName != "legion-worker" then "serviceAccountName \($s.serviceAccountName)" else empty end),
      (if $s.automountServiceAccountToken != false then "automountServiceAccountToken \($s.automountServiceAccountToken)" else empty end),
      (if ([$s.volumes[] | select(.projected) | .projected.sources[] | select(.serviceAccountToken)] | length) != 1
        or ([$s.volumes[] | select(.projected) | .projected.sources[] | select(.serviceAccountToken) | .serviceAccountToken.audience] != ["middleman-legion"])
        then "the projected gateway token source is not the one middleman-legion token" else empty end),
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
  ' <<<"$spec"
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
# pod_shape_watcher checks every Sandbox pod of the run once it is Running, and records its facts. A
# departure is recorded as the run's violation, and the next bounded wait aborts naming it.
pod_shape_watcher() {
  local pod uid problems
  trap - EXIT ERR
  set +e
  while :; do
    while IFS=$'\t' read -r pod uid; do
      [ -n "$pod" ] || continue
      grep -qF " $uid " "$evidence/pods-checked.txt" 2>/dev/null && continue
      op get pod "$pod" -o json >"$evidence/pods/$uid.json" 2>/dev/null
      problems=$(check_pod_shape "$pod")
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
unchecked_sandbox_pods() {
  local uid
  for uid in $(jq -r 'select(.object.kind == "Pod") | .object | select(.metadata.labels["legion.dev/probe"] == null and .metadata.labels["legion.dev/e2e-control"] == null)
      | select(any(.status.containerStatuses[]?; .name == "worker" and .ready)) | .metadata.uid' "$evidence/pod-watch.json" | sort -u); do
    grep -qF " $uid " "$evidence/pods-checked.txt" 2>/dev/null || printf '%s\n' "$uid"
  done
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
collect_transcripts() {
  local pod
  for tree in $tree1 $tree2 $tree3; do
    pod=$(tree_pod "$tree") || continue
    op exec "$pod" -c worker -- tar -C /home/legion/.omp/profiles/legion/agent/sessions -cf - . 2>/dev/null |
      tar -C "$evidence/transcripts" -xf - 2>/dev/null || true
  done
}
cleanup() {
  local status=$? p
  # A second signal must not cut the teardown short, and a closed output must not end it.
  trap '' HUP INT TERM PIPE
  exec >&7 2>&7
  set +e
  stop_pid "$shape_pid"
  [ -z "$tree1" ] || record_pair >/dev/null 2>&1
  stop_pid "$daemon_pid"
  collect_transcripts
  stop_pid "$watch_pid"
  stop_pid "$events_pid"
  stop_pid "$sampler_pid"
  stop_pid "$interests_pid"
  # The namespace label, the durable consumers and the project are shared by every Stage 4b run,
  # so a run that never passed prerequisites' ownership checks (the lock, the ports, no leftover
  # objects or consumers) owns none of them and removes nothing.
  if [ -n "$locked" ]; then
    # The teardown waits for the run's labelled pods to go but deletes no pod itself, so the run's
    # own control pods (the memory hog, the reachability pod) go first.
    op delete pod -l "legion.dev/project=$run_label,legion.dev/e2e-control" --ignore-not-found --wait=false >/dev/null 2>&1
    teardown
    if [ -z "$compared" ] && [ -n "$snapshotted" ]; then (namespace_clean) || status=1; fi
    delete_consumers
    [ -z "$fixture_branch" ] || gh api -X DELETE "repos/$repo/git/refs/heads/$fixture_branch" >/dev/null 2>&1
    if [ -z "$audited" ] && [ -n "$prod_baseline" ]; then production_audit || status=1; fi
  fi
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null; done
  docker rm -f "$pg_container" >/dev/null 2>&1
  rm -rf "$HOME/.omp/profiles/$profile" "$work"
  [ -n "$ok" ] || echo "stage 4b e2e: FAIL (check $check)"
  echo "evidence: $evidence (transcript.log, logs/daemon.log, pod-watch.json, pods/, transcripts/, the namespace snapshots)"
  exit "$status"
}
trap cleanup EXIT
trap 'echo "CHECK $check: FAIL: line $LINENO exited $?: $BASH_COMMAND"' ERR
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
  sessions=$(jq -R -s -c 'split("\n") | map(fromjson? | select(.msg == "api: claim registered") | .session) | unique' "$daemon_log")
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
  unanswered=$(interests_unanswered "$evidence/interests-outcomes.txt") || unanswered='"the interest sample outcomes could not be read"'
  if [ "$unanswered" != "[]" ]; then
    jq -c 'if type == "string" then [.] else [{"sessions the Envoy listener left unanswered for 3 samples in a row": .}] end' <<<"$unanswered" >"$evidence/production-interests-outside.json"
  elif [ "$unvouched" != "[]" ]; then
    jq -c '[{"registered sessions with no interest sample": .}]' <<<"$unvouched" >"$evidence/production-interests-outside.json"
  elif outside=$(interests_outside "$evidence/interests.jsonl"); then
    printf '%s\n' "$outside" >"$evidence/production-interests-outside.json"
  else
    printf '"the interest samples could not be read"\n' >"$evidence/production-interests-outside.json"
  fi
  audit_verdict "$evidence/production-issues-touched-outside.json" "$evidence/production-interests-outside.json"
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
# to $evidence/interests.jsonl, each line labelled, and each attempt's outcome to
# $evidence/interests-outcomes.txt as "TIME SESSION ok|absent|error DETAIL". A session no longer
# registered answers 404 (absent). No answer, or any other, is an error, so an unreadable listener
# never reads as a clean sample.
interests_sample() {
  local session code now tmp=$work/interest.$BASHPID.json
  for session in $(jq -R -r 'fromjson? | select(.msg == "api: claim registered") | .session' "$daemon_log" | sort -u); do
    now=$(date -u +%FT%T.%3NZ)
    if ! code=$(curl -sS --max-time 20 -o "$tmp" -w '%{http_code}' -H "@$work/envoy-auth-header" "$envoy_url/v1/interests/$session" 2>&1); then
      printf '%s %s error unreachable: %s\n' "$now" "$session" "$(tr '\n' ' ' <<<"$code")" >>"$evidence/interests-outcomes.txt"
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
# interests_unanswered OUTCOMES prints, as one JSON array, each session whose samples went
# unanswered 3 or more times in a row, with its longest such run. The sampler leaves 5 s between a
# session's samples in any case; one or two failures widen that gap to 10 or 15 s, a blip the
# evidence keeps; a third in a row is a listener not answering for that session, which the audit
# cannot vouch past. An ok or absent answer ends a run.
interests_unanswered() {
  awk -v limit=3 '
    $3 == "error" {
      run[$2]++
      if (run[$2] == 1) from[$2] = $1
      if (run[$2] > worst[$2]) { worst[$2] = run[$2]; wfrom[$2] = from[$2]; wto[$2] = $1; wlast[$2] = $0 }
      next
    }
    { run[$2] = 0 }
    END { for (s in worst) if (worst[s] >= limit) printf "%s\t%d\t%s\t%s\t%s\n", s, worst[s], wfrom[s], wto[s], wlast[s] }
  ' "$1" | jq -R -s -c '[split("\n")[] | select(. != "") | split("\t") | {session: .[0], consecutive: (.[1] | tonumber), from: .[2], to: .[3], last: .[4]}]'
}
# start_interests_sampler samples every 5 s while the daemon runs, besides every checkpoint's pass,
# so a session that registers and ends between two checkpoints is still seen.
start_interests_sampler() {
  : >>"$evidence/interests-outcomes.txt"
  (
    trap - EXIT ERR
    set +e
    while :; do
      interests_sample sampler
      sleep 5
    done
  ) >/dev/null 2>&1 9>&- 7>&- &
  interests_pid=$!
}
# interests_outside FILE prints, as one JSON array, every sampled topic in FILE outside the run: a
# topic of the run names LEGSMOKE, the project's own subject space (notifications.legion.legsmoke.),
# a legion-legsmoke- role, or the session itself.
interests_outside() {
  jq -s -c --arg p "$project" --arg t "legion-$run_label-" --arg space "notifications.legion.$run_label." '[.[] | .session_id as $s | .topics[]?
    | select((contains($p) or contains($t) or startswith($space) or contains($s)) | not) | {session: $s, topic: .}] | unique' "$1"
}
audit_verdict() { [ "$(jq -c . "$1")" = "[]" ] && [ "$(jq -c . "$2")" = "[]" ]; }

# ==== checkpoints ===================================================================================

begin prerequisites
for tool in go docker jq curl ss kubectl aws gh bun jj mise shellcheck secrets; do command -v "$tool" >/dev/null || fail "$tool is required"; done
[ -n "$runtime_context" ] || fail "LEGION_E2E_RUNTIME_CONTEXT is unset: the daemon runs as the Legion daemon's restricted identity, never the operator's"
[ -r "$runtime_kubeconfig" ] || fail "the runtime kubeconfig $runtime_kubeconfig is not readable"
case "$image" in *@sha256:*) ;; *) fail "LEGION_E2E_IMAGE must be the worker image by digest (…@sha256:…), not '$image'" ;; esac
mkdir -p "$(dirname "$lock")"
exec 9>"$lock"
flock -n 9 || fail "another Stage 4b run holds $lock: one run at a time"
for port in "$port_daemon" "$port_worker_stream"; do
  [ -z "$(ss -Hltn "sport = :$port")" ] || fail "port $port is taken on the devbox: $(ss -Hltnp "sport = :$port")"
done
imds=$(curl -sf -m 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60') ||
  fail "instance metadata is unreachable; the daemon binds the devbox's private address, read from it"
host=$(curl -sf -m 5 -H "X-aws-ec2-metadata-token: $imds" http://169.254.169.254/latest/meta-data/local-ipv4) || fail "instance metadata has no local-ipv4"
unset imds
leftover=$(op get sandboxes,pods,pvc -l "legion.dev/project=$run_label" -o name 2>&1) || fail "the operator context cannot list namespace $namespace: $leftover"
[ -z "$leftover" ] || fail "namespace $namespace already holds objects labelled legion.dev/project=$run_label, which another run left or owns: $(tr '\n' ' ' <<<"$leftover")"
stale_consumers=$(bun "$root/scripts/e2e/lib/nats-stream.ts" consumers "$nats_url" "$stream" "legion-go-$project-") ||
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
read_bearers
pass

begin preflight
# The runtime identity is the restricted role, and nothing more.
who=$(rk auth whoami -o json) || blocked "kubectl auth whoami under $runtime_context failed"
jq -e '.status.userInfo.username | test("assumed-role/production-legion-daemon/")' <<<"$who" >/dev/null ||
  fail "the runtime identity is $(jq -r .status.userInfo.username <<<"$who"), not the production-legion-daemon role"
jq -e '.status.userInfo.groups | index("legion-daemon")' <<<"$who" >/dev/null || fail "the runtime identity is not in group legion-daemon"
[ "$(rk auth can-i list secrets -n "$namespace" 2>/dev/null)" = no ] || fail "the runtime identity can list Secrets in $namespace"
note "[runtime] $(jq -r .status.userInfo.username <<<"$who"); list secrets -n $namespace: no"
kubectl --context "$operator" get crd sandboxes.agents.x-k8s.io -o name >/dev/null || fail "the Agent Sandbox CRD is not installed"
floor=$(kubectl --context "$operator" get nodepool legion -o json |
  jq -c '[.spec.template.spec.requirements[] | select(.key == "karpenter.k8s.aws/instance-cpu")]')
jq -e 'any(.[]; .operator == "Gt" and (.values | index("3")))' <<<"$floor" >/dev/null || fail "the legion NodePool has no instance-cpu Gt 3 floor: $floor"
note "[operator] CRD sandboxes.agents.x-k8s.io installed; NodePool legion floor $floor"
# LEGSMOKE's stale todo roots would be admitted at boot ahead of the run's own.
stale=$(dispatch_get "issues?project=$project&status=todo&limit=200" | jq -r '.[] | select(.parent == null or .parent == "") | .key')
for key in $stale; do
  set_status "$key" backlog
  note "moved stale todo root $key to backlog"
done
[ -z "$(dispatch_get "issues?project=$project&status=todo&limit=200" | jq -r '.[].key')" ] || fail "LEGSMOKE still holds todo issues"
# The configured stream carries both halves of the workflow's intake. Dispatch publishes every
# project's issue events under subjects that name the issue, so any project's shows the Dispatch
# half (LEGSMOKE's own age out between runs; admission is where the run's are seen); the GitHub
# half is the smoke repository's own.
for subject in "notifications.dispatch.issue.>" "notifications.github.sjawhar.legion-smoke.>"; do
  seen=$(nats_stream last "$nats_url" "$stream" "$subject" 20) ||
    blocked "production NATS $stream carries no message on $subject (bun scripts/e2e/lib/nats-stream.ts last $nats_url $stream '$subject'): the daemon's intake would miss that half of the workflow"
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
          # The image has no curl: bun answers the HTTP services, bash's /dev/tcp the NATS port.
          for url in $dispatch_base/healthz $envoy_url/healthz $gateway_url/health; do
            printf '%s ' "\$url"
            URL="\$url" bun -e 'const r = await fetch(process.env.URL, { signal: AbortSignal.timeout(10000) }).catch(() => null); console.log(r ? r.status : "unreachable")'
          done
          printf '%s ' $nats_url
          timeout 5 bash -c 'exec 3<>/dev/tcp/nats.internal.trajectorylabs.com/4222 && head -c 4 <&3' || printf unreachable
          echo
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: [ALL] } }
EOF
op apply -f "$reach" >/dev/null || blocked "the operator could not create the reachability pod ($reach)"
until_true 600 "the reachability pod to finish" sh -c "timeout 120 kubectl --context '$operator' -n '$namespace' get pod legion-e2e4b-reach-$$ -o jsonpath='{.status.phase}' | grep -qx 'Succeeded\|Failed'"
op logs "legion-e2e4b-reach-$$" >"$evidence/reach.txt" 2>&1
op delete pod "legion-e2e4b-reach-$$" --wait=false >/dev/null 2>&1
while read -r url answer; do
  case "$answer" in 000 | unreachable | "") fail "a pod on the Legion pool cannot reach $url ($evidence/reach.txt)" ;; esac
  note "[pod] $url → $answer"
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
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion") || fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"
# The build's source is the run's recorded source, or the tree changed between the two.
[ "$(sed -n 's/^source: //p' <<<"$built")" = "$(jq -r .revision "$evidence/run.json")" ] ||
  fail "the tree changed between prerequisites ($(jq -r .revision "$evidence/run.json")) and the build ($(sed -n 's/^source: //p' <<<"$built"))"
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=legion \
  -p "127.0.0.1::5432" postgres:16 >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion
mkdir -p "$state"
cat >"$work/instructions.md" <<'EOF'
# Stage 4b proof instructions

This is a throwaway workflow proof on the disposable LEGSMOKE project. Do not act until a targeted
human Dispatch message gives the next exact proof operation. Follow that instruction precisely, use
the Go-daemon Legion tools and handoffs, and do not create work outside the issue's smoke branch.
EOF
write_legion_config
out=$("$work/legion" start --check-config --config "$work/legion.yaml" 2>&1) || fail "legion start --check-config refused the proof's config: $out"
note "$out"
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
tree1=$(new_issue "Stage 4b proof tree 1: the whole workflow ($work)")
tree2=$(new_issue "Stage 4b proof tree 2: planner beside tree 1, with the repository fixture ($work)")
tree3=$(new_issue "Stage 4b proof tree 3: the held phase for the controller ($work)")
push_fixture "$tree2"
for issue in "$tree1" "$tree2" "$tree3"; do set_status "$issue" todo; done
until_true 300 "two roots admitted and one waiting in rank order" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg a '$tree1' --arg b '$tree2' --arg c '$tree3' '.admission.cap == 2 and .admission.active == [\$a,\$b] and .admission.waiting == [\$c]'"
state_file admission
note "active $(jq -c .admission.active "$evidence/admission.json"), waiting $(jq -c .admission.waiting "$evidence/admission.json")"
shape_pid=
pod_shape_watcher 9>&- 7>&- &
shape_pid=$!
pass

# drive_spec ISSUE: the architect writes its spec and registers the gate; with gates.design off the
# daemon approves the registered version itself and the issue moves to planning.
drive_spec() {
  local issue=$1 artifact
  artifact=$(dispatch_get "issues/$issue" | jq -er .primary_artifact_id)
  wait_for_worker "$issue" architect
  send_agent "$issue" architect "Stage 4b proof spec operation: update this issue's primary spec document with one tiny, concrete one-file smoke change for $repo, and say in it that a review of the pull request may ask for one more line appended to that same file, which is in scope. Then use the Go-daemon Legion operation to register the gate for exactly artifact $artifact at its current version. The design gate is off, so no approval is needed. Wait after registering."
  until_true 600 "the $issue architect to register primary artifact $artifact" gate_registered "$issue" "$artifact"
  wait_for_phase "$issue" planning
}

begin spec-posted
drive_spec "$tree1"
drive_spec "$tree2"
for issue in "$tree1" "$tree2"; do
  version=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.currentVersion')
  note "$issue: the architect posted its spec (version $version) and the daemon moved it to planning"
done
pass

begin tree-separation
wait_for_worker "$tree1" planner
send_agent "$tree1" planner "Stage 4b proof planning operation: write the required .legion/plan.json handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$tree1" implementing 900
wait_for_worker "$tree1" implementer
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
# path, and which ones a pod's agent loads is the boundary LEGION-263 owns. The markers live in the
# pod's own /tmp, which goes with the pod once the planner's phase ends and its Sandbox suspends, so
# they are read while the planner waits after its first turn; then it plans. The argv the pod ran
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
# Tree 2 leaves the line: its slot frees and the waiting root takes it.
set_status "$tree2" backlog
until_true 300 "tree 3 to take tree 2's admission slot" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg c '$tree3' '(.admission.active | index(\$c)) != null'"
until_true 300 "tree 2's Sandboxes to be suspended" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get pods -l 'legion.dev/project=$run_label,legion.dev/tree=$tree2' -o name) && [ -z \"\$out\" ]"
note "tree 2 moved to backlog, its pods gone; tree 3 ($tree3) admitted"
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
send_agent "$tree1" reviewer "Stage 4b proof review operation: review pull request #$pr_number in $repo as your role requires, running the deep and code-quality review passes your instructions name as task subagents, then submit APPROVE on it at its current head as legion-reviewer[bot] and complete the reviewer handoff."
until_true 1800 "the reviewer's two thermonuclear dispatches to reach an outcome" pair_settled
record_pair || fail "the reviewer's session and its review pair could not be recorded"
note "the review pair's dispatches are kept in $evidence/review-pair ($(jq -r -s 'map("\(.agent): \(.calls | length) calls, \(.results | length) results, \(.deliveries | length) deliveries") | join("; ")' "$evidence"/review-pair/thermonuclear-*.json))"
until_true 1800 "legion-reviewer[bot] approval of pull request #$pr_number at its head" reviewer_approved_head
until_true 900 "$tree1 to leave reviewing for retro" issue_phase_in "$tree1" retro merging
if issue_phase "$tree1" retro >/dev/null; then
  wait_for_worker "$tree1" implementer
  send_agent "$tree1" implementer "Stage 4b proof retro: write the required retro handoff for pull request #$pr_number and complete the phase. Do not change the approved implementation."
fi
wait_for_phase "$tree1" merging 1800
for role in planner implementer tester reviewer merger; do
  claim_view "$tree1" "$role" >/dev/null 2>&1 || continue
done
note "$tree1 moved planner → implementer → tester → reviewer → retro → merging with real agents; $repo#$pr_number changes $smoke_file"
pass

begin review-pair
# The reviewer's two review passes are the image's thermonuclear agents, dispatched by name, and each
# must have run: a delivered background result names the agent as completed, and the subagent's own
# session, beside the reviewer's, ends in an accepted yield. A missing agent is refused to the model
# as "Unknown agent", an agent whose declared model the pod cannot resolve fails "No model
# selected", and a model that substitutes the bundled reviewer still posts a verdict, which looks
# the same from outside. tree-moved recorded the dispatches (record_pair); each failure below
# quotes them.
stem=$(cat "$evidence/review-pair/session-stem")
for agent in $pair_agents; do
  d=$evidence/review-pair/$agent.json
  [ "$(jq '.calls | length' "$d")" -gt 0 ] || fail "the reviewer dispatched no task naming $agent"
  said=$(jq -r '[.results[].text, .deliveries[].content] | join("\n")' "$d")
  if grep -qF "Unknown agent \"$agent\"" <<<"$said"; then fail "the reviewer's task for $agent was refused: $(grep -F 'Unknown agent' <<<"$said" | head -1)"; fi
  if grep -qF 'No model selected' <<<"$said"; then fail "the reviewer's task for $agent did not run: $(grep -F 'No model selected' <<<"$said" | head -1)"; fi
  id=$(jq -r --arg agent "$agent" '[.deliveries[].content | capture("<task-result id=\"(?<id>[^\"]+)\" agent=\"" + $agent + "\" status=\"completed\"")? | .id] | first // empty' "$d")
  [ -n "$id" ] || fail "the reviewer's task for $agent has no completed delivery: $(jq -c '[.results[] | {isError, text: .text[0:300]}] + [.deliveries[] | .content[0:300]]' "$d")"
  sub=$evidence/review-pair/$stem/$id.jsonl
  [ -s "$sub" ] || fail "$agent's session $stem/$id.jsonl is not beside the reviewer's"
  jq -R -s -e '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "toolResult" and .message.toolName == "yield" and .message.isError == false)] | length > 0' "$sub" >/dev/null ||
    fail "$agent's session $id holds no accepted yield"
  note "$agent ran as $id: its delivery says completed and its session ends in an accepted yield"
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
# A pod's projected gateway token rotates within its 600 s expiry, and a model turn after the
# rotation still goes through the gateway. The architect's pod is the longest-lived.
pod=$(claim_sandbox "$tree1" architect)
pod_uid=$(op get pod "$pod" -o jsonpath='{.metadata.uid}')
# token_hash prints the token file's sha256, or fails: an exec that did not answer is never a hash.
token_hash() { pod_exec "$pod" sha256sum /var/run/legion/gateway/token | cut -d' ' -f1 | grep -xE '[0-9a-f]{64}'; }
first=$(token_hash) || fail "the architect pod $pod's gateway token could not be read"
started=$(date +%s)
token_rotated() {
  local now
  now=$(token_hash) || return 1
  [ "$now" != "$first" ]
}
until_true 900 "the architect pod's gateway token to rotate" token_rotated
now_uid=$(op get pod "$pod" -o jsonpath='{.metadata.uid}')
[ "$now_uid" = "$pod_uid" ] || fail "the architect's pod was replaced during the wait ($pod_uid -> $now_uid), so the new token is another pod's, not a rotation"
rotated=$(date -u +%FT%TZ)
note "the token file changed $(($(date +%s) - started)) s after sampling (at $rotated)"
send_agent "$tree1" architect "Stage 4b proof: reply to this message with one short sentence, then wait."
# The poll runs in this shell: the session readers need the run's own variables, which a child sh
# would not have.
turn_after_rotation() {
  claim_session_text "$tree1" architect | jq -R -s -e --arg at "$rotated" '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant" and .timestamp > $at)] | any(.message.stopReason == "stop")' >/dev/null
}
until_true 600 "a completed architect turn after the rotation" turn_after_rotation
after=$(claim_session_text "$tree1" architect | jq -R -s -c --arg at "$rotated" '[split("\n")[] | fromjson? | select(.type == "message" and .message.role == "assistant" and .timestamp > $at) | "\(.message.provider)/\(.message.model)"] | unique')
note "turns after the rotation were answered by $after"
jq -e 'all(.[]; test("^anthropic/claude-[a-z0-9.-]+-legion$"))' <<<"$after" >/dev/null || fail "a turn after the rotation left the gateway's aliases: $after"
pass

begin idle-suspend
# A finished worker's Sandbox is Suspended, its pod gone, the tree volume bound.
for role in planner tester reviewer; do
  if [ "$(claim_view "$tree1" "$role" | jq -r .state)" = suspended ]; then note "$role on $tree1 is suspended"; fi
done
suspended=$(op get sandboxes -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o json | jq -c '[.items[] | select(.spec.operatingMode == "Suspended") | .metadata.name]')
[ "$suspended" != "[]" ] || fail "no Sandbox of $tree1 is Suspended after its phases finished"
bound=$(op get pvc -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].status.phase}')
[ "$bound" = Bound ] || fail "the tree volume of $tree1 is '$bound', want Bound"
note "Suspended Sandboxes of $tree1: $suspended; its tree volume Bound"
pass

begin kill-pod-resume
pod=$(claim_sandbox "$tree1" merger)
uid=$(claim_pod_uid "$tree1" merger)
session=$(claim_view "$tree1" merger | jq -r .session)
driver_action kill "$uid"
op exec "$pod" -c worker -- sh -c 'kill 1' >/dev/null 2>&1 || true
until_true 300 "the merger's Sandbox to report Finished for pod $uid" sh -c \
  "timeout 120 kubectl --context '$operator' -n '$namespace' get sandbox '$pod' -o json | jq -e '.status.conditions[]? | select(.type == \"Finished\" and .status == \"True\")' >/dev/null || [ \"\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree1' '.issues[\$i].workers.merger.claim.locator.incarnation // empty')\" != '$uid' ]"
until_true 600 "the merger to relaunch with a new pod" sh -c \
  "[ \"\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree1' '.issues[\$i].workers.merger.claim.locator.incarnation // empty')\" != '$uid' ]"
new_uid=$(claim_pod_uid "$tree1" merger)
[ "$(claim_view "$tree1" merger | jq -r .session)" = "$session" ] || fail "the relaunched merger has another session"
resume=$(op get pod "$pod" -o json | jq -r '[.spec.containers[] | select(.name == "worker") | .command[]?] | map(select(startswith("--resume"))) | first // empty')
[ -n "$resume" ] || fail "the relaunched merger pod runs without --resume"
note "killed PID 1 of $pod (uid $uid); relaunched as uid $new_uid, same session $session, $resume"
pass

begin fence
# (a) A pod the controller recreates on its own is never adopted: the daemon suspends it, and the
# claim relaunches with a third uid.
uid=$(claim_pod_uid "$tree1" merger)
# (b) needs the boot token of the generation (a) replaces, so it is read before the delete.
boot_token() { op get secret "$pod-boot" -o json | jq -er '.data.LEGION_BOOT_TOKEN // empty | @base64d' | grep .; }
old_token=$(boot_token) || blocked "the merger's boot Secret $pod-boot has no LEGION_BOOT_TOKEN"
(umask 077 && printf '%s' "$old_token" >"$work/old-boot-token")
driver_action delete "$uid"
op delete pod "$pod" --wait=false >/dev/null
until_true 600 "the merger's claim to move off pod $uid" sh -c \
  "[ \"\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree1' '.issues[\$i].workers.merger.claim.locator.incarnation // empty')\" != '$uid' ]"
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

begin controller
# `legion controller start` on the devbox registers with the Sandbox daemon; tree 3 supplies the
# held phase whose notice reaches it; `legion status … backlog` from the operator shell takes
# tree 3 out, and Dispatch shows it.
(cd "$root" && bun install --frozen-lockfile >/dev/null)
bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --dest "$work/plugin" >/dev/null
bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --dest "$evidence/model-gateway" --cache-dir "$work/model-gateway-cache" >/dev/null ||
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
  "cd '$work' && OMP_PROFILE='$profile' '$work/legion' controller start --config '$work/controller.yaml' 2>'$evidence/logs/controller.stderr'; sleep 3600"
until_true 300 "controllerLocator in the state" sh -c "'$work/legion' state --json --config '$work/legion.yaml' | jq -e '.controllerLocator.sessionId != null' >/dev/null"
controller_session=$(daemon_state | jq -r .controllerLocator.sessionId)
note "controllerLocator $(daemon_state | jq -c .controllerLocator)"
wait_for_worker "$tree3" architect
drive_spec "$tree3"
wait_for_worker "$tree3" planner
killed=" "
kills=0
until issue_phase "$tree3" held >/dev/null 2>&1; do
  [ "$kills" -lt 8 ] || fail "$tree3 was not held after $kills killed planner launches"
  until_true 600 "a new planner pod on $tree3" sh -c \
    "inc=\$('$work/legion' state --json --config '$work/legion.yaml' | jq -r --arg i '$tree3' '.issues[\$i].workers.planner.claim.locator.incarnation // empty'); [ -n \"\$inc\" ] && ! grep -qF \" \$inc \" <<<'$killed'"
  uid=$(claim_pod_uid "$tree3" planner)
  pod=$(claim_sandbox "$tree3" planner)
  driver_action kill "$uid"
  op exec "$pod" -c worker -- sh -c 'kill 1' >/dev/null 2>&1 || true
  killed="$killed$uid "
  kills=$((kills + 1))
  note "killed planner launch $kills of $tree3 (uid $uid)"
  until_true 600 "$tree3 to be held or its planner relaunched" sh -c \
    "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg i '$tree3' --arg u '$uid' '.issues[\$i].phase == \"held\" or ((.issues[\$i].workers.planner.claim.locator.incarnation // \"\") != \$u)' >/dev/null"
done
note "$tree3 is held after $kills killed planner launches"
# The held notice reaches the controller: its session, on this machine, holds the Envoy delivery.
controller_notice() {
  local file
  for file in "$HOME/.omp/profiles/$profile/agent/sessions"/*/*.jsonl; do
    [ -f "$file" ] || continue
    grep -F '"customType":"envoy-message"' "$file" | grep -qF "$(notice_needle held "$tree3")" && return 0
  done
  return 1
}
until_true 300 "the held notice for $tree3 to reach the controller session $controller_session" controller_notice
note "the controller session $controller_session received the held notice for $tree3"
interests_sample "$check"
before_status=$(dispatch_get "issues/$tree3" | jq -r .status)
out=$("$work/legion" status "$tree3" backlog --operator-token-file "$work/operator-token" --config "$work/legion.yaml" 2>&1) || fail "legion status $tree3 backlog from the operator shell: $out"
until_true 120 "Dispatch to show $tree3 in backlog" dispatch_status_is "$tree3" backlog
note "legion status $tree3 backlog from the operator shell, with the operator bearer: Dispatch moved $tree3 from $before_status to $(dispatch_get "issues/$tree3" | jq -r .status)"
until_true 600 "$tree3's pods to be gone" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get pods -l 'legion.dev/project=$run_label,legion.dev/tree=$tree3' -o name) && [ -z \"\$out\" ]"
pass

begin "done"
wait_for_worker "$tree1" merger
send_agent "$tree1" merger "Stage 4b proof READY operation: verify pull request #$pr_number is ready to merge and call the legion tool's handoff_complete with ready true."
wait_for_phase "$tree1" awaiting_merge 900
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
note "$repo#$pr_number merged by the proof human; the production check and the sign-off closed $tree1"
pass

begin node-release
# Tree 1 lingers (linger_hours 0.3): after the pool's consolidateAfter, its node is gone while its
# Sandboxes still exist Suspended and its volume is Bound.
node=$(jq -r 'select(.object.kind == "Pod") | .object | select(.metadata.labels["legion.dev/tree"] == "'"$tree1"'") | .spec.nodeName // empty' "$evidence/pod-watch.json" | tail -1)
until_true 1500 "tree 1's node $node to be released" sh -c "out=\$(timeout 120 kubectl --context '$operator' get node '$node' -o name --ignore-not-found) && [ -z \"\$out\" ]"
modes=$(op get sandboxes -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].spec.operatingMode}')
bound=$(op get pvc -l "legion.dev/project=$run_label,legion.dev/tree=$tree1" -o jsonpath='{.items[*].status.phase}')
if [ -z "$modes" ] || grep -qv Suspended <<<"$(tr ' ' '\n' <<<"$modes")"; then fail "tree 1's Sandboxes are '$modes' after its node went, want all Suspended"; fi
[ "$bound" = Bound ] || fail "tree 1's volume is '$bound' after its node went, want Bound"
note "at $(date -u +%FT%TZ) node $node is gone; tree 1's Sandboxes Suspended ($modes), its volume Bound"
pass

begin close
until_true 1500 "tree 1 to close at linger expiry" sh -c \
  "out=\$(timeout 120 kubectl --context '$operator' -n '$namespace' get sandboxes,pvc -l 'legion.dev/project=$run_label,legion.dev/tree=$tree1' -o name) && [ -z \"\$out\" ]"
note "tree 1's Sandboxes and tree volume are deleted"
pass

begin re-admission
set_status "$tree1" todo
until_true 900 "the re-admitted tree 1 to report workspace-lost and relaunch a fresh architect" sh -c \
  "grep -c 'workspace-lost:' '$daemon_log' | grep -q '^[1-9]'"
wait_for_worker "$tree1" architect
pod=$(tree_pod "$tree1")
recovered=$(pod_exec "$pod" cat "/legion/workspaces/$repo/${tree1,,}/.legion/workspace-recovered.json")
jq -e --arg b "legion/$tree1" 'tostring | contains($b)' <<<"$recovered" >/dev/null || fail "the recovery marker does not name legion/$tree1: $recovered"
lost=$(grep -c 'workspace-lost:' "$daemon_log")
[ "$lost" = 1 ] || fail "workspace-lost was reported $lost times, want exactly once"
note "one workspace-lost, then a fresh session whose workspace holds .legion/workspace-recovered.json naming legion/$tree1"
pass

begin operator-close
# The operator's `legion claims close` on the Sandbox runtime (#1337). A workflow issue's tree is the
# workflow's to close: the close of re-admitted tree 1's live root is refused 409, and its claims,
# Sandboxes and pods are untouched. A tree no workflow issue backs, which the operator spawns here,
# closes with its worker live: the root and the worker are retired, and the tree's Sandboxes, pods
# and volume are gone.
claims_cli() { "$work/legion" claims "$@" --config "$work/legion.yaml" --operator-token-file "$work/operator-token"; }
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
set_status "$tree1" backlog
optree="S4BOP-$$"
printf '%s\n' "You are a Stage 4b operator-close fixture, the root of a tree no workflow issue backs. Do nothing and wait." >"$work/op-architect.md"
printf '%s\n' "You are a Stage 4b operator-close fixture, a worker of that tree. Do nothing and wait." >"$work/op-worker.md"
op_root=$(claims_cli spawn --json --tree "$optree" --issue "$optree" --role architect --prompt-file "$work/op-architect.md" | jq -er .token) ||
  fail "the operator could not spawn the root of $optree"
op_worker=$(claims_cli spawn --json --tree "$optree" --issue "$optree-1" --role implementer --prompt-file "$work/op-worker.md" | jq -er .token) ||
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
every=$(unchecked_sandbox_pods)
[ -z "$every" ] || fail "Sandbox pods were never shape-checked: $every"
unames=$(cat "$evidence"/pods/*.txt | sed -n 's/^uname=//p' | sort -u | tr '\n' ' ')
note "$(wc -l <"$evidence/pods-checked.txt") Sandbox pods shape-checked (gVisor uname -r: $unames)"
grep -h '"init"' "$evidence"/pods/*.txt | jq -s -c 'map({role, init: [.init[] | select(.name == "workspace-fetch") | {started, finished}]})' >"$evidence/workspace-fetch.json"
note "workspace-fetch per pod (started, finished) and node ephemeral-storage use: $evidence/workspace-fetch.json, $evidence/pods/"
pass

begin pod-watch-verdict
stop_pid "$shape_pid"
shape_pid=
if ! pod_watch_verdict "$evidence/pod-watch.json" "$evidence/driver-actions.txt" "$daemon_log"; then
  fail "the pod watch saw terminations the run cannot account for: $(tr '\n' ';' <"$work/pod-watch-verdict.txt")"
fi
pressure=$(jq -R -c 'fromjson? | select(.pressure? and (.pressure | index("MemoryPressure")))' "$evidence/node-memory.txt")
[ -z "$pressure" ] || fail "a node of the run reported MemoryPressure: $(head -3 <<<"$pressure" | tr '\n' ' ')"
note "no node of the run reported MemoryPressure ($(grep -c '"pressure"' "$evidence/node-memory.txt") samples)"
jq -c 'select(.object.kind == "Pod") | .object' "$evidence/pod-watch.json" | tail -1 |
  jq -c '{kind: "Pod", object: (.status.containerStatuses[0].state = {terminated: {reason: "OOMKilled", exitCode: 137}})}' >"$work/injected.json"
cat "$evidence/pod-watch.json" "$work/injected.json" >"$evidence/controls/pod-watch-with-oom.json"
expect_failure pod-watch-synthetic-oom pod_watch_verdict "$evidence/controls/pod-watch-with-oom.json" "$evidence/driver-actions.txt" "$daemon_log"
{ cat "$daemon_log"; jq -cn '{msg: "supervise: process died", incarnation: "00000000-e2e4-control", observed: "gone", detail: "synthetic"}'; } >"$evidence/controls/daemon-log-with-death.log"
expect_failure pod-watch-synthetic-death pod_watch_verdict "$evidence/pod-watch.json" "$evidence/driver-actions.txt" "$evidence/controls/daemon-log-with-death.log"
pass

begin hygiene
stop_pid "$daemon_pid"
daemon_pid=
teardown
namespace_clean
delete_consumers
left=$(nats_stream consumers "$nats_url" "$stream" "$consumers_prefix")
[ -z "$left" ] || fail "the run's durable consumers remain on production NATS: $left"
pass

begin production-audit
audit_verdict_ok=
if production_audit; then audit_verdict_ok=1; fi
[ -n "$audit_verdict_ok" ] || fail "the run wrote outside LEGSMOKE or subscribed outside it: $evidence/production-issues-touched-outside.json, $evidence/production-interests-outside.json"
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
# The unanswered-sample rule: a blip passes, a listener that stops answering fails.
printf '%s\n' "t1 control ok" "t2 control error unreachable: timeout" "t3 control ok" "t4 control error answered 503: busy" \
  "t5 control error answered 503: busy" "t6 control absent" >"$evidence/controls/interests-outcomes-transient.txt"
printf '%s\n' "t1 control ok" "t2 control error unreachable: timeout" "t3 control error answered 503: busy" \
  "t4 control error unreachable: timeout" "t5 control ok" >"$evidence/controls/interests-outcomes-sustained.txt"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-transient.txt")" = "[]" ] ||
  fail "two unanswered samples in a row failed the sample rule, which only three in a row do"
[ "$(interests_unanswered "$evidence/controls/interests-outcomes-sustained.txt" | jq length)" = 1 ] ||
  fail "three unanswered samples in a row passed the sample rule"
note "the unanswered-sample rule's controls: two failures in a row pass, three fail; this run had $(grep -c ' error ' "$evidence/interests-outcomes.txt") unanswered samples of $(grep -c . "$evidence/interests-outcomes.txt")"
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
