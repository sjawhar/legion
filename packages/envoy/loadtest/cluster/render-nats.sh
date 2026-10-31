#!/usr/bin/env bash
# render-nats.sh: regenerates manifests/nats.yaml, the cluster copy's three-pod NATS JetStream hub,
# so that a run needs no helm and no chart download.
#
#   render-nats.sh [--out FILE] [--pin-from FILE]
#
# It downloads chart nats/nats 2.15.1 from https://nats-io.github.io/k8s/helm/charts/, refuses an
# archive whose sha256 is not the chart index's b209c627…58cc1 (the value INFRA-549 records),
# renders it with `helm template --skip-tests` (helm v3.20.2 through go run) and nats-values.yaml,
# with each image pinned to the digest a registry manifest read gives its tag, and refuses a render
# that holds a kind the load-test Role cannot write or an image not pinned by digest. The output
# keeps the placeholders __NAMESPACE__, __STORAGE_CLASS__ and __RUN_ID__, which cluster.sh fills.
#
# --out FILE writes FILE instead of manifests/nats.yaml. --pin-from FILE takes each image's digest
# from FILE (an earlier render) instead of the registry: cluster.test.sh re-renders that way and
# compares, so a tag that moved since the committed render does not fail the comparison.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
source "$here/cluster.sh"

CHART_REPO=https://nats-io.github.io/k8s/helm/charts
CHART_VERSION=2.15.1
CHART_SHA256=b209c62784b1b24c714d5ff6503b4c7dd948ec187d4222beee16b579f1258cc1
HELM=helm.sh/helm/v3/cmd/helm@v3.20.2
# Each image: the chart value that names it, and the tag render-nats.sh pins (the chart's own
# defaults, on mirror.gcr.io).
IMAGES=(
  "container.image mirror.gcr.io/library/nats:2.15.1-alpine"
  "reloader.image mirror.gcr.io/natsio/nats-server-config-reloader:0.24.0"
  "natsBox.container.image mirror.gcr.io/natsio/nats-box:0.20.0"
)
# The kinds the load-test Role may write that the chart renders; it may write no
# PodDisruptionBudget and no ServiceAccount.
ALLOWED_KINDS=" ConfigMap Deployment Secret Service StatefulSet "

out=$here/manifests/nats.yaml
pin_from=""
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || die "$1 needs a value"
  case $1 in
    --out) out=$2 ;;
    --pin-from) pin_from=$2 ;;
    *) usage ;;
  esac
  shift 2
done
[ -z "$pin_from" ] || [ -f "$pin_from" ] || die "--pin-from $pin_from is not a file"
for tool in curl go sha256sum jq; do
  command -v "$tool" >/dev/null || die "render-nats.sh needs $tool on PATH"
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/render-nats.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT

curl -fsSL --max-time 60 -o "$tmp/index.yaml" "$CHART_REPO/index.yaml" || die "could not read $CHART_REPO/index.yaml"
url=$(grep -oE "https://[^[:space:]\"']+/nats-$CHART_VERSION\.tgz" "$tmp/index.yaml" | head -n1)
[ -n "$url" ] || die "$CHART_REPO/index.yaml lists no nats-$CHART_VERSION.tgz"
curl -fsSL --max-time 120 -o "$tmp/nats.tgz" "$url" || die "could not download $url"
sha=$(sha256_of "$tmp/nats.tgz")
[ "$sha" = "$CHART_SHA256" ] || die "$url has sha256 $sha, not $CHART_SHA256; refusing it"

sets=()
pins=()
for entry in "${IMAGES[@]}"; do
  read -r key ref <<<"$entry"
  if [ -n "$pin_from" ]; then
    repo=${ref%:*}
    pinned=$(grep -oE "image: ${repo//./\\.}@sha256:[0-9a-f]{64}" "$pin_from" | head -n1 || true)
    [ -n "$pinned" ] || die "--pin-from $pin_from pins no $repo"
    pinned=${pinned#image: }
  else
    pinned=$(resolve_image "$ref") || die "$ref does not resolve to a digest"
  fi
  sets+=(--set "$key.fullImageName=$pinned")
  pins+=("$ref -> $pinned")
done

(cd -- "$tmp" && go run "$HELM" template nats nats.tgz --skip-tests -f "$here/nats-values.yaml" "${sets[@]}") >"$tmp/rendered.yaml" ||
  die "helm template failed"

while read -r kind; do
  [[ $ALLOWED_KINDS == *" $kind "* ]] || die "the render holds a $kind, which the load-test Role cannot write; turn it off in nats-values.yaml"
done < <(sed -n 's/^kind: //p' "$tmp/rendered.yaml" | sort -u)
if grep -E '^[[:space:]]*image:' "$tmp/rendered.yaml" | grep -vqE '@sha256:[0-9a-f]{64}$'; then
  die "the render holds an image not pinned by digest: $(grep -E '^[[:space:]]*image:' "$tmp/rendered.yaml" | grep -vE '@sha256:' | tr -s ' ' | tr '\n' ' ')"
fi

{
  printf '# nats.yaml: rendered by render-nats.sh (do not edit): chart nats/nats %s from\n' "$CHART_VERSION"
  printf '# %s/\n# (archive sha256 %s),\n' "$CHART_REPO" "$CHART_SHA256"
  printf '# helm template --skip-tests at helm v3.20.2 with nats-values.yaml, each image pinned to\n'
  printf '# its digest:\n'
  printf '#   %s\n' "${pins[@]}"
  printf '# cluster.sh fills the namespace, storage class and run id placeholders.\n'
  cat -- "$tmp/rendered.yaml"
} >"$out.tmp"
mv -- "$out.tmp" "$out"
printf 'render-nats.sh: wrote %s\n' "$out" >&2
