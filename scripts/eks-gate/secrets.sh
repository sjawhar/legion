#!/usr/bin/env bash
# Gate 2 persistent providers Secret creation. Values stay on stdin, never argv or output.
set -euo pipefail
# shellcheck source=scripts/eks-gate/lib.sh
source "${BASH_SOURCE[0]%/*}/lib.sh"

context=""
secret_name=""
failures=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --context) context="${2:-}"; shift 2 ;;
    --name) secret_name="${2:-}"; shift 2 ;;
    *) gate_failed secrets/arguments "unknown argument $1"; exit 2 ;;
  esac
done
require_context secrets
if [[ ! "$secret_name" =~ ^legion-[a-z0-9]+-providers$ ]]; then
  gate_failed secrets/name '--name must match legion-<project-token>-providers'
  exit 2
fi
keys=(ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY DISPATCH_TOKEN ENVOY_TOKEN)
for key in "${keys[@]}"; do
  if [ -z "${!key:-}" ]; then
    gate_failed secrets/input "$key is unset or empty"
  fi
done
[ "$failures" -eq 0 ] || exit 1
if jq -n --arg name "$secret_name" \
  --arg anthropic "$ANTHROPIC_API_KEY" --arg gemini "$GEMINI_API_KEY" --arg openai "$OPENAI_API_KEY" \
  --arg dispatch "$DISPATCH_TOKEN" --arg envoy "$ENVOY_TOKEN" \
  '{apiVersion:"v1",kind:"Secret",metadata:{name:$name,namespace:"legion"},type:"Opaque",stringData:{ANTHROPIC_API_KEY:$anthropic,GEMINI_API_KEY:$gemini,OPENAI_API_KEY:$openai,DISPATCH_TOKEN:$dispatch,ENVOY_TOKEN:$envoy}}' |
  kc apply -f - >/dev/null; then
  gate_ok secrets/providers-secret "$secret_name keys ${keys[*]}"
else
  gate_failed secrets/providers-secret "could not apply $secret_name"
  exit 1
fi
