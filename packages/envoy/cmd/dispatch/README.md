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
| `DISPATCH_TEST_HOOKS` | Set to `1` to mount `POST /api/v1/events/_test/disconnect`, which closes every open SSE connection. Test/e2e only — leave unset in every real deployment. |
| `ENVOY_URL` | Base URL of the Envoy listener (`GET /v1/sessions`) behind `GET /api/v1/agents`; defaults to `http://127.0.0.1:9020`. |

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries. Existing dashboard mappings take
precedence over this seed. Dispatch resolves every external issue through the
stored mapping, then falls back to `DISPATCH_DEFAULT_PROJECT` when configured.
An unmapped external repository without a default project is rejected. An issue
created through the default also gets a `repo:owner/name` label.

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

`DISPATCH_DEFAULT_PROJECT` must already exist in Postgres — running the
command below against a fresh database applies migrations, then exits with
`DISPATCH_DEFAULT_PROJECT "LOCAL" does not exist`. Create the project once:

```sh
docker exec dispatch-pg psql -U postgres -d dispatch \
  -c "insert into projects (key, name) values ('LOCAL', 'Local project')"
```

Then run (or re-run) the server:

```sh
cd packages/envoy
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
DISPATCH_AGENT_TOKEN=local-agent-token \
DISPATCH_IDENTITY='header:X-Dispatch-User' \
DISPATCH_ALLOWED_LOGINS=sjawhar \
DISPATCH_INSECURE_COOKIE=1 \
DISPATCH_DEFAULT_PROJECT=LOCAL \
go run ./cmd/dispatch
```

The default listen address is `:8766`. Set `DISPATCH_LISTEN_HOST` and
`DISPATCH_PORT` to change it.

## Document migration preflight

Before deploying against a database that may contain documents from before the
tree model, inspect it without writing any data:

```sh
cd packages/envoy
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go run ./cmd/dispatch check-documents
```

The command prints each document's tree or legacy state, legacy markdown parse
result, and anchor/resolvable-anchor counts. It exits nonzero for an unparseable
legacy document. Normal server boot performs the one-shot conversion from legacy
`Y.Text` rooms and offset anchors to the ProseMirror tree and mark anchors.

Migration `0009_project_artifacts` deletes malformed derived artifact references, reports their
count, and re-derives them from source text on the next write. It aborts server boot before a
migration record or schema change only when an existing artifact has no owning issue. On success
it backfills each artifact's project and generated `ref_key`.

## Search

Migration 0010 adds stored generated `search` columns. Postgres computes them on every write, so
no application code writes or refreshes the search vectors. Search covers issue titles, the latest
settled document text, comments, asks (questions and free-text answers), and messages. Live
document text takes up to the 2 s settle delay to appear in search results.

Search snippets are escaped text with only server-inserted `<mark>` elements around matches. Native
issue creation rejects a title that near-duplicates an existing issue in the same project with
`409 POSSIBLE_DUPLICATE` and up to five candidates; `force` bypasses that check, and external
references skip it.

To measure search latency against a restored corpus copy, run:

```sh
DISPATCH_ADMIN_URL='postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable' \
  bash packages/envoy/scripts/restore-dispatch-dump.sh ~/dispatch.dump dispatch_search_bench
DISPATCH_BENCH_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_search_bench?sslmode=disable' \
  go test ./internal/dispatch/api/ -run TestSearchLatencyOnCorpus -count=1 -v
```

The restore script replaces only the named scratch database. Set `DISPATCH_ADMIN_URL` to point at
the Postgres `postgres` database for a non-default local port.

## Routes

