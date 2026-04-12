#!/usr/bin/env bash
# Verify invariants of release-envoy-and-plugin.yaml
# Agent-executable: run after any workflow modification to catch regressions.
set -euo pipefail

WORKFLOW=".github/workflows/release-envoy-and-plugin.yaml"

if [ ! -f "$WORKFLOW" ]; then
  echo "FAIL: $WORKFLOW not found"
  exit 1
fi

PASS=0
FAIL=0

check() {
  local desc="$1" result="$2"
  if [ "$result" = "true" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

echo "Checking workflow invariants: $WORKFLOW"
echo

TEXT=$(cat "$WORKFLOW")

# --- Docker job invariants ---
echo "Docker job:"
# Docker tags must include SHA and latest
check "docker tags include github.sha" \
  "$(echo "$TEXT" | grep -q 'ghcr.io/sjawhar/legion/envoy:\${{ github.sha }}' && echo true || echo false)"
check "docker tags include latest" \
  "$(echo "$TEXT" | grep -q 'ghcr.io/sjawhar/legion/envoy:latest' && echo true || echo false)"
# Docker must NOT be gated on should_release
# Extract the docker job block (from "  docker:" to the next top-level job or EOF)
DOCKER_BLOCK=$(echo "$TEXT" | sed -n '/^  docker:/,/^  [a-z]/p' | head -n -1)
check "docker job not gated on should_release" \
  "$(echo "$DOCKER_BLOCK" | grep -q "should_release" && echo false || echo true)"

echo
echo "Envoy job:"
ENVOY_BLOCK=$(echo "$TEXT" | sed -n '/^  envoy:/,/^  [a-z]/p' | head -n -1)
check "envoy job not gated on should_release" \
  "$(echo "$ENVOY_BLOCK" | grep -q "should_release" && echo false || echo true)"

echo
echo "Plugin job:"
PLUGIN_BLOCK=$(echo "$TEXT" | sed -n '/^  plugin:/,/^  [a-z]/p' | head -n -1)
check "plugin job gated on should_release" \
  "$(echo "$PLUGIN_BLOCK" | grep -q "should_release" && echo true || echo false)"

echo
echo "Release job:"
RELEASE_BLOCK=$(echo "$TEXT" | sed -n '/^  release:/,/^  [a-z]/p')
check "release job gated on should_release" \
  "$(echo "$RELEASE_BLOCK" | grep -q "should_release" && echo true || echo false)"

echo
echo "Trigger paths:"
check "trigger includes packages/envoy/**" \
  "$(echo "$TEXT" | grep -q 'packages/envoy/\*\*' && echo true || echo false)"
check "trigger includes packages/contracts/**" \
  "$(echo "$TEXT" | grep -q 'packages/contracts/\*\*' && echo true || echo false)"
check "trigger includes packages/envoy-plugin/**" \
  "$(echo "$TEXT" | grep -q 'packages/envoy-plugin/\*\*' && echo true || echo false)"

echo
echo "Dependencies:"
check "docker depends on envoy" \
  "$(echo "$DOCKER_BLOCK" | grep -q 'needs:.*envoy' && echo true || echo false)"
check "docker depends on version" \
  "$(echo "$DOCKER_BLOCK" | grep -q 'needs:.*version' && echo true || echo false)"

echo
echo "---"
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "PASS: all workflow invariants verified"
