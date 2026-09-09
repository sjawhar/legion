# Dispatch HTTP Server (Go)

`cmd/dispatch` runs the Go server for the Dispatch dashboard. It owns HTTP
startup, Postgres migration, identity selection, GitHub OAuth sign-in, and the
GitHub REST/GraphQL proxy.

## Startup

The server requires `DATABASE_URL` and `DISPATCH_AGENT_TOKEN`. It opens a
`pgxpool.Pool`, applies embedded migrations from
`internal/dispatch/store/migrations`, and only then starts serving.

`DISPATCH_REPO_PROJECTS` is optional configuration for agent-side issue
resolution. The router receives it with the agent token for API handlers.

## Identity

`internal/dispatch/identity` is the sole human identity boundary. Handler code
must call `Identity.Login` and pass any error to `identity.WriteError`.

- `DISPATCH_IDENTITY=cookie` is the default. `CookieIdentity` validates a
  signed `dsession`; `DISPATCH_ALLOWED_LOGINS` is required and the OAuth
  callback enforces it before persisting tokens or issuing a cookie.
- `DISPATCH_IDENTITY=header:<Header-Name>` uses `HeaderIdentity`. Every
  request's supplied login must be in `DISPATCH_ALLOWED_LOGINS`. This mode
  deliberately logs a warning and is rejected alongside
  `DISPATCH_APP_CLIENT_ID` unless `DISPATCH_IDENTITY_HEADER_TRUSTED=1`.

GitHub proxy handlers need a stored user token after identity resolution. If
one is absent, they return `503 GITHUB_TOKEN_UNAVAILABLE`.

## Persistence

`internal/dispatch/store.Store` holds the shared `*pgxpool.Pool`.
`Store.Migrate` records each committed migration in `schema_migrations`.
`PgUserStore` implements `auth.UserStore` against the `users` table. Existing
file and NATS KV user stores are intentionally absent; users sign in again
after this cutover.

Use `scripts/dev-postgres.sh` to start the local `postgres:16` container. Its
printed connection string is also the expected
`DISPATCH_TEST_DATABASE_URL`.

## Routes

| Path | Method | Identity | Purpose |
| --- | --- | --- | --- |
| `/auth/start` | GET | none | Begin GitHub OAuth. |
| `/auth/callback` | GET | OAuth state | Exchange and persist an allowed login's token pair. |
| `/auth/logout` | POST | `Identity` | Remove the resolved user's stored tokens. |
| `/auth/whoami` | GET | `Identity` | Return the resolved login. |
| `/api/github/rest/...` | any | `Identity` | Proxy GitHub REST with the resolved user's token. |
| `/api/github/graphql` | POST | `Identity` | Proxy GitHub GraphQL with the resolved user's token. |
| `/healthz` | GET | none | Report database readiness. |
| `/api/v1/inbox?project=` | GET | `Identity` or bearer | Open asks, newest first, with issue key and title. |
| `/api/v1/issues/{key}/asks` | POST | `Identity` or bearer | Create an ask, optionally anchored to a document range. |
| `/api/v1/asks/{id}` | GET | `Identity` or bearer | Read an ask. |
| `/api/v1/asks/{id}/answer` | POST | `Identity` | Answer an open ask; bearer callers are forbidden. |
| `/api/v1/issues/{key}/comments?artifact=` | GET | `Identity` or bearer | List comments; an artifact ID limits results to anchored comments. |
| `/api/v1/issues/{key}/comments` | POST | `Identity` or bearer | Create a comment, reply, or anchored suggestion. |
| `/api/v1/comments/{id}/resolve` | POST | `Identity` or bearer | Mark a comment resolved. |
| `/api/v1/comments/{id}/accept` | POST | `Identity` | Apply and accept an anchored suggestion. |
| `/api/v1/comments/{id}/reject` | POST | `Identity` | Reject a suggestion. |
| `/api/v1/issues/{key}/events` | GET | `Identity` or bearer | List events: `after` (ascending), `order=desc` with optional exclusive `before` (descending), or up to 50 exact `ids` (ascending); paged requests are capped at 200. |
| `/api/v1/issues/{key}/messages` | POST | `Identity` or bearer | Post a short issue message. |
| `/api/v1/artifacts/{id}` | GET | `Identity` or bearer | Read an artifact, including posts that reference it. |

## Checks

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
