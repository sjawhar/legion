#!/usr/bin/env bash
# Runs `legion sessions import` once per tree a claims list names, each in a one-off pod of the
# worker image that mounts that tree's volume at /legion and the session database's URL key: steps
# 3 and 4 of docs/kubernetes.md "Copying file sessions before turning it on". Run it with the
# daemon scaled to 0 and no pod of any tree running, since a tree volume attaches to one node at a
# time. Each pod runs on the Legion pool under gVisor as the image's user, as the trees' own pods
# did, prints the import's lines, and is deleted; the script exits 1 when any tree's import did.
#
# usage: scripts/sessions-import-pods.sh <claims.json> <namespace> <worker image@sha256> <project> <url secret> <url key> [<kubectl args>…]
#   <claims.json>  what `legion claims list --json` printed before the daemon stopped
#   <project>      legion.yaml's project; the tree volume of tree T is the earlier release's
#                  tree-legion-<project token>-<t>-architect (docs/kubernetes.md, "Upgrading a
#                  deployment with running trees"), the project token lowercased with every
#                  non-alphanumeric removed
#   <url secret>   the Secret holding the session database's postgres:// URL, <url key> its key
#                  (the providers Secret and session_dsn_secret, as runtime.kubernetes names them)
#   <kubectl args> passed to every kubectl call, e.g. --context <restricted context>
set -euo pipefail

if (($# < 6)); then
  sed -n '9,18p' "$0" >&2
  exit 2
fi
claims=$1 namespace=$2 image=$3 project=$4 secret=$5 key=$6
shift 6
kubectl=(kubectl -n "$namespace" "$@")
[[ $image == *@sha256:* ]] || { echo "sessions-import-pods: image $image is not pinned by digest" >&2; exit 2; }
token=$(tr -cd '[:alnum:]' <<<"$project" | tr '[:upper:]' '[:lower:]')
trees=$(jq -r '.claims[] | select(.sessionFile != "") | .tree' "$claims" | sort -u)
[[ -n $trees ]] || { echo "sessions-import-pods: $claims records no session" >&2; exit 0; }

configmap=legion-sessions-import-claims
"${kubectl[@]}" create configmap "$configmap" --from-file=claims.json="$claims" --dry-run=client -o yaml | "${kubectl[@]}" apply -f - >/dev/null
trap '"${kubectl[@]}" delete configmap "$configmap" --ignore-not-found >/dev/null' EXIT

failed=0
for tree in $trees; do
  lower=$(tr '[:upper:]' '[:lower:]' <<<"$tree")
  pvc="tree-legion-$token-$lower-architect"
  pod="legion-sessions-import-$lower"
  if ((${#pvc} > 63)); then
    echo "sessions-import-pods: $tree's volume name $pvc is past 63 characters, which the earlier release hashed; copy it by hand" >&2
    failed=1
    continue
  fi
  if ! "${kubectl[@]}" get pvc "$pvc" >/dev/null 2>&1; then
    echo "sessions-import-pods: $tree has no volume $pvc: its sessions can only be marked lost (--mark-lost)" >&2
    failed=1
    continue
  fi
  "${kubectl[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  labels: {legion.dev/project: "$token", legion.dev/sessions-import: "true"}
spec:
  restartPolicy: Never
  runtimeClassName: gvisor
  automountServiceAccountToken: false
  enableServiceLinks: false
  nodeSelector: {legion.dev/pool: legion}
  tolerations: [{key: legion.dev/pool, operator: Equal, value: legion, effect: NoSchedule}]
  securityContext: {runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  volumes:
    - {name: tree, persistentVolumeClaim: {claimName: "$pvc", readOnly: true}}
    - {name: url, secret: {secretName: "$secret", items: [{key: "$key", path: OMP_SESSION_SQL_DSN}], defaultMode: 0440}}
    - {name: claims, configMap: {name: "$configmap"}}
  containers:
    - name: import
      image: "$image"
      args: [sessions, import, --dsn-file, /var/run/legion/sessions/OMP_SESSION_SQL_DSN, --claims, /var/run/legion/claims/claims.json, --tree, "$tree", --tree-volume, /legion]
      volumeMounts:
        - {name: tree, mountPath: /legion, readOnly: true}
        - {name: url, mountPath: /var/run/legion/sessions, readOnly: true}
        - {name: claims, mountPath: /var/run/legion/claims, readOnly: true}
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, seccompProfile: {type: RuntimeDefault}}
EOF
  "${kubectl[@]}" wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=15m "pod/$pod" >/dev/null 2>&1 ||
    "${kubectl[@]}" wait --for=jsonpath='{.status.phase}'=Failed --timeout=10s "pod/$pod" >/dev/null 2>&1 || true
  echo "== $tree ($pvc)"
  "${kubectl[@]}" logs "$pod" || true
  phase=$("${kubectl[@]}" get pod "$pod" -o jsonpath='{.status.phase}')
  [[ $phase == Succeeded ]] || { echo "sessions-import-pods: $tree's import pod ended $phase" >&2; failed=1; }
  "${kubectl[@]}" delete pod "$pod" --wait >/dev/null
done
exit "$failed"
