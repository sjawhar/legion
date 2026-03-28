#!/usr/bin/env bash
set -euo pipefail

host="${1:?usage: sync-host.sh user@host}"
root="$(cd "$(dirname "$0")/../../.." && pwd)"

rsync -az "$root/packages/envoy-plugin/dist/" "$host:~/legion/default/packages/envoy-plugin/dist/"
rsync -az "$HOME/.dotfiles/opencode/plugins/envoy.ts" "$host:~/.dotfiles/opencode/plugins/envoy.ts"
