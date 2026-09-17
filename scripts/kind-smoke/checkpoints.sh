#!/usr/bin/env bash
# scripts/kind-smoke/checkpoints.sh <name> — one named checkpoint against a running instance.
# Reads the records under the instance's state directory, redacted daemon state through the
# endpoint, durable daemon state only for active-phase selection, Dispatch through the instance's
# scratch server, and pods through kubectl with the instance kubeconfig. Prints exactly one line:
#   CHECKPOINT <name> OK: <detail>                      exit 0
#   CHECKPOINT <name> FAILED: <reason>                  exit 1
#   CHECKPOINT <name> SKIPPED-BLOCKED: <what is lacking> exit 3
# A mode that cannot supply the needed signal blocks before any network call; never a false green.
# shellcheck disable=SC2016  # jq filters are single-quoted on purpose; their $vars are jq's
set -euo pipefail
# shellcheck source=scripts/kind-smoke/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

checkpoints="admitted architect-pod spec-posted tree-moved kill-pod-resume scheduling controller-pane exec-auth plugin-skew volume-lost pod-hygiene worker-cap done"
checkpoint="${1:-}"
usage() { printf 'usage: checkpoints.sh <%s> [--wait-refresh]\n' "$(printf '%s' "$checkpoints" | tr ' ' '|')" >&2; }
[ -n "$checkpoint" ] || { usage; exit 2; }
shift
case " $checkpoints " in
  *" $checkpoint "*) ;;
  *) usage; exit 2 ;;
esac
wait_refresh=0
for flag in "$@"; do
  case "$checkpoint:$flag" in
    exec-auth:--wait-refresh) wait_refresh=1 ;;
    *) printf 'unknown checkpoint flag %s\n' "$flag" >&2; usage; exit 2 ;;
  esac
done

# shellcheck disable=SC2034  # The sourced poll() reads this process's global.
poll_timeout_line=0   # a timeout is reported by the FAILED line, with the last observation
ok() { printf 'CHECKPOINT %s OK: %s\n' "$checkpoint" "$*"; exit 0; }
failed() { printf 'CHECKPOINT %s FAILED: %s\n' "$checkpoint" "$*" >&2; exit 1; }
blocked() { printf 'CHECKPOINT %s SKIPPED-BLOCKED: %s\n' "$checkpoint" "$*" >&2; exit 3; }

# ---- records and budgets -----------------------------------------------------------------------

smoke_init
# shellcheck disable=SC2034  # smoke_port() reads the selected record-derived base.
port_base="$(record_require port-base)"
# shellcheck disable=SC2034  # The retained port is part of the checkpoint runtime context.
port_dispatch="$(smoke_port dispatch)"
port_daemon="$(smoke_port daemon)"
# shellcheck disable=SC2034  # The retained gateway is part of the checkpoint runtime context.
gateway="$(record_require gateway)"
project="$(record_require project)"
dispatch_project="$(record_require dispatch-project)"
root_issue="$(record_require root-issues | head -n1)"
controller="$(record_require controller)"
github_ingress="$(record_require github-ingress)"
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
  [exec-auth]="${SMOKE_WAIT_EXEC_AUTH:-60}"
  [controller-pane]="${SMOKE_WAIT_CONTROLLER_PANE:-120}"
  [scheduling]="${SMOKE_WAIT_SCHEDULING:-60}"
  [plugin-skew]="${SMOKE_WAIT_PLUGIN_SKEW:-120}"
  [volume-lost]="${SMOKE_WAIT_VOLUME_LOST:-900}"
)

# ---- readers -----------------------------------------------------------------------------------
# Each reader fills a global; a predicate that cannot read its inputs fails the checkpoint at once.

state_doc=""
# shellcheck disable=SC2154  # smoke_init() initializes the shared state directory global.
read_state() {
  state_doc="$(daemon_state 2>/dev/null)" ||
    failed "daemon state unreachable on 127.0.0.1:$port_daemon (is the port-forward alive? see $state/logs/port-forward.log)"
}
sq() { printf '%s' "$state_doc" | jq -r "$@"; } # sq FILTER [ARGS…] — query the last state read
# The state endpoint deliberately does not expose phases: their completion summaries are durable
# internal state. The kill target needs only the active phase's role, session, and assignment time,
# so it reads the recorded daemon state directly without ever printing it.
phase_state_doc=""
read_phase_state() {
  local mode daemon_state_dir
  mode="$(record_read daemon-mode)"
  case "$mode" in
    host)
      daemon_state_dir="$(record_require host-daemon-state-dir)"
      [ "$daemon_state_dir" = "$state/host-daemon/state" ] && [ ! -L "$daemon_state_dir" ] ||
        failed "host daemon state record '$daemon_state_dir' is not this instance's state directory"
      phase_state_doc="$(cat "$daemon_state_dir/state.json" 2>/dev/null)" ||
        failed "could not read host daemon state $daemon_state_dir/state.json"
      ;;
    cluster)
      phase_state_doc="$(kc exec deploy/legion-daemon-demo -c daemon -- cat /var/lib/legion/state.json 2>/dev/null)" ||
        failed "could not read the in-cluster daemon state"
      ;;
    *) failed "unknown daemon mode '${mode:-<none>}' while reading durable phase state" ;;
  esac
}
phase_sq() { printf '%s' "$phase_state_doc" | jq -r "$@"; }
# The Dispatch readers below run inside `$(…)` at their call sites, so they never call `failed`
# (an exit there leaves only the subshell and the checkpoint would print two verdicts): they return
# curl's status, and the caller — in the main shell — records the miss with `dispatch_miss` and
# returns 1, so the poll retries a transient failure and a persistent one ends in exactly one
# FAILED line naming Dispatch.
dispatch_miss() { last="Dispatch did not answer GET /api/v1/$1 (see $state/logs/dispatch.log)"; }
issue_json() { dispatch_get "issues/$1" 2>/dev/null; }
issue_status() { issue_json "$1" | jq -r '.status // empty'; }
children_keys() { # children_keys ROOT — one key per line; non-zero when Dispatch did not answer
  local listed
  if listed="$(dispatch_get "issues?project=$dispatch_project&parent=$1" 2>/dev/null)" && [ "$(printf '%s' "$listed" | jq -r 'length')" -gt 0 ]; then
    printf '%s' "$listed" | jq -r '.[].key'
    return 0
  fi
  issue_json "$1" | jq -r '.children[]?.key // empty'
}
pod_json() { kc get pod "$1" -o json 2>/dev/null || echo '{}'; }
pods_json() { kc get pods -o json 2>/dev/null || echo '{"items":[]}'; }
pvc_phase() { kc get pvc "$1" -o json 2>/dev/null | jq -r '.status.phase // empty'; }
controller_expected() { [[ "$controller" == tmux\ * ]]; }

