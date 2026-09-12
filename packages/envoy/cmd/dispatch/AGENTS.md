# Dispatch HTTP Server (Go)

`cmd/dispatch` serves the Dispatch dashboard, native API, document rooms, GitHub
OAuth, and the GitHub REST/GraphQL proxy.

## Startup and persistence

`DATABASE_URL` and `DISPATCH_AGENT_TOKEN` are required. Startup opens a
`pgxpool.Pool`, applies embedded migrations from
`internal/dispatch/store/migrations`, then starts HTTP serving. The Postgres
store contains users, native issues, artifacts, document updates, and the event
outbox.

Migration 0010 adds stored generated `search` columns; Postgres maintains them on writes and no
application code writes or refreshes them.

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries. Stored dashboard mappings are
authoritative, and external issues fall back to `DISPATCH_DEFAULT_PROJECT` when
configured. An unmapped repository without a default is rejected; issues that
use the default get a `repo:owner/name` label. `DISPATCH_NATS_DISABLED=1` leaves
database and SSE paths available and makes `/healthz` report `nats: null`.
Otherwise Dispatch loads config in this precedence order: user `envoy.json`,
repository `envoy.json`, then environment overrides. A present `NATS_URLS`
overrides merged `natsUrls` before `bus.Connect` and the outbox start. A present
`DISPATCH_SERVER_URL` overrides merged `dispatch.serverUrl`; it must be an
absolute `http` or `https` URL with no path, and is the exact browser origin
used for GitHub OAuth. It must equal the URL humans type into the browser, with
`<DISPATCH_SERVER_URL>/auth/callback` registered on the GitHub App. Host
adapters can override their configured Dispatch base URL with `DISPATCH_URL`.
`DISPATCH_TEST_HOOKS=1` mounts `POST /api/v1/events/_test/disconnect` (closes
every open SSE connection, as if the server had restarted) — unset in every real
deployment; e2e's `run-server.sh` sets it so the web client's
reconnect-from-lastId path can be exercised without seeding thousands of
events to trip the SSE replay cap.

