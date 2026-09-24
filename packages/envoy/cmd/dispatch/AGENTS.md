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
stored rows carry a mark ID while the mark moves with its text. Schema version 8 is an
empty migration: it was recorded from Go by the one-time conversion of pre-Proof `Y.Text`
rooms and offset anchors, which every deployed database has run. Migration
`0009_project_artifacts` prunes malformed
derived artifact references with a notice before altering the schema; the parser rejects them on
later source writes. It aborts server boot before recording the migration only when an artifact
has no owning issue. A successful migration backfills `project_key` and generated `ref_key`.
Migration `0032_refs_provenance` gives every mention edge a `kind`, `created_at`, and
`source_seq` (the global `events.id` that introduced it), creates the `graph_edges` view over
`refs` and the structural columns, and refuses to run when any ask or comment anchor carries a
non-uuid `artifact_id`, since the view casts it on every row. `envoy-dispatch rebuild-refs`
reparses every document version, ask, comment, and issue message, reconciles `refs` with the
text (surviving edges keep their provenance, orphan sources are deleted), and prints counts; it
reads `DATABASE_URL` and the envoy config's `dispatch.server_url`, refusing an empty URL because
dashboard-URL mentions are recognised only against it.

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
- `DISPATCH_OIDC_ISSUER` + `DISPATCH_OIDC_AUDIENCE` (both or neither; half the
  pair refuses to boot naming the missing one) make a Kubernetes pod's projected
  service-account token a third bearer credential. `resolveBootConfig` only
  reads the pair, through the shared `oidc.ConfigFromEnv` the listener also
  uses; `oidc.Discover` runs in the boot path beside the store and document
  setup, under `oidc.DiscoveryTimeout`, so an issuer that is unreachable — or
  one that accepts the connection and never answers — refuses the boot naming
  it instead of hanging before the port is bound. A bearer is tried as the
  shared token first, and a JWT-shaped one
  (`oidc.LooksLikeJWT`) is then verified rather than looked up as a personal
  token: it succeeds as `{kind: "session", id: <body actor id>}` carrying
  `service`, the token's verified `sub`
  (`system:serviceaccount:<namespace>:<name>`), or is refused
  `401 OIDC_TOKEN_INVALID` naming the reason class — never demoted to the
  personal-token path, whose 401 carries `UNAUTHORIZED`. `service` is set from
  the token alone: `bearerSessionActor` copies it from the authenticated actor
  exactly as it copies `owner`, and a request body naming one is ignored.
  Unset, the branch does not exist and bearer handling is unchanged.

The GitHub proxy needs the resolved user's stored GitHub token. Without one it
returns `503 GITHUB_TOKEN_UNAVAILABLE`.
## Routes

The `/api/v1` routes are one table, `api/routes_table.go` (`routes()`): `Register` mounts it and
`GET /api/v1` (public) serves it as `{routes: [{method, path, auth, description}], docs}` sorted by
path then method. Add a route by adding a row — never a `mux.HandleFunc` line — and bump the pinned
count in `api/routes_table_test.go`. `auth` is `public`, `any` (user or bearer), `human`, or
`bearer`; it describes the check the handler makes, the handler still enforces it. An unknown path
under a server root (`/api`, `/v1`, `/auth`, `/ws`, `/healthz`) is answered by
`routes/router.go` with `404 {"code":"NOT_FOUND","error":"no route for <METHOD> <path>","hint":"GET
/api/v1 lists every route"}` before any dashboard lookup; only paths outside those roots fall back
to the SPA shell.

Every `/api/v1` route accepts an authenticated user or an agent bearer unless
the table says human only.

