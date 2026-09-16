# Deploy

Compose definitions run the on-prem Envoy listener, Dispatch server, its
Postgres database, and the daily Postgres backup worker. All services use host
networking. The listener serves `127.0.0.1:9020` for local OpenCode session
registration and webhook ingress; Dispatch serves `127.0.0.1:8766` by default
for the SPA, GitHub OAuth, and the native Dispatch API.

## Host configuration lives in `compose/.env`

Docker Compose reads `compose/.env` next to the compose files on every
`docker compose` invocation, so the host's Dispatch settings belong there — not
in a shell session. A `docker compose up` run by anyone (or any agent) then
reproduces the same deployment; exported variables only override it.
`compose/.env` holds secrets and is git-ignored; `compose/dispatch.env.example`
is the template:

```bash
cp compose/dispatch.env.example compose/.env
$EDITOR compose/.env
```

For browser access over the tailnet, bind Dispatch to the host's Tailscale
IPv4 address and allow the plain-HTTP session cookie. The tailnet link is
already encrypted; nothing else on the host can reach that address:

```
DISPATCH_LISTEN_HOST=100.x.y.z        # tailscale ip -4
DISPATCH_SERVER_URL=http://sami-agents:8766 # the exact browser URL and OAuth callback origin
DISPATCH_INSECURE_COOKIE=1            # cookies over the http:// tailnet URL
```

The origin humans open in the browser is the OAuth callback origin: the server builds
`<dispatch.serverUrl>/auth/callback` from `dispatch.serverUrl` in the mounted `envoy.json`, and
the GitHub App must list exactly that URL. On Sami's devbox that is `http://sami-agents:8766`
(recorded as `DISPATCH_PUBLIC_ORIGIN` in `compose/.env`); a deploy whose `/auth/start` redirect
stops matching it breaks sign-in for everyone, so the auto-deployer checks the redirect after
every deploy and rolls back on a mismatch.

Never bind `0.0.0.0`: that exposes the OAuth endpoints and session cookies on
every interface.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `compose/dispatch.compose.yml` — Dispatch, Postgres, and backup worker.
- `scripts/up-listener.sh` — starts the listener with `docker compose`.
- `scripts/up-dispatch.sh` — starts Dispatch and its dependencies with
  `docker compose`.
- `scripts/sync-host.sh` — rsyncs `deploy/` to a remote host.
- `scripts/install-docker-debian.sh` — Docker install helper for fresh hosts.
- `scripts/read-secret.sh` — reads a secret from local SOPS-encrypted state.

## Image

The listener and Dispatch share the image built by `../docker/Dockerfile`. Its
build context is the repository root: a Bun stage builds the SPA and generates
the Go contracts, then a Go stage builds `envoy-listener` and `envoy-dispatch`.

`ENVOY_IMAGE_TAG` has no Compose default. `scripts/up-listener.sh` and
`scripts/up-dispatch.sh` set it to `local` for a checkout build. A host synced
without the Dockerfile must set a published image tag and pull it:

```bash
export ENVOY_IMAGE_TAG=<sha-or-release-tag>
docker compose -f compose/dispatch.compose.yml pull
docker compose -f compose/dispatch.compose.yml up -d
```

## Auto-deployer

`scripts/autodeploy.sh` polls `main`, selects the newest published Dispatch
image among its last 30 commits, creates a `pg_dump`, then pulls and starts
only the `dispatch` service. It retains the ten newest pre-deploy dumps. Its
defaults target the devbox; set `REPO`, `COMPOSE_PROJECT`, or `BACKUPS` to run
against another checkout, Compose project, or dump directory.

Run it continuously in its supervised host service, or use one pass for a
smoke check:

```bash
cd packages/envoy/deploy
scripts/autodeploy.sh --once
```
After health succeeds, the deployer reads `DISPATCH_SERVER_URL` from
`compose/.env` and checks that `/auth/start` emits exactly
`$DISPATCH_SERVER_URL/auth/callback`. It persists the new `ENVOY_IMAGE_TAG`
only after both checks; a failed deployment restores the previous tag and
restarts only `dispatch`.

## Listener configuration

| Var | Required | Notes |
| --- | --- | --- |
| `ENVOY_MACHINE_ID` | yes | Logical machine name used in published metadata. |
| `NATS_URLS` | yes | Comma-separated NATS URLs. |
| `ENVOY_LISTENER_PORT` | no | Defaults to `9020`. |
| `ENVOY_LISTEN_HOST` | no | Defaults to `127.0.0.1`. |
| `ENVOY_API_TOKEN` | conditional | Required when `ENVOY_LISTEN_HOST` is not loopback; matching bearer token for Listener API requests. |
| `ENVOY_API_ALLOW_UNAUTHENTICATED` | Fargate transition only | Set to `1` only temporarily to start a non-loopback listener without `ENVOY_API_TOKEN`. |
| `ENVOY_HOST_BRIDGE` | no | Address used to reach host services; defaults to `127.0.0.1`. |
| `ENVOY_WEBHOOKS` | no | Comma-separated enabled webhook providers. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | conditional | Required when GitHub webhooks are enabled. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | no | Defaults to `@legion`. |
| `ENVOY_SLACK_SIGNING_SECRET` | conditional | Required when Slack webhooks are enabled. |
| `ENVOY_GHOSTWISPR_SIGNING_SECRET` | optional | Empty skips Ghost Wispr signature verification. |