Each document room has two shared Yjs types: the authoritative
`Y.XmlFragment("prosemirror")` tree and `Y.Map("marks")`, the server-maintained
projection of Postgres comment, ask, and suggestion records. Go renders canonical
markdown from the tree for reads and versions; anchors are Proof marks, so their
stored rows carry a mark ID while the mark moves with its text. Boot migrates legacy
`Y.Text` rooms and offset anchors once; run `dispatch check-documents` beforehand to
inspect a database without changing it. Migration `0009_project_artifacts` prunes malformed
derived artifact references with a notice before altering the schema; the parser rejects them on
later source writes. It aborts server boot before recording the migration only when an artifact
has no owning issue. A successful migration backfills `project_key` and generated `ref_key`.

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
- Agents normally authenticate as a `session` actor with a personal `dsp_` token
  minted by a human in Settings, sent as `Authorization: Bearer <token>`.
  `DISPATCH_AGENT_TOKEN` is the shared devbox fallback; its callers have no
  owner attribution. Bearer callers cannot act as users.

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
| `/auth/whoami` | GET | identity | Return the resolved human identity. |
| `/api/github/rest/...` | any | identity | Proxy GitHub REST with the user's token. |
| `/api/github/graphql` | POST | identity | Proxy GitHub GraphQL with the user's token. |
| `/healthz` | GET | public | Report database and NATS readiness. |
| `/api/v1/projects` | GET, POST | POST human only | List projects (including `open_asks`) or create one. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | user or bearer | List non-primary artifacts in a project (`?unlinked=true` selects unlinked ones) or create an unlinked project artifact. |
| `/api/v1/settings/repo-projects` | GET | human only | List repository-to-project mappings. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | human only | Create or replace, or remove, a repository mapping. |
| `/api/v1/me/agent-tokens` | GET, POST | human only | List personal token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | human only | Revoke a personal agent token. |
| `/api/v1/issues` | GET, POST | POST human or bearer | List or create native issues. Creation refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. |
| `/api/v1/search?q=&project=&limit=` | GET | user or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results contain `<mark>` snippets and SPA `href`s. `limit` is 1–50 (default 20); an under-two-character or stop-word-only query returns `400 INVALID_QUERY`, and an invalid limit returns `400 INVALID_LIMIT`. |
| `/api/v1/issues/{key}` | GET, PATCH | PATCH human or bearer | Read or update an issue. |
| `/api/v1/issues/resolve` | GET | user or bearer | Resolve an external issue reference to its native key. |
| `/api/v1/issues/{key}/events` | GET | user or bearer | Read events by forward cursor, descending page, or exact IDs. |
| `/api/v1/issues/{key}/references` | GET | user or bearer | Read the eight-hop artifact reference closure; matching `If-None-Match` returns `304`. |
| `/api/v1/inbox` | GET | user or bearer | List open asks, newest first. |
| `/api/v1/agents` | GET | human only | List live Envoy sessions, newest first. `api/agents.go` proxies the listener through `internal/dispatch/envoy`; unavailable listener responses are `503 ENVOY_UNAVAILABLE`. |
| `/api/v1/issues/{key}/asks` | POST | user or bearer | Create an ask. |
| `/api/v1/issues/{key}/asks?state=` | GET | user or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). |
| `/api/v1/asks/{id}` | GET | user or bearer | Read an ask. |
| `/api/v1/asks/{id}` | PATCH | user or bearer | Edit one or more of `question`, `options`, `multiple`, or `urgency` while the ask is open. A bearer caller must be the asking session; a human may edit any open ask. The response has nullable `edited_at`; `ask.edited` records the full current ask, prior mutable fields, and `edited_by`. Ask anchors cannot be changed by this route. |
| `/api/v1/asks/{id}/answer` | POST | human only | Answer an open ask. |
| `/api/v1/asks/{id}/resolve` | POST | user or bearer | Retract or self-resolve an open ask with a recorded reason. |
| `/api/v1/issues/{key}/comments` | GET, POST | user or bearer | List or create comments and suggestions. |
| `/api/v1/comments/{id}` | GET | user or bearer | Read a comment and its reply chain. |
| `/api/v1/comments/{id}/resolve` | POST | user or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/accept` | POST | human only | Apply and accept a suggestion. |
| `/api/v1/comments/{id}/reject` | POST | human only | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | user or bearer | Post a short issue message. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | user or bearer | List issue artifacts or create a version from a multipart file or JSON inline content. The JSON form requires `Content-Type: application/json`. |
| `/api/v1/artifacts/{id}` | GET | user or bearer | Read an artifact, versions, and incoming references. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/text` | GET | user or bearer | Read a live document's markdown. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | user or bearer | Read a document version or download a blob. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions` | POST | user or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | user or bearer | Apply document edit operations. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | user or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | user or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | user or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | user or bearer | Read outgoing and incoming reference edges. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | user or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an issue artifact's live markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an issue artifact version or download its blob. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply issue-document edit operations. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | user or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an unlinked project document version or download its blob. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply project-document edit operations. |
| `/api/v1/me/state` | GET | identity | Read the user's issue UI state. |
| `/api/v1/me/issues/{key}/state` | PUT | identity | Update the user's issue UI state. |
| `/api/v1/events` | GET | identity | Stream durable events with SSE. Omitting `since` (a cold client) subscribes before resolving the current head internally, so no separate request can race it. |
| `/api/v1/events/_test/disconnect` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every open SSE connection; not mounted otherwise. |
| `/ws/doc/{room}` | GET | user or bearer | Join the Hocuspocus document room. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. `natstail` reads
`natsUrls` from `envoy.json` and prints a matching envelope:

```sh
go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1
```

## Checks

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
