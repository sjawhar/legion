#!/bin/sh
# secrets-via-broker.sh is installed as `secrets`, first on the PATH of Stage 4a's previous-layout
# fixture only (stage4a-sandbox-runtime.sh). That fixture is an older commit's harness, which reads
# the implement App's key with `secrets NAME -- command...` from the local store. The live proofs'
# App keys are in the secrets broker, so this turns that one call shape into
# `agent-secrets NAME --reason=… -- command...`, the call the current harness makes. Any other
# shape is refused, so no other `secrets` use is silently sent to the broker.
set -eu
if [ "$#" -lt 3 ] || [ "$2" != -- ]; then
  echo "secrets-via-broker: only 'secrets NAME -- command...' is supported, not: secrets $*" >&2
  exit 2
fi
name=$1
case "$name" in
  [A-Za-z_]*) ;;
  *) echo "secrets-via-broker: '$name' is not a secret name" >&2; exit 2 ;;
esac
shift 2
exec agent-secrets "$name" --reason=legion-live-proof-github-app-key -- "$@"
