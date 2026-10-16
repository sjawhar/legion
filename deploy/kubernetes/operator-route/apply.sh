#!/usr/bin/env bash
# Writes the operator's models.yml and overlay.yml, read from the directory it is given, into the
# ConfigMap this directory's pod.yml mounts: legion-operator-route in namespace legion. The operator
# keeps both files in that directory, and this script writes the ConfigMap from them: to change the
# model a role uses, edit overlay.yml there and run it again.
#
#   deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>
#
# --context is required: kubectl's current context is never assumed, since more than one cluster can
# hold a namespace of the same name. The line reporting what was applied names the cluster that
# context points at. Both files go in as they are; a models.yml still holding the example's baseUrl
# placeholder is refused, since nothing expands it.
#
# It writes with a server-side apply under its own field manager, legion-operator-route-apply, and
# forces conflicts: the two data keys become that manager's whichever manager held them before (an
# earlier client-side apply, or the server-side apply of a tool that declared the ConfigMap before
# the operator's directory did), so the apply never fails on a field-manager conflict, and every
# other field, such as another manager's labels and annotations, stays as that manager left it.
# Before it applies, it names on stderr each other field manager holding a data key, kubectl's
# client-side apply (an earlier run of this script) aside. A tool that still declares the ConfigMap
# acts at its own next apply: one that forces conflicts writes its own copy back, and one that does
# not fails on any key whose value differs, leaving this script's copy in place. Stop it declaring
# the ConfigMap before running this.
#
# Re-running it updates the ConfigMap, but pod.yml mounts both files by subPath, which the kubelet
# never refreshes: pods started after it read the new files, and a running pod keeps the ones it
# started with until it restarts. The ConfigMap carries no legion.dev/project label; a live harness
# run deletes only objects labelled with its own run's project, so it leaves this one alone.
#
# Stdout says what was applied, and stderr each other field manager that held a data key. A refusal
# names what is wrong and exits 1; bad arguments exit 2.
set -euo pipefail

me=operator-route/apply.sh
namespace=legion
name=legion-operator-route
manager=legion-operator-route-apply
# shellcheck disable=SC2016  # the example models.yml's literal placeholder, not an expansion
placeholder='${MODEL_BASE_URL}'

refuse() {
  echo "$me: $*" >&2
  exit 2
}
fail() {
  echo "$me: $*" >&2
  exit 1
}

context=
dir=
while [ $# -gt 0 ]; do
  case "$1" in
  --context)
    [ $# -ge 2 ] || refuse "--context needs a kube context"
    context=$2
    shift 2
    ;;
  -*) refuse "unknown argument '$1'" ;;
  *)
    [ -z "$dir" ] || refuse "one directory, not '$dir' and '$1'"
    dir=$1
    shift
    ;;
  esac
done

[ -n "$context" ] || refuse "--context is required: name the kube context whose cluster should hold the route"
[ -n "$dir" ] || refuse "name the directory holding the operator's models.yml and overlay.yml"
[ -d "$dir" ] || fail "$dir is not a directory"
models=$dir/models.yml
overlay=$dir/overlay.yml
for file in "$models" "$overlay"; do
  [ -f "$file" ] && [ -r "$file" ] || fail "$file is not a readable file"
done
# A placeholder on a line before any comment is a value nothing will expand.
if grep -qE '^[^#]*\$\{MODEL_BASE_URL\}' "$models"; then
  fail "$models still holds $placeholder: put the model endpoint's base URL in its place"
fi

cluster=$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$context\")].context.cluster}") ||
  fail "could not read the kubeconfig to resolve --context $context"
[ -n "$cluster" ] || fail "--context $context names no context in the kubeconfig"
target="namespace $namespace of cluster $cluster (context $context)"
kube=(kubectl --context "$context" --namespace "$namespace")

work=$(mktemp -d /tmp/legion-operator-route.XXXXXXXX)
trap 'rm -rf "$work"' EXIT
"${kube[@]}" create configmap "$name" \
  --from-file=models.yml="$models" --from-file=overlay.yml="$overlay" \
  --dry-run=client -o yaml >"$work/configmap.yaml"
# Each managedFields entry as its manager, a tab, then the keys it holds under data. A ConfigMap not
# there yet gives nothing.
# shellcheck disable=SC2016  # Go template variables, not shell expansions
holders='{{range .metadata.managedFields}}{{.manager}}{{"\t"}}{{range $key, $_ := index .fieldsV1 "f:data"}}{{$key}} {{end}}{{"\n"}}{{end}}'
held=$("${kube[@]}" get configmap "$name" --ignore-not-found --show-managed-fields -o go-template="$holders") ||
  fail "could not read ConfigMap $name in $target"
while IFS=$'\t' read -r holder fields; do
  case "$holder" in "" | "$manager" | kubectl-client-side-apply) continue ;; esac
  taken=
  case " $fields " in *" f:models.yml "*) taken=data.models.yml ;; esac
  case " $fields " in *" f:overlay.yml "*) taken+="${taken:+ and }data.overlay.yml" ;; esac
  [ -z "$taken" ] ||
    echo "$me: field manager $holder holds $taken of ConfigMap $name in $target, which this apply writes as $manager: while that tool still declares the ConfigMap, its next apply writes its own copy back if it forces conflicts, and fails on any value that differs if it does not" >&2
done <<<"$held"
"${kube[@]}" apply --server-side --force-conflicts --field-manager="$manager" -f "$work/configmap.yaml" >/dev/null ||
  fail "could not apply ConfigMap $name in $target"
echo "$me: applied ConfigMap $name in $target: models.yml and overlay.yml from $dir; pods started from now on read them, and a running pod keeps its own until it restarts"