## Dispatch configuration

Generate the agent token once and record it, with the external database URL, in
`compose/.env`:

```bash
cd packages/envoy/deploy
cp compose/dispatch.env.example compose/.env
sed -i "s|^DISPATCH_AGENT_TOKEN=.*|DISPATCH_AGENT_TOKEN=$(openssl rand -hex 32)|" compose/.env
$EDITOR compose/.env          # DATABASE_URL, logins, listen host
scripts/up-dispatch.sh
curl "http://$(tailscale ip -4):8766/healthz"
```

The Dispatch service reads its public browser origin and NATS URLs from
`compose/.env`; it does not mount an agent's `envoy.json`. Set
`DISPATCH_NATS_DISABLED=1` when no NATS connection is available.

| Var | Required | Notes |
| --- | --- | --- |
| `DATABASE_URL` | yes | The external Dispatch Postgres URL (production: the Aurora `dispatch` database). The compose runs no Postgres of its own. |
| `DISPATCH_SERVER_URL` | yes | Public browser origin and GitHub OAuth callback origin. It must be the URL humans type into their browser; the GitHub App must list `<DISPATCH_SERVER_URL>/auth/callback`. |
| `NATS_URLS` | yes | Comma-separated NATS URLs for Dispatch. |
| `DISPATCH_AGENT_TOKEN` | yes | Shared devbox fallback bearer token; per-person tokens minted in Dispatch Settings are preferred for individual agents. |
| `DISPATCH_ALLOWED_LOGINS` | human identity | Cookie identity requires it at startup; header identity accepts only included logins. |
| `DISPATCH_LISTEN_HOST` | no | Defaults to `127.0.0.1`; for direct tailnet access, set it to `$(tailscale ip -4)`, never `0.0.0.0`. |
| `DISPATCH_PORT` | no | Defaults to `8766`; the healthcheck follows it. |
| `DISPATCH_IDENTITY` | no | `cookie` (default) or `header:<name>` for a trusted proxy or tests. |
| `DISPATCH_IDENTITY_HEADER_TRUSTED` | conditional | Set to `1` when header identity and GitHub OAuth credentials share a deployment. |
| `DISPATCH_REPO_PROJECTS` | no | Optional boot seed for repository-to-project settings (`owner/repo=KEY,...`). Existing dashboard settings are not overwritten. |
| `DISPATCH_DEFAULT_PROJECT` | no | Native Dispatch project for external repositories without a stored mapping. |
| `DISPATCH_NATS_DISABLED` | no | Set to `1` to run database and SSE paths without NATS. |
| `ENVOY_URL` | no | Envoy listener base URL for live agent titles; defaults to `http://127.0.0.1:9020` (set it when `ENVOY_LISTENER_PORT` changes). |
| `ENVOY_TOKEN` | conditional | Matching Listener API bearer token when `ENVOY_API_TOKEN` protects the listener. |
| `DISPATCH_URL` | no | Host-adapter override for the Dispatch base URL; use with `DISPATCH_TOKEN`. |
| `DISPATCH_TOKEN` | host adapters | Bearer token paired with `DISPATCH_URL`. |
| `DISPATCH_APP_CLIENT_ID` / `DISPATCH_APP_CLIENT_SECRET` | OAuth | GitHub OAuth credentials. The GitHub proxy needs a stored user token. |
| `DISPATCH_INSECURE_COOKIE` | HTTP | Set to `1` whenever browsers reach Dispatch over `http://` (the tailnet deployment); leave unset behind an HTTPS terminator. |

The Dispatch signing material is a named volume; the database is external.

## Backups and restore

The database's nightly logical backup is not this compose's job: agent-c's
`dispatch-backup` Fargate task (`meta/infra/pulumi/components/dispatch/backup.py`)
`pg_dump`s the Aurora `dispatch` database into `production-dispatch-pg-backups`
under the `dispatch/` prefix and pages `#eng-alerts` when a day has no dump; restores
go through agent-c's `scripts/dispatch_restore.py`. `scripts/autodeploy.sh` still
takes its own pre-deploy dump of `DATABASE_URL` before every image roll, kept locally
for rollback.

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```

This ships only `deploy/`; the remote runs a pinned image and does not build
the listener or Dispatch image locally.
