#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${1:?Usage: build-push-images.sh <git-sha-tag>}"
REGISTRY="${ENVOY_REGISTRY:-ghcr.io/sjawhar/legion}"

echo "Building and pushing multi-arch images with tag: $TAG"
echo "Registry: $REGISTRY"

for svc in listener github slack; do
  echo ""
  echo "=== Building envoy-${svc}:${TAG} ==="
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t "${REGISTRY}/envoy-${svc}:${TAG}" \
    -f "docker/${svc}.Dockerfile" \
    --push \
    .
  echo "=== Pushed ${REGISTRY}/envoy-${svc}:${TAG} ==="
done

echo ""
echo "All images built and pushed with tag: $TAG"
echo "Next steps:"
echo "  cd infra"
echo "  pulumi config set envoy:imageTag $TAG"
echo "  pulumi preview"
echo "  pulumi up"
