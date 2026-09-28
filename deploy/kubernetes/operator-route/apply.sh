#!/usr/bin/env bash
# Creates or updates the ConfigMap this directory's pod.yml mounts, holding the operator's
# models.yml and overlay.yml.
#
#   deploy/kubernetes/operator-route/apply.sh --context <kubectl context> --base-url https://<endpoint> \
#     [--namespace legion] [--name legion-operator-route] [--dry-run]
#
# --context is required: kubectl's current context is never assumed, since more than one cluster can
# hold a namespace of the same name. The line reporting what was applied, or with --dry-run what
# would have been, names the cluster that context points at.
# --base-url is the model endpoint, which replaces models.yml's placeholder; the repository carries
# no default, and nothing else in the pair is substituted.
#
# Re-running it updates the ConfigMap, but pod.yml mounts both files by subPath, which the kubelet
# never refreshes: a change reaches only pods created after it. The ConfigMap carries no
# legion.dev/project label; a live harness run deletes only objects labelled with its own run's
# project, so it leaves this one alone.
#
# Stdout says what was applied. A refusal names what is wrong and exits 1; bad arguments exit 2.
set -euo pipefail

me=operator-route/apply.sh
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck disable=SC2016  # the operator route's literal placeholder, not an expansion
placeholder='${MODEL_BASE_URL}'

refuse() {
  echo "$me: $*" >&2
  exit 2
}
fail() {
  echo "$me: $*" >&2
  exit 1
}

base_url=
context=
namespace=legion
name=legion-operator-route
dry_run=
while [ $# -gt 0 ]; do
  case "$1" in
  --base-url)
    [ $# -ge 2 ] || refuse "--base-url needs the model endpoint"
    base_url=$2
    shift 2
    ;;
  --context)
    [ $# -ge 2 ] || refuse "--context needs a kubectl context"
    context=$2
    shift 2
    ;;
  --namespace)
    [ $# -ge 2 ] || refuse "--namespace needs a namespace"
    namespace=$2
    shift 2
    ;;
  --name)
    [ $# -ge 2 ] || refuse "--name needs a ConfigMap name"
    name=$2
    shift 2
    ;;
  --dry-run)
    dry_run=1
    shift
    ;;
  *) refuse "unknown argument '$1'" ;;
  esac
done

# The same shape lib/model-gateway-url.sh holds the harnesses to: the value becomes a plain YAML
# scalar in models.yml, so a character outside :/._~- or a trailing colon would change what YAML
# reads.
[ -n "$context" ] || refuse "--context is required: name the kubectl context whose cluster should hold the route"
[ -n "$base_url" ] || refuse "--base-url is required: the model endpoint models.yml's provider calls"
case "$base_url" in
*[!A-Za-z0-9:/._~-]*) refuse "--base-url holds a character other than letters, digits and :/._~-" ;;
*:) refuse "--base-url ends in a colon, which YAML reads as a mapping key" ;;
https://?*) ;;
*) refuse "--base-url is not an https:// URL" ;;
esac

models=$(<"$here/models.yml")
[ "$models" != "${models//"$placeholder"/}" ] || fail "models.yml carries no $placeholder for --base-url to replace"
work=$(mktemp -d /tmp/legion-operator-route.XXXXXXXX)
trap 'rm -rf "$work"' EXIT
printf '%s\n' "${models//"$placeholder"/"$base_url"}" >"$work/models.yml"

cluster=$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$context\")].context.cluster}") ||
  fail "could not read the kubeconfig to resolve --context $context"
[ -n "$cluster" ] || fail "--context $context names no context in the kubeconfig"
target="namespace $namespace of cluster $cluster (context $context)"
kube=(kubectl --context "$context" --namespace "$namespace")

"${kube[@]}" create configmap "$name" \
  --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$here/overlay.yml" \
  --dry-run=client -o yaml >"$work/configmap.yaml"

if [ -n "$dry_run" ]; then
  cat "$work/configmap.yaml"
  echo "$me: --dry-run: ConfigMap $name for $target was not applied"
  exit 0
fi

"${kube[@]}" apply -f "$work/configmap.yaml" >/dev/null ||
  fail "could not apply ConfigMap $name in $target"
echo "$me: applied ConfigMap $name in $target: models.yml (baseUrl $base_url) and overlay.yml"
