---
title: Running Dispatch
description: What the Dispatch server needs, how to run it, its health check, migrations, and where its settings are documented.
sidebar:
  order: 7
---

Dispatch is one Go program. It serves the web app, the HTTP API, live documents, and GitHub
sign-in. Its source is `packages/envoy/cmd/dispatch`, and the web app it serves is built from
`packages/dispatch`. The container image `ghcr.io/sjawhar/legion/envoy` carries it as
`/usr/local/bin/envoy-dispatch`.

## What it needs

| Need | Setting | Notes |
| --- | --- | --- |
| Postgres | `DATABASE_URL` | Required. Dispatch keeps everything here and applies its own migrations at start. Leave `pool_max_conns` out of the URL; Dispatch refuses it. |
| A shared agent token | `DISPATCH_AGENT_TOKEN` | Required to start. A fallback credential for agents; agents normally use personal tokens made in Settings. |
| GitHub sign-in | `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_CLIENT_SECRET`, `DISPATCH_APP_PEM_B64` | A GitHub App. Without it the server starts, but sign-in and the GitHub features answer `503`. |
| The browser address | `DISPATCH_SERVER_URL` | The exact address people type, such as `https://dispatch.internal.example`. The GitHub App's callback URL is this address followed by `/auth/callback`. |
| Who may sign in | `DISPATCH_ALLOWED_LOGINS` | A comma-separated list of GitHub logins. Required for the default cookie sign-in. |
| A stable session key | `DISPATCH_SIGNING_KEY` | Signs sign-in cookies. When unset, Dispatch creates one under `~/.local/share/dispatch`; keep that directory on a volume, or every restart signs everyone out. |
| An Envoy listener | `ENVOY_URL` | Where the Agents page finds live agent sessions. Defaults to `http://127.0.0.1:9020`. |
| NATS | `NATS_URLS`, `NATS_NKEY_SEED_FILE` | Where Dispatch publishes its events for agents. The Agents page's live view also needs it. Dispatch refuses a NATS server on another machine unless `ENVOY_ALLOW_REMOTE_NATS=1`. Set `DISPATCH_NATS_DISABLED=1` to run without it. |
| An address to listen on | `DISPATCH_LISTEN_HOST`, `DISPATCH_PORT` | Port `8766` by default. |

The [configuration reference](/legion/dispatch/reference/configuration/) lists every setting.
When neither `NATS_URLS` nor `DISPATCH_NATS_DISABLED=1` is set, Dispatch reads the NATS address
from `~/.config/opencode/envoy.json`, the file agents use, so set one of them explicitly.

People sign in with the GitHub App by default. To sign people in through a reverse proxy that
already knows who they are, set `DISPATCH_IDENTITY=header:<Header-Name>`. Serve Dispatch over
HTTPS. `DISPATCH_INSECURE_COOKIE=1` is only for trying it over plain HTTP on your own machine.

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
   DISPATCH_ALLOWED_LOGINS='<your GitHub login>' \
   DISPATCH_SERVER_URL=http://127.0.0.1:8766 \
   DISPATCH_LISTEN_HOST=127.0.0.1 \
   DISPATCH_INSECURE_COOKIE=1 \
   DISPATCH_NATS_DISABLED=1 \
   DISPATCH_WEB_DIST="$PWD/packages/dispatch/web/dist" \
   go -C packages/envoy run ./cmd/dispatch
   ```

   `DISPATCH_WEB_DIST` is where the web app was built. `DISPATCH_NATS_DISABLED=1` keeps this run off
   any message bus.

4. Open `http://127.0.0.1:8766`. To sign in with GitHub, add your GitHub App's three settings to
   step 3.

To look around without a GitHub App, add `DISPATCH_DEV_SIGNIN=1` to step 3, then open
`http://127.0.0.1:8766/auth/_dev/signin?login=<your GitHub login>`. It signs in any login on the
allowlist without GitHub. Dispatch accepts it only on your own machine, with every address in the
setup on that machine, and that server needs a database of its own.

## First steps after it starts

Sign in, then open **Settings**:

- **Projects**: create your first project, with a key such as `CORE` and a name.
- **Repository mappings**: choose which project issues from each GitHub repository land in.
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

`GET /api/v1` lists every API route with its method, who may call it, and a description.
