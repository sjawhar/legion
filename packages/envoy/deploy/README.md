# Deploy

Compose definitions run the on-prem Envoy listener, the Dispatch server against an external
database, and a single-node Envoy with its own NATS. All services use host networking. The
listener serves `127.0.0.1:9020` for local session registration and webhook ingress; Dispatch
serves `127.0.0.1:8766` by default for the SPA, Google sign-in through a Cognito sign-in pool or
with Google directly, and the native Dispatch API.

## Host configuration lives in `compose/.env`

Docker Compose reads `compose/.env` next to the compose files on every
`docker compose` invocation, so the host's Dispatch settings belong there — not
in a shell session. A `docker compose up` run by anyone (or any agent) then
reproduces the same deployment; exported variables only override it.
`compose/.env` holds secrets and is git-ignored; `compose/dispatch.env.example`
is the template for Dispatch:

```bash
cp compose/dispatch.env.example compose/.env
$EDITOR compose/.env
```

`compose/single-node.env.example` is the template for `single-node.compose.yml`;
the site's Envoy page ("Running Envoy on one machine") uses it.

For browser access over the tailnet, bind Dispatch to the host's Tailscale
IPv4 address and allow the plain-HTTP session cookie. The tailnet link is
already encrypted; nothing else on the host can reach that address:

```
DISPATCH_LISTEN_HOST=100.x.y.z        # tailscale ip -4
DISPATCH_SERVER_URL=http://example-host-devbox:8766 # the exact browser URL and sign-in callback origin
DISPATCH_INSECURE_COOKIE=1            # cookies over the http:// tailnet URL
```

The origin humans open in the browser is the sign-in callback origin: the server builds
`<DISPATCH_SERVER_URL>/auth/callback` from `DISPATCH_SERVER_URL` in `compose/.env`, and
Dispatch's client at the sign-in issuer — the sign-in pool's app client, or the Google web
client — must list exactly that URL. On Sami's devbox that is `http://example-host-devbox:8766`
(recorded as `DISPATCH_PUBLIC_ORIGIN` in `compose/.env`); a deploy whose `/auth/start` redirect
stops matching it breaks sign-in for everyone, so the auto-deployer checks the redirect after
every deploy and rolls back on a mismatch.

Never bind `0.0.0.0`: that exposes the sign-in endpoints and session cookies on
every interface.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `compose/single-node.compose.yml` — Envoy on one machine: a JetStream NATS of its own and the
  listener, both on loopback, for sessions that need to reach only each other. The site's Envoy
  page ("Running Envoy on one machine") walks through it.
- `compose/dispatch.compose.yml` — the production Dispatch server against an external database
  (`DATABASE_URL`).
- `compose/dispatch.acceptance.compose.yml` — isolated Postgres and Dispatch services for a
  deployed-image acceptance run, building the image itself; it is separate so production Compose
  commands need no acceptance-only harness variables, and the acceptance recipe none of
  production's.
