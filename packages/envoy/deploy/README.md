# Deploy

Compose definitions run the on-prem Envoy listener, Dispatch server, its
Postgres database, and the nightly Postgres backup worker. All services use host
networking. The listener serves `127.0.0.1:9020` for local OpenCode session
registration and webhook ingress; Dispatch serves `127.0.0.1:8766` by default
for the SPA, GitHub OAuth, and the native Dispatch API.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `compose/dispatch.compose.yml` — Dispatch, Postgres, and backup worker.
- `scripts/up-listener.sh` — starts the listener with `docker compose`.
- `scripts/up-dispatch.sh` — starts Dispatch and its dependencies with
  `docker compose`.
- `scripts/pg-backup.sh` — uploads an immediate backup, then repeats daily.
- `scripts/sync-host.sh` — rsyncs `deploy/` to a remote host.
- `scripts/install-docker-debian.sh` — Docker install helper for fresh hosts.
- `scripts/read-secret.sh` — reads a secret from local SOPS-encrypted state.

## Image

The listener and Dispatch share the image built by `../docker/Dockerfile`. Its
build context is the repository root: a Bun stage builds the SPA and generates
the Go contracts, then a Go stage builds `envoy-listener` and `envoy-dispatch`.
The backup worker builds `docker/pg-backup.Dockerfile`, which supplies both
`pg_dump` and the AWS CLI.

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
| `ENVOY_HOST_BRIDGE` | no | Address used to reach host services; defaults to `127.0.0.1`. |
| `ENVOY_WEBHOOKS` | no | Comma-separated enabled webhook providers. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | conditional | Required when GitHub webhooks are enabled. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | no | Defaults to `@legion`. |
| `ENVOY_SLACK_SIGNING_SECRET` | conditional | Required when Slack webhooks are enabled. |
| `ENVOY_GHOSTWISPR_SIGNING_SECRET` | optional | Empty skips Ghost Wispr signature verification. |

## Dispatch configuration

Set the secrets before first startup. Both values are opaque random strings:

```bash
export DISPATCH_PG_PASSWORD="$(openssl rand -hex 32)"
export DISPATCH_AGENT_TOKEN="$(openssl rand -hex 32)"
export DISPATCH_ALLOWED_LOGINS="<github-login>"
export DISPATCH_BACKUP_BUCKET="<private-s3-bucket>"
deploy/scripts/up-dispatch.sh
curl http://127.0.0.1:8766/healthz
```

| Var | Required | Notes |
| --- | --- | --- |
| `DISPATCH_PG_PASSWORD` | yes | Password for the Compose-managed Postgres database. |
| `DISPATCH_PG_PORT` | no | Defaults to `55432`; set it with `DISPATCH_PORT` for a second stack. |
| `DISPATCH_AGENT_TOKEN` | yes | Agent bearer token; generate with `openssl rand -hex 32`. |
| `DISPATCH_ALLOWED_LOGINS` | yes | Comma-separated GitHub login allowlist. |
| `DISPATCH_BACKUP_BUCKET` | yes | Private S3 bucket receiving daily PostgreSQL dumps. |
| `AWS_PROFILE` | no | AWS profile for backups; defaults to `default`. |
| `DISPATCH_LISTEN_HOST` | no | Defaults to `127.0.0.1`. |
| `DISPATCH_PORT` | no | Defaults to `8766`; the healthcheck follows it. |
| `DISPATCH_IDENTITY` | no | Defaults to `cookie`; `header:<name>` is for a trusted proxy or tests. |
| `DISPATCH_REPO_PROJECTS` | no | Maps external repositories to native Dispatch projects. |
| `DISPATCH_NATS_DISABLED` | no | Set to `1` to run database and SSE paths without NATS. |
| `DISPATCH_APP_CLIENT_ID` / `DISPATCH_APP_CLIENT_SECRET` | no | GitHub OAuth credentials. Without them the server starts, but the GitHub proxy has no user token and returns `503`. |
| `DISPATCH_INSECURE_COOKIE` | conditional | Set only for HTTP OAuth on a local or tailnet deployment. |

The service derives `DATABASE_URL` from the configured password and Postgres
port. The database data and Dispatch signing material are named volumes.

## Backups and restore

`pg-backup` waits for Postgres, runs `pg_dump -Fc` against Dispatch, uploads to
`s3://$DISPATCH_BACKUP_BUCKET/dispatch-<utc-timestamp>.dump`, and retries in
one hour if an upload fails. It mounts `${HOME}/.aws` read-only and uses
`AWS_PROFILE`.

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
