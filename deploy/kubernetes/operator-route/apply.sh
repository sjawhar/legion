#!/usr/bin/env bash
# Creates or updates the durable ConfigMap this directory's pod.yml mounts, so every Legion pod in
# the namespace reads the operator's model route. Idempotent: re-run it to change the route.
#
#   deploy/kubernetes/operator-route/apply.sh --base-url https://<gateway>/anthropic \
#     [--context <kubectl context>] [--namespace legion] [--name legion-operator-route] [--dry-run]
#
# --base-url is the operator's model gateway endpoint, which replaces models.yml's placeholder; the
# repository carries no default, and nothing else in the pair is substituted. The ConfigMap is
# applied with no legion.dev/project label, so a live harness run's teardown — which deletes by that
# label — leaves it alone.
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
    [ $# -ge 2 ] || refuse "--base-url needs the model gateway endpoint"
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
[ -n "$base_url" ] || refuse "--base-url is required: the model gateway endpoint models.yml's provider calls"
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

kube=(kubectl)
[ -z "$context" ] || kube+=(--context "$context")
kube+=(--namespace "$namespace")

"${kube[@]}" create configmap "$name" \
  --from-file=models.yml="$work/models.yml" --from-file=overlay.yml="$here/overlay.yml" \
  --dry-run=client -o yaml >"$work/configmap.yaml"

if [ -n "$dry_run" ]; then
  cat "$work/configmap.yaml"
  echo "$me: --dry-run: ConfigMap $name for namespace $namespace was not applied"
  exit 0
fi

"${kube[@]}" apply -f "$work/configmap.yaml" >/dev/null ||
  fail "could not apply ConfigMap $name in namespace $namespace"
echo "$me: applied ConfigMap $name in namespace $namespace: models.yml (baseUrl $base_url) and overlay.yml"
