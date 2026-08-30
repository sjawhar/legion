# Deploy

Compose definitions for the on-prem Envoy containers: the `envoy-listener`
NATS subscriber and the `envoy-dispatch` HTTP server. Both run on the host
network and connect outbound to a single NATS server (typically reached via
Tailscale). The listener serves `127.0.0.1:9020` for local OpenCode session
registration and webhook ingress when configured; dispatch serves
`127.0.0.1:8766` for the Dispatch SPA, GitHub OAuth, and the MCP endpoint
that creates Dispatch threads as GitHub sub-issues.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `compose/dispatch.compose.yml` — host-network dispatch container.
- `scripts/up-listener.sh` — `docker compose up -d --build` for the listener.
- `scripts/up-dispatch.sh` — builds the SPA on the host, then
  `docker compose up -d --build` for dispatch.
- `scripts/sync-host.sh` — rsync this package to a remote host over SSH.
- `scripts/install-docker-debian.sh` — Docker install helper for fresh hosts.
- `scripts/read-secret.sh` — read a secret from local SOPS-encrypted state.

## Listener envs

| Var | Required | Notes |
|---|---|---|
| `ENVOY_MACHINE_ID` | yes | Logical machine name (used in published metadata) |
| `NATS_URLS` | yes | Comma-separated NATS URLs (one is fine) |
| `ENVOY_LISTENER_PORT` | no | Defaults to 9020 |
| `ENVOY_LISTEN_HOST` | no | Bind host for the listener API; defaults to `127.0.0.1`. Set an explicit reachable address only when external access is required. |
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

## Bring up the dispatch server

Configuration comes from the host's `~/.config/opencode/envoy.json`
(`natsUrls` plus the `dispatch` block), mounted read-only into the container:

```bash
deploy/scripts/up-dispatch.sh
curl http://127.0.0.1:8766/healthz
```

The SPA is built on the host (`bun run build:web` in `packages/dispatch`)
because it lives outside this package's Docker build context; the up script
does this on a full checkout, and accepts a prebuilt
`packages/dispatch/web/dist` on sync-host targets.

The service binds `127.0.0.1` by default even though it runs on the host
network. Dispatch envs:

| Var | Required | Notes |
|---|---|---|
| `DISPATCH_LISTEN_HOST` | no | Bind host; the compose service defaults it to `127.0.0.1`. Set an explicit reachable address (e.g. a Tailscale IP) only when remote access is required. |
| `DISPATCH_PORT` | no | Defaults to 8766; the compose healthcheck tracks it |
| `DISPATCH_APP_CLIENT_ID` / `DISPATCH_APP_CLIENT_SECRET` | no | GitHub App OAuth credentials. Without them the server still starts: the dashboard responds 503 and `/mcp` works with any GitHub bearer the caller supplies. Alternatively drop an `app.json` into the `dispatch-data` volume at `/home/envoy/.local/share/dispatch/app.json`. |
| `DISPATCH_INSECURE_COOKIE` | conditional | Session cookies are `Secure` by default, so a browser will not return them over plain HTTP. Set to any value when using the OAuth dashboard on `http://` (local/tailnet). |

The Legion daemon reaches this server through its required
`LEGION_DISPATCH_URL` (`http://127.0.0.1:8766`) and `LEGION_DISPATCH_BEARER`
(a GitHub token; the server forwards it to GitHub per request).

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```
