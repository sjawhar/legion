#!/usr/bin/env bash
# envoy-watchdog.sh — Detects and recovers from container removal.
#
# Installed by Pulumi to /usr/local/bin/envoy-watchdog.sh on each on-prem machine.
# Called by a systemd timer every 60s.
#
# Unlike `restart: unless-stopped` (which only handles exits), this watchdog
# handles containers being completely removed from Docker state.
#
# Usage: envoy-watchdog.sh <config-file>
# Config file is JSON with container parameters, written by Pulumi.
set -euo pipefail

CONFIG="${1:-/etc/envoy/watchdog.json}"

if [ ! -f "$CONFIG" ]; then
    echo "ERROR: config file not found: $CONFIG"
    exit 1
fi

# Read config — Pulumi writes this during provisioning.
CONTAINER_NAME=$(jq -r '.container_name' "$CONFIG")
IMAGE=$(jq -r '.image' "$CONFIG")
ENV_FILE=$(jq -r '.env_file' "$CONFIG")
NETWORK_MODE=$(jq -r '.network_mode' "$CONFIG")
HEALTHCHECK_CMD=$(jq -r '.healthcheck_cmd' "$CONFIG")

check_container() {
    # Returns 0 if container exists and is running, 1 otherwise.
    local state
    state=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "missing")
    [ "$state" = "running" ]
}

recover_container() {
    local state
    state=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "missing")

    case "$state" in
        running)
            return 0
            ;;
        exited|created|paused)
            echo "Container $CONTAINER_NAME is $state — starting"
            docker start "$CONTAINER_NAME"
            ;;
        missing)
            echo "Container $CONTAINER_NAME is missing — recreating"

            # Build docker run args from config
            local -a args=(
                --name "$CONTAINER_NAME"
                --restart unless-stopped
                --network "$NETWORK_MODE"
                --env-file "$ENV_FILE"
            )

            # Add volumes from config (if any)
            local volumes
            volumes=$(jq -r '.volumes[]? // empty' "$CONFIG" 2>/dev/null)
            while IFS= read -r vol; do
                [ -n "$vol" ] && args+=(--volume "$vol")
            done <<< "$volumes"

            # Add healthcheck
            if [ -n "$HEALTHCHECK_CMD" ] && [ "$HEALTHCHECK_CMD" != "null" ]; then
                args+=(
                    --health-cmd "$HEALTHCHECK_CMD"
                    --health-interval 10s
                    --health-timeout 3s
                    --health-retries 3
                    --health-start-period 30s
                )
            fi

            docker run -d "${args[@]}" "$IMAGE"
            ;;
        *)
            echo "Container $CONTAINER_NAME is in unexpected state: $state — removing and recreating"
            docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
            recover_container  # recurse once (will hit 'missing' branch)
            ;;
    esac
}

if ! check_container; then
    recover_container
    echo "Recovery complete"
else
    # Silent when healthy — only log on recovery
    :
fi
