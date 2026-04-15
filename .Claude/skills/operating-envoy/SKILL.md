---
name: operating-envoy
description: "Operational guide for the Envoy notification system — covers NATS JetStream, Fargate deployment, on-prem listener management, and tsnet/Tailscale configuration. Use when troubleshooting NATS connectivity, managing JetStream consumers or streams, debugging message delivery, checking cluster health, deploying or redeploying Envoy listener containers to Fargate or on-prem machines, diagnosing tsnet/Tailscale issues, or understanding the listener-per-machine architecture. Triggers on: NATS errors, JetStream problems, consumer binding conflicts, 'consumer is already bound', 'no matching interests', message delivery failures, stale consumer, envoy healthz, NATS connection refused, Fargate deployment issues, ECS task failures, tsnet auth errors, Tailscale duplicate node key, EFS permissions, ENVOY_MACHINE_ID, envoy-listener crashes, rolling deploy problems, webhook ingress."
---

# Operating Envoy

Operational reference for the NATS JetStream messaging layer in Legion's Envoy notification system. This covers the real problems you'll hit — consumer conflicts, cluster health, delivery debugging, and deployment issues.

## Architecture

```
                    ┌─────────────────────────────────────────────────────────┐
                    │              NATS JetStream Cluster                     │
                    │         Stream: ENVOY_NOTIFICATIONS                    │
                    │     KV: envoy_sessions, envoy_interests                │
                    └──────┬────────────────┬────────────────┬───────────────┘
                           │                │                │
          ┌────────────────┴──┐   ┌─────────┴──────┐   ┌────┴──────────────┐
          │  sami-agents-mx   │   │     sami       │   │   sami-claude     │
          │  (EC2)            │   │  (on-prem)     │   │   (on-prem)       │
          │                   │   │                │   │                   │
          │  listener :9020   │   │  listener :9020│   │  listener :9020   │
          │  ├─ consume NATS  │   │  ├─ consume    │   │  ├─ consume       │
          │  ├─ match local   │   │  ├─ match local│   │  ├─ match local   │
          │  └─ deliver local │   │  └─ deliver    │   │  └─ deliver       │
          │                   │   │                │   │                   │
          │  sessions: ~30    │   │  sessions: ~5  │   │  sessions: ~5     │
          └───────────────────┘   └────────────────┘   └───────────────────┘

   ┌──────────────────────────┐
   │   Fargate Listener       │          GitHub ──webhook──► ALB ──► Fargate
   │   (WEBHOOK INGRESS ONLY) │          Slack  ──webhook──► ALB ──► Fargate
   │                          │
   │   ├─ receive webhooks    │    Fargate publishes to NATS. It does NOT
   │   ├─ publish to NATS     │    deliver to sessions. On-prem listeners
   │   └─ tsnet for tailnet   │    consume from NATS and deliver locally.
   │                          │
   │   sessions: 0            │    /v1/sessions = [] on Fargate is CORRECT.
   │   interests: 0           │    /v1/interests = [] on Fargate is CORRECT.
   └──────────────────────────┘
```

**Key insight**: Each on-prem machine has its own listener that consumes from NATS and
delivers to local sessions only. Fargate is special — it only publishes webhooks to NATS.
If Fargate shows zero sessions/interests, that's working correctly, not an error.

## NATS Resources
- **Stream**: `ENVOY_NOTIFICATIONS` — subjects: `notifications.>`, 1-hour retention, file storage
- **Consumer per machine**: `listener-{ENVOY_MACHINE_ID}` — durable, explicit ACK, max 20 redeliveries
- **KV buckets**:
  - `envoy_sessions` — live sessions with 5-min TTL (plugin heartbeats every 2 min)
  - `envoy_interests` — topic subscriptions, permanent (no TTL, reaper cleans orphans)
  - `envoy_roles` — role assignments (e.g., legion-controller)

## Common Operations

### Check cluster health

```bash
# From any machine with NATS access:
nats server ls                                    # list cluster members
nats server report jetstream                      # JetStream resource usage
nats stream info ENVOY_NOTIFICATIONS              # stream state, message count, consumer count
nats consumer ls ENVOY_NOTIFICATIONS              # list all consumers
nats consumer info ENVOY_NOTIFICATIONS listener-sami  # specific consumer state
```

Without the `nats` CLI, use the monitoring HTTP API (port 8222):
```bash
curl -s http://127.0.0.1:8222/healthz             # basic health
curl -s http://127.0.0.1:8222/routez              # cluster routes/peers
curl -s http://127.0.0.1:8222/jsz                 # JetStream overview
curl -s http://127.0.0.1:8222/jsz?consumers=true  # include consumer details
```

### Check listener health

```bash
curl -s http://127.0.0.1:9020/healthz | jq        # listener health + consumer lag
curl -s http://127.0.0.1:9020/metrics              # Prometheus metrics
curl -s http://127.0.0.1:9020/v1/sessions | jq     # registered sessions
```

