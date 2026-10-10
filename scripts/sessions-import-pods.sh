#!/usr/bin/env bash
# Runs `legion sessions import` in one-off pods of the worker image, one per tree, each mounting the
# tree's volume as legion-v10.0.0 left it at /legion and the session database's URL key: steps 3 and
# 4 of docs/kubernetes.md "Copying file sessions before turning it on". It copies only from
# legion-v10.0.0, whose every role runs in a Sandbox of its own: a project whose Sandboxes are issue
# pods (legion-v10.1.0 and later) is refused by name, as that runbook has no copy path for them.
# Run it with the daemon scaled to 0: it refuses while any Sandbox of the project is not Suspended,
# whose pod may still be starting, and while any pod of the project has not ended, since such a pod
# is a writer the copy would miss, and a volume attaches to one node at a time.
#
# Each tree's volume and root Sandbox are found by legion-v10.0.0's own labels (legion.dev/project,
# legion.dev/role=architect, legion.dev/tree and legion.dev/issue: `labels` in
# internal/runtime/sandbox/manifest.go at that tag, which its Sandboxes and their volume claim
# templates carry), and each pod is scheduled as that root Sandbox's pods were: its pod template's
# node selector, tolerations and priority class. It runs as the namespace's default ServiceAccount
# with no token, not as the Sandbox's: it needs no identity, and a namespace admission policy may let
# only the agent-sandbox controller create pods as the worker ServiceAccount. Each pod runs under
# gVisor as the image's user, prints the import's lines, and is deleted; the script exits 1 when
# any import did.
#
# usage: scripts/sessions-import-pods.sh <claims.json> <namespace> <worker image@sha256> <project> <url secret> <url key> [<kubectl args>…]
#   <claims.json>  what `legion claims list --json` printed before the daemon stopped
#   <project>      legion.yaml's project; its token is lowercased with every non-alphanumeric removed
#   <url secret>   the Secret holding the session database's postgres:// URL, <url key> its key
#                  (the providers Secret and session_dsn_secret, as runtime.kubernetes names them)
#   <kubectl args> passed to every kubectl call, e.g. --context <restricted context>
set -euo pipefail

# Prints the header's usage block, from its `# usage:` line through its `<kubectl args>` line, so a
# header edit above or within it never shifts what is printed.
usage() {
  sed -n '/^# usage: /,/^#   <kubectl args>/p' "$0" >&2
  exit 2
}

