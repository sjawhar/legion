#!/usr/bin/env bash
# scripts/kind-smoke/checkpoints.sh <name> — one named checkpoint against a running instance.
# Reads the records under the instance's state directory, daemon state through the port-forward
# (the redacted GET /legion/v1/state), Dispatch through the instance's scratch server, and pods
# through kubectl with the instance kubeconfig. Prints exactly one line:
#   CHECKPOINT <name> OK: <detail>                      exit 0
#   CHECKPOINT <name> FAILED: <reason>                  exit 1
#   CHECKPOINT <name> SKIPPED-BLOCKED: <what is lacking> exit 3
# A mode that cannot supply the needed signal blocks before any network call; never a false green.
set -euo pipefail
source "${BASH_SOURCE[0]%/*}/lib.sh"

checkpoints="admitted architect-pod spec-posted tree-moved kill-pod-resume pod-hygiene worker-cap done"
checkpoint="${1:-}"
case " $checkpoints " in
  *" $checkpoint "*) ;;
  *)
    printf 'usage: checkpoints.sh <%s>\n' "$(printf '%s' "$checkpoints" | tr ' ' '|')" >&2
    exit 2
    ;;
esac

poll_timeout_line=0   # a timeout is reported by the FAILED line, with the last observation
ok() { printf 'CHECKPOINT %s OK: %s\n' "$checkpoint" "$*"; exit 0; }
failed() { printf 'CHECKPOINT %s FAILED: %s\n' "$checkpoint" "$*" >&2; exit 1; }
blocked() { printf 'CHECKPOINT %s SKIPPED-BLOCKED: %s\n' "$checkpoint" "$*" >&2; exit 3; }

# ---- records and budgets -----------------------------------------------------------------------

smoke_init
port_base="$(record_require port-base)"
port_nats="$(smoke_port nats)"
port_listener="$(smoke_port listener)"
port_dispatch="$(smoke_port dispatch)"
port_postgres="$(smoke_port postgres)"
port_daemon="$(smoke_port daemon)"
gateway="$(record_require gateway)"
project="$(record_require project)"
dispatch_project="$(record_require dispatch-project)"
root_issue="$(record_require root-issues | head -n1)"
controller="$(record_require controller)"
github_ingress="$(record_require github-ingress)"
session_store="$(record_require session-store)"
worker_cap="$(record_require worker-cap)"
root_issue_count="$(record_require root-issue-count)"
repo="$(record_require repo)"
[ -s "$(record_require kubeconfig)" ] || fail "kubeconfig $(record_read kubeconfig) is missing: run scripts/kind-smoke/up.sh first"
declare -A budget=(
  [admitted]="${SMOKE_WAIT_ADMITTED:-180}"
  [architect-pod]="${SMOKE_WAIT_ARCHITECT_POD:-600}"
  [spec-posted]="${SMOKE_WAIT_SPEC_POSTED:-1200}"
  [tree-moved]="${SMOKE_WAIT_TREE_MOVED:-900}"
  [kill-phase]="${SMOKE_WAIT_KILL_PHASE:-3600}"
  [kill-resume]="${SMOKE_WAIT_KILL_RESUME:-600}"
  [kill-complete]="${SMOKE_WAIT_KILL_COMPLETE:-1800}"
  [cap-queue]="${SMOKE_WAIT_CAP_QUEUE:-1800}"
  [cap-promote]="${SMOKE_WAIT_CAP_PROMOTE:-1800}"
  [done]="${SMOKE_WAIT_DONE:-7200}"
)

# ---- readers -----------------------------------------------------------------------------------
# Each reader fills a global; a predicate that cannot read its inputs fails the checkpoint at once.

