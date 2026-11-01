---
title: Running Dispatch
description: What the Dispatch server needs, how to run it, its health check, running more than one copy, and its migrations.
sidebar:
  order: 8
---

Dispatch is one Go program. It serves the web app, the HTTP API, live documents, and Google
sign-in. Its source is `packages/envoy/cmd/dispatch`, and the web app it serves is built from
`packages/dispatch`. The container image `ghcr.io/sjawhar/legion/envoy` carries it as
`/usr/local/bin/envoy-dispatch`.

Pin the image by digest. Each `ghcr.io/sjawhar/legion/envoy:<commit>` image that
`.github/workflows/release-envoy-listener.yaml` publishes carries a GitHub artifact attestation
from that workflow (images published before attestation began carry none), and a tag does not say
which run pushed it (a run dispatched by hand from any branch pushes one too), so accept a digest
only when a run on `main` built it:

```bash
gh attestation verify oci://ghcr.io/sjawhar/legion/envoy@sha256:… --repo sjawhar/legion \
  --cert-identity https://github.com/sjawhar/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

`latest` is a separate image index with its own digest and no attestation. To start from it, read
the commit it names and pin that commit's image instead:

```bash
docker buildx imagetools inspect --raw ghcr.io/sjawhar/legion/envoy:latest \
  | jq -r '.annotations["org.opencontainers.image.revision"]'
