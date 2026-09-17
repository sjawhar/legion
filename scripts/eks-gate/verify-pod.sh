#!/usr/bin/env bash
# Gate 3 pod-spec proof for a running Legion worker.
set -euo pipefail
# shellcheck source=scripts/eks-gate/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

context=""
pod_name=""
failures=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) context="${2:-}"; shift 2 ;;
    -*) gate_failed verify-pod/arguments "unknown argument $1"; exit 2 ;;
    *) [ -z "$pod_name" ] || { gate_failed verify-pod/arguments "unexpected pod $1"; exit 2; }; pod_name="$1"; shift ;;
  esac
done
require_context verify-pod
if [ -z "$pod_name" ]; then gate_failed verify-pod/pod 'a pod name is required'; exit 2; fi
pod="$(kc get pod "$pod_name" -o json 2>/dev/null)" || { gate_failed verify-pod/pod "$pod_name was not found"; exit 1; }
node="$(printf '%s' "$pod" | jq -r '.spec.nodeName // empty')"
if [ -z "$node" ]; then
  gate_failed verify-pod/node-pool 'pod has no assigned node'
else
  node_doc="$(kc get node "$node" -o json 2>/dev/null)" || node_doc='{}'
  if [ "$(printf '%s' "$node_doc" | jq -r '.metadata.labels["legion.dev/pool"] // empty')" = legion ]; then
    gate_ok verify-pod/node-pool "$node is labelled legion.dev/pool=legion"
  else
    gate_failed verify-pod/node-pool "$node is not labelled legion.dev/pool=legion"
  fi
fi
if [ "$(printf '%s' "$pod" | jq -r '.metadata.annotations["karpenter.sh/do-not-disrupt"] // empty')" = true ]; then gate_ok verify-pod/do-not-disrupt; else gate_failed verify-pod/do-not-disrupt 'annotation is absent'; fi
if printf '%s' "$pod" | jq -e '.spec.tolerations[]? | select(.key == "legion.dev/pool" and .operator == "Equal" and .value == "legion" and .effect == "NoSchedule")' >/dev/null; then gate_ok verify-pod/toleration; else gate_failed verify-pod/toleration 'legion pool taint is not tolerated'; fi
if [ "$(printf '%s' "$pod" | jq -r '.spec.priorityClassName // empty')" = legion ]; then gate_ok verify-pod/priority-class; else gate_failed verify-pod/priority-class 'priorityClassName is not legion'; fi
bad_security="$(printf '%s' "$pod" | jq -r '[ (.spec.containers[]?, .spec.initContainers[]?) | select(.securityContext.allowPrivilegeEscalation != false or .securityContext.capabilities.drop != ["ALL"]) | .name ] | first // empty')"
if [ -z "$bad_security" ]; then gate_ok verify-pod/security-context; else gate_failed verify-pod/security-context "container $bad_security is not restricted"; fi
bad_env="$(printf '%s' "$pod" | jq -r '[.spec.containers[]? | .name as $container | .env[]? | select((.value // "") | test("^(sk-|ghp_|github_pat_|xox)")) | "\($container)/\(.name)"] | first // empty')"
if [ -z "$bad_env" ]; then gate_ok verify-pod/no-secret-env; else gate_failed verify-pod/no-secret-env "$bad_env has a secret-looking value"; fi
pvc="$(printf '%s' "$pod" | jq -r '[.spec.volumes[]?.persistentVolumeClaim.claimName // empty] | first // empty')"
if [ -z "$pvc" ]; then
  gate_failed verify-pod/pvc-bound 'pod mounts no PVC'
else
  phase="$(kc get pvc "$pvc" -o json 2>/dev/null | jq -r '.status.phase // empty')"
  if [ "$phase" = Bound ]; then gate_ok verify-pod/pvc-bound "$pvc Bound"; else gate_failed verify-pod/pvc-bound "$pvc is ${phase:-absent}"; fi
fi
[ "$failures" -eq 0 ] || exit 1
