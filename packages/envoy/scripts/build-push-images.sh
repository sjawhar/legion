#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${1:?Usage: build-push-images.sh <git-sha-tag>}"
REGISTRY="${ENVOY_REGISTRY:-ghcr.io/sjawhar/legion}"

echo "Building and pushing multi-arch image with tag: $TAG"
echo "Registry: $REGISTRY"

echo ""
echo "=== Building envoy:${TAG} ==="
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t "${REGISTRY}/envoy:${TAG}" \
  -f docker/Dockerfile \
  --push \
  .
echo "=== Pushed ${REGISTRY}/envoy:${TAG} ==="

echo ""
echo "Image built and pushed with tag: $TAG"
echo "Next steps:"
echo "  cd infra"
echo "  pulumi config set envoy:imageTag $TAG"
echo "  pulumi preview"
echo "  pulumi up"