state_doc=""
read_state() {
  state_doc="$(daemon_state 2>/dev/null)" ||
    failed "daemon state unreachable on 127.0.0.1:$port_daemon (is the port-forward alive? see $state/logs/port-forward.log)"
}
sq() { printf '%s' "$state_doc" | jq -r "$@"; } # sq FILTER [ARGS…] — query the last state read
issue_json() { dispatch_get "issues/$1" 2>/dev/null || failed "Dispatch did not answer GET /api/v1/issues/$1 (see $state/logs/dispatch.log)"; }
issue_status() { issue_json "$1" | jq -r '.status // empty'; }
children_keys() { # children_keys ROOT — one key per line
  local listed
  listed="$(dispatch_get "issues?project=$dispatch_project&parent=$1" 2>/dev/null | jq -r '.[].key' || true)"
  if [ -n "$listed" ]; then printf '%s\n' "$listed"; else issue_json "$1" | jq -r '.children[]?.key // empty'; fi
}
pod_json() { kc get pod "$1" -o json 2>/dev/null || echo '{}'; }
pods_json() { if [ $# -gt 0 ]; then kc get pods -l "$1" -o json 2>/dev/null; else kc get pods -o json 2>/dev/null; fi || echo '{"items":[]}'; }
pvc_phase() { kc get pvc "$1" -o json 2>/dev/null | jq -r '.status.phase // empty'; }
controller_expected() { [[ "$controller" == tmux\ * ]]; }

last=""                # the last observation, printed on a timeout
lifecycle_statuses=" todo in_progress testing needs_review retro done "

# ---- admitted -----------------------------------------------------------------------------------

try_admitted() {
  local status tree ctl
  status="$(issue_status "$root_issue")"
  read_state
  tree="$(sq --arg k "$root_issue" '.trees[$k].status // empty')"
  ctl="$(sq '.controllerLocator.external // false')"
  case "$lifecycle_statuses" in
    *" $status "*) ;;
    *) last="$root_issue has Dispatch status '${status:-<none>}', not a released one (todo or later)"; return 1 ;;
  esac
  [ -n "$tree" ] || { last="no tree recorded for $root_issue in daemon state (status=$status)"; return 1; }
  if controller_expected && [ "$ctl" != true ]; then
    last="the run has a controller pane but daemon state has no external controllerLocator yet"
    return 1
  fi
  last="$root_issue status=$status tree=$tree controller=$(controller_expected && printf external || printf none)"
}
cp_admitted() {
  poll "${budget[admitted]}" "$root_issue to be admitted" try_admitted || failed "$last"
  ok "$last"
}

# ---- architect-pod ------------------------------------------------------------------------------

try_architect_pod() {
  local status tree_status runtime pod pvc gen pod_doc phase
  status="$(issue_status "$root_issue")"
  read_state
  tree_status="$(sq --arg k "$root_issue" '.trees[$k].status // empty')"
  runtime="$(sq --arg k "$root_issue" '.trees[$k].locator.runtime // empty')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  pvc="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  gen="$(sq --arg k "$root_issue" '.trees[$k].generation // empty')"
  [ "$status" = in_progress ] || { last="$root_issue has Dispatch status '${status:-<none>}', expected in_progress"; return 1; }
  [ "$tree_status" = active ] || { last="tree $root_issue is '${tree_status:-<none>}', expected active"; return 1; }
  [ "$runtime" = kubernetes ] || { last="tree $root_issue has locator runtime '${runtime:-<none>}', expected kubernetes"; return 1; }
  [ -n "$pod" ] && [ -n "$pvc" ] || { last="tree $root_issue locator lacks podName/pvcName"; return 1; }
  pod_doc="$(pod_json "$pod")"
  phase="$(printf '%s' "$pod_doc" | jq -r '.status.phase // empty')"
  [ "$phase" = Running ] || { last="pod $pod is '${phase:-absent}', expected Running"; return 1; }
  local want got
  for want in "legion.dev/project=$project" "legion.dev/role=architect" "legion.dev/generation=$gen" "legion.dev/tree=$(k8s_slug "$root_issue")" "legion.dev/issue=$(k8s_slug "$root_issue")"; do
    got="$(printf '%s' "$pod_doc" | jq -r --arg l "${want%%=*}" '.metadata.labels[$l] // empty')"
    [ "$got" = "${want#*=}" ] || { last="pod $pod label ${want%%=*} is ${got:-absent}, state says ${want#*=}"; return 1; }
  done
  [ "$(pvc_phase "$pvc")" = Bound ] || { last="claim $pvc is '$(pvc_phase "$pvc")', expected Bound"; return 1; }
  printf '%s' "$pod_doc" | jq -e --arg c "$pvc" '[.spec.volumes[]?.persistentVolumeClaim.claimName // empty] | index($c) != null' >/dev/null ||
    { last="pod $pod does not mount claim $pvc"; return 1; }
  last="$root_issue in_progress; tree active generation $gen; pod $pod Running with its legion.dev labels; claim $pvc Bound and mounted"
}
cp_architect_pod() {
  poll "${budget[architect-pod]}" "the root architect pod of $root_issue" try_architect_pod || failed "$last"
  ok "$last"
}