| Path | Method | Access | Purpose |
| --- | --- | --- | --- |
| `/api/v1` | GET | public | List every `/api/v1` route with method, auth, and purpose. |
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
| `/api/v1/settings/architecture-sources` | GET | human only | List every project's architecture source with its sync bookkeeping. |
| `/api/v1/projects/{key}/architecture-source` | GET, PUT, DELETE | GET user or bearer; PUT, DELETE human only | A project's architecture source (`{repo, branch}`; the directory is fixed at `.dispatch/architecture/`). PUT proves the GitHub App can read the repository first (`409 SOURCE_ACCESS` otherwise); DELETE removes the source and the model it projected; GET is `404 SOURCE_NOT_FOUND` without one. |
| `/api/v1/projects/{key}/architecture-source/sync` | POST | user or bearer | Import the model now; `200` carries the updated row (`last_error` set when the model was rejected and the previous projection stays up), `409 SOURCE_ACCESS` a credential or branch problem. |
| `/api/v1/projects/{key}/architecture` | GET | user or bearer | The component tree with the work attached: `source`, `totals` (`issues_done`/`issues_total` over every filed issue, `unassigned`, `not_architectural`, `components_without_work` over non-external components, `retired_links`), one row per component (`done`/`total` count the distinct issues whose effective set names it or any component it contains, parents and icebox included, each once; `own_*` only those naming it itself; `issues` say how each qualified: `direct` > `inherited` > `contained` with `via`), and the `unassigned`, `not_architectural` (with `reason`, `inherited_from`), and `retired_links` lists. `404 SOURCE_NOT_FOUND` without a source. |
| `/api/v1/me/agent-tokens` | GET, POST | human only | List personal token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | human only | Revoke a personal agent token. |
| `/api/v1/users` | GET | human only | The sign-in allowlist as `{users: [{login}]}`, sorted lowercase: the assignee picker's options (pure config, no DB). |
| `/api/v1/whoami` | GET | user or bearer | Who the server takes the caller for: `{kind: "user", login}` for a human, `{kind: "agent", owner, service}` for a bearer (`owner` is the personal token's lowercase login, null under the shared token; `service` is a verified service-account token's Kubernetes subject, null for every other bearer). |
| `/api/v1/issues` | GET, POST | POST human or bearer | List or create native issues. Creation refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. |
| `/api/v1/search?q=&project=&limit=` | GET | user or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results contain `<mark>` snippets and SPA `href`s. `limit` is 1–50 (default 20); an under-two-character or stop-word-only query returns `400 INVALID_QUERY`, and an invalid limit returns `400 INVALID_LIMIT`. |
| `/api/v1/issues/{key}` | GET, PATCH | PATCH human or bearer | Read or update an issue. `assignee` (an allowlisted login, lowercased; `null` clears; absent leaves it) may be set by any caller; an unlisted login is `400 ASSIGNEE_NOT_ALLOWED`. `components` is the issue's own architecture attachment: `null` or `{mode: "inherit"}` deletes it (the issue takes its nearest ancestor's again), `{mode: "explicit", ids}` names bare component ids of the issue's project (`400 COMPONENTS_INPUT` for an unknown, retired, external, or other-project id), `{mode: "none", reason}` declares the issue not architectural; it is the one field besides `rank` a closed issue accepts without reopening. Every issue read carries the effective `components` (`mode`, `ids`, `unknown` for retired ids, `reason`, `inherited_from`), resolved up the parent chain in the same query. |
| `/api/v1/issues/resolve` | GET | user or bearer | Resolve an external issue reference to its native key. |
| `/api/v1/issues/{key}/events` | GET | user or bearer | Read events by forward cursor, descending page, or exact IDs. |
| `/api/v1/issues/{key}/references` | GET | user or bearer | Read the eight-hop artifact reference closure; matching `If-None-Match` returns `304`. |
| `/api/v1/references?to=\|from=&kind=&since=` | GET | user or bearer | Edges of one node in the reference graph, newest first, cross-project. Exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference; `kind` is a csv of `mentions`, `child_of`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`, `part_of`, `depends_on`, `affects`; `since=<events.id>` keeps mentions introduced after it and excludes structural edges. Each edge carries the other `node` (`kind`, `id`, `issue_key`, `project`, `ref`; no `ref` for sessions; a `component` node is `<project>/<id>` with ref `dispatch://<PROJECT>/component/<id>`, and one a re-import retired is omitted with its edges), an `excerpt` (the containing block, with `block_id`, for a document mention; the node's text head otherwise), `created_at`, and `source_seq`. `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`; `404` when the node does not exist. |
| `/api/v1/inbox?project=&assignee=` | GET | human only | List open asks, newest first. `assignee=me\|unassigned\|<login>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that login (`400 ASSIGNEE_NOT_ALLOWED` when unlisted). |
| `/api/v1/agents` | GET | user or bearer | List live Envoy sessions with their `capabilities`, newest first, so a session can pick a target that advertises the delivery mode it wants. `api/agents.go` proxies the listener through `internal/dispatch/envoy`; unavailable listener responses are `503 ENVOY_UNAVAILABLE`. |
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
| `/api/v1/me/agents/state` | GET | identity | Read the user's per-agent conversation state: `{[session_id]: {cleared_before}}`. |
| `/api/v1/me/agents/{session_id}/state` | PUT | identity | Clear an agent's conversation for this user: `{cleared_before: <RFC3339>}`, 400 `INVALID_STATE` when malformed or more than a minute ahead of the server clock. |
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
  go test ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
