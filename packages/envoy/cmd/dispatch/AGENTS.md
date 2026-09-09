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

## Checks

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