# ---- spec-posted ---------------------------------------------------------------------------------

try_spec_posted() {
  local doc artifact children worker
  doc="$(issue_json "$root_issue")"
  artifact="$(printf '%s' "$doc" | jq -r '.primary_artifact_id // empty')"
  read_state
  # hard failures: the gate is off, so nothing may be registered or requested
  [ "$(sq --arg k "$root_issue" '.gates[$k] // empty')" = "" ] ||
    failed "$root_issue registered a design gate although the run has gates.design: off"
  dispatch_get "issues/$root_issue/asks" 2>/dev/null | jq -e '[.[]? | select(.kind == "approval" and .state == "open")] | length == 0' >/dev/null ||
    failed "an approval request is open on $root_issue although gates.design is off"
  [ -n "$artifact" ] || { last="$root_issue has no primary spec document yet"; return 1; }
  children="$(children_keys "$root_issue" | paste -sd, - | sed 's/,/, /g')"
  worker="$(sq --arg k "$root_issue" '[.roles[] | select(.issue == $k and .role != "architect" and .locator != null) | .role] | first // empty')"
  if [ -n "$children" ]; then last="spec $artifact posted; gate off, none registered; children: $children"
  elif [ -n "$worker" ]; then last="spec $artifact posted; gate off, none registered; phase worker: $worker"
  else last="spec $artifact posted but the tree has not moved past it (no child issue, no phase worker claimed on $root_issue)"; return 1
  fi
}
cp_spec_posted() {
  poll "${budget[spec-posted]}" "the spec of $root_issue and the tree moving past it" try_spec_posted || failed "$last"
  ok "$last"
}

# ---- tree-moved ----------------------------------------------------------------------------------

try_tree_moved() {
  local children child claims token role issue pod phase
  read_state
  children="$(children_keys "$root_issue")"
  for child in $children; do
    [ "$(sq --arg c "$child" '.trees[$c] // empty')" = "" ] || failed "child $child is admitted as a tree of its own (LEGION-57)"
    sq -e --arg c "$child" '(.admission.queue + .admission.active) | index($c) == null' >/dev/null ||
      failed "child $child is admitted as a tree of its own (LEGION-57): it is in the admission queue or active set"
  done
  # a sub-architect on a released child, or a phase worker on the root itself
  claims="$(sq --arg k "$root_issue" --argjson children "$(printf '%s\n' $children | jq -R . | jq -sc .)" \
    '.roles | to_entries[] | select(.value.locator.podName != null) | select((.value.role == "architect" and (.value.issue as $i | $children | index($i) != null)) or (.value.issue == $k and .value.role != "architect")) | "\(.key) \(.value.role) \(.value.issue) \(.value.locator.podName)"')"
  [ -n "$claims" ] || { last="no sub-architect on a child of $root_issue and no phase worker on it holds a claim with a pod (children: ${children:-none})"; return 1; }
  while read -r token role issue pod; do
    phase="$(pod_json "$pod" | jq -r '.status.phase // empty')"
    if [ "$phase" = Running ]; then
      if [ "$role" = architect ]; then last="sub-architect on $issue pod $pod Running; no child is a tree of its own"
      else last="phase worker $role on $issue pod $pod Running; no child is a tree of its own"; fi
      return 0
    fi
    last="claim $token names pod $pod, which is '${phase:-absent}', not Running"
  done <<<"$claims"
  return 1
}
cp_tree_moved() {
  poll "${budget[tree-moved]}" "the tree of $root_issue to move (a sub-architect or phase worker pod)" try_tree_moved || failed "$last"
  ok "$last"
}

# ---- kill-pod-resume ----------------------------------------------------------------------------
# The target is the root architect's pod (SMOKE_KILL_ROLE=architect, the one value this child
# supports): the daemon resurrects a crashed root by itself on its next resync probe, with
# `--resume` of the recorded session file, whereas a crashed phase worker is only retired until
# something addresses it (spec Design, "Which agent the kill targets"). The kill is a real crash —
# SIGKILL to the container's process from the kind node's own PID namespace — never a graceful
# delete, which would make the root self-report its exit and take the re-admission path instead.

