#!/usr/bin/env bash
# Runs `legion sessions import` in one-off pods of the worker image, each mounting a volume of the
# earlier release at /legion and the session database's URL key: steps 3 and 4 of docs/kubernetes.md
# "Copying file sessions before turning it on". Run it with the daemon scaled to 0 and no pod of any
# tree running, since a volume attaches to one node at a time. Each pod runs under gVisor as the
# image's user and is scheduled as the pods of the Sandbox whose volume it reads were: that Sandbox's
# pod template's node selector, tolerations, priority class and service account. It prints the
# import's lines and is deleted; the script exits 1 when any import did.
#
# usage: scripts/sessions-import-pods.sh [--claim <token> --pvc <name>] <claims.json> <namespace> <worker image@sha256> <project> <url secret> <url key> [<kubectl args>…]
#   --claim, --pvc copy the one claim <token> from the volume <name> instead of every tree, scheduled
#                  as the claim's own Sandbox is: a claim that belongs to no tree
#   <claims.json>  what `legion claims list --json` printed before the daemon stopped
#   <project>      legion.yaml's project; tree T's volume is the earlier release's
#                  tree-<root Sandbox>, the root Sandbox being the claim legion-<project token>-<t>-architect's
#                  (docs/kubernetes.md, "Upgrading a deployment with running trees"), the project
#                  token lowercased with every non-alphanumeric removed
#   <url secret>   the Secret holding the session database's postgres:// URL, <url key> its key
#                  (the providers Secret and session_dsn_secret, as runtime.kubernetes names them)
#   <kubectl args> passed to every kubectl call, e.g. --context <restricted context>
set -euo pipefail

usage() {
  sed -n '10,20p' "$0" >&2
  exit 2
}

