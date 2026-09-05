#!/bin/bash
set -euo pipefail

# The shared Dockerfile builds from the repo root: its bun stage bakes the
# dispatch SPA and regenerates the Go contracts from the whole workspace.
cd "$(dirname "$0")/../../.."

TAG="${1:?Usage: build-push-images.sh <git-sha-tag>}"
REGISTRY="${ENVOY_REGISTRY:?ENVOY_REGISTRY is required (e.g. ghcr.io/your-org/your-repo)}"

echo "Building and pushing multi-arch image with tag: $TAG"
echo "Registry: $REGISTRY"

echo ""
echo "=== Building envoy:${TAG} ==="
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t "${REGISTRY}/envoy:${TAG}" \
  -f packages/envoy/docker/Dockerfile \
  --push \
  .
echo "=== Pushed ${REGISTRY}/envoy:${TAG} ==="

echo ""
echo "Image built and pushed with tag: $TAG"
echo "To deploy, set imageTag=$TAG in your stack config and run pulumi up."
