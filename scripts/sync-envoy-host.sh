#!/usr/bin/env bash
set -euo pipefail

host="${1:?usage: sync-envoy-host.sh user@host}"
root="$(cd "$(dirname "$0")/.." && pwd)"

ssh "$host" 'mkdir -p ~/legion/default/packages/envoy'

"$root/packages/envoy/deploy/scripts/sync-host.sh" "$host"
"$root/packages/envoy-plugin/scripts/sync-host.sh" "$host"