The `/healthz` response includes `num_pending` (messages waiting for delivery) and `num_ack_pending` (delivered but not yet acknowledged). High `num_pending` means the listener is falling behind.

### Delete a stale consumer

When a consumer is "already bound" (e.g., after a crash or during rolling deploys):

```bash
nats consumer rm ENVOY_NOTIFICATIONS listener-fargate-prod
```

The listener recreates its consumer automatically on startup. The consumer name is `listener-{ENVOY_MACHINE_ID}`.

If you don't have the `nats` CLI:
```bash
# Install it:
curl -sf https://binaries.nats.dev/nats-io/natscli/nats@latest | sh

# Or use nats-box container:
docker run --rm natsio/nats-box nats -s nats://<host>:4222 consumer rm ENVOY_NOTIFICATIONS <consumer-name>
```

### View KV bucket contents

```bash
nats kv ls envoy_sessions           # list all live sessions
nats kv get envoy_sessions <key>    # get specific session
nats kv ls envoy_interests          # list all interest subscriptions
nats kv ls envoy_roles              # list role assignments
```

### Purge a stream (DESTRUCTIVE — drops all messages)

```bash
nats stream purge ENVOY_NOTIFICATIONS
```

Only do this if you're okay losing all undelivered messages. The stream recreates itself empty.

## Troubleshooting

### "consumer is already bound to a subscription"

**Cause**: Two processes are trying to use the same durable consumer simultaneously. Common during rolling deploys (old container still running) or when `ENVOY_MACHINE_ID` is duplicated across machines.

**Fix**:
1. Check if another process holds the consumer: `nats consumer info ENVOY_NOTIFICATIONS listener-<machine-id>`
2. If it's a stale binding from a dead process: `nats consumer rm ENVOY_NOTIFICATIONS listener-<machine-id>`
3. If two machines have the same `ENVOY_MACHINE_ID`: fix the config — each machine MUST have a unique ID

The listener has a retry loop (10 attempts, 3s × attempt backoff) that handles transient conflicts during rolling deploys. If it still fails after 30s, the consumer is genuinely held by another process.

**Death loop scenario**: If the container exhausts all 10 retries and exits, it closes the NATS connection first (releases the consumer binding). But images built BEFORE PR #570 used `log.Fatalf` which calls `os.Exit` and skips cleanup — the consumer stays bound and every subsequent container restart hits the same error. Fix: delete the consumer manually (`nats consumer rm`), then deploy an image with PR #570 or later.
### "no matching interests"

**Cause**: Messages arrive on NATS but no sessions have subscribed to matching topics. The `envoy_interests` bucket is empty for this machine.

**Check**:
```bash
curl -s http://127.0.0.1:9020/v1/sessions | jq length   # how many sessions?
nats kv ls envoy_interests                                # any interests at all?
```

**Common causes**:
- Listener just started — sessions haven't re-registered yet (takes up to 2 min for plugin heartbeat)
- Wrong `ENVOY_MACHINE_ID` — interests are per-machine; if the ID doesn't match, the listener won't see them
- Plugin's `ENVOY_URL` points to wrong host — default is `http://127.0.0.1:9020`

### "nats: no responders" or connection refused

**Cause**: NATS server is unreachable. Either it's not running, the URL is wrong, or there's a network partition.

**Check**:
```bash
nats server ping                     # quick connectivity test
nats server ls                       # cluster member list
docker ps | grep nats                # is the NATS container running?
curl -s http://127.0.0.1:8222/varz   # NATS server stats
```

**Common causes**:
- NATS container was removed (check `docker ps -a | grep nats`)
- Wrong `NATS_URLS` in listener config — should be `nats://127.0.0.1:4222` for local peer, plus remote peers comma-separated
- Firewall blocking port 4222 (client) or 6222 (cluster routes)

### tsnet "Authkey is set; but state is NoState"

**Cause**: Empty tsnet state directory (fresh EFS or wiped state). The tsnet library ignores the auth key when state is `NoState` instead of `NeedsLogin`.

**Fix**: Set `TSNET_FORCE_LOGIN=1` in the container environment. This forces tsnet to use the auth key regardless of state. It's a one-time bootstrap issue — once the node registers and state is written, the env var becomes a no-op.

### tsnet "Duplicate node key"

**Cause**: During rolling deploys, old and new containers mount the same EFS volume and present the same node key simultaneously.

