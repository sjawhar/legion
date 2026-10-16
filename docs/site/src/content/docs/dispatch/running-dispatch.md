---
title: Running Dispatch
description: What the Dispatch server needs, how to run it, its health check, and its migrations.
sidebar:
  order: 8
---

Dispatch is one Go program. It serves the web app, the HTTP API, live documents, and Google
sign-in. Its source is `packages/envoy/cmd/dispatch`, and the web app it serves is built from
`packages/dispatch`. The container image `ghcr.io/sjawhar/legion/envoy` carries it as
`/usr/local/bin/envoy-dispatch`.

## What it needs

| Need | Setting | Notes |
| --- | --- | --- |
| Postgres | `DATABASE_URL` | Required. Dispatch keeps everything here and applies its own migrations at start. Leave `pool_max_conns` out of the URL; Dispatch refuses it. |
| A shared agent token | `DISPATCH_AGENT_TOKEN` | Required to start. A fallback credential for agents; agents normally use personal tokens made in Settings. To change it without locking agents out, set the new value followed by the old one, separated by a space, until every agent has moved, then the new one alone. |
| Google sign-in | `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET`, and `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | The OpenID Connect issuer people sign in through, Dispatch's client there and that client's secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with Google itself as the issuer. Required for the default cookie sign-in. [Signing people in](#signing-people-in) covers both. |
| The browser address | `DISPATCH_SERVER_URL` | The exact address people type, such as `https://dispatch.internal.example`. Dispatch's client at the issuer lists this address followed by `/auth/callback` as a callback URL. |
| The GitHub App | `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64` | Without it the server starts, but the web app's GitHub reads answer `503` and architecture sources cannot be saved. |
| A stable session key | `DISPATCH_SIGNING_KEY` | Signs sign-in cookies and seals the issuer's refresh tokens Dispatch stores. When unset, Dispatch creates one under `~/.local/share/dispatch`; keep that directory on a volume, or every restart signs everyone out. Changing the key signs everyone out and leaves every stored refresh token unopenable, so each person signs in again, but revokes none at the issuer: after the key leaks, sign people out at the issuer too. |
| An Envoy listener | `ENVOY_URL` | Where the Agents page finds live agent sessions. Defaults to `http://127.0.0.1:9020`. |
| NATS | `NATS_URLS`, `NATS_NKEY_SEED_FILE` | Where Dispatch publishes its events for agents. The Agents page's live view also needs it. Dispatch refuses a NATS server on another machine unless `ENVOY_ALLOW_REMOTE_NATS=1`. Set `DISPATCH_NATS_DISABLED=1` to run without it. |
| An address to listen on | `DISPATCH_LISTEN_HOST`, `DISPATCH_PORT` | Port `8766` by default. |

These are the settings most deployments need, not all of them. The full list, generated from the
server, is in [Configuration](/legion/dispatch/reference/configuration/).
When neither `NATS_URLS` nor `DISPATCH_NATS_DISABLED=1` is set, Dispatch reads the NATS address
from `envoy.natsUrls` in `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`, the file agents use,
whatever that file's `envoy.enabled` says, so set one of them explicitly.
A Dispatch that reaches NATS only through a SOCKS5 proxy, such as a tailnet sidecar's, sets
`ENVOY_NATS_SOCKS5_PROXY` to the proxy's `host:port` (for example `127.0.0.1:1055`) with
`ENVOY_ALLOW_REMOTE_NATS=1`, and every NATS connection then goes through the proxy, which is
handed each server's host name unresolved.

People sign in through a Cognito sign-in pool or with Google directly by default, and Dispatch
names each person by their lowercase email. `DISPATCH_IDENTITY=header:<Header-Name>` names people by a request header instead; it is for
tests and local harnesses only, needs `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, and refuses every
`DISPATCH_SIGNIN_*` setting. Serve Dispatch over HTTPS. `DISPATCH_INSECURE_COOKIE=1` is only for
trying it over plain HTTP on your own machine.

## Signing people in

Dispatch signs people in with OpenID Connect's authorization code flow, in one of two ways, chosen
by which of `DISPATCH_SIGNIN_GROUP` and `DISPATCH_SIGNIN_DOMAIN` is set beside the issuer, the client
ID and the client secret. Setting both, or some of the settings without the others, refuses the
start, naming what is wrong.

- **Through a Cognito sign-in pool**, federating whatever identity provider the deployment
  chooses: `DISPATCH_SIGNIN_ISSUER` is
  the pool's issuer, the client is Dispatch's confidential app client in the pool, and
  `DISPATCH_SIGNIN_GROUP` names the pool group (`cognito:groups`) a person must be in. Dispatch
  names the person by the email in their pool username, never by the `email` claim, which a
  person can write.
- **With Google directly**, for one Workspace domain: `DISPATCH_SIGNIN_ISSUER` is
  `https://accounts.google.com`, the client is a Google OAuth client, and
  `DISPATCH_SIGNIN_DOMAIN` is the domain, a lowercase name such as `example.com`. A domain under
  any other issuer, and a group under Google's, refuse the start: only Google's ID tokens carry
  `hd`, and they carry no group. A person signs in when their ID token's `hd` (the account's
  Workspace domain) is the domain and Google has verified their `email`; Dispatch names them by
  that email, lowercased. Anyone else reaches a page saying why: not in the domain, email not
  verified, or no email. No AWS account is involved.

  Set `DISPATCH_SIGNIN_DOMAIN` to the organization's **primary** domain: Google puts the primary
  domain in `hd` for every account of the organization, whichever domain its address is on. A
  secondary domain in the setting admits nobody, and a member whose address is on a secondary
  domain signs in, named by that address.

