#!/usr/bin/env bash
# Prints LEGION_E2E_MODEL_GATEWAY_URL, the model gateway's Anthropic endpoint, once it is one a
# stage proof can use: the proofs write it into an Oh My Pi models.yml as a plain YAML scalar, so it
# must be an https:// URL of letters, digits and :/._~- alone, not ending in a colon (YAML reads
# that as a mapping key). The repository carries no default; the operator sets it to the anthropic
# provider's baseUrl in their own gateway route (~/.omp/agent/models.yml).
#
#   gateway=$(bash scripts/e2e/lib/model-gateway-url.sh)
#
# Stdout is the URL; a refusal names the variable on stderr, never its value, and exits 1.
set -euo pipefail

fail() {
  echo "model-gateway-url: LEGION_E2E_MODEL_GATEWAY_URL $*" >&2
  exit 1
}

gateway=${LEGION_E2E_MODEL_GATEWAY_URL:-}
[ -n "$gateway" ] ||
  fail "is unset: set it to the model gateway's Anthropic endpoint, the anthropic provider's baseUrl in your own ~/.omp/agent/models.yml"
case "$gateway" in
*[!A-Za-z0-9:/._~-]*) fail "holds a character other than letters, digits and :/._~-" ;;
*:) fail "ends in a colon, which YAML reads as a mapping key" ;;
https://?*) ;;
*) fail "is not an https:// URL" ;;
esac
printf '%s\n' "$gateway"