- `scripts/up-listener.sh` — starts the listener with `docker compose`.
- `scripts/up-dispatch.sh` — starts Dispatch with `docker compose`.
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
| `ENVOY_ALLOW_REMOTE_NATS` | yes, for a shared NATS | Set to `1` to reach a NATS that is not this host's own. The listener owns the `ENVOY_NOTIFICATIONS` stream, so every start reconciles it; without this the start refuses a non-loopback `NATS_URLS`, naming the URL. `compose/listener.compose.yml` sets it. |
| `NATS_NKEY_SEED_FILE` | no | A file holding the NATS nkey user seed this process connects as (trimmed); wins over `NATS_NKEY_SEED`. A set but empty, missing, unreadable, blank or non-user-seed value refuses startup naming the variable and path. Neither set connects without a credential. |
| `NATS_NKEY_SEED` | no | The NATS nkey user seed itself, when `NATS_NKEY_SEED_FILE` is unset. |
| `ENVOY_NATS_SOCKS5_PROXY` | no | A SOCKS5 proxy, as `host:port`, every NATS connection goes through (reconnects included), each server's host name handed to it unresolved. A value that is not `host:port`, or that holds an nkey seed, refuses startup naming the variable, without printing the value; a set proxy also needs `ENVOY_ALLOW_REMOTE_NATS=1`. Unset dials NATS directly. |
| `ENVOY_LISTENER_PORT` | no | Defaults to `9020`. |
| `ENVOY_LISTEN_HOST` | no | Defaults to `127.0.0.1`. |
| `ENVOY_API_TOKEN` | conditional | Required when `ENVOY_LISTEN_HOST` is not loopback and no OIDC pair is set; matching bearer token for Listener API requests. Empty no longer opens `/v1` while a verifier is configured. |
| `ENVOY_OIDC_ISSUER` | conditional | OIDC issuer whose projected service-account tokens authenticate to `/v1` beside `ENVOY_API_TOKEN`. Set with `ENVOY_OIDC_AUDIENCE` or not at all; half the pair refuses to boot naming the missing one, as does an issuer that does not answer discovery. |
| `ENVOY_OIDC_AUDIENCE` | conditional | Audience those tokens must carry (`envoy`). Set with `ENVOY_OIDC_ISSUER` or not at all. |
| `ENVOY_ALLOWED_HOSTS` | conditional | Comma-separated host names or addresses (no port) an unauthenticated listener serves `/v1` to beside loopback; required for each name clients reach it by when neither `ENVOY_API_TOKEN` nor the OIDC pair is set and they do not dial loopback. Any other `Host` gets a JSON 421. Unused when a bearer is required. |
| `ENVOY_API_ALLOW_UNAUTHENTICATED` | Fargate transition only | Set to `1` only temporarily to start a non-loopback listener with neither credential. |
| `ENVOY_HOST_BRIDGE` | no | Where the listener reaches a session on this machine that registers a port (OpenCode's serve port). Unset, it is `host.docker.internal`; the compose files set `127.0.0.1`, since host networking does not resolve `host.docker.internal`. |
| `ENVOY_WEBHOOKS` | no | Comma-separated enabled webhook providers. Without `github` the listener opens no CI store and publishes no CI settlements. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | conditional | Required when GitHub webhooks are enabled. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | no | Defaults to `@legion`. |
| `ENVOY_SLACK_SIGNING_SECRET` | conditional | Required when Slack webhooks are enabled. |
| `ENVOY_GHOSTWISPR_SIGNING_SECRET` | optional | Empty skips Ghost Wispr signature verification. |

A process is given a NATS nkey seed only after its NATS server has nkey users (the server's stage 1,
which keeps a `no_auth_user` fallback): a server with no users sends no nonce, and the Go client
refuses the nkey (`nats: nkeys not supported by the server`) instead of connecting without it.

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
| `DISPATCH_SERVER_URL` | yes | Public browser origin and sign-in callback origin. It must be the URL humans type into their browser; Dispatch's client at the sign-in issuer (the sign-in pool's app client, or the Google web client) must list `<DISPATCH_SERVER_URL>/auth/callback`. |
| `NATS_URLS` | yes | Comma-separated NATS URLs for Dispatch. |
| `ENVOY_ALLOW_REMOTE_NATS` | yes, for a shared NATS | Set to `1` to reach a NATS that is not this host's own. The server owns the `ENVOY_NOTIFICATIONS` stream, so every start reconciles it; without this the start refuses a non-loopback NATS, naming the URL. `compose/dispatch.compose.yml` sets it. |
| `NATS_NKEY_SEED_FILE` | no | A file holding the NATS nkey user seed this process connects as (trimmed); wins over `NATS_NKEY_SEED`. A set but empty, missing, unreadable, blank or non-user-seed value refuses startup naming the variable and path. Neither set connects without a credential. |
| `NATS_NKEY_SEED` | no | The NATS nkey user seed itself, when `NATS_NKEY_SEED_FILE` is unset. |
| `ENVOY_NATS_SOCKS5_PROXY` | no | A SOCKS5 proxy, as `host:port`, every NATS connection Dispatch makes goes through (reconnects included), each server's host name handed to it unresolved. A value that is not `host:port`, or that holds an nkey seed, refuses startup naming the variable, without printing the value; a set proxy also needs `ENVOY_ALLOW_REMOTE_NATS=1`. Unset dials NATS directly. |
| `DISPATCH_AGENT_TOKEN` | yes | Shared devbox fallback bearer token; per-person tokens minted in Dispatch Settings are preferred for individual agents. To rotate it, set the new value followed by the old one, separated by a space or a line feed, move every consumer, then set the new value alone (`cmd/dispatch/README.md`). |
| `DISPATCH_SIGNIN_ISSUER` / `DISPATCH_SIGNIN_CLIENT_ID` / `DISPATCH_SIGNIN_CLIENT_SECRET`, with `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | cookie identity | The issuer people sign in through, Dispatch's client there and its secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with the issuer `https://accounts.google.com` and a Google web client. Cookie identity requires the issuer, client ID, secret and one of the two at startup; some without the others, both the group and the domain, a domain under another issuer, or a group under Google's refuse to boot. Header identity is for tests and local harnesses only, and refuses every one of these settings. `DISPATCH_ALLOWED_LOGINS` is removed and refused at startup. |
| `DISPATCH_LISTEN_HOST` | no | Defaults to `127.0.0.1`; for direct tailnet access, set it to `$(tailscale ip -4)`, never `0.0.0.0`. |
| `DISPATCH_PORT` | no | Defaults to `8766`; the healthcheck follows it. |
| `DISPATCH_IDENTITY` | no | `cookie` (default) is Dispatch's production identity. `header:<name>` is only for tests and local harnesses. |
| `DISPATCH_IDENTITY_HEADER_TRUSTED` | test/local header identity | Set to `1` for every header-identity test or local harness. A missing flag refuses boot. |
| `DISPATCH_REPO_PROJECTS` | no | Optional boot seed for repository-to-project settings (`owner/repo=KEY,...`). Existing dashboard settings are not overwritten. |
| `DISPATCH_DEFAULT_PROJECT` | no | Native Dispatch project for external repositories without a stored mapping. |
| `DISPATCH_NATS_DISABLED` | no | Set to `1` to run database and SSE paths without NATS. |
| `ENVOY_URL` | no | Envoy listener base URL for live agent titles; defaults to `http://127.0.0.1:9020` (set it when `ENVOY_LISTENER_PORT` changes). |
| `ENVOY_TOKEN` | conditional | Matching Listener API bearer token when `ENVOY_API_TOKEN` protects the listener. |
| `DISPATCH_OIDC_ISSUER` | conditional | OIDC issuer whose projected service-account tokens authenticate as agents beside `DISPATCH_AGENT_TOKEN`. Set with `DISPATCH_OIDC_AUDIENCE` or not at all; half the pair refuses to boot naming the missing one, as does an issuer that does not answer discovery. |
| `DISPATCH_OIDC_AUDIENCE` | conditional | Audience those tokens must carry (`dispatch`). Set with `DISPATCH_OIDC_ISSUER` or not at all. |
| `DISPATCH_URL` | no | Host-adapter override for the Dispatch base URL; use with `DISPATCH_TOKEN`. |
| `DISPATCH_TOKEN` | host adapters | Bearer token paired with `DISPATCH_URL`. |
| `DISPATCH_APP_CLIENT_ID` | GitHub App | The GitHub App's client ID; with its private key (the `pem` in the data volume's `app.json`) Dispatch handles webhooks and architecture sources and reads GitHub for the web app. `DISPATCH_APP_CLIENT_SECRET` is removed and refused at startup. |
| `DISPATCH_INSECURE_COOKIE` | HTTP | Set to `1` whenever browsers reach Dispatch over `http://` (the tailnet deployment); leave unset behind an HTTPS terminator. |

The Dispatch signing material is a named volume; the database is external.

## Backups and restore

The database's nightly logical backup is not this compose's job: the production
deployment's own scheduled backup task `pg_dump`s the `dispatch` database to object
storage and alerts when a day has no dump; restores go through that deployment's own
restore tooling. `scripts/autodeploy.sh` still takes its own pre-deploy dump of
`DATABASE_URL` before every image roll, kept locally for rollback.

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```

This ships only `deploy/`; the remote runs a pinned image and does not build
the listener or Dispatch image locally.
