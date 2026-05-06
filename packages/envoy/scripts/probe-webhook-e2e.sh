#!/usr/bin/env bash
# probe-webhook-e2e.sh — On-demand probe for the GitHub webhook → Envoy session
# delivery path. See packages/envoy/scripts/README.md for usage.
#
# Exit codes:
#   0 — synthetic webhook arrived at a live session via Envoy delivery.
#   1 — webhook accepted by ALB but did not reach the local listener within timeout.
#   2 — webhook rejected by ALB (signature, format, or 5xx).
#   3 — local listener unreachable, subscribe call failed, or required tooling at startup. Cleanup unsubscribe failures are silently tolerated by the cleanup trap and do NOT cause a non-zero exit (use `curl -s http://127.0.0.1:9020/v1/interests/` after a run to verify cleanup).
#   4 — required tooling missing (curl, openssl, python3, jq, read-secret.sh).
set -euo pipefail

LISTENER_URL="${LISTENER_URL:-http://127.0.0.1:9020}"
WEBHOOK_URL="${WEBHOOK_URL:-https://webhooks.trajectorylabs.com/webhook/github}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-30}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
READ_SECRET="${SCRIPT_DIR}/../deploy/scripts/read-secret.sh"

# --- preflight: required tooling ---
for cmd in curl openssl python3 jq; do
  command -v "$cmd" >/dev/null || { echo "ERR: missing required command: $cmd" >&2; exit 4; }
done
[ -x "$READ_SECRET" ] || { echo "ERR: not executable: $READ_SECRET" >&2; exit 4; }

# --- generate a unique trigger so concurrent probes can't collide ---
TRIGGER="probe-$(date +%s)-$$-$RANDOM"
PROBE_SESSION="ses_probe_${TRIGGER}"
PROBE_TOPIC="notifications.github.legion-probe.canary.issue.1.comment"
DELIVERY_ID="probe-${TRIGGER}"

# --- state filled in by setup steps; cleanup trap consumes these ---
RECEIVER_PID=""
RECEIVER_LOG=""
RECEIVER_PORT_FILE=""
RECEIVER_PORT=""
HTTP_BODY=""

