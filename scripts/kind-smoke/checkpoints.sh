#!/usr/bin/env bash
# scripts/kind-smoke/checkpoints.sh <name> — one named checkpoint against a running instance.
# Reads the records under the instance's state directory, daemon state through the port-forward
# (the redacted GET /legion/v1/state), Dispatch through the instance's scratch server, and pods
# through kubectl with the instance kubeconfig. Prints exactly one line:
#   CHECKPOINT <name> OK: <detail>                      exit 0
#   CHECKPOINT <name> FAILED: <reason>                  exit 1
#   CHECKPOINT <name> SKIPPED-BLOCKED: <what is lacking> exit 3
# A mode that cannot supply the needed signal blocks before any network call; never a false green.
# shellcheck disable=SC2016  # jq filters are single-quoted on purpose; their $vars are jq's
set -euo pipefail
# shellcheck source=scripts/kind-smoke/lib.sh
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
port_dispatch="$(smoke_port dispatch)"
port_daemon="$(smoke_port daemon)"
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
)

# ---- readers -----------------------------------------------------------------------------------
# Each reader fills a global; a predicate that cannot read its inputs fails the checkpoint at once.

state_doc=""
read_state() {
  state_doc="$(daemon_state 2>/dev/null)" ||
    failed "daemon state unreachable on 127.0.0.1:$port_daemon (is the port-forward alive? see $state/logs/port-forward.log)"
}
sq() { printf '%s' "$state_doc" | jq -r "$@"; } # sq FILTER [ARGS…] — query the last state read
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
  read_tree_snapshot || return 1
  live="$(printf '%s\n' "$snap_claims" | awk 'NF {print $1}' | while read -r t; do [ "$(sq --arg t "$t" '.roles[$t].locator.podName // empty')" != "" ] && echo "$t"; done | paste -sd, -)"
  [ -n "$live" ] || { last="no phase worker or sub-architect holds a claim with a pod on the tree of $root_issue yet (the kill must land mid-phase)"; return 1; }
  gen0="$(sq --arg k "$root_issue" '.trees[$k].generation')"
  session0="$(sq --arg t "$arch_token" '.roles[$t].sessionId // empty')"
  pod0="$pod"
  pvc0="$(sq --arg k "$root_issue" '.trees[$k].locator.pvcName // empty')"
  file0="$(sq --arg k "$root_issue" '.trees[$k].locator.ompSessionFile // empty')"
  claims0="$snap_claims"
  statuses0="$snap_statuses"
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
  ok "$root_issue architect pod $pod0 → $pod1 generation $gen0→$gen1 (kill: $kill_method, landed: $landed; LEGION-177 workaround $workaround, $keeper) session $session0 unchanged; $resume; tree moved after the replacement registered — $moved"
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
  pod-hygiene) cp_pod_hygiene ;;
  worker-cap) cp_worker_cap ;;
  "done") cp_done ;;
esac
