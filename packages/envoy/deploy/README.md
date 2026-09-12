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
The backup worker uses the digest-pinned `eeshugerman/postgres-backup-s3`
image, so it is available to a host that receives only `deploy/`.

`ENVOY_IMAGE_TAG` has no Compose default. `scripts/up-listener.sh` and
`scripts/up-dispatch.sh` set it to `local` for a checkout build. A host synced
without the Dockerfile must set a published image tag and pull it:

```bash
export ENVOY_IMAGE_TAG=<sha-or-release-tag>
docker compose -f compose/dispatch.compose.yml pull
docker compose -f compose/dispatch.compose.yml up -d
```

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

Generate the secrets once and record them in `compose/.env`; both are opaque
random strings:

```bash
cd packages/envoy/deploy
cp compose/dispatch.env.example compose/.env
sed -i "s|^DISPATCH_PG_PASSWORD=.*|DISPATCH_PG_PASSWORD=$(openssl rand -hex 32)|" compose/.env
sed -i "s|^DISPATCH_AGENT_TOKEN=.*|DISPATCH_AGENT_TOKEN=$(openssl rand -hex 32)|" compose/.env
$EDITOR compose/.env          # logins, bucket, region, listen host
scripts/up-dispatch.sh
curl "http://$(tailscale ip -4):8766/healthz"
```

The mounted `~/.config/opencode/envoy.json` supplies `natsUrls` and
`dispatch.serverUrl`. Set `DISPATCH_NATS_DISABLED=1` when no NATS connection is
available. Host adapters use the same file or the `DISPATCH_URL` and
`DISPATCH_TOKEN` environment variables.

| Var | Required | Notes |
| --- | --- | --- |
| `DISPATCH_PG_PASSWORD` | yes | Password for the Compose-managed Postgres database. |
| `DISPATCH_PG_PORT` | no | Host-network Postgres port, shared by the Dispatch server and backup worker; defaults to `55432`. |
| `DISPATCH_AGENT_TOKEN` | yes | Shared devbox fallback bearer token; per-person tokens minted in Dispatch Settings are preferred for individual agents. |
| `DISPATCH_ALLOWED_LOGINS` | human identity | Cookie identity requires it at startup; header identity accepts only included logins. |
| `DISPATCH_BACKUP_BUCKET` | yes | Private S3 bucket receiving daily PostgreSQL dumps. |
| `S3_REGION` | yes | AWS Region that contains the backup bucket. |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | conditional | Set both for static S3 credentials; leave both unset to use the instance role. |
| `DISPATCH_LISTEN_HOST` | no | Defaults to `127.0.0.1`; for direct tailnet access, set it to `$(tailscale ip -4)`, never `0.0.0.0`. |
| `DISPATCH_PORT` | no | Defaults to `8766`; the healthcheck follows it. |
| `DISPATCH_IDENTITY` | no | `cookie` (default) or `header:<name>` for a trusted proxy or tests. |
| `DISPATCH_IDENTITY_HEADER_TRUSTED` | conditional | Set to `1` when header identity and GitHub OAuth credentials share a deployment. |
| `DISPATCH_REPO_PROJECTS` | no | Optional boot seed for repository-to-project settings (`owner/repo=KEY,...`). Existing dashboard settings are not overwritten. |
| `DISPATCH_DEFAULT_PROJECT` | no | Native Dispatch project for external repositories without a stored mapping. |
| `DISPATCH_NATS_DISABLED` | no | Set to `1` to run database and SSE paths without NATS; otherwise `natsUrls` in `envoy.json` is required. |
| `ENVOY_URL` | no | Envoy listener base URL for live agent titles; defaults to `http://127.0.0.1:9020` (set it when `ENVOY_LISTENER_PORT` changes). |
| `ENVOY_TOKEN` | conditional | Matching Listener API bearer token when `ENVOY_API_TOKEN` protects the listener. |
| `DISPATCH_URL` | no | Host-adapter override for the Dispatch base URL; use with `DISPATCH_TOKEN`. |
| `DISPATCH_TOKEN` | host adapters | Bearer token paired with `DISPATCH_URL`. |
| `DISPATCH_APP_CLIENT_ID` / `DISPATCH_APP_CLIENT_SECRET` | OAuth | GitHub OAuth credentials. The GitHub proxy needs a stored user token. |
| `DISPATCH_INSECURE_COOKIE` | HTTP | Set to `1` whenever browsers reach Dispatch over `http://` (the tailnet deployment); leave unset behind an HTTPS terminator. |

The service derives `DATABASE_URL` from the configured password and Postgres
port. The database data and Dispatch signing material are named volumes.

## Backups and restore

The digest-pinned `eeshugerman/postgres-backup-s3` worker runs daily, retains
30 days of dumps under the `dispatch` prefix, and connects directly to the
Compose-managed Postgres database. It uses both `S3_ACCESS_KEY_ID` and
`S3_SECRET_ACCESS_KEY` when supplied; when both are unset, AWS resolves the
instance role.

Download the desired dump, then restore it into the target database:

```bash
pg_restore -d "$DATABASE_URL" --clean --if-exists dispatch-<ts>.dump
```

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```

This ships only `deploy/`; the remote runs a pinned image and does not build
the listener or Dispatch image locally.