(($# >= 6)) || usage
claims=$1 namespace=$2 image=$3 project=$4 secret=$5 key=$6
shift 6
kubectl=(kubectl -n "$namespace" "$@")
[[ $image == *@sha256:* ]] || { echo "sessions-import-pods: image $image is not pinned by digest" >&2; exit 2; }
token=$(tr -cd '[:alnum:]' <<<"$project" | tr '[:upper:]' '[:lower:]')
selector="legion.dev/project=$token"

# The release: legion-v10.0.0 labels every Sandbox with its role; an issue pod of a later release
# carries an issue label and no role.
issue_pods=$("${kubectl[@]}" get sandboxes -l "$selector,legion.dev/issue,!legion.dev/role,!legion.dev/probe" -o name)
if [[ -n $issue_pods ]]; then
  echo "sessions-import-pods: project $token runs issue pods ($(wc -l <<<"$issue_pods") Sandboxes with an issue label and no role, e.g. ${issue_pods%%$'\n'*}), so it is on legion-v10.1.0 or later; this script copies only from legion-v10.0.0's per-claim Sandboxes (docs/kubernetes.md \"Copying file sessions before turning it on\")" >&2
  exit 2
fi
# A Sandbox not Suspended (legion-v10.0.0 reads an unset operating mode as Running) can start a
# pod after the pod check below, so every one must be Suspended first.
awake=$("${kubectl[@]}" get sandboxes -l "$selector,!legion.dev/probe" -o json |
  jq -r '.items[] | select((.spec.operatingMode // "Running") != "Suspended") | "sandbox/\(.metadata.name)"')
if [[ -n $awake ]]; then
  echo "sessions-import-pods: $(wc -l <<<"$awake") Sandbox(es) of project $token are not Suspended, e.g. ${awake%%$'\n'*}: its pod can still start and write its session after the copy; suspend every claim and scale the daemon to 0 first (steps 1 and 2)" >&2
  exit 2
fi
unended=$("${kubectl[@]}" get pods -l "$selector,!legion.dev/sessions-import" --field-selector=status.phase!=Succeeded,status.phase!=Failed -o name)
if [[ -n $unended ]]; then
  echo "sessions-import-pods: $(wc -l <<<"$unended") pod(s) of project $token have not ended, e.g. ${unended%%$'\n'*}: such an agent writes its session after the copy; suspend every claim and scale the daemon to 0 first (steps 1 and 2)" >&2
  exit 2
fi

trees=$(jq -r '.claims[] | select(.sessionFile != "" and .tree != "") | .tree' "$claims" | sort -u)
while read -r untreed; do
  [[ -n $untreed ]] && echo "sessions-import-pods: $untreed records a session and belongs to no tree; step 5 marks it lost" >&2
done < <(jq -r '.claims[] | select(.sessionFile != "" and .tree == "") | .token' "$claims")
[[ -n $trees ]] || { echo "sessions-import-pods: $claims records no session of any tree" >&2; exit 0; }

configmap=legion-sessions-import-claims
"${kubectl[@]}" delete configmap "$configmap" --ignore-not-found >/dev/null
"${kubectl[@]}" create configmap "$configmap" --from-file=claims.json="$claims" >/dev/null
trap '"${kubectl[@]}" delete configmap "$configmap" --ignore-not-found >/dev/null' EXIT

failed=0
for tree in $trees; do
  if ((${#tree} > 63)); then
    echo "sessions-import-pods: tree $tree is past a label value's 63 characters, which legion-v10.0.0 hashed; copy it by hand" >&2
    failed=1
    continue
  fi
  root="$selector,legion.dev/role=architect,legion.dev/tree=$tree,legion.dev/issue=$tree"
  volume=$("${kubectl[@]}" get pvc -l "$root" -o jsonpath='{.items[*].metadata.name}')
  if [[ -z $volume || $volume == *" "* ]]; then
    echo "sessions-import-pods: tree $tree has ${volume:-no} volume labelled $root, want one: its sessions can only be marked lost (step 5)" >&2
    failed=1
    continue
  fi
  # The scheduling legion-v10.0.0 gave the root Sandbox's pods, which a cluster's admission may
  # require of every pod on the Legion pool: an absent field is the API's default.
  scheduling=$("${kubectl[@]}" get sandboxes -l "$root" -o json | jq -ce 'if (.items | length) == 1 then .items[0].spec.podTemplate.spec |
    {nodeSelector: (.nodeSelector // {}), tolerations: (.tolerations // []),
     priorityClassName: (.priorityClassName // "")} else empty end') || {
    echo "sessions-import-pods: tree $tree has no one root Sandbox labelled $root to schedule its import as; copy it by hand" >&2
    failed=1
    continue
  }
  pod="legion-sessions-import-$(tr '[:upper:]' '[:lower:]' <<<"$tree")"
  # A pod a run interrupted before its delete would report its own output as this run's.
  "${kubectl[@]}" delete pod "$pod" --ignore-not-found --wait >/dev/null
  "${kubectl[@]}" create -f - >/dev/null <<EOF
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
  securityContext: {runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  volumes:
    - {name: tree, persistentVolumeClaim: {claimName: "$volume", readOnly: true}}
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
  # Waits for the pod to end, Succeeded or Failed, for up to 15 minutes, so a pod that fails is
  # reported at once.
  deadline=$((SECONDS + 900))
  while :; do
    phase=$("${kubectl[@]}" get pod "$pod" -o jsonpath='{.status.phase}')
    [[ $phase == Succeeded || $phase == Failed ]] && break
    ((SECONDS < deadline)) || break
    sleep 5
  done
  echo "== $tree ($volume)"
  "${kubectl[@]}" logs "$pod" || true
  [[ $phase == Succeeded ]] || { echo "sessions-import-pods: tree $tree's import pod ended ${phase:-without a phase}" >&2; failed=1; }
  "${kubectl[@]}" delete pod "$pod" --wait >/dev/null
done
exit "$failed"
