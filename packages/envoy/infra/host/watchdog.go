package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// The watchdog's texts reproduce the Docker-host program's byte for byte, indentation included,
// since a remote.Command's create and delete are compared as strings: any change to them is an
// update that runs create again on the host.

// watchdogConfig is /etc/envoy/watchdog.json, the container's parameters the watchdog recreates
// a removed container from. Its fields marshal in this order.
type watchdogConfig struct {
	ContainerName  string `json:"container_name"`
	Image          string `json:"image"`
	EnvFile        string `json:"env_file"`
	NetworkMode    string `json:"network_mode"`
	HealthcheckCmd string `json:"healthcheck_cmd"`
}

// watchdogCreate is the command that writes the watchdog's files on the host and starts its
// timer: the env file holds envs, one per line, and the closing echo names the component.
func watchdogCreate(name string, envs []string, image string, port int) (string, error) {
	// JSON.stringify(config, null, 2): two-space indent, and no HTML escaping, which
	// encoding/json applies by default.
	var config bytes.Buffer
	enc := json.NewEncoder(&config)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(watchdogConfig{
		ContainerName:  defaultContainerName,
		Image:          image,
		EnvFile:        "/etc/envoy/listener.env",
		NetworkMode:    "host",
		HealthcheckCmd: fmt.Sprintf("curl -sf http://127.0.0.1:%d/healthz", port),
	}); err != nil {
		return "", fmt.Errorf("render watchdog.json: %w", err)
	}
	return `
        set -e
        mkdir -p /etc/envoy

        # Write env file
        cat > /etc/envoy/listener.env << 'ENVEOF'
` + strings.Join(envs, "\n") + `
ENVEOF

        # Write watchdog config
        cat > /etc/envoy/watchdog.json << 'CFGEOF'
` + strings.TrimSuffix(config.String(), "\n") + `
CFGEOF

        # Write watchdog script
        cat > /usr/local/bin/envoy-watchdog << 'SCRIPTEOF'
` + watchdogScript + `
SCRIPTEOF
        chmod +x /usr/local/bin/envoy-watchdog

        # Write systemd unit
        cat > /etc/systemd/system/envoy-watchdog.service << 'UNITEOF'
` + watchdogUnit + `
UNITEOF

        # Write systemd timer
        cat > /etc/systemd/system/envoy-watchdog.timer << 'TIMEREOF'
` + watchdogTimer + `
TIMEREOF

        # Enable and start the timer
        systemctl daemon-reload
        systemctl enable envoy-watchdog.timer
        systemctl start envoy-watchdog.timer
        echo "Watchdog deployed on ` + name + `"
      `, nil
}

// watchdogDelete stops the watchdog and removes every file watchdogCreate writes.
const watchdogDelete = `
        systemctl stop envoy-watchdog.timer 2>/dev/null || true
        systemctl disable envoy-watchdog.timer 2>/dev/null || true
        rm -f /usr/local/bin/envoy-watchdog /etc/systemd/system/envoy-watchdog.{service,timer} /etc/envoy/watchdog.json /etc/envoy/listener.env
        systemctl daemon-reload
      `

// watchdogScript starts a stopped envoy-listener container and recreates a removed one from
// watchdog.json and the env file.
const watchdogScript = `#!/usr/bin/env bash
# envoy-watchdog — detects and recovers from container removal.
# Deployed by Pulumi. Run by systemd timer every 60s.
set -euo pipefail

CONFIG="/etc/envoy/watchdog.json"
CONTAINER_NAME="envoy-listener"

check_running() {
    local state
    state=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "missing")
    [ "$state" = "running" ]
}

if check_running; then
    exit 0
fi

STATE=$(docker inspect --format '{{.State.Status}}' "$CONTAINER_NAME" 2>/dev/null || echo "missing")
echo "envoy-watchdog: container is $STATE — recovering"

case "$STATE" in
    exited|created|paused)
        docker start "$CONTAINER_NAME"
        ;;
    missing)
        IMAGE=$(jq -r '.image' "$CONFIG")
        ENV_FILE=$(jq -r '.env_file' "$CONFIG")
        NETWORK=$(jq -r '.network_mode' "$CONFIG")
        HC_CMD=$(jq -r '.healthcheck_cmd' "$CONFIG")

        ARGS=(--name "$CONTAINER_NAME" --restart unless-stopped --network "$NETWORK" --env-file "$ENV_FILE")

        if [ -n "$HC_CMD" ] && [ "$HC_CMD" != "null" ]; then
            ARGS+=(--health-cmd "$HC_CMD" --health-interval 10s --health-timeout 3s --health-retries 3 --health-start-period 30s)
        fi

        docker run -d "${ARGS[@]}" "$IMAGE"
        ;;
    *)
        docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
        exec "$0"  # re-exec to hit 'missing' branch
        ;;
esac
echo "envoy-watchdog: recovery complete"
`

// watchdogUnit runs the script once per activation.
const watchdogUnit = `[Unit]
Description=Envoy Listener Watchdog
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/envoy-watchdog
`

// watchdogTimer activates the unit 30 s after boot and every 60 s after.
const watchdogTimer = `[Unit]
Description=Envoy Listener Watchdog Timer

[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
AccuracySec=5s

[Install]
WantedBy=timers.target
`
