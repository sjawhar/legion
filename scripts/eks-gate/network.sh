#!/usr/bin/env bash
# Gate 2 network and storage proof: dedicated-pool reachability and default-pool refusal.
set -euo pipefail
# shellcheck source=scripts/eks-gate/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

context=""
devbox_ip=""
failures=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) context="${2:-}"; shift 2 ;;
    --devbox-ip) devbox_ip="${2:-}"; shift 2 ;;
    *) gate_failed network/arguments "unknown argument $1"; exit 2 ;;
  esac
done
require_context network
imds_header=""
cleanup() {
  local status=$?
  [ -z "$imds_header" ] || rm -f -- "$imds_header"
  kc delete pod legion-gate2 --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kc delete pod legion-gate2-default --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kc delete pvc legion-gate2 --ignore-not-found --wait=true >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

[ -n "$devbox_ip" ] || {
  imds_header="$(mktemp)" || { gate_failed network/devbox-ip 'could not create an IMDSv2 header file'; exit 1; }
  imds_token="$(curl -fsS -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)" || {
    gate_failed network/devbox-ip 'could not obtain an IMDSv2 token'; exit 1;
  }
  printf 'X-aws-ec2-metadata-token: %s\n' "$imds_token" >"$imds_header"
  devbox_ip="$(curl -fsS -H "@$imds_header" http://169.254.169.254/latest/meta-data/local-ipv4)" || {
    gate_failed network/devbox-ip 'could not read IMDSv2 local-ipv4'; exit 1;
  }
}

if ! cat <<'YAML' | kc apply -f - >/dev/null; then
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: legion-gate2, namespace: legion }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: gp2
  resources: { requests: { storage: 1Gi } }
---
apiVersion: v1
kind: Pod
metadata: { name: legion-gate2, namespace: legion }
spec:
  restartPolicy: Never
  nodeSelector: { legion.dev/pool: legion }
  tolerations: [{ key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule }]
  priorityClassName: legion
  volumes: [{ name: tree, persistentVolumeClaim: { claimName: legion-gate2 } }]
  containers:
    - name: nc
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts: [{ name: tree, mountPath: /v }]
---
apiVersion: v1
kind: Pod
metadata: { name: legion-gate2-default, namespace: legion }
spec:
  restartPolicy: Never
  containers:
    - name: nc
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
YAML
  gate_failed network/setup 'could not apply gate-2 PVC and pods'
  exit 1
fi
if ! kc wait --for=condition=Ready pod/legion-gate2 --timeout=10m; then
  gate_failed network/legion-pool-ready 'legion-gate2 did not become Ready within 10m'
  exit 1
fi
if ! kc wait --for=condition=Ready pod/legion-gate2-default --timeout=10m; then
  gate_failed network/default-pool-ready 'legion-gate2-default did not become Ready within 10m'
  exit 1
fi
pvc_phase="$(kc get pvc legion-gate2 -o json | jq -r '.status.phase // empty')" || pvc_phase=""
if [ "$pvc_phase" = Bound ]; then gate_ok network/pvc-bound 'legion-gate2 Bound'; else gate_failed network/pvc-bound "legion-gate2 is ${pvc_phase:-absent}"; fi

targets=(
  'nats nats.internal.trajectorylabs.com 4222'
  'envoy-listener envoy-listener.internal.trajectorylabs.com 9020'
  'dispatch dispatch.internal.trajectorylabs.com 443'
  "daemon $devbox_ip 13370"
  "worker-stream $devbox_ip 13371"
)
for target in "${targets[@]}"; do
  read -r name host port <<<"$target"
  if kc exec legion-gate2 -- nc -zv -w3 "$host" "$port" >/dev/null 2>&1; then
    gate_ok "network/legion-pool-reaches $name:$port"
  else
    gate_failed "network/legion-pool-reaches $name:$port" 'refused'
  fi
  if kc exec legion-gate2-default -- nc -zv -w3 "$host" "$port" >/dev/null 2>&1; then
    if [ "$name" = dispatch ]; then
      gate_ok "network/default-pool-reaches $name:$port"
    else
      gate_failed "network/default-pool-refused $name:$port" reachable
    fi
  elif [ "$name" = dispatch ]; then
    gate_failed "network/default-pool-reaches $name:$port" refused
  else
    gate_ok "network/default-pool-refused $name:$port"
  fi
done
provider_id="$(kc get nodes -l legion.dev/pool=legion -o jsonpath='{.items[0].spec.providerID}')" || provider_id=""
instance_id="${provider_id##*/}"
if [ -z "$provider_id" ] || [ "$instance_id" = "$provider_id" ]; then
  gate_failed network/node-security-groups 'legion pool node has no EC2 providerID'
else
  groups="$(aws ec2 describe-instances --instance-ids "$instance_id" --query 'Reservations[0].Instances[0].SecurityGroups[].GroupName' --output text 2>/dev/null)" || groups=""
  if [[ "$groups" == *eks-cluster-sg-production* && "$groups" == *legion-nodes* ]]; then
    gate_ok network/node-security-groups 'cluster and legion-nodes'
  else
    gate_failed network/node-security-groups 'expected the cluster and legion-nodes security groups'
  fi
fi

[ "$failures" -eq 0 ] || exit 1
