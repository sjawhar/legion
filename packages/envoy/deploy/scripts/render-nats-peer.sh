#!/bin/bash
set -euo pipefail

dir="${1:?usage: render-nats-peer.sh <dir>}"
mkdir -p "$dir"
cat > "$dir/nats.conf" <<EOF
server_name=${NATS_SERVER_NAME:?NATS_SERVER_NAME required}
listen=0.0.0.0:4222

jetstream {
  store_dir=/data
}

cluster {
  name: envoy
  listen: 0.0.0.0:6222
  routes: [
$(printf '    %s\n' ${NATS_ROUTES:?NATS_ROUTES required})
  ]
}
EOF