arch_token="$(role_token "$project" "$root_issue" architect)"
tree_claims() { # tree_claims → "token generation role issue" for every non-root claim on the tree, sorted
  local children
  children="$(children_keys "$root_issue" | jq -R . | jq -sc .)"
  sq --arg k "$root_issue" --argjson children "$children" \
    '.roles | to_entries[] | select(.value.issue != null) | select((.value.issue == $k and .value.role != "architect") or (.value.issue as $i | $children | index($i) != null)) | "\(.key) \(.value.generation // 0) \(.value.role) \(.value.issue)"' | sort
}
tree_statuses() { # tree_statuses → "KEY status" for the root and every child, sorted
  local k
  { for k in $root_issue $(children_keys "$root_issue"); do printf '%s %s\n' "$k" "$(issue_status "$k")"; done; } | sort
}
try_kill_target() { # the root is active, ready-confirmed, Running, and some worker or sub-architect holds a claim with a pod
  local tree_status ready pod phase live
  read_state
  tree_status="$(sq --arg k "$root_issue" '.trees[$k].status // empty')"
  ready="$(sq --arg k "$root_issue" '.trees[$k].readyConfirmedAt // empty')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  [ "$tree_status" = active ] || { last="tree $root_issue is '${tree_status:-<none>}', expected active"; return 1; }
  [ -n "$ready" ] || { last="tree $root_issue is not ready-confirmed yet"; return 1; }
  [ -n "$pod" ] || { last="tree $root_issue has no pod locator"; return 1; }
  phase="$(pod_json "$pod" | jq -r '.status.phase // empty')"
  [ "$phase" = Running ] || { last="root pod $pod is '${phase:-absent}', expected Running"; return 1; }
  live="$(tree_claims | awk '{print $1}' | while read -r t; do [ "$(sq --arg t "$t" '.roles[$t].locator.podName // empty')" != "" ] && echo "$t"; done | paste -sd, -)"
  [ -n "$live" ] || { last="no phase worker or sub-architect holds a claim with a pod on the tree of $root_issue yet (the kill must land mid-phase)"; return 1; }
  gen0="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  session0="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  pod0="$pod"
  uid0="$(sq --arg k "$root_issue" '.trees[$k].locator.podUid // empty')"
  pvc0="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  file0="$(sq --arg k "$root_issue" '.trees[$k].locator.ompSessionFile // empty')"
  claims0="$(tree_claims)"
  statuses0="$(tree_statuses)"
  last="mid-phase: root pod $pod0 generation $gen0, live claims $live"
}
apply_legion_177_workaround() {
  [ "${SMOKE_LEGION_177_WORKAROUND:-1}" = 1 ] || { note "WORKAROUND LEGION-177 skipped (SMOKE_LEGION_177_WORKAROUND=${SMOKE_LEGION_177_WORKAROUND})"; workaround="off"; return 0; }
  local out status=0
  out="$(kc exec "$pod0" -c worker -- git --git-dir="/legion/repos/github.com/$repo/.git" config --unset credential.interactive 2>&1)" || status=$?
  case "$status" in
    0) note "WORKAROUND LEGION-177 applied"; workaround="applied" ;;
    5) note "WORKAROUND LEGION-177 applied (credential.interactive was already unset)"; workaround="applied (already unset)" ;;
    *) failed "could not apply the LEGION-177 workaround in pod $pod0 (git config --unset credential.interactive exited $status): $out" ;;
  esac
}
crash_root_pod() { # SIGKILL from the node's PID namespace; fall back to a forced delete
  local container node hostpid
  container="$(pod_json "$pod0" | jq -r '.status.containerStatuses[]? | select(.name == "worker") | .containerID // empty' | sed 's|^containerd://||')"
  node="$(kind get nodes --name "$cluster" 2>/dev/null | head -n1 || true)"
  if [ -n "$container" ] && [ -n "$node" ] &&
    hostpid="$(docker exec "$node" crictl inspect -o go-template --template '{{.info.pid}}' "$container" 2>/dev/null)" &&
    [[ "$hostpid" =~ ^[0-9]+$ ]] && [ "$hostpid" -gt 1 ] &&
    docker exec "$node" kill -9 "$hostpid" 2>/dev/null; then
    kill_method="docker exec $node kill -9 $hostpid (container ${container:0:12})"
  else
    kc delete pod "$pod0" --grace-period=0 --force >/dev/null 2>&1 || failed "neither a SIGKILL through the kind node nor kubectl delete --grace-period=0 --force removed pod $pod0"
    kill_method="kubectl delete pod $pod0 --grace-period=0 --force (fallback)"
  fi
}
resume_arg() { pod_json "$1" | jq -r '[.spec.containers[]? | select(.name == "worker") | .command[]? | select(startswith("--resume="))] | join("\n")'; }
init_failure() { # init_failure POD → the failed init container's reason and log tail, or nothing
  local doc
  doc="$(pod_json "$1")"
  printf '%s' "$doc" | jq -e '[.status.initContainerStatuses[]? | select((.state.terminated.exitCode // 0) != 0 or (.state.waiting.reason // "") == "CrashLoopBackOff" or (.lastState.terminated.exitCode // 0) != 0)] | length > 0' >/dev/null 2>&1 || return 1
  kc logs "$1" -c workspace-init --tail=20 2>&1 || true
}
try_kill_resumed() {
  local gen pod phase claims statuses initlog
  read_state
  gen="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  if [ "$gen" = "$gen0" ]; then
    claims="$(tree_claims)"
    statuses="$(tree_statuses)"
    if [ "$claims" != "$claims0" ] || [ "$statuses" != "$statuses0" ]; then
      failed "the tree moved on without a generation change: recorded generation $gen0, current $gen — the kill ($kill_method) did not land"
    fi
    last="tree $root_issue still at generation $gen0 (pod ${pod:-none}); waiting for the resync probe to resurrect it"
    return 1
  fi
  [ -n "$pod" ] && [ "$pod" != "$pod0" ] || { last="tree $root_issue is at generation $gen but its locator still names $pod0"; return 1; }
  phase="$(pod_json "$pod" | jq -r '.status.phase // empty')"
  if initlog="$(init_failure "$pod")"; then
    failed "replacement pod $pod: its init container failed (LEGION-177 without the workaround? SMOKE_LEGION_177_WORKAROUND=${SMOKE_LEGION_177_WORKAROUND:-1}); workspace-init log tail: $initlog"
  fi
  case "$phase" in
    Running) ;;
    Failed | Succeeded) failed "replacement pod $pod left Running ($phase) before registering; worker log tail: $(kc logs "$pod" -c worker --tail=50 2>&1)" ;;
    *) last="replacement pod $pod is '${phase:-absent}', waiting for Running"; return 1 ;;
  esac
  gen1="$gen"
  pod1="$pod"
  last="replacement pod $pod1 Running at generation $gen1"
}
try_kill_registered() { # the replacement reached /process/ready on the new generation
  local gen ready phase
  read_state
  gen="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  [ "$gen" = "$gen1" ] || failed "generation advanced again from $gen1 to $gen before the replacement registered (launch failures?)"
  phase="$(pod_json "$pod1" | jq -r '.status.phase // empty')"
  case "$phase" in
    Failed | Succeeded) failed "replacement pod $pod1 left Running ($phase) before registering — a different session was refused (409), or the boot failed; worker log tail: $(kc logs "$pod1" -c worker --tail=50 2>&1)" ;;
  esac
  ready="$(sq --arg k "$root_issue" '.trees[$k].readyConfirmedAt // empty')"
  [ -n "$ready" ] || { last="replacement pod $pod1 Running at generation $gen1 but not ready-confirmed yet"; return 1; }
  session1="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  last="replacement registered: generation $gen1 ready-confirmed, session ${session1:-<none>}"
}
try_tree_moved_after() {
  local claims statuses
  read_state
  claims="$(tree_claims)"
  statuses="$(tree_statuses)"
  if [ "$claims" != "$claims0" ]; then moved="claims changed: $(diff <(printf '%s\n' "$claims0") <(printf '%s\n' "$claims") | grep '^[<>]' | tr '\n' ';' | tr -s ' ')"; return 0; fi
  if [ "$statuses" != "$statuses0" ]; then moved="status changed: $(diff <(printf '%s\n' "$statuses0") <(printf '%s\n' "$statuses") | grep '^[<>]' | tr '\n' ';' | tr -s ' ')"; return 0; fi
  last="the tree of $root_issue has not moved since the resurrection (claims: $(printf '%s' "$claims" | awk '{print $3"/"$4"@"$2}' | paste -sd, -); statuses: $(printf '%s' "$statuses" | paste -sd, -))"
  return 1
}
cp_kill_pod_resume() {
  local role="${SMOKE_KILL_ROLE:-architect}" resume
  [ "$role" = architect ] ||
    blocked "SMOKE_KILL_ROLE=$role is not supported: a crashed phase worker is only retired until something addresses it, so only the root architect (SMOKE_KILL_ROLE=architect) is resurrected by the daemon itself"
  poll "${budget[kill-phase]}" "the tree of $root_issue to be mid-phase" try_kill_target || failed "$last"
  [ -n "$session0" ] || failed "daemon state records no session id for $arch_token"
  [ -n "$file0" ] || failed "daemon state records no ompSessionFile for tree $root_issue; the replacement could not be checked for --resume"
  [ -n "$pvc0" ] || failed "daemon state records no pvcName for tree $root_issue"
  apply_legion_177_workaround
  crash_root_pod
  poll "${budget[kill-resume]}" "the replacement root pod" try_kill_resumed || failed "$last"
  [ "$gen1" = "$((gen0 + 1))" ] || failed "generation advanced from $gen0 to $gen1, expected $((gen0 + 1))"
  [ "$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')" = "$pvc0" ] ||
    failed "replacement pod $pod1 has claim '$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')', recorded $pvc0"
  pod_json "$pod1" | jq -e --arg c "$pvc0" '[.spec.volumes[]?.persistentVolumeClaim.claimName // empty] | index($c) != null' >/dev/null ||
    failed "replacement pod $pod1 does not mount claim $pvc0"
  resume="$(resume_arg "$pod1")"
  [ -n "$resume" ] || failed "replacement pod $pod1 carries no --resume argument"
  [ "$(printf '%s\n' "$resume" | wc -l)" = 1 ] || failed "replacement pod $pod1 carries more than one --resume argument: $(printf '%s' "$resume" | paste -sd' ' -)"
  [ "${resume#--resume=}" = "$file0" ] || failed "replacement pod $pod1 $resume does not name the recorded session file $file0"
  poll "${budget[kill-resume]}" "the replacement to register" try_kill_registered || failed "$last"
  [ "$session1" = "$session0" ] ||
    failed "the replacement registered session '${session1:-<none>}', recorded $session0 (a different agent); worker log tail: $(kc logs "$pod1" -c worker --tail=50 2>&1)"
  poll "${budget[kill-complete]}" "the tree of $root_issue to keep working" try_tree_moved_after || failed "$last"
  ok "$root_issue architect pod $pod0 → $pod1 generation $gen0→$gen1 (kill: $kill_method; LEGION-177 workaround $workaround) session $session0 unchanged; $resume; tree moved afterwards — $moved"
}

