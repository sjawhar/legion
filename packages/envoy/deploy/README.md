# Deploy

## Layout

- `compose/listener.compose.yml` — host-network listener with opencode registry and host home mounts
- `compose/github.compose.yml` — host-network GitHub receiver
- `compose/slack.compose.yml` — host-network Slack receiver
- `compose/nats/peer.compose.yml` — JetStream peer node (all machines run as peers via Tailscale)

## Secrets

Never store plaintext secrets here. Launch ingress services with the wrapper scripts:

- `deploy/scripts/up-github.sh`
- `deploy/scripts/up-slack.sh`

These use `secrets ... -- docker compose ...` so the secrets exist only in the process environment at runtime.
If the local SOPS file has a MAC mismatch, the helper falls back to a scoped `sops --ignore-mac` read for only the requested key.

## GitHub mention trigger

The GitHub receiver can publish a second `.mention` topic when a comment body contains a matching trigger.

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

Example:

```bash
export ENVOY_MACHINE_ID=sami-agents-mx
export NATS_URLS=nats://127.0.0.1:4222,nats://sami:4222,nats://sami-claude:4222
export ENVOY_HOME=/home/ubuntu
export ENVOY_OPENCODE_BIN=/home/ubuntu/.mise/installs/github-sjawhar-opencode/1.3.2-sami.20260328-035401/opencode
deploy/scripts/up-listener.sh
```

## GitHub receiver envs

- `ENVOY_MACHINE_ID`
- `NATS_URLS`
- `ENVOY_GITHUB_WEBHOOK_SECRET`
- `ENVOY_GITHUB_MENTION_TRIGGER` (optional, default `@legion`)
