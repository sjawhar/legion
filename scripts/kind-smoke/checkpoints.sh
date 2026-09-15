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

case "$checkpoint" in
  admitted) cp_admitted ;;
  architect-pod) cp_architect_pod ;;
  spec-posted) cp_spec_posted ;;
  tree-moved) cp_tree_moved ;;
  *) failed "checkpoint $checkpoint is not implemented yet" ;;
esac
