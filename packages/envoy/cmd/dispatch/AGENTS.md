# Dispatch HTTP Server (Go)

`cmd/dispatch` serves the Dispatch dashboard, native API, document rooms, GitHub
OAuth, and the GitHub REST/GraphQL proxy.

## Startup and persistence

`DATABASE_URL` and `DISPATCH_AGENT_TOKEN` are required. Startup opens a
`pgxpool.Pool`, applies embedded migrations from
`internal/dispatch/store/migrations`, then starts HTTP serving. The Postgres
store contains users, native issues, artifacts, document updates, and the event
outbox.

`DISPATCH_REPO_PROJECTS` optionally maps specific external repositories to
native issue projects; `DISPATCH_DEFAULT_PROJECT` names the project that
catches every other repository (required unless `DISPATCH_REPO_PROJECTS`
already covers every repository the deployment will see; validated against
Postgres at boot). An issue created from a repository that only resolves
through the default also gets a `repo:owner/name` label so it stays
filterable. `DISPATCH_NATS_DISABLED=1` leaves database and SSE paths
available and makes `/healthz` report `nats: null`. Otherwise Dispatch reads
`natsUrls` from shared `envoy.json`, connects through `bus.Connect`, and runs
the outbox. Host adapters can override their configured Dispatch base URL
with `DISPATCH_URL`; the server reads `dispatch.serverUrl` from shared
`envoy.json`.

Documents use a Yjs `Y.Text` named `content`. `GET /ws/doc/{room}` uses
Hocuspocus framing. Server-side edits use the document service, persist updates,
and create settled or named versions.
## Identity

`internal/dispatch/identity` is the human identity boundary. Handlers resolve
users through `Identity.Login` and write identity errors with
`identity.WriteError`.

- `DISPATCH_IDENTITY=cookie` is the default. Cookie identity requires
  `DISPATCH_ALLOWED_LOGINS`; GitHub OAuth accepts only those logins before
  storing a token pair and issuing a cookie.
- `DISPATCH_IDENTITY=header:<Header-Name>` accepts only allowlisted logins from
  a trusted proxy header. When GitHub OAuth credentials are configured, it also
  requires `DISPATCH_IDENTITY_HEADER_TRUSTED=1`.
- GitHub OAuth credentials come from `DISPATCH_APP_CLIENT_ID` and
  `DISPATCH_APP_CLIENT_SECRET`, or the Dispatch app credentials file.
- Agents authenticate with `Authorization: Bearer $DISPATCH_AGENT_TOKEN` and a
  `session` actor. Bearer callers cannot act as users.

The GitHub proxy needs the resolved user's stored GitHub token. Without one it
returns `503 GITHUB_TOKEN_UNAVAILABLE`.
## Routes

Every `/api/v1` route accepts an authenticated user or an agent bearer unless
the table says human only.

| Path | Method | Access | Purpose |
| --- | --- | --- | --- |
| `/auth/start` | GET | public | Start GitHub OAuth. |
| `/auth/callback` | GET | OAuth state | Exchange an allowlisted GitHub login's token pair. |
| `/auth/logout` | POST | identity | Remove the resolved user's tokens. |
| `/auth/whoami` | GET | identity | Return the resolved login. |
| `/api/github/rest/...` | any | identity | Proxy GitHub REST with the user's token. |
| `/api/github/graphql` | POST | identity | Proxy GitHub GraphQL with the user's token. |
| `/healthz` | GET | public | Report database and NATS readiness. |
| `/api/v1/projects` | GET, POST | POST human only | List or create projects. |
| `/api/v1/issues` | GET, POST | POST human or bearer | List or create native issues. |
| `/api/v1/issues/{key}` | GET, PATCH | PATCH human or bearer | Read or update an issue. |
| `/api/v1/issues/resolve` | GET | user or bearer | Resolve an external issue reference to its native key. |
| `/api/v1/issues/{key}/events` | GET | user or bearer | Read events by forward cursor, descending page, or exact IDs. |
| `/api/v1/inbox` | GET | user or bearer | List open asks, newest first. |
| `/api/v1/issues/{key}/asks` | POST | user or bearer | Create an ask. |
| `/api/v1/asks/{id}` | GET | user or bearer | Read an ask. |
| `/api/v1/asks/{id}/answer` | POST | human only | Answer an open ask. |
| `/api/v1/issues/{key}/comments` | GET, POST | user or bearer | List or create comments and suggestions. |
| `/api/v1/comments/{id}` | GET | user or bearer | Read a comment and its reply chain. |
| `/api/v1/comments/{id}/resolve` | POST | user or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/accept` | POST | human only | Apply and accept a suggestion. |
| `/api/v1/comments/{id}/reject` | POST | human only | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | user or bearer | Post a short issue message. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | user or bearer | List or upload artifact versions. |
| `/api/v1/artifacts/{id}/primary` | POST | human only | Make a document the issue primary artifact. |
| `/api/v1/artifacts/{id}` | GET | user or bearer | Read an artifact, versions, and references. |
| `/api/v1/artifacts/{id}/text` | GET | user or bearer | Read a live document's markdown. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | user or bearer | Read a document version or download a blob. |
| `/api/v1/artifacts/{id}/versions` | POST | user or bearer | Create a named live-document version. |
| `/api/v1/artifacts/{id}/edits` | POST | user or bearer | Apply document edit operations. |
| `/api/v1/me/state` | GET | identity | Read the user's issue UI state. |
| `/api/v1/me/issues/{key}/state` | PUT | identity | Update the user's issue UI state. |
| `/api/v1/events` | GET | identity | Stream durable events with SSE. |
| `/ws/doc/{room}` | GET | user or bearer | Join the Hocuspocus document room. |

## Checks

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
