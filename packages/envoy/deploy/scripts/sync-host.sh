#!/bin/bash
set -euo pipefail

host="${1:?usage: sync-host.sh user@host}"
root="$(cd "$(dirname "$0")/../../../.." && pwd)"
rsync -az --delete --exclude '.git' --exclude '.jj' --exclude 'docker-data' "$root/packages/envoy/" "$host:~/legion/default/packages/envoy/"
# The dispatch compose service bind-mounts packages/dispatch/web/dist, which
# lives outside packages/envoy. Build the SPA here and deliver only the dist
# so the target host does not need bun or the dispatch package sources.
(cd "$root/packages/dispatch" && bun run build:web)
rsync -az --delete "$root/packages/dispatch/web/dist/" "$host:~/legion/default/packages/dispatch/web/dist/"
