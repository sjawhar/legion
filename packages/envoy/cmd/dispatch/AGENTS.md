# Dispatch HTTP Server (Go)

`cmd/dispatch` serves the Dispatch dashboard, native API, document rooms, GitHub
OAuth, and the GitHub REST/GraphQL proxy.

## Startup and persistence

`DATABASE_URL` and `DISPATCH_AGENT_TOKEN` are required. Startup opens the shared
`pgxpool.Pool` at `store.sharedPoolSize` connections — the size is a property of
Dispatch rather than of the URL or the task's CPU allotment, and a `DATABASE_URL`
carrying `pool_max_conns` is refused at open rather than silently overridden —
applies embedded migrations from `internal/dispatch/store/migrations` (refusing
the whole set, applying nothing, when `pgmigrate.Load` refuses it, and bounding
every migration's lock waits at `pgmigrate.LockTimeout`; README "Database
migrations"), then starts HTTP serving. The Postgres store contains users,
native issues, artifacts, document updates, and the event outbox.
`envoy-dispatch census` (`cmd/dispatch/census.go`, `store.Census` over
`pgmigrate.Census`) is a deployment's pre-deploy census of the migrations the
database has not applied, read-only, run before the service rolls (README
"Pre-deploy census"); `runSubcommand` refuses an argument it does not know with
exit 2 instead of serving, since serving migrates.

Every Dispatch setting `cmd/dispatch` reads is a row of the settings table
(`cmd/dispatch/settings.go`): `main` reads every row once (`processSettings`),
`resolveBootConfig` and the subcommands take their values from that read
(`settingValues`, which panics on a name the table does not list), and each
reader in `internal/dispatch` is handed its value — a parameter, an option
(`routes.AppContextOptions.InsecureCookie`/`EnvoyToken`, `envoy.WithToken`),
or the table's lookup (`config.Load`'s environment, `auth.LoadAppFromEnv`) —
as are the NATS connects `cmd/dispatch` makes through `internal/bus`
(`bus.WithEnvironment`, `bus.Dial`'s environment). A new setting is a new row,
never an `os.Getenv`: `TestNoReaderBypassesTheSettingsTable` fails on any other
environment read under `cmd/dispatch` or `internal/dispatch`, and on a dot import
of `os` or `syscall`, whose bare `Getenv` it could not tell from a local
function; `TestEverySettingReachesItsReader` fails until the row has a case that
hands it to its reader. `envoy-dispatch settings` prints the table, and the docs
site's configuration reference (`docs/site/generators/dispatch-config.ts`) is
generated from it.

`dispatchHandler` mounts the one `GET /healthz` the process serves on its own
mux, above the dashboard router, and the probe reads the database through
`store.Pool.Healthy` — a one-connection pool nothing else uses, under
`store.healthProbeTimeout`, two seconds covering dial and query. It never waits
on the shared pool: a busy period legitimately leaves that pool with no free
connection, and a probe queued behind a writer reads as a dead process, so the
ALB fails it at five seconds and ECS replaces the task, cancelling every
in-flight request of every other client. The compose healthcheck and the deploy
script are tighter still at three seconds. The deadline covers the other half —
a database that drops packets rather than refusing is answered `503` with
`db: false` inside the bound, never with silence, and the reason is logged.

Beside `ok`, `db` and `nats`, `/healthz` names what is deployed: `commit`, the
legion commit the image build stamped (`main.buildCommit`, from the Dockerfile's
`LEGION_COMMIT` build argument, which the release workflow sets to its
`github.sha` and which must be a full sha or empty; `null` in an unstamped
build), and `schema_version`, `max(version)` from `schema_migrations`, read by
the same probe query (`null` whenever `db` is false). The deployment repository's
dispatch-apply lane compares both with the image pin it applied and that commit's
migrations.

Migration 0010 added stored generated `search` columns. Migrations `0057`–`0061` made them plain
columns whose `BEFORE INSERT OR UPDATE` triggers maintain `search_text`-normalised vectors, so no
application code writes or refreshes them. Each trigger builds its vector with `search_vector`
(0068): the whole text where its vector fits Postgres's limit on one tsvector, and otherwise the
longest of its first half, quarter, eighth, … that fits (`README.md` "Search"). The issues trigger
also writes `issues.title_lexemes` (0069), the title's own lexemes through `search_vector`, which
the duplicate-title check reads for every stored title and builds the same way for the new one.

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries. Stored dashboard mappings are
authoritative, and external issues fall back to `DISPATCH_DEFAULT_PROJECT` when
configured. An unmapped repository without a default is rejected; issues that
use the default get a `repo:owner/name` label. `DISPATCH_NATS_DISABLED=1` leaves
database and SSE paths available and makes `/healthz` report `nats: null`.
Otherwise Dispatch loads config in this precedence order: user `envoy.json`,
repository `envoy.json`, then environment overrides. A present `NATS_URLS`
overrides merged `natsUrls` before `bus.ConnectOwningStream` and the outbox
start; the server owns `ENVOY_NOTIFICATIONS` on whichever server that resolves
to, and refuses one that is not this machine's unless `ENVOY_ALLOW_REMOTE_NATS=1`
says the run means it (every deployment sets it). A present
`DISPATCH_SERVER_URL` overrides merged `dispatch.serverUrl`; it must be an
absolute `http` or `https` URL with no path, and is the exact browser origin
used for GitHub OAuth. It must equal the URL humans type into the browser, with
`<DISPATCH_SERVER_URL>/auth/callback` registered on the GitHub App. Host
adapters can override their configured Dispatch base URL with `DISPATCH_URL`.
`DISPATCH_TEST_HOOKS=1` mounts two test-only routes — unset in every real
deployment. `POST /api/v1/events/_test/disconnect` closes every open SSE
connection, as if the server had restarted; e2e's `run-server.sh` sets the flag
so the web client's reconnect-from-lastId path can be exercised without seeding
thousands of events to trip the SSE replay cap. `POST
/api/v1/artifacts/_test/quiesce` closes every live document, flushing each
through the store, and waits for the settlements in flight, leaving the service
able to load documents again; `e2e/seed.ts` calls it before truncating so its
`TRUNCATE` cannot cross lock order with a settlement.

With NATS configured and the GitHub App private key loaded, startup also runs the webhook
redelivery sweep (`internal/dispatch/redeliver`, wired in `cmd/dispatch/redeliver.go`). Every
`redeliver.Interval` it lists the App webhook's failed deliveries and redelivers them through
GitHub's App API. Its cursor and per-delivery claims live in the `envoy_webhook_redelivery` KV
bucket. `envoy-dispatch redeliver-webhooks --since <d> [--dry-run]` runs the same sweep once
over a chosen window without moving the cursor. The README's "Webhook redelivery" section has
the rules: 4xx is never redelivered, bounded attempts, GitHub's rate limits, the log lines.

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
- `DISPATCH_DEV_SIGNIN=1` mounts `GET /auth/_dev/signin?login=<login>&next=<path>`
  (`routes/devsignin.go`, which holds everything that can mint a cookie without
  GitHub), which issues the cookie identity's own session cookie for an
  allowlisted login through the callback's `issueSession`, with no GitHub
  exchange. `devSignInFence` (`cmd/dispatch/main.go`, run by
  `resolveBootConfig`) refuses it unless identity is cookie, the host of
  `boot.ListenAddr` (the one address `main` binds) is a loopback IP literal
  (`routes.LoopbackHostPort`, which the route's peer check also uses), every
  `DATABASE_URL` host `pgx.ParseConfig` finds is loopback or a unix socket
  (`routes.LoopbackName`), `DISPATCH_SIGNING_KEY` is unset,
  `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, and a set
  `DISPATCH_AGENT_SECRETS_URL` and `ENVOY_URL` (the listener mentions and
  messages are delivered through) each name a loopback host
  (`routes.LoopbackURL`, the one URL rule every fence item uses).
  `devSignInLoadedFence` (run by `main` once it has read `envoy.json` and the
  App, before anything connects) checks what the environment does not hold: the
  dashboard origin must name `127.0.0.1`, `[::1]` or `localhost`
  (`routes.DevSignInOrigin`), and a loaded App private key must come from
  `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback
  host. A key from `app.json`, where a developer keeps the real App's key, is
  refused whatever the base, naming the file: a signed-in session can save an
  architecture source, which has the App probe and import the repository the
  caller names. The loopback check is on the host, not on what listens there,
  and every App call hands a signed App JWT to whatever owns that port, so the
  environment's key must be a throwaway, as `packages/dispatch/e2e/run-server.sh`
  generates one per run. The key is the credential that acts (`githubapp.New`
  builds no client without it, and the App JWT names the client ID), so the App
  ID is not fenced. This fence leaves the OAuth client pair alone too: an
  exchange needs a code GitHub issues after a person signs in there, and the
  pair's token refresh (`ProxyConfig.refresh`, `internal/dispatch/githubapi/proxy.go`)
  needs a stored token pair, which only `requireUser`'s dev sign-in check
  (`routes/router.go`) keeps from being read under the flag. A change to that
  check unfences the pair.
  `routes.BuildAppContext` also refuses a dashboard origin that is not
  loopback and stores the origin's host in the unexported `devSignInHost`, the
  only switch `New` reads, so no caller can mount the route without that check.
  The signing key is then
  `auth.NewSigningKey`, generated per process and never the data-dir file, so a
  cookie it mints dies with the process. While it is on, the whole router
  answers a request whose `Host` is not the dashboard origin's
  `421 HOST_MISMATCH` (`requireHost`), the route serves only a loopback peer
  with no forwarding header and logs every mint at WARN, and `requireUser`
  answers `503 GITHUB_TOKEN_UNAVAILABLE` before it reads a stored token pair.
  The key bounds the cookie only: a `dsp_` token a dev session mints, and the
  session and token rows its sign-out changes, are rows every server on the
  same database acts on, so a dev-sign-in server needs a database of its own.
- GitHub OAuth credentials come from `DISPATCH_APP_CLIENT_ID` and
  `DISPATCH_APP_CLIENT_SECRET`, or the Dispatch app credentials file.
- Agents normally authenticate as a `session` actor with a personal `dsp_` token
  minted by a human in Settings, sent as `Authorization: Bearer <token>`.
  `DISPATCH_AGENT_TOKEN` is the shared devbox fallback; its callers have no
  owner attribution. The API and the document websocket (`/ws/doc/{room}`)
  read the bearer with one helper, `auth.BearerToken`, and compare it with one
  constant-time helper, `auth.MatchesSharedAgentToken`. Bearer callers cannot
  act as users.
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
| `/auth/_dev/signin` | GET | public, `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header | Issue an allowlisted login's session cookie with no GitHub exchange and redirect to the sanitized `next`; `400 DEV_SIGNIN_INPUT`, `403 LOGIN_NOT_ALLOWED`, `403 DEV_SIGNIN_FORBIDDEN`. Not mounted otherwise. |
| `/api/github/rest/...` | any | identity | Proxy GitHub REST with the user's token. |
| `/api/github/graphql` | POST | identity | Proxy GitHub GraphQL with the user's token. |
| `/healthz` | GET | public | Report that the process serves, Postgres answers within two seconds on the health pool, and NATS is connected where configured, plus `commit` (the build's legion commit, or `null`) and `schema_version` (the highest applied migration, or `null` when the database did not answer). |
| `/api/v1/projects` | GET, POST | POST human only | List projects (including `open_asks`) or create one. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | user or bearer | List non-primary artifacts in a project (`?unlinked=true` selects unlinked ones) or create an unlinked project artifact. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose formatting is more items than one document update can store (1,048,576), naming the count. |
| `/api/v1/settings/repo-projects` | GET | human only | List repository-to-project mappings. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | human only | Create or replace, or remove, a repository mapping. |
| `/api/v1/settings/architecture-sources` | GET | human only | List every project's architecture source with its sync bookkeeping. |
| `/api/v1/projects/{key}/architecture-source` | GET, PUT, DELETE | GET user or bearer; PUT, DELETE human only | A project's architecture source (`{repo, branch}`; the directory is fixed at `.dispatch/architecture/`). PUT proves the GitHub App can read the repository first (`409 SOURCE_ACCESS` otherwise); DELETE removes the source and the model it projected; GET answers `200 null` without one, since whether a project has a source is a question with an ordinary negative answer (DELETE and `/sync`, which act on a source, keep `404 SOURCE_NOT_FOUND`). |
| `/api/v1/projects/{key}/architecture-source/sync` | POST | user or bearer | Import the model now; `200` carries the updated row (`last_error` set when the model was rejected and the previous projection stays up), `409 SOURCE_ACCESS` a credential or branch problem. |
| `/api/v1/projects/{key}/architecture` | GET | user or bearer | The component tree with the work attached: `source`, `totals` (`issues_done`/`issues_total` over every filed issue, `unassigned`, `not_architectural`, `components_without_work` over non-external components, `retired_links`), one row per component (`done`/`total` count the distinct issues whose effective set names it or any component it contains, parents and icebox included, each once; `own_*` only those naming it itself; `issues` say how each qualified: `direct` > `inherited` > `contained` with `via`), and the `unassigned`, `not_architectural` (with `reason`, `inherited_from`), and `retired_links` lists. `404 SOURCE_NOT_FOUND` without a source. |
| `/api/v1/me/agent-tokens` | GET, POST | human only | List personal token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | human only | Revoke a personal agent token. |
| `/api/v1/users` | GET | human only | The sign-in allowlist as `{users: [{login}]}`, sorted lowercase: the assignee picker's options (pure config, no DB). |
| `/api/v1/whoami` | GET | user or bearer | Who the server takes the caller for: `{kind: "user", login}` for a human, `{kind: "agent", owner, service}` for a bearer (`owner` is the personal token's lowercase login, null under the shared token; `service` is a verified service-account token's Kubernetes subject, null for every other bearer). |
| `/api/v1/issues` | GET, POST | POST human or bearer | List or create native issues. The listing is every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more; alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match; a repeated, blank, non-integer or out-of-range value, or `cursor`, is `400 INVALID_QUERY` naming the parameter. Creation without a `spec`, or with a blank one, gives an empty primary document at version 1. A title over `contracts.IssueTitleMax` (1,000) UTF-16 units, counted after trimming, is `400 CAP_EXCEEDED` before the duplicate check reads the project's titles. Creation refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`. |
| `/api/v1/search?q=&project=&limit=` | GET | user or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results contain `<mark>` snippets and SPA `href`s. `limit` is 1–50 (default 20); an under-two-character query returns `400 INVALID_QUERY`, while a stop-word-only query returns `200` with no results; a query over `contracts.SearchQueryMax` (1,000) UTF-16 units, counted after trimming, returns `400 CAP_EXCEEDED` before anything runs, a project that is not a project key returns `400 INVALID_PROJECT` (an empty one searches every project), and an invalid limit returns `400 INVALID_LIMIT`. Both ride in the URL; `packages/contracts/AGENTS.md` "Search limits" owns what keeps it under the load balancer's limit. A text whose search vector would pass Postgres's limit on one vector is found by the words in its opening part only (`README.md` "Search"). |
| `/api/v1/issues/{key}` | GET, PATCH | PATCH human or bearer | Read or update an issue. A `title` is trimmed, and one over `contracts.IssueTitleMax` (1,000) UTF-16 units is `400 CAP_EXCEEDED`, as on creation. `assignee` (an allowlisted login, lowercased; `null` clears; absent leaves it) may be set by any caller; an unlisted login is `400 ASSIGNEE_NOT_ALLOWED`. `components` is the issue's own architecture attachment: `null` or `{mode: "inherit"}` deletes it (the issue takes its nearest ancestor's again), `{mode: "explicit", ids}` names bare component ids of the issue's project (`400 COMPONENTS_INPUT` for an unknown, retired, external, or other-project id), `{mode: "none", reason}` declares the issue not architectural; it is the one field besides `rank` a closed issue accepts without reopening. Every issue read carries the effective `components` (`mode`, `ids`, `unknown` for retired ids, `reason`, `inherited_from`), resolved up the parent chain in the same query. |
| `/api/v1/issues/resolve` | GET | user or bearer | Resolve an external issue reference to its native key. |
| `/api/v1/issues/{key}/events` | GET | user or bearer | Read events by forward cursor, descending page, or exact IDs. |
| `/api/v1/issues/{key}/references` | GET | user or bearer | Read the eight-hop artifact reference closure; matching `If-None-Match` returns `304`. |
| `/api/v1/references?to=\|from=&kind=&since=` | GET | user or bearer | Edges of one node in the reference graph, newest first, cross-project. Exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference; `kind` is a csv of `mentions`, `child_of`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`, `part_of`, `depends_on`, `affects`; `since=<events.id>` keeps mentions introduced after it and excludes structural edges. Each edge carries the other `node` (`kind`, `id`, `issue_key`, `project`, `ref`; no `ref` for sessions; a `component` node is `<project>/<id>` with ref `dispatch://<PROJECT>/component/<id>`, and one a re-import retired is omitted with its edges), an `excerpt` (the containing block, with `block_id`, for a document mention; the node's text head otherwise), `created_at`, and `source_seq`. `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`; `404` when the node does not exist. |
| `/api/v1/inbox?project=&assignee=` | GET | human only | List open asks, newest first. `assignee=me\|unassigned\|<login>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that login (`400 ASSIGNEE_NOT_ALLOWED` when unlisted). |
| `/api/v1/agents` | GET | user or bearer | List live Envoy sessions with their `capabilities`, newest first, so a session can pick a target that advertises the delivery mode it wants. `api/agents.go` proxies the listener through `internal/dispatch/envoy`; unavailable listener responses are `503 ENVOY_UNAVAILABLE`. |
| `/api/v1/issues/{key}/asks` | POST | user or bearer | Create an ask. |
| `/api/v1/issues/{key}/asks?state=` | GET | user or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). |
| `/api/v1/asks/{id}` | GET | user or bearer | Read an ask. It carries `anchor_block`, where the anchor's block stands (its path, and in a table the row, column and header), when the live document holds it. When Dispatch cannot read the anchor's document the read still answers, without `anchor_block` and with `anchor_block_error`, the code the API answers that error with elsewhere: `DOC_SERVICE_UNAVAILABLE` (its room or store could not be reached), `DOC_SCHEMA` (its live tree is outside the schema) or `INTERNAL`. |
| `/api/v1/asks/{id}` | PATCH | user or bearer | Edit one or more of `question`, `options`, `multiple`, or `urgency` while the ask is open. A bearer caller must be the asking session; a human may edit any open ask. The response has nullable `edited_at`; `ask.edited` records the full current ask, prior mutable fields, and `edited_by`. Ask anchors cannot be changed by this route. On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it. Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, while naming `question` or `options` replaces that part with the markdown pipeline's own parse. A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200. Text the block cannot carry unchanged is `400 ASK_BLOCK_TEXT` naming the field, with nothing written. |
| `/api/v1/asks/{id}/answer` | POST | human only | Answer an open ask. An approval ask is moved to each document version before the human sees it, and an `ASK_EDITED` response means the question changed after the human reviewed it, so they reload and answer the moved request. |
| `/api/v1/asks/{id}/resolve` | POST | user or bearer | Retract or self-resolve an open ask with a recorded reason. On a block ask the block's `state` is written with it, so no later settlement reopens it or credits the repair to whoever next edits the document. A reason beginning `removed from the document in version`, which marks a retraction settlement wrote, is `400 INVALID_RESOLUTION`. |
| `/api/v1/issues/{key}/comments` | GET, POST | user or bearer | List or create comments and suggestions. |
| `/api/v1/comments/{id}` | GET | user or bearer | Read a comment and its reply chain. It carries `anchor_block`, where the anchor's block stands (its path, and in a table the row, column and header), when the live document holds it. When Dispatch cannot read the anchor's document the read still answers, without `anchor_block` and with `anchor_block_error`, the code the API answers that error with elsewhere: `DOC_SERVICE_UNAVAILABLE` (its room or store could not be reached), `DOC_SCHEMA` (its live tree is outside the schema) or `INTERNAL`. |
| `/api/v1/comments/{id}/resolve` | POST | user or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/accept` | POST | human only | Apply and accept a suggestion. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open; one removed after it answers `200` with `lost: true`. |
| `/api/v1/comments/{id}/reject` | POST | human only | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | user or bearer | Post a short issue message. |
| `/api/v1/messages/{id}?session=` | GET | user or bearer | Read the conversation a message belongs to, issue-less or not, by any message id in it: the thread root with its deliveries and every reply, oldest first. Who may read which thread is the route's description in `internal/dispatch/api/routes_table.go` and `packages/envoy/AGENTS.md` ("Targeted Dispatch messages"). |
| `/api/v1/messages/{id}/deliveries/{attempt}/accept` | POST | bearer only | The attempt's session records that it took the message as its user's own turn, `{actor: {kind: "session", id}}`, and is answered the attempt with the message's stored `body` (`AcceptedMessageDelivery`); `403 ACCEPT_FORBIDDEN` for another session's attempt or a non-bearer. One compare-and-set under the message's row lock (`FOR NO KEY UPDATE`, which the session's reply's foreign-key `FOR KEY SHARE` does not wait for), after the event owner's lock. It succeeds only for a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets that session (`409 ACCEPT_NOT_DIRECT`); that a person wrote (`409 ACCEPT_NOT_WRITTEN_BY_PERSON`); sent as a Send or an Aside (`409 ACCEPT_NOT_ASIDE_OR_STEER` for a BTW); when no attempt of it was accepted before (`409 ACCEPT_ALREADY_ACCEPTED`); only the message's latest attempt (`409 ACCEPT_SUPERSEDED`), whose `requested_by` is a person (`409 ACCEPT_NOT_REQUESTED_BY_PERSON`; a row from before migration 0054 records nobody) and that person the message's author, logins compared case-insensitively (`409 ACCEPT_NOT_REQUESTED_BY_AUTHOR`: another person's retry), which Dispatch did not record as failed (`409 ACCEPT_FAILED`), created within the last minute by Postgres's clock (`409 ACCEPT_STALE`). A pending attempt may be accepted: the route writes only `accepted_at` and `accepted_as: "user_turn"`, never the state or claim the send settles, and appends `message.accepted` (`{message_id, attempt, session_id, accepted_as, target}`), never a second `message.delivery`; the event is issue-less, so the outbox never publishes it to NATS. The actor is the bearer's own claim, as on every session write. |
| `/api/v1/broadcasts` | POST | human only | Send one message to many sessions: `{body, delivery, session_ids, idempotency_key}`. Writes the broadcast, its key and one issue-less targeted message per recipient in one transaction, answers 201, then delivers behind the request (four workers, each on its own tracking context derived from the server lifetime) through the ordinary targeted-message path, so a cut-off request cannot strand a recipient. Each recipient comes back with its first attempt pending. A selected session that is not live or does not advertise `delivery` is excluded and named in `excluded` (never switched to another mode); a selection with no reachable recipient is `400 BROADCAST_EMPTY` and writes nothing. At most 100 recipients (`contracts.MaxBroadcastRecipients`, generated from `MAX_BROADCAST_RECIPIENTS` in `packages/contracts`, which the dashboard also reads), duplicates collapsed. `idempotency_key` (required; letters, digits, `.`, `_`, `:` and `-`, at most 128) names one send and belongs to the sender's canonical login: a repeat with the same key and the same body, mode and `session_ids` answers `200` with the broadcast the key made, read as it stands, its `excluded` the requested sessions that broadcast has no recipient for (`excluded by the original send`); the same key with a different request is `409 BROADCAST_KEY_REUSED` naming that broadcast as `broadcast_id` and sends nothing; a missing or malformed key is `400 BROADCAST_INPUT`, a longer one `400 CAP_EXCEEDED`. The key is looked up before the listener is read, so a repeat is answered while the listener is down. |
| `/api/v1/broadcasts` | GET | human only | List the 50 newest broadcasts with their recipient and reply counts. |
| `/api/v1/broadcasts/{id}` | GET | human only | Read one broadcast with every recipient's message, delivery attempts and replies. Recipients come back in the order the send named them, after exclusions; a broadcast written before migration 0052, or by an older server during a rollout, has no stored order and falls back to `created_at, id`. Unknown or malformed id is `404 BROADCAST_NOT_FOUND`. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | user or bearer | List issue artifacts or create a version from a multipart file or JSON inline content. The JSON form requires `Content-Type: application/json`. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose formatting is more items than one document update can store (1,048,576), naming the count. |
| `/api/v1/artifacts/{id}` | GET | user or bearer | Read an artifact, versions, and incoming references. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/text` | GET | user or bearer | Read a live document's markdown. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/blocks` | GET | user or bearer | A document's blocks with markdown ranges, tokens, and reference counts; a table counts its cells' anchors. |
| `/api/v1/artifacts/{id}/blocks/{block_id}` | GET | user or bearer | Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it (colspans and rowspans placed) and the row's cells; `404 TARGET_NOT_FOUND` for an id the live document does not hold. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | user or bearer | Read a document version or download a blob. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions` | POST | user or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | user or bearer | Apply document edit operations. `{id}` must be a UUID. An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read; an ask it carries through unchanged is not its to refuse. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/artifacts/{id}/approval-requests` | POST | user or bearer | Ask a human to approve a document's latest settled version. The question is `Approve <name> (version <N>)?` followed by the optional `summary` (blank is `400 SUMMARY_INPUT`; past the ask cap at the longest version a request can reach, ten digits, is `400 CAP_EXCEEDED` naming `summary`). One open approval row follows every document version in place, rewording its question and emitting `ask.edited`; while its `requested_version` is below the new version it waits on the agent. Only the move that takes it from the human notifies; a later move while it already waits on the agent is `quiet: true`, `notify: false`, and reaches no follower. Calling this route again while the row waits on the agent - moved, or a thread reply newer than its last hand-back holds the turn - hands it back to the human: a summary that changes its question rewords it first (`ask.edited`; an omitted summary keeps its prior one), then `requested_version` becomes the latest version and `ask.handed_back` is emitted, leaving `edited_at` as it was. While the row waits on the human, the same summary or none is a repeat that answers `200` with no event, and a different one is `409 APPROVAL_WAITS_ON_HUMAN` and changes nothing, since it would rewrite the card the human is reading. It answers `201` when it wrote anything, with the document's `approval` as the call left it (`waiting_on` while awaiting). |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | user or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | user or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | user or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | user or bearer | Read outgoing and incoming reference edges. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | user or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an issue artifact's live markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an issue artifact version or download its blob. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply issue-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | user or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an unlinked project document version or download its blob. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply project-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/me/state` | GET | identity | Read the user's issue UI state. |
| `/api/v1/me/issues/{key}/state` | PUT | identity | Update the user's issue UI state. |
| `/api/v1/me/agents/state` | GET | identity | Read the user's per-agent conversation state: `{[session_id]: {cleared_before?, read_through?, unread_replies}}`, where `unread_replies` counts the session's replies to the user's direct messages newer than both the Clear and the read mark and not read by id. |
| `/api/v1/me/agents/{session_id}/state` | PUT | identity | Clear an agent's conversation (`cleared_before`), mark it read (`read_through`, which only moves forward and covers every reply up to it), and/or mark some of its replies read by id (`read_replies`, messages the path's session wrote, covering those alone, each in any form `uuid.Parse` reads) for this user: at least one; 400 `INVALID_STATE` when none is given, a cutoff is malformed or more than a minute ahead of the server clock, or a `read_replies` id is not a uuid or not a message that session wrote (nothing is written then); an accepted cutoff is stored no later than the server's now. Appends `user_agent_state.updated` (`{login, session_id}`) for the viewer's other devices, except for a request naming only replies already read by id or passed by the read mark, which changes nothing (a reply the Clear hides but the read mark has not passed is neither: the first request naming it writes its row and appends the event), and answers the session's state. |
| `/api/v1/events` | GET | identity | Stream durable events with SSE. Omitting `since` (a cold client) subscribes before resolving the current head internally, so no separate request can race it. |
| `/api/v1/artifacts/_test/quiesce` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every live document and wait for the settlements in flight; not mounted otherwise. |
| `/api/v1/events/_test/disconnect` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every open SSE connection; not mounted otherwise. |
| `/ws/doc/{room}` | GET | user or bearer | Join the Hocuspocus document room. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. `natstail` reads
`natsUrls` from `envoy.json` and prints a matching envelope:

```sh
go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1
```

`natstail` publishes nothing and owns nothing on the bus: it neither creates nor
updates `ENVOY_NOTIFICATIONS`, and it refuses a NATS server that is not this
machine's, naming the URL. A machine whose `envoy.json` names a shared NATS
(an agent devbox names production's) runs it with
`ENVOY_ALLOW_REMOTE_NATS=1 go run ./cmd/natstail …`, which is the run saying it
means that server.

## Checks

```sh
cd packages/envoy
env DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
  go test ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