# sandbox_name is the earlier release's SandboxName of a claim token (dnsName in
# internal/runtime/sandbox/names.go at legion-v10.0.0): lowercased, every character outside
# [a-z0-9-] a dash, dash runs collapsed and dashes at either end trimmed, and past 63 characters its
# first 54, a dash and the first 8 hex of the token's sha256.
sandbox_name() {
  local slug
  slug=$(tr '[:upper:]' '[:lower:]' <<<"$1" | sed -E 's/[^a-z0-9-]/-/g; s/-+/-/g; s/^-//; s/-$//')
  if ((${#slug} > 63)); then
    slug="${slug:0:54}-$(printf '%s' "$1" | sha256sum | cut -c1-8)"
  fi
  printf '%s\n' "$slug"
}

claim='' pvc=''
while (($#)); do
  case $1 in
  --claim)
    (($# >= 2)) || usage
    claim=$2
    shift 2
    ;;
  --pvc)
    (($# >= 2)) || usage
    pvc=$2
    shift 2
    ;;
  *) break ;;
  esac
done
if [[ -n $claim$pvc && (-z $claim || -z $pvc) ]]; then
  echo "sessions-import-pods: --claim and --pvc are given together" >&2
  exit 2
fi
(($# >= 6)) || usage
claims=$1 namespace=$2 image=$3 project=$4 secret=$5 key=$6
shift 6
kubectl=(kubectl -n "$namespace" "$@")
[[ $image == *@sha256:* ]] || { echo "sessions-import-pods: image $image is not pinned by digest" >&2; exit 2; }
token=$(tr -cd '[:alnum:]' <<<"$project" | tr '[:upper:]' '[:lower:]')

# runs is one line per pod: its name, the Sandbox whose pods it is scheduled as, the volume it
# mounts, and the import's selection flag and value.
runs=()
if [[ -n $claim ]]; then
  jq -e --arg c "$claim" 'any(.claims[]; .token == $c)' "$claims" >/dev/null ||
    { echo "sessions-import-pods: $claims names no claim $claim" >&2; exit 2; }
  runs+=("legion-sessions-import-claim $(sandbox_name "$claim") $pvc --claim $claim")
else
  trees=$(jq -r '.claims[] | select(.sessionFile != "" and .tree != "") | .tree' "$claims" | sort -u)
  while read -r untreed; do
    [[ -n $untreed ]] && echo "sessions-import-pods: $untreed records a session and belongs to no tree; copy it with --claim $untreed --pvc <its volume>, or mark it lost" >&2
  done < <(jq -r '.claims[] | select(.sessionFile != "" and .tree == "") | .token' "$claims")
  [[ -n $trees ]] || { echo "sessions-import-pods: $claims records no session of any tree" >&2; exit 0; }
  for tree in $trees; do
    lower=$(tr '[:upper:]' '[:lower:]' <<<"$tree")
    root=$(sandbox_name "legion-$token-$lower-architect")
    runs+=("legion-sessions-import-$lower $root tree-$root --tree $tree")
  done
fi

configmap=legion-sessions-import-claims
"${kubectl[@]}" create configmap "$configmap" --from-file=claims.json="$claims" --dry-run=client -o yaml | "${kubectl[@]}" apply -f - >/dev/null
trap '"${kubectl[@]}" delete configmap "$configmap" --ignore-not-found >/dev/null' EXIT

failed=0
for run in "${runs[@]}"; do
  read -r pod sandbox volume flag selected <<<"$run"
  if ! "${kubectl[@]}" get pvc "$volume" >/dev/null 2>&1; then
    echo "sessions-import-pods: $selected has no volume $volume: its sessions can only be marked lost (--mark-lost)" >&2
    failed=1
    continue
  fi
  # The scheduling the earlier release gave the Sandbox's pods, which a cluster's admission may
  # require of every pod on the Legion pool: an absent field is the API's default.
  if ! scheduling=$("${kubectl[@]}" get sandboxes "$sandbox" -o json | jq -ce '.spec.podTemplate.spec |
    {nodeSelector: (.nodeSelector // {}), tolerations: (.tolerations // []),
     priorityClassName: (.priorityClassName // ""), serviceAccountName: (.serviceAccountName // "")}'); then
    echo "sessions-import-pods: $selected has no Sandbox $sandbox to schedule its import as; copy it by hand" >&2
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
  nodeSelector: $(jq -c .nodeSelector <<<"$scheduling")
  tolerations: $(jq -c .tolerations <<<"$scheduling")
  priorityClassName: $(jq -c .priorityClassName <<<"$scheduling")
  serviceAccountName: $(jq -c .serviceAccountName <<<"$scheduling")
  securityContext: {runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  volumes:
    - {name: tree, persistentVolumeClaim: {claimName: "$volume", readOnly: true}}
    - {name: url, secret: {secretName: "$secret", items: [{key: "$key", path: OMP_SESSION_SQL_DSN}], defaultMode: 0440}}
    - {name: claims, configMap: {name: "$configmap"}}
  containers:
    - name: import
      image: "$image"
      args: [sessions, import, --dsn-file, /var/run/legion/sessions/OMP_SESSION_SQL_DSN, --claims, /var/run/legion/claims/claims.json, "$flag", "$selected", --tree-volume, /legion]
      volumeMounts:
        - {name: tree, mountPath: /legion, readOnly: true}
        - {name: url, mountPath: /var/run/legion/sessions, readOnly: true}
        - {name: claims, mountPath: /var/run/legion/claims, readOnly: true}
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, seccompProfile: {type: RuntimeDefault}}
EOF
  "${kubectl[@]}" wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=15m "pod/$pod" >/dev/null 2>&1 ||
    "${kubectl[@]}" wait --for=jsonpath='{.status.phase}'=Failed --timeout=10s "pod/$pod" >/dev/null 2>&1 || true
  echo "== $selected ($volume)"
  "${kubectl[@]}" logs "$pod" || true
  phase=$("${kubectl[@]}" get pod "$pod" -o jsonpath='{.status.phase}')
  [[ $phase == Succeeded ]] || { echo "sessions-import-pods: $selected's import pod ended $phase" >&2; failed=1; }
  "${kubectl[@]}" delete pod "$pod" --wait >/dev/null
done
exit "$failed"
