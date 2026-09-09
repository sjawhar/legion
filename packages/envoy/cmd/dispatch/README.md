# Dispatch HTTP server

Go binary serving the Dispatch dashboard, GitHub OAuth sign-in, and the
per-user GitHub REST and GraphQL proxy. Dispatch stores user OAuth tokens and
application state in Postgres.

## Required configuration

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. Dispatch applies embedded migrations before serving. |
| `DISPATCH_AGENT_TOKEN` | Shared bearer token for agent API callers. |
| `DISPATCH_ALLOWED_LOGINS` | Comma-separated GitHub login allowlist. Required for cookie identity mode and enforced during OAuth sign-in. |

`DISPATCH_REPO_PROJECTS` is optional and maps GitHub repositories to Dispatch
projects for the agent API.

`dispatch.serverUrl` in the merged `envoy.json` identifies Dispatch's public
base URL. The server recognizes native issue, artifact, ask, and comment links
under that URL when it stores post references.

GitHub App credentials come either from these environment variables or from
`~/.local/share/dispatch/app.json`; environment variables take precedence:

| Variable | Purpose |
| --- | --- |
| `DISPATCH_APP_CLIENT_ID` | GitHub App OAuth client ID. |
| `DISPATCH_APP_CLIENT_SECRET` | GitHub App OAuth client secret. |
| `DISPATCH_APP_PEM_B64` | Base64-encoded GitHub App private key. |
| `DISPATCH_SIGNING_KEY` | Stable HMAC key for cookie sessions. |

When no GitHub App credentials are configured, the server still starts, but
OAuth and GitHub proxy routes respond with `503`.

## Identity

`DISPATCH_IDENTITY` controls how browser requests identify a human:

- `cookie` (the default) accepts signed `dsession` cookies. GitHub OAuth
  callbacks reject logins outside `DISPATCH_ALLOWED_LOGINS` before token
  persistence or cookie issuance.
- `header:<Header-Name>` trusts a reverse-proxy identity header and checks the
  allowlist on every request. This mode logs a boot warning. When
  `DISPATCH_APP_CLIENT_ID` is set, it requires
  `DISPATCH_IDENTITY_HEADER_TRUSTED=1` to prevent a direct client from
  supplying its own header.

`DISPATCH_INSECURE_COOKIE=1` omits the `Secure` attribute for local plain-HTTP
testing. Do not use it on an HTTPS deployment.

## Local Postgres

Start the local database once:

```sh
cd packages/envoy
./scripts/dev-postgres.sh
```

It prints the `DATABASE_URL` to use for development and tests. The database
retains the migrated schema; tests only truncate rows they create.

## Running locally

```sh
cd packages/envoy
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
DISPATCH_AGENT_TOKEN=local-agent-token \
DISPATCH_IDENTITY='header:X-Dispatch-User' \
DISPATCH_ALLOWED_LOGINS=sjawhar \
DISPATCH_INSECURE_COOKIE=1 \
go run ./cmd/dispatch
```

The default listen address is `:8766`. Set `DISPATCH_LISTEN_HOST` and
`DISPATCH_PORT` to change it.

## Routes

| Path | Method | Identity | Purpose |
| --- | --- | --- | --- |
| `/auth/start` | GET | none | Start the GitHub OAuth web flow. |
| `/auth/callback` | GET | OAuth state | Exchange OAuth code, enforce allowlist, persist tokens, issue cookie. |
| `/auth/logout` | POST | cookie or trusted header | Remove the caller's stored tokens and clear the session cookie. |
| `/auth/whoami` | GET | cookie or trusted header | Return the resolved GitHub login. |
| `/api/github/rest/...` | any | cookie or trusted header | Proxy a GitHub REST request using the caller's stored token. |
| `/api/github/graphql` | POST | cookie or trusted header | Proxy GitHub GraphQL using the caller's stored token. |
| `/healthz` | GET | none | Report Postgres readiness. |
| `/api/v1/inbox?project=` | GET | cookie, trusted header, or bearer | List open asks newest-first, including their issue key and title. |
| `/api/v1/issues/{key}/asks` | POST | cookie, trusted header, or bearer | Create an optionally anchored ask. |
| `/api/v1/asks/{id}` | GET | cookie, trusted header, or bearer | Read an ask. |
| `/api/v1/asks/{id}/answer` | POST | cookie or trusted header | Answer an open ask. |
| `/api/v1/issues/{key}/comments?artifact=` | GET | cookie, trusted header, or bearer | List comments, optionally limited to an artifact ID. |
| `/api/v1/issues/{key}/comments` | POST | cookie, trusted header, or bearer | Create a comment, reply, or suggestion. |
| `/api/v1/comments/{id}/resolve` | POST | cookie, trusted header, or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/accept` | POST | cookie or trusted header | Apply and accept an anchored suggestion. |
| `/api/v1/comments/{id}/reject` | POST | cookie or trusted header | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | cookie, trusted header, or bearer | Post an issue message. |
| `/api/v1/artifacts/{id}` | GET | cookie, trusted header, or bearer | Read an artifact and its incoming references. |
| `/...` | GET | none | Serve the dashboard static files. |

A caller resolved by header identity without a stored GitHub token receives
`503` with code `GITHUB_TOKEN_UNAVAILABLE` from GitHub proxy routes.

## Tests

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