| Path | Method | Identity | Purpose |
| --- | --- | --- | --- |
| `/auth/start` | GET | none | Start the GitHub OAuth web flow. |
| `/auth/callback` | GET | OAuth state | Exchange OAuth code, enforce allowlist, persist tokens, issue cookie. |
| `/auth/logout` | POST | cookie or trusted header | Remove the caller's stored tokens and clear the session cookie. |
| `/auth/whoami` | GET | cookie or trusted header | Return the resolved GitHub login. |
| `/api/github/rest/...` | any | cookie or trusted header | Proxy a GitHub REST request using the caller's stored token. |
| `/api/github/graphql` | POST | cookie or trusted header | Proxy GitHub GraphQL using the caller's stored token. |
| `/healthz` | GET | none | Report Postgres and NATS readiness. |
| `/api/v1/events` | GET | cookie, trusted header, or bearer | Stream durable events with SSE. Omitting `since` (a cold client) subscribes before resolving the current head internally, so no separate request can race it. |
| `/api/v1/events/_test/disconnect` | POST | as above, plus `DISPATCH_TEST_HOOKS=1` | Close every open SSE connection; not mounted unless `DISPATCH_TEST_HOOKS=1`. |
| `/api/v1/inbox?project=` | GET | cookie, trusted header, or bearer | List open asks newest-first, including their issue key and title. |
| `/api/v1/agents` | GET | cookie or trusted header | List live Envoy sessions (`session_id`, `title`, `dir`, `machine_id`, `roles`, `capabilities`, `last_seen`), newest first; `503 ENVOY_UNAVAILABLE` when the listener cannot be reached. |
| `/api/v1/projects` | GET | cookie, trusted header, or bearer | List projects (`key`, `name`, `open_asks`, `created_at`), ordered by key. |
| `/api/v1/projects` | POST | cookie or trusted header | Create a project from `key` and `name`; rejects a duplicate key with `409 PROJECT_EXISTS`. |
| `/api/v1/settings/repo-projects` | GET | cookie or trusted header | List external repository-to-project mappings. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | cookie or trusted header | Create or replace, or remove, an external repository mapping. |
| `/api/v1/issues?project=&status=&parent=&updated_since=` | GET | cookie, trusted header, or bearer | List issue summaries. Filters are optional; `updated_since` is RFC3339 and inclusive, matching issue changes and later issue events. Summaries contain `key`, `title`, `status`, `parent`, `updated_at`, `last_seq`, and `open_asks`. |
| `/api/v1/search?q=&project=&limit=` | GET | cookie, trusted header, or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results with `<mark>` snippets and SPA `href`s; `limit` 1–50 (default 20). `400 INVALID_QUERY` under 2 characters or stop words only; `400 INVALID_LIMIT`. |
| `/api/v1/issues/{key}/references` | GET | cookie, trusted header, or bearer | Read the issue's eight-hop artifact reference closure. An `If-None-Match` value equal to the response ETag returns `304`. |
| `/api/v1/issues` | POST | cookie, trusted header, or bearer | Create an issue and its primary document. Omitting or leaving `spec` blank seeds the writing-a-spec skeleton. Refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. |
| `/api/v1/issues/{key}/asks` | POST | cookie, trusted header, or bearer | Create an optionally anchored ask. An anchor is exactly `{artifact, quote, occurrence?}` for a server-written quote mark or `{artifact, mark_id}` for a mark already written by a browser. |
| `/api/v1/issues/{key}/asks?state=` | GET | cookie, trusted header, or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). |
| `/api/v1/asks/{id}` | GET | cookie, trusted header, or bearer | Read an ask and its reply thread. |
| `/api/v1/asks/{id}/answer` | POST | cookie or trusted header | Answer an open ask. |
| `/api/v1/asks/{id}/resolve` | POST | cookie, trusted header, or bearer | Retract or self-resolve an open ask with a recorded reason. |
| `/api/v1/issues/{key}/comments?artifact=` | GET | cookie, trusted header, or bearer | List comments, optionally limited to an artifact ID. |
| `/api/v1/issues/{key}/comments` | POST | cookie, trusted header, or bearer | Create a comment, root-level reply, or suggestion. An anchored comment uses the same quote-or-mark-ID shape as an ask; replies inherit their root's anchor and send none. |
| `/api/v1/comments/{id}` | GET | cookie, trusted header, or bearer | Read a comment and its reply chain. |
| `/api/v1/comments/{id}` | PATCH | cookie or trusted header | Edit a comment body. Human authors only. |
| `/api/v1/comments/{id}/resolve` | POST | cookie, trusted header, or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/reopen` | POST | cookie or trusted header | Reopen a resolved thread-root comment. |
| `/api/v1/comments/{id}/accept` | POST | cookie or trusted header | Apply and accept an anchored suggestion. |
| `/api/v1/comments/{id}/reject` | POST | cookie or trusted header | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | cookie, trusted header, or bearer | Post an issue message. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List issue artifacts or create a version from a multipart `file` or JSON `{name, content, summary?, actor?}`. The JSON form requires `Content-Type: application/json`. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List non-primary project artifacts (or only unlinked documents with `?unlinked=true`), or create an unlinked project artifact. |
| `/api/v1/artifacts/{id}` | GET | cookie, trusted header, or bearer | Read an artifact, its versions, and incoming references. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/text` | GET | cookie, trusted header, or bearer | Read a live document's markdown. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | cookie, trusted header, or bearer | Read a document version or download a blob. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions` | POST | cookie, trusted header, or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | cookie, trusted header, or bearer | Apply document edit operations. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | cookie, trusted header, or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | cookie, trusted header, or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | cookie, trusted header, or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | cookie, trusted header, or bearer | Read an artifact's outgoing and incoming reference edges. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an issue artifact's live document markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an issue artifact version or download its blob. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply issue-document edit operations. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an unlinked project document version or download its blob. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply project-document edit operations. |
| `/...` | GET | none | Serve the dashboard static files. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. Use
`go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1`
to print one matching envelope from the `natsUrls` configured in `envoy.json`.

A caller resolved by header identity without a stored GitHub token receives
`503` with code `GITHUB_TOKEN_UNAVAILABLE` from GitHub proxy routes.

## Document errors

| Status / code | Meaning |
| --- | --- |
| `404 TARGET_NOT_FOUND` | The quote requested by an anchor or document edit is absent. |
| `409 TARGET_AMBIGUOUS` | A quote matches more than once without an `occurrence`; the response includes candidate ranges and context. |
| `409 ANCHOR_MISSING` | A browser submitted a `mark_id` that the server did not observe in the live tree. |
| `409 ANCHOR_ORPHANED` | An operation needs a mark whose anchored text has been deleted. |
| `400 INVALID_ANCHOR` | An anchor must provide exactly one of a nonempty `quote` or nonempty `mark_id`, with its document artifact. |
| `400 INVALID_MARKDOWN` | Uploaded document content cannot be represented by the Proof schema. Malformed edit replacements report `INVALID_OP`. |
| `500 DOC_SCHEMA` | The live tree contains a node or mark outside the Proof schema and cannot be rendered safely. |

## Comment errors

| Status / code | Meaning |
| --- | --- |
| `400 INVALID_COMMENT` | A reply must target a thread-root comment. |
| `403 NOT_AUTHOR` | Only a comment's author may edit it. |

## Tests

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test -timeout 60s ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
