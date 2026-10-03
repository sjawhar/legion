---
title: Running Dispatch
description: What the Dispatch server needs, how to run it, its health check, and its migrations.
sidebar:
  order: 7
---

Dispatch is one Go program. It serves the web app, the HTTP API, live documents, and Google
sign-in. Its source is `packages/envoy/cmd/dispatch`, and the web app it serves is built from
`packages/dispatch`. The container image `ghcr.io/sjawhar/legion/envoy` carries it as
`/usr/local/bin/envoy-dispatch`.

## What it needs

| Need | Setting | Notes |
| --- | --- | --- |
| Postgres | `DATABASE_URL` | Required. Dispatch keeps everything here and applies its own migrations at start. Leave `pool_max_conns` out of the URL; Dispatch refuses it. |
| A shared agent token | `DISPATCH_AGENT_TOKEN` | Required to start. A fallback credential for agents; agents normally use personal tokens made in Settings. |
| Google sign-in | `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET`, `DISPATCH_SIGNIN_GROUP` | The OpenID Connect sign-in pool people sign in to with Google Workspace, Dispatch's app client in it and that client's secret, and the pool group a person must be in. Required for the default cookie sign-in, all four together. |
| The browser address | `DISPATCH_SERVER_URL` | The exact address people type, such as `https://dispatch.internal.example`. The sign-in pool's app client lists this address followed by `/auth/callback` as a callback URL. |
| The GitHub App | `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64` | Without it the server starts, but the web app's GitHub reads answer `503` and architecture sources cannot be saved. |
| A stable session key | `DISPATCH_SIGNING_KEY` | Signs sign-in cookies. When unset, Dispatch creates one under `~/.local/share/dispatch`; keep that directory on a volume, or every restart signs everyone out. |
| An Envoy listener | `ENVOY_URL` | Where the Agents page finds live agent sessions. Defaults to `http://127.0.0.1:9020`. |
| NATS | `NATS_URLS`, `NATS_NKEY_SEED_FILE` | Where Dispatch publishes its events for agents. The Agents page's live view also needs it. Dispatch refuses a NATS server on another machine unless `ENVOY_ALLOW_REMOTE_NATS=1`. Set `DISPATCH_NATS_DISABLED=1` to run without it. |
| An address to listen on | `DISPATCH_LISTEN_HOST`, `DISPATCH_PORT` | Port `8766` by default. |

These are the settings most deployments need, not all of them. The full list, generated from the
server, is in [Configuration](/legion/dispatch/reference/configuration/).
When neither `NATS_URLS` nor `DISPATCH_NATS_DISABLED=1` is set, Dispatch reads the NATS address
from `~/.config/opencode/envoy.json`, the file agents use, so set one of them explicitly.

People sign in with Google Workspace through the sign-in pool by default, and Dispatch names each
person by their lowercase email. `DISPATCH_IDENTITY=header:<Header-Name>` names people by a request
header instead; it is for tests and local harnesses only, needs `DISPATCH_IDENTITY_HEADER_TRUSTED=1`,
and refuses every `DISPATCH_SIGNIN_*` setting. Serve Dispatch over HTTPS.
`DISPATCH_INSECURE_COOKIE=1` is only for trying it over plain HTTP on your own machine.

## Running it on your machine

From a checkout of this repository, with Docker, Bun, and Go installed:

1. Start a Postgres:

   ```sh
   docker run -d --name dispatch-pg -e POSTGRES_PASSWORD=dispatch -e POSTGRES_DB=dispatch \
     -p 127.0.0.1:55432:5432 postgres:16
   ```

2. Build the web app:

   ```sh
   bun install
   (cd packages/dispatch && bun run build:web)
   ```

3. Start the server, still from the repository root:

   ```sh
   DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
   DISPATCH_AGENT_TOKEN='<a long random string>' \
   DISPATCH_DEV_SIGNIN=1 \
   DISPATCH_SERVER_URL=http://127.0.0.1:8766 \
   DISPATCH_LISTEN_HOST=127.0.0.1 \
   DISPATCH_INSECURE_COOKIE=1 \
   DISPATCH_NATS_DISABLED=1 \
   DISPATCH_WEB_DIST="$PWD/packages/dispatch/web/dist" \
   go -C packages/envoy run ./cmd/dispatch
   ```

   `DISPATCH_WEB_DIST` is where the web app was built. `DISPATCH_NATS_DISABLED=1` keeps this run off
   any message bus. `DISPATCH_DEV_SIGNIN=1` lets you sign in without a sign-in pool.

4. Open `http://127.0.0.1:8766/auth/_dev/signin?login=<your email>`. It signs in anyone by the email
   it names, with no sign-in pool. Dispatch accepts it only on your own machine, with every address
   in the setup on that machine, and that server needs a database of its own.

To sign in through a real sign-in pool instead, replace `DISPATCH_DEV_SIGNIN=1` in step 3 with the
four `DISPATCH_SIGNIN_*` settings, and list `http://127.0.0.1:8766/auth/callback` as a callback URL
of the pool's app client.

## First steps after it starts

Sign in, then open **Settings**:

- **Projects**: create your first project, with a key such as `CORE` and a name.
- **Repositories → Projects**: choose which project issues from each GitHub repository land in.
  `DISPATCH_DEFAULT_PROJECT` catches repositories with no mapping; that project must already exist.
- **Architecture sources**: point a project at a repository branch holding its architecture
  model. Saving checks that the GitHub App can read it.
- **Agent tokens**: make a token for each agent you run.
  [Dispatch for agents](/legion/dispatch/for-agents/#connecting-an-agent) covers the rest.

## Health check

`GET /healthz` needs no credential. It answers `200` when the server is healthy:

```json
{"ok":true,"db":true,"nats":null,"commit":null,"schema_version":63}
```

- `db` is whether Postgres answered within two seconds. When it does not, the answer is `503` with
  `db: false`.
- `nats` is whether NATS is connected, or `null` when NATS is turned off.
- `commit` is the source commit the image was built from, or `null` in a build that was not
  stamped.
- `schema_version` is the newest database migration applied.

## Migrations

Dispatch applies its database migrations every time it starts, before it serves anything. Servers
starting together take turns, so a migration runs once. Each migration waits at most five seconds
for a lock. When something holds the table longer, the start fails and names the migration and
what it waited on; end the holder, or let it finish, and start again.

Before you roll a new version out, you can see what its migrations will touch:

```sh
DATABASE_URL='<your database URL>' envoy-dispatch census
```

It reads the database without changing it, and reports each pending migration's tables, their
sizes, and any long-running transactions in the way.

## The HTTP API

`GET /api/v1` lists every API route with its method, who may call it, and a description. The
[HTTP API reference](/legion/dispatch/reference/api/) is the same list.
