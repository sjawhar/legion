#!/usr/bin/env bash
# Gate 3 node-loss proof: an EBS-backed tree keeps the same OMP session after its node drains.
set -euo pipefail
# shellcheck source=scripts/eks-gate/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

context=""
daemon_url=""
tree=""
failures=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) context="${2:-}"; shift 2 ;;
    --daemon-url) daemon_url="${2:-}"; shift 2 ;;
    -*) gate_failed node-loss/arguments "unknown argument $1"; exit 2 ;;
    *) [ -z "$tree" ] || { gate_failed node-loss/arguments "unexpected tree $1"; exit 2; }; tree="$1"; shift ;;
  esac
done
require_context node-loss
if [ -z "$daemon_url" ]; then gate_failed node-loss/daemon-url '--daemon-url is required'; exit 2; fi
if [ -z "$tree" ]; then gate_failed node-loss/tree 'a tree key is required'; exit 2; fi

state_url="${daemon_url%/}/legion/v1/state"
state() { curl -fsS "$state_url"; }
initial="$(state 2>/dev/null)" || { gate_failed node-loss/daemon-state "GET $state_url failed"; exit 1; }
pvc="$(printf '%s' "$initial" | jq -r --arg tree "$tree" '.trees[$tree].locator.pvcName // empty')"
if [ -z "$pvc" ]; then gate_failed node-loss/claims "$tree has no PVC-backed tree locator"; exit 1; fi
root_claims="$(printf '%s' "$initial" | jq -r --arg tree "$tree" --arg pvc "$pvc" '
  .trees[$tree] as $root
  | .roles | to_entries[]
  | select(.value.issue == $tree and .value.role == "architect" and .value.sessionId != null)
  | select($root.locator.pvcName == $pvc and $root.locator.podName != null and $root.generation != null and $root.readyConfirmedAt != null)
  | [.key, .value.role, .value.sessionId, ($root.generation | tostring), ($root.readyConfirmedAt | tostring), $root.locator.podName, "tree"] | @tsv')"
worker_claims="$(printf '%s' "$initial" | jq -r --arg tree "$tree" --arg pvc "$pvc" '
  .roles | to_entries[]
  | select(.value.issue == $tree and (.value.issue != $tree or .value.role != "architect"))
  | select(.value.locator.pvcName == $pvc and .value.locator.podName != null and .value.sessionId != null and .value.generation != null and .value.readyConfirmedAt != null)
  | [.key, .value.role, .value.sessionId, (.value.generation | tostring), (.value.readyConfirmedAt | tostring), .value.locator.podName, "role"] | @tsv')"
claims="$root_claims"
if [ -n "$worker_claims" ]; then
  [ -z "$claims" ] || claims+=$'\n'
  claims+="$worker_claims"
fi
if [ -z "$claims" ]; then gate_failed node-loss/claims "$tree has no live role claims on $pvc"; exit 1; fi
records="$(mktemp)"
cleanup() { rm -f -- "$records"; }
trap cleanup EXIT
node=""
zone=""
count=0
while IFS=$'\t' read -r token role session generation ready pod source; do
  [ -n "$token" ] || continue
  [ -n "$session" ] && [ -n "$generation" ] && [ -n "$ready" ] ||
    { gate_failed node-loss/claims "role $role lacks a sessionId, generation, or ready confirmation"; continue; }
  pod_doc="$(kc get pod "$pod" -o json 2>/dev/null)" || { gate_failed node-loss/claims "could not read pod $pod"; continue; }
  pod_node="$(printf '%s' "$pod_doc" | jq -r '.spec.nodeName // empty')"
  [ -n "$pod_node" ] || { gate_failed node-loss/claims "pod $pod has no assigned node"; continue; }
  if [ -z "$node" ]; then
    node="$pod_node"
    node_doc="$(kc get node "$node" -o json 2>/dev/null)" || { gate_failed node-loss/claims "could not read node $node"; continue; }
    zone="$(printf '%s' "$node_doc" | jq -r '.metadata.labels["topology.kubernetes.io/zone"] // empty')"
    [ -n "$zone" ] || { gate_failed node-loss/claims "node $node has no topology.kubernetes.io/zone"; continue; }
  elif [ "$pod_node" != "$node" ]; then
    gate_failed node-loss/claims "pod $pod is on $pod_node, but the tree is on $node"
    continue
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$token" "$role" "$session" "$generation" "$ready" "$pod" "$node" "$source" >>"$records"
  count=$((count + 1))
done <<<"$claims"
[ "$failures" -eq 0 ] || exit 1
[ "$count" -gt 0 ] || { gate_failed node-loss/claims "no drainable live role claims were recorded"; exit 1; }
gate_ok node-loss/claims "recorded $count role$( [ "$count" = 1 ] || printf s) on $node in $zone"
if kubectl --context "$context" cordon "$node" >/dev/null; then gate_ok node-loss/cordon "$node"; else gate_failed node-loss/cordon "$node"; exit 1; fi
if kubectl --context "$context" drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=10m >/dev/null; then gate_ok node-loss/drain "$node"; else gate_failed node-loss/drain "$node"; exit 1; fi

