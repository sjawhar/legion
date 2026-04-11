# Deploy

## Layout

- `compose/listener.compose.yml` — host-network listener (includes webhook handlers when `ENVOY_WEBHOOKS` is set)
- `compose/nats/peer.compose.yml` — JetStream peer node (all machines run as peers via Tailscale)

## Secrets

Webhook secrets are passed via environment variables to the listener container.
Required when the corresponding provider is enabled via `ENVOY_WEBHOOKS`:

## Webhook configuration

The listener handles webhooks when `ENVOY_WEBHOOKS` is set (comma-separated list of enabled providers: `github`, `slack`, `ghostwispr`).

### GitHub mention trigger

The listener can publish a second `.mention` topic when a comment body contains a matching trigger.

- `ENVOY_GITHUB_MENTION_TRIGGER` — optional, defaults to `@legion`

GitHub does not provide a native `app_mention` webhook event like Slack. Mention routing is body-based and additive: matching comments still publish to `.comment`, and also publish to `.mention`.

## Host sync

```bash
cd ~/legion/default
./scripts/sync-envoy-host.sh sami@sami
./scripts/sync-envoy-host.sh claude@sami-claude
./scripts/sync-envoy-host.sh ghost-wispr@ghost-wispr
```

## Pi Docker install

```bash
ssh ghost-wispr@ghost-wispr 'bash -s' < ~/legion/default/packages/envoy/deploy/scripts/install-docker-debian.sh
```

## NATS peer envs

For each peer host, export:

- `NATS_SERVER_NAME`
- `NATS_ROUTES`

Peer nodes now store JetStream data in a Docker named volume (`nats_data`) instead of a bind-mounted repo path. This keeps cluster state independent of repo checkouts and survives source tree moves/removal.

If the cluster state is corrupt and you intentionally want a clean reset, run:

```bash
cd ~/legion/default/packages/envoy/deploy/compose/nats
docker compose -f peer.compose.yml down -v
```

Then bring the peer back with `../scripts/up-nats-peer.sh` after regenerating `nats.conf`.

Example:

```bash
export NATS_SERVER_NAME=sami-agents-mx
export NATS_ROUTES="nats://sami:6222 nats://sami-claude:6222"
deploy/scripts/up-nats-peer.sh
```

## Listener envs

- `ENVOY_MACHINE_ID`
- `NATS_URLS`
- `ENVOY_HOME`
- `ENVOY_OPENCODE_BIN`
- `ENVOY_WEBHOOKS` — comma-separated enabled webhook providers (e.g., `github,slack,ghostwispr`)
- `ENVOY_GITHUB_WEBHOOK_SECRET` — required when `github` is in `ENVOY_WEBHOOKS`
- `ENVOY_GITHUB_MENTION_TRIGGER` — optional, default `@legion`
- `ENVOY_SLACK_SIGNING_SECRET` — required when `slack` is in `ENVOY_WEBHOOKS`
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` — optional even when `ghostwispr` is in `ENVOY_WEBHOOKS`

Example:

```bash
export ENVOY_MACHINE_ID=sami-agents-mx
export NATS_URLS=nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222
export ENVOY_HOME=/home/ubuntu
export ENVOY_WEBHOOKS=github,slack
export ENVOY_GITHUB_WEBHOOK_SECRET=your-secret
export ENVOY_SLACK_SIGNING_SECRET=your-secret
deploy/scripts/up-listener.sh
```