The Google client to create, in the Google Cloud console of the organization whose Workspace it
is, is a **web application** OAuth client with `<DISPATCH_SERVER_URL>/auth/callback` as an
authorized redirect URI. Set its OAuth consent screen's user type to **Internal**, so that only the
organization's own accounts can complete the sign-in at Google. Google accepts an `https` redirect
whose host ends in a public suffix, or plain `http` to a loopback address such as
`http://127.0.0.1:8766/auth/callback` for a server on your own machine. Its client ID and secret
are `DISPATCH_SIGNIN_CLIENT_ID` and `DISPATCH_SIGNIN_CLIENT_SECRET`. Dispatch asks Google for
offline access, with consent on every sign-in, so each sign-in returns the refresh token Dispatch
confirms the person with, and names the domain to Google's account chooser as a hint; the check
that admits a person is Dispatch's own.

Either way, Dispatch keeps the issuer's refresh token, sealed, and confirms the person with it at
least hourly: the first request after the hour refreshes the sign-in and applies the same rule to
the refreshed ID token. A person the rule no longer admits loses every browser session, and so
does anyone whose refresh fails for any reason, an outage at the issuer included: while the issuer
cannot be reached, people are signed out as their hour lapses, and sign in again once it answers.
Logout ends Dispatch's session only, not the issuer's.

A person is their email address, so an address that passes from one account to another is one
person to Dispatch. With Google directly, Dispatch also records the Google account (its `sub`,
which Google never reuses) that signed in to each address: when another account signs in to the
same address, as when a Workspace admin gives a deleted account's address to a new one, Dispatch
ends every session of the address before it signs the new account in, and the hourly confirmation
ends them should the refreshed sign-in name another account. Through a Cognito pool there is no
such record: the old holder of a reassigned address keeps the sessions they already have until
their cookie expires, 30 days after it was issued, unless the pool user is removed from
`DISPATCH_SIGNIN_GROUP` or disabled.

Moving a deployment from the Cognito pool to a domain needs no session-generation bump: the first
sign-in under the domain records a subject the address's row did not hold before, which ends
every session of the address by itself (the same check that ends a reassigned address's sessions,
above). It still needs the refresh-token step: revoke everyone's refresh tokens at the pool (a
global sign-out, or disabling the app client), then clear any still stored in plain text —
`update people set refresh_token = null, confirmed_at = null where refresh_token not like 'v1:%'`
— before the cutover: domain mode's own retirement sends such a row's token to Google's
revocation endpoint instead, which cannot revoke a token Google never issued, and logs an ERROR
every boot until the row is cleared or its person signs in again. A person's own next sign-in
under the domain overwrites their sealed pool token anyway, but someone who never signs in again
keeps theirs in `people` indefinitely, still redeemable at the pool until it is revoked there.

