#!/usr/bin/env bash
# Prints LEGION_E2E_POD_SERVICE_ACCOUNT, the ServiceAccount the deployment's pods run as, once it
# is a name a stage proof can use: the proofs write it into the operator route's pod.yml (and
# pod-key-file.yml) in place of the ${POD_SERVICE_ACCOUNT} placeholder, which the daemon's loader
# refuses unfilled, so it must be a Kubernetes ServiceAccount name — a DNS-1123 subdomain. The
# repository carries no default; the operator sets it to the account their own pods run as.
#
#   pod_service_account=$(bash scripts/e2e/lib/pod-service-account.sh)
#
# Stdout is the account; a refusal names the variable on stderr, never its value, and exits 1.
set -euo pipefail

fail() {
  echo "pod-service-account: LEGION_E2E_POD_SERVICE_ACCOUNT $*" >&2
  exit 1
}

account=${LEGION_E2E_POD_SERVICE_ACCOUNT:-}
[ -n "$account" ] ||
  fail "is unset: set it to the ServiceAccount the deployment's pods run as"
[[ $account =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ && ${#account} -le 253 ]] ||
  fail "is not a Kubernetes ServiceAccount name: a DNS-1123 subdomain of at most 253 characters — lowercase letters, digits, - and ., beginning and ending with a letter or a digit"
printf '%s\n' "$account"