check_replacements() {
  local fresh token role prior_session prior_generation prior_ready old_pod old_node source claim observed session generation ready pod phase pod_doc new_node new_node_doc new_zone pool phase_pvc prompt
  fresh="$(state 2>/dev/null)" || { check_name="node-loss/daemon-state"; check_reason="GET $state_url failed"; return 1; }
  while IFS=$'\t' read -r token role prior_session prior_generation prior_ready old_pod old_node source; do
    claim="$(printf '%s' "$fresh" | jq -c --arg token "$token" '.roles[$token] // empty')"
    [ -n "$claim" ] && [ "$claim" != null ] || { check_name="node-loss/session-$role"; check_reason="role $role is not yet re-registered"; return 1; }
    if [ "$source" = tree ]; then
      observed="$(printf '%s' "$fresh" | jq -c --arg tree "$tree" '.trees[$tree] // empty')"
    else
      observed="$claim"
    fi
    session="$(printf '%s' "$claim" | jq -r '.sessionId // empty')"
    if [ "$session" != "$prior_session" ]; then check_name="node-loss/session-$role"; check_reason="session $role changed"; return 2; fi
    generation="$(printf '%s' "$observed" | jq -r '.generation // empty')"
    if ! [[ "$generation" =~ ^[0-9]+$ ]] || [ "$generation" -le "$prior_generation" ]; then check_name="node-loss/generation-$role"; check_reason="generation did not advance"; return 1; fi
    ready="$(printf '%s' "$observed" | jq -r '.readyConfirmedAt // empty')"
    if [ -z "$ready" ] || [ "$ready" = "$prior_ready" ]; then check_name="node-loss/ready-$role"; check_reason="readiness was not renewed"; return 1; fi
    pod="$(printf '%s' "$observed" | jq -r '.locator.podName // empty')"
    [ -n "$pod" ] && [ "$pod" != "$old_pod" ] || { check_name="node-loss/pod-$role"; check_reason="replacement pod is not registered yet"; return 1; }
    pod_doc="$(kc get pod "$pod" -o json 2>/dev/null)" || { check_name="node-loss/pod-$role"; check_reason="could not read replacement pod $pod"; return 1; }
    phase="$(printf '%s' "$pod_doc" | jq -r '.status.phase // empty')"
    [ "$phase" = Running ] || { check_name="node-loss/pod-$role"; check_reason="replacement pod $pod is ${phase:-absent}"; return 1; }
    new_node="$(printf '%s' "$pod_doc" | jq -r '.spec.nodeName // empty')"
    [ -n "$new_node" ] && [ "$new_node" != "$old_node" ] || { check_name="node-loss/node-$role"; check_reason="replacement pod has not moved to a new node"; return 1; }
    new_node_doc="$(kc get node "$new_node" -o json 2>/dev/null)" || { check_name="node-loss/node-$role"; check_reason="could not read replacement node $new_node"; return 1; }
    pool="$(printf '%s' "$new_node_doc" | jq -r '.metadata.labels["legion.dev/pool"] // empty')"
    new_zone="$(printf '%s' "$new_node_doc" | jq -r '.metadata.labels["topology.kubernetes.io/zone"] // empty')"
    [ "$pool" = legion ] || { check_name="node-loss/node-$role"; check_reason="replacement node $new_node is not in legion.dev/pool=legion"; return 2; }
    [ "$new_zone" = "$zone" ] || { check_name="node-loss/node-$role"; check_reason="replacement node $new_node is in ${new_zone:-no-zone}, expected $zone"; return 2; }
    phase_pvc="$(kc get pvc "$pvc" -o json 2>/dev/null | jq -r '.status.phase // empty')"
    [ "$phase_pvc" = Bound ] || { check_name="node-loss/pvc-$role"; check_reason="$pvc is ${phase_pvc:-absent}"; return 1; }
    prompt="$(printf '%s' "$pod_doc" | jq -r '[.spec.containers[]? | select(.name == "worker") | .command as $command | $command | to_entries[] | select(.value == "--append-system-prompt") | $command[.key + 1]] | first // empty')"
    [[ "$prompt" != "Your workspace was recreated from"* ]] || { check_name="node-loss/recovery-prompt-$role"; check_reason='present'; return 2; }
  done <"$records"
  return 0
}

wait_seconds="${NODE_LOSS_WAIT:-900}"
interval="${NODE_LOSS_POLL_INTERVAL:-5}"
waited=0
while :; do
  status=0
  check_replacements || status=$?
  [ "$status" = 0 ] && break
  if [ "$status" = 2 ]; then gate_failed "$check_name" "$check_reason"; exit 1; fi
  if [ "$waited" -ge "$wait_seconds" ]; then gate_failed "$check_name" "$check_reason after ${wait_seconds}s"; exit 1; fi
  sleep "$interval"
  waited=$((waited + interval))
done
while IFS=$'\t' read -r _ role _ _ _ _ _ _; do
  gate_ok "node-loss/session-$role" 'session unchanged'
  gate_ok "node-loss/generation-$role" 'generation incremented'
  gate_ok "node-loss/ready-$role" 'readiness renewed'
  gate_ok "node-loss/recovery-prompt-$role" absent
done <"$records"