last=""                # the last observation, printed on a timeout
lifecycle_statuses=" todo in_progress testing needs_review retro done "

# ---- admitted -----------------------------------------------------------------------------------

try_admitted() {
  local status tree ctl
  status="$(issue_status "$root_issue")" || { dispatch_miss "issues/$root_issue"; return 1; }
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
  status="$(issue_status "$root_issue")" || { dispatch_miss "issues/$root_issue"; return 1; }
  read_state
  tree_status="$(sq --arg k "$root_issue" '.trees[$k].status // empty')"
  runtime="$(sq --arg k "$root_issue" '.trees[$k].locator.runtime // empty')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  pvc="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  gen="$(sq --arg k "$root_issue" '.trees[$k].generation // empty')"
  [ "$status" = in_progress ] || { last="$root_issue has Dispatch status '${status:-<none>}', expected in_progress"; return 1; }
  [ "$tree_status" = active ] || { last="tree $root_issue is '${tree_status:-<none>}', expected active"; return 1; }
  [ "$runtime" = kubernetes ] || { last="tree $root_issue has locator runtime '${runtime:-<none>}', expected kubernetes"; return 1; }
  if [ -z "$pod" ] || [ -z "$pvc" ]; then last="tree $root_issue locator lacks podName/pvcName"; return 1; fi
  pod_doc="$(pod_json "$pod")"
  phase="$(printf '%s' "$pod_doc" | jq -r '.status.phase // empty')"
  [ "$phase" = Running ] || { last="pod $pod is '${phase:-absent}', expected Running"; return 1; }
  local want got
  # the labels carry the raw issue key (k8s-manifests.ts); only the pod and claim names are slugged
  for want in "legion.dev/project=$project" "legion.dev/role=architect" "legion.dev/generation=$gen" "legion.dev/tree=$root_issue" "legion.dev/issue=$root_issue"; do
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
  local doc artifact asks children worker
  doc="$(issue_json "$root_issue")" || { dispatch_miss "issues/$root_issue"; return 1; }
  artifact="$(printf '%s' "$doc" | jq -r '.primary_artifact_id // empty')"
  asks="$(dispatch_get "issues/$root_issue/asks" 2>/dev/null)" || { dispatch_miss "issues/$root_issue/asks"; return 1; }
  read_state
  # hard failures: the gate is off, so nothing may be registered or requested
  [ "$(sq --arg k "$root_issue" '.gates[$k] // empty')" = "" ] ||
    failed "$root_issue registered a design gate although the run has gates.design: off"
  printf '%s' "$asks" | jq -e '[.[]? | select(.kind == "approval" and .state == "open")] | length == 0' >/dev/null ||
    failed "an approval request is open on $root_issue although gates.design is off"
  [ -n "$artifact" ] || { last="$root_issue has no primary spec document yet"; return 1; }
  children="$(children_keys "$root_issue")" || { dispatch_miss "issues?project=$dispatch_project&parent=$root_issue"; return 1; }
  children="$(printf '%s' "$children" | paste -sd, - | sed 's/,/, /g')"
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
  children="$(children_keys "$root_issue")" || { dispatch_miss "issues?project=$dispatch_project&parent=$root_issue"; return 1; }
  for child in $children; do
    [ "$(sq --arg c "$child" '.trees[$c] // empty')" = "" ] || failed "child $child is admitted as a tree of its own (LEGION-57)"
    sq -e --arg c "$child" '(.admission.queue + .admission.active) | index($c) == null' >/dev/null ||
      failed "child $child is admitted as a tree of its own (LEGION-57): it is in the admission queue or active set"
  done
  # a sub-architect on a released child, or a phase worker on the root itself
  # shellcheck disable=SC2086  # $children is a newline list of keys, split on purpose
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
implementer_token="$(role_token "$project" "$root_issue" implementer)"
tree_claims() { # tree_claims → "token generation role issue" for every non-root claim on the tree, sorted; non-zero on a Dispatch miss
  local children
  children="$(children_keys "$root_issue" | jq -R . | jq -sc .)" || return 1
  sq --arg k "$root_issue" --argjson children "$children" \
    '.roles | to_entries[] | select(.value.issue != null) | select((.value.issue == $k and .value.role != "architect") or (.value.issue as $i | $children | index($i) != null)) | "\(.key) \(.value.generation // 0) \(.value.role) \(.value.issue)"' | sort
}
tree_statuses() { # tree_statuses → "KEY status" for the root and every child, sorted; non-zero on a Dispatch miss
  local k children s out=""
  children="$(children_keys "$root_issue")" || return 1
  for k in $root_issue $children; do
    s="$(issue_status "$k")" || return 1
    out+="$k $s"$'\n'
  done
  printf '%s' "$out" | sort
}
# read_tree_snapshot → sets snap_claims and snap_statuses, or records the Dispatch miss and returns 1
read_tree_snapshot() {
  snap_claims="$(tree_claims)" || { dispatch_miss "issues?project=$dispatch_project&parent=$root_issue"; return 1; }
  snap_statuses="$(tree_statuses)" || { dispatch_miss "issues/<tree of $root_issue>"; return 1; }
}
try_kill_target() { # an early, active implementer turn makes the root crash unambiguous
  local tree_status ready pod phase live active_phase phase_session assigned_at claim_session claim_ready claim_pod claim_pod_phase phase_started now age activity
  read_state
  tree_status="$(sq --arg k "$root_issue" '.trees[$k].status // empty')"
  ready="$(sq --arg k "$root_issue" '.trees[$k].readyConfirmedAt // empty')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  [ "$tree_status" = active ] || { last="tree $root_issue is '${tree_status:-<none>}', expected active"; return 1; }
  [ -n "$ready" ] || { last="tree $root_issue is not ready-confirmed yet"; return 1; }
  [ -n "$pod" ] || { last="tree $root_issue has no pod locator"; return 1; }
  phase="$(pod_json "$pod" | jq -r '.status.phase // empty')"
  [ "$phase" = Running ] || { last="root pod $pod is '${phase:-absent}', expected Running"; return 1; }
  read_tree_snapshot || return 1
  live="$(printf '%s\n' "$snap_claims" | awk 'NF {print $1}' | while read -r t; do [ "$(sq --arg t "$t" '.roles[$t].locator.podName // empty')" != "" ] && echo "$t"; done | paste -sd, -)"
  [ -n "$live" ] || { last="no phase worker or sub-architect holds a claim with a pod on the tree of $root_issue yet (the kill must land mid-phase)"; return 1; }
  read_phase_state
  active_phase="$(phase_sq --arg k "$root_issue" '.phases[$k].phase // empty')"
  [ "$active_phase" = implementer ] || { last="active phase is ${active_phase:-<none>}, expected implementer"; return 1; }
  phase_session="$(phase_sq --arg k "$root_issue" '.phases[$k].sessionId // empty')"
  assigned_at="$(phase_sq --arg k "$root_issue" '.phases[$k].assignedAt // empty')"
  claim_session="$(sq --arg t "$implementer_token" '.roles[$t].sessionId // empty')"
  [ "$claim_session" = "$phase_session" ] || { last="implementer claim session ${claim_session:-<none>} does not match active phase session ${phase_session:-<none>}"; return 1; }
  claim_ready="$(sq --arg t "$implementer_token" '.roles[$t].readyConfirmedAt // empty')"
  [ -n "$claim_ready" ] || { last="implementer claim is not ready-confirmed"; return 1; }
  claim_pod="$(sq --arg t "$implementer_token" '.roles[$t].locator.podName // empty')"
  [ -n "$claim_pod" ] || { last="implementer claim has no pod locator"; return 1; }
  claim_pod_phase="$(pod_json "$claim_pod" | jq -r '.status.phase // empty')"
  [ "$claim_pod_phase" = Running ] || { last="implementer pod $claim_pod is '${claim_pod_phase:-absent}', expected Running"; return 1; }
  phase_started="$(date -u -d "$assigned_at" +%s 2>/dev/null)" || { last="implementer phase assignedAt '$assigned_at' is not a timestamp"; return 1; }
  now="$(date -u +%s)"
  age=$((now - phase_started))
  [ "$age" -ge 0 ] || { last="implementer phase assignedAt '$assigned_at' is in the future"; return 1; }
  [ "$age" -lt 120 ] || { last="implementer phase is ${age}s old, expected under 120s"; return 1; }
  activity="$(kc logs "$claim_pod" -c worker --tail=200 2>/dev/null || true)"
  printf '%s\n' "$activity" | awk '
    /^agent_start$/ { started = 1; tool = 0; next }
    /^agent_end$/ { started = 0; tool = 0; next }
    /^tool_execution_start / && started { tool = 1 }
    END { exit !(started && tool) }
  ' || { last="implementer has no mid-task tool activity"; return 1; }
  gen0="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  session0="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  pod0="$pod"
  pvc0="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  file0="$(sq --arg k "$root_issue" '.trees[$k].locator.ompSessionFile // empty')"
  claims0="$snap_claims"
  statuses0="$snap_statuses"
  last="mid-phase: root pod $pod0 generation $gen0, active implementer $claim_pod has tool activity at ${age}s"
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
# shellcheck disable=SC2154  # smoke_init() initializes the recorded cluster global.
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
# try_kill_landed — the direct evidence that the kill reached the process: pod0 is Failed, its worker
# container terminated with 137 (SIGKILL), or the pod is gone (the forced-delete fallback). While pod0
# is still Running, a tree that moves on is not the daemon's doing and must not be blamed on it.
try_kill_landed() {
  local doc err phase code status=0
  # pod0 is read with stderr kept: only a NotFound is "gone" (the forced-delete fallback); an API
  # blip, a timeout, or a stale kubeconfig is retried within the budget and, at expiry, names kubectl
  err="$(mktemp)"
  doc="$(kc get pod "$pod0" -o json 2>"$err")" || status=$?
  if [ "$status" != 0 ]; then
    if grep -q 'NotFound' "$err"; then
      rm -f "$err"
      landed="pod $pod0 gone"
      return 0
    fi
    last="could not read pod $pod0: $(tr '\n' ' ' <"$err" | cut -c1-200)"
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
  phase="$(printf '%s' "$doc" | jq -r '.status.phase // empty')"
  code="$(printf '%s' "$doc" | jq -r '[.status.containerStatuses[]? | select(.name == "worker") | .state.terminated.exitCode // .lastState.terminated.exitCode // empty] | first // empty')"
  if [ "$phase" = Failed ] || [ "$code" = 137 ]; then
    landed="pod $pod0 ${phase:-Failed}${code:+ (worker exit $code)}"
    return 0
  fi
  read_state
  read_tree_snapshot || return 1
  if [ "$(sq --arg k "$root_issue" '.trees[$k].generation')" = "$gen0" ] && { [ "$snap_claims" != "$claims0" ] || [ "$snap_statuses" != "$statuses0" ]; }; then
    failed "the kill did not land: pod $pod0 is still $phase after $kill_method while the tree moved on at generation $gen0"
  fi
  last="pod $pod0 is still $phase after $kill_method; waiting for it to die"
  return 1
}
try_kill_resumed() {
  local gen pod phase initlog
  read_state
  gen="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  if [ "$gen" = "$gen0" ]; then
    # the kill landed ($landed); while the root is dead a phase worker may still finish and move the
    # issue's status at this generation — other work continuing, never a verdict. Only the budget
    # (one resync interval plus a pod start) decides that the daemon did not resurrect the root.
    # (read before the poll: poll's own `local budget` — its scalar first argument — shadows this array inside every predicate it runs)
    last="no replacement within ${resume_budget}s: the daemon did not resurrect the root (recorded generation $gen0, current $gen; pod0 $landed, locator ${pod:-none}; its resync probe should have — see the daemon log)"
    return 1
  fi
  if [ -z "$pod" ] || [ "$pod" = "$pod0" ]; then last="tree $root_issue is at generation $gen but its locator still names $pod0"; return 1; fi
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
  # the tree as it stands once the replacement is registered: "keeps working afterwards" is judged
  # against this, not the kill-time snapshot, so a change the dying architect set in motion does
  # not count as the resurrected one driving the tree
  read_tree_snapshot || return 1
  claims1="$snap_claims"
  statuses1="$snap_statuses"
  last="replacement registered: generation $gen1 ready-confirmed, session ${session1:-<none>}"
}
try_tree_moved_after() {
  local claims statuses
  read_state
  read_tree_snapshot || return 1
  claims="$snap_claims"
  statuses="$snap_statuses"
  if [ "$claims" != "$claims1" ]; then moved="claims changed: $(diff <(printf '%s\n' "$claims1") <(printf '%s\n' "$claims") | grep '^[<>]' | tr '\n' ';' | tr -s ' ')"; return 0; fi
  if [ "$statuses" != "$statuses1" ]; then moved="status changed: $(diff <(printf '%s\n' "$statuses1") <(printf '%s\n' "$statuses") | grep '^[<>]' | tr '\n' ';' | tr -s ' ')"; return 0; fi
  last="the tree of $root_issue has not moved since the replacement registered (claims: $(printf '%s' "$claims" | awk '{print $3"/"$4"@"$2}' | paste -sd, -); statuses: $(printf '%s' "$statuses" | paste -sd, -))"
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
  keeper="$(pid_is_live legion-177-keeper && printf 'keeper running (pgid %s)' "$(<"$state/pids/legion-177-keeper.pid")" || printf 'keeper not running')"
  crash_root_pod
  poll "${budget[kill-resume]}" "the kill to land on $pod0" try_kill_landed || failed "$last"
  resume_budget="${budget[kill-resume]}"
  poll "$resume_budget" "the replacement root pod" try_kill_resumed || failed "$last"
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
  record_write checkpoint-kill-pod-resume ok
  ok "$root_issue architect pod $pod0 → $pod1 generation $gen0→$gen1 (kill: $kill_method, landed: $landed; LEGION-177 workaround $workaround, $keeper) session $session0 unchanged; $resume; tree moved after the replacement registered — $moved"
}

# ---- host daemon checks --------------------------------------------------------------------------

try_exec_auth() {
  local calls
  calls="$(wc -l <"$state/host-daemon/exec-calls.log" 2>/dev/null || true)"
  [ "$calls" -ge 1 ] || { last="the kubeconfig exec plugin has not minted a token"; return 1; }
  grep -Fq 'kubeconfig exec plugin minted a token' "$state/logs/daemon.log" 2>/dev/null ||
    { last="daemon.log has no kubeconfig exec plugin token-mint line"; return 1; }
  last="exec plugin minted $calls token$( [ "$calls" = 1 ] || printf s)"
}
try_exec_auth_refresh() {
  local calls
  calls="$(wc -l <"$state/host-daemon/exec-calls.log" 2>/dev/null || true)"
  [ "$calls" -gt "$exec_calls_before" ] || {
    last="exec plugin has not refreshed its token from $exec_calls_before mint$( [ "$exec_calls_before" = 1 ] || printf s)"
    return 1
  }
  last="exec plugin refreshed from $exec_calls_before to $calls token mints"
}
cp_exec_auth() {
  [ "$(record_read daemon-mode)" = host ] || blocked "exec-auth needs SMOKE_DAEMON_MODE=host"
  [ -f "$state/host-daemon/exec-calls.log" ] ||
    failed "host daemon exec call log is missing: run scripts/kind-smoke/up.sh with SMOKE_DAEMON_MODE=host first"
  poll "${budget[exec-auth]}" "the kubeconfig exec plugin to mint a token" try_exec_auth || failed "$last"
  if [ "$wait_refresh" = 1 ]; then
    exec_calls_before="$(wc -l <"$state/host-daemon/exec-calls.log")"
    poll 180 "the kubeconfig exec plugin token refresh" try_exec_auth_refresh || failed "$last"
  fi
  ok "$last"
}

try_controller_pane() {
  local runtime server panes window capture
  read_state
  runtime="$(sq '.controllerLocator.runtime // empty')"
  [ "$runtime" = tmux ] || { last="daemon state controllerLocator.runtime is '${runtime:-<none>}', expected tmux"; return 1; }
  server="$(record_read controller-tmux-server)"
  [ -n "$server" ] || { last="daemon state has a tmux controller locator but no controller-tmux-server record"; return 1; }
  window="$(sq '.controllerLocator.tmuxWindowId // empty')"
  [ -n "$window" ] || { last="daemon state controllerLocator has no tmuxWindowId"; return 1; }
  panes="$(tmux -L "$server" list-panes -a -F '#{pane_current_command}' 2>/dev/null || true)"
  [ -n "$panes" ] || { last="daemon state has a tmux controller locator but server $server has no pane"; return 1; }
  capture="$server $window $state/logs/controller-pane.log"
  if [ "$(record_read controller-pane-capture)" != "$capture" ]; then
    tmux -L "$server" pipe-pane -t "$window" -o "cat >>$state/logs/controller-pane.log" || {
      last="could not capture controller pane $window on tmux server $server"
      return 1
    }
    record_write controller-pane-capture "$capture"
  fi
  last="controller pane running on tmux server $server (capturing $state/logs/controller-pane.log)"
}
cp_controller_pane() {
  [ "$(record_read daemon-mode)" = host ] || blocked "controller-pane needs SMOKE_DAEMON_MODE=host"
  poll "${budget[controller-pane]}" "the daemon-spawned controller pane" try_controller_pane || failed "$last"
  ok "$last"
}

provider_secret_values() {
  local name
  name="$(record_read providers-secret)"
  [ -n "$name" ] || { last="providers-secret record is missing"; return 1; }
  provider_values="$(kc get secret "$name" -o json 2>/dev/null | jq -r '.data // {} | to_entries[] | .value | @base64d')" ||
    { last="could not read providers Secret $name"; return 1; }
}
try_scheduling() {
  local pods count doc pod node priority annotation bad_container strings c name value secret
  pods="$(pods_json)"
  count="$(printf '%s' "$pods" | jq '[.items[] | select(.metadata.labels["legion.dev/project"] != null)] | length')"
  [ "$count" -gt 0 ] || { last="no Legion pod is labelled legion.dev/project"; return 1; }
  node="$(record_read legion-node)"
  [ -n "$node" ] || { last="legion-node record is missing"; return 1; }
  provider_secret_values || return 1
  while IFS= read -r doc; do
    pod="$(printf '%s' "$doc" | jq -r '.metadata.name')"
    [ "$(printf '%s' "$doc" | jq -r '.spec.nodeName // empty')" = "$node" ] ||
      { last="pod $pod spec.nodeName is $(printf '%s' "$doc" | jq -r '.spec.nodeName // "<none>"'), expected $node"; return 1; }
    priority="$(printf '%s' "$doc" | jq -r '.spec.priorityClassName // empty')"
    [ "$priority" = legion ] || { last="pod $pod spec.priorityClassName is '${priority:-<none>}', expected legion"; return 1; }
    annotation="$(printf '%s' "$doc" | jq -r '.metadata.annotations["karpenter.sh/do-not-disrupt"] // empty')"
    [ "$annotation" = true ] || { last="pod $pod annotation karpenter.sh/do-not-disrupt is '${annotation:-<none>}', expected true"; return 1; }
    printf '%s' "$doc" | jq -e '.spec.tolerations[]? | select(.key == "legion.dev/pool" and .operator == "Equal" and .value == "legion" and .effect == "NoSchedule")' >/dev/null ||
      { last="pod $pod lacks toleration legion.dev/pool=legion:NoSchedule"; return 1; }
    bad_container="$(printf '%s' "$doc" | jq -r '[ (.spec.containers[]?, .spec.initContainers[]?) | select(.securityContext.allowPrivilegeEscalation != false or .securityContext.capabilities.drop != ["ALL"]) | .name ] | first // empty')"
    [ -z "$bad_container" ] || { last="pod $pod container $bad_container does not set allowPrivilegeEscalation=false and capabilities.drop=[ALL]"; return 1; }
    strings="$(printf '%s' "$doc" | jq -r '(.spec.containers[]?, .spec.initContainers[]?) | .name as $container | .env[]? | "\($container)\t\(.name)\t\(.value // "")"')"
    while IFS=$'\t' read -r c name value; do
      [ -n "$c" ] || continue
      [[ "$value" =~ ^(sk-|ghp_|github_pat_|xox) ]] &&
        { last="pod $pod container $c env $name contains a secret-looking value"; return 1; }
      while IFS= read -r secret; do
        [ -n "$secret" ] && [ "$value" = "$secret" ] &&
          { last="pod $pod container $c env $name contains a providers Secret value"; return 1; }
      done <<<"$provider_values"
    done <<<"$strings"
  done < <(printf '%s' "$pods" | jq -c '.items[] | select(.metadata.labels["legion.dev/project"] != null)')
  last="$count Legion pod(s) satisfy placement and hardening"
}
cp_scheduling() {
  poll "${budget[scheduling]}" "every Legion pod's placement and hardening" try_scheduling || failed "$last"
  ok "$last"
}

live_process_count() {
  sq '[.trees[]?.locator, .roles[]?.locator, .controllerLocator] | map(select(. != null)) | map(.podName // .tmuxPaneId // empty) | map(select(length > 0)) | unique | length'
}
try_plugin_skew() {
  warnings="$(awk -v installed="installed $plugin_version — relaunch it (LEGION-164)" '
    index($0, "runs pi-legion-envoy") && index($0, installed) { count += 1 }
    END { print count + 0 }
  ' "$state/logs/daemon.log" 2>/dev/null)"
  [ "$warnings" = "$plugin_live_count" ] || {
    last="expected $plugin_live_count live process warning$( [ "$plugin_live_count" = 1 ] || printf s) for installed pi-legion-envoy $plugin_version, found $warnings"
    return 1
  }
  last="$plugin_live_count live process warning$( [ "$plugin_live_count" = 1 ] || printf s) names installed pi-legion-envoy $plugin_version"
}
cp_plugin_skew() {
  [ "$(record_read daemon-mode)" = host ] || blocked "plugin-skew needs SMOKE_DAEMON_MODE=host"
  plugin_tgz="${SMOKE_PLUGIN_TGZ:-}"
  [ -n "$plugin_tgz" ] && [ -f "$plugin_tgz" ] ||
    failed "plugin-skew needs SMOKE_PLUGIN_TGZ to name a version-bumped pi-legion-envoy tarball"
  plugin_manifest="$(tar -xOf "$plugin_tgz" package/package.json 2>/dev/null)" ||
    failed "could not read package/package.json from SMOKE_PLUGIN_TGZ=$plugin_tgz"
  plugin_version="$(printf '%s' "$plugin_manifest" | jq -r '.version // empty')" ||
    failed "SMOKE_PLUGIN_TGZ package.json has no version"
  [ -n "$plugin_version" ] || failed "SMOKE_PLUGIN_TGZ package.json has no version"
  read_state
  plugin_live_count="$(live_process_count)"
  [ "$plugin_live_count" -gt 0 ] || failed "no live pane or pod has a recorded locator to check for plugin skew"
  if sq -r '[.trees[]?.locator.pluginVersion, .roles[]?.locator.pluginVersion] | map(select(. != null)) | unique[]' | grep -Fxq "$plugin_version"; then
    failed "SMOKE_PLUGIN_TGZ version $plugin_version is not a version bump over a live process"
  fi
  plugin_source="$state/host-daemon/plugin-skew"
  [ ! -e "$plugin_source" ] ||
    failed "plugin-skew source $plugin_source already exists: rerun scripts/kind-smoke/up.sh after down.sh"
  mkdir "$plugin_source" || failed "could not create plugin-skew source $plugin_source"
  tar -xzf "$plugin_tgz" -C "$plugin_source" --strip-components=1 ||
    failed "could not extract SMOKE_PLUGIN_TGZ=$plugin_tgz"
  omp plugin install "$plugin_source" >/dev/null ||
    failed "omp plugin install extracted SMOKE_PLUGIN_TGZ failed"
  daemon_ctl="${SMOKE_DAEMON_CTL:-${BASH_SOURCE[0]%/*}/daemon-ctl.sh}"
  "$daemon_ctl" stop >/dev/null || failed "daemon-ctl.sh stop failed during plugin-skew"
  "$daemon_ctl" start >/dev/null || failed "daemon-ctl.sh start failed during plugin-skew"
  poll "${budget[plugin-skew]}" "the daemon plugin-skew journal lines" try_plugin_skew || failed "$last"
  ok "$last"
}

try_volume_lost_target() {
  local claims count
  read_state
  volume_pvc="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  [ -n "$volume_pvc" ] || { last="tree $root_issue has no PVC record"; return 1; }
  volume_root_pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  volume_root_generation="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  volume_root_session="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  [ -n "$volume_root_pod" ] && [ -n "$volume_root_session" ] || { last="root architect of $root_issue is not ready-confirmed"; return 1; }
  claims="$(sq --arg k "$root_issue" --arg pvc "$volume_pvc" '.roles | to_entries[] | select(.value.issue == $k and .value.role != "architect" and .value.locator.pvcName == $pvc and .value.sessionId != null and .value.readyConfirmedAt != null and .value.pendingAssignment == null) | "\(.key)\t\(.value.role)\t\(.value.sessionId)\t\(.value.launchFailures // 0)"')"
  count="$(printf '%s\n' "$claims" | grep -c . || true)"
  [ "$count" -gt 0 ] || { last="no ready-confirmed worker claim for $root_issue has a Running pod"; return 1; }
  volume_claims="$claims"
  last="tree $root_issue root $volume_root_pod and $count ready-confirmed worker claim(s) share PVC $volume_pvc"
}
worker_recovery_ready() {
  local token role failures from_ref pod resumes prompt
  read_state
  while IFS=$'\t' read -r token role _ failures; do
    from_ref="$(sq --arg t "$token" '.roles[$t].workspaceLost.fromRef // empty')"
    [ "$from_ref" = "legion/$root_issue" ] || { last="worker $role has workspaceLost.fromRef '${from_ref:-<none>}'"; return 1; }
    pod="$(sq --arg t "$token" '.roles[$t].locator.podName // empty')"
    [ -n "$pod" ] || { last="worker $role has no replacement pod"; return 1; }
    resumes="$(resume_arg "$pod")"
    [ -z "$resumes" ] || { last="worker $role replacement $pod carries $resumes"; return 1; }
    prompt="$(pod_json "$pod" | jq -r '[.spec.containers[]? | select(.name == "worker") | .command as $command | $command | to_entries[] | select(.value == "--append-system-prompt") | $command[.key + 1]] | first // empty')"
    [[ "$prompt" == "Your workspace was recreated from"* ]] || { last="worker $role replacement $pod has no recovery prompt"; return 1; }
    [ "$(sq --arg t "$token" '.roles[$t].launchFailures // 0')" = "$failures" ] || { last="worker $role launchFailures changed"; return 1; }
  done <<<"$volume_claims"
}
try_volume_lost_recovered() {
  local root_from root_pod root_generation root_session root_resume root_prompt
  read_state
  root_from="$(sq --arg k "$root_issue" '.trees[$k].workspaceLost.fromRef // empty')"
  [ "$root_from" = "legion/$root_issue" ] || { last="tree $root_issue has workspaceLost.fromRef '${root_from:-<none>}'"; return 1; }
  root_pod="$(sq --arg k "$root_issue" '.trees[$k].locator.podName // empty')"
  root_generation="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  [ -n "$root_pod" ] && [ "$root_pod" != "$volume_root_pod" ] && [ "$root_generation" -gt "$volume_root_generation" ] || { last="tree $root_issue has no new root generation after volume loss"; return 1; }
  root_session="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  [ -n "$root_session" ] && [ "$root_session" != "$volume_root_session" ] || { last="recovered root did not register a new session"; return 1; }
  root_resume="$(resume_arg "$root_pod")"
  [ -z "$root_resume" ] || { last="recovered root $root_pod carries $root_resume"; return 1; }
  root_prompt="$(pod_json "$root_pod" | jq -r '[.spec.containers[]? | select(.name == "worker") | .command as $command | $command | to_entries[] | select(.value == "--append-system-prompt") | $command[.key + 1]] | first // empty')"
  [[ "$root_prompt" == "Your workspace was recreated from"* ]] || { last="recovered root $root_pod has no recovery prompt"; return 1; }
  grep -Fq worker-recovered "$state/logs/daemon.log" 2>/dev/null || { last="daemon log has no worker-recovered notice"; return 1; }
  worker_recovery_ready || return 1
  last="tree $root_issue recovered root $root_pod and every recorded worker from volume loss"
}
cp_volume_lost() {
  if [ "$(record_read daemon-mode)" != host ] && [ -z "${SMOKE_ARCHITECT_LOG:-}" ]; then
    blocked "volume-lost needs SMOKE_DAEMON_MODE=host or SMOKE_ARCHITECT_LOG"
  fi
  [ "$(record_read checkpoint-kill-pod-resume)" = ok ] ||
    blocked "volume-lost needs a successful kill-pod-resume checkpoint first"
  poll "${budget[volume-lost]}" "a ready-confirmed tree before volume loss" try_volume_lost_target || failed "$last"
  kc delete pods -l "legion.dev/tree=$root_issue" --wait >/dev/null || failed "could not delete every pod of tree $root_issue"
  kc delete pvc "$volume_pvc" --wait >/dev/null 2>&1 || {
    [ "$(pvc_phase "$volume_pvc")" = "" ] || failed "could not delete tree PVC $volume_pvc"
  }
  poll "${budget[volume-lost]}" "the whole-tree workspace-loss recovery" try_volume_lost_recovered || failed "$last"
  ok "$last"
}

# ---- pod-hygiene ---------------------------------------------------------------------------------

quantity_jq='def quantity: if type == "number" then . else (capture("^(?<n>[0-9.]+)(?<u>[A-Za-z]*)$") | (.n | tonumber) * ({"":1,"m":0.001,"k":1000,"M":1000000,"G":1000000000,"T":1000000000000,"Ki":1024,"Mi":1048576,"Gi":1073741824,"Ti":1099511627776}[.u] // (error("unknown quantity unit " + .u)))) end;'
cp_pod_hygiene() {
  local pods checked profile_problem
  pods="$(pods_json)"
  checked="$(printf '%s' "$pods" | jq '[.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null)] | length')"
  [ "$checked" -gt 0 ] || failed "no legion.dev/project pods are running; run architect-pod first"
  # 1. the daemon is never labelled legion.dev/project (the orphan sweep would delete it)
  kc get deploy legion-daemon-demo -o json 2>/dev/null | jq -e '.spec.template.metadata.labels | has("legion.dev/project") | not' >/dev/null ||
    failed "the daemon Deployment's pod template carries legion.dev/project"
  printf '%s' "$pods" | jq -e '[.items[] | select(.metadata.labels["app.kubernetes.io/name"] == "legion-daemon" and .metadata.labels["legion.dev/project"] != null)] | length == 0' >/dev/null ||
    failed "the daemon pod carries legion.dev/project"
  # 2. every Legion pod's containers carry exactly its role profile's requests and limits
# shellcheck disable=SC2154  # smoke_init() initializes the shared records directory global.
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
  local strings secret
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
    environ="$(kc exec "$p" -c worker -- cat /proc/1/environ 2>/dev/null | tr '\0' '\n')" ||
      failed "could not read the PID 1 environment of pod $p (kubectl exec failed); nothing is claimed clean for it"
    [ -n "$environ" ] || failed "pod $p PID 1 environment read back empty; nothing is claimed clean for it"
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
# The daemon's cap bounds running or currently-prompted workers; a finished worker's pod stays alive
# idle for worker_idle_retire_seconds and does not count, and the redacted state carries no run
# state, so a pod count over-approximates the daemon's own. What is observable: the daemon queued a
# task while at least one worker pod ran (it judged the cap reached), the head was promoted once a
# runner finished, and an excess of worker pods over the cap never lasts longer than idle lingering
# can explain (worker_idle_retire_seconds + the stop timeout) — a sustained excess is a violation.

worker_idle_retire="$(record_read worker-idle-retire)"
worker_idle_retire="${worker_idle_retire:-600}"
excess_allowance=$((worker_idle_retire + 30))
running_workers() { # running_workers → names of Pending/Running Legion worker pods not yet being deleted (root architects excluded)
  pods_json | jq -r '.items[] | select(.metadata.labels["legion.dev/project"] != null and .metadata.labels["legion.dev/probe"] == null and .metadata.deletionTimestamp == null and (.status.phase == "Pending" or .status.phase == "Running")) | select((.metadata.labels["legion.dev/role"] == "architect" and .metadata.labels["legion.dev/issue"] == .metadata.labels["legion.dev/tree"]) | not) | .metadata.name'
}
cap_tick() { # every tick: count the worker pods; an excess over the cap may only be idle lingering
  running="$(running_workers)"
  running_count="$(printf '%s' "$running" | grep -c . || true)"
  peak=$((running_count > peak ? running_count : peak))
  local now
  now="$(date +%s)"
  if [ "$running_count" -gt "$worker_cap" ]; then
    [ -n "$excess_since" ] || excess_since="$now"
    local lasted=$((now - excess_since))
    excess_longest=$((lasted > excess_longest ? lasted : excess_longest))
    [ "$lasted" -le "$excess_allowance" ] ||
      failed "$running_count worker pods have run for ${lasted}s with worker_cap $worker_cap, longer than idle lingering (worker_idle_retire_seconds $worker_idle_retire + 30s) can explain: $(printf '%s' "$running" | paste -sd, -)"
  else
    excess_since=""
  fi
}
try_cap_queued() {
  read_state
  cap_tick
  local n
  n="$(sq '.workerAdmission.queue | length')"
  if [ "$n" -eq 0 ]; then
    zero_ticks=0   # a zero-running sample counts only when the queue held a task on consecutive samples
    last="the worker queue is empty ($running_count worker pod(s) running: $(printf '%s' "$running" | paste -sd, -))"
    return 1
  fi
  # a queue with nothing running is a violation only when it persists: between a retired worker's pod
  # deletion and the promoted one's creation (the daemon awaits the old pod before it spawns) one tick
  # can legitimately see zero
  if [ "$running_count" -ge 1 ]; then
    zero_ticks=0
  else
    zero_ticks=$((zero_ticks + 1))
    [ "$zero_ticks" -lt 2 ] || failed "the worker queue held a task while no worker pod ran for $zero_ticks consecutive samples: the daemon judged the cap reached with nothing running"
    last="the worker queue holds a task while no worker pod runs (sample $zero_ticks of 2 before this counts)"
    return 1
  fi
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
  if [ "$root_issue_count" != 2 ] || [ "$worker_cap" != 1 ]; then
    blocked "worker-cap needs a run started with SMOKE_ROOT_ISSUES=2 SMOKE_WORKER_CAP=1 (this run: SMOKE_ROOT_ISSUES=$root_issue_count SMOKE_WORKER_CAP=$worker_cap)"
  fi
  peak=0
  zero_ticks=0
  excess_since=""
  excess_longest=0
  poll "${budget[cap-queue]}" "the worker queue to hold a task" try_cap_queued || failed "$last"
  local ran_at_queue="$running_count"
  poll "${budget[cap-promote]}" "$head_issue/$head_role to be promoted" try_cap_promoted || failed "$last"
  ok "queue held $head_issue/$head_role while $ran_at_queue worker pod(s) ran; promoted to pod $promoted_pod; worker pods peaked at $peak with worker_cap $worker_cap (an excess is idle lingering, allowed up to worker_idle_retire_seconds $worker_idle_retire + 30s; longest ${excess_longest}s), sampled every ${SMOKE_POLL_INTERVAL:-5}s"
}

# ---- done -----------------------------------------------------------------------------------------

try_done() {
  local keys k kids status merged report=()
  keys="$(record_require root-issues)"
  for k in $keys; do
    kids="$(children_keys "$k")" || { dispatch_miss "issues?project=$dispatch_project&parent=$k"; return 1; }
    keys+=" $kids"
  done
  for k in $keys; do
    status="$(issue_status "$k")" || { dispatch_miss "issues/$k"; return 1; }
    [ "$status" = "done" ] || { last="$k is '${status:-<none>}', not done"; return 1; }
    merged="$(gh pr list --repo "$repo" --state merged --search "head:legion/$k" --json number --jq '.[].number' 2>/dev/null)" ||
      { last="gh could not list the merged pull requests of $repo for $k (rate limit, auth, or network)"; return 1; }
    [ -n "$merged" ] || { last="$k is done but no merged pull request on $repo has head legion/$k"; return 1; }
    report+=("$k merged ($(printf '%s' "$merged" | paste -sd, -))")
  done
  last="$(printf '%s; ' "${report[@]}")"
}
cp_done() {
  [[ "$controller" != none:* ]] || blocked "the run has no controller (${controller#none: })"
  [ "$github_ingress" = envoy ] || blocked "the run has SMOKE_GITHUB_INGRESS=none: merges need GitHub events"
  command -v gh >/dev/null 2>&1 || blocked "gh is not on PATH: needed to confirm the pull requests merged"
  # a gh that cannot reach GitHub is a lack of the run, decided once before the long wait
  gh pr list --repo "$repo" --limit 1 --json number >/dev/null 2>&1 ||
    blocked "gh cannot list pull requests on $repo (unauthenticated, rate-limited, or offline): needed to confirm the pull requests merged"
  poll "${budget[done]}" "every root and child issue to reach done with a merged pull request" try_done || failed "$last"
  ok "$last"
}

case "$checkpoint" in
  admitted) cp_admitted ;;
  architect-pod) cp_architect_pod ;;
  spec-posted) cp_spec_posted ;;
  tree-moved) cp_tree_moved ;;
  kill-pod-resume) cp_kill_pod_resume ;;
  scheduling) cp_scheduling ;;
  controller-pane) cp_controller_pane ;;
  exec-auth) cp_exec_auth ;;
  plugin-skew) cp_plugin_skew ;;
  volume-lost) cp_volume_lost ;;
  pod-hygiene) cp_pod_hygiene ;;
  worker-cap) cp_worker_cap ;;
  "done") cp_done ;;
esac