# ---- pod-hygiene ---------------------------------------------------------------------------------

quantity_jq='def quantity: if type == "number" then . else (capture("^(?<n>[0-9.]+)(?<u>[A-Za-z]*)$") | (.n | tonumber) * ({"":1,"m":0.001,"k":1000,"M":1000000,"G":1000000000,"T":1000000000000,"Ki":1024,"Mi":1048576,"Gi":1073741824,"Ti":1099511627776}[.u] // (error("unknown quantity unit " + .u)))) end;'
cp_pod_hygiene() {
  local pods checked profile_problem secret_problem env_problem
  pods="$(pods_json)"
  checked="$(printf '%s' "$pods" | jq '[.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null)] | length')"
  [ "$checked" -gt 0 ] || failed "no legion.dev/project pods are running; run architect-pod first"
  # 1. the daemon is never labelled legion.dev/project (the orphan sweep would delete it)
  kc get deploy legion-daemon-demo -o json 2>/dev/null | jq -e '.spec.template.metadata.labels | has("legion.dev/project") | not' >/dev/null ||
    failed "the daemon Deployment's pod template carries legion.dev/project"
  printf '%s' "$pods" | jq -e '[.items[] | select(.metadata.labels["app.kubernetes.io/name"] == "legion-daemon" and .metadata.labels["legion.dev/project"] != null)] | length == 0' >/dev/null ||
    failed "the daemon pod carries legion.dev/project"
  # 2. every Legion pod's containers carry exactly its role profile's requests and limits
  profile_problem="$(printf '%s' "$pods" | jq -r --slurpfile profiles "$records/profiles.json" "$quantity_jq"'
    $profiles[0] as $p
    | [.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null)
      | .metadata.name as $pod | (.metadata.labels["legion.dev/role"] // "") as $role | ($p.role_profiles[$role]) as $profile
      | if $profile == null then "pod \($pod) has role \(if $role == "" then "<none>" else $role end), which names no profile" else
        ((.spec.containers + (.spec.initContainers // []))[] | .name as $c
          | (["requests","limits"][] as $kind | (["cpu","memory","ephemeral-storage"][] as $field
            | (.resources[$kind][$field] // "0") as $actual | ($p.resources[$profile][$kind][$field]) as $want
            | select(($actual | quantity) != ($want | quantity))
            | "pod \($pod) container \($c) \($kind).\($field) is \($actual), profile \($profile) says \($want)"))) end] | first // empty')"
  [ -z "$profile_problem" ] || failed "$profile_problem"
  # 3. no secret value the rig wrote appears in any container's env, command, or args — compared by value, never printed
  local secrets=() f line
  for f in dispatch-token envoy-token operator-token postgres-password; do [ -s "$state/secrets/$f" ] && secrets+=("$(<"$state/secrets/$f")"); done
  if [ -f "$state/overlay/secrets/providers.env" ]; then
    while IFS= read -r line; do [ -n "${line#*=}" ] && [ "${line#*=}" != "$line" ] && secrets+=("${line#*=}"); done <"$state/overlay/secrets/providers.env"
  fi
  [ "${#secrets[@]}" -gt 0 ] || failed "no secret values recorded under $state/secrets; run up.sh first"
  local strings s secret
  strings="$(printf '%s' "$pods" | jq -r '.items[] | .metadata.name as $pod | (.spec.containers + (.spec.initContainers // []))[] | .name as $c | ((.env[]? | "\($pod)\t\($c)\tenv \(.name)\t\(.value // "")"), (.command[]? | "\($pod)\t\($c)\tcommand\t\(.)"), (.args[]? | "\($pod)\t\($c)\targs\t\(.)"))')"
  while IFS=$'\t' read -r pod c where value; do
    for secret in "${secrets[@]}"; do
      [[ "$value" == *"$secret"* ]] && failed "pod $pod container $c $where contains a secret value"
    done
  done <<<"$strings"
  # 4. PID 1 of every Running Legion pod (the shim) carries no provider key and no secret value
  local running p environ name
  running="$(printf '%s' "$pods" | jq -r '.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null and .status.phase == "Running") | .metadata.name')"
  for p in $running; do
    environ="$(kc exec "$p" -c worker -- cat /proc/1/environ 2>/dev/null | tr '\0' '\n' || true)"
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      name="${line%%=*}"
      case "$name" in ANTHROPIC_API_KEY | GEMINI_API_KEY | OPENAI_API_KEY) failed "pod $p PID 1 environment carries $name" ;; esac
      for secret in "${secrets[@]}"; do
        [[ "${line#*=}" == *"$secret"* ]] && failed "pod $p PID 1 environment carries a secret value in $name"
      done
    done <<<"$environ"
  done
  ok "$checked pods checked; profiles match; no secret in env/command/args; PID 1 clean; daemon unlabelled"
}

# ---- worker-cap -----------------------------------------------------------------------------------

running_workers() { # running_workers → names of Pending/Running Legion pods that count against worker_cap (root architects excluded)
  pods_json | jq -r '.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null and (.status.phase == "Pending" or .status.phase == "Running")) | select((.metadata.labels["legion.dev/role"] == "architect" and .metadata.labels["legion.dev/issue"] == .metadata.labels["legion.dev/tree"]) | not) | .metadata.name'
}
cap_tick() { # every tick: the running count never exceeds the cap
  running="$(running_workers)"
  running_count="$(printf '%s' "$running" | grep -c . || true)"
  [ "$running_count" -le 1 ] || failed "running phase-worker pods reached $running_count with worker_cap 1: $(printf '%s' "$running" | paste -sd, -)"
  peak=$((running_count > peak ? running_count : peak))
}
try_cap_queued() {
  read_state
  cap_tick
  local n
  n="$(sq '.workerAdmission.queue | length')"
  [ "$n" -gt 0 ] || { last="the worker queue is empty ($running_count worker pod(s) running: $(printf '%s' "$running" | paste -sd, -))"; return 1; }
  [ "$running_count" = 1 ] || failed "queue is non-empty while $running_count worker pods run (expected exactly 1 with worker_cap 1): $(printf '%s' "$running" | paste -sd, -)"
  head_issue="$(sq '.workerAdmission.queue[0].issue')"
  head_role="$(sq '.workerAdmission.queue[0].role')"
  head_token="$(sq '.workerAdmission.queue[0].roleToken')"
  last="queue head $head_issue/$head_role while $running runs"
}
try_cap_promoted() {
  read_state
  cap_tick
  local pod phase
  sq -e --arg t "$head_token" '[.workerAdmission.queue[] | select(.roleToken == $t)] | length == 0' >/dev/null ||
    { last="$head_issue/$head_role is still queued ($running_count running: $(printf '%s' "$running" | paste -sd, -))"; return 1; }
  pod="$(sq --arg t "$head_token" '.roles[$t].locator.podName // empty')"
  [ -n "$pod" ] || { last="$head_issue/$head_role left the queue but holds no pod yet"; return 1; }
  phase="$(pod_json "$pod" | jq -r '.status.phase // empty')"
  case "$phase" in Pending | Running) promoted_pod="$pod"; return 0 ;; esac
  last="$head_issue/$head_role names pod $pod, which is '${phase:-absent}'"
  return 1
}
cp_worker_cap() {
  [ "$root_issue_count" = 2 ] && [ "$worker_cap" = 1 ] ||
    blocked "worker-cap needs a run started with SMOKE_ROOT_ISSUES=2 SMOKE_WORKER_CAP=1 (this run: SMOKE_ROOT_ISSUES=$root_issue_count SMOKE_WORKER_CAP=$worker_cap)"
  peak=0
  poll "${budget[cap-queue]}" "the worker queue to hold a task" try_cap_queued || failed "$last"
  poll "${budget[cap-promote]}" "$head_issue/$head_role to be promoted" try_cap_promoted || failed "$last"
  ok "queue held $head_issue/$head_role while 1 pod ran; promoted to pod $promoted_pod; running count never exceeded 1 (peak $peak, sampled every ${SMOKE_POLL_INTERVAL:-5}s)"
}

# ---- done -----------------------------------------------------------------------------------------

try_done() {
  local keys k status merged_count report=()
  keys="$(record_require root-issues) $(for k in $(record_require root-issues); do children_keys "$k"; done)"
  for k in $keys; do
    status="$(issue_status "$k")"
    [ "$status" = done ] || { last="$k is '${status:-<none>}', not done"; return 1; }
    merged_count="$(gh pr list --repo "$repo" --state merged --search "head:legion/$k" --json number --jq 'length' 2>/dev/null || echo 0)"
    [ "$merged_count" -ge 1 ] || { last="$k is done but no merged pull request on $repo has head legion/$k"; return 1; }
    report+=("$k merged ($(gh pr list --repo "$repo" --state merged --search "head:legion/$k" --json number --jq '.[].number' 2>/dev/null | paste -sd, -))")
  done
  last="$(printf '%s; ' "${report[@]}")"
}
cp_done() {
  [[ "$controller" != none:* ]] || blocked "the run has no controller (${controller#none: })"
  [ "$github_ingress" = envoy ] || blocked "the run has SMOKE_GITHUB_INGRESS=none: merges need GitHub events"
  command -v gh >/dev/null 2>&1 || blocked "gh is not on PATH: needed to confirm the pull requests merged"
  poll "${budget[done]}" "every root and child issue to reach done with a merged pull request" try_done || failed "$last"
  ok "$last"
}

case "$checkpoint" in
  admitted) cp_admitted ;;
  architect-pod) cp_architect_pod ;;
  spec-posted) cp_spec_posted ;;
  tree-moved) cp_tree_moved ;;
  kill-pod-resume) cp_kill_pod_resume ;;
  pod-hygiene) cp_pod_hygiene ;;
  worker-cap) cp_worker_cap ;;
  done) cp_done ;;
esac
