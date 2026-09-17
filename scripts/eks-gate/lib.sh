#!/usr/bin/env bash
# Shared output and Kubernetes helpers for the production EKS rollout gates.
set -euo pipefail

gate_ok() { # gate_ok NAME [DETAIL]
  if [ "$#" = 1 ]; then
    printf 'GATE %s OK\n' "$1"
  else
    printf 'GATE %s OK: %s\n' "$1" "$2"
  fi
}
gate_failed() { # gate_failed NAME DETAIL
  printf 'GATE %s FAILED: %s\n' "$1" "$2" >&2
  failures=$((failures + 1))
}
require_context() {
  [ -n "$context" ] || {
    gate_failed "$1/context" '--context is required'
    exit 2
  }
}
kc() { kubectl --context "$context" -n legion "$@"; }
