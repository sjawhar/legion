# Deploy

Compose definition for the on-prem `envoy-listener` container. The listener is
a stateless NATS subscriber that connects outbound to a single NATS server
(typically reached via Tailscale on the host network) and serves
`127.0.0.1:9020` for local OpenCode session registration and webhook ingress
when configured.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `scripts/up-listener.sh` — `docker compose up -d --build` for the listener.
- `scripts/sync-host.sh` — rsync this package to a remote host over SSH.
- `scripts/install-docker-debian.sh` — Docker install helper for fresh hosts.
- `scripts/read-secret.sh` — read a secret from local SOPS-encrypted state.

## Listener envs

| Var | Required | Notes |
|---|---|---|
| `ENVOY_MACHINE_ID` | yes | Logical machine name (used in published metadata) |
| `NATS_URLS` | yes | Comma-separated NATS URLs (one is fine) |
| `ENVOY_LISTENER_PORT` | no | Defaults to 9020 |
| `ENVOY_HOST_BRIDGE` | no | Address used to reach host services from sessions; defaults to `127.0.0.1` |
| `ENVOY_WEBHOOKS` | no | Comma-separated providers to enable on this listener: `github`, `slack`, `ghostwispr`. Only set when this host is the ingress point for that source. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | conditional | Required when `github` is in `ENVOY_WEBHOOKS` |
| `ENVOY_GITHUB_MENTION_TRIGGER` | no | Defaults to `@legion`. Comments containing this trigger publish an extra `.mention` topic. |
| `ENVOY_SLACK_SIGNING_SECRET` | conditional | Required when `slack` is in `ENVOY_WEBHOOKS` |
| `ENVOY_GHOSTWISPR_SIGNING_SECRET` | optional | When `ghostwispr` is in `ENVOY_WEBHOOKS`; can be empty to skip signature verification |

## GitHub mention routing

GitHub does not have a native `app_mention` event like Slack. The listener
re-publishes any comment whose body contains the mention trigger to a
`.mention` topic in addition to the original `.comment` topic. Configure via
`ENVOY_GITHUB_MENTION_TRIGGER` (default `@legion`).

## Example: bring up a listener

```bash
export ENVOY_MACHINE_ID=$(hostname)
export NATS_URLS=nats://nats.example.local:4222
deploy/scripts/up-listener.sh
```

For webhook ingress (only on the host that receives webhooks directly):

```bash
export ENVOY_WEBHOOKS=github,slack
export ENVOY_GITHUB_WEBHOOK_SECRET=...
export ENVOY_SLACK_SIGNING_SECRET=...
deploy/scripts/up-listener.sh
```

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```
