# Envoy diagnostic scripts

## probe-webhook-e2e.sh

On-demand end-to-end probe for the GitHub webhook → Envoy session delivery
path. Run it any time you suspect webhooks aren't reaching subscribed
sessions (e.g., agents subscribed to `notifications.github.<owner>.<repo>.pr.<n>.checks`
not seeing settled CI events).

### Usage

```bash
WEBHOOK_URL=https://your-ingress.example.com/webhook/github \
  packages/envoy/scripts/probe-webhook-e2e.sh
```

Required env:

- `WEBHOOK_URL` — the public ingress that fronts the webhook receiver Envoy
  publishes from. **No default** — supply your deployment's URL.

Run from a host with:

- A local Envoy listener reachable at `http://127.0.0.1:9020` (override with
  `LISTENER_URL=...`).
- SOPS-decryptable access to `ENVOY_GITHUB_WEBHOOK_SECRET` (the same secret
  the receiver verifies against). The probe reads it via
  `packages/envoy/deploy/scripts/read-secret.sh`.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | The synthetic webhook was delivered to a live session via the full pipeline. |
| 1 | Webhook ingress accepted the payload but the on-prem listener never delivered it (the bridge is broken). |
| 2 | Webhook ingress rejected the request (signature mismatch, malformed, or 5xx). |
| 3 | Local listener unreachable or subscribe call failed at startup. (Cleanup unsubscribe failures are tolerated by the cleanup trap and do not flip the exit code.) |
| 4 | Required tool missing on the host (`curl`, `openssl`, `python3`, `jq`, or `read-secret.sh`). |

### Diagnosing failures

- **Exit 1 (most common):** the receiver published to its NATS but the
  on-prem listener never saw it. Check `docker logs envoy-listener --since 1m`
  on the on-prem host for any `received` line containing the probe's trigger
  string. If absent, the event never made it from receiver-side NATS to the
  listener — the bridge between them is broken.
- **Exit 2:** the receiver itself rejects the request. Verify
  `ENVOY_GITHUB_WEBHOOK_SECRET` matches what the receiver has in its secret
  store. Confirm the URL with `curl -i $WEBHOOK_URL/../healthz` if there's a
  health endpoint.
- **Exit 3:** the local listener is down or on a different port. Check
  `curl http://127.0.0.1:9020/healthz` and the running container
  (`docker ps | grep envoy-listener`).
- **Exit 4:** install the missing tool.

### Why the topic is `notifications.github.legion-probe.canary.issue.1.comment`

The `legion-probe/canary` repo does not exist on GitHub — that's intentional.
The probe sends a fake `issue_comment` event with that owner/repo, the
receiver normalizes the envelope topic from the payload (it does not call
the GitHub API to validate the resource), and the topic flows through
exactly the same code path real events do. Using a synthetic owner/repo
guarantees no real subscriber will ever match the probe traffic.

### Tuning

- `TIMEOUT_SECONDS=60 ...` — wait longer (default 30s).
- `LISTENER_URL=http://other-host:9020 ...` — point at a different on-prem listener.

## e2e-local.sh

Runs the local listener against a throwaway `nats:2.10-alpine` container. It
posts signed public GitHub fixtures and a three-paragraph direct message,
checks the one-warning response for a never-seen GitHub repository, rejects an
unheld role publish before claiming it for the fake session, captures the raw
notification envelopes, and proves both the Go prompt text and the shared
TypeScript renderer retain one summary followed by the full direct message.

Docker downloads `nats:2.10-alpine` automatically on the first run when it is
not already cached.

```bash
packages/envoy/scripts/e2e-local.sh
```

The run writes its reusable evidence to `packages/envoy/out/e2e/`:

- `envelopes.jsonl` — raw NATS envelope frames
- `session-prompts.jsonl` — raw fake-session `prompt_async` requests
- `rendered-ts.txt` — `renderInbound` output for every envelope
- `rendered-go.txt` — the corresponding Go `Deliverer.Text` output

Use `E2E_NATS_PORT`, `E2E_PORT`, or `E2E_SESSION_PORT` to avoid local port
collisions. The script owns only the literal `envoy-e2e-nats` container and
removes it on exit. Unlike `probe-webhook-e2e.sh`, it has no deployment URL or
secret prerequisite: it validates local branch behavior only.
