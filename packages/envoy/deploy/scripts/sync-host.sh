#!/bin/bash
set -euo pipefail

host="${1:?usage: sync-host.sh user@host}"
root="$(cd "$(dirname "$0")/../../../.." && pwd)"
rsync -az --delete --exclude '.git' --exclude '.jj' --exclude 'docker-data' "$root/packages/envoy/" "$host:~/legion/default/packages/envoy/"