Moving the other way, from a domain back to the Cognito pool, needs both steps: the group sign-in
path never records or compares a subject, so nothing ends the domain sessions on its own, and it
never clears the recorded subject either. Revoke the refresh tokens at Google (remove the
client's access), then clear them in Dispatch's database, clear the recorded subject along with
them, and end every browser session,
`update people set refresh_token = null, confirmed_at = null, sub = null; update user_sessions set generation = generation + 1`,
and start Dispatch with the new settings. Skipping the session step there leaves a cookie issued
before the switch valid: the first sign-in under the pool records a fresh confirmation for the
address, and that cookie's generation still matches. Skipping the `sub` clear leaves a later
return to domain mode unprotected: the same Google account signing in again finds its own
`sub` still on the row, so nothing ends sessions a pool-era cookie still held. Everyone then signs
in again under the new issuer; a refresh token left from the old one would be refused at the
first hourly confirmation, and is still redeemable at the old issuer until it is revoked there.

## Running it on your machine

To see Dispatch working, seeded with example issues, asks, agents and a broadcast and opened
already signed in, run `bun run dispatch:local` at the repository root. It starts Dispatch beside
stand-ins for an Envoy listener and GitHub, empties and reseeds its database every time it starts,
and prints the address to open; `packages/dispatch/README.md` ("Local instance") covers it.

To run Dispatch on a database that keeps what you put in it, from a checkout of this repository,
with Docker, Bun, and Go installed:

1. Start a Postgres:

   ```sh
   packages/envoy/scripts/dev-postgres.sh
   ```

   It runs a container named `dispatch-pg` on `127.0.0.1:55432` from a pgvector image, which
   Dispatch's migrations need, and starts or reuses that container when it is already there. A
   `dispatch-pg` made from another image is refused, with the commands that move its data onto the
   right one.

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
   any message bus. `DISPATCH_DEV_SIGNIN=1` lets you sign in without an issuer.

4. Open `http://127.0.0.1:8766/auth/_dev/signin?login=<your email>`. It signs in anyone by the email
   it names, with no issuer. Dispatch accepts it only on your own machine, with every address in the
   setup on that machine, and that server needs a database of its own.

To sign in for real instead, replace `DISPATCH_DEV_SIGNIN=1` in step 3 with the issuer, client ID,
client secret and group or domain settings, and list `http://127.0.0.1:8766/auth/callback` as a
callback URL of Dispatch's client: the sign-in pool's app client, or a Google web client, with
`DISPATCH_SIGNIN_ISSUER=https://accounts.google.com` and `DISPATCH_SIGNIN_DOMAIN=example.com` for
your domain. With Google directly, the server needs no AWS account or credentials: without them
it logs that meaning search is unavailable and serves everything else.

## First steps after it starts

Sign in, then open **Settings**:

- **Projects**: create your first project, with a key such as `CORE` and a name.
- **Repositories → Projects**: choose which project issues from each GitHub repository land in.
  `DISPATCH_DEFAULT_PROJECT` catches repositories with no mapping; that project must already exist.
- **Architecture sources**: point a project at a repository branch holding its architecture
  model. Saving checks that the GitHub App can read it.
- **Delivery timeline**: name the repository that deploys, its deploy and pull-request-check
  workflows, the job whose success is a production deploy, and whose merged pull requests count.
  Three job names are optional and feed the [Pipeline](/legion/dispatch/delivery/#pipeline) page:
  the deploy workflow's staging end-to-end job and staging deploy job (each left empty when the
  pipeline has no such step, and the page then omits its card), and the PR-checks workflow's
  roll-up job, which only reports the other jobs' result and which the failing-jobs table leaves
  out. Saving checks that the GitHub App can read the repository. Until it is set, the
  [Delivery](/legion/dispatch/delivery/) page shows the same form. Every save, of any field,
  starts the delivery import over: what is already stored stays on both pages, the freshness line
  reads "never checked" until the next five-minute pass, that pass lists the last 28 days from
  GitHub again, and two re-listings walk those 28 days a day per pass, about 28 passes in all:
  one of each run's branch and event, and one of each deploy run's creation time, which decides
  the deploy a pull request is credited to.
- **Agent tokens**: make a token for each agent you run.
  [Dispatch for agents](/legion/dispatch/for-agents/#connecting-an-agent) covers the rest.

## Health check

`GET /healthz` needs no credential. It answers `200` when the server is healthy:

```json
{"ok":true,"db":true,"nats":null,"files":null,"commit":null,"schema_version":63}
```

- `db` is whether Postgres answered within two seconds. When it does not, the answer is `503` with
  `db: false`.
- `nats` is whether NATS is connected, or `null` when NATS is turned off.
- `files` is whether the file store's bucket (`DISPATCH_FILE_STORE_BUCKET`) answered within two
  seconds, or `null` when no bucket is set. It never decides `ok` or the status: a bucket that
  stops answering is `200` with `files: false`, so an outage of the bucket fails uploads and
  downloads and nothing else; a check that wants the bucket asserts `files: true` itself.
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
sizes, and any long-running transactions in the way. Run before a deploy, it is a gate: a census
that refuses, such as one counting rows a migration would rewrite, should fail the deploy before
any server moves. Dispatch repairs rows it already holds in code, at start, for that reason.

## The HTTP API

`GET /api/v1` lists every API route with its method, who may call it, and a description. The
[HTTP API reference](/legion/dispatch/reference/api/) is the same list.
