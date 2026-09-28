#!/usr/bin/env bash
# Prints LEGION_E2E_MODEL_GATEWAY_AUDIENCE, the audience the model gateway accepts on a worker's
# projected ServiceAccount token, once it is one a stage proof can use: the proofs write it into the
# operator route's pod.yml inside a double-quoted YAML string, so it must be letters, digits and
# ._:/- alone. The repository carries no default; the operator sets it to the audience their own
# gateway verifies.
#
#   gateway_audience=$(bash scripts/e2e/lib/model-gateway-audience.sh)
#
# Stdout is the audience; a refusal names the variable on stderr, never its value, and exits 1.
set -euo pipefail

fail() {
  echo "model-gateway-audience: LEGION_E2E_MODEL_GATEWAY_AUDIENCE $*" >&2
  exit 1
}

audience=${LEGION_E2E_MODEL_GATEWAY_AUDIENCE:-}
[ -n "$audience" ] ||
  fail "is unset: set it to the audience your model gateway accepts on a worker's projected ServiceAccount token"
case "$audience" in
*[!A-Za-z0-9._:/-]*) fail "holds a character other than letters, digits and ._:/-" ;;
esac
printf '%s\n' "$audience"