**Fix**: The listener's graceful shutdown (SIGTERM handler) deregisters the node key before exiting. Ensure:
1. The container image has the graceful shutdown fix (PR #568+)
2. ECS `stopTimeout` is ≥ 30s (default) to allow clean shutdown
3. EFS permissions match the container user (uid must match EFS access point posix_user)

### Listener container exits with code 1

Check the container logs for the specific error. Common causes:
- `ENVOY_MACHINE_ID is required` — missing env var
- `NATS_URLS is required` — missing env var
- `consumer is already bound` — see above
- `ENVOY_TSNET_TAGS is required` — tsnet enabled but tags not set

### "no such host: envoy-nats.envoy.local" (Fargate)

**Cause**: ECS Service Connect / Cloud Map DNS isn't resolving the NATS service name. The NATS task isn't registered in the private DNS namespace, or DNS hasn't propagated yet.

**Check**:
```bash
aws servicediscovery list-services --region us-west-1    # is there a service for envoy-nats?
aws servicediscovery list-instances --service-id <id>     # is the NATS task registered?
```

**Common causes**:
- NATS task just started — DNS propagation takes 10-30s. The listener retries NATS connection (10 attempts, 1s backoff), so brief delays are handled.
- Cloud Map namespace or service registration is missing from the Pulumi/ECS config
- NATS task crashed and was replaced — new task gets a new IP, DNS updates lag

**Workaround**: Use the NATS task's private IP directly in `NATS_URLS` instead of the DNS name. Less elegant but eliminates service discovery dependency.

### Duplicate Tailscale nodes after deploy

**Cause**: A previous deploy left a stale tsnet node in Tailscale admin. The old container didn't deregister its node key before exiting (pre-PR #568 behavior, or SIGKILL before graceful shutdown completed).

**Fix**: Remove the stale node from Tailscale admin:
1. Go to Tailscale admin console → Machines
2. Find the duplicate node (the one that's offline or has an older `Last Seen`)
3. Click "..." → Remove

Or via API:
```bash
# List machines, find the stale one by hostname + last-seen
curl -s -H "Authorization: Bearer $TAILSCALE_API_KEY" \
  https://api.tailscale.com/api/v2/tailnet/-/devices | jq '.devices[] | select(.hostname=="envoy-listener")'
```

**Prevention**: The graceful shutdown fix (PR #568) deregisters the node on SIGTERM. Ensure:
- Container image includes the graceful shutdown code
- ECS `stopTimeout` ≥ 30s (default) to allow clean deregistration
- EFS permissions match container UID so tsnet can write state

## Envoy Listener Environment Variables

### Required (all deployments)
| Variable | Description | Example |
|----------|-------------|---------|
| `ENVOY_MACHINE_ID` | Unique per machine, used for consumer name | `sami-agents-mx` |
| `NATS_URLS` | Comma-separated NATS peer URLs | `nats://127.0.0.1:4222,nats://sami:4222` |
| `PORT` | HTTP listen port | `9020` |

### Required for tsnet (Fargate only)
| Variable | Description | Example |
|----------|-------------|---------|
| `ENVOY_TSNET_ENABLED` | Enable tsnet | `true` |
| `ENVOY_TSNET_HOSTNAME` | Tailscale hostname | `envoy-listener` |
| `ENVOY_TSNET_STATE_DIR` | Persistent state path (EFS mount) | `/var/lib/envoy-tsnet/listener` |
| `ENVOY_TSNET_OAUTH_CLIENT_ID` | Tailscale OAuth client ID | from Secrets Manager |
| `ENVOY_TSNET_OAUTH_CLIENT_SECRET` | Tailscale OAuth client secret | from Secrets Manager |
| `ENVOY_TSNET_TAGS` | ACL tags | `tag:envoy` |
| `TSNET_FORCE_LOGIN` | Force auth on empty state | `1` |

### Required for webhooks (Fargate only)
| Variable | Description | Example |
|----------|-------------|---------|
| `ENVOY_WEBHOOKS` | Enabled providers | `github,slack` |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | GitHub webhook secret | from Secrets Manager |
| `ENVOY_SLACK_SIGNING_SECRET` | Slack signing secret | from Secrets Manager |

### Optional
| Variable | Description | Default |
|----------|-------------|---------|
| `ENVOY_HOST_BRIDGE` | Local delivery address | `127.0.0.1` |
| `ENVOY_KV_REPLICAS` | JetStream KV replica count | `1` (set to NATS cluster size) |
| `ENVOY_GITHUB_MENTION_TRIGGER` | GitHub mention keyword | none |

## Deployment Checklist

Before deploying a new listener image:

- [ ] `ENVOY_MACHINE_ID` is unique across all machines
- [ ] `NATS_URLS` includes local peer (if running) and all remote peers
- [ ] `ENVOY_KV_REPLICAS` matches NATS cluster size
- [ ] For Fargate: `TSNET_FORCE_LOGIN=1` is set (handles fresh EFS)
- [ ] For Fargate: EFS access point `posix_user` UID matches container user UID
- [ ] For Fargate: GHCR package is public (or registry credentials configured)
- [ ] Container image has ENTRYPOINT set (`envoy-listener`)
- [ ] Graceful shutdown works: `docker stop` → logs "received signal, shutting down"

After deployment:
- [ ] `curl /healthz` returns `{"status":"healthy"}`
- [ ] `curl /v1/sessions` shows registered sessions (may take up to 2 min)
- [ ] `nats consumer info ENVOY_NOTIFICATIONS listener-<machine-id>` shows the consumer bound
- [ ] For Fargate: tsnet registered on tailnet (check Tailscale admin)