cleanup() {
  local rc=$?
  set +e
  if [ -n "${PROBE_SESSION:-}" ]; then
    curl -fsS -X POST -H 'Content-Type: application/json' \
      "${LISTENER_URL}/v1/interests/unsubscribe" \
      -d "$(jq -nc --arg s "$PROBE_SESSION" \
            '{session_id:$s, topics:[]}')" \
      >/dev/null 2>&1 || true
  fi
  if [ -n "$RECEIVER_PID" ] && kill -0 "$RECEIVER_PID" 2>/dev/null; then
    kill "$RECEIVER_PID" 2>/dev/null || true
    wait "$RECEIVER_PID" 2>/dev/null || true
  fi
  if [ -n "$RECEIVER_LOG" ] && [ -f "$RECEIVER_LOG" ]; then
    rm -f "$RECEIVER_LOG"
  fi
  if [ -n "$RECEIVER_PORT_FILE" ] && [ -f "$RECEIVER_PORT_FILE" ]; then
    rm -f "$RECEIVER_PORT_FILE"
  fi
  if [ -n "$HTTP_BODY" ] && [ -f "$HTTP_BODY" ]; then
    rm -f "$HTTP_BODY"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

echo "probe id: $TRIGGER"
echo "topic:    $PROBE_TOPIC"

# --- spawn local HTTP receiver mimicking OpenCode prompt_async ---
RECEIVER_LOG="$(mktemp /tmp/probe-receiver-XXXXXX.log)"
RECEIVER_PORT_FILE="$(mktemp /tmp/probe-port-XXXXXX)"

python3 - "$RECEIVER_LOG" "$RECEIVER_PORT_FILE" <<'PY' &
import sys, json, http.server, socketserver, threading

log_path = sys.argv[1]
port_path = sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(n) if n else b""
        with open(log_path, "ab") as f:
            f.write(self.path.encode() + b"\n")
            f.write(body + b"\n---\n")
            f.flush()
        self.send_response(204)  # match real OpenCode prompt_async semantics
        self.end_headers()
    def log_message(self, *a, **kw):
        pass  # quiet

with socketserver.TCPServer(("127.0.0.1", 0), Handler) as srv:
    with open(port_path, "w") as f:
        f.write(str(srv.server_address[1]))
    srv.serve_forever()
PY
RECEIVER_PID=$!

# wait up to 3s for the receiver to bind
for _ in $(seq 1 30); do
  if [ -s "$RECEIVER_PORT_FILE" ]; then break; fi
  sleep 0.1
done
RECEIVER_PORT="$(cat "$RECEIVER_PORT_FILE" 2>/dev/null || true)"
rm -f "$RECEIVER_PORT_FILE"
if [ -z "$RECEIVER_PORT" ]; then
  echo "ERR: receiver did not bind within 3s" >&2
  exit 3
fi
echo "receiver: 127.0.0.1:$RECEIVER_PORT (log: $RECEIVER_LOG)"

# --- register synthetic session with the listener ---
SUBSCRIBE_BODY=$(jq -nc \
  --arg s "$PROBE_SESSION" \
  --arg t "$PROBE_TOPIC" \
  --arg d "/tmp/probe-${TRIGGER}" \
  --arg title "webhook-e2e-probe ${TRIGGER}" \
  --argjson p "$RECEIVER_PORT" \
  '{session_id:$s, dir:$d, topics:[$t], port:$p, title:$title}')

if ! curl -fsS -m 5 -X POST -H 'Content-Type: application/json' \
       "${LISTENER_URL}/v1/interests/subscribe" -d "$SUBSCRIBE_BODY" >/dev/null; then
  echo "ERR: failed to subscribe probe session at ${LISTENER_URL}" >&2
  exit 3
fi
echo "subscribed: $PROBE_SESSION → $PROBE_TOPIC"

# --- read webhook secret from SOPS ---
WEBHOOK_SECRET="$("$READ_SECRET" ENVOY_GITHUB_WEBHOOK_SECRET 2>/dev/null || true)"
if [ -z "$WEBHOOK_SECRET" ]; then
  echo "ERR: could not read ENVOY_GITHUB_WEBHOOK_SECRET via $READ_SECRET" >&2
  exit 3
fi

# --- build the payload ---
PAYLOAD=$(jq -nc \
  --arg trigger "$TRIGGER" \
  '{
     action: "created",
     issue: { number: 1, title: "webhook probe", state: "open" },
     comment: { id: 1, body: ("probe trigger: " + $trigger) },
     repository: { name: "canary", owner: { login: "legion-probe" } },
     sender: { login: "envoy-probe", type: "User" }
   }')

SIG="sha256=$(printf '%s' "$PAYLOAD" \
  | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" \
  | awk '{print $NF}')"

# --- send to the public ALB ---
HTTP_BODY="$(mktemp /tmp/probe-resp-XXXXXX)"
HTTP_CODE=$(curl -sS -m 10 -o "$HTTP_BODY" -w '%{http_code}' \
  -X POST "$WEBHOOK_URL" \
  -H "Content-Type: application/json" \
  -H "X-GitHub-Event: issue_comment" \
  -H "X-GitHub-Delivery: $DELIVERY_ID" \
  -H "X-Hub-Signature-256: $SIG" \
  --data-raw "$PAYLOAD" || true)
echo "ALB POST: http=$HTTP_CODE body=$(tr -d '\n' < "$HTTP_BODY")"
rm -f "$HTTP_BODY"
if [ "$HTTP_CODE" != "200" ]; then
  echo "ERR: ALB rejected webhook (expected 200)" >&2
  exit 2
fi

# --- subsequent tasks fill in the rest ---
echo "ERR: probe not yet implemented past skeleton" >&2
exit 3