docker buildx imagetools inspect ghcr.io/sjawhar/legion/envoy:<that commit>   # its Digest is the one to pin
```

## What it needs

| Need | Setting | Notes |
| --- | --- | --- |
| Postgres | `DATABASE_URL` | Required. Dispatch keeps everything here and applies its own migrations at start. Leave `pool_max_conns` out of the URL; Dispatch refuses it. |
| A shared agent token | `DISPATCH_AGENT_TOKEN` | Required to start. A fallback credential for agents; agents normally use personal tokens made in Settings. To change it without locking agents out, set the new value followed by the old one, separated by a space, until every agent has moved, then the new one alone. |
| Google sign-in | `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET`, and `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | The OpenID Connect issuer people sign in through, Dispatch's client there and that client's secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with Google itself as the issuer. Required for the default cookie sign-in. [Signing people in](#signing-people-in) covers both. |
| The browser address | `DISPATCH_SERVER_URL` | The exact address people type, such as `https://dispatch.internal.example`. Dispatch's client at the issuer lists this address followed by `/auth/callback` as a callback URL. |
| The GitHub App | `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64` | `DISPATCH_APP_PEM_B64` holds the App's private key as the PEM GitHub gives or the base64 of that PEM; a key that does not parse stops boot. Without the App the server starts, but the web app's GitHub reads answer `503` and architecture sources cannot be saved. |
| A stable session key | `DISPATCH_SIGNING_KEY` | Signs sign-in cookies and seals the issuer's refresh tokens Dispatch stores. It holds one key, or several separated by spaces: the first signs and seals, and every one is accepted, which is how the key is changed without signing anyone out ([Changing the session key](#changing-the-session-key)). When unset, Dispatch creates one under `~/.local/share/dispatch`; keep that directory on a volume, or every restart signs everyone out. Give every server the same keys when you run more than one: a server whose keys another running server does not share refuses to start ([Running more than one copy](#running-more-than-one-copy)). |
| An Envoy listener | `ENVOY_URL` | Where the Agents page finds live agent sessions. Defaults to `http://127.0.0.1:9020`. |
| NATS | `NATS_URLS`, `NATS_NKEY_SEED_FILE` | Where Dispatch publishes its events for agents. The Agents page's live view also needs it. Dispatch refuses a NATS server on another machine unless `ENVOY_ALLOW_REMOTE_NATS=1`. A NATS that is down, or refuses Dispatch's credential, when Dispatch starts does not stop it: Dispatch serves, connects once NATS answers, and publishes what was written meanwhile. Set `DISPATCH_NATS_DISABLED=1` to run without it. |
| An address to listen on | `DISPATCH_LISTEN_HOST`, `DISPATCH_PORT` | Port `8766` by default. |
| Meaning search | `DISPATCH_EMBEDDER`, and for Azure the `DISPATCH_AZURE_*` settings | Search by meaning embeds text with Cohere Embed v4 on AWS Bedrock (the default, with the AWS SDK's region and credentials) or on Azure AI Foundry (`DISPATCH_EMBEDDER=azure`: the endpoint, the deployment's tokens-per-minute quota, and either the deployment's key or a keyless sign-in through a federated token). Without it, search answers by words alone and says so. The server embeds everything it already holds by itself, at start and every ten minutes; `GET /api/v1/search/coverage` shows how far it has got. |

These are the settings most deployments need, not all of them. The full list, generated from the
server, is in [Configuration](/legion/dispatch/reference/configuration/).
When neither `NATS_URLS` nor `DISPATCH_NATS_DISABLED=1` is set, Dispatch reads the NATS address
from `envoy.natsUrls` in `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`, the file agents use,
whatever that file's `envoy.enabled` says, so set one of them explicitly.
A Dispatch that reaches NATS only through a SOCKS5 proxy, such as a tailnet sidecar's, sets
`ENVOY_NATS_SOCKS5_PROXY` to the proxy's `host:port` (for example `127.0.0.1:1055`) with
`ENVOY_ALLOW_REMOTE_NATS=1`, and every NATS connection then goes through the proxy, which is
handed each server's host name unresolved.
Dispatch's server creates and updates the `ENVOY_NOTIFICATIONS` stream as the Envoy listener
does, so give it the listener's `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES`
([Running the listener](/legion/dispatch/envoy/#running-the-listener)): it creates the stream and
its webhook redelivery bucket with that many copies, raises one it finds with fewer, raises a
smaller byte limit, or none, to its own, never lowers either, and with no byte limit of its own
keeps the one the stream has. The stream holds the most copies and the largest limit any Envoy
program of the deployment carries, so a clustered deployment sets both alike in every listener and
in Dispatch's server. Like the listener, it reads the stream again every 30 seconds and raises
copies and a limit a start with other settings lowered
([Keeping the stream](/legion/dispatch/envoy/#keeping-the-stream)).

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
does anyone whose refresh token the issuer refuses as expired or revoked. A refresh the issuer
does not answer, as in an outage at the issuer, signs nobody out: that request is refused with
`503 SIGNIN_UNAVAILABLE`, and the person's first request after the issuer answers again refreshes
and goes through. Logout ends Dispatch's session only, not the issuer's.

Dispatch does not need the issuer to start. It reads the issuer's settings in the background,
retrying until the issuer answers, and until then a sign-in answers `503 SIGNIN_UNAVAILABLE` while
agents, and people whose hour has not lapsed, are served. A sign-in started on one server completes
on whichever server the issuer sends the browser back to, since what it needs travels in a signed
cookie, as long as every server has the same `DISPATCH_SERVER_URL`.

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

## Changing the session key

`DISPATCH_SIGNING_KEY` takes the new key beside the old one, so the key changes without signing
anyone out. Make the change in three steps, and restart every server after each before the next:

1. Add the new key second, after the current one: `<old key> <new key>`. Every server still signs
   with the old key and now accepts the new one.
2. Move the new key first: `<new key> <old key>`. Every server signs with the new key and still
   accepts the old one, so the sign-ins made under it keep working.
3. Remove the old key, no sooner than 30 days after step 2: a sign-in cookie lasts 30 days, and each
   stored refresh token is sealed again under the new key by its person's next hourly confirmation.

Each step keeps every server's keys shared with the step before, so a rolling restart passes the
check that refuses a server whose keys another running server does not share. Replacing the key in
one step fails that check beside a server still on the old key, and on a single server signs
everyone out.

Two rules come with it. The first release of Dispatch that takes several keys is never the release
you first change the key in, and while the setting holds two keys, never roll back to a release
older than that: such a release reads both keys as one, and signs everyone out. Removing a key
revokes no refresh token at the issuer: whoever holds a database backup and the old key can still
use the tokens sealed under it until they expire, so after a key leaks, sign people out at the
issuer too.

Separate the keys with one space or one line feed. Every whitespace character separates two keys,
so a key itself holds none, and two in a row, a CR LF among them, enclose an empty key, which
Dispatch refuses to start with.

On a deployment built by `envoy-infra`'s `gcp:Dispatch`, you write no key and edit no Secret:
each step above is one `pulumi up` of the component's `signingKeyGenerations`, the ordered list
of key generations `DISPATCH_SIGNING_KEY` holds. A deployment running one key is at `[0]`; step 1
is `[0, 1]`, step 2 is `[1, 0]`, and step 3 is `[1]`. The component generates each generation's
key, renders them in the list's order and replaces the app's pods on each apply, which is the
restart each step needs. Its README (`packages/envoy/infra/README.md`) holds the rest of the
procedure: which pods roll, what a dropped generation costs, and the two lists the component
refuses.

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

   It runs a container named `dispatch-pg` on `127.0.0.1:55432` from a pinned image, pgvector on
   PostgreSQL 18 (Dispatch's migrations need its `vector` extension, and 18 is the version
   Dispatch's tests and dev databases run), and starts or reuses that container when it is already
   there. A `dispatch-pg` made from another image or PostgreSQL major is refused, with the commands
   that dump its databases and restore them into a new container on the right one.

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
your domain. With Google directly, the server needs no AWS account or credentials. Search by meaning
needs AWS credentials for Bedrock, or the Azure settings ([What it needs](#what-it-needs)); without
either, the server logs that meaning search is unavailable and serves everything else.

## First steps after it starts

Sign in, then open **Settings**:

- **Projects**: create your first project, with a key such as `CORE` and a name.
- **Repositories → Projects**: choose which project issues from each GitHub repository land in.
  `DISPATCH_DEFAULT_PROJECT` catches repositories with no mapping; that project must already exist.
- **Projects → Default assignee**: name the person each project's unowned work lands on, in the
  projects table's own column. An issue created with no assignee, by an agent on the shared token
  and under no parent that names one, goes to that person; a project with no default leaves such
  an issue unassigned. Only a person sets it, here — no agent command or flag does — and the
  picker's **Unassigned** clears it.
- **Architecture sources**: add each repository branch holding part of a project's architecture
  model. A project can have several, one per repository, and its model is all of their components
  together. Saving checks that the GitHub App can read the repository.
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

`GET /healthz` needs no credential. It answers `200` while the server can serve requests, which is
while Postgres answers:

```json
{"ok":true,"db":true,"nats":null,"files":null,"signin":null,"service_tokens":null,"commit":null,"fleet_commit":null,"schema_version":121}
```

It is a readiness check, not a liveness check: it fails while the database cannot be reached, so a
liveness probe on it would restart every server at once during a database failover. Point a load
balancer's or Kubernetes' readiness probe at it.

- `ok` is whether this server can serve requests, the same as `db`.
- `db` is whether Postgres answered within two seconds. When it does not, the answer is `503` with
  `db: false`.
- `nats` is whether NATS is connected, or `null` when NATS is turned off. A server serves while NATS
  is down, so it never decides `ok` or the status, and a NATS that stops answering without closing
  the connection reads `false` within three seconds.
- `files` is whether the file store's bucket (`DISPATCH_FILE_STORE_BUCKET`) answered within two
  seconds, or `null` when no bucket is set. It never decides `ok` or the status: a bucket that
  stops answering is `200` with `files: false`, so an outage of the bucket fails uploads and
  downloads and nothing else.
- `signin` and `service_tokens` are whether the sign-in issuer and the service-account token issuer
  have been reached, or `null` when that issuer is not set. Neither decides `ok` or the status.
- `commit` is the source commit this server's image was built from, or `null` in a build that was
  not stamped.
- `fleet_commit` is the commit every running server was last found serving, the latest announced
  build, or `null` before any was found. It is read from the database, so it is reported while NATS
  is down too. It can trail by a few seconds.
- `schema_version` is the newest database migration applied.

A check that needs NATS, the bucket or sign-in asserts `nats: true`, `files: true` or `signin: true`
itself.

## Metrics

`GET /metrics` needs no credential either, so whoever reaches the server's HTTP port can read it.
It answers in the Prometheus text format with one counter,
`envoy_dispatch_outbox_publish_failures_total`: one count for each destination of an event
publish that did not reach NATS, labelled `reason`. `denied` is a publish NATS refused under the
server's own grant, `refused` one NATS refuses however often it is retried, and `failed` any other
failure. A denied publish also logs `dispatch outbox: publish event` at ERROR, naming the subject.

## Behind a proxy

A page's live updates ride one WebSocket per tab, `GET /ws/live`. A browser keeps WebSockets apart
from the six connections it opens to a host for ordinary requests, so a socket that stays open
never takes one of them. That holds only while the load balancer or proxy in front of Dispatch
does not advertise HTTP/2 extended CONNECT (`SETTINGS_ENABLE_CONNECT_PROTOCOL`): a browser that
sees it runs the socket inside its HTTP/2 connection instead, and pages can hang until the tab is
reloaded. From a checkout of this repository with `bun install` run,
`bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` prints the settings the proxy sends and
exits 1 when it advertises extended CONNECT, 0 when it does not or speaks no HTTP/2, and 2 when it
gets no answer. Run it after any change to the proxy.

An open document keeps a second WebSocket, `GET /ws/doc/<id>`, for as long as it is open. Each
socket carries a message while the page is quiet: Dispatch writes a heartbeat to `/ws/live` every
15 seconds, and on `/ws/doc` the page's document provider renews its presence every 15 to 18
seconds, which ygo sends back to it. A page connected read-only gets an empty presence reply
instead: a closed issue's document, and also a page on an open issue whose `schema_version` no
longer matches the server's, such as a tab left open across an upgrade. A proxy whose idle timeout
is longer than about 20 seconds therefore never closes either one.

## Running more than one copy

Dispatch runs as more than one server against one database, so a new version rolls out without
taking Dispatch away from everyone: start the new version, let it take traffic, then stop the old
one. Three things have to be true first; Dispatch checks the first for you.

- **Give every server the same `DISPATCH_SIGNING_KEY`.** A sign-in made on one server is checked by
  whichever server answers next, so each server must accept the key the others sign with. Left
  unset, each server makes a key of its own in its own data directory. Each server records the
  servers that are running in the database, with a fingerprint of each key that reveals nothing of
  it, and a server whose keys a running server does not share refuses to start: it logs why,
  naming the other server and the steps of [changing the session key](#changing-the-session-key),
  and exits before it serves anything. Before it exits it waits up to 15 seconds and one more
  report (5 seconds) for the other server to stop reporting in, so a server started right after
  another was killed, which never removed itself from the database, starts once the killed
  server's record lapses; a server that is running keeps reporting in and keeps it refused. A
  rolling update to a changed key stalls the same way on the first new server while the old ones
  keep serving. A server that stopped reporting in for longer than 15 seconds (frozen, or cut off
  from the database) is not counted meanwhile, so it checks its keys again before it counts once
  more: beside a running server whose keys it does not share, it logs why, finishes the requests
  it is answering, and exits 1.
- **Give every server the same embedder settings.** Each stored vector records the embedder that
  made it, and each server drops the vectors another embedder made and embeds that text again, so
  two servers set up differently keep replacing each other's work. A rollout that changes the
  settings settles once the last old server stops. For the same reason, a new model on Azure goes
  under a new deployment name: the name, not the model, tells the vectors apart.
- **Every migration leaves the version before it able to read and write the database**
  ([Migrations](#migrations)), because a rolling update runs both versions against the one
  database while it lasts.
- **Expect an issue's notifications out of order while two servers publish.** Each server's
  events outbox claims a batch of events at a time, and nothing yet keeps two servers from
  publishing adjacent batches at once, so a later event of an issue can reach NATS before an
  earlier one, and the Legion daemon drops the earlier one as stale. Run one server, and replace it
  rather than rolling it (Kubernetes `strategy: Recreate`), until a single-publisher lock lands.

A second server also costs connections, and Postgres counts them per server. Each server opens up
to 64 connections for requests, up to 14 for the periodic work it runs once across the servers,
up to 4 for loading documents, one for the health check, one for the events outbox, up to 2 for
reading where the event log is safe to stream from (the event horizon), and one for its entry in
the list of running servers, which it renews every five seconds. It also holds up to four more
open for as long as it runs. Three of them wait on Postgres notifications: one for delivery, one
for the live socket's event-feed relay, and one that the server shares for everything else,
its document sessions and the waking of its background work when the rows that work reads change
included. The other servers on the same database find each server by the name of that shared
connection. The fourth sends the cursors of the server's document sessions and its questions and
answers to the other servers about a document. It opens the first time the server has one to
send. A document edit's own notification goes out in the transaction that writes the edit, and
needs no connection of its own. That is 91 connections for one server at its ceiling, and twice
that for two, past a default Postgres's limit of 100. Check the database's connection limit
before you start the second server: a limit sized for one server, with whatever else uses that
database counted in, is the thing that runs out.

What a reader sees while two servers are serving:

- Nothing they write is lost, whichever server takes it. Every edit of a document is appended
  under that document's lock, edits made at the same time merge rather than overwrite, and the
  stored history holds all of them.
- Two people on one document see each other's typing, cursors and presence, whichever server each
  one is on. Each server tells the others through Postgres, so this works while the message bus
  is down. A read never returns text older than what is stored. An agent's edit sees what a
  person on another server just typed, when that server answers within half a second.
- A document session stays open while the other servers write. The server closes it, and its
  browsers reconnect, in these cases only:
  - The document's stored history is replaced, for example by a rebuild.
  - Its issue closes or reopens. The editors on every server reconnect read-only or editable to
    match.
  - The session fails on its server, which then reloads it.
  - Its server shuts down while an editor is connected.
- A server keeps an edit that it took before an issue's close was saved, or up to 2 seconds after
  that, even when the edit reaches the database after the close. An edit typed while the close is
  still being saved is kept too, however long the close takes. The server refuses an edit that it
  took later than that, and the session reconnects read-only.
- When a server's listener connection to the database drops, the server reconnects. Its first
  wait is 1 second, plus up to half again. Each next wait doubles, up to 30 seconds plus up to half
  again, so one wait can last 45 seconds. Until it reconnects, the editors open on that server do
  not receive the other servers' edits and cursors. They catch up when the connection is back. In
  that time a read through the API still returns the stored text. Nothing anyone writes is lost,
  except an edit to a document whose issue closed more than 2 seconds before (above).
- During the rollout that brings this version, a server of the earlier version behaves as before.
  It still closes a session that falls behind, and its browsers reconnect. It sends no cursors to
  the other servers and shows none of theirs. It does not act when an issue is closed or reopened
  through a server of this version, so its open sessions of that issue keep the access they had.
  A document that the close or reopen writes is the exception. A close writes the issue's open
  asks into their documents as resolved, and a reopen writes them open again. The earlier server's
  session of such a document falls behind, so it closes, and its browsers reconnect with the
  access the issue now allows.
- A page of either version keeps loading. Each server stores its version of the dashboard's files
  in the database when it starts, before it takes traffic, and serves the other version's files
  from there, so a page one server sent finds its files whichever server answers next. A server
  that cannot store them does not start, and the roll keeps the servers it would have replaced. A
  version older than the first that stores them stores nothing, so a roll away from one of those
  versions can still leave a page asking for files no server has.
- The dashboard's **A new version is available** notice waits until every server runs one
  version: while two versions are serving, nobody is told to reload. A page that fails to load one
  of its files reloads itself, at most three times in five minutes.
- Signing in works whichever server each step reaches, and a sign-in made on one server is good on
  the other.

Dispatch announces a new version on NATS (`server.started`) once every running server serves it,
not as each one starts: a rollout announces once its last old server has stopped, restarting
servers on the version already announced announces nothing, and rolling back announces the
earlier version again.

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

A new version rolls into the running one: every migration Dispatch ships is written to leave the
version before it able to read and write the database, so both can run against it while the roll
lasts ([Running more than one copy](#running-more-than-one-copy)). A change that cannot be
written that way is called out in its pull request, and that deploy stops the running server
before the new version starts (on Kubernetes, `strategy: Recreate`).

Once a new version has migrated, don't start the older one again: if the new version fails, fix
it forward. Dispatch does not refuse to start against a schema newer than its own, so an older
server keeps writing to the migrated database as if nothing changed. Some releases depend on that
never happening: the one that adds each issue's repository (migration `0113`) holds every issue to
32 links below its root in its API alone, and an issue an older server puts deeper fails to load
until you clear the parent of an ancestor that still loads. That release was deployed with the
running server stopped first, and no server older than it has run against the database since.

The release that moves live updates onto one WebSocket per tab and keys every write (INFRA-258)
is one of those changes: deploy it with no server of the earlier version running. An earlier
server ignores a write's `Idempotency-Key`, so while both run a write whose answer was lost can be
run once by each: the page and the `dispatch` command both retry such a write, and its retry can
reach the other version. At one replica the Deployment's `Recreate` already stops the old pod
first; above one replica, scale to zero before the new version starts. Releases after it roll
again. Rolling it back brings the hazard back the other way: a tab still open, and a `dispatch`
client that saw this version keep keys in the last five minutes, retry a write whose answer was
lost against the earlier server, which runs it again. So fix it forward, as above; if you must roll
back, stop this version first, knowing a write in flight at that moment may land twice. Once a
later release has dropped the broadcast send's old key table (migration `0141`), rolling back past
this version also stops broadcasts: the earlier version's broadcast send reads and writes that
table, so every broadcast send fails while the rest of that version keeps working. A tab opened
before the deploy needs a reload: its event route is gone and it shows
**Live updates unavailable**, and its broadcast body still carries a field the new server refuses.

From that release on, every page write that could otherwise happen twice carries an
`Idempotency-Key`, and Dispatch keeps each key's answer for 24 hours, so a change tried again after
a lost answer, a timeout or an outage is made once. `PUT`, `DELETE`, and the sign-out
(`POST /auth/logout`) carry no key; the first two are already idempotent and sign-out cannot create
content twice. The `dispatch` command keys every `POST` and `PATCH` it makes. A page that has not
heard back keeps trying and says so, and a script that calls the API itself can send a key of its
own ([For agents](/legion/dispatch/for-agents/#the-http-api)).

The release that lets a project import its components from several repositories (migration
`0117`) is deployed the same way: stop every server of the earlier version, or scale it to zero,
before the new version starts, and never start a version older than it again. An older server
cannot work with the new database: saving an architecture source fails with a server error, an
import that changes a project's components fails, and an import of an empty architecture folder
deletes the components of every source of the project. Its census refuses while any component
belongs to a project with no architecture source, which only a change made directly in the
database leaves behind; delete those rows on the version still running, then take the census
again:

```sql
delete from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);
```

## The HTTP API

`GET /api/v1` lists every API route with its method, who may call it, and a description. The
[HTTP API reference](/legion/dispatch/reference/api/) is the same list.
